package query

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

type fakeMetrics struct {
	window   time.Duration // RollupWatermark가 받은 해상도
	coverage time.Time     // 1시간 rollup이 덮는 가장 이른 window (zero면 m0 − 1일)
	// watermark가 zero면 모든 step이 계산 완료된 것으로 본다(m0 + 1시간)
	watermark time.Time
	buckets   []telemetrystore.MetricBucket
	got       telemetrystore.MetricQuery
	err       error
}

func (f *fakeMetrics) RollupWatermark(_ context.Context, _ authz.Principal, w time.Duration, _, _ time.Time) (time.Time, error) {
	f.window = w
	if f.watermark.IsZero() {
		return m0.Add(time.Hour), nil
	}
	return f.watermark, nil
}

func (f *fakeMetrics) RollupCoverageStart(context.Context, authz.Principal, time.Duration, time.Time) (time.Time, error) {
	if f.coverage.IsZero() {
		return m0.Add(-24 * time.Hour), nil
	}
	return f.coverage, nil
}

func (f *fakeMetrics) MetricBuckets(_ context.Context, p authz.Principal, q telemetrystore.MetricQuery, _ time.Time) ([]telemetrystore.MetricBucket, error) {
	f.got = q
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, err
	}
	return f.buckets, f.err
}

var m0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func metricHandler(t *testing.T, k *keys, m *fakeMetrics) *Handler {
	t.Helper()
	h := newHandler(t, k, &fakeStore{})
	h.cfg.Metrics = m
	return h
}

