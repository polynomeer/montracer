package query

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/queryplan"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// AggregateStore는 trace·log 집계 저장소다 (telemetrystore.QueryStore, ADR 0074).
type AggregateStore interface {
	AggregateLogs(ctx context.Context, p authz.Principal, q telemetrystore.LogAggregateQuery, now time.Time) (telemetrystore.AggregateResult, error)
	AggregateTraces(ctx context.Context, p authz.Principal, q telemetrystore.TraceAggregateQuery, now time.Time) (telemetrystore.AggregateResult, error)
}

// aggregateRequest는 POST /api/v1/query/{traces,logs}/aggregate 본문이다(ADR 0074 §1). range·filter는 검색과 같다.
// projection·order·limit·cursor는 없다(모르는 field라 400).
type aggregateRequest struct {
	Signal       string           `json:"signal"`
	Range        *requestRange    `json:"range"`
	Filter       *queryplan.Node  `json:"filter"`
	Facets       []aggregateFacet `json:"facets"`
	Histogram    bool             `json:"histogram"`
	AllowPartial bool             `json:"allow_partial"`
}

type aggregateFacet struct {
	Field string `json:"field"`
	TopN  int    `json:"top_n"`
}

// facetDef는 facet 하나의 종류와 소유 filter field다(ADR 0074 §3 표). 화면은 facet 값을 그 field로 고른다.
type facetDef struct {
	kind  telemetrystore.FacetKind
	owned func(field string) bool
}

func fields(names ...string) func(string) bool {
	return func(f string) bool {
		for _, n := range names {
			if f == n {
				return true
			}
		}
		return false
	}
}

var (
	traceFacets = map[string]facetDef{
		"root_service_id": {telemetrystore.FacetRootService, fields("root_service_id")},
		"has_error":       {telemetrystore.FacetHasError, fields("has_error")},
		// root span의 버전, 서비스별로 가른다(ADR 0075)
		"root_service_version": {telemetrystore.FacetRootServiceVersion, fields("root_service_version")},
	}
	logFacets = map[string]facetDef{
		"service":  {telemetrystore.FacetService, fields("service_id", "service.name")},
		"severity": {telemetrystore.FacetSeverity, fields("severity_number")},
		// resource 속성(ADR 0075): 버전은 서비스별로, pod는 namespace별로 가른다. pod는 따로 센다
		"service.version": {telemetrystore.FacetServiceVersion, fields("service.version")},
		"k8s.pod":         {telemetrystore.FacetK8sPod, fields("k8s.pod.name", "k8s.namespace.name")},
	}
)

const attributeFacetPrefix = "attributes."

// facetDefFor는 요청 facet 이름의 정의다. attributes.<key>는 log만, key 검증은 filter와 같다(queryplan으로 컴파일해 본다).
func facetDefFor(signal, name string) (facetDef, string, bool) {
	if signal == "traces" {
		d, ok := traceFacets[name]
		return d, "", ok
	}
	if d, ok := logFacets[name]; ok {
		return d, "", true
	}
	if key, ok := strings.CutPrefix(name, attributeFacetPrefix); ok {
		probe := &queryplan.Node{Op: "exists", Field: name}
		if _, err := queryplan.Compile(probe, queryplan.LogCatalog); err != nil {
			return facetDef{}, "", false
		}
		return facetDef{telemetrystore.FacetAttribute, fields(name)}, key, true
	}
	return facetDef{}, "", false
}

