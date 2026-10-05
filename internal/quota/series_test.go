package quota

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetry/envelope"
)

type fakeSeries struct {
	values    map[LabelValue]time.Time
	failTouch bool
	rows      map[[16]byte]time.Time
	known     int // Known 호출 수
	count     int // ActiveCount 호출 수
	failNext  bool
}

func (f *fakeSeries) Known(_ context.Context, _ authz.TenantID, ids [][16]byte, since time.Time) (map[[16]byte]bool, error) {
	f.known++
	if f.failNext {
		return nil, errors.New("pg down")
	}
	out := map[[16]byte]bool{}
	for _, id := range ids {
		if t, ok := f.rows[id]; ok && t.After(since) {
			out[id] = true
		}
	}
	return out, nil
}

func (f *fakeSeries) ActiveCount(_ context.Context, _ authz.TenantID, since time.Time) (int, error) {
	f.count++
	n := 0
	for _, t := range f.rows {
		if t.After(since) {
			n++
		}
	}
	return n, nil
}

func (f *fakeSeries) KnownValues(_ context.Context, _ authz.TenantID, vals []LabelValue, since time.Time) (map[LabelValue]bool, error) {
	out := map[LabelValue]bool{}
	for _, v := range vals {
		if t, ok := f.values[v]; ok && t.After(since) {
			out[v] = true
		}
	}
	return out, nil
}

func (f *fakeSeries) ValueCounts(_ context.Context, _ authz.TenantID, keys []LabelKey, since time.Time) (map[LabelKey]int, error) {
	out := map[LabelKey]int{}
	want := map[LabelKey]bool{}
	for _, k := range keys {
		want[k] = true
	}
	for v, t := range f.values {
		if k := (LabelKey{v.Metric, v.Key}); want[k] && t.After(since) {
			out[k]++
		}
	}
	return out, nil
}

func (f *fakeSeries) TouchValues(_ context.Context, _ authz.TenantID, vals []LabelValue, now time.Time) error {
	if f.values == nil {
		f.values = map[LabelValue]time.Time{}
	}
	for _, v := range vals {
		f.values[v] = now
	}
	return nil
}

func (f *fakeSeries) Touch(_ context.Context, _ authz.TenantID, ids [][16]byte, _ []string, now time.Time) error {
	if f.failTouch {
		return errors.New("pg down")
	}
	for _, id := range ids {
		f.rows[id] = now
	}
	return nil
}

func ref(i uint8) envelope.StreamRef {
	return envelope.StreamRef{StreamID: [16]byte{i}, Metric: "m", Attributes: pcommon.NewMap(), Resource: pcommon.NewMap(), Scope: pcommon.NewMap()}
}

var tA, _ = authz.ParseTenantID(tenantA)

