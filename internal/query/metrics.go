package query

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/httpapi"
	"github.com/polynomeer/montracer/internal/metricagg"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// MetricStore는 metric 조회 저장소다 (telemetrystore.QueryStore).
type MetricStore interface {
	MetricBuckets(ctx context.Context, p authz.Principal, q telemetrystore.MetricQuery, now time.Time) ([]telemetrystore.MetricBucket, error)
	RollupWatermark(ctx context.Context, p authz.Principal, window time.Duration, since, now time.Time) (time.Time, error)
	RollupCoverageStart(ctx context.Context, p authz.Principal, window time.Duration, now time.Time) (time.Time, error)
}

// 집계 연산 (D02 §10 MVP: rate, sum, avg, min, max, histogram_quantile, group_by).
var aggregations = map[string]bool{
	"rate": true, "increase": true, "sum": true, // monotonic sum (counter)
	"avg": true, "min": true, "max": true, // gauge·non-monotonic
	"count": true, "hist_sum": true, "p50": true, "p90": true, "p95": true, "p99": true, // histogram
}

var quantiles = map[string]float64{"p50": 0.5, "p90": 0.9, "p95": 0.95, "p99": 0.99}

// 점 단위 결측 사유. 값이 null이면 반드시 하나가 붙는다 (계약 6).
const (
	ReasonNoData          = "no_data"          // 그 step에 window가 없다
	ReasonMissingBaseline = "missing_baseline" // counter 기준점이 없어 증가량을 모른다 (ADR 0025 §3)
	ReasonNotApplicable   = "not_applicable"   // 그 step의 유형에 이 연산 값이 없다
	ReasonBoundsMismatch  = "bounds_mismatch"  // histogram 경계가 달라 병합할 수 없다
	ReasonUnitConflict    = "unit_conflict"    // 단위가 다른 stream이 한 group에 섞였다
	ReasonTypeConflict    = "type_conflict"    // 유형이 다른 stream이 한 group·step에 섞였다
	ReasonPending         = "pending"          // rollup이 아직 이 step을 계산하지 않았다(watermark 이후)
)

// metricRequest는 POST /api/v1/query/metrics 본문이다 (D02 §13 "range, expression, step_seconds", §19 QuerySpec 확장).
type metricRequest struct {
	Range       requestRange     `json:"range"`
	StepSeconds int              `json:"step_seconds"`
	Expression  metricExpression `json:"expression"`
}

// metricExpression은 query 언어가 아닌 구조화 식이다 (D02 §13 "별도 query 언어를 만들지 않는다").
type metricExpression struct {
	Metric      string          `json:"metric"`
	Aggregation string          `json:"aggregation"`
	Filter      json.RawMessage `json:"filter"`
	GroupBy     []string        `json:"group_by"`
}

type requestRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// filterNode는 D02 §19 filter AST의 부분집합이다: and와 eq leaf만 받는다.
type filterNode struct {
	Op    string          `json:"op"`
	Args  []filterNode    `json:"args"`
	Field string          `json:"field"`
	Value json.RawMessage `json:"value"`
}

func invalid(field, reason string) error { return badField(field, reason) }

func badField(field, reason string) *apierr.Error {
	return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: field, Reason: reason})
}

// unsupported는 문법은 맞지만 지원하지 않는 의미다 → 422 (D02 §19).
func unsupported(field, reason string) error {
	e := badField(field, reason)
	e.Status = http.StatusUnprocessableEntity
	return e
}

func flattenFilter(n filterNode, depth int, out *[]telemetrystore.LabelMatch) error {
	if depth > 4 {
		return invalid("filter", "depth must be at most 4")
	}
	switch n.Op {
	case "and":
		for _, a := range n.Args {
			if err := flattenFilter(a, depth+1, out); err != nil {
				return err
			}
		}
		return nil
	case "eq":
		var v string
		if err := json.Unmarshal(n.Value, &v); err != nil {
			return invalid("filter", "eq value must be a string")
		}
		*out = append(*out, telemetrystore.LabelMatch{Key: n.Field, Value: v})
		return nil
	case "or", "not", "in", "contains", "gte", "lte", "gt", "lt", "neq":
		return unsupported("filter", "metric filter supports only and/eq for now")
	default:
		return invalid("filter", "unknown op")
	}
}

