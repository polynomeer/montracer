// Package monitor는 monitor 정의(MonitorSpec)의 검증·정규화다 (D02 §14·§17·§20, ADR 0049).
//
// control-api(저장 전 validate)와 alert-worker(평가)가 같은 정규형을 쓴다. 정규형은 기본값을 모두 채운
// 명시적 JSON이다 — 저장된 revision만 보고도 평가 의미가 정해진다(D02 §11 "immutable evaluation version").
package monitor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 한도 (D02 §14·§17, ADR 0027·0049).
const (
	MaxNameLength       = 200
	MaxMetricLength     = 256
	MaxLabelLength      = 256
	MaxGroupBy          = 5
	MaxFilterLeaves     = 20
	MaxFilterDepth      = 4
	MinWindowSeconds    = 60
	MaxWindowSeconds    = 86400
	MaxForSeconds       = 86400
	MaxNoDataSeconds    = 86400
	MaxMinimumRequests  = 1_000_000_000
	DefaultMinRequests  = 100 // D02 §17 오류율 경보 시작값
	RecoveryEvaluations = 2   // D02 §17 "복구 조건이 2회 연속 충족되면 OK"
	// DefaultErrorMetric은 오류율 monitor의 원천이다. 서비스 RED와 같다(ADR 0042, 계약 5).
	DefaultErrorMetric = "http.server.request.duration"
	// MaxSpecBytes는 요청 본문 상한이다.
	MaxSpecBytes = 64 << 10
)

// EvaluationSeconds는 허용 평가 주기다 (D02 §14).
var EvaluationSeconds = []int{30, 60, 300}

// 연산 (ADR 0027 §1). 유형별 허용은 query-api 사전(ADR 0046)이 알려주고, 평가 시 맞지 않으면 EVALUATION_ERROR가 아니라
// not_applicable 결측으로 드러난다(ADR 0049).
var aggregations = map[string]bool{
	"rate": true, "increase": true, "sum": true, "avg": true, "min": true, "max": true,
	"count": true, "hist_sum": true, "p50": true, "p90": true, "p95": true, "p99": true,
}

var operators = map[string]bool{"gt": true, "gte": true, "lt": true, "lte": true}

// Kind는 query 종류다.
const (
	// KindMetric은 metric 하나의 집계 값과 threshold를 비교한다.
	KindMetric = "metric"
	// KindErrorRatio는 HTTP 서버 histogram의 5xx 수 / 전체 수다(서비스 RED 오류율과 같은 정의, ADR 0042).
	KindErrorRatio = "error_ratio"
)

// NoData 동작 (D02 §17 "missing 정책은 monitor마다 명시"). 결측을 OK로 바꾸는 선택지는 없다(계약 6).
const (
	// NoDataState는 NO_DATA 상태로 둔다(알림하지 않는다).
	NoDataState = "no_data"
	// NoDataAlert는 결측이 AfterSeconds 이상 이어지면 ALERT로 둔다.
	NoDataAlert = "alert"
)

// FilterNode는 label 조건이다. and와 eq leaf만 받는다(ADR 0027 §1 metric filter).
type FilterNode struct {
	Op    string       `json:"op"`
	Field string       `json:"field,omitempty"`
	Value *string      `json:"value,omitempty"`
	Args  []FilterNode `json:"args,omitempty"`
}

// Query는 평가할 값이다.
type Query struct {
	Kind        string      `json:"kind"`
	Metric      string      `json:"metric"`
	Aggregation *string     `json:"aggregation"` // metric만. error_ratio는 null
	Filter      *FilterNode `json:"filter"`
	GroupBy     []string    `json:"group_by"`
}

// Condition은 위반 조건이다: 값 operator threshold.
type Condition struct {
	Operator  string  `json:"operator"`
	Threshold float64 `json:"threshold"`
}

// NoData는 결측 정책이다.
type NoData struct {
	Action       string `json:"action"`
	AfterSeconds *int   `json:"after_seconds"` // alert만
}

