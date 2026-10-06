package catalog

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

var (
	tenantA = mustTenant("11111111-1111-4111-8111-111111111111")
	tenantB = mustTenant("22222222-2222-4222-8222-222222222222")
	t0      = time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
)

func mustTenant(s string) authz.TenantID {
	t, err := authz.ParseTenantID(s)
	if err != nil {
		panic(err)
	}
	return t
}

type fakeStore struct {
	mu    sync.Mutex
	calls map[authz.TenantID][][]controldb.ServiceObservation
	err   error
}

func (f *fakeStore) Observe(_ context.Context, tenant authz.TenantID, obs []controldb.ServiceObservation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.calls == nil {
		f.calls = map[authz.TenantID][][]controldb.ServiceObservation{}
	}
	f.calls[tenant] = append(f.calls[tenant], obs)
	return nil
}

type counts struct{ written, dropped, failed int }

func (c *counts) ObserveCatalog(w, d int, f bool) {
	c.written += w
	c.dropped += d
	if f {
		c.failed++
	}
}

func svc(id, lang string) Sighting {
	return Sighting{ServiceID: id, Environment: "prod", Namespace: "shop", Name: "checkout-" + id, Language: lang}
}

// tenant별로 모아 한 번에 쓰고, 같은 서비스는 가장 늦은 관측 시각과 아는 언어를 쓴다.
func TestFlushAggregatesPerTenant(t *testing.T) {
	st, obs := &fakeStore{}, &counts{}
	clock := t0
	r := New(Config{Store: st, Observer: obs, Now: func() time.Time { return clock }})
	r.Observe(tenantA, []Sighting{svc("a", "")}, t0)
	r.Observe(tenantA, []Sighting{svc("a", "go"), svc("b", "")}, t0.Add(time.Second))
	r.Observe(tenantA, []Sighting{svc("a", "")}, t0.Add(-time.Minute)) // 더 이른 관측: 덮지 않는다
	r.Observe(tenantB, []Sighting{svc("a", "java")}, t0)
	r.Flush(context.Background())

	if len(st.calls[tenantA]) != 1 || len(st.calls[tenantB]) != 1 {
		t.Fatalf("calls = %+v", st.calls)
	}
	got := map[string]controldb.ServiceObservation{}
	for _, o := range st.calls[tenantA][0] {
		got[o.ServiceID] = o
	}
	if a := got["a"]; !a.SeenAt.Equal(t0.Add(time.Second)) || a.Language != "go" || len(got) != 2 {
		t.Errorf("tenant A = %+v", got)
	}
	if obs.written != 3 {
		t.Errorf("written = %d", obs.written)
	}
}

// TouchEvery 안에 다시 본 서비스는 쓰지 않고, 지나면 쓴다(last_seen 갱신).
func TestTouchEveryCache(t *testing.T) {
	st := &fakeStore{}
	clock := t0
	r := New(Config{Store: st, Now: func() time.Time { return clock }, TouchEvery: 5 * time.Minute})
	r.Observe(tenantA, []Sighting{svc("a", "")}, clock)
	r.Flush(context.Background())
	clock = clock.Add(time.Minute)
	r.Observe(tenantA, []Sighting{svc("a", "")}, clock)
	r.Flush(context.Background())
	if len(st.calls[tenantA]) != 1 {
		t.Fatalf("rewrote within TouchEvery: %d calls", len(st.calls[tenantA]))
	}
	clock = clock.Add(5 * time.Minute)
	r.Observe(tenantA, []Sighting{svc("a", "")}, clock)
	r.Flush(context.Background())
	if len(st.calls[tenantA]) != 2 || !st.calls[tenantA][1][0].SeenAt.Equal(clock) {
		t.Fatalf("calls = %+v", st.calls[tenantA])
	}
}

// queue가 차면 막지 않고 버리며 센다.
func TestQueueFullDropsWithoutBlocking(t *testing.T) {
	obs := &counts{}
	r := New(Config{Store: &fakeStore{}, Observer: obs, QueueSize: 1})
	done := make(chan struct{})
	go func() {
		r.Observe(tenantA, []Sighting{svc("a", "")}, t0)
		r.Observe(tenantA, []Sighting{svc("b", ""), svc("c", "")}, t0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Observe blocked")
	}
	if obs.dropped != 2 {
		t.Errorf("dropped = %d", obs.dropped)
	}
}

// 쓰기 실패는 cache를 갱신하지 않아 다음 sighting이 다시 쓴다.
func TestWriteFailureRetriedByNextSighting(t *testing.T) {
	st, obs := &fakeStore{err: errors.New("pg down")}, &counts{}
	clock := t0
	r := New(Config{Store: st, Observer: obs, Now: func() time.Time { return clock }})
	r.Observe(tenantA, []Sighting{svc("a", "")}, clock)
	r.Flush(context.Background())
	if obs.failed != 1 {
		t.Fatalf("failed = %d", obs.failed)
	}
	st.err = nil
	r.Observe(tenantA, []Sighting{svc("a", "")}, clock)
	r.Flush(context.Background())
	if len(st.calls[tenantA]) != 1 {
		t.Errorf("not retried: %+v", st.calls)
	}
}

func TestRunFlushesOnStop(t *testing.T) {
	st := &fakeStore{}
	r := New(Config{Store: st, FlushEvery: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	r.Observe(tenantA, []Sighting{svc("a", "")}, t0)
	cancel()
	<-done
	if len(st.calls[tenantA]) != 1 {
		t.Errorf("pending sighting not flushed on stop")
	}
}
