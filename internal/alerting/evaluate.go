// Package alerting은 monitor 평가 의미다: 정규형 MonitorSpec → metric 조회 → group별 값 → 상태 전이 (D02 §17·§21, ADR 0050).
//
// 저장·스케줄과 무관한 순수 계산이다. alert-worker(주기 평가)와 dry-run이 같은 함수를 쓴다.
// 값이 없는 것(데이터 없음·최소 요청 미달·병합 불가)은 위반도 정상도 아닌 NO_DATA이고, 조회 실패는 EVALUATION_ERROR다(계약 6).
package alerting

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/polynomeer/montracer/internal/metricvalue"
	"github.com/polynomeer/montracer/internal/monitor"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// StatusKey는 HTTP 응답 status label이다(서비스 RED와 같다, ADR 0042).
const StatusKey = monitor.StatusKey

// MaxGroups는 monitor 하나의 group 상한이다 (D02 §20 "group 최대 1,000").
const MaxGroups = 1000

// 값이 없는 이유 (metricvalue 사유에 더해).
const (
	// ReasonBelowMinimum: error_ratio의 window 요청 수가 minimum_requests 미만(D02 §21 최소 데이터 조건 미충족 = NO_DATA).
	ReasonBelowMinimum = "below_minimum_requests"
	// ReasonNoRequests: 요청 0건이라 비율이 없다.
	ReasonNoRequests = "no_requests"
	// ReasonGroupMissing: 전에 있던 group이 이번 window에 없다.
	ReasonGroupMissing = "group_missing"
	// ReasonNoWatermark: rollup이 아직 이 tenant의 window를 하나도 확정하지 않았다.
	ReasonNoWatermark = "no_watermark"
	// ReasonStaleWatermark: rollup watermark가 StaleAfter보다 오래 멈췄다(수집·rollup 장애). 마지막 window를 계속 평가하면
	// 오래된 판정(OK일 수 있다)이 그대로 남으므로 결측으로 본다(D02 §21 freshness, 계약 6).
	ReasonStaleWatermark = "stale_watermark"
)

// StaleAfter는 watermark가 이만큼 지금보다 뒤처지면 멈춘 것으로 보는 한도다.
// 정상 지연은 watermark 정의(최대 관측 − 2분)와 rollup 주기로 수 분 안이다. 늦은 데이터 재계산 한도(10분, D02 §07)와 같게 둔다.
const StaleAfter = 10 * time.Minute

// WindowStatus는 평가 window를 고를 수 있었는지다.
type WindowStatus int

const (
	WindowOK    WindowStatus = iota
	WindowNone               // watermark 없음
	WindowStale              // watermark가 StaleAfter보다 오래 멈춤
)

// Window는 평가할 window [Start, End)다. End는 rollup watermark 이전의 분 경계다(확정 window만, D02 §17).
type Window struct {
	Start, End time.Time
}

// EvaluationWindow는 now·watermark에서 평가 window를 고른다. window 끝은 min(now, watermark)를 분 단위로 내린 값이다.
// watermark가 없으면 WindowNone, StaleAfter보다 오래 멈췄으면 WindowStale이다(둘 다 결측으로 평가한다).
func EvaluationWindow(s monitor.Spec, now, watermark time.Time) (Window, WindowStatus) {
	if watermark.IsZero() {
		return Window{}, WindowNone
	}
	if now.Sub(watermark) > StaleAfter {
		return Window{}, WindowStale
	}
	end := now
	if watermark.Before(end) {
		end = watermark
	}
	end = end.UTC().Truncate(time.Minute)
	return Window{Start: end.Add(-time.Duration(s.WindowSeconds) * time.Second), End: end}, WindowOK
}

// Query는 window의 metric 조회 조건이다: 범위 전체 한 점(step = window), 1분 rollup.
// error_ratio는 group_by 뒤에 status를 더해 count를 받는다.
func Query(s monitor.Spec, w Window) telemetrystore.MetricQuery {
	q := telemetrystore.MetricQuery{
		Metric:      s.Query.Metric,
		Range:       telemetrystore.TimeRange{From: w.Start, To: w.End},
		StepSeconds: s.WindowSeconds,
		GroupBy:     append([]string{}, s.Query.GroupBy...),
		Window:      time.Minute,
	}
	if s.Query.Kind == monitor.KindErrorRatio {
		q.GroupBy = append(q.GroupBy, StatusKey)
	}
	flatten(s.Query.Filter, &q.Filters)
	return q
}

func flatten(n *monitor.FilterNode, out *[]telemetrystore.LabelMatch) {
	if n == nil {
		return
	}
	switch n.Op {
	case "and":
		for i := range n.Args {
			flatten(&n.Args[i], out)
		}
	case "eq":
		if n.Value != nil {
			*out = append(*out, telemetrystore.LabelMatch{Key: n.Field, Value: *n.Value})
		}
	}
}

// GroupResult는 group 하나의 이번 window 값이다. Value가 nil이면 Reason이 있다.
type GroupResult struct {
	Key     string            // group 식별(정렬된 key=value를 이은 문자열). group_by가 없으면 ""
	Labels  map[string]string // group_by key → 값
	Value   *float64
	Reason  string
	Partial bool     // 일부 window만으로 계산(값이 바뀔 수 있다), 또는 status를 모르는 요청이 섞였다
	Total   *float64 // error_ratio: window 전체 요청 수
	// UnknownStatus는 error_ratio에서 status label이 없는 요청 수다(응답 없이 끊긴 요청·속성 누락 SDK).
	// 분모에는 넣고 5xx로 세지 않으므로 오류율이 낮게 나올 수 있다 → Partial로 표시한다.
	UnknownStatus *float64
}