// Spec은 정규형 MonitorSpec이다. 모든 필드가 채워진다.
type Spec struct {
	Name                 string    `json:"name"`
	Enabled              bool      `json:"enabled"`
	Query                Query     `json:"query"`
	WindowSeconds        int       `json:"window_seconds"`
	EvaluationSeconds    int       `json:"evaluation_seconds"`
	Condition            Condition `json:"condition"`
	MinimumRequests      *int      `json:"minimum_requests"` // error_ratio만
	ForSeconds           int       `json:"for_seconds"`
	RecoveryEvaluations  int       `json:"recovery_evaluations"`
	NoData               NoData    `json:"no_data"`
	NotificationPolicyID *string   `json:"notification_policy_id"`
}

// Violation은 필드 하나의 오류다(apierr.FieldViolation과 같은 모양).
type Violation struct {
	Field  string
	Reason string
}

// ValidationError는 검증 실패다. Unsupported면 문법은 맞지만 아직 지원하지 않는 의미다(422).
type ValidationError struct {
	Violations  []Violation
	Unsupported bool
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Violations))
	for i, v := range e.Violations {
		parts[i] = v.Field + ": " + v.Reason
	}
	return "monitor: invalid spec: " + strings.Join(parts, "; ")
}

// 경고 (저장은 된다). 화면이 설명한다.
const (
	WarnDryRunUnavailable   = "dry_run_unavailable"   // 24시간 dry-run은 평가기와 함께(ADR 0049)
	WarnSingleEvaluation    = "for_seconds_zero"      // 한 번 위반으로 ALERT
	WarnNoMinimumRequests   = "minimum_requests_zero" // 저트래픽 노이즈(D02 §17)
	WarnSubMinuteEvaluation = "evaluation_below_rollup_resolution"
)

// rawSpec은 입력 모양이다. 생략 가능한 필드는 포인터다.
type rawSpec struct {
	Name                 *string         `json:"name"`
	Enabled              *bool           `json:"enabled"`
	Query                *rawQuery       `json:"query"`
	WindowSeconds        *int            `json:"window_seconds"`
	EvaluationSeconds    *int            `json:"evaluation_seconds"`
	Condition            *rawCondition   `json:"condition"`
	MinimumRequests      *int            `json:"minimum_requests"`
	ForSeconds           *int            `json:"for_seconds"`
	RecoveryEvaluations  *int            `json:"recovery_evaluations"`
	NoData               json.RawMessage `json:"no_data"`
	NotificationPolicyID *string         `json:"notification_policy_id"`
}

type rawQuery struct {
	Kind        *string         `json:"kind"`
	Metric      *string         `json:"metric"`
	Aggregation *string         `json:"aggregation"`
	Filter      json.RawMessage `json:"filter"`
	GroupBy     []string        `json:"group_by"`
}

type rawCondition struct {
	Operator  *string  `json:"operator"`
	Threshold *float64 `json:"threshold"`
}

// D02 §14 예시의 문자열 형식: "alert_after_600s"
var alertAfter = regexp.MustCompile(`^alert_after_([0-9]{1,6})s$`)