// 한도에 닿으면 기존 series는 계속 받고 신규만 거절한다 (D02 §10).
func TestSeriesLimitKeepsExisting(t *testing.T) {
	store := &fakeSeries{rows: map[[16]byte]time.Time{}}
	l := NewSeriesLimiter(SeriesConfig{Store: store, Default: 3})
	rej, err := l.Admit(context.Background(), tA, []envelope.StreamRef{ref(1), ref(2), ref(3)}, t0)
	if err != nil || (rej[0] != "") || (rej[1] != "") || (rej[2] != "") {
		t.Fatalf("under limit: %v %v", rej, err)
	}
	// 기존 2개 + 신규 2개(한 series는 point 두 개) → 신규 모두 거절, 기존 통과
	rej, err = l.Admit(context.Background(), tA, []envelope.StreamRef{ref(1), ref(4), ref(2), ref(5), ref(4)}, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{false, true, false, true, true}
	for i := range want {
		if (rej[i] != "") != want[i] {
			t.Fatalf("rejected = %v, want %v", rej, want)
		}
	}
	if len(store.rows) != 3 {
		t.Errorf("registry rows = %d, rejected series must not be registered", len(store.rows))
	}
}

// 다른 replica가 등록한 series도 기존 series로 받는다(등록부 공유).
func TestSeriesKnownFromRegistry(t *testing.T) {
	store := &fakeSeries{rows: map[[16]byte]time.Time{{9}: t0}}
	l := NewSeriesLimiter(SeriesConfig{Store: store, Default: 1})
	rej, err := l.Admit(context.Background(), tA, []envelope.StreamRef{{StreamID: [16]byte{9}, Metric: "m", Attributes: pcommon.NewMap(), Resource: pcommon.NewMap()}}, t0.Add(time.Minute))
	if err != nil || (rej[0] != "") {
		t.Fatalf("series registered by another replica rejected: %v %v", rej, err)
	}
}

// 아는 series는 cache로 판정해 등록부를 다시 조회하지 않는다(TouchEvery 안).
func TestSeriesCache(t *testing.T) {
	store := &fakeSeries{rows: map[[16]byte]time.Time{}}
	l := NewSeriesLimiter(SeriesConfig{Store: store})
	refs := []envelope.StreamRef{ref(1), ref(2)}
	_, _ = l.Admit(context.Background(), tA, refs, t0)
	calls := store.known
	_, _ = l.Admit(context.Background(), tA, refs, t0.Add(5*time.Minute))
	if store.known != calls {
		t.Errorf("registry queried for cached series")
	}
	_, _ = l.Admit(context.Background(), tA, refs, t0.Add(11*time.Minute)) // TouchEvery 지남 → last_seen 갱신
	if store.known != calls+1 || !store.rows[refs[0].StreamID].Equal(t0.Add(11*time.Minute)) {
		t.Errorf("stale cache not refreshed: known=%d", store.known)
	}
}

// 비활성(1시간)이 된 series는 다시 신규로 센다.
func TestSeriesExpiresAfterActiveWindow(t *testing.T) {
	store := &fakeSeries{rows: map[[16]byte]time.Time{}}
	l := NewSeriesLimiter(SeriesConfig{Store: store, Default: 1})
	_, _ = l.Admit(context.Background(), tA, []envelope.StreamRef{ref(1)}, t0)
	rej, _ := l.Admit(context.Background(), tA, []envelope.StreamRef{ref(2)}, t0.Add(2*time.Hour))
	if rej[0] != "" {
		t.Error("series 1 is inactive after 1h; series 2 should fit")
	}
}

func TestSeriesRegistryErrorPropagates(t *testing.T) {
	store := &fakeSeries{rows: map[[16]byte]time.Time{}, failNext: true}
	l := NewSeriesLimiter(SeriesConfig{Store: store})
	if _, err := l.Admit(context.Background(), tA, []envelope.StreamRef{ref(1)}, t0); err == nil {
		t.Error("registry error must not be swallowed (caller fails closed)")
	}
}

func TestSeriesLimitOverride(t *testing.T) {
	store := &fakeSeries{rows: map[[16]byte]time.Time{}}
	l := NewSeriesLimiter(SeriesConfig{Store: store, Default: 1, Limit: func(string) int { return 2 }})
	rej, _ := l.Admit(context.Background(), tA, []envelope.StreamRef{ref(1), ref(2), ref(3)}, t0)
	if rej[0] != "" || rej[1] != "" || rej[2] == "" {
		t.Errorf("override 2: %v", rej)
	}
}

func TestCheckDimensions(t *testing.T) {
	mk := func(kv ...string) envelope.StreamRef {
		a := pcommon.NewMap()
		for i := 0; i+1 < len(kv); i += 2 {
			a.PutStr(kv[i], kv[i+1])
		}
		return envelope.StreamRef{Metric: "m", Attributes: a, Resource: pcommon.NewMap(), Scope: pcommon.NewMap()}
	}
	if r := CheckDimensions(mk("http.route", "/cart")); r != "" {
		t.Errorf("normal = %q", r)
	}
	for _, k := range []string{"user_id", "User.Id", "session.id", "request_id", "trace_id", "enduser.id"} {
		if r := CheckDimensions(mk(k, "x")); r != ReasonForbiddenDimension {
			t.Errorf("%s = %q", k, r)
		}
	}
	res := envelope.StreamRef{Metric: "m", Attributes: pcommon.NewMap(), Resource: pcommon.NewMap(), Scope: pcommon.NewMap()}
	res.Resource.PutStr("session_id", "x")
	if CheckDimensions(res) != ReasonForbiddenDimension {
		t.Error("forbidden key in resource not caught")
	}
	scope := mk("http.route", "/cart")
	scope.Scope = pcommon.NewMap()
	scope.Scope.PutStr("request_id", "x")
	if CheckDimensions(scope) != ReasonForbiddenDimension {
		t.Error("forbidden key in scope not caught")
	}
	long := mk()
	long.Metric = strings.Repeat("m", MaxMetricNameLen+1)
	if CheckDimensions(long) != ReasonInvalidMetricName {
		t.Error("long metric name not rejected (등록부 제약 위반 전에 거절)")
	}
	many := mk()
	for i := range MaxMetricLabels + 1 {
		many.Attributes.PutStr("k"+strconv.Itoa(i), "v")
	}
	if CheckDimensions(many) != ReasonTooManyLabels {
		t.Error("21 labels not rejected")
	}
}

func TestOverridesActiveSeriesValidation(t *testing.T) {
	base := `"records_per_second":1,"records_burst":1,"bytes_per_second":1,"bytes_burst":1`
	if o, err := ParseOverrides([]byte(`{"tenants":{"` + tenantA + `":{"metrics":{` + base + `,"active_series":500000}}}}`)); err != nil || o[tenantA]["metrics"].ActiveSeries != 500000 {
		t.Fatalf("metrics active_series: %v %v", o, err)
	}
	for _, bad := range []string{
		`{"tenants":{"` + tenantA + `":{"logs":{` + base + `,"active_series":10}}}}`,
		`{"tenants":{"` + tenantA + `":{"metrics":{` + base + `,"active_series":-1}}}}`,
	} {
		if _, err := ParseOverrides([]byte(bad)); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
}

// 등록 실패 시 세어 둔 자리를 돌려준다 — 다음 요청이 너무 일찍 거절되지 않게.
func TestSeriesTouchFailureReleasesRoom(t *testing.T) {
	store := &fakeSeries{rows: map[[16]byte]time.Time{}, failTouch: true}
	l := NewSeriesLimiter(SeriesConfig{Store: store, Default: 1})
	if _, err := l.Admit(context.Background(), tA, []envelope.StreamRef{ref(1)}, t0); err == nil {
		t.Fatal("expected error")
	}
	store.failTouch = false
	rej, err := l.Admit(context.Background(), tA, []envelope.StreamRef{ref(2)}, t0.Add(time.Second))
	if err != nil || (rej[0] != "") {
		t.Errorf("room not released after failed touch: %v %v", rej, err)
	}
}

func valueRef(id uint8, key, value string) envelope.StreamRef {
	a := pcommon.NewMap()
	a.PutStr(key, value)
	return envelope.StreamRef{StreamID: [16]byte{id}, Metric: "http.requests", Attributes: a, Resource: pcommon.NewMap(), Scope: pcommon.NewMap()}
}

// key당 활성 값 상한 (D02 §10): 새 값이 상한을 넘기는 신규 series만 거절하고, 이미 있는 값의 신규 series와 기존 series는 받는다.
func TestLabelValuesPerKey(t *testing.T) {
	store := &fakeSeries{rows: map[[16]byte]time.Time{}}
	l := NewSeriesLimiter(SeriesConfig{Store: store, ValuesPerKey: 2})
	ctx := context.Background()
	rej, err := l.Admit(ctx, tA, []envelope.StreamRef{valueRef(1, "route", "/a"), valueRef(2, "route", "/b")}, t0)
	if err != nil || rej[0] != "" || rej[1] != "" {
		t.Fatalf("first two values: %v %v", rej, err)
	}
	// 세 번째 값 → 거절. 이미 있는 값(/a)으로 만든 다른 신규 series(다른 stream)는 받는다. 기존 series 1은 그대로 받는다.
	rej, err = l.Admit(ctx, tA, []envelope.StreamRef{valueRef(3, "route", "/c"), valueRef(4, "route", "/a"), valueRef(1, "route", "/a")}, t0.Add(time.Minute))
	if err != nil || rej[0] != ReasonLabelValueLimit || rej[1] != "" || rej[2] != "" {
		t.Fatalf("third value: %v %v", rej, err)
	}
	if _, ok := store.rows[[16]byte{3}]; ok {
		t.Error("rejected series registered")
	}
	// 같은 요청 안에서 같은 새 값을 쓰는 두 series는 자리 하나만 쓴다
	l2 := NewSeriesLimiter(SeriesConfig{Store: &fakeSeries{rows: map[[16]byte]time.Time{}}, ValuesPerKey: 1})
	rej, _ = l2.Admit(ctx, tA, []envelope.StreamRef{valueRef(1, "route", "/x"), valueRef(2, "route", "/x"), valueRef(3, "route", "/y")}, t0)
	if rej[0] != "" || rej[1] != "" || rej[2] != ReasonLabelValueLimit {
		t.Errorf("shared new value: %v", rej)
	}
}

// 기존 series를 갱신할 때 그 값들의 활성 시각도 갱신한다 — 값이 비활성으로 빠져 상한이 느슨해지지 않게.
func TestExistingSeriesKeepsValuesActive(t *testing.T) {
	store := &fakeSeries{rows: map[[16]byte]time.Time{}}
	l := NewSeriesLimiter(SeriesConfig{Store: store, ValuesPerKey: 1})
	ctx := context.Background()
	_, _ = l.Admit(ctx, tA, []envelope.StreamRef{valueRef(1, "route", "/a")}, t0)
	_, _ = l.Admit(ctx, tA, []envelope.StreamRef{valueRef(1, "route", "/a")}, t0.Add(50*time.Minute)) // 10분 넘어 갱신
	rej, _ := l.Admit(ctx, tA, []envelope.StreamRef{valueRef(2, "route", "/b")}, t0.Add(70*time.Minute))
	if rej[0] != ReasonLabelValueLimit {
		t.Errorf("value /a expired despite active series: %v", rej)
	}
}
