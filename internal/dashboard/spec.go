// Package dashboard는 dashboard 정의(DashboardSpec)의 검증·정규화다 (D02 §14, D05 §10~11, ADR 0057).
//
// control-api가 저장 전에 검증하고 정규형을 저장한다. 정규형은 기본값을 모두 채운 명시적 JSON이다 — 화면은
// 저장된 정의만 보고 그릴 수 있다. 위젯의 metric query는 query-api의 구조화 식(POST /query/metrics expression)과 같다.
// 저장은 query를 실행하지 않는다: 보는 사람의 권한으로 화면이 query-api를 부를 때 다시 인가된다(D05 §04).
package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// 한도 (D02 §14 "최대 50 위젯", D05 §11 "24-column grid", ADR 0057).
const (
	Version           = 1
	MaxTitleLength    = 200
	MaxDescription    = 2000
	MaxWidgets        = 50
	MaxVariables      = 10
	GridColumns       = 24
	MaxWidgetHeight   = 48
	MaxGridRows       = 1000
	MaxTextLength     = 10_000
	MaxUnitLength     = 32
	MaxMetricLength   = 256
	MaxLabelLength    = 256
	MaxGroupBy        = 5
	MaxFilterLeaves   = 20
	MaxFilterDepth    = 4
	MaxDisplayLimit   = 100
	DefaultLimit      = 10
	DefaultDecimals   = 2
	MaxDecimals       = 6
	MaxSpecBytes      = 256 << 10
	variablePrefix    = "$"
	maxWidgetIDLength = 64
)

// 위젯 종류 (D05 §11). histogram은 bucket 분포 조회 API가 생길 때까지 422(아직 지원하지 않음)다(ADR 0057).
const (
	KindTimeSeries = "time_series"
	KindStat       = "stat"
	KindTable      = "table"
	KindHistogram  = "histogram"
	KindTopList    = "top_list"
	KindText       = "text"
)

// 변수 종류 (D05 §11 "environment, service, team"). team은 서비스 소유 팀이 생길 때까지 422다(ADR 0057).
const (
	VarEnvironment = "environment"
	VarService     = "service"
	VarTeam        = "team"
)

// VariableField는 변수 종류가 거르는 label이다(위젯 filter의 "$이름" 값은 이 field의 eq leaf에만 쓸 수 있다).
var VariableField = map[string]string{
	VarEnvironment: "deployment.environment.name",
	VarService:     "service.name",
}

// TimeRanges는 위젯 시간 덮어쓰기 값이다(web 조사 context 상대 범위와 같다, 최대 7일 = metric 조회 한도).
var TimeRanges = []string{"15m", "1h", "4h", "1d", "7d"}

var aggregations = map[string]bool{
	"rate": true, "increase": true, "sum": true, "avg": true, "min": true, "max": true,
	"count": true, "hist_sum": true, "p50": true, "p90": true, "p95": true, "p99": true,
}

