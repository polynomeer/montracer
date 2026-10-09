package alertworker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/monitor"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

var (
	tenant = mustTenant("11111111-1111-4111-8111-111111111111")
	t0     = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
)

func mustTenant(s string) authz.TenantID {
	t, err := authz.ParseTenantID(s)
	if err != nil {
		panic(err)
	}
	return t
}

const specJSON = `{"name":"오류율","query":{"kind":"error_ratio"},"window_seconds":300,"evaluation_seconds":60,
	"condition":{"operator":"gt","threshold":0.02},"for_seconds":0,"no_data":{"action":"no_data"}}`

func canonical(t *testing.T) []byte {
	t.Helper()
	s, _, err := monitor.Normalize([]byte(specJSON))
	if err != nil {
		t.Fatal(err)
	}
	return s.Canonical()
}

// fakeStore는 monitor 하나의 lease·상태를 메모리로 흉내 낸다.
type fakeStore struct {
	mu        sync.Mutex
	spec      []byte
	instances []controldb.StoredInstance
	claimed   bool
	loseLease bool
	failWrite error
	completed []controldb.Completion
	claims    int
}

func (f *fakeStore) DueMonitors(context.Context, time.Time, int) ([]controldb.DueMonitor, error) {
	return []controldb.DueMonitor{{Tenant: tenant, MonitorID: "m1"}}, nil
}

func (f *fakeStore) ClaimEvaluation(_ context.Context, tn authz.TenantID, id string, now time.Time, _ time.Duration) (controldb.Claim, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	if f.claimed {
		return controldb.Claim{}, false, nil // 다른 worker가 이미 잡음
	}
	return controldb.Claim{Tenant: tn, MonitorID: id, Revision: 1, Slot: now.Truncate(time.Minute), Spec: f.spec, Instances: f.instances}, true, nil
}

func (f *fakeStore) CompleteEvaluation(ctx context.Context, _ controldb.Claim, _ string, r controldb.Completion) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err // 실제 저장소처럼 만료된 context로는 쓰지 못한다
	}
	if f.loseLease {
		return controldb.ErrLeaseLost
	}
	if f.failWrite != nil {
		return f.failWrite
	}
	f.completed = append(f.completed, r)
	f.instances = nil
	for _, g := range r.Groups {
		f.instances = append(f.instances, g.Instance)
	}
	return nil
}

type fakeMetrics struct {
	errors, total uint64
	queryErr      error
	hang          bool // 조회가 context 만료까지 돌아오지 않는다
	watermark     time.Time
	principals    []authz.Principal
}

func (m *fakeMetrics) MetricBuckets(ctx context.Context, p authz.Principal, q telemetrystore.MetricQuery, _ time.Time) ([]telemetrystore.MetricBucket, error) {
	m.principals = append(m.principals, p)
	if m.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if m.queryErr != nil {
		return nil, m.queryErr
	}
	b := func(status string, n uint64) telemetrystore.MetricBucket {
		return telemetrystore.MetricBucket{Group: []string{status}, StepStart: q.Range.From, Types: []string{"histogram"}, Units: []string{"s"},
			Windows: 5, Streams: 1, HistogramWindows: 5, Count: n, BoundsVariants: 1}
	}
	return []telemetrystore.MetricBucket{b("200", m.total-m.errors), b("500", m.errors)}, nil
}

func (m *fakeMetrics) RollupWatermark(_ context.Context, p authz.Principal, _ time.Duration, _, _ time.Time) (time.Time, error) {
	m.principals = append(m.principals, p)
	return m.watermark, nil
}

type counter struct {
	evals, lost, failed int
	scans               []time.Time
}

func (c *counter) ObserveEvaluation(string, int, time.Duration) { c.evals++ }
func (c *counter) ObserveLeaseLost()                            { c.lost++ }
func (c *counter) ObserveError(string)                          { c.failed++ }
func (c *counter) ObserveScan(at time.Time)                     { c.scans = append(c.scans, at) }

