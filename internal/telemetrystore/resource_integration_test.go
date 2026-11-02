//go:build integration

package telemetrystore

import (
	"context"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/queryplan"
)

type resourceLog struct {
	service  string
	eventID  string
	at       time.Time
	resource ResourceAttrs
}

func insertResourceLogs(t *testing.T, tenant authz.TenantID, exp time.Time, rows ...resourceLog) {
	t.Helper()
	conn := rawConn(t, "MONTRACER_TEST_CH_INGEST_DSN")
	batch, err := conn.PrepareBatch(context.Background(), `INSERT INTO logs_local
		(tenant_id, service_id, event_id, event_time, severity, trace_id, span_id, body, attributes, version, expires_at,
		 service_version, service_instance_id, k8s_namespace_name, k8s_pod_name)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := batch.Append(tenant.String(), r.service, r.eventID, r.at, uint8(9), string(make([]byte, 16)), string(make([]byte, 8)),
			"x", map[string]string{}, versionAt(r.at), exp,
			r.resource.ServiceVersion, r.resource.ServiceInstanceID, r.resource.K8sNamespace, r.resource.K8sPod); err != nil {
			t.Fatal(err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatal(err)
	}
}

// resource 열(ADR 0075): 검색 행, neq가 값 없는 행을 빼는 것, 버전 facet의 서비스별 맥락, pod facet의 namespace 맥락, tenant 경계.
func TestResourceColumnsLogs(t *testing.T) {
	s := openQuery(t)
	tenant, viewer := newTenant(t)
	other, otherViewer := newTenant(t)
	ctx := context.Background()
	now := time.Now().UTC()
	base := now.Add(-time.Hour).Truncate(time.Millisecond)
	exp := base.Add(24 * time.Hour)
	const svcB = "bbbbbbbb-0000-4000-8000-000000000002"
	rows := []resourceLog{
		{svcA, "r:1", base.Add(time.Second), ResourceAttrs{ServiceVersion: "1.2.0", K8sNamespace: "shop", K8sPod: "redis-0"}},
		{svcA, "r:2", base.Add(2 * time.Second), ResourceAttrs{ServiceVersion: "1.2.0", K8sNamespace: "cache", K8sPod: "redis-0"}},
		{svcB, "r:3", base.Add(3 * time.Second), ResourceAttrs{ServiceVersion: "1.2.0"}},
		{svcA, "r:4", base.Add(4 * time.Second), ResourceAttrs{}},
		// pod는 있는데 namespace 속성이 없다: 맥락 key를 생략한다
		{svcA, "r:5", base.Add(5 * time.Second), ResourceAttrs{K8sPod: "lonely-0"}},
	}
	insertResourceLogs(t, tenant, exp, rows...)
	insertResourceLogs(t, other, exp, resourceLog{svcA, "r:9", base.Add(time.Second), ResourceAttrs{ServiceVersion: "9.9.9-secret", K8sPod: "secret-pod"}})
	rng := TimeRange{From: base, To: base.Add(time.Minute)}
	compile := func(f string) queryplan.Compiled {
		t.Helper()
		var n *queryplan.Node
		if f != "" {
			n = parse(t, f)
		}
		c, err := queryplan.CompileWith(n, queryplan.LogCatalog, queryplan.Options{ServiceIDs: map[string][]string{}})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	search := func(p authz.Principal, f string) []LogRecord {
		t.Helper()
		got, _, err := s.SearchLogs(ctx, p, LogQuery{Range: rng, Filter: compile(f), Limit: 100}, now)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	all := search(viewer, "")
	if len(all) != 5 || all[4].Resource != (ResourceAttrs{ServiceVersion: "1.2.0", K8sNamespace: "shop", K8sPod: "redis-0"}) || all[1].Resource != (ResourceAttrs{}) {
		t.Fatalf("rows = %+v", all)
	}
	// neq는 값 없는 행(r:4)을 빼고, exists는 값 있는 행만
	if got := search(viewer, `{"field":"service.version","op":"neq","value":"0.1.0"}`); len(got) != 3 {
		t.Errorf("neq = %d rows, want 3 (값 없는 행 제외)", len(got))
	}
	if got := search(viewer, `{"field":"k8s.pod.name","op":"exists"}`); len(got) != 3 {
		t.Errorf("exists = %d", len(got))
	}
	// 다른 tenant의 값은 보이지 않는다
	if got := search(viewer, `{"field":"service.version","op":"eq","value":"9.9.9-secret"}`); len(got) != 0 {
		t.Errorf("other tenant leaked: %+v", got)
	}
	if got := search(otherViewer, ""); len(got) != 1 || got[0].Resource.K8sPod != "secret-pod" {
		t.Errorf("other tenant own = %+v", got)
	}
	res, err := s.AggregateLogs(ctx, viewer, LogAggregateQuery{Range: rng, Filter: compile(""), ReceivedBefore: now, Facets: []FacetSpec{
		{Field: "service.version", Kind: FacetServiceVersion, Filter: compile(""), TopN: 10},
		{Field: "k8s.pod", Kind: FacetK8sPod, Filter: compile(""), TopN: 10},
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	// 같은 버전 문자열이라도 서비스별로 갈린다(svcA 2, svcB 1), 버전 없음 1
	ver := res.Facets[0]
	if len(ver.Values) != 2 || ver.Values[0].Count != 2 || ver.Values[0].Context[ContextServiceID] != svcA || ver.Values[1].Context[ContextServiceID] != svcB || ver.Missing != 2 {
		t.Errorf("version facet = %+v", ver)
	}
	// 같은 pod 이름이라도 namespace별로 갈린다, pod 없음 2, namespace 없는 pod는 맥락 key 없음
	pod := res.Facets[1]
	if len(pod.Values) != 3 || pod.Missing != 2 || pod.Other != 0 {
		t.Fatalf("pod facet = %+v", pod)
	}
	byKey := map[string]FacetValue{}
	for _, v := range pod.Values {
		ns, ok := v.Context[ContextK8sNamespace]
		if !ok {
			ns = "<none>"
		}
		byKey[v.Value+"|"+ns] = v
	}
	if _, ok := byKey["redis-0|cache"]; !ok {
		t.Errorf("pod facet = %+v", pod.Values)
	}
	if _, ok := byKey["redis-0|shop"]; !ok {
		t.Errorf("pod facet = %+v", pod.Values)
	}
	if v, ok := byKey["lonely-0|<none>"]; !ok || v.Context != nil {
		t.Errorf("pod without namespace = %+v", pod.Values)
	}
}

// trace: span 단계 resource 조건 둘의 and는 같은 span이다. root 버전 facet·조건은 root span 기준이다.
func TestResourceColumnsTraces(t *testing.T) {
	s := openQuery(t)
	ctx := context.Background()
	tenant, viewer := newTenant(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	base := now.Add(-20 * time.Minute)
	exp := now.Add(24 * time.Hour)
	const svcB = "bbbbbbbb-0000-4000-8000-000000000002"
	sp := func(trace, span, parent, svc string, off time.Duration, res ResourceAttrs) searchSpan {
		return searchSpan{tenant: tenant, service: svc, traceID: trace, spanID: span, parentID: parent, name: "op",
			start: base.Add(off), dur: time.Millisecond, version: versionAt(base), expires: exp, resource: res}
	}
	t1, t2 := "1111cccccccccccccccccccccccccccc", "2222cccccccccccccccccccccccccccc"
	insertSearchSpans(t,
		// t1: root svcA 1.2.0, 자식 svcB 2.0.0
		sp(t1, "0000000000000001", "", svcA, 0, ResourceAttrs{ServiceVersion: "1.2.0"}),
		sp(t1, "0000000000000002", "0000000000000001", svcB, time.Millisecond, ResourceAttrs{ServiceVersion: "2.0.0", K8sPod: "pay-0"}),
		// t2: root svcA 버전 없음
		sp(t2, "0000000000000003", "", svcA, time.Minute, ResourceAttrs{}),
	)
	rng := TimeRange{From: base.Add(-time.Minute), To: now}
	run := func(filter string) AggregateResult {
		t.Helper()
		var n *queryplan.Node
		if filter != "" {
			n = parse(t, filter)
		}
		spanF, sumF, err := queryplan.Split(n, queryplan.IsTraceSummaryField)
		if err != nil {
			t.Fatal(err)
		}
		a, err := queryplan.CompileWith(spanF, queryplan.TraceSpanCatalog, queryplan.Options{ParamPrefix: "s", ServiceIDs: map[string][]string{}})
		if err != nil {
			t.Fatal(err)
		}
		b, err := queryplan.CompileWith(sumF, queryplan.TraceSummaryCatalog, queryplan.Options{ParamPrefix: "t"})
		if err != nil {
			t.Fatal(err)
		}
		all, err := queryplan.CompileWith(nil, queryplan.TraceSummaryCatalog, queryplan.Options{ParamPrefix: "fa0_"})
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.AggregateTraces(ctx, viewer, TraceAggregateQuery{Range: rng, SpanFilter: a, TraceFilter: b, ReceivedBefore: now,
			Facets: []FacetSpec{{Field: "root_service_version", Kind: FacetRootServiceVersion, Filter: all, TopN: 10}}}, now)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	all := run("")
	if f := all.Facets[0]; all.Total != 2 || len(f.Values) != 1 || f.Values[0].Value != "1.2.0" || f.Values[0].Context[ContextServiceID] != svcA || f.Missing != 1 {
		t.Fatalf("root version facet = %+v", all)
	}
	// 같은 span: svcB의 2.0.0 span이 있는 trace는 t1, svcA의 2.0.0 span은 없다
	if r := run(`{"op":"and","args":[{"field":"service_id","op":"eq","value":"` + svcB + `"},{"field":"service.version","op":"eq","value":"2.0.0"}]}`); r.Total != 1 {
		t.Errorf("same span B/2.0.0 = %d", r.Total)
	}
	if r := run(`{"op":"and","args":[{"field":"service_id","op":"eq","value":"` + svcA + `"},{"field":"service.version","op":"eq","value":"2.0.0"}]}`); r.Total != 0 {
		t.Errorf("same span A/2.0.0 = %d", r.Total)
	}
	if r := run(`{"field":"root_service_version","op":"eq","value":"1.2.0"}`); r.Total != 1 {
		t.Errorf("root version filter = %d", r.Total)
	}
	if r := run(`{"field":"k8s.pod.name","op":"eq","value":"pay-0"}`); r.Total != 1 {
		t.Errorf("pod filter = %d", r.Total)
	}
	// 검색 행의 root 버전
	spanAll, _ := queryplan.CompileWith(nil, queryplan.TraceSpanCatalog, queryplan.Options{ParamPrefix: "s"})
	got, _, err := s.SearchTraces(ctx, viewer, TraceSearchQuery{Range: rng, SpanFilter: spanAll, TraceFilter: spanAll, Limit: 10}, now)
	if err != nil || len(got) != 2 {
		t.Fatalf("search = %v %v", got, err)
	}
	versions := map[string]string{}
	for _, x := range got {
		versions[x.TraceID[:4]] = x.RootServiceVersion
	}
	if versions["1111"] != "1.2.0" || versions["2222"] != "" {
		t.Errorf("root versions = %v", versions)
	}
}