// aggregate는 POST /api/v1/query/{signal}/aggregate다(ADR 0074). 경로가 signal을 정한다.
func (h *Handler) aggregate(signal string) principalHandler {
	return func(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
		if h.cfg.Aggregates == nil {
			return apierr.New(apierr.NotFound, "집계를 사용할 수 없습니다")
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			e := apierr.New(apierr.InvalidArgument, "요청 본문이 너무 큽니다")
			e.Status = http.StatusRequestEntityTooLarge
			return e
		}
		var req aggregateRequest
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			return invalid("body", "must be a JSON aggregate query (range, filter, facets, histogram)")
		}
		if req.Signal != "" && req.Signal != signal {
			return invalid("signal", "must match the path ("+signal+") or be omitted")
		}
		now := h.cfg.Now().UTC()
		rng := telemetrystore.TimeRange{From: now.Add(-defaultSearchRange), To: now}
		if req.Range != nil {
			rng = telemetrystore.TimeRange{From: req.Range.From.UTC(), To: req.Range.To.UTC()}
		}
		if len(req.Facets) > telemetrystore.MaxAggregateFacets {
			return invalid("facets", "at most "+strconv.Itoa(telemetrystore.MaxAggregateFacets))
		}
		seen, attrs := map[string]bool{}, 0
		for i, f := range req.Facets {
			path := "facets[" + strconv.Itoa(i) + "]"
			if _, _, ok := facetDefFor(signal, f.Field); !ok {
				return invalid(path+".field", "not an aggregatable field for "+signal)
			}
			if seen[f.Field] {
				return invalid(path+".field", "duplicate facet")
			}
			seen[f.Field] = true
			// 따로 세는 facet(속성·pod)은 요청당 상한이 있다(ADR 0074 §5, 0075 §3)
			if strings.HasPrefix(f.Field, attributeFacetPrefix) || f.Field == "k8s.pod" {
				if attrs++; attrs > telemetrystore.MaxSeparateFacets {
					return invalid(path+".field", "at most "+strconv.Itoa(telemetrystore.MaxSeparateFacets)+" attribute or pod facets")
				}
			}
			if f.TopN < 0 || f.TopN > telemetrystore.MaxFacetTopN {
				return invalid(path+".top_n", "must be within [1, "+strconv.Itoa(telemetrystore.MaxFacetTopN)+"]")
			}
		}
		if signal == "traces" {
			return h.aggregateTraces(w, r, p, req, rng, now)
		}
		return h.aggregateLogs(w, r, p, req, rng, now)
	}
}

// facetSpecs는 facet마다 소유 조건을 뺀 filter를 컴파일한다(parameter 접두어는 facet마다 다르다). compile은 signal의 catalog다.
func facetSpecs(signal string, req []aggregateFacet, filter *queryplan.Node, compile func(n *queryplan.Node, prefix string) (queryplan.Compiled, error)) ([]telemetrystore.FacetSpec, error) {
	specs := make([]telemetrystore.FacetSpec, len(req))
	for i, f := range req {
		def, key, _ := facetDefFor(signal, f.Field)
		without, removed := queryplan.Without(filter, def.owned)
		c, err := compile(without, "fa"+strconv.Itoa(i)+"_")
		if err != nil {
			return nil, planError(err)
		}
		top := f.TopN
		if top == 0 {
			top = telemetrystore.DefaultFacetTopN
		}
		specs[i] = telemetrystore.FacetSpec{Field: f.Field, Kind: def.kind, AttributeKey: key, Filter: c, TopN: top, Excludes: removed}
	}
	return specs, nil
}

func (h *Handler) aggregateLogs(w http.ResponseWriter, r *http.Request, p authz.Principal, req aggregateRequest, rng telemetrystore.TimeRange, now time.Time) error {
	compiled, err := queryplan.Compile(req.Filter, queryplan.LogCatalog)
	if err != nil {
		return planError(err)
	}
	// facet filter도 미리 검증한다(서비스 이름은 아직 풀지 않음)
	if _, err := facetSpecs("logs", req.Facets, req.Filter, func(n *queryplan.Node, prefix string) (queryplan.Compiled, error) {
		return queryplan.CompileWith(n, queryplan.LogCatalog, queryplan.Options{ParamPrefix: prefix})
	}); err != nil {
		return err
	}
	resp := aggregateResponse{Meta: newMeta(r)}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.QueryTimeout)
	defer cancel()
	// 검색과 같이 입력 검증 뒤에 tenant 실행 slot 하나를 잡는다. 하위 query는 그 안에서 순서대로 실행한다(ADR 0074 §5).
	release, err := h.gate.acquire(ctx, p.Tenant().String())
	if err != nil {
		return err
	}
	defer release()
	var ids map[string][]string
	recompile := func(resolved map[string][]string) (queryplan.Compiled, error) {
		ids = resolved
		return queryplan.CompileWith(req.Filter, queryplan.LogCatalog, queryplan.Options{ServiceIDs: resolved})
	}
	q := telemetrystore.LogAggregateQuery{Range: rng, Histogram: req.Histogram, ReceivedBefore: now}
	var unresolved bool
	if q.Filter, q.EnvironmentServices, unresolved, err = h.resolveServices(ctx, p, compiled, recompile); err != nil {
		return err
	}
	if unresolved {
		resp.Meta.Warnings = append(resp.Meta.Warnings, WarningServiceNameUnresolved)
	}
	if q.Facets, err = facetSpecs("logs", req.Facets, req.Filter, func(n *queryplan.Node, prefix string) (queryplan.Compiled, error) {
		return queryplan.CompileWith(n, queryplan.LogCatalog, queryplan.Options{ParamPrefix: prefix, ServiceIDs: resolvedOrEmpty(ids)})
	}); err != nil {
		return err
	}
	res, err := h.cfg.Aggregates.AggregateLogs(ctx, p, q, now)
	if err != nil {
		return err
	}
	return h.writeAggregate(w, "logs", resp, res)
}