func (r metricRequest) toQuery() (telemetrystore.MetricQuery, error) {
	e := r.Expression
	if !aggregations[e.Aggregation] {
		return telemetrystore.MetricQuery{}, invalid("expression.aggregation", "one of rate, increase, sum, avg, min, max, count, hist_sum, p50, p90, p95, p99")
	}
	if r.Range.From.IsZero() || r.Range.To.IsZero() {
		return telemetrystore.MetricQuery{}, invalid("range", "from and to are required (RFC3339 UTC)")
	}
	// 7일 한도는 사용자가 준 범위로 본다(step 정렬로 늘어난 한 step은 허용).
	if r.Range.To.Sub(r.Range.From) > telemetrystore.MaxMetricRange {
		e := apierr.New(apierr.QueryBudgetExceeded, "조회 범위를 줄이세요")
		e.Details = map[string]any{"max_range_seconds": int64(telemetrystore.MaxMetricRange / time.Second)}
		return telemetrystore.MetricQuery{}, e
	}
	step := time.Duration(r.StepSeconds) * time.Second
	q := telemetrystore.MetricQuery{Metric: e.Metric, StepSeconds: r.StepSeconds, GroupBy: e.GroupBy}
	switch from, to := r.Range.From.UTC(), r.Range.To.UTC(); {
	case step > 0 && to.Sub(from) == step && from.Equal(from.Truncate(time.Minute)):
		// 범위 전체를 한 점으로 (요약 값, ADR 0042): step이 범위 길이와 같고 시작이 분 경계면 epoch 정렬 없이 그대로 쓴다.
		// 정렬하면 범위가 두 step으로 갈라져 분위수를 하나로 합칠 수 없다. 시작이 시간 경계가 아니면 1분 rollup을 읽는다.
		q.Range = telemetrystore.TimeRange{From: from, To: to}
		if !from.Equal(from.Truncate(time.Hour)) {
			q.Window = time.Minute
		}
	case step > 0:
		// step 경계로 맞춘다: [floor(from), ceil(to))
		alignedFrom, alignedTo := from.Truncate(step), to.Truncate(step)
		if alignedTo.Before(to) {
			alignedTo = alignedTo.Add(step)
		}
		q.Range = telemetrystore.TimeRange{From: alignedFrom, To: alignedTo}
	}
	if len(e.Filter) > 0 && string(e.Filter) != "null" {
		var root filterNode
		if err := json.Unmarshal(e.Filter, &root); err != nil {
			return telemetrystore.MetricQuery{}, invalid("expression.filter", "must be a filter AST")
		}
		if err := flattenFilter(root, 1, &q.Filters); err != nil {
			return telemetrystore.MetricQuery{}, err
		}
	}
	return q, nil
}

// MetricPoint는 series의 한 step이다. 값이 없으면 V가 null이고 Reason이 있다.
type MetricPoint struct {
	T       time.Time `json:"t"`
	V       *float64  `json:"v"`
	Reason  string    `json:"reason,omitempty"`
	Partial bool      `json:"partial"`
}

// MissingInterval은 값이 없는 연속 step 구간 [from, to)다.
type MissingInterval struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// MetricSeries는 group 하나의 결과다 (D02 §13 "completeness·missing 구간으로 누락을 표현").
type MetricSeries struct {
	Labels       map[string]string `json:"labels"`
	Unit         *string           `json:"unit"`
	Points       []MetricPoint     `json:"points"`
	Completeness float64           `json:"completeness"` // 값이 있는 step / 전체 step
	Missing      []MissingInterval `json:"missing"`
	Flags        []string          `json:"flags"`
}

type metricResponse struct {
	Data struct {
		Series []MetricSeries `json:"series"`
		// SourceWindowSeconds는 실제로 읽은 rollup 해상도(60 = metric_1m, 3600 = metric_1h)다.
		// 1시간 step이라도 1시간 rollup이 범위를 덮지 못하면 1분을 읽는다(ADR 0028 §2). 화면 legend가 쓴다(ADR 0047).
		SourceWindowSeconds int `json:"source_window_seconds"`
		// Range는 step 경계로 맞춘 실제 조회 범위 [from, to)다.
		Range requestRange `json:"range"`
	} `json:"data"`
	Meta Meta `json:"meta"`
}

