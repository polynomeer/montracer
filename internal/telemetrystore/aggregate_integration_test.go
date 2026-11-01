//go:build integration

package telemetrystore

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/queryplan"
)

func facetMap(f FacetResult) map[string]uint64 {
	out := map[string]uint64{}
	for _, v := range f.Values {
		out[v.Value] = v.Count
	}
	return out
}

func sumBuckets(h *Histogram) (count, errs uint64) {
	for _, b := range h.Buckets {
		if b.Count != nil {
			count += *b.Count
		}
		if b.ErrorCount != nil {
			errs += *b.ErrorCount
		}
	}
	return count, errs
}

// log 집계(ADR 0074): dedup된 수, 만료·다른 tenant 제외, facet의 소유 조건 빼기, 속성 facet의 missing·other, 히스토그램 합 = total,
// 검색 page 수와 같음, group 상한 초과는 그 facet만 too_many_values, environment 범위.
func TestAggregateLogs(t *testing.T) {
	s := openQuery(t)
	tenantA, viewerA := newTenant(t)
	tenantB, viewerB := newTenant(t)
	ctx := context.Background()
	now := time.Now().UTC()
	base := now.Add(-time.Hour).Truncate(time.Millisecond)
	recv := base.Add(time.Second)
	exp := base.Add(24 * time.Hour)
	insertLogs(t,
		logRow{tenantA, "uid:1", base.Add(1 * time.Minute), 9, "", "order received", map[string]string{"http.route": "/checkout"}, versionAt(recv), exp},
		logRow{tenantA, "uid:2", base.Add(2 * time.Minute), 17, "", "charge failed", map[string]string{"http.route": "/charge"}, versionAt(recv), exp},
		logRow{tenantA, "uid:3", base.Add(3 * time.Minute), 21, "", "db down", nil, versionAt(recv), exp},
		// 재전송(나중 수신): 한 번만 센다
		logRow{tenantA, "uid:2", base.Add(2 * time.Minute), 17, "", "charge failed (resent)", map[string]string{"http.route": "/resent"}, versionAt(recv.Add(time.Minute)), exp},
		// 만료
		logRow{tenantA, "uid:4", base.Add(4 * time.Minute), 17, "", "expired", nil, versionAt(recv), base.Add(-time.Minute)},
		logRow{tenantB, "uid:9", base.Add(1 * time.Minute), 17, "", "tenant-b", nil, versionAt(recv), exp},
	)
	rng := TimeRange{From: base, To: base.Add(15 * time.Minute)}
	run := func(p authz.Principal, filter string, envs []string) AggregateResult {
		t.Helper()
		var n *queryplan.Node
		if filter != "" {
			n = parse(t, filter)
		}
		full, err := queryplan.CompileWith(n, queryplan.LogCatalog, queryplan.Options{ServiceIDs: map[string][]string{}})
		if err != nil {
			t.Fatal(err)
		}
		facet := func(i int, name string, kind FacetKind, key string, owned ...string) FacetSpec {
			w, removed := queryplan.Without(n, func(f string) bool {
				for _, o := range owned {
					if o == f {
						return true
					}
				}
				return false
			})
			c, err := queryplan.CompileWith(w, queryplan.LogCatalog, queryplan.Options{ParamPrefix: "fa" + strconv.Itoa(i) + "_", ServiceIDs: map[string][]string{}})
			if err != nil {
				t.Fatal(err)
			}
			return FacetSpec{Field: name, Kind: kind, AttributeKey: key, Filter: c, TopN: 10, Excludes: removed}
		}
		res, err := s.AggregateLogs(ctx, p, LogAggregateQuery{Range: rng, Filter: full, Histogram: true, ReceivedBefore: now, EnvironmentServices: envs,
			Facets: []FacetSpec{
				facet(0, "service", FacetService, "", "service_id", "service.name"),
				facet(1, "severity", FacetSeverity, "", "severity_number"),
				facet(2, "attributes.http.route", FacetAttribute, "http.route", "attributes.http.route"),
			}}, now)
		if err != nil {
			t.Fatalf("aggregate %s: %v", filter, err)
		}
		return res
	}

	all := run(viewerA, "", nil)
	if all.Total != 3 {
		t.Fatalf("total = %d (dedup·만료·tenant 제외 후 3)", all.Total)
	}
	if got := facetMap(all.Facets[0]); got[svcA] != 3 || len(got) != 1 {
		t.Errorf("service facet = %v", got)
	}
	if got := facetMap(all.Facets[1]); got["info"] != 1 || got["error"] != 1 || got["fatal"] != 1 {
		t.Errorf("severity facet = %v", got)
	}
	// 재전송은 최초 수신 값(/charge)으로 센다. 속성 없음은 missing
	if f := all.Facets[2]; facetMap(f)["/charge"] != 1 || facetMap(f)["/checkout"] != 1 || f.Missing != 1 || f.Other != 0 || f.Values == nil {
		t.Errorf("route facet = %+v", f)
	}
	// 범위 시작이 30초 배수가 아니라 칸이 31개(첫·끝 칸은 범위로 잘림)
	if c, _ := sumBuckets(all.Histogram); c != all.Total || all.Histogram.Step != 30*time.Second || len(all.Histogram.Buckets) != 31 {
		t.Errorf("histogram sum = %d step %v buckets %d", c, all.Histogram.Step, len(all.Histogram.Buckets))
	}
	if b := all.Histogram.Buckets[2]; b.BySeverity == nil || b.BySeverity[3] != 1 {
		t.Errorf("minute-1 bucket = %+v (info 1)", b)
	}

	// 심각도 ≥ 17: total 2. 심각도 facet은 자기 조건을 빼고 세고(3), 서비스·속성 facet은 그대로 건다
	errs := run(viewerA, `{"field":"severity_number","op":"gte","value":17}`, nil)
	if errs.Total != 2 || !errs.Facets[1].ExcludesOwnFilter || facetMap(errs.Facets[1])["info"] != 1 || facetMap(errs.Facets[0])[svcA] != 2 {
		t.Errorf("severity filter = %+v", errs)
	}
	if f := errs.Facets[2]; facetMap(f)["/charge"] != 1 || f.Missing != 1 || f.ExcludesOwnFilter {
		t.Errorf("route facet under severity filter = %+v", f)
	}
	// 검색 page를 끝까지 넘긴 수와 같다
	full, err := queryplan.CompileWith(parse(t, `{"field":"severity_number","op":"gte","value":17}`), queryplan.LogCatalog, queryplan.Options{ServiceIDs: map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := s.SearchLogs(ctx, viewerA, LogQuery{Range: rng, Filter: full, Limit: 1000, ReceivedBefore: now}, now)
	if err != nil || uint64(len(rows)) != errs.Total {
		t.Errorf("search rows = %d, aggregate total = %d (%v)", len(rows), errs.Total, err)
	}
	// 다른 tenant
	if b := run(viewerB, "", nil); b.Total != 1 {
		t.Errorf("tenant B total = %d", b.Total)
	}
	// environment 제한 key: 범위가 없으면 403, 범위에 없는 서비스는 세지 않는다
	prodKey := keyPrincipal(t, tenantA, []string{"prod"})
	if _, err := s.AggregateLogs(ctx, prodKey, LogAggregateQuery{Range: rng, Filter: full}, now); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("scoped without services = %v", err)
	}
	// 범위 밖: 모든 하위 query(속성 facet 포함)가 environment predicate를 건다
	if r := run(prodKey, "", []string{"bbbbbbbb-0000-4000-8000-000000000002"}); r.Total != 0 || len(r.Facets[0].Values) != 0 ||
		r.Facets[2].Values == nil || len(r.Facets[2].Values) != 0 || r.Facets[2].Missing != 0 || r.Facets[2].Other != 0 {
		t.Errorf("out-of-scope = %+v", r)
	}

	// group 상한 초과: 그 facet만 too_many_values, 나머지는 그대로(근사하지 않는다)
	prev := facetGroupLimit
	facetGroupLimit = 2
	t.Cleanup(func() { facetGroupLimit = prev })
	over := run(viewerA, "", nil)
	if f := over.Facets[2]; f.Values != nil || f.Reason != FacetTooManyValues {
		t.Errorf("overflow facet = %+v", f)
	}
	if over.Total != 3 || facetMap(over.Facets[0])[svcA] != 3 {
		t.Errorf("other results must stay: %+v", over)
	}
}

// trace 집계(ADR 0074): 검색과 같은 요약을 거친 trace 수, root 서비스 facet(root 없음은 missing), 오류 facet,
// 요약 조건(root_service_id·duration_ms)이 수에 들어감, 소유 조건 빼기, 오류 히스토그램, tenant·environment 범위.
func TestAggregateTraces(t *testing.T) {
	s := openQuery(t)
	ctx := context.Background()
	tenantA, viewerA := newTenant(t)
	tenantB, viewerB := newTenant(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	exp := now.Add(7 * 24 * time.Hour)
	base := now.Add(-30 * time.Minute)
	const (
		svcB = "bbbbbbbb-0000-4000-8000-000000000002"
		t1   = "1111bbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		t2   = "2222bbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		t3   = "3333bbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	sp := func(tenant authz.TenantID, trace, span, parent, svc string, off, dur time.Duration, status uint8) searchSpan {
		// version은 pipeline과 같이 수신 시각에서 만든다 — 집계는 수신 snapshot(요청 시각)의 version 하한을 건다
		return searchSpan{tenant: tenant, service: svc, traceID: trace, spanID: span, parentID: parent, name: "op",
			start: base.Add(off), dur: dur, status: status, version: versionAt(base), expires: exp}
	}
	insertSearchSpans(t,
		sp(tenantA, t1, "0000000000000001", "", svcA, 0, 840*time.Millisecond, 1),
		sp(tenantA, t1, "0000000000000002", "0000000000000001", svcB, 60*time.Millisecond, 500*time.Millisecond, 2),
		sp(tenantA, t2, "0000000000000003", "", svcA, time.Minute, 30*time.Millisecond, 0),
		sp(tenantA, t3, "0000000000000004", "00000000000000ff", svcB, 2*time.Minute, 10*time.Millisecond, 0),
		sp(tenantB, t1, "0000000000000009", "", svcB, 0, time.Second, 2),
	)
	rng := TimeRange{From: base.Add(-5 * time.Minute), To: now}
	run := func(p authz.Principal, filter string, envs []string) AggregateResult {
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
		facet := func(i int, name string, kind FacetKind) FacetSpec {
			w, removed := queryplan.Without(sumF, func(f string) bool { return f == name })
			c, err := queryplan.CompileWith(w, queryplan.TraceSummaryCatalog, queryplan.Options{ParamPrefix: "fa" + strconv.Itoa(i) + "_"})
			if err != nil {
				t.Fatal(err)
			}
			return FacetSpec{Field: name, Kind: kind, Filter: c, TopN: 10, Excludes: removed}
		}
		res, err := s.AggregateTraces(ctx, p, TraceAggregateQuery{Range: rng, SpanFilter: a, TraceFilter: b, Histogram: true, ReceivedBefore: now,
			EnvironmentServices: envs, Facets: []FacetSpec{facet(0, "root_service_id", FacetRootService), facet(1, "has_error", FacetHasError)}}, now)
		if err != nil {
			t.Fatalf("aggregate %s: %v", filter, err)
		}
		return res
	}
	all := run(viewerA, "", nil)
	if all.Total != 3 {
		t.Fatalf("total = %d", all.Total)
	}
	if f := all.Facets[0]; facetMap(f)[svcA] != 2 || f.Missing != 1 {
		t.Errorf("root facet = %+v (root 없는 t3은 missing)", f)
	}
	if got := facetMap(all.Facets[1]); got["true"] != 1 || got["false"] != 2 {
		t.Errorf("has_error facet = %v", got)
	}
	if c, e := sumBuckets(all.Histogram); c != 3 || e != 1 {
		t.Errorf("histogram = %d/%d", c, e)
	}
	// root 서비스 조건: total 2, root facet은 자기 조건을 빼고(svcA 2·missing 1), 오류 facet은 건다
	root := run(viewerA, `{"field":"root_service_id","op":"in","value":["`+svcA+`"]}`, nil)
	if root.Total != 2 || !root.Facets[0].ExcludesOwnFilter || root.Facets[0].Missing != 1 || facetMap(root.Facets[1])["false"] != 1 {
		t.Errorf("root filter = %+v", root)
	}
	// 요약 조건 duration_ms와 span 조건
	if r := run(viewerA, `{"field":"duration_ms","op":"gte","value":500}`, nil); r.Total != 1 {
		t.Errorf("duration total = %d", r.Total)
	}
	if r := run(viewerA, `{"field":"service_id","op":"eq","value":"`+svcB+`"}`, nil); r.Total != 2 {
		t.Errorf("span service total = %d (svcB span이 있는 t1·t3)", r.Total)
	}
	// 검색 결과를 끝까지 센 수와 같다
	spanAll, err := queryplan.CompileWith(nil, queryplan.TraceSpanCatalog, queryplan.Options{ParamPrefix: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.SearchTraces(ctx, viewerA, TraceSearchQuery{Range: rng, SpanFilter: spanAll, TraceFilter: spanAll, Limit: 1000, ReceivedBefore: now}, now); err != nil || uint64(len(got)) != all.Total {
		t.Errorf("search = %d, aggregate = %d (%v)", len(got), all.Total, err)
	}
	// root 없는 trace는 root_service_id 조건(모두 0인 UUID 포함)과 맞지 않는다
	if r := run(viewerA, `{"field":"root_service_id","op":"eq","value":"00000000-0000-0000-0000-000000000000"}`, nil); r.Total != 0 {
		t.Errorf("zero uuid matched rootless traces: %d", r.Total)
	}
	if b := run(viewerB, "", nil); b.Total != 1 || facetMap(b.Facets[1])["true"] != 1 {
		t.Errorf("tenant B = %+v", b)
	}
	prodKey := keyPrincipal(t, tenantA, []string{"prod"})
	if r := run(prodKey, "", []string{svcA}); r.Total != 2 || facetMap(r.Facets[1])["true"] != 0 {
		t.Errorf("svcA scope = %+v (svcB 오류 span이 보이면 안 된다)", r)
	}
}

// 심각도 묶음 경계(OTel SeverityNumber): 0·25 이상 unspecified, 1–4 trace, 4/5, 12/13, 20/21, 24.
func TestAggregateLogSeverityClasses(t *testing.T) {
	s := openQuery(t)
	tenant, viewer := newTenant(t)
	ctx := context.Background()
	now := time.Now().UTC()
	base := now.Add(-time.Hour).Truncate(time.Millisecond)
	exp := base.Add(24 * time.Hour)
	var rows []logRow
	for i, sev := range []uint8{0, 1, 4, 5, 8, 9, 12, 13, 16, 17, 20, 21, 24, 30} {
		rows = append(rows, logRow{tenant, "sev:" + string(rune('a'+i)), base.Add(time.Duration(i) * time.Second), sev, "", "x", nil, versionAt(base), exp})
	}
	insertLogs(t, rows...)
	full, err := queryplan.CompileWith(nil, queryplan.LogCatalog, queryplan.Options{ServiceIDs: map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.AggregateLogs(ctx, viewer, LogAggregateQuery{Range: TimeRange{From: base, To: base.Add(time.Minute)}, Filter: full, Histogram: true,
		ReceivedBefore: now, Facets: []FacetSpec{{Field: "severity", Kind: FacetSeverity, Filter: full, TopN: 7}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint64{"unspecified": 2, "trace": 2, "debug": 2, "info": 2, "warn": 2, "error": 2, "fatal": 2}
	got := facetMap(res.Facets[0])
	for k, v := range want {
		if got[k] != v {
			t.Errorf("severity facet = %v, want %v", got, want)
			break
		}
	}
	// 히스토그램의 묶음별 수 합도 같다(UInt32 key로 scan된다)
	var bySev [7]uint64
	for _, b := range res.Histogram.Buckets {
		for i, c := range b.BySeverity {
			bySev[i] += c
		}
	}
	if bySev != [7]uint64{2, 2, 2, 2, 2, 2, 2} {
		t.Errorf("histogram by severity = %v", bySev)
	}
}