// Normalize는 MonitorSpec JSON을 검증하고 정규형과 경고를 돌려준다. 모르는 필드는 오류다.
func Normalize(body []byte) (Spec, []string, error) {
	if len(body) > MaxSpecBytes {
		return Spec{}, nil, &ValidationError{Violations: []Violation{{"body", fmt.Sprintf("at most %d bytes", MaxSpecBytes)}}}
	}
	var raw rawSpec
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return Spec{}, nil, &ValidationError{Violations: []Violation{{"body", "must be a MonitorSpec JSON object without unknown fields"}}}
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Spec{}, nil, &ValidationError{Violations: []Violation{{"body", "must contain a single JSON object"}}}
	}
	v := &validator{}
	s := Spec{Enabled: true, RecoveryEvaluations: RecoveryEvaluations}
	var warnings []string

	// name
	if raw.Name == nil {
		v.add("name", "required")
	} else {
		s.Name = strings.TrimSpace(*raw.Name)
		if n := utf8.RuneCountInString(s.Name); n < 1 || n > MaxNameLength || !utf8.ValidString(s.Name) {
			v.add("name", fmt.Sprintf("1..%d characters", MaxNameLength))
		}
	}
	if raw.Enabled != nil {
		s.Enabled = *raw.Enabled
	}

	// evaluation·window·for
	if raw.EvaluationSeconds == nil {
		v.add("evaluation_seconds", "required (30, 60 or 300)")
	} else if !contains(EvaluationSeconds, *raw.EvaluationSeconds) {
		v.add("evaluation_seconds", "must be 30, 60 or 300")
	} else {
		s.EvaluationSeconds = *raw.EvaluationSeconds
	}
	if raw.WindowSeconds == nil {
		v.add("window_seconds", "required")
	} else {
		s.WindowSeconds = *raw.WindowSeconds
		switch {
		case s.WindowSeconds < MinWindowSeconds || s.WindowSeconds > MaxWindowSeconds:
			v.add("window_seconds", "must be within [60, 86400]")
		case s.WindowSeconds%60 != 0:
			v.add("window_seconds", "must be a multiple of 60 (metric rollup is per minute)")
		case s.EvaluationSeconds > 0 && s.WindowSeconds%s.EvaluationSeconds != 0:
			v.add("window_seconds", "must be a multiple of evaluation_seconds")
		}
	}
	// for_seconds는 필수다: 생략을 0(한 번 위반으로 ALERT)으로 보면 노이즈 알림이 된다(D02 §17 시작값은 2분 유지).
	// 기본값을 지어내지 않고 명시하게 한다(no_data와 같은 원칙).
	if raw.ForSeconds == nil {
		v.add("for_seconds", "required (e.g. 120 to require 2 minutes of sustained violation; 0 alerts on a single evaluation)")
	} else {
		s.ForSeconds = *raw.ForSeconds
	}
	switch {
	case raw.ForSeconds == nil:
	case s.ForSeconds < 0 || s.ForSeconds > MaxForSeconds:
		v.add("for_seconds", "must be within [0, 86400]")
	case s.EvaluationSeconds > 0 && s.ForSeconds%s.EvaluationSeconds != 0:
		v.add("for_seconds", "must be a multiple of evaluation_seconds")
	case s.ForSeconds == 0:
		warnings = append(warnings, WarnSingleEvaluation)
	}
	// 복구 횟수는 D02 §17로 고정이다. 정규형을 그대로 다시 보낼 수 있게(조회 → 수정) 필드는 받되 2만 허용한다.
	if raw.RecoveryEvaluations != nil && *raw.RecoveryEvaluations != RecoveryEvaluations {
		v.addUnsupported("recovery_evaluations", "fixed at 2 consecutive evaluations (D02 §17)")
	}
	if s.EvaluationSeconds > 0 && s.EvaluationSeconds < 60 {
		warnings = append(warnings, WarnSubMinuteEvaluation)
	}

	// query
	if raw.Query == nil {
		v.add("query", "required")
	} else {
		s.Query = v.query(raw.Query)
	}

	// condition
	if raw.Condition == nil || raw.Condition.Operator == nil || raw.Condition.Threshold == nil {
		v.add("condition", "operator and threshold are required")
	} else {
		s.Condition = Condition{Operator: *raw.Condition.Operator, Threshold: *raw.Condition.Threshold}
		if !operators[s.Condition.Operator] {
			v.add("condition.operator", "must be gt, gte, lt or lte")
		}
		if math.IsNaN(s.Condition.Threshold) || math.IsInf(s.Condition.Threshold, 0) {
			v.add("condition.threshold", "must be a finite number")
		}
		if s.Query.Kind == KindErrorRatio && (s.Condition.Threshold < 0 || s.Condition.Threshold > 1) {
			v.add("condition.threshold", "error_ratio threshold is a fraction within [0, 1] (UI percent / 100)")
		}
	}

	// minimum_requests: 오류율만 (저트래픽 노이즈 감소, D02 §17)
	switch {
	case s.Query.Kind == KindErrorRatio:
		mr := DefaultMinRequests
		if raw.MinimumRequests != nil {
			mr = *raw.MinimumRequests
		}
		if mr < 0 || mr > MaxMinimumRequests {
			v.add("minimum_requests", "must be within [0, 1e9]")
		}
		if mr == 0 {
			warnings = append(warnings, WarnNoMinimumRequests)
		}
		s.MinimumRequests = &mr
	case raw.MinimumRequests != nil:
		v.add("minimum_requests", "only for query.kind error_ratio")
	}

	// no_data (필수: 결측 정책을 monitor마다 명시, D02 §17)
	s.NoData = v.noData(raw.NoData, s.WindowSeconds, s.EvaluationSeconds)

	// notification policy: 정책 테이블·webhook과 함께(ADR 0049 §5). 그 전에는 받지 않는다.
	if raw.NotificationPolicyID != nil {
		v.addUnsupported("notification_policy_id", "notification policies are not available yet; omit or send null")
	}

	if len(v.violations) > 0 {
		// 모든 위반이 '아직 지원하지 않음'일 때만 422, 하나라도 형식 오류면 400
		return Spec{}, nil, &ValidationError{Violations: v.violations, Unsupported: v.unsupported == len(v.violations)}
	}
	warnings = append(warnings, WarnDryRunUnavailable)
	return s, warnings, nil
}

