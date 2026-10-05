//go:build integration

package telemetrystore

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
)

type m1m struct {
	tenant   string
	stream   [16]byte
	window   time.Time
	resource string
	attrs    string
	typ      string
	unit     string
	revision uint64
	increase float64
	hasInc   bool
	total    float64
	samples  uint32
	hasValue bool
	bounds   []float64
	buckets  []uint64
	count    uint64
	hasHist  bool
	flags    []string
	partial  bool
}

func insertM1m(t *testing.T, rows ...m1m) {
	t.Helper()
	c := rawConn(t, "MONTRACER_TEST_CH_ADMIN_DSN")
	b, err := c.PrepareBatch(context.Background(), `INSERT INTO metric_1m (tenant_id, metric_name, stream_id, window_start,
		type, temporality, is_monotonic, unit, resource_json, attributes_json, samples, has_value, last, min, max, total,
		has_increase, increase, has_histogram, count, hist_sum, bounds, buckets, resets, flags, partial, revision,
		computed_at, expires_at)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		bounds, buckets, flags := r.bounds, r.buckets, r.flags
		if bounds == nil {
			bounds = []float64{}
		}
		if buckets == nil {
			buckets = []uint64{}
		}
		if flags == nil {
			flags = []string{}
		}
		if err := b.Append(r.tenant, "it.metric", string(r.stream[:]), r.window, r.typ, "cumulative", true, r.unit,
			r.resource, r.attrs, r.samples, r.hasValue, r.total, r.total, r.total, r.total, r.hasInc, r.increase,
			r.hasHist, r.count, float64(r.count), bounds, buckets, uint32(0), flags, r.partial, r.revision,
			time.Now(), r.window.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Send(); err != nil {
		t.Fatal(err)
	}
}

func rnd16() [16]byte {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return b
}

// API key principal (environment 제한) — authz의 실제 인증 경로로 만든다.
func keyPrincipal(t *testing.T, tenant authz.TenantID, envs []string) authz.Principal {
	t.Helper()
	h, _ := authz.NewKeyHasher(bytes.Repeat([]byte{3}, 32))
	g, _ := h.Generate(authz.KindAPIKey, nil)
	rec := authz.KeyRecord{KeyID: g.KeyID, Tenant: tenant, Kind: authz.KindAPIKey, Hash: g.Hash, Scopes: []authz.Action{authz.TelemetryRead},
		Environments: envs, IssuerRole: authz.RoleTenantAdmin, ExpiresAt: time.Now().Add(time.Hour)}
	p, err := h.Authenticate(context.Background(), g.Token, authz.KindAPIKey, func(context.Context, string) (authz.KeyRecord, error) { return rec, nil }, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMetricBuckets(t *testing.T) {
	s := openQuery(t)
	tenant, pa := newTenant(t)
	other, _ := newTenant(t)
	w0 := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	prod := `{"deployment.environment.name":"prod","service.name":"checkout"}`
	stage := `{"deployment.environment.name":"stage","service.name":"checkout"}`
	sa, sb, sc := rnd16(), rnd16(), rnd16()
	insertM1m(t,
		// stream a (prod, route /cart): 4개 1분 window, 그중 하나는 revision이 다른 두 행 — 큰 revision만 쓴다
		m1m{tenant: tenant.String(), stream: sa, window: w0, resource: prod, attrs: `{"http.route":"/cart"}`, typ: "sum", unit: "1", revision: 1, increase: 10, hasInc: true},
		m1m{tenant: tenant.String(), stream: sa, window: w0.Add(time.Minute), resource: prod, attrs: `{"http.route":"/cart"}`, typ: "sum", unit: "1", revision: 1, increase: 999, hasInc: true},
		m1m{tenant: tenant.String(), stream: sa, window: w0.Add(time.Minute), resource: prod, attrs: `{"http.route":"/cart"}`, typ: "sum", unit: "1", revision: 2, increase: 20, hasInc: true},
		m1m{tenant: tenant.String(), stream: sa, window: w0.Add(5 * time.Minute), resource: prod, attrs: `{"http.route":"/cart"}`, typ: "sum", unit: "1", revision: 1, increase: 5, hasInc: true},
		// stream b (prod, route /pay)
		m1m{tenant: tenant.String(), stream: sb, window: w0, resource: prod, attrs: `{"http.route":"/pay"}`, typ: "sum", unit: "1", revision: 1, increase: 1, hasInc: true},
		// stream c (stage)
		m1m{tenant: tenant.String(), stream: sc, window: w0, resource: stage, attrs: `{"http.route":"/cart"}`, typ: "sum", unit: "1", revision: 1, increase: 100, hasInc: true},
		// 다른 tenant의 같은 metric
		m1m{tenant: other.String(), stream: sa, window: w0, resource: prod, attrs: `{"http.route":"/cart"}`, typ: "sum", unit: "1", revision: 9, increase: 7777, hasInc: true},
	)
	ctx := context.Background()
	now := time.Now()
	q := MetricQuery{Metric: "it.metric", StepSeconds: 300, Range: TimeRange{From: w0, To: w0.Add(10 * time.Minute)}, GroupBy: []string{"http.route"}}
	bs, err := s.MetricBuckets(ctx, pa, q, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, b := range bs {
		got[b.Group[0]+"@"+b.StepStart.Sub(w0).String()] = b.Increase
	}
	// /cart 0~5분 = 10 + 20(최신 revision) + stage 100, 5~10분 = 5. /pay = 1. 다른 tenant의 7777은 없다
	want := map[string]float64{"/cart@0s": 130, "/cart@5m0s": 5, "/pay@0s": 1}
	if len(got) != len(want) {
		t.Fatalf("buckets = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (all=%v)", k, got[k], v, got)
		}
	}

	// rollup watermark = 그 tenant의 가장 늦은 window 끝 (다른 tenant 행과 무관)
	wm, err := s.RollupWatermark(ctx, pa, w0.Add(-time.Hour), now)
	if err != nil || !wm.Equal(w0.Add(6*time.Minute)) {
		t.Errorf("rollup watermark = %v, %v (want w0+6m)", wm, err)
	}

	// label 필터 (point 속성 → 없으면 resource 속성)
	q.Filters = []LabelMatch{{Key: "deployment.environment.name", Value: "stage"}}
	bs, err = s.MetricBuckets(ctx, pa, q, now)
	if err != nil || len(bs) != 1 || bs[0].Increase != 100 {
		t.Errorf("filter stage = %+v, %v", bs, err)
	}

	// environment 제한 key: prod만 보인다 (stage 100 제외)
	q.Filters = nil
	q.GroupBy = nil
	pk := keyPrincipal(t, tenant, []string{"prod"})
	bs, err = s.MetricBuckets(ctx, pk, q, now)
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, b := range bs {
		total += b.Increase
	}
	if total != 36 { // 10 + 20 + 5 + 1
		t.Errorf("prod-only total = %v, want 36", total)
	}

	// 사용자 문자열은 binding으로만 들어간다: SQL 조각을 넣어도 일치하는 label이 없을 뿐이다
	q.Filters = []LabelMatch{{Key: "http.route') OR 1=1 --", Value: "x' OR '1'='1"}}
	bs, err = s.MetricBuckets(ctx, pa, q, now)
	if err != nil || len(bs) != 0 {
		t.Errorf("injection-like filter = %+v, %v", bs, err)
	}
}

func TestMetricBucketsHistogramMerge(t *testing.T) {
	s := openQuery(t)
	tenant, pa := newTenant(t)
	w0 := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	b := []float64{10, 100}
	insertM1m(t,
		m1m{tenant: tenant.String(), stream: rnd16(), window: w0, typ: "histogram", unit: "ms", revision: 1, hasHist: true, bounds: b, buckets: []uint64{5, 1, 0}, count: 6},
		m1m{tenant: tenant.String(), stream: rnd16(), window: w0, typ: "histogram", unit: "ms", revision: 1, hasHist: true, bounds: b, buckets: []uint64{1, 2, 3}, count: 6},
		m1m{tenant: tenant.String(), stream: rnd16(), window: w0.Add(time.Minute), typ: "histogram", unit: "ms", revision: 1, hasHist: true, bounds: []float64{5}, buckets: []uint64{1, 1}, count: 2},
		m1m{tenant: tenant.String(), stream: rnd16(), window: w0.Add(time.Minute), typ: "histogram", unit: "ms", revision: 1, hasHist: true, bounds: b, buckets: []uint64{1, 1, 1}, count: 3},
	)
	bs, err := s.MetricBuckets(context.Background(), pa, MetricQuery{Metric: "it.metric", StepSeconds: 60,
		Range: TimeRange{From: w0, To: w0.Add(2 * time.Minute)}}, time.Now())
	if err != nil || len(bs) != 2 {
		t.Fatalf("buckets = %+v, %v", bs, err)
	}
	if bs[0].BoundsVariants != 1 || bs[0].Count != 12 || len(bs[0].Buckets) != 3 || bs[0].Buckets[0] != 6 || bs[0].Buckets[1] != 3 || bs[0].Buckets[2] != 3 {
		t.Errorf("merged = %+v", bs[0])
	}
	if bs[1].BoundsVariants != 2 {
		t.Errorf("mixed bounds not detected: %+v", bs[1])
	}
}