func postMetrics(h http.Handler, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/query/metrics", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// body는 D02 §13 형식(range, expression, step_seconds)이다. extra는 expression 안에 들어간다.
func body(agg string, extra string) string {
	return `{"range":{"from":"2026-10-05T12:00:10Z","to":"2026-10-05T12:04:30Z"},"step_seconds":60,` +
		`"expression":{"metric":"http.requests","aggregation":"` + agg + `"` + extra + `}}`
}

func counterBucket(group string, minute int, inc float64) telemetrystore.MetricBucket {
	return telemetrystore.MetricBucket{Group: []string{group}, StepStart: m0.Add(time.Duration(minute) * time.Minute),
		Types: []string{"sum"}, Units: []string{"1"}, Windows: 1, Streams: 1, IncreaseWindows: 1, Increase: inc}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) metricResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	var r metricResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// 범위는 step 경계로 맞추고, 없는 step은 null + no_data, 연속 결측은 한 구간, completeness는 값 있는 step 비율.
func TestMetricRateWithGaps(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	m := &fakeMetrics{buckets: []telemetrystore.MetricBucket{counterBucket("checkout", 0, 120), counterBucket("checkout", 3, 60)}}
	r := decode(t, postMetrics(metricHandler(t, k, m), tok, body("rate", `,"group_by":["service.name"],"filter":{"op":"and","args":[{"field":"env","op":"eq","value":"prod"}]}`)))
	if !m.got.Range.From.Equal(m0) || !m.got.Range.To.Equal(m0.Add(5*time.Minute)) {
		t.Errorf("aligned range = %v ~ %v", m.got.Range.From, m.got.Range.To)
	}
	if len(m.got.Filters) != 1 || m.got.Filters[0] != (telemetrystore.LabelMatch{Key: "env", Value: "prod"}) || m.got.GroupBy[0] != "service.name" {
		t.Errorf("query = %+v", m.got)
	}
	s := r.Data.Series[0]
	if s.Labels["service.name"] != "checkout" || len(s.Points) != 5 || *s.Unit != "1" {
		t.Fatalf("series = %+v", s)
	}
	if *s.Points[0].V != 2 || s.Points[1].V != nil || s.Points[1].Reason != ReasonNoData || *s.Points[3].V != 1 {
		t.Errorf("points = %+v (rate = increase / 60s)", s.Points)
	}
	if s.Completeness != 0.4 || len(s.Missing) != 2 || !s.Missing[0].From.Equal(m0.Add(time.Minute)) || !s.Missing[0].To.Equal(m0.Add(3*time.Minute)) {
		t.Errorf("completeness=%v missing=%+v", s.Completeness, s.Missing)
	}
	if r.Meta.ResolutionSeconds == nil || *r.Meta.ResolutionSeconds != 60 || r.Meta.Sampled != nil || r.Meta.Watermark == nil {
		t.Errorf("meta = %+v (metric은 sampled=null)", r.Meta)
	}
}

// 기준점 없는 counter window는 0이 아니라 null + missing_baseline이다.
func TestMetricMissingBaselineIsNotZero(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	b := counterBucket("", 0, 0)
	b.IncreaseWindows, b.Partial, b.Flags = 0, true, []string{"missing_baseline"}
	r := decode(t, postMetrics(metricHandler(t, k, &fakeMetrics{buckets: []telemetrystore.MetricBucket{b}}), tok, body("increase", "")))
	p := r.Data.Series[0].Points[0]
	if p.V != nil || p.Reason != ReasonMissingBaseline || !p.Partial {
		t.Errorf("point = %+v", p)
	}
}

// histogram 분위수는 bucket을 병합한 뒤 계산한다. 경계가 섞이면 값을 내지 않는다.
func TestMetricQuantileAndBoundsMismatch(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	h := telemetrystore.MetricBucket{Group: []string{}, StepStart: m0, Types: []string{"histogram"}, Units: []string{"ms"}, Windows: 2,
		HistogramWindows: 2, Count: 1010, BoundsVariants: 1, Bounds: []float64{10, 100, 900, 1000}, Buckets: []uint64{1000, 0, 0, 10, 0}}
	mixed := h
	mixed.StepStart, mixed.BoundsVariants = m0.Add(time.Minute), 2
	r := decode(t, postMetrics(metricHandler(t, k, &fakeMetrics{buckets: []telemetrystore.MetricBucket{h, mixed}}), tok, body("p95", "")))
	pts := r.Data.Series[0].Points
	if pts[0].V == nil || *pts[0].V > 10 {
		t.Errorf("merged p95 = %+v (instance별 p95 평균이면 ~500)", pts[0])
	}
	if pts[1].V != nil || pts[1].Reason != ReasonBoundsMismatch {
		t.Errorf("mixed bounds = %+v", pts[1])
	}
}

// step이 범위 길이와 같고 시작이 분 경계면 범위 전체를 한 점으로 집계한다(요약 값, ADR 0042).
// epoch 정렬을 하면 두 step으로 갈라져 분위수를 합칠 수 없다. 시작이 시간 경계가 아니면 1분 rollup을 읽는다.
func TestMetricWholeRangeSingleStep(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	req := func(from, to string, step int) string {
		return fmt.Sprintf(`{"range":{"from":%q,"to":%q},"step_seconds":%d,"expression":{"metric":"http.requests","aggregation":"p95"}}`, from, to, step)
	}
	cases := []struct {
		name, from, to   string
		step             int
		wantFrom, wantTo string
		wantWindow       time.Duration
	}{
		{"분 경계, 시간 경계 아님", "2026-10-05T12:07:00Z", "2026-10-05T13:07:00Z", 3600, "2026-10-05T12:07:00Z", "2026-10-05T13:07:00Z", time.Minute},
		{"시간 경계", "2026-10-05T12:00:00Z", "2026-10-05T13:00:00Z", 3600, "2026-10-05T12:00:00Z", "2026-10-05T13:00:00Z", time.Hour},
		{"분 경계 아님 → 정렬", "2026-10-05T12:07:30Z", "2026-10-05T13:07:30Z", 3600, "2026-10-05T12:00:00Z", "2026-10-05T14:00:00Z", time.Hour},
		{"범위가 step보다 김 → 정렬", "2026-10-05T12:07:00Z", "2026-10-05T14:07:00Z", 3600, "2026-10-05T12:00:00Z", "2026-10-05T15:00:00Z", time.Hour},
	}
	for _, c := range cases {
		m := &fakeMetrics{coverage: m0.Add(-48 * time.Hour)}
		rec := postMetrics(metricHandler(t, k, m), tok, req(c.from, c.to, c.step))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
		if got := m.got.Range; got.From.Format(time.RFC3339) != c.wantFrom || got.To.Format(time.RFC3339) != c.wantTo {
			t.Errorf("%s: range = %v..%v", c.name, got.From, got.To)
		}
		if m.window != c.wantWindow {
			t.Errorf("%s: window = %v", c.name, m.window)
		}
	}
}

// hist_sum은 histogram 관측값의 합이다(endpoint total time). 다른 유형이면 0이 아니라 not_applicable이다.
func TestMetricHistogramSum(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	h := telemetrystore.MetricBucket{Group: []string{}, StepStart: m0, Types: []string{"histogram"}, Units: []string{"s"}, Windows: 2,
		HistogramWindows: 2, Count: 4, HistSum: 1.25, BoundsVariants: 1, Bounds: []float64{1}, Buckets: []uint64{4, 0}}
	r := decode(t, postMetrics(metricHandler(t, k, &fakeMetrics{buckets: []telemetrystore.MetricBucket{h}}), tok, body("hist_sum", "")))
	if p := r.Data.Series[0].Points[0]; p.V == nil || *p.V != 1.25 {
		t.Errorf("hist_sum = %+v", p)
	}
	r = decode(t, postMetrics(metricHandler(t, k, &fakeMetrics{buckets: []telemetrystore.MetricBucket{counterBucket("", 0, 10)}}), tok, body("hist_sum", "")))
	if p := r.Data.Series[0].Points[0]; p.V != nil || p.Reason != ReasonNotApplicable {
		t.Errorf("hist_sum on counter = %+v", p)
	}
}

func TestMetricGaugeAndNotApplicable(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	g := telemetrystore.MetricBucket{Group: []string{}, StepStart: m0, Types: []string{"gauge"}, Units: []string{"By"}, Windows: 2,
		ValueWindows: 2, Total: 30, Samples4Avg: 3, Min: 5, Max: 15}
	for agg, want := range map[string]float64{"avg": 10, "min": 5, "max": 15} {
		r := decode(t, postMetrics(metricHandler(t, k, &fakeMetrics{buckets: []telemetrystore.MetricBucket{g}}), tok, body(agg, "")))
		if v := r.Data.Series[0].Points[0].V; v == nil || *v != want {
			t.Errorf("%s = %v", agg, v)
		}
	}
	// gauge에 rate를 임의로 적용하지 않는다 (D02 §07)
	r := decode(t, postMetrics(metricHandler(t, k, &fakeMetrics{buckets: []telemetrystore.MetricBucket{g}}), tok, body("rate", "")))
	if p := r.Data.Series[0].Points[0]; p.V != nil || p.Reason != ReasonNotApplicable {
		t.Errorf("rate on gauge = %+v", p)
	}
	// 단위가 다른 stream이 섞이면 값을 내지 않는다
	g.Units = []string{"By", "KiBy"}
	r = decode(t, postMetrics(metricHandler(t, k, &fakeMetrics{buckets: []telemetrystore.MetricBucket{g}}), tok, body("avg", "")))
	if p := r.Data.Series[0].Points[0]; p.V != nil || p.Reason != ReasonUnitConflict || r.Data.Series[0].Unit != nil {
		t.Errorf("unit conflict = %+v unit=%v", p, r.Data.Series[0].Unit)
	}
	// NaN(보내지 않은 sum 등)은 JSON 값으로 쓰지 않는다
	g.Units, g.Total = []string{"By"}, math.NaN()
	r = decode(t, postMetrics(metricHandler(t, k, &fakeMetrics{buckets: []telemetrystore.MetricBucket{g}}), tok, body("avg", "")))
	if p := r.Data.Series[0].Points[0]; p.V != nil {
		t.Errorf("NaN leaked as value: %+v", p)
	}
}

func TestMetricRequestErrors(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	h := metricHandler(t, k, &fakeMetrics{})
	cases := map[string]struct {
		body   string
		status int
	}{
		"unknown aggregation": {body("median", ""), 400},
		"missing range":       {`{"metric":"x","step_seconds":60,"aggregation":"rate"}`, 400},
		"unknown field":       {body("rate", `,"promql":"rate(x[5m])"`), 400},
		"top-level metric":    {`{"metric":"x","range":{"from":"2026-10-05T12:00:00Z","to":"2026-10-05T12:05:00Z"},"step_seconds":60}`, 400},
		"8 days":              {`{"range":{"from":"2026-10-01T00:00:00Z","to":"2026-10-09T00:00:00Z"},"step_seconds":3600,"expression":{"metric":"x","aggregation":"rate"}}`, 422},
		"or filter":           {body("rate", `,"filter":{"op":"or","args":[]}`), 422},
		"non-string eq":       {body("rate", `,"filter":{"op":"eq","field":"a","value":1}`), 400},
		"deep filter":         {body("rate", `,"filter":{"op":"and","args":[{"op":"and","args":[{"op":"and","args":[{"op":"and","args":[{"op":"eq","field":"a","value":"b"}]}]}]}]}`), 400},
		"not json":            {`rate(x[5m])`, 400},
	}
	for name, c := range cases {
		if rec := postMetrics(h, tok, c.body); rec.Code != c.status {
			t.Errorf("%s: status = %d body=%s", name, rec.Code, rec.Body)
		}
	}
	noRead := k.issue(t, authz.KindAPIKey, []authz.Action{authz.DashboardsRead}, nil)
	if rec := postMetrics(h, noRead, body("rate", "")); rec.Code != http.StatusForbidden {
		t.Errorf("no telemetry.read = %d", rec.Code)
	}
}

// rollup watermark 이후 step: 값이 없으면 no_data가 아니라 pending, 있으면 partial (계약 6, D02 §13 watermark).
func TestMetricPendingAfterWatermark(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	m := &fakeMetrics{watermark: m0.Add(2 * time.Minute), buckets: []telemetrystore.MetricBucket{
		counterBucket("", 0, 60), counterBucket("", 2, 60),
	}}
	r := decode(t, postMetrics(metricHandler(t, k, m), tok, body("increase", "")))
	pts := r.Data.Series[0].Points
	if pts[0].Partial || pts[1].Reason != ReasonNoData || !pts[2].Partial || pts[3].Reason != ReasonPending {
		t.Errorf("points = %+v", pts)
	}
	if r.Meta.Watermark == nil || *r.Meta.Watermark != "2026-10-05T12:02:00Z" {
		t.Errorf("meta.watermark = %v", r.Meta.Watermark)
	}
}

// step 안의 1분 window가 빠지면 합산 값은 하한일 뿐이다 → partial.
func TestMetricMissingWindowsMarkPartial(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	full := telemetrystore.MetricBucket{Group: []string{}, StepStart: m0, Types: []string{"sum"}, Units: []string{"1"},
		Windows: 10, Streams: 2, IncreaseWindows: 10, Increase: 100}
	short := full
	short.StepStart, short.Windows, short.IncreaseWindows = m0.Add(5*time.Minute), 7, 7
	m := &fakeMetrics{buckets: []telemetrystore.MetricBucket{full, short}}
	b := `{"range":{"from":"2026-10-05T12:00:00Z","to":"2026-10-05T12:10:00Z"},"step_seconds":300,"expression":{"metric":"m","aggregation":"increase"}}`
	pts := decode(t, postMetrics(metricHandler(t, k, m), tok, b)).Data.Series[0].Points
	if pts[0].Partial || !pts[1].Partial {
		t.Errorf("points = %+v (2 streams × 5 windows 기대)", pts)
	}
}

// 유형이 섞인 step, 단위가 step마다 다른 series는 값을 내지 않는다.
func TestMetricConflictsPerPoint(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	mixedType := counterBucket("", 0, 10)
	mixedType.Types = []string{"gauge", "sum"}
	r := decode(t, postMetrics(metricHandler(t, k, &fakeMetrics{buckets: []telemetrystore.MetricBucket{mixedType}}), tok, body("increase", "")))
	if p := r.Data.Series[0].Points[0]; p.V != nil || p.Reason != ReasonTypeConflict {
		t.Errorf("type conflict = %+v", p)
	}
	a, b := counterBucket("", 0, 10), counterBucket("", 1, 10)
	b.Units = []string{"ms"}
	r = decode(t, postMetrics(metricHandler(t, k, &fakeMetrics{buckets: []telemetrystore.MetricBucket{a, b}}), tok, body("increase", "")))
	s := r.Data.Series[0]
	if s.Unit != nil || s.Points[0].Reason != ReasonUnitConflict || s.Points[1].Reason != ReasonUnitConflict {
		t.Errorf("unit across steps = %+v", s)
	}
}

// step이 1시간의 배수면 1시간 rollup을 읽고, 기대 window 수도 1시간 단위로 센다 (ADR 0028).
func TestMetricHourlyResolution(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	b := telemetrystore.MetricBucket{Group: []string{}, StepStart: m0, Types: []string{"sum"}, Units: []string{"1"},
		Windows: 2, Streams: 1, IncreaseWindows: 2, Increase: 7200}
	m := &fakeMetrics{watermark: m0.Add(24 * time.Hour), buckets: []telemetrystore.MetricBucket{b}}
	body := `{"range":{"from":"2026-10-05T12:00:00Z","to":"2026-10-05T14:00:00Z"},"step_seconds":7200,"expression":{"metric":"m","aggregation":"rate"}}`
	r := decode(t, postMetrics(metricHandler(t, k, m), tok, body))
	p := r.Data.Series[0].Points[0]
	if m.window != time.Hour || p.V == nil || *p.V != 1 || p.Partial {
		t.Errorf("window=%v point=%+v (2시간 step = 1시간 window 2개, rate 7200/7200s)", m.window, p)
	}
	if r.Data.SourceWindowSeconds != 3600 || !r.Data.Range.From.Equal(m0) || !r.Data.Range.To.Equal(m0.Add(2*time.Hour)) {
		t.Errorf("source window %d range %+v, want 3600 and the aligned range", r.Data.SourceWindowSeconds, r.Data.Range)
	}
}

// 배포 직후처럼 1시간 rollup이 범위 시작을 덮지 못하면 1분 rollup을 읽는다 — 덮지 못한 시간을 no_data로 보이지 않게.
func TestMetricHourlyFallsBackWhenNotCovered(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	m := &fakeMetrics{coverage: m0.Add(time.Hour), watermark: m0.Add(24 * time.Hour)}
	body := `{"range":{"from":"2026-10-05T12:00:00Z","to":"2026-10-05T14:00:00Z"},"step_seconds":3600,"expression":{"metric":"m","aggregation":"rate"}}`
	r := decode(t, postMetrics(metricHandler(t, k, m), tok, body))
	if m.got.Window != time.Minute || m.window != time.Minute || r.Data.SourceWindowSeconds != 60 {
		t.Errorf("window = %v / watermark window %v / reported %d, want 1m fallback", m.got.Window, m.window, r.Data.SourceWindowSeconds)
	}
}