// resolvedOrEmpty는 facet filter 컴파일의 service.name 풀이다. 전체 filter에 이름이 없었으면(풀지 않음) 빈 풀이다 —
// facet filter는 전체 filter의 부분이라 이름이 새로 생기지 않는다.
func resolvedOrEmpty(ids map[string][]string) map[string][]string {
	if ids == nil {
		return map[string][]string{}
	}
	return ids
}

func (h *Handler) aggregateTraces(w http.ResponseWriter, r *http.Request, p authz.Principal, req aggregateRequest, rng telemetrystore.TimeRange, now time.Time) error {
	// 검색과 같이 나누기 전에 전체 filter를 한 번 검증한다(D02 §15 한도는 요청 전체에)
	if _, err := queryplan.Compile(req.Filter, queryplan.TraceCatalog); err != nil {
		return planError(err)
	}
	spanNode, sumNode, err := queryplan.Split(req.Filter, queryplan.IsTraceSummaryField)
	if err != nil {
		return planError(err)
	}
	spanCompiled, err := queryplan.CompileWith(spanNode, queryplan.TraceSpanCatalog, queryplan.Options{ParamPrefix: "s"})
	if err != nil {
		return planError(err)
	}
	sumCompiled, err := queryplan.CompileWith(sumNode, queryplan.TraceSummaryCatalog, queryplan.Options{ParamPrefix: "t"})
	if err != nil {
		return planError(err)
	}
	// trace facet은 요약 단계 field만 소유한다 — 요약 조건에서만 뺀다(span 단계 matched는 공유)
	facets, err := facetSpecs("traces", req.Facets, sumNode, func(n *queryplan.Node, prefix string) (queryplan.Compiled, error) {
		return queryplan.CompileWith(n, queryplan.TraceSummaryCatalog, queryplan.Options{ParamPrefix: prefix})
	})
	if err != nil {
		return err
	}
	resp := aggregateResponse{Meta: newMeta(r)}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.QueryTimeout)
	defer cancel()
	release, err := h.gate.acquire(ctx, p.Tenant().String())
	if err != nil {
		return err
	}
	defer release()
	recompile := func(ids map[string][]string) (queryplan.Compiled, error) {
		return queryplan.CompileWith(spanNode, queryplan.TraceSpanCatalog, queryplan.Options{ParamPrefix: "s", ServiceIDs: ids})
	}
	q := telemetrystore.TraceAggregateQuery{Range: rng, TraceFilter: sumCompiled, Facets: facets, Histogram: req.Histogram, ReceivedBefore: now}
	var unresolved bool
	if q.SpanFilter, q.EnvironmentServices, unresolved, err = h.resolveServices(ctx, p, spanCompiled, recompile); err != nil {
		return err
	}
	if unresolved {
		resp.Meta.Warnings = append(resp.Meta.Warnings, WarningServiceNameUnresolved)
	}
	res, err := h.cfg.Aggregates.AggregateTraces(ctx, p, q, now)
	if err != nil {
		return err
	}
	// 저장된 trace는 tail sampling을 거친 표본이다(계약 5 — 요청 수·오류율로 쓰지 않는다)
	sampled := true
	resp.Meta.Sampled = &sampled
	return h.writeAggregate(w, "traces", resp, res)
}

