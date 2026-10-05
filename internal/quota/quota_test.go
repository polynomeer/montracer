package quota

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const (
	tenantA = "11111111-1111-4111-8111-111111111111"
	tenantB = "22222222-2222-4222-8222-222222222222"
)

var t0 = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

func small() Limits {
	return Limits{RecordsPerSecond: 100, RecordsBurst: 200, BytesPerSecond: 1000, BytesBurst: 2000}
}

func TestBurstThenRateLimited(t *testing.T) {
	l := New(Config{Default: small()})
	if d := l.Allow(tenantA, "traces", 200, 100, t0); d.Outcome != Allowed {
		t.Fatalf("first = %+v", d)
	}
	d := l.Allow(tenantA, "traces", 50, 100, t0)
	if d.Outcome != RateLimited || d.Limit != "records" || d.RetryAfter != 500*time.Millisecond {
		t.Fatalf("second = %+v, want records 0.5s", d)
	}
	// 0.5초 뒤에는 통과
	if d := l.Allow(tenantA, "traces", 50, 100, t0.Add(500*time.Millisecond)); d.Outcome != Allowed {
		t.Fatalf("after wait = %+v", d)
	}
}

// 두 bucket 중 하나라도 부족하면 아무것도 소비하지 않는다.
func TestAllOrNothing(t *testing.T) {
	l := New(Config{Default: small()})
	l.Allow(tenantA, "logs", 0, 2000, t0) // byte bucket 소진
	d := l.Allow(tenantA, "logs", 150, 10, t0)
	if d.Outcome != RateLimited || d.Limit != "bytes" {
		t.Fatalf("= %+v", d)
	}
	// record bucket은 소비되지 않았어야 한다: byte가 차면 150 record가 바로 통과
	if d := l.Allow(tenantA, "logs", 150, 10, t0.Add(10*time.Millisecond)); d.Outcome != Allowed {
		t.Fatalf("record tokens were consumed by a rejected request: %+v", d)
	}
}

func TestOverBurstIsPermanent(t *testing.T) {
	l := New(Config{Default: small()})
	if d := l.Allow(tenantA, "traces", 201, 1, t0); d.Outcome != OverBurst || d.Limit != "records" {
		t.Fatalf("records = %+v", d)
	}
	if d := l.Allow(tenantA, "traces", 1, 2001, t0); d.Outcome != OverBurst || d.Limit != "bytes" {
		t.Fatalf("bytes = %+v", d)
	}
	// 거절이 token을 쓰지 않았다
	if d := l.Allow(tenantA, "traces", 200, 2000, t0); d.Outcome != Allowed {
		t.Fatalf("after over-burst = %+v", d)
	}
}

// noisy neighbor: 한 tenant·signal의 소진이 다른 tenant나 다른 signal에 영향을 주지 않는다 (D06 §03 Noisy tenant).
func TestIsolationBetweenTenantsAndSignals(t *testing.T) {
	l := New(Config{Default: small()})
	l.Allow(tenantA, "traces", 200, 2000, t0)
	if d := l.Allow(tenantA, "traces", 1, 1, t0); d.Outcome != RateLimited {
		t.Fatalf("A traces = %+v", d)
	}
	if d := l.Allow(tenantB, "traces", 200, 2000, t0); d.Outcome != Allowed {
		t.Errorf("B traces = %+v", d)
	}
	if d := l.Allow(tenantA, "logs", 200, 2000, t0); d.Outcome != Allowed {
		t.Errorf("A logs = %+v", d)
	}
}

// global 전략: rate는 replica 수로 나누고 burst는 나누지 않는다 (burst를 나누면 replica가 많을 때
// 최대 요청보다 작아져 정상 batch가 413을 받는다).
func TestReplicasDivideRateNotBurst(t *testing.T) {
	l := New(Config{Default: small(), Replicas: 4})
	if d := l.Allow(tenantA, "traces", 200, 2000, t0); d.Outcome != Allowed {
		t.Fatalf("full burst = %+v", d)
	}
	d := l.Allow(tenantA, "traces", 25, 1, t0)
	if d.Outcome != RateLimited || d.RetryAfter != time.Second || d.RatePerReplica != 25 || d.Replicas != 4 || d.Burst != 200 {
		t.Errorf("rate/4 = 25/s → 1s, got %+v", d)
	}
}

// 동시에 거절된 요청이 token을 잃게 하지 않는다 (x/time/rate CancelAt의 한계를 피한 구현).
func TestConcurrentRejectionsDoNotLeakTokens(t *testing.T) {
	l := New(Config{Default: small()})
	l.Allow(tenantA, "traces", 200, 0, t0) // 소진
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() { defer wg.Done(); l.Allow(tenantA, "traces", 150, 10, t0.Add(time.Second)) }()
	}
	wg.Wait()
	// 1초 동안 100개 적립 → 위 요청은 모두 거절(150 > 100). 2초 시점에는 200이 가득 차 있어야 한다.
	if d := l.Allow(tenantA, "traces", 200, 10, t0.Add(2*time.Second)); d.Outcome != Allowed {
		t.Fatalf("tokens leaked by concurrent rejections: %+v", d)
	}
}