// value는 bucket 하나의 연산 값이다.
func value(agg string, b telemetrystore.MetricBucket, step time.Duration) (float64, string) {
	if len(b.Units) > 1 {
		return 0, ReasonUnitConflict
	}
	if len(b.Types) > 1 {
		return 0, ReasonTypeConflict
	}
	switch agg {
	case "rate", "increase", "sum":
		if b.IncreaseWindows == 0 {
			if slices.Contains(b.Flags, metricagg.FlagMissingBaseline) {
				return 0, ReasonMissingBaseline
			}
			return 0, ReasonNotApplicable
		}
		if agg == "rate" {
			return b.Increase / step.Seconds(), ""
		}
		return b.Increase, ""
	case "avg", "min", "max":
		if b.ValueWindows == 0 || b.Samples4Avg == 0 {
			return 0, ReasonNotApplicable
		}
		switch agg {
		case "avg":
			return b.Total / float64(b.Samples4Avg), ""
		case "min":
			return b.Min, ""
		default:
			return b.Max, ""
		}
	case "count":
		if b.HistogramWindows == 0 {
			return 0, ReasonNotApplicable
		}
		return float64(b.Count), ""
	case "hist_sum":
		// 관측값의 합(예: 요청 소요시간 합계). 서비스 상세의 endpoint "total time" 열(D05 §05)이다.
		if b.HistogramWindows == 0 {
			return 0, ReasonNotApplicable
		}
		return b.HistSum, ""
	default: // quantile
		if b.HistogramWindows == 0 {
			return 0, ReasonNotApplicable
		}
		if b.BoundsVariants != 1 {
			return 0, ReasonBoundsMismatch
		}
		v, ok := metricagg.Quantile(quantiles[agg], b.Bounds, b.Buckets)
		if !ok {
			return 0, ReasonNoData // 관측 0건 — 0ms가 아니다
		}
		return v, ""
	}
}

// buildSeries는 bucket을 group별 series로 만든다. 모든 step을 채우고 없는 step은 null + no_data다.
// watermark 이후 step은 rollup이 아직 계산하지 않았다: 값이 없으면 pending, 있으면 partial이다.
func buildSeries(agg string, q telemetrystore.MetricQuery, buckets []telemetrystore.MetricBucket, watermark time.Time) ([]MetricSeries, []string) {
	step := time.Duration(q.StepSeconds) * time.Second
	type group struct {
		labels []string
		byStep map[int64]telemetrystore.MetricBucket
		units  []string
		flags  []string
	}
	groups := map[string]*group{}
	var order []string
	types := map[string]bool{}
	for _, b := range buckets {
		k := strings.Join(b.Group, "\x00")
		g, ok := groups[k]
		if !ok {
			g = &group{labels: b.Group, byStep: map[int64]telemetrystore.MetricBucket{}}
			groups[k] = g
			order = append(order, k)
		}
		g.byStep[b.StepStart.Unix()] = b
		for _, u := range b.Units {
			if !slices.Contains(g.units, u) {
				g.units = append(g.units, u)
			}
		}
		for _, f := range b.Flags {
			if !slices.Contains(g.flags, f) {
				g.flags = append(g.flags, f)
			}
		}
		for _, t := range b.Types {
			types[t] = true
		}
	}
	var warnings []string
	if len(types) > 1 {
		warnings = append(warnings, "type_conflict") // 같은 metric 이름에 다른 유형의 stream이 있다
	}
	out := make([]MetricSeries, 0, len(order))
	for _, k := range order {
		g := groups[k]
		s := MetricSeries{Labels: map[string]string{}, Points: []MetricPoint{}, Missing: []MissingInterval{}, Flags: g.flags}
		if s.Flags == nil {
			s.Flags = []string{}
		}
		slices.Sort(s.Flags)
		for i, key := range q.GroupBy {
			s.Labels[key] = g.labels[i]
		}
		if len(g.units) == 1 {
			u := g.units[0]
			s.Unit = &u
		}
		unitConflict := len(g.units) > 1 // step마다 단위가 달라도 한 series로 이을 수 없다
		perWindow := step / q.Window     // step당 기대 window 수
		present, total := 0, 0
		inGap := false
		for t := q.Range.From; t.Before(q.Range.To); t = t.Add(step) {
			total++
			p := MetricPoint{T: t}
			b, ok := g.byStep[t.Unix()]
			pending := watermark.IsZero() || t.Add(step).After(watermark)
			switch {
			case !ok && pending:
				p.Reason = ReasonPending
			case !ok:
				p.Reason = ReasonNoData
			case unitConflict:
				p.Reason = ReasonUnitConflict
			}
			if p.Reason == "" {
				v, reason := value(agg, b, step)
				switch {
				case reason != "":
					p.Reason, p.Partial = reason, b.Partial || pending
				case math.IsNaN(v) || math.IsInf(v, 0):
					p.Reason = ReasonNotApplicable // 보내지 않은 sum 등 — JSON에 NaN을 쓰지 않는다
				default:
					p.V, p.Partial = &v, b.Partial || pending || missingWindows(agg, b, perWindow)
				}
			}
			switch {
			case p.V != nil:
				present++
				inGap = false
			case inGap:
				s.Missing[len(s.Missing)-1].To = t.Add(step) // 연속 결측은 한 구간으로
			default:
				s.Missing = append(s.Missing, MissingInterval{From: t, To: t.Add(step)})
				inGap = true
			}
			s.Points = append(s.Points, p)
		}
		if total > 0 {
			s.Completeness = float64(present) / float64(total)
		}
		out = append(out, s)
	}
	return out, warnings
}

