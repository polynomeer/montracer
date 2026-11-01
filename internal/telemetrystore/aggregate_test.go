package telemetrystore

import (
	"context"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/queryplan"
)

var aggT0 = time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)

// 간격은 범위를 60칸 이하로 나누는 가장 짧은 후보다(ADR 0074 §4).
func TestHistogramStep(t *testing.T) {
	for d, want := range map[time.Duration]time.Duration{
		5 * time.Minute:  10 * time.Second,
		15 * time.Minute: 30 * time.Second,
		time.Hour:        time.Minute,
		6 * time.Hour:    10 * time.Minute,
		24 * time.Hour:   30 * time.Minute,
	} {
		if got := HistogramStep(TimeRange{From: aggT0, To: aggT0.Add(d)}); got != want {
			t.Errorf("step(%v) = %v, want %v", d, got, want)
		}
	}
}

// 배수가 아닌 범위는 61칸까지 나오고 경계 칸은 범위로 잘린다. 빈 지난 칸은 0, snapshot 이후는 null(future),
// snapshot 직전 60초에 걸친 칸은 incomplete다.
func TestBuildHistogram(t *testing.T) {
	r := TimeRange{From: aggT0.Add(30 * time.Second), To: aggT0.Add(time.Hour + 30*time.Second)}
	step := HistogramStep(r)
	snapshot := aggT0.Add(time.Hour + 30*time.Second)
	h := buildHistogram(r, step, snapshot, map[uint32]uint64{0: 3, 60: 1}, map[uint32]uint64{0: 1}, nil)
	if step != time.Minute || len(h.Buckets) != 61 {
		t.Fatalf("buckets = %d step %v", len(h.Buckets), step)
	}
	first, last := h.Buckets[0], h.Buckets[60]
	if !first.Start.Equal(r.From) || !first.End.Equal(aggT0.Add(time.Minute)) || *first.Count != 3 || *first.ErrorCount != 1 {
		t.Errorf("first = %+v", first)
	}
	if !last.End.Equal(r.To) || *last.Count != 1 || !last.Incomplete {
		t.Errorf("last = %+v", last)
	}
	if mid := h.Buckets[30]; mid.Count == nil || *mid.Count != 0 || mid.Incomplete || *mid.ErrorCount != 0 {
		t.Errorf("empty past bucket = %+v", mid)
	}
	// 미래 칸
	h = buildHistogram(TimeRange{From: aggT0, To: aggT0.Add(15 * time.Minute)}, 30*time.Second, aggT0.Add(10*time.Minute), map[uint32]uint64{}, nil, map[uint32]uint64{7*19 + 5: 2})
	if b := h.Buckets[20]; b.Count != nil || b.Reason != "future" {
		t.Errorf("future = %+v", b)
	}
	if b := h.Buckets[19]; b.Count == nil || !b.Incomplete || b.BySeverity[5] != 2 {
		t.Errorf("recent = %+v", b)
	}
	if b := h.Buckets[17]; b.Incomplete {
		t.Errorf("older than grace must be complete: %+v", b)
	}
}

// top_n·other·missing: 수 내림차순, 같으면 값 오름차순. 합 = 전체.
func TestTopValues(t *testing.T) {
	vals, other, missing := topValues(map[string]uint64{"b": 5, "a": 5, "c": 9, "d": 1, "": 4}, 2, "", true)
	if len(vals) != 2 || vals[0] != (FacetValue{"c", 9}) || vals[1] != (FacetValue{"a", 5}) || other != 6 || missing != 4 {
		t.Fatalf("got %v %d %d", vals, other, missing)
	}
	// 값 없음 표시가 없는 facet은 ""도 값이다
	vals, _, missing = topValues(map[string]uint64{"": 2}, 5, "", false)
	if len(vals) != 1 || missing != 0 {
		t.Fatalf("no-missing facet: %v %d", vals, missing)
	}
}

// 속성 facet: group 상한은 throw다 — any·break는 조용히 틀린 수를 낸다(ADR 0074 §5). 읽기 상한은 절반이다.
func TestAggregateSettings(t *testing.T) {
	tenant, err := authz.ParseTenantID("11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s := aggregateSettings(ctx, tenant, false)
	if s["max_execution_time"] != 2 {
		t.Fatalf("settings = %v", s)
	}
	// 본 query: 읽기 예산은 profile 값, group 상한 없음(trace 요약 GROUP BY trace_id)
	for _, k := range []string{"max_bytes_to_read", "max_rows_to_group_by", "group_by_overflow_mode"} {
		if _, ok := s[k]; ok {
			t.Fatalf("main query must not set %s: %v", k, s)
		}
	}
	if v, ok := s[TenantSetting].(clickhouse.CustomSetting); !ok || v.Value != tenant.String() {
		t.Fatalf("tenant setting = %v", s[TenantSetting])
	}
	if a := aggregateSettings(context.Background(), tenant, true); a["max_bytes_to_read"] != attributeBytesBudget || a["max_execution_time"] != 5 ||
		a["group_by_overflow_mode"] != "throw" || a["max_rows_to_group_by"] != MaxFacetGroups {
		t.Fatalf("attribute settings = %v", a)
	}
}

// group 상한 초과만 too_many_values다. 같은 코드(158)의 읽기 상한 초과는 예산 초과다. 연결 실패는 요청 전체의 실패다.
func TestFacetFailure(t *testing.T) {
	overflow := &clickhouse.Exception{Code: 158, Message: "Limit for rows (controlled by 'max_rows_to_group_by' setting) exceeded"}
	rowsRead := &clickhouse.Exception{Code: 158, Message: "Limit for rows (controlled by 'max_rows_to_read' setting) exceeded"}
	cases := []struct {
		err     error
		reason  string
		wantErr bool
	}{
		{overflow, FacetTooManyValues, false},
		{rowsRead, FacetBudgetExceeded, false},
		{&clickhouse.Exception{Code: 307}, FacetBudgetExceeded, false},
		{&clickhouse.Exception{Code: 159}, FacetTimeout, false},
		{context.DeadlineExceeded, FacetTimeout, false},
		{&clickhouse.Exception{Code: 202}, "", true},
		{context.Canceled, "", true},
	}
	for _, c := range cases {
		res, err := facetFailure(FacetResult{Field: "attributes.x", Values: []FacetValue{{"a", 1}}, Other: 3}, c.err)
		if (err != nil) != c.wantErr || res.Reason != c.reason || (err == nil && (res.Values != nil || res.Other != 0)) {
			t.Errorf("%v: got %+v %v", c.err, res, err)
		}
	}
}

func TestCheckFacets(t *testing.T) {
	ok := queryplan.Compiled{SQL: "1"}
	attr := func(k string) FacetSpec {
		return FacetSpec{Field: "attributes." + k, Kind: FacetAttribute, AttributeKey: k, Filter: ok, TopN: 10}
	}
	if err := checkFacets([]FacetSpec{attr("a"), attr("b")}, FacetAttribute); err != nil {
		t.Fatal(err)
	}
	for name, fs := range map[string][]FacetSpec{
		"three attributes": {attr("a"), attr("b"), attr("c")},
		"five facets":      {attr("a"), attr("b"), {Kind: FacetService, Filter: ok, TopN: 1}, {Kind: FacetService, Filter: ok, TopN: 1}, {Kind: FacetService, Filter: ok, TopN: 1}},
		"top_n":            {{Kind: FacetAttribute, Filter: ok, TopN: 21}},
		"wrong signal":     {{Kind: FacetRootService, Filter: ok, TopN: 1}},
	} {
		if err := checkFacets(fs, FacetAttribute, FacetService); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
