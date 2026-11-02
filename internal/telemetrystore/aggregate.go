package telemetrystore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/queryplan"
)

// 집계 한도 (ADR 0074 §1·§5, 시작값).
const (
	MaxAggregateFacets = 4
	// MaxSeparateFacets는 요청당 따로 세는 facet(속성·pod — 값 수가 커서 group 상한을 두는 것) 수다(ADR 0074 §5, 0075 §3).
	MaxSeparateFacets    = 2
	MaxAttributeFacets   = MaxSeparateFacets
	DefaultFacetTopN     = 10
	MaxFacetTopN         = 20
	MaxHistogramBuckets  = 60
	MaxFacetGroups       = 100000
	HistogramRecentGrace = 60 * time.Second
	// attributeBytesBudget은 속성 facet query 하나의 읽기 상한이다(query 계정 기본 max_bytes_to_read 10GB의 절반) —
	// 한 요청이 검색 한 번의 2배를 넘게 읽지 않는다(ADR 0074 §5). profile(migrations/clickhouse 00001)의 값을 바꾸면 함께 바꾼다.
	attributeBytesBudget = 5_000_000_000
	// aggregateQueryTime은 query 하나의 실행 상한이다(interactive 기본 5초와 같다). 요청에 남은 시간이 더 짧으면 그것이다.
	aggregateQueryTime = 5 * time.Second
)

// facetGroupLimit은 group 수 상한이다. 시험이 작은 값으로 초과를 확인할 수 있게 변수로 둔다.
var facetGroupLimit = MaxFacetGroups

// histogramSteps는 히스토그램 간격 후보다(ADR 0074 §4).
var histogramSteps = []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour}

// HistogramStep은 범위 길이를 MaxHistogramBuckets로 나눈 값 이상인 가장 짧은 간격이다.
func HistogramStep(r TimeRange) time.Duration {
	need := r.To.Sub(r.From) / MaxHistogramBuckets
	for _, s := range histogramSteps {
		if s >= need {
			return s
		}
	}
	return histogramSteps[len(histogramSteps)-1]
}

// histogramOrigin은 범위 시작 이하의 가장 큰 간격 배수(UTC)다.
func histogramOrigin(r TimeRange, step time.Duration) time.Time {
	return r.From.UTC().Truncate(step)
}

// FacetKind는 facet 종류다(ADR 0074 §3 표).
type FacetKind int

const (
	FacetRootService        FacetKind = iota + 1 // traces: root span 서비스
	FacetHasError                                // traces: 오류 span이 있음
	FacetService                                 // logs: 서비스
	FacetSeverity                                // logs: 심각도 묶음
	FacetAttribute                               // logs: attributes.<key>
	FacetServiceVersion                          // logs: service.version, (service_id, version)으로 센다 (ADR 0075)
	FacetK8sPod                                  // logs: k8s.pod.name, (namespace, pod)으로 센다 (ADR 0075, 따로 센다)
	FacetRootServiceVersion                      // traces: root span의 service.version, (root 서비스, version)으로 센다 (ADR 0075)
)

// separate는 따로 세는 facet인지다(값 수가 커서 group 상한 query로 센다).
func (k FacetKind) separate() bool { return k == FacetAttribute || k == FacetK8sPod }

// Facet 값 맥락 key(FacetValue.Context).
const (
	ContextServiceID    = "service_id"
	ContextK8sNamespace = "k8s.namespace.name"
)

// SeverityClasses는 log 심각도 묶음 이름이다(OTel SeverityNumber 0, 1–4, 5–8, 9–12, 13–16, 17–20, 21–24).
// 범위 밖(25 이상)은 unspecified다.
var SeverityClasses = []string{"unspecified", "trace", "debug", "info", "warn", "error", "fatal"}

// severityClassSQL은 severity column을 묶음 번호(SeverityClasses index)로 바꾸는 고정 표현이다.
const severityClassSQL = `toUInt32(multiIf(severity = 0 OR severity > 24, 0, intDiv(severity - 1, 4) + 1))`