// Canonical은 정규형 JSON이다(필드 순서 고정). 저장·revision 비교에 쓴다.
func (s Spec) Canonical() []byte {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // 정규형은 항상 직렬화된다
	}
	return b
}

type validator struct {
	violations  []Violation
	unsupported int // '아직 지원하지 않음' 위반 수
}

func (v *validator) add(field, reason string) {
	v.violations = append(v.violations, Violation{field, reason})
}

func (v *validator) addUnsupported(field, reason string) {
	v.add(field, reason)
	v.unsupported++
}

func (v *validator) query(q *rawQuery) Query {
	out := Query{GroupBy: []string{}}
	if q.Kind == nil {
		v.add("query.kind", "required (metric or error_ratio)")
		return out
	}
	out.Kind = *q.Kind
	switch out.Kind {
	case KindMetric:
		if q.Metric == nil {
			v.add("query.metric", "required")
		} else {
			out.Metric = *q.Metric
		}
		if q.Aggregation == nil || !aggregations[*q.Aggregation] {
			v.add("query.aggregation", "one of rate, increase, sum, avg, min, max, count, hist_sum, p50, p90, p95, p99")
		} else {
			a := *q.Aggregation
			out.Aggregation = &a
		}
	case KindErrorRatio:
		// 5xx/전체는 HTTP 서버 histogram과 status label을 전제로 한다. 다른 metric은 의미가 깨지므로
		// 지금은 이 원천만 받는다(다른 HTTP 서버 histogram은 요구가 생기면 허용 목록으로, ADR 0049).
		out.Metric = DefaultErrorMetric
		if q.Metric != nil && *q.Metric != DefaultErrorMetric {
			v.addUnsupported("query.metric", "error_ratio supports only "+DefaultErrorMetric+" (HTTP server histogram with status code)")
		}
		if q.Aggregation != nil {
			v.add("query.aggregation", "not allowed for error_ratio (5xx count / all count)")
		}
	default:
		v.add("query.kind", "must be metric or error_ratio")
		return out
	}
	if l := len(out.Metric); l < 1 || l > MaxMetricLength || !utf8.ValidString(out.Metric) {
		v.add("query.metric", "1..256 bytes")
	}
	seen := map[string]bool{}
	for i, k := range q.GroupBy {
		f := "query.group_by[" + strconv.Itoa(i) + "]"
		switch {
		case k == "" || len(k) > MaxLabelLength:
			v.add(f, "1..256 bytes")
		case seen[k]:
			v.add(f, "duplicate key")
		}
		seen[k] = true
	}
	if len(q.GroupBy) > MaxGroupBy {
		v.add("query.group_by", fmt.Sprintf("at most %d keys", MaxGroupBy))
	}
	if q.GroupBy != nil {
		out.GroupBy = append([]string{}, q.GroupBy...)
	}
	if len(q.Filter) > 0 && string(q.Filter) != "null" {
		var f FilterNode
		dec := json.NewDecoder(bytes.NewReader(q.Filter))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			v.add("query.filter", "must be a filter AST (and / eq)")
		} else {
			leaves := 0
			v.filter("query.filter", &f, 1, &leaves)
			if leaves > MaxFilterLeaves {
				v.add("query.filter", fmt.Sprintf("at most %d conditions", MaxFilterLeaves))
			}
			out.Filter = &f
		}
	}
	return out
}