func newWorker(t *testing.T, s *fakeStore, m *fakeMetrics, clock *time.Time, obs *counter) *Worker {
	t.Helper()
	w, err := New(Config{Store: s, Metrics: m, Worker: "w1", Observer: obs, Now: func() time.Time { return *clock }})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// 위반 → 사건 열림(새 id), 계속 위반 → 같은 id, 정상 2회 → 닫힘 event에 그 id, 저장 상태엔 id 없음
func TestEpisodeLifecycle(t *testing.T) {
	clock := t0
	store := &fakeStore{spec: canonical(t)}
	metrics := &fakeMetrics{errors: 50, total: 1000, watermark: t0}
	obs := &counter{}
	w := newWorker(t, store, metrics, &clock, obs)
	tick := func() controldb.GroupWrite {
		t.Helper()
		if n, err := w.Tick(context.Background()); err != nil || n != 1 {
			t.Fatalf("tick: %d %v", n, err)
		}
		last := store.completed[len(store.completed)-1]
		if len(last.Groups) != 1 {
			t.Fatalf("groups = %+v", last.Groups)
		}
		return last.Groups[0]
	}
	g := tick()
	if !g.Opened || !g.Changed || g.Instance.State != "ALERT" || g.Instance.EpisodeID == nil || g.EpisodeOut != g.Instance.EpisodeID {
		t.Fatalf("open: %+v", g)
	}
	episode := *g.Instance.EpisodeID
	// 시스템 principal은 그 tenant의 telemetry 읽기만
	for _, p := range metrics.principals {
		if p.Kind() != authz.KindSystem || p.Tenant() != tenant || p.Subject() != Subject {
			t.Errorf("principal = %+v", p)
		}
	}
	// 다음 window도 위반: 같은 사건, 변화 없음(outbox 없음)
	clock, metrics.watermark = t0.Add(time.Minute), t0.Add(time.Minute)
	g = tick()
	if g.Changed || g.Opened || *g.Instance.EpisodeID != episode {
		t.Fatalf("still alerting: %+v", g)
	}
	// 정상 2회: RECOVERING → OK, 닫힘 event에 사건 id, 저장 상태는 id 없음
	metrics.errors = 0
	clock, metrics.watermark = t0.Add(2*time.Minute), t0.Add(2*time.Minute)
	g = tick()
	if g.Instance.State != "RECOVERING" || *g.Instance.EpisodeID != episode {
		t.Fatalf("recovering: %+v", g)
	}
	clock, metrics.watermark = t0.Add(3*time.Minute), t0.Add(3*time.Minute)
	g = tick()
	if !g.Closed || g.Instance.State != "OK" || g.Instance.EpisodeID != nil || g.EpisodeOut == nil || *g.EpisodeOut != episode {
		t.Fatalf("closed: %+v", g)
	}
	if obs.evals != 4 || len(obs.scans) != 4 || !obs.scans[3].Equal(t0.Add(3*time.Minute)) {
		t.Errorf("observed %d evaluations, scans %v", obs.evals, obs.scans)
	}
}

// 조회 실패는 worker 오류가 아니라 EVALUATION_ERROR 결과로 기록된다(이전 값으로 대체하지 않는다)
func TestQueryFailureIsRecorded(t *testing.T) {
	clock := t0
	store := &fakeStore{spec: canonical(t)}
	metrics := &fakeMetrics{queryErr: errors.New("clickhouse down"), watermark: t0}
	w := newWorker(t, store, metrics, &clock, &counter{})
	if _, err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := store.completed[0]
	if c.Status != "error" || c.Reason != "query_failed" || len(c.Groups) != 1 || c.Groups[0].Instance.State != "EVALUATION_ERROR" || c.Groups[0].Instance.LastValue != nil {
		t.Errorf("completion = %+v", c)
	}
}

// watermark가 멈추면 결측으로 기록한다
func TestStaleWatermarkIsNoData(t *testing.T) {
	clock := t0
	store := &fakeStore{spec: canonical(t)}
	metrics := &fakeMetrics{total: 1000, watermark: t0.Add(-time.Hour)}
	w := newWorker(t, store, metrics, &clock, &counter{})
	if _, err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := store.completed[0]; c.Status != "no_data" || c.Reason != "stale_watermark" || c.Groups[0].Instance.State != "NO_DATA" || c.WindowEnd != nil {
		t.Errorf("completion = %+v", c)
	}
}

// 다른 worker가 잡았으면 평가하지 않고, lease를 잃은 결과는 쓰지 않는다
func TestClaimAndLeaseLost(t *testing.T) {
	clock := t0
	store := &fakeStore{spec: canonical(t), claimed: true}
	metrics := &fakeMetrics{total: 1000, watermark: t0}
	obs := &counter{}
	w := newWorker(t, store, metrics, &clock, obs)
	if n, err := w.Tick(context.Background()); err != nil || n != 0 || len(metrics.principals) != 0 {
		t.Errorf("claimed elsewhere: n=%d err=%v queries=%d", n, err, len(metrics.principals))
	}
	store.claimed, store.loseLease = false, true
	if n, err := w.Tick(context.Background()); err != nil || n != 0 || obs.lost != 1 || len(store.completed) != 0 {
		t.Errorf("lease lost: n=%d err=%v lost=%d", n, err, obs.lost)
	}
}

// 저장된 정의를 해석하지 못하면 기존 group은 모두 EVALUATION_ERROR가 된다(이전 ALERT·값을 그대로 두지 않고, 사건은 유지)
func TestInvalidStoredSpec(t *testing.T) {
	clock := t0
	episode, reason, v := "0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f01", "violation", 0.05
	since := t0.Add(-time.Hour)
	store := &fakeStore{spec: []byte(`{"not":"a spec"}`), instances: []controldb.StoredInstance{{
		GroupKey: "", Labels: map[string]string{}, State: "ALERT", EpisodeID: &episode, EpisodeReason: &reason,
		ViolationSince: &since, OKStreak: 1, LastValue: &v,
	}}}
	w := newWorker(t, store, &fakeMetrics{watermark: t0}, &clock, &counter{})
	if _, err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := store.completed[0]
	if c.Status != "error" || c.Reason != "invalid_spec" || len(c.Groups) != 1 {
		t.Fatalf("completion = %+v", c)
	}
	g := c.Groups[0]
	if !g.Changed || g.From != "ALERT" || g.To != "EVALUATION_ERROR" || g.Instance.LastValue != nil || g.Instance.OKStreak != 0 ||
		g.Instance.EpisodeID == nil || *g.Instance.EpisodeID != episode || g.Instance.ViolationSince == nil || g.Opened || g.Closed {
		t.Errorf("group = %+v", g)
	}
	// 다음 slot도 같은 오류: 변화 없음(event 없음)
	clock = t0.Add(time.Minute)
	if _, err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g := store.completed[1].Groups[0]; g.Changed {
		t.Errorf("repeated error emitted change: %+v", g)
	}
}

// 조회가 시간 상한까지 돌아오지 않아도 EVALUATION_ERROR 결과는 저장된다(완료는 조회와 다른 context)
func TestQueryTimeoutIsRecorded(t *testing.T) {
	clock := t0
	store := &fakeStore{spec: canonical(t)}
	w, err := New(Config{Store: store, Metrics: &fakeMetrics{hang: true, watermark: t0}, Worker: "w1", Observer: &counter{},
		QueryTimeout: 30 * time.Millisecond, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := w.Tick(context.Background()); err != nil || n != 1 {
		t.Fatalf("tick: %d %v", n, err)
	}
	if c := store.completed[0]; c.Status != "error" || c.Reason != "query_failed" || c.Groups[0].Instance.State != "EVALUATION_ERROR" {
		t.Errorf("completion = %+v", c)
	}
}

func TestLeaseMustCoverTimeouts(t *testing.T) {
	if _, err := New(Config{Store: &fakeStore{}, Metrics: &fakeMetrics{}, Worker: "w", Lease: 10 * time.Second}); err == nil {
		t.Error("lease shorter than query + writes accepted")
	}
}

// 저장 실패는 monitor 하나만 실패로 세고(lease 만료 뒤 다시 시도) tick은 계속된다
func TestWriteFailureIsCounted(t *testing.T) {
	clock := t0
	store := &fakeStore{spec: canonical(t), failWrite: errors.New("pg down")}
	obs := &counter{}
	w := newWorker(t, store, &fakeMetrics{total: 1000, watermark: t0}, &clock, obs)
	if n, err := w.Tick(context.Background()); err != nil || n != 0 || obs.failed != 1 || obs.evals != 0 {
		t.Errorf("n=%d err=%v failed=%d evals=%d", n, err, obs.failed, obs.evals)
	}
}

// 종료(parent 취소)로 끊긴 조회는 결과를 쓰지 않는다: 종료는 조회 실패가 아니다(거짓 EVALUATION_ERROR·전이 없음)
func TestShutdownDoesNotRecordError(t *testing.T) {
	clock := t0
	store := &fakeStore{spec: canonical(t)}
	obs := &counter{}
	w := newWorker(t, store, &fakeMetrics{hang: true, watermark: t0}, &clock, obs)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	if n, err := w.Tick(ctx); err != nil || n != 0 {
		t.Errorf("tick: %d %v", n, err)
	}
	if len(store.completed) != 0 || obs.failed != 0 {
		t.Errorf("completed=%d failed=%d", len(store.completed), obs.failed)
	}
}