// FacetSpec은 집계할 facet 하나다. Filter는 이 facet의 소유 조건을 뺀 filter를 컴파일한 것이다(queryplan.Without).
// trace facet의 Filter는 요약 단계 조건이다(span 단계 조건은 facet마다 다를 수 없다).
type FacetSpec struct {
	Field        string // 응답 이름
	Kind         FacetKind
	AttributeKey string
	Filter       queryplan.Compiled
	TopN         int
	Excludes     bool // 소유 조건을 뺐다
}

// FacetValue는 facet 값 하나의 수다.
type FacetValue struct {
	Value string
	Count uint64
	// Context는 같은 문자열의 다른 대상을 가르는 맥락이다(버전의 서비스, pod의 namespace, ADR 0075 §3). 없으면 nil.
	Context map[string]string
}

// FacetResult는 facet 하나의 결과다. Values가 nil이면 Reason이 있다(too_many_values·budget_exceeded·timeout).
type FacetResult struct {
	Field             string
	Values            []FacetValue
	Other, Missing    uint64
	Reason            string
	ExcludesOwnFilter bool
}

// 속성 facet을 셀 수 없는 사유.
const (
	FacetTooManyValues  = "too_many_values"
	FacetBudgetExceeded = "budget_exceeded"
	FacetTimeout        = "timeout"
)

// HistogramBucket은 칸 하나다. Count가 nil이면 Reason(future)이다. Incomplete면 수집 지연으로 더 들어올 수 있다.
type HistogramBucket struct {
	Start, End time.Time
	Count      *uint64
	ErrorCount *uint64  // traces
	BySeverity []uint64 // logs, SeverityClasses 순서
	Incomplete bool
	Reason     string
}

// Histogram은 시간 히스토그램이다.
type Histogram struct {
	Step    time.Duration
	Buckets []HistogramBucket
}

// AggregateResult는 집계 결과다.
type AggregateResult struct {
	Total     uint64
	Histogram *Histogram
	Facets    []FacetResult
}

// LogAggregateQuery는 log 집계 조건이다. Filter는 검색과 같은 전체 filter다.
type LogAggregateQuery struct {
	Range               TimeRange
	Filter              queryplan.Compiled
	Facets              []FacetSpec
	Histogram           bool
	ReceivedBefore      time.Time
	EnvironmentServices []string
}

// TraceAggregateQuery는 trace 집계 조건이다. SpanFilter·TraceFilter는 검색과 같다.
type TraceAggregateQuery struct {
	Range               TimeRange
	SpanFilter          queryplan.Compiled
	TraceFilter         queryplan.Compiled
	Facets              []FacetSpec
	Histogram           bool
	ReceivedBefore      time.Time
	EnvironmentServices []string
}

func checkFacets(fs []FacetSpec, allowed ...FacetKind) error {
	if len(fs) > MaxAggregateFacets {
		return invalidArg("facets", fmt.Sprintf("at most %d", MaxAggregateFacets))
	}
	attrs := 0
	for _, f := range fs {
		ok := false
		for _, k := range allowed {
			ok = ok || f.Kind == k
		}
		if !ok {
			return invalidArg("facets", "facet not allowed for this signal: "+f.Field)
		}
		if f.TopN < 1 || f.TopN > MaxFacetTopN {
			return invalidArg("facets", fmt.Sprintf("top_n must be within [1, %d]", MaxFacetTopN))
		}
		if f.Filter.SQL == "" || f.Filter.Unresolved {
			return errors.New("telemetrystore: facet filters must be compiled with resolved service names")
		}
		if f.Kind.separate() {
			attrs++
		}
	}
	if attrs > MaxSeparateFacets {
		return invalidArg("facets", fmt.Sprintf("at most %d attribute or pod facets", MaxSeparateFacets))
	}
	return nil
}

// scope는 environment 범위 mandatory predicate의 값이다(검색과 같다).
func aggregateScope(p authz.Principal, services []string) (uint8, []string, error) {
	if !p.EnvironmentRestricted() {
		return 0, []string{}, nil
	}
	if services == nil {
		return 0, nil, ErrEnvironmentScoped
	}
	if len(services) > MaxEnvironmentServices {
		return 0, nil, errors.New("telemetrystore: environment service scope too large")
	}
	return 1, services, nil
}