// missingWindows는 합산형 값(증가량·histogram)에서 step 안의 1분 window가 빠졌는지 본다.
// 빠진 window는 0으로 더해진 셈이라 값이 실제보다 작을 수 있다 → partial (계약 6). gauge 값 통계는 해당 없다.
func missingWindows(agg string, b telemetrystore.MetricBucket, perWindow time.Duration) bool {
	expected := b.Streams * int(perWindow)
	switch agg {
	case "rate", "increase", "sum":
		return b.IncreaseWindows < expected
	case "count", "hist_sum", "p50", "p90", "p95", "p99":
		return b.HistogramWindows < expected
	default:
		return false
	}
}

// queryMetrics는 POST /api/v1/query/metrics 다 (D02 §13, ADR 0027).
func (h *Handler) queryMetrics(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Metrics == nil {
		return apierr.New(apierr.NotFound, "metric 조회를 사용할 수 없습니다")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		e := apierr.New(apierr.InvalidArgument, "요청 본문이 너무 큽니다")
		e.Status = http.StatusRequestEntityTooLarge
		return e
	}
	var req metricRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return invalid("body", "must be a JSON metric query")
	}
	q, err := req.toQuery()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.QueryTimeout)
	defer cancel()
	// 입력 검증을 마친 뒤에만 tenant 실행 slot을 잡는다 (D02 §15)
	release, err := h.gate.acquire(ctx, p.Tenant().String())
	if err != nil {
		return err
	}
	defer release()
	now := h.cfg.Now()
	// 해상도 선택 (ADR 0028 §2): 1시간 step이라도 1시간 rollup이 범위 시작을 덮지 못하면(배포 직후 등) 1분 rollup을 읽는다.
	// 덮지 못한 구간을 no_data로 보이지 않게 한다(계약 6).
	if q.Window == 0 {
		q.Window = telemetrystore.SourceWindow(q.StepSeconds)
	}
	if q.Window == time.Hour {
		cov, err := h.cfg.Metrics.RollupCoverageStart(ctx, p, time.Hour, now)
		if err != nil {
			return err
		}
		if cov.IsZero() || cov.After(q.Range.From) {
			q.Window = time.Minute
		}
	}
	buckets, err := h.cfg.Metrics.MetricBuckets(ctx, p, q, now)
	if err != nil {
		return err
	}
	watermark, err := h.cfg.Metrics.RollupWatermark(ctx, p, q.Window, q.Range.From.Add(-time.Hour), now)
	if err != nil {
		return err
	}
	series, warnings := buildSeries(req.Expression.Aggregation, q, buckets, watermark)
	if warnings == nil {
		warnings = []string{}
	}
	resolution := q.StepSeconds
	var resp metricResponse
	resp.Data.Series = series
	resp.Data.SourceWindowSeconds = int(q.Window / time.Second)
	resp.Data.Range = requestRange{From: q.Range.From.UTC(), To: q.Range.To.UTC()}
	resp.Meta = Meta{
		RequestID:         httpapi.RequestIDFrom(r.Context()),
		SchemaVersion:     SchemaVersion,
		FailedShards:      []string{},
		Warnings:          warnings,
		ResolutionSeconds: &resolution,
		Watermark:         watermarkString(watermark),
		// metric은 sampled=null이다 (D02 §13). coverage는 series별 completeness로 표현한다.
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("query: encode metric response: %w", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out)
	return nil
}

// watermarkString은 rollup watermark를 RFC3339로 돌려준다. 모르면 null이다.
func watermarkString(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	v := t.UTC().Format(time.RFC3339)
	return &v
}