// aggregateResponse는 집계 성공 응답이다(공통 envelope, next_cursor 없음).
type aggregateResponse struct {
	Data aggregateData `json:"data"`
	Meta Meta          `json:"meta"`
}

type aggregateData struct {
	Total     uint64         `json:"total"`
	Histogram *histogramJSON `json:"histogram"`
	Facets    []facetJSON    `json:"facets"`
}

type histogramJSON struct {
	StepSeconds int          `json:"step_seconds"`
	Buckets     []bucketJSON `json:"buckets"`
}

// bucketJSON은 칸 하나다. count가 null이면 reason(future)이 있다 — 0으로 채우지 않는다(계약 6).
type bucketJSON struct {
	Start      string            `json:"start"`
	End        string            `json:"end"`
	Count      *uint64           `json:"count"`
	ErrorCount *uint64           `json:"error_count,omitempty"`
	BySeverity map[string]uint64 `json:"by_severity,omitempty"`
	Incomplete bool              `json:"incomplete"`
	Reason     *string           `json:"reason"`
}

type facetJSON struct {
	Field             string           `json:"field"`
	Values            []facetValueJSON `json:"values"`
	OtherCount        *uint64          `json:"other_count"`
	MissingCount      *uint64          `json:"missing_count"`
	Reason            *string          `json:"reason"`
	ExcludesOwnFilter bool             `json:"excludes_own_filter"`
}

type facetValueJSON struct {
	Value string `json:"value"`
	Count uint64 `json:"count"`
	// Context는 같은 문자열의 다른 대상을 가르는 맥락이다(버전의 service_id, pod의 namespace, ADR 0075). 없으면 생략
	Context map[string]string `json:"context,omitempty"`
}

func (h *Handler) writeAggregate(w http.ResponseWriter, signal string, resp aggregateResponse, res telemetrystore.AggregateResult) error {
	resp.Data.Total = res.Total
	resp.Data.Facets = make([]facetJSON, 0, len(res.Facets))
	if res.Histogram != nil {
		step := int(res.Histogram.Step / time.Second)
		resp.Meta.ResolutionSeconds = &step
		hj := &histogramJSON{StepSeconds: step, Buckets: make([]bucketJSON, 0, len(res.Histogram.Buckets))}
		for _, b := range res.Histogram.Buckets {
			bj := bucketJSON{Start: b.Start.UTC().Format(time.RFC3339Nano), End: b.End.UTC().Format(time.RFC3339Nano),
				Count: b.Count, Incomplete: b.Incomplete}
			if b.Reason != "" {
				reason := b.Reason
				bj.Reason = &reason
			}
			if b.Count != nil {
				bj.ErrorCount = b.ErrorCount
				if b.BySeverity != nil {
					bj.BySeverity = make(map[string]uint64, len(b.BySeverity))
					for i, c := range b.BySeverity {
						bj.BySeverity[telemetrystore.SeverityClasses[i]] = c
					}
				}
			}
			hj.Buckets = append(hj.Buckets, bj)
		}
		resp.Data.Histogram = hj
	}
	for _, f := range res.Facets {
		fj := facetJSON{Field: f.Field, ExcludesOwnFilter: f.ExcludesOwnFilter}
		if f.Values == nil {
			reason := f.Reason
			fj.Reason = &reason
			if h.cfg.ObserveFacetUnavailable != nil {
				h.cfg.ObserveFacetUnavailable(signal, f.Reason)
			}
		} else {
			other, missing := f.Other, f.Missing
			fj.OtherCount, fj.MissingCount = &other, &missing
			fj.Values = make([]facetValueJSON, len(f.Values))
			for i, v := range f.Values {
				fj.Values[i] = facetValueJSON{Value: v.Value, Count: v.Count, Context: v.Context}
			}
		}
		resp.Data.Facets = append(resp.Data.Facets, fj)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	return json.NewEncoder(w).Encode(resp)
}