// aggregateSettings는 query 하나의 설정이다: tenant, 남은 시간 안의 실행 상한. 속성 facet이면 group 수 상한(넘으면 throw —
// any·break는 조용히 틀린 수를 낸다, ADR 0074 §5)과 읽기 상한 절반. 본 query에는 group 상한을 두지 않는다 — trace 요약의
// GROUP BY trace_id가 trace 수만큼 group을 만들고, 고정 facet(sumMapIf)은 GROUP BY가 아니다.
func aggregateSettings(ctx context.Context, tenant authz.TenantID, attribute bool) clickhouse.Settings {
	exec := aggregateQueryTime
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); left < exec {
			exec = left
		}
	}
	settings := clickhouse.Settings{
		TenantSetting:        clickhouse.CustomSetting{Value: tenant.String()},
		"max_execution_time": executionSeconds(exec),
	}
	if attribute {
		settings["max_rows_to_group_by"] = facetGroupLimit
		settings["group_by_overflow_mode"] = "throw"
		settings["max_bytes_to_read"] = attributeBytesBudget
	}
	return settings
}

func aggregateContext(ctx context.Context, tenant authz.TenantID, attribute bool) context.Context {
	return clickhouse.Context(ctx, clickhouse.WithSettings(aggregateSettings(ctx, tenant, attribute)))
}

// isGroupOverflow는 max_rows_to_group_by 초과(TOO_MANY_ROWS 중 group by 상한)다. 읽기 상한 초과도 같은 코드라 문구로 가른다.
// 서버 문구는 "Limit for rows to GROUP BY exceeded"다(26.3 실측, 설정 이름이 들어 있지 않다 — PS-0018). 통합 시험이 고정한다.
func isGroupOverflow(err error) bool {
	var ex *clickhouse.Exception
	return errors.As(err, &ex) && ex.Code == 158 &&
		(strings.Contains(ex.Message, "rows to GROUP BY") || strings.Contains(ex.Message, "max_rows_to_group_by"))
}

func validateAggregate(p authz.Principal, r TimeRange, now time.Time) error {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return err
	}
	if now.IsZero() {
		return errors.New("telemetrystore: now is required")
	}
	return r.validate(MaxLogSearchRange)
}

// addParams는 컴파일 결과의 parameter를 더한다(facet마다 다른 접두어로 컴파일되어 이름이 겹치지 않는다).
func addParams(args []any, cs ...queryplan.Compiled) []any {
	for _, c := range cs {
		for name, v := range c.Params {
			args = append(args, clickhouse.Named(name, v))
		}
	}
	return args
}

// aggCol은 집계 query의 column 하나다. count는 UInt64 하나, 나머지는 sumMapIf 결과(keys, counts)다.
type aggCol struct {
	expr   string
	count  *uint64
	strs   *[]string
	ints   *[]uint32
	counts *[]uint64
}

func countCol(expr string) aggCol { return aggCol{expr: expr, count: new(uint64)} }

func strMapCol(expr string) aggCol {
	return aggCol{expr: expr, strs: new([]string), counts: new([]uint64)}
}

func intMapCol(expr string) aggCol {
	return aggCol{expr: expr, ints: new([]uint32), counts: new([]uint64)}
}

// aggregateSQL은 안쪽 행(from)에 column들을 한 번에 집계하는 query다. sumMapIf의 Tuple은 바깥에서 두 column으로 편다.
func aggregateSQL(cols []aggCol, from string) string {
	in := make([]string, len(cols))
	out := make([]string, 0, len(cols)*2)
	for i, c := range cols {
		in[i] = fmt.Sprintf("%s AS c%d", c.expr, i)
		if c.count != nil {
			out = append(out, fmt.Sprintf("c%d", i))
		} else {
			out = append(out, fmt.Sprintf("tupleElement(c%d, 1)", i), fmt.Sprintf("tupleElement(c%d, 2)", i))
		}
	}
	return "SELECT " + strings.Join(out, ", ") + " FROM (SELECT " + strings.Join(in, ", ") + " FROM (" + from + "))"
}

