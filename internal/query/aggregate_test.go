package query

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// fakeAggregates는 받은 조건을 기록하고 정해진 결과를 돌려준다(집계 SQL은 telemetrystore 통합 시험이 본다).
type fakeAggregates struct {
	logs   []telemetrystore.LogAggregateQuery
	traces []telemetrystore.TraceAggregateQuery
	result telemetrystore.AggregateResult
	err    error
}

func (f *fakeAggregates) AggregateLogs(_ context.Context, p authz.Principal, q telemetrystore.LogAggregateQuery, _ time.Time) (telemetrystore.AggregateResult, error) {
	f.logs = append(f.logs, q)
	if p.EnvironmentRestricted() && q.EnvironmentServices == nil {
		return telemetrystore.AggregateResult{}, telemetrystore.ErrEnvironmentScoped
	}
	return f.result, f.err
}

func (f *fakeAggregates) AggregateTraces(_ context.Context, _ authz.Principal, q telemetrystore.TraceAggregateQuery, _ time.Time) (telemetrystore.AggregateResult, error) {
	f.traces = append(f.traces, q)
	return f.result, f.err
}

type facetObs struct{ signal, reason string }

func aggregateHandler(t *testing.T, k *keys, agg AggregateStore, services ServiceStore, obs *[]facetObs) *Handler {
	t.Helper()
	cfg := Config{Authenticate: k.authenticate, Store: &fakeStore{}, Services: services,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	if agg != nil {
		cfg.Aggregates = agg
	}
	if obs != nil {
		cfg.ObserveFacetUnavailable = func(signal, reason string) { *obs = append(*obs, facetObs{signal, reason}) }
	}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func u64(v uint64) *uint64 { return &v }

func sampleAggregate() telemetrystore.AggregateResult {
	t0 := now.Add(-time.Minute)
	return telemetrystore.AggregateResult{
		Total: 7,
		Histogram: &telemetrystore.Histogram{Step: 30 * time.Second, Buckets: []telemetrystore.HistogramBucket{
			{Start: t0, End: t0.Add(30 * time.Second), Count: u64(7), ErrorCount: u64(2), BySeverity: []uint64{0, 0, 0, 5, 0, 2, 0}},
			{Start: t0.Add(30 * time.Second), End: now, Count: u64(0), ErrorCount: u64(0), BySeverity: make([]uint64, 7), Incomplete: true},
			{Start: now, End: now.Add(30 * time.Second), Reason: "future"},
		}},
		Facets: []telemetrystore.FacetResult{
			{Field: "service", Values: []telemetrystore.FacetValue{{Value: "aaaaaaaa-0000-4000-8000-000000000001", Count: 6}}, Other: 1, ExcludesOwnFilter: true},
			{Field: "attributes.user.id", Reason: telemetrystore.FacetTooManyValues},
		},
	}
}

type aggregateBody struct {
	Data struct {
		Total     uint64 `json:"total"`
		Histogram *struct {
			StepSeconds int              `json:"step_seconds"`
			Buckets     []map[string]any `json:"buckets"`
		} `json:"histogram"`
		Facets []map[string]any `json:"facets"`
	} `json:"data"`
	Meta map[string]any `json:"meta"`
}

func decodeAggregate(t *testing.T, body []byte) aggregateBody {
	t.Helper()
	var b aggregateBody
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return b
}

// 성공 응답(OpenAPI 계약): 미래 칸은 count null + reason, 셀 수 없는 facet은 values·합 null + reason(0으로 채우지 않음, 계약 6).
func TestAggregateLogsResponse(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	agg := &fakeAggregates{result: sampleAggregate()}
	var obs []facetObs
	h := aggregateHandler(t, k, agg, &fakeServices{byName: map[string][]string{"checkout": {"aaaaaaaa-0000-4000-8000-000000000001"}}}, &obs)
	rec := post(h, "/api/v1/query/logs/aggregate", tok, `{"filter":{"op":"and","args":[
		{"field":"service.name","op":"in","value":["checkout"]},{"field":"severity_number","op":"gte","value":17}]},
		"facets":[{"field":"service"},{"field":"severity","top_n":7},{"field":"attributes.user.id"}],"histogram":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	b := decodeAggregate(t, rec.Body.Bytes())
	if b.Data.Total != 7 || b.Data.Histogram.StepSeconds != 30 || b.Meta["resolution_seconds"] != float64(30) || b.Meta["sampled"] != nil {
		t.Fatalf("body = %+v", b)
	}
	if fut := b.Data.Histogram.Buckets[2]; fut["count"] != nil || fut["reason"] != "future" || fut["by_severity"] != nil {
		t.Errorf("future bucket = %v", fut)
	}
	if sev := b.Data.Histogram.Buckets[0]["by_severity"].(map[string]any); sev["info"] != float64(5) || sev["error"] != float64(2) {
		t.Errorf("by_severity = %v", sev)
	}
	if f := b.Data.Facets[1]; f["values"] != nil || f["other_count"] != nil || f["reason"] != "too_many_values" {
		t.Errorf("unavailable facet = %v", f)
	}
	if len(obs) != 1 || obs[0] != (facetObs{"logs", "too_many_values"}) {
		t.Errorf("observed = %v", obs)
	}
	// 전체 filter는 서비스 이름이 풀린 것, facet filter는 소유 조건만 뺀 것이다
	q := agg.logs[0]
	if q.Filter.Unresolved || !strings.Contains(q.Filter.SQL, "service_id IN") || !strings.Contains(q.Filter.SQL, "severity >=") {
		t.Errorf("filter = %s", q.Filter.SQL)
	}
	svc, sev, attr := q.Facets[0], q.Facets[1], q.Facets[2]
	if !svc.Excludes || strings.Contains(svc.Filter.SQL, "service_id") || !strings.Contains(svc.Filter.SQL, "severity >=") || svc.TopN != telemetrystore.DefaultFacetTopN {
		t.Errorf("service facet = %+v", svc)
	}
	if !sev.Excludes || strings.Contains(sev.Filter.SQL, "severity") || !strings.Contains(sev.Filter.SQL, "service_id IN") || sev.TopN != 7 || sev.Filter.Unresolved {
		t.Errorf("severity facet = %+v", sev)
	}
	if attr.Excludes || attr.AttributeKey != "user.id" || attr.Kind != telemetrystore.FacetAttribute ||
		!strings.Contains(attr.Filter.SQL, "service_id IN") || !strings.Contains(attr.Filter.SQL, "severity >=") {
		t.Errorf("attribute facet = %+v", attr)
	}
	if !q.Histogram || !q.ReceivedBefore.Equal(now) {
		t.Errorf("query = %+v", q)
	}
}

// trace 집계: 저장된 trace는 표본이다(meta.sampled=true). root 서비스 facet은 요약 단계 root_service_id만 빼고,
// span 단계 service.name은 공유한다.
func TestAggregateTracesResponse(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	res := sampleAggregate()
	res.Facets = []telemetrystore.FacetResult{{Field: "root_service_id", Values: []telemetrystore.FacetValue{}, Missing: 3, ExcludesOwnFilter: true}}
	agg := &fakeAggregates{result: res}
	h := aggregateHandler(t, k, agg, &fakeServices{byName: map[string][]string{}}, nil)
	rec := post(h, "/api/v1/query/traces/aggregate", tok, `{"filter":{"op":"and","args":[
		{"field":"service.name","op":"eq","value":"nope"},
		{"field":"root_service_id","op":"in","value":["aaaaaaaa-0000-4000-8000-000000000001"]},
		{"field":"duration_ms","op":"gte","value":500}]},
		"facets":[{"field":"root_service_id"},{"field":"has_error"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	b := decodeAggregate(t, rec.Body.Bytes())
	if b.Meta["sampled"] != true || b.Data.Facets[0]["missing_count"] != float64(3) {
		t.Errorf("body = %+v", b)
	}
	// 풀지 못한 service.name은 경고로 알린다(0이 "그런 서비스 없음"인지 구별)
	if w := b.Meta["warnings"].([]any); len(w) != 1 || w[0] != WarningServiceNameUnresolved {
		t.Errorf("warnings = %v", w)
	}
	q := agg.traces[0]
	if !strings.Contains(q.SpanFilter.SQL, "service_id IN") || !strings.Contains(q.TraceFilter.SQL, "root_service_id") {
		t.Errorf("filters = %s | %s", q.SpanFilter.SQL, q.TraceFilter.SQL)
	}
	root, errFacet := q.Facets[0], q.Facets[1]
	if !root.Excludes || strings.Contains(root.Filter.SQL, "root_service_id") || !strings.Contains(root.Filter.SQL, "duration_ms") {
		t.Errorf("root facet = %+v", root)
	}
	if errFacet.Excludes || !strings.Contains(errFacet.Filter.SQL, "root_service_id") {
		t.Errorf("has_error facet = %+v", errFacet)
	}
	if b.Data.Histogram == nil {
		t.Error("histogram from store must be passed through")
	}
}

func TestAggregateRejects(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	agg := &fakeAggregates{result: telemetrystore.AggregateResult{}}
	h := aggregateHandler(t, k, agg, &fakeServices{}, nil)
	for name, c := range map[string]struct{ path, body string }{
		"projection":          {"/api/v1/query/logs/aggregate", `{"projection":["body"]}`},
		"cursor":              {"/api/v1/query/logs/aggregate", `{"cursor":"x"}`},
		"unknown facet":       {"/api/v1/query/logs/aggregate", `{"facets":[{"field":"body"}]}`},
		"trace facet on logs": {"/api/v1/query/logs/aggregate", `{"facets":[{"field":"has_error"}]}`},
		"attribute on traces": {"/api/v1/query/traces/aggregate", `{"facets":[{"field":"attributes.x"}]}`},
		"bad attribute key":   {"/api/v1/query/logs/aggregate", `{"facets":[{"field":"attributes.a b"}]}`},
		"duplicate":           {"/api/v1/query/logs/aggregate", `{"facets":[{"field":"service"},{"field":"service"}]}`},
		"top_n":               {"/api/v1/query/logs/aggregate", `{"facets":[{"field":"service","top_n":21}]}`},
		"five facets": {"/api/v1/query/logs/aggregate", `{"facets":[{"field":"service"},{"field":"severity"},` +
			`{"field":"attributes.a"},{"field":"attributes.b"},{"field":"attributes.c"}]}`},
		"three attributes": {"/api/v1/query/logs/aggregate", `{"facets":[{"field":"attributes.a"},{"field":"attributes.b"},{"field":"attributes.c"}]}`},
		"signal mismatch":  {"/api/v1/query/logs/aggregate", `{"signal":"traces"}`},
		"bad filter":       {"/api/v1/query/traces/aggregate", `{"filter":{"field":"nope","op":"eq","value":1}}`},
	} {
		if rec := post(h, c.path, tok, c.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(agg.logs)+len(agg.traces) != 0 {
		t.Error("rejected requests must not reach the store")
	}
}

// flag off(Aggregates nil)는 404다(ADR 0074 Rollback).
func TestAggregateDisabled(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	h := aggregateHandler(t, k, nil, nil, nil)
	for _, p := range []string{"/api/v1/query/logs/aggregate", "/api/v1/query/traces/aggregate"} {
		if rec := post(h, p, tok, `{}`); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}

// environment로 제한된 key는 catalog의 서비스 범위로만 센다(mandatory predicate, AST 밖이라 facet 조건 빼기로 지워지지 않는다).
func TestAggregateEnvironmentScopedKey(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, []string{"prod"})
	agg := &fakeAggregates{result: telemetrystore.AggregateResult{}}
	h := aggregateHandler(t, k, agg, &fakeServices{envIDs: map[string][]string{"prod": {"aaaaaaaa-0000-4000-8000-000000000001"}}}, nil)
	if rec := post(h, "/api/v1/query/logs/aggregate", tok, `{"facets":[{"field":"service"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := agg.logs[0].EnvironmentServices; len(got) != 1 {
		t.Errorf("scope = %v", got)
	}
	// catalog가 없으면 제한된 key는 403(저장소가 범위 없이 실행하지 않는다)
	h = aggregateHandler(t, k, agg, nil, nil)
	if rec := post(h, "/api/v1/query/logs/aggregate", tok, `{}`); rec.Code != http.StatusForbidden {
		t.Errorf("no catalog: %d %s", rec.Code, rec.Body)
	}
}

type aggBudgetErr struct{}

func (aggBudgetErr) Error() string                  { return "budget" }
func (aggBudgetErr) BudgetExceeded() map[string]any { return map[string]any{} }

type aggTimeoutErr struct{}

func (aggTimeoutErr) Error() string      { return "timeout" }
func (aggTimeoutErr) QueryTimeout() bool { return true }

// 본 query의 예산·시간 초과는 요청 전체의 422·504다(건수를 0으로 주지 않는다, ADR 0074 §5).
func TestAggregateStoreErrors(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	for want, err := range map[int]error{http.StatusUnprocessableEntity: aggBudgetErr{}, http.StatusGatewayTimeout: aggTimeoutErr{}} {
		h := aggregateHandler(t, k, &fakeAggregates{err: err}, &fakeServices{}, nil)
		for _, p := range []string{"/api/v1/query/logs/aggregate", "/api/v1/query/traces/aggregate"} {
			if rec := post(h, p, tok, `{"histogram":true}`); rec.Code != want || strings.Contains(rec.Body.String(), `"total"`) {
				t.Errorf("%s %v: %d %s", p, err, rec.Code, rec.Body)
			}
		}
	}
}

// resource facet(ADR 0075): 버전은 서비스별 맥락, pod는 namespace 맥락으로 그대로 전달하고, pod facet은 namespace 조건까지 소유한다.
// 따로 세는 facet(속성·pod)은 합쳐 요청당 2개다.
func TestAggregateResourceFacets(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	res := telemetrystore.AggregateResult{Total: 3, Facets: []telemetrystore.FacetResult{
		{Field: "service.version", Values: []telemetrystore.FacetValue{{Value: "1.2.0", Count: 2, Context: map[string]string{"service_id": "aaaaaaaa-0000-4000-8000-000000000001"}}}, Missing: 1},
		{Field: "k8s.pod", Values: []telemetrystore.FacetValue{{Value: "redis-0", Count: 1, Context: map[string]string{"k8s.namespace.name": "shop"}}}, ExcludesOwnFilter: true},
	}}
	agg := &fakeAggregates{result: res}
	h := aggregateHandler(t, k, agg, &fakeServices{}, nil)
	rec := post(h, "/api/v1/query/logs/aggregate", tok, `{"filter":{"op":"and","args":[
		{"field":"k8s.namespace.name","op":"eq","value":"shop"},{"field":"k8s.pod.name","op":"eq","value":"redis-0"},
		{"field":"service.version","op":"neq","value":"0.9.0"}]},"facets":[{"field":"service.version"},{"field":"k8s.pod"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"context":{"service_id":"aaaaaaaa-0000-4000-8000-000000000001"}`) ||
		!strings.Contains(rec.Body.String(), `"context":{"k8s.namespace.name":"shop"}`) {
		t.Errorf("context missing: %s", rec.Body)
	}
	q := agg.logs[0]
	ver, pod := q.Facets[0], q.Facets[1]
	if ver.Kind != telemetrystore.FacetServiceVersion || !ver.Excludes || strings.Contains(ver.Filter.SQL, "service_version") ||
		!strings.Contains(ver.Filter.SQL, "k8s_pod_name") {
		t.Errorf("version facet = %+v", ver)
	}
	if pod.Kind != telemetrystore.FacetK8sPod || strings.Contains(pod.Filter.SQL, "k8s_pod_name") || strings.Contains(pod.Filter.SQL, "k8s_namespace_name") ||
		!strings.Contains(pod.Filter.SQL, "service_version") {
		t.Errorf("pod facet = %+v", pod)
	}
	if rec := post(h, "/api/v1/query/logs/aggregate", tok, `{"facets":[{"field":"k8s.pod"},{"field":"attributes.a"},{"field":"attributes.b"}]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("three separate facets: %d", rec.Code)
	}
	if rec := post(h, "/api/v1/query/traces/aggregate", tok, `{"facets":[{"field":"k8s.pod"}]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("pod facet on traces: %d", rec.Code)
	}
	if rec := post(h, "/api/v1/query/traces/aggregate", tok, `{"filter":{"field":"root_service_version","op":"eq","value":"1.2.0"},"facets":[{"field":"root_service_version"}]}`); rec.Code != http.StatusOK ||
		agg.traces[0].Facets[0].Kind != telemetrystore.FacetRootServiceVersion || !agg.traces[0].Facets[0].Excludes {
		t.Errorf("root version facet: %d %+v", rec.Code, agg.traces)
	}
}