var (
	widgetIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	varNamePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// FilterNode는 label 조건이다. and와 eq leaf만 받는다(query-api metric filter와 같다). eq 값이 "$이름"이면 변수다.
type FilterNode struct {
	Op    string       `json:"op"`
	Field string       `json:"field,omitempty"`
	Value *string      `json:"value,omitempty"`
	Args  []FilterNode `json:"args,omitempty"`
}

// MetricQuery는 위젯의 metric 식이다(POST /api/v1/query/metrics의 expression).
type MetricQuery struct {
	Metric      string      `json:"metric"`
	Aggregation string      `json:"aggregation"`
	Filter      *FilterNode `json:"filter"`
	GroupBy     []string    `json:"group_by"`
}

// Display는 종류별 표시 설정이다. 그 종류가 쓰는 필드만 채워진다.
type Display struct {
	Stack    *bool   `json:"stack,omitempty"`    // time_series
	Decimals *int    `json:"decimals,omitempty"` // stat·table·top_list
	Limit    *int    `json:"limit,omitempty"`    // table·top_list
	Sort     *string `json:"sort,omitempty"`     // table·top_list: desc | asc
}

// TimeOverride는 위젯만 다른 상대 시간 범위를 쓴다(D05 §11 time_override). null이면 dashboard 범위다.
type TimeOverride struct {
	Range string `json:"range"`
}

// Widget은 위젯 하나다. text 위젯은 Query가 null이고 Text가 있다(글자 그대로 표시, HTML·markdown 해석 없음).
type Widget struct {
	ID           string        `json:"id"`
	Kind         string        `json:"kind"`
	Title        string        `json:"title"`
	Unit         *string       `json:"unit"`
	Display      Display       `json:"display"`
	TimeOverride *TimeOverride `json:"time_override"`
	Query        *MetricQuery  `json:"query"`
	Text         *string       `json:"text"`
}

// Variable은 전역 변수다. Default가 null이면 "전체"다.
type Variable struct {
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`
	Default *string `json:"default"`
}

// Placement는 위젯 하나의 24열 grid 위치다.
type Placement struct {
	WidgetID string `json:"widget_id"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	W        int    `json:"w"`
	H        int    `json:"h"`
}

// Spec은 정규형 DashboardSpec이다(D05 §11: version, title, description, variables, layout, widgets).
type Spec struct {
	Version     int         `json:"version"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	Variables   []Variable  `json:"variables"`
	Layout      []Placement `json:"layout"`
	Widgets     []Widget    `json:"widgets"`
}

// Violation은 필드 하나의 오류다(apierr.FieldViolation과 같은 모양).
type Violation struct {
	Field  string
	Reason string
}

// ValidationError는 검증 실패다. Unsupported면 문법은 맞지만 아직 지원하지 않는 의미뿐이다(422).
type ValidationError struct {
	Violations  []Violation
	Unsupported bool
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Violations))
	for i, v := range e.Violations {
		parts[i] = v.Field + ": " + v.Reason
	}
	return "dashboard: invalid spec: " + strings.Join(parts, "; ")
}

type rawSpec struct {
	Version     *int           `json:"version"`
	Title       *string        `json:"title"`
	Description *string        `json:"description"`
	Variables   []rawVariable  `json:"variables"`
	Layout      []rawPlacement `json:"layout"`
	Widgets     []rawWidget    `json:"widgets"`
}

type rawVariable struct {
	Name    *string `json:"name"`
	Kind    *string `json:"kind"`
	Default *string `json:"default"`
}

type rawPlacement struct {
	WidgetID *string `json:"widget_id"`
	X        *int    `json:"x"`
	Y        *int    `json:"y"`
	W        *int    `json:"w"`
	H        *int    `json:"h"`
}

type rawWidget struct {
	ID           *string       `json:"id"`
	Kind         *string       `json:"kind"`
	Title        *string       `json:"title"`
	Unit         *string       `json:"unit"`
	Display      *rawDisplay   `json:"display"`
	TimeOverride *TimeOverride `json:"time_override"`
	Query        *rawQuery     `json:"query"`
	Text         *string       `json:"text"`
}

type rawDisplay struct {
	Stack    *bool   `json:"stack"`
	Decimals *int    `json:"decimals"`
	Limit    *int    `json:"limit"`
	Sort     *string `json:"sort"`
}

type rawQuery struct {
	Metric      *string         `json:"metric"`
	Aggregation *string         `json:"aggregation"`
	Filter      json.RawMessage `json:"filter"`
	GroupBy     []string        `json:"group_by"`
}

// Normalize는 DashboardSpec JSON을 검증하고 정규형을 돌려준다. 모르는 필드는 오류다.
func Normalize(body []byte) (Spec, error) {
	if len(body) > MaxSpecBytes {
		return Spec{}, &ValidationError{Violations: []Violation{{"body", fmt.Sprintf("at most %d bytes", MaxSpecBytes)}}}
	}
	var raw rawSpec
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return Spec{}, &ValidationError{Violations: []Violation{{"body", "must be a DashboardSpec JSON object without unknown fields"}}}
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Spec{}, &ValidationError{Violations: []Violation{{"body", "must contain a single JSON object"}}}
	}
	v := &validator{}
	s := Spec{Version: Version, Variables: []Variable{}, Layout: []Placement{}, Widgets: []Widget{}}
	if raw.Version != nil && *raw.Version != Version {
		v.add("version", fmt.Sprintf("must be %d", Version))
	}
	if raw.Title == nil {
		v.add("title", "required")
	} else {
		s.Title = strings.TrimSpace(*raw.Title)
		if !v.text("title", s.Title, 1, MaxTitleLength) {
			s.Title = ""
		}
	}
	if raw.Description != nil {
		s.Description = strings.TrimSpace(*raw.Description)
		v.text("description", s.Description, 0, MaxDescription)
	}

	// 개수 상한은 먼저 본다: 넘으면 항목별 검증을 하지 않고 바로 거절한다. 그래서 오류 수와 배치 겹침 검사(O(n²))가
	// 위젯 50개 안으로 묶인다 — 큰 본문 하나가 오류 목록으로 메모리를 쓰지 않는다(리뷰 반영, PS-0013)
	switch {
	case len(raw.Variables) > MaxVariables:
		return Spec{}, &ValidationError{Violations: []Violation{{"variables", fmt.Sprintf("at most %d", MaxVariables)}}}
	case len(raw.Widgets) > MaxWidgets:
		return Spec{}, &ValidationError{Violations: []Violation{{"widgets", fmt.Sprintf("at most %d widgets (D02 §14)", MaxWidgets)}}}
	case len(raw.Layout) > MaxWidgets:
		return Spec{}, &ValidationError{Violations: []Violation{{"layout", fmt.Sprintf("at most %d placements (one per widget)", MaxWidgets)}}}
	}
	// 변수
	vars := map[string]string{} // 이름 → 종류
	unsupportedVars := map[string]bool{}
	for i, rv := range raw.Variables {
		path := fmt.Sprintf("variables[%d]", i)
		out := Variable{}
		switch {
		case rv.Name == nil || !varNamePattern.MatchString(*rv.Name):
			v.add(path+".name", "required, [a-z][a-z0-9_]{0,31}")
		case vars[*rv.Name] != "":
			v.add(path+".name", "duplicate variable name")
		default:
			out.Name = *rv.Name
		}
		switch {
		case rv.Kind == nil:
			v.add(path+".kind", "required (environment or service)")
		case *rv.Kind == VarTeam:
			v.addUnsupported(path+".kind", "team variable needs service ownership (not available yet)")
			if rv.Name != nil {
				unsupportedVars[*rv.Name] = true // 이 변수의 참조도 "아직 지원하지 않음"이다(형식 오류가 아니다)
			}
		case VariableField[*rv.Kind] == "":
			v.add(path+".kind", "one of environment, service")
		default:
			out.Kind = *rv.Kind
		}
		if rv.Default != nil {
			d := strings.TrimSpace(*rv.Default)
			if v.text(path+".default", d, 1, MaxLabelLength) {
				out.Default = &d
			}
		}
		if out.Name != "" && out.Kind != "" {
			vars[out.Name] = out.Kind
		}
		s.Variables = append(s.Variables, out)
	}

	// 위젯
	ids := map[string]bool{}
	for i, rw := range raw.Widgets {
		w := v.widget(fmt.Sprintf("widgets[%d]", i), rw, vars, unsupportedVars)
		if w.ID != "" {
			if ids[w.ID] {
				v.add(fmt.Sprintf("widgets[%d].id", i), "duplicate widget id")
			}
			ids[w.ID] = true
		}
		s.Widgets = append(s.Widgets, w)
	}

	// 배치: 위젯마다 정확히 하나, 24열 안, 겹치지 않음(D05 §11)
	placed := map[string]bool{}
	for i, rp := range raw.Layout {
		path := fmt.Sprintf("layout[%d]", i)
		p := Placement{}
		if rp.WidgetID == nil || !ids[*rp.WidgetID] {
			v.add(path+".widget_id", "must be the id of a widget in this dashboard")
		} else if placed[*rp.WidgetID] {
			v.add(path+".widget_id", "widget placed twice")
		} else {
			p.WidgetID = *rp.WidgetID
			placed[p.WidgetID] = true
		}
		get := func(name string, ptr *int, lo, hi int) int {
			if ptr == nil || *ptr < lo || *ptr > hi {
				v.add(path+"."+name, fmt.Sprintf("integer %d..%d", lo, hi))
				return lo
			}
			return *ptr
		}
		p.X = get("x", rp.X, 0, GridColumns-1)
		p.Y = get("y", rp.Y, 0, MaxGridRows)
		p.W = get("w", rp.W, 1, GridColumns)
		p.H = get("h", rp.H, 1, MaxWidgetHeight)
		if p.X+p.W > GridColumns {
			v.add(path, fmt.Sprintf("x + w must be at most %d columns", GridColumns))
		}
		s.Layout = append(s.Layout, p)
	}
	for id := range ids {
		if !placed[id] {
			v.add("layout", "every widget needs a placement (missing "+id+")")
		}
	}
	for i := range s.Layout {
		for j := i + 1; j < len(s.Layout); j++ {
			// 위젯을 가리키지 않는 배치는 이미 오류다 — 겹침은 유효한 배치끼리만 본다
			if s.Layout[i].WidgetID != "" && s.Layout[j].WidgetID != "" && overlaps(s.Layout[i], s.Layout[j]) {
				v.add(fmt.Sprintf("layout[%d]", j), fmt.Sprintf("overlaps layout[%d]", i))
			}
		}
	}

	if len(v.violations) > 0 {
		return Spec{}, &ValidationError{Violations: v.violations, Unsupported: v.unsupported == len(v.violations)}
	}
	return s, nil
}

// Canonical은 정규형 JSON이다.
func (s Spec) Canonical() []byte {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // 정규형은 항상 직렬화된다
	}
	return b
}

func overlaps(a, b Placement) bool {
	return a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H
}

type validator struct {
	violations  []Violation
	unsupported int
}

func (v *validator) add(field, reason string) {
	v.violations = append(v.violations, Violation{field, reason})
}

func (v *validator) addUnsupported(field, reason string) {
	v.add(field, reason)
	v.unsupported++
}

// text는 길이(글자 수)·UTF-8·제어 문자를 본다. 줄바꿈·탭은 허용한다(설명·text 위젯).
func (v *validator) text(field, s string, lo, hi int) bool {
	n := utf8.RuneCountInString(s)
	if !utf8.ValidString(s) || n < lo || n > hi {
		v.add(field, fmt.Sprintf("%d..%d characters", lo, hi))
		return false
	}
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			v.add(field, "must not contain control characters")
			return false
		}
	}
	return true
}

func (v *validator) widget(path string, rw rawWidget, vars map[string]string, unsupported map[string]bool) Widget {
	w := Widget{}
	if rw.ID == nil || !widgetIDPattern.MatchString(*rw.ID) {
		v.add(path+".id", fmt.Sprintf("required, 1..%d of [A-Za-z0-9_-]", maxWidgetIDLength))
	} else {
		w.ID = *rw.ID
	}
	if rw.Title != nil {
		w.Title = strings.TrimSpace(*rw.Title)
		v.text(path+".title", w.Title, 0, MaxTitleLength)
	}
	if rw.Unit != nil {
		u := strings.TrimSpace(*rw.Unit)
		if v.text(path+".unit", u, 1, MaxUnitLength) {
			w.Unit = &u
		}
	}
	if rw.TimeOverride != nil {
		if !slices.Contains(TimeRanges, rw.TimeOverride.Range) {
			v.add(path+".time_override.range", "one of "+strings.Join(TimeRanges, ", "))
		} else {
			t := *rw.TimeOverride
			w.TimeOverride = &t
		}
	}
	if rw.Kind == nil {
		v.add(path+".kind", "required")
		return w
	}
	w.Kind = *rw.Kind
	d := rw.Display
	if d == nil {
		d = &rawDisplay{}
	}
	switch w.Kind {
	case KindText:
		if rw.Query != nil {
			v.add(path+".query", "text widget has no query")
		}
		if rw.Text == nil {
			v.add(path+".text", "required for a text widget")
		} else if v.text(path+".text", *rw.Text, 1, MaxTextLength) {
			t := *rw.Text
			w.Text = &t
		}
		v.noDisplay(path, d, false, false, false, false)
		return w
	case KindHistogram:
		v.addUnsupported(path+".kind", "histogram widget needs a bucket distribution query (not available yet)")
		return w
	case KindTimeSeries, KindStat, KindTable, KindTopList:
	default:
		v.add(path+".kind", "one of time_series, stat, table, top_list, text (histogram is not available yet)")
		return w
	}
	if rw.Text != nil {
		v.add(path+".text", "only text widgets have text")
	}
	if rw.Query == nil {
		v.add(path+".query", "required")
		return w
	}
	q := v.query(path+".query", rw.Query, vars, unsupported)
	switch w.Kind {
	case KindStat:
		if len(q.GroupBy) > 0 {
			v.add(path+".query.group_by", "a stat widget shows one number (no group_by)")
		}
	case KindTopList:
		if len(q.GroupBy) == 0 {
			v.add(path+".query.group_by", "a top list needs at least one group_by key")
		}
	}
	w.Query = &q
	// 표시 기본값(종류가 쓰는 것만)
	switch w.Kind {
	case KindTimeSeries:
		v.noDisplay(path, d, true, false, false, false)
		stack := d.Stack != nil && *d.Stack
		w.Display.Stack = &stack
	case KindStat:
		v.noDisplay(path, d, false, true, false, false)
		w.Display.Decimals = v.decimals(path, d.Decimals)
	case KindTable, KindTopList:
		v.noDisplay(path, d, false, true, true, true)
		w.Display.Decimals = v.decimals(path, d.Decimals)
		limit := DefaultLimit
		if d.Limit != nil {
			if *d.Limit < 1 || *d.Limit > MaxDisplayLimit {
				v.add(path+".display.limit", fmt.Sprintf("1..%d", MaxDisplayLimit))
			} else {
				limit = *d.Limit
			}
		}
		w.Display.Limit = &limit
		sort := "desc"
		if d.Sort != nil {
			if *d.Sort != "desc" && *d.Sort != "asc" {
				v.add(path+".display.sort", "desc or asc")
			} else {
				sort = *d.Sort
			}
		}
		w.Display.Sort = &sort
	}
	return w
}

// noDisplay는 그 종류가 쓰지 않는 표시 필드를 거절한다(조용히 버리지 않는다).
func (v *validator) noDisplay(path string, d *rawDisplay, stack, decimals, limit, sort bool) {
	if d.Stack != nil && !stack {
		v.add(path+".display.stack", "not used by this widget kind")
	}
	if d.Decimals != nil && !decimals {
		v.add(path+".display.decimals", "not used by this widget kind")
	}
	if d.Limit != nil && !limit {
		v.add(path+".display.limit", "not used by this widget kind")
	}
	if d.Sort != nil && !sort {
		v.add(path+".display.sort", "not used by this widget kind")
	}
}

func (v *validator) decimals(path string, p *int) *int {
	n := DefaultDecimals
	if p != nil {
		if *p < 0 || *p > MaxDecimals {
			v.add(path+".display.decimals", fmt.Sprintf("0..%d", MaxDecimals))
		} else {
			n = *p
		}
	}
	return &n
}

func (v *validator) query(path string, q *rawQuery, vars map[string]string, unsupported map[string]bool) MetricQuery {
	out := MetricQuery{GroupBy: []string{}}
	if q.Metric == nil || strings.TrimSpace(*q.Metric) == "" || len(*q.Metric) > MaxMetricLength || !plain(*q.Metric) {
		v.add(path+".metric", fmt.Sprintf("required, at most %d bytes of UTF-8 without control characters", MaxMetricLength))
	} else {
		out.Metric = strings.TrimSpace(*q.Metric)
	}
	if q.Aggregation == nil || !aggregations[*q.Aggregation] {
		v.add(path+".aggregation", "one of rate, increase, sum, avg, min, max, count, hist_sum, p50, p90, p95, p99")
	} else {
		out.Aggregation = *q.Aggregation
	}
	if len(q.GroupBy) > MaxGroupBy {
		v.add(path+".group_by", fmt.Sprintf("at most %d keys", MaxGroupBy))
	}
	seen := map[string]bool{}
	for i, k := range q.GroupBy {
		if k == "" || len(k) > MaxLabelLength || seen[k] || !plain(k) {
			v.add(fmt.Sprintf("%s.group_by[%d]", path, i), "non-empty, unique, at most 256 bytes")
			continue
		}
		seen[k] = true
		out.GroupBy = append(out.GroupBy, k)
	}
	if len(q.Filter) > 0 && string(q.Filter) != "null" {
		var f FilterNode
		dec := json.NewDecoder(bytes.NewReader(q.Filter))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			v.add(path+".filter", "must be a filter AST (and/eq)")
			return out
		}
		leaves := 0
		v.filter(path+".filter", &f, 1, &leaves, vars, unsupported)
		if leaves > MaxFilterLeaves {
			v.add(path+".filter", fmt.Sprintf("at most %d conditions", MaxFilterLeaves))
		}
		out.Filter = &f
	}
	return out
}

func (v *validator) filter(path string, n *FilterNode, depth int, leaves *int, vars map[string]string, unsupported map[string]bool) {
	if depth > MaxFilterDepth {
		v.add(path, fmt.Sprintf("depth must be at most %d", MaxFilterDepth))
		return
	}
	switch n.Op {
	case "and":
		if len(n.Args) == 0 || n.Field != "" || n.Value != nil {
			v.add(path, "and needs args only")
		}
		for i := range n.Args {
			v.filter(fmt.Sprintf("%s.args[%d]", path, i), &n.Args[i], depth+1, leaves, vars, unsupported)
		}
	case "eq":
		*leaves++
		if n.Field == "" || len(n.Field) > MaxLabelLength || len(n.Args) > 0 || !plain(n.Field) {
			v.add(path, "eq needs field (1..256 bytes, no control characters) and value")
		}
		if n.Value == nil || *n.Value == "" || len(*n.Value) > MaxLabelLength || !plain(*n.Value) {
			v.add(path+".value", "string of 1..256 bytes without control characters")
			return
		}
		// "$이름"은 변수다: 있는 변수이고, 그 변수 종류의 field에만 쓴다(environment 변수로 service.name을 거르지 않게)
		if name, ok := strings.CutPrefix(*n.Value, variablePrefix); ok {
			kind := vars[name]
			switch {
			case unsupported[name]:
				v.addUnsupported(path+".value", "$"+name+" is a team variable (not available yet)")
			case kind == "":
				v.add(path+".value", "unknown variable $"+name)
			case VariableField[kind] != n.Field:
				v.add(path+".value", fmt.Sprintf("$%s is a %s variable; use it on %s", name, kind, VariableField[kind]))
			}
		}
	case "or", "not", "in", "neq", "contains", "gt", "gte", "lt", "lte", "exists":
		v.addUnsupported(path+".op", "metric filter supports only and/eq for now")
	default:
		v.add(path+".op", "unknown op")
	}
}

// plain은 label·metric 이름 검사다: UTF-8이고 제어 문자가 없다(개행·NUL이 정규형에 들어가 조회·화면·로그를 망치지 않게).
func plain(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