func scanDest(cols []aggCol) []any {
	var dest []any
	for _, c := range cols {
		switch {
		case c.count != nil:
			dest = append(dest, c.count)
		case c.strs != nil:
			dest = append(dest, c.strs, c.counts)
		default:
			dest = append(dest, c.ints, c.counts)
		}
	}
	return dest
}

func (c aggCol) strMap() map[string]uint64 {
	out := map[string]uint64{}
	for i, k := range *c.strs {
		out[k] += (*c.counts)[i]
	}
	return out
}

func (c aggCol) intMap() map[uint32]uint64 {
	out := map[uint32]uint64{}
	for i, k := range *c.ints {
		out[k] += (*c.counts)[i]
	}
	return out
}

// topValues는 값별 수를 top_n·other·missing으로 나눈다. 수 내림차순, 같으면 값 오름차순. missingKey 값은 missing이다.
func topValues(counts map[string]uint64, topN int, missingKey string, hasMissing bool) ([]FacetValue, uint64, uint64) {
	var missing uint64
	vals := make([]FacetValue, 0, len(counts))
	for k, c := range counts {
		if hasMissing && k == missingKey {
			missing += c
			continue
		}
		if c > 0 {
			vals = append(vals, FacetValue{Value: k, Count: c})
		}
	}
	sort.Slice(vals, func(i, j int) bool {
		if vals[i].Count != vals[j].Count {
			return vals[i].Count > vals[j].Count
		}
		return vals[i].Value < vals[j].Value
	})
	var other uint64
	if len(vals) > topN {
		for _, v := range vals[topN:] {
			other += v.Count
		}
		vals = vals[:topN]
	}
	return vals, other, missing
}

// pairSep은 맥락과 값을 한 map key로 잇는 구분자다. 맥락(service_id UUID)에는 나오지 않는다.
const pairSep = "\x00"

