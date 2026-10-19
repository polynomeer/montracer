package servicemap

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeStore struct {
	maxObs   map[string]time.Time
	progress Progress
	pairs    []Pair
	reads    [][2]time.Time
	written  [][]Row
}

func (s *fakeStore) MaxObserved(context.Context, time.Time, time.Time) (map[string]time.Time, error) {
	return s.maxObs, nil
}

func (s *fakeStore) LoadProgress(context.Context, time.Time) (Progress, error) {
	return s.progress, nil
}

func (s *fakeStore) ReadPairs(_ context.Context, from, to time.Time) ([]Pair, error) {
	s.reads = append(s.reads, [2]time.Time{from, to})
	var out []Pair
	for _, p := range s.pairs {
		if !p.WindowStart.Before(from) && p.WindowStart.Before(to) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *fakeStore) WriteWindows(_ context.Context, rows []Row) error {
	s.written = append(s.written, rows)
	return nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func clientPair(tenant string, w time.Time, matched bool, n uint64) Pair {
	p := Pair{Tenant: tenant, WindowStart: w, Source: svcA, Kind: KindClient, Requests: n, LatencyLE: make([]uint64, len(Bounds)), LastSeen: w}
	if matched {
		p.Matched, p.Target = true, svcB
	} else {
		p.Host = "payment" // 계측되지 않은 상대 → peer
	}
	return p
}

func TestCycleWritesWholeWindowsWithOneRevision(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	w := now.Add(-5 * time.Minute)
	st := &fakeStore{maxObs: map[string]time.Time{"a": now, "b": now}}
	st.pairs = []Pair{clientPair("a", w, true, 2), clientPair("a", w, false, 1), clientPair("b", w, true, 7)}
	j := New(Config{Store: st, Logger: quiet(), Now: func() time.Time { return now }})
	if err := j.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.written) != 1 || len(st.written[0]) != 3 {
		t.Fatalf("written = %+v", st.written)
	}
	rev := st.written[0][0].Revision
	for _, r := range st.written[0] {
		if r.Revision != rev || !r.ExpiresAt.Equal(r.WindowStart.Add(30*24*time.Hour)) || !r.ComputedAt.Equal(now) {
			t.Fatalf("row = %+v", r)
		}
	}

	// 같은 내용이면 다시 쓰지 않는다
	if err := j.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.written) != 1 {
		t.Fatalf("unchanged window rewritten: %d writes", len(st.written))
	}

	// 늦게 온 server span이 짝을 찾음: tenant a의 window만 새 revision으로 통째로 다시 쓴다(옛 peer 행은 조회에서 가려진다)
	st.pairs[1] = clientPair("a", w, true, 1)
	if err := j.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.written) != 2 || len(st.written[1]) != 1 {
		t.Fatalf("second write = %+v", st.written)
	}
	r := st.written[1][0]
	if r.Tenant != "a" || r.Requests != 3 || r.Destination != svcB || r.Revision <= rev {
		t.Fatalf("rewritten row = %+v", r)
	}
}

func TestCycleReadsInChunksAndKeepsTenantRanges(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	st := &fakeStore{
		maxObs:   map[string]time.Time{"a": now, "b": now},
		progress: Progress{Done: map[string]time.Time{"a": now.Add(-40 * time.Minute)}},
	}
	// tenant b의 범위(최근 10분) 밖 pair는 a의 범위 때문에 읽혀도 쓰지 않는다
	st.pairs = []Pair{clientPair("a", now.Add(-35*time.Minute), true, 1), clientPair("b", now.Add(-35*time.Minute), true, 1)}
	j := New(Config{Store: st, Logger: quiet(), Now: func() time.Time { return now }})
	if err := j.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.reads) != 4 { // [−40m, −2m) 를 10분씩
		t.Fatalf("reads = %v", st.reads)
	}
	if len(st.written) != 1 || len(st.written[0]) != 1 || st.written[0][0].Tenant != "a" {
		t.Fatalf("written = %+v", st.written)
	}
}

func TestCycleWithoutTenantsWritesNothing(t *testing.T) {
	st := &fakeStore{maxObs: map[string]time.Time{}}
	j := New(Config{Store: st, Logger: quiet()})
	if err := j.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.reads) != 0 || len(st.written) != 0 {
		t.Fatal("read or wrote without tenants")
	}
}