// 시각이 뒤섞여 들어와도 같은 구간을 두 번 적립하지 않는다.
func TestNonMonotonicTimeDoesNotOverAdmit(t *testing.T) {
	l := New(Config{Default: small()})
	l.Allow(tenantA, "traces", 200, 0, t0.Add(time.Second))
	l.Allow(tenantA, "traces", 1, 0, t0) // 과거 시각: 적립 시각을 되돌리지 않는다
	if d := l.Allow(tenantA, "traces", 100, 0, t0.Add(time.Second)); d.Outcome != RateLimited {
		t.Fatalf("time rewind re-credited tokens: %+v", d)
	}
}

func TestOverridesApplyAndChange(t *testing.T) {
	var mu sync.Mutex
	o := Overrides{tenantA: {"traces": {RecordsPerSecond: 1000, RecordsBurst: 1000, BytesPerSecond: 1000, BytesBurst: 1000}}}
	l := New(Config{Default: small(), Overrides: func() Overrides { mu.Lock(); defer mu.Unlock(); return o }})
	if d := l.Allow(tenantA, "traces", 1000, 1, t0); d.Outcome != Allowed {
		t.Fatalf("override = %+v", d)
	}
	if d := l.Allow(tenantB, "traces", 1000, 1, t0); d.Outcome != OverBurst {
		t.Fatalf("B uses default = %+v", d)
	}
	mu.Lock()
	o = Overrides{}
	mu.Unlock()
	if d := l.Allow(tenantA, "traces", 201, 1, t0.Add(time.Second)); d.Outcome != OverBurst {
		t.Fatalf("after override removed = %+v", d)
	}
}

// byte burst 하한: overrides가 최대 요청보다 낮게 잡아도 정상 요청이 413이 되지 않는다.
func TestMinBytesBurstFloor(t *testing.T) {
	o := Overrides{tenantA: {"logs": {RecordsPerSecond: 10, RecordsBurst: 10, BytesPerSecond: 10, BytesBurst: 10}}}
	l := New(Config{Default: small(), MinBytesBurst: 8 << 20, Overrides: func() Overrides { return o }})
	if d := l.Allow(tenantA, "logs", 1, 8<<20, t0); d.Outcome != Allowed {
		t.Fatalf("8MiB request = %+v", d)
	}
}

func TestIdleBucketsEvicted(t *testing.T) {
	l := New(Config{Default: small(), IdleTTL: time.Minute})
	l.Allow(tenantA, "traces", 1, 1, t0)
	l.Allow(tenantB, "traces", 1, 1, t0.Add(5*time.Minute)) // sweep
	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n != 1 {
		t.Errorf("buckets = %d, want 1 (idle A evicted)", n)
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	for d, want := range map[time.Duration]int{0: 1, 300 * time.Millisecond: 1, 1500 * time.Millisecond: 2, 10 * time.Minute: 60} {
		if got := RetryAfterSeconds(d); got != want {
			t.Errorf("RetryAfterSeconds(%v) = %d, want %d", d, got, want)
		}
	}
}

func TestParseOverrides(t *testing.T) {
	good := `{"tenants":{"` + tenantA + `":{"traces":{"records_per_second":1,"records_burst":2,"bytes_per_second":3,"bytes_burst":4}}}}`
	if o, err := ParseOverrides([]byte(good)); err != nil || o[tenantA]["traces"].BytesBurst != 4 {
		t.Fatalf("good: %v %v", o, err)
	}
	for name, bad := range map[string]string{
		"unknown field":  `{"tenants":{},"x":1}`,
		"bad tenant":     `{"tenants":{"acme":{}}}`,
		"upper tenant":   `{"tenants":{"11111111-1111-4111-8111-11111111111A":{}}}`,
		"unknown signal": `{"tenants":{"` + tenantA + `":{"profiles":{"records_per_second":1,"records_burst":1,"bytes_per_second":1,"bytes_burst":1}}}}`,
		"partial limits": `{"tenants":{"` + tenantA + `":{"traces":{"records_per_second":1}}}}`,
		"not json":       `tenants: {}`,
	} {
		if _, err := ParseOverrides([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// 잘못 편집된 파일은 적용하지 않고 직전 값을 유지한다.
func TestFileOverridesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrides.json")
	write := func(s string, mod time.Time) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"tenants":{"`+tenantA+`":{"logs":{"records_per_second":5,"records_burst":5,"bytes_per_second":5,"bytes_burst":5}}}}`, t0)
	f, err := LoadFileOverrides(path)
	if err != nil || f.Get()[tenantA]["logs"].RecordsBurst != 5 {
		t.Fatalf("load: %v", err)
	}
	write(`{"tenants": broken`, t0.Add(time.Minute))
	if changed, err := f.reload(); err == nil || changed {
		t.Fatalf("broken file applied: changed=%v err=%v", changed, err)
	}
	if f.Get()[tenantA]["logs"].RecordsBurst != 5 {
		t.Error("previous overrides lost after broken edit")
	}
	write(`{"tenants":{}}`, t0.Add(2*time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	reloaded := make(chan struct{}, 1)
	go f.Watch(ctx, 10*time.Millisecond, func() { reloaded <- struct{}{} }, nil)
	select {
	case <-reloaded:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not reload")
	}
	cancel()
	if len(f.Get()) != 0 {
		t.Errorf("overrides = %v", f.Get())
	}
	if _, err := LoadFileOverrides(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file must fail at startup")
	}
}