// pairValues는 "<맥락>\x00<값>" key의 수를 facet 값으로 나눈다. 값이 ""이면(또는 key가 "") 값 없음이다.
// 수 내림차순, 같으면 값·맥락 오름차순.
func pairValues(counts map[string]uint64, topN int, contextKey string) ([]FacetValue, uint64, uint64) {
	var missing uint64
	vals := make([]FacetValue, 0, len(counts))
	for k, c := range counts {
		ctx, v, ok := strings.Cut(k, pairSep)
		if !ok || v == "" {
			missing += c
			continue
		}
		if c > 0 {
			vals = append(vals, FacetValue{Value: v, Count: c, Context: map[string]string{contextKey: ctx}})
		}
	}
	sort.Slice(vals, func(i, j int) bool {
		a, b := vals[i], vals[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Value != b.Value {
			return a.Value < b.Value
		}
		return a.Context[contextKey] < b.Context[contextKey]
	})
	var other uint64
	if len(vals) > topN {
		for _, v := range vals[topN:] {
			other += v.Count
		}
		vals = vals[:topN]
	}
	return vals, other, missing
}

// buildHistogram은 칸별 수(index → 수)를 칸 목록으로 만든다. 범위로 자른 경계 칸, snapshot 이후 칸(null, future),
// snapshot 직전 HistogramRecentGrace에 걸친 칸(incomplete)을 표시한다. 빈 지난 칸은 0이다(ADR 0074 §4).
func buildHistogram(r TimeRange, step time.Duration, snapshot time.Time, counts map[uint32]uint64, errs map[uint32]uint64, sev map[uint32]uint64) *Histogram {
	origin := histogramOrigin(r, step)
	h := &Histogram{Step: step}
	for i := uint32(0); ; i++ {
		start := origin.Add(time.Duration(i) * step)
		if !start.Before(r.To) {
			break
		}
		end := start.Add(step)
		b := HistogramBucket{Start: maxTime(start, r.From), End: minTime(end, r.To)}
		if !b.Start.Before(snapshot) {
			b.Reason = "future"
			h.Buckets = append(h.Buckets, b)
			continue
		}
		c := counts[i]
		b.Count = &c
		if errs != nil {
			e := errs[i]
			b.ErrorCount = &e
		}
		if sev != nil {
			b.BySeverity = make([]uint64, len(SeverityClasses))
			for k := range SeverityClasses {
				b.BySeverity[k] = sev[i*uint32(len(SeverityClasses))+uint32(k)] //nolint:gosec // 7개
			}
		}
		b.Incomplete = b.End.After(snapshot.Add(-HistogramRecentGrace))
		h.Buckets = append(h.Buckets, b)
	}
	return h
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// AggregateLogs는 log 총 수·히스토그램·facet을 센다(ADR 0074). 검색과 같은 mandatory predicate와 dedup(event_id마다
// 최초 수신 한 행)을 한 번 거친 뒤, 전체 filter와 facet별 filter를 조건부 집계(sumMapIf)로 함께 센다. 속성 facet만
// 같은 snapshot으로 따로 센다(값 수가 커서 group 상한을 둔다).
func (s *QueryStore) AggregateLogs(ctx context.Context, p authz.Principal, q LogAggregateQuery, now time.Time) (AggregateResult, error) {
	if err := validateAggregate(p, q.Range, now); err != nil {
		return AggregateResult{}, err
	}
	scoped, scope, err := aggregateScope(p, q.EnvironmentServices)
	if err != nil {
		return AggregateResult{}, err
	}
	if q.Filter.SQL == "" || q.Filter.Unresolved {
		return AggregateResult{}, errors.New("telemetrystore: filter must be compiled with resolved service names (queryplan.CompileWith)")
	}
	if err := checkFacets(q.Facets, FacetService, FacetSeverity, FacetAttribute, FacetServiceVersion, FacetK8sPod); err != nil {
		return AggregateResult{}, err
	}
	snapshot := q.ReceivedBefore
	if snapshot.IsZero() {
		snapshot = now
	}
	step := HistogramStep(q.Range)
	origin := histogramOrigin(q.Range, step)
	args := []any{
		clickhouse.Named("tenant", p.Tenant().String()),
		clickhouse.Named("from", ch64(q.Range.From)), clickhouse.Named("to", ch64(q.Range.To)),
		clickhouse.Named("now", seconds(now)),
		clickhouse.Named("min_version", minVersionReceivedBy(snapshot)),
		clickhouse.Named("env_scoped", scoped), clickhouse.Named("env_services", scope),
		clickhouse.Named("origin", origin.Unix()), clickhouse.Named("step", uint32(step/time.Second)), //nolint:gosec // ≤ 1시간
	}
	args = addParams(args, q.Filter)
	// 안쪽은 검색과 같다(mandatory predicate만으로 고른 뒤 event_id마다 최초 수신 한 행). 바깥은 조건부 집계뿐이다.
	// 조건 조각은 queryplan이 만든 고정 column 표현과 parameter뿐이다(계약 4).
	inner := `
		SELECT service_id, event_time, severity, trace_id, span_id, body, attributes,
		       service_version, service_instance_id, k8s_namespace_name, k8s_pod_name,
		       toUInt32(intDiv(toUnixTimestamp(event_time) - {origin:Int64}, {step:UInt32})) AS idx
		FROM logs_local
		WHERE tenant_id = {tenant:UUID}
		  AND event_time >= {from:DateTime64(9, 'UTC')} AND event_time < {to:DateTime64(9, 'UTC')}
		  AND expires_at > {now:DateTime('UTC')}
		  AND version >= {min_version:UInt64}
		  AND ({env_scoped:UInt8} = 0 OR service_id IN {env_services:Array(UUID)})
		ORDER BY event_id, version DESC
		LIMIT 1 BY event_id`
	base := append([]any{}, args...)
	total := countCol("countIf(" + q.Filter.SQL + ")")
	cols := []aggCol{total}
	var hist, histSev aggCol
	if q.Histogram {
		hist = intMapCol("sumMapIf([idx], [toUInt64(1)], " + q.Filter.SQL + ")")
		histSev = intMapCol("sumMapIf([toUInt32(idx * 7 + " + severityClassSQL + ")], [toUInt64(1)], " + q.Filter.SQL + ")")
		cols = append(cols, hist, histSev)
	}
	facetCol := map[int]aggCol{}
	for i, f := range q.Facets {
		var c aggCol
		switch f.Kind {
		case FacetService:
			c = strMapCol("sumMapIf([toString(service_id)], [toUInt64(1)], " + f.Filter.SQL + ")")
		case FacetSeverity:
			c = intMapCol("sumMapIf([" + severityClassSQL + "], [toUInt64(1)], " + f.Filter.SQL + ")")
		case FacetServiceVersion:
			// 서비스별로 가른다(다른 서비스의 같은 버전 문자열을 합치지 않는다, ADR 0075 §3)
			c = strMapCol("sumMapIf([concat(toString(service_id), '\\x00', service_version)], [toUInt64(1)], " + f.Filter.SQL + ")")
		default:
			continue // 속성 facet은 따로
		}
		facetCol[i] = c
		cols = append(cols, c)
		args = addParams(args, f.Filter)
	}
	if err := s.conn.QueryRow(aggregateContext(ctx, p.Tenant(), false), aggregateSQL(cols, inner), args...).Scan(scanDest(cols)...); err != nil {
		return AggregateResult{}, classify("log aggregate", err)
	}
	out := AggregateResult{Total: *total.count, Facets: make([]FacetResult, len(q.Facets))}
	if q.Histogram {
		out.Histogram = buildHistogram(q.Range, step, snapshot, hist.intMap(), nil, histSev.intMap())
	}
	for i, f := range q.Facets {
		res := FacetResult{Field: f.Field, ExcludesOwnFilter: f.Excludes}
		switch f.Kind {
		case FacetService:
			res.Values, res.Other, res.Missing = topValues(facetCol[i].strMap(), f.TopN, "", false)
		case FacetSeverity:
			named := map[string]uint64{}
			for k, c := range facetCol[i].intMap() {
				if int(k) < len(SeverityClasses) {
					named[SeverityClasses[k]] += c
				}
			}
			res.Values, res.Other, res.Missing = topValues(named, f.TopN, "", false)
		case FacetServiceVersion:
			res.Values, res.Other, res.Missing = pairValues(facetCol[i].strMap(), f.TopN, ContextServiceID)
		case FacetAttribute, FacetK8sPod:
			var err error
			if res, err = s.logSeparateFacet(ctx, p, f, inner, base); err != nil {
				return AggregateResult{}, err
			}
		}
		out.Facets[i] = res
	}
	return out, nil
}

// logSeparateFacet은 따로 세는 facet(속성·pod) 하나를 같은 snapshot·같은 안쪽 행으로 센다. group 상한을 넘거나 예산·시간을 넘으면 그 facet만
// 값 없이 사유를 준다 — 근사 값으로 바꾸지 않는다(계약 6, ADR 0074 §5). 그 밖의 실패(연결·취소)는 요청 전체의 실패다.
func (s *QueryStore) logSeparateFacet(ctx context.Context, p authz.Principal, f FacetSpec, inner string, base []any) (FacetResult, error) {
	res := FacetResult{Field: f.Field, ExcludesOwnFilter: f.Excludes}
	args := append(append([]any{}, base...), clickhouse.Named("attr_key", f.AttributeKey), clickhouse.Named("top", uint32(f.TopN+1))) //nolint:gosec // ≤ 21
	args = addParams(args, f.Filter)
	// 값 식과 맥락 식은 고정 표현이다(사용자 값은 attr_key parameter뿐). pod는 namespace별로 가른다(ADR 0075 §3).
	valueSQL, ctxSQL, ctxKey := `if(mapContains(attributes, {attr_key:String}), attributes[{attr_key:String}], NULL)`, `''`, ""
	if f.Kind == FacetK8sPod {
		valueSQL, ctxSQL, ctxKey = `if(k8s_pod_name != '', k8s_pod_name, NULL)`, `if(k8s_pod_name != '', k8s_namespace_name, '')`, ContextK8sNamespace
	}
	// 값 없음은 NULL 한 묶음이다. NULL을 맨 앞에 두고 top_n+1개를 읽어 missing과 top_n 값을 함께 얻는다.
	// all은 LIMIT 전 전체 합이다(window). other = all − missing − Σvalues.
	rows, err := s.conn.Query(aggregateContext(ctx, p.Tenant(), true), `
		SELECT v, ctx, c, sum(c) OVER () AS all
		FROM (
			SELECT `+valueSQL+` AS v, `+ctxSQL+` AS ctx, count() AS c
			FROM (`+inner+`)
			WHERE `+f.Filter.SQL+`
			GROUP BY v, ctx
		)
		ORDER BY isNull(v) DESC, c DESC, v ASC, ctx ASC
		LIMIT {top:UInt32}`, args...)
	if err != nil {
		return facetFailure(res, err)
	}
	defer func() { _ = rows.Close() }()
	var all, sum uint64
	vals := []FacetValue{}
	for rows.Next() {
		var (
			v      *string
			ctxVal string
			c      uint64
		)
		if err := rows.Scan(&v, &ctxVal, &c, &all); err != nil {
			return res, fmt.Errorf("telemetrystore: scan separate facet: %w", err)
		}
		if v == nil {
			res.Missing = c
			continue
		}
		if len(vals) < f.TopN {
			fv := FacetValue{Value: *v, Count: c}
			// 맥락 값이 없으면(pod는 있는데 namespace 속성이 없음) key를 생략한다 — 빈 문자열을 값처럼 보이지 않는다(계약 6)
			if ctxKey != "" && ctxVal != "" {
				fv.Context = map[string]string{ctxKey: ctxVal}
			}
			vals = append(vals, fv)
			sum += c
		}
	}
	if err := rows.Err(); err != nil {
		return facetFailure(res, err)
	}
	res.Values, res.Other = vals, all-res.Missing-sum
	return res, nil
}

// facetFailure는 속성 facet query의 실패를 그 facet의 사유로 바꾼다. 사유가 아닌 실패는 그대로 돌려준다.
func facetFailure(res FacetResult, err error) (FacetResult, error) {
	res.Values, res.Other, res.Missing = nil, 0, 0
	c := classify("attribute facet", err)
	var (
		budget  *budgetError
		timeout *timeoutError
	)
	switch {
	case isGroupOverflow(err):
		res.Reason = FacetTooManyValues
	case errors.As(c, &timeout) || errors.Is(err, context.DeadlineExceeded):
		res.Reason = FacetTimeout
	case errors.As(c, &budget):
		res.Reason = FacetBudgetExceeded
	default:
		return res, c
	}
	return res, nil
}

// AggregateTraces는 저장된 trace 총 수·시작 시각 히스토그램·facet을 센다(ADR 0074). 검색과 같은 trace 요약을 한 번 거친 뒤
// (시작이 범위 안인 trace), 전체 요약 조건과 facet별 요약 조건을 조건부 집계로 함께 센다. trace facet은 모두 요약 단계
// field만 소유하므로 span 단계 조건(matched)은 하나를 공유한다.
//
// 수는 tail sampling을 거친 표본의 수다 — 요청 수·오류율이 아니다(계약 5, 호출자가 meta.sampled=true).
func (s *QueryStore) AggregateTraces(ctx context.Context, p authz.Principal, q TraceAggregateQuery, now time.Time) (AggregateResult, error) {
	if err := validateAggregate(p, q.Range, now); err != nil {
		return AggregateResult{}, err
	}
	scoped, scope, err := aggregateScope(p, q.EnvironmentServices)
	if err != nil {
		return AggregateResult{}, err
	}
	for _, f := range []queryplan.Compiled{q.SpanFilter, q.TraceFilter} {
		if f.SQL == "" || f.Unresolved {
			return AggregateResult{}, errors.New("telemetrystore: filters must be compiled with resolved service names (queryplan.CompileWith)")
		}
	}
	if err := checkFacets(q.Facets, FacetRootService, FacetHasError, FacetRootServiceVersion); err != nil {
		return AggregateResult{}, err
	}
	snapshot := q.ReceivedBefore
	if snapshot.IsZero() {
		snapshot = now
	}
	step := HistogramStep(q.Range)
	origin := histogramOrigin(q.Range, step)
	args := []any{
		clickhouse.Named("tenant", p.Tenant().String()),
		clickhouse.Named("from", ch64(q.Range.From)), clickhouse.Named("to", ch64(q.Range.To)),
		clickhouse.Named("pad_from", ch64(q.Range.From.Add(-TraceSearchPadding))), clickhouse.Named("pad_to", ch64(q.Range.To.Add(TraceSearchPadding))),
		clickhouse.Named("now", seconds(now)),
		clickhouse.Named("min_version", minVersionReceivedBy(snapshot)),
		clickhouse.Named("env_scoped", scoped), clickhouse.Named("env_services", scope),
		clickhouse.Named("span_limit", TraceSummarySpanLimit),
		clickhouse.Named("origin", origin.Unix()), clickhouse.Named("step", uint32(step/time.Second)), //nolint:gosec // ≤ 1시간
	}
	args = addParams(args, q.SpanFilter, q.TraceFilter)
	rows := `
		SELECT start_time, duration_ms, span_count, has_error, roots, root_service_id, root_service_version,
		       toUInt32(intDiv(toUnixTimestamp(start_time) - {origin:Int64}, {step:UInt32})) AS idx
		FROM (` + traceSummarySQL(q.SpanFilter.SQL) + `)
		WHERE start_time >= {from:DateTime64(9, 'UTC')} AND start_time < {to:DateTime64(9, 'UTC')}`
	full := q.TraceFilter.SQL
	total := countCol("countIf(" + full + ")")
	cols := []aggCol{total}
	var hist, histErr aggCol
	if q.Histogram {
		hist = intMapCol("sumMapIf([idx], [toUInt64(1)], " + full + ")")
		histErr = intMapCol("sumMapIf([idx], [toUInt64(has_error)], " + full + ")")
		cols = append(cols, hist, histErr)
	}
	facetCol := make([]aggCol, len(q.Facets))
	for i, f := range q.Facets {
		switch f.Kind {
		case FacetRootService:
			// root span이 없는 trace는 값 없음('')이다
			facetCol[i] = strMapCol("sumMapIf([if(roots > 0, toString(root_service_id), '')], [toUInt64(1)], " + f.Filter.SQL + ")")
		case FacetHasError:
			facetCol[i] = strMapCol("sumMapIf([if(has_error, 'true', 'false')], [toUInt64(1)], " + f.Filter.SQL + ")")
		case FacetRootServiceVersion:
			// root 서비스별로 가른다. root가 없거나 버전이 없으면 값 없음(ADR 0075 §3)
			facetCol[i] = strMapCol("sumMapIf([if(roots > 0, concat(toString(root_service_id), '\\x00', root_service_version), '')], [toUInt64(1)], " + f.Filter.SQL + ")")
		}
		cols = append(cols, facetCol[i])
		args = addParams(args, f.Filter)
	}
	if err := s.conn.QueryRow(aggregateContext(ctx, p.Tenant(), false), aggregateSQL(cols, rows), args...).Scan(scanDest(cols)...); err != nil {
		return AggregateResult{}, classify("trace aggregate", err)
	}
	out := AggregateResult{Total: *total.count, Facets: make([]FacetResult, len(q.Facets))}
	if q.Histogram {
		out.Histogram = buildHistogram(q.Range, step, snapshot, hist.intMap(), histErr.intMap(), nil)
	}
	for i, f := range q.Facets {
		res := FacetResult{Field: f.Field, ExcludesOwnFilter: f.Excludes}
		if f.Kind == FacetRootServiceVersion {
			res.Values, res.Other, res.Missing = pairValues(facetCol[i].strMap(), f.TopN, ContextServiceID)
		} else {
			res.Values, res.Other, res.Missing = topValues(facetCol[i].strMap(), f.TopN, "", f.Kind == FacetRootService)
		}
		out.Facets[i] = res
	}
	return out, nil
}