// Result는 한 번의 평가 결과다. GroupLimitExceeded면 group이 MaxGroups를 넘어 평가하지 않는다(일부만 평가하지 않는다).
type Result struct {
	Groups             []GroupResult
	GroupLimitExceeded bool
}

// GroupKey는 label을 key 순서로 이은 식별자다. 같은 label이면 늘 같다.
func GroupKey(groupBy []string, labels map[string]string) string {
	if len(groupBy) == 0 {
		return ""
	}
	keys := append([]string{}, groupBy...)
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%q=%q", k, labels[k])
	}
	return strings.Join(parts, ",")
}

// Compute는 bucket(Query로 받은 범위 전체 한 점)을 group별 값으로 바꾼다.
func Compute(s monitor.Spec, buckets []telemetrystore.MetricBucket) Result {
	step := time.Duration(s.WindowSeconds) * time.Second
	perWindow := step / time.Minute
	type acc struct {
		labels       map[string]string
		total, errs  float64
		unknown      float64
		any, partial bool
		reasons      []string
		dropped      []string // no_data가 아닌 사유로 버린 bucket(충돌·병합 불가)
	}
	groups := map[string]*acc{}
	var order []string
	get := func(labels map[string]string) *acc {
		k := GroupKey(s.Query.GroupBy, labels)
		a, ok := groups[k]
		if !ok {
			a = &acc{labels: labels}
			groups[k] = a
			order = append(order, k)
		}
		return a
	}
	var out Result
	for _, b := range buckets {
		labels := map[string]string{}
		for i, k := range s.Query.GroupBy {
			if i < len(b.Group) {
				labels[k] = b.Group[i]
			}
		}
		if s.Query.Kind == monitor.KindErrorRatio {
			status := ""
			if n := len(s.Query.GroupBy); n < len(b.Group) {
				status = b.Group[n]
			}
			a := get(labels)
			v, reason := metricvalue.Value("count", b, step)
			if reason != "" {
				a.reasons = append(a.reasons, reason)
				if reason != metricvalue.ReasonNoData {
					a.dropped = append(a.dropped, reason)
				}
				continue
			}
			a.any = true
			a.total += v
			switch {
			case status == "":
				a.unknown += v
			case isServerError(status):
				a.errs += v
			}
			a.partial = a.partial || b.Partial || metricvalue.MissingWindows("count", b, perWindow)
			continue
		}
		a := get(labels)
		v, reason := metricvalue.Value(*s.Query.Aggregation, b, step)
		if reason != "" || math.IsNaN(v) || math.IsInf(v, 0) {
			if reason == "" {
				reason = metricvalue.ReasonNotApplicable
			}
			a.reasons = append(a.reasons, reason)
			continue
		}
		a.any, a.total = true, v
		a.partial = b.Partial || metricvalue.MissingWindows(*s.Query.Aggregation, b, perWindow)
	}
	if len(order) > MaxGroups {
		return Result{GroupLimitExceeded: true}
	}
	sort.Strings(order)
	for _, k := range order {
		a := groups[k]
		r := GroupResult{Key: k, Labels: a.labels, Partial: a.partial}
		switch {
		case !a.any:
			r.Reason = firstReason(a.reasons)
		case len(a.dropped) > 0:
			// 일부 status bucket을 충돌·병합 불가로 버렸다: 분자·분모가 모두 하한이라 비율의 방향도 모른다.
			// 남은 bucket으로 값을 만들지 않고 결측으로 둔다(정상으로 판정해 복구 횟수에 넣지 않는다, 계약 6).
			r.Reason = firstReason(a.dropped)
		case s.Query.Kind == monitor.KindErrorRatio:
			total := a.total
			r.Total = &total
			if a.unknown > 0 {
				u := a.unknown
				r.UnknownStatus, r.Partial = &u, true
			}
			switch {
			case total == 0:
				r.Reason = ReasonNoRequests
			case s.MinimumRequests != nil && total < float64(*s.MinimumRequests):
				r.Reason = ReasonBelowMinimum
			default:
				v := a.errs / total
				r.Value = &v
			}
		default:
			v := a.total
			r.Value = &v
		}
		out.Groups = append(out.Groups, r)
	}
	// group_by가 없는데 series가 하나도 없으면 하나의 group("")이 데이터 없음이다
	if len(s.Query.GroupBy) == 0 && len(out.Groups) == 0 {
		out.Groups = []GroupResult{{Key: "", Labels: map[string]string{}, Reason: metricvalue.ReasonNoData}}
	}
	return out
}

// 이유 우선순위: 데이터 이상이 단순 부재보다 정확하다.
var reasonOrder = []string{
	metricvalue.ReasonBoundsMismatch, metricvalue.ReasonUnitConflict, metricvalue.ReasonTypeConflict,
	metricvalue.ReasonMissingBaseline, metricvalue.ReasonNotApplicable, metricvalue.ReasonNoData,
}

func firstReason(rs []string) string {
	for _, r := range reasonOrder {
		for _, x := range rs {
			if x == r {
				return r
			}
		}
	}
	return metricvalue.ReasonNoData
}

func isServerError(code string) bool {
	if len(code) != 3 {
		return false
	}
	return code[0] == '5' && code[1] >= '0' && code[1] <= '9' && code[2] >= '0' && code[2] <= '9'
}

// Violates는 값이 조건을 어기는지다. gt는 엄격히 크다(정확히 threshold는 위반이 아니다, D06 §05 2% 경계 시험).
func Violates(c monitor.Condition, v float64) bool {
	switch c.Operator {
	case "gt":
		return v > c.Threshold
	case "gte":
		return v >= c.Threshold
	case "lt":
		return v < c.Threshold
	case "lte":
		return v <= c.Threshold
	default:
		return false
	}
}