func (v *validator) filter(path string, n *FilterNode, depth int, leaves *int) {
	if depth > MaxFilterDepth {
		v.add(path, "depth must be at most 4")
		return
	}
	switch n.Op {
	case "and":
		if len(n.Args) == 0 || n.Field != "" || n.Value != nil {
			v.add(path, "and needs args only")
		}
		for i := range n.Args {
			v.filter(path+".args["+strconv.Itoa(i)+"]", &n.Args[i], depth+1, leaves)
		}
	case "eq":
		*leaves++
		if n.Field == "" || len(n.Field) > MaxLabelLength || len(n.Args) > 0 {
			v.add(path, "eq needs field (1..256 bytes) and value")
		}
		if n.Value == nil || len(*n.Value) > MaxLabelLength {
			v.add(path+".value", "string of at most 256 bytes")
		}
	case "or", "not", "in", "neq", "contains", "gt", "gte", "lt", "lte", "exists":
		v.addUnsupported(path+".op", "metric filter supports only and/eq for now")
	default:
		v.add(path+".op", "unknown op")
	}
}

func (v *validator) noData(raw json.RawMessage, window, eval int) NoData {
	if len(raw) == 0 || string(raw) == "null" {
		v.add("no_data", `required: {"action":"no_data"} or {"action":"alert","after_seconds":N} (missing data is never treated as OK)`)
		return NoData{}
	}
	var nd struct {
		Action       *string `json:"action"`
		AfterSeconds *int    `json:"after_seconds"`
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		// D02 §14 예시의 문자열 형식도 받는다
		switch m := alertAfter.FindStringSubmatch(str); {
		case str == NoDataState:
			nd.Action = &str
		case m != nil:
			a := NoDataAlert
			n, _ := strconv.Atoi(m[1])
			nd.Action, nd.AfterSeconds = &a, &n
		default:
			v.add("no_data", `must be "no_data", "alert_after_<seconds>s" or an object`)
			return NoData{}
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&nd); err != nil {
			v.add("no_data", "must be an object with action and after_seconds")
			return NoData{}
		}
	}
	if nd.Action == nil {
		v.add("no_data.action", "required (no_data or alert)")
		return NoData{}
	}
	switch *nd.Action {
	case NoDataState:
		if nd.AfterSeconds != nil {
			v.add("no_data.after_seconds", "only for action alert")
		}
		return NoData{Action: NoDataState}
	case NoDataAlert:
		if nd.AfterSeconds == nil {
			v.add("no_data.after_seconds", "required for action alert")
			return NoData{Action: NoDataAlert}
		}
		a := *nd.AfterSeconds
		switch {
		case a < window || a > MaxNoDataSeconds:
			v.add("no_data.after_seconds", "must be within [window_seconds, 86400]")
		case eval > 0 && a%eval != 0:
			v.add("no_data.after_seconds", "must be a multiple of evaluation_seconds")
		}
		return NoData{Action: NoDataAlert, AfterSeconds: &a}
	default:
		v.add("no_data.action", "must be no_data or alert (treating missing data as OK is not allowed)")
		return NoData{}
	}
}

func contains(xs []int, x int) bool {
	for _, y := range xs {
		if x == y {
			return true
		}
	}
	return false
}
