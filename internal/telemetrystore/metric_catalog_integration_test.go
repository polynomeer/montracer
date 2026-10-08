//go:build integration

package telemetrystore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// metric 사전(ADR 0046): 이름순·조합(유형·단위 충돌)·series 수·마지막 관측, 이름 검색·keyset,
// mandatory predicate(tenant·시간·만료·environment 범위), 검색어는 parameter(SQL 조각이 아무것도 바꾸지 않음).
func TestMetricCatalog(t *testing.T) {
	s := openQuery(t)
	tenant, pa := newTenant(t)
	other, pb := newTenant(t)
	w0 := time.Now().UTC().Truncate(time.Minute).Add(-30 * time.Minute)
	prod := `{"deployment.environment.name":"prod","service.name":"checkout"}`
	stage := `{"deployment.environment.name":"stage","service.name":"checkout"}`
	ta := tenant.String()
	s1, s2, s3, s4, s5, s6 := rnd16(), rnd16(), rnd16(), rnd16(), rnd16(), rnd16()
	insertM1m(t,
		// cat.counter: 두 stream, 마지막 window는 w0+5m
		m1m{tenant: ta, name: "cat.counter", stream: s1, window: w0, resource: prod, typ: "sum", unit: "{request}", revision: 1, hasInc: true},
		m1m{tenant: ta, name: "cat.counter", stream: s1, window: w0.Add(5 * time.Minute), resource: prod, typ: "sum", unit: "{request}", revision: 1, hasInc: true},
		m1m{tenant: ta, name: "cat.counter", stream: s2, window: w0, resource: stage, typ: "sum", unit: "{request}", revision: 1, hasInc: true},
		// cat.duration: 단위가 다른 두 stream → 조합 둘(충돌)
		m1m{tenant: ta, name: "cat.duration", stream: s3, window: w0, resource: prod, typ: "histogram", temporality: "delta", nonMono: true, unit: "ms", revision: 1, hasHist: true},
		m1m{tenant: ta, name: "cat.duration", stream: s4, window: w0, resource: prod, typ: "histogram", temporality: "delta", nonMono: true, unit: "s", revision: 1, hasHist: true},
		// cat.gauge: stage에만
		m1m{tenant: ta, name: "cat.gauge", stream: s5, window: w0, resource: stage, typ: "gauge", temporality: "unspecified", nonMono: true, unit: "1", revision: 1, hasValue: true},
		// 범위 밖·만료된 행은 사전에 없다
		m1m{tenant: ta, name: "cat.old", stream: s6, window: w0.Add(-2 * time.Hour), resource: prod, typ: "gauge", unit: "1", revision: 1},
		m1m{tenant: ta, name: "cat.expired", stream: s6, window: w0, resource: prod, typ: "gauge", unit: "1", revision: 1, expired: true},
		// 다른 tenant
		m1m{tenant: other.String(), name: "cat.secret", stream: s1, window: w0, resource: prod, typ: "gauge", unit: "1", revision: 1},
	)
	ctx := context.Background()
	now := time.Now()
	rng := TimeRange{From: w0.Add(-10 * time.Minute), To: w0.Add(20 * time.Minute)}

	all, more, err := s.MetricCatalog(ctx, pa, MetricCatalogQuery{Range: rng, Contains: "CAT.", Limit: 100}, now)
	if err != nil {
		t.Fatal(err)
	}
	if more || len(all) != 3 || all[0].Name != "cat.counter" || all[1].Name != "cat.duration" || all[2].Name != "cat.gauge" {
		t.Fatalf("catalog = %+v more=%v", all, more)
	}
	c := all[0].Variants
	if len(c) != 1 || c[0].Type != "sum" || !c[0].Monotonic || c[0].Temporality != "cumulative" || c[0].Unit != "{request}" ||
		c[0].Series != 2 || !c[0].LastSeen.Equal(w0.Add(6*time.Minute)) {
		t.Errorf("counter = %+v", c)
	}
	if d := all[1].Variants; len(d) != 2 || d[0].Unit != "ms" || d[1].Unit != "s" || d[0].Monotonic {
		t.Errorf("duration variants = %+v", d)
	}

	// keyset: 한 개씩
	var names []string
	q := MetricCatalogQuery{Range: rng, Contains: "cat.", Limit: 1}
	for i := 0; i < 5; i++ {
		page, more, err := s.MetricCatalog(ctx, pa, q, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range page {
			names = append(names, d.Name)
		}
		if !more {
			break
		}
		q.After = page[len(page)-1].Name
	}
	if fmt.Sprint(names) != "[cat.counter cat.duration cat.gauge]" {
		t.Errorf("pages = %v", names)
	}

	// environment 제한 key: prod stream만(stage만 있는 gauge는 없고, counter series는 1)
	prodKey := keyPrincipal(t, tenant, []string{"prod"})
	scoped, _, err := s.MetricCatalog(ctx, prodKey, MetricCatalogQuery{Range: rng, Contains: "cat.", Limit: 100}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 2 || scoped[0].Variants[0].Series != 1 {
		t.Errorf("prod-scoped catalog = %+v", scoped)
	}

	// 다른 tenant는 자기 것만
	otherCat, _, err := s.MetricCatalog(ctx, pb, MetricCatalogQuery{Range: rng, Contains: "cat.", Limit: 100}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherCat) != 1 || otherCat[0].Name != "cat.secret" {
		t.Errorf("other tenant catalog = %+v", otherCat)
	}

	// 검색어는 parameter다: SQL 조각은 이름 일치로만 쓰인다
	inj, _, err := s.MetricCatalog(ctx, pa, MetricCatalogQuery{Range: rng, Contains: "') > 0 OR 1=1 --", Limit: 100}, now)
	if err != nil || len(inj) != 0 {
		t.Errorf("injection-shaped q = %+v, %v", inj, err)
	}

	// 24시간 초과는 예산 초과
	if _, _, err := s.MetricCatalog(ctx, pa, MetricCatalogQuery{Range: TimeRange{From: w0.Add(-25 * time.Hour), To: w0}, Limit: 10}, now); !isBudget(err) {
		t.Errorf("25h range err = %v", err)
	}
}

func TestMetricLabelKeys(t *testing.T) {
	s := openQuery(t)
	tenant, pa := newTenant(t)
	w0 := time.Now().UTC().Truncate(time.Minute).Add(-20 * time.Minute)
	ta := tenant.String()
	s1, s2, s3 := rnd16(), rnd16(), rnd16()
	insertM1m(t,
		// s1: 예전 window에는 old.key, 최근 window에는 http.route — 최근 label을 쓴다
		m1m{tenant: ta, name: "lbl.metric", stream: s1, window: w0, resource: `{"service.name":"checkout"}`, attrs: `{"old.key":"x"}`, typ: "gauge", unit: "1", revision: 1},
		m1m{tenant: ta, name: "lbl.metric", stream: s1, window: w0.Add(time.Minute), resource: `{"service.name":"checkout"}`, attrs: `{"http.route":"/cart"}`, typ: "gauge", unit: "1", revision: 1},
		m1m{tenant: ta, name: "lbl.metric", stream: s2, window: w0, resource: `{"service.name":"payment","http.route":"/r"}`, attrs: `{}`, typ: "gauge", unit: "1", revision: 1},
		m1m{tenant: ta, name: "lbl.other", stream: s3, window: w0, resource: `{"other.only":"1"}`, attrs: `{}`, typ: "gauge", unit: "1", revision: 1},
	)
	keys, truncated, err := s.MetricLabelKeys(context.Background(), pa, "lbl.metric", TimeRange{From: w0.Add(-time.Minute), To: w0.Add(5 * time.Minute)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, k := range keys {
		got[k.Key] = fmt.Sprintf("%v/%d", k.Sources, k.Series)
	}
	want := map[string]string{"service.name": "[resource]/2", "http.route": "[attribute resource]/2"}
	if truncated || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("keys = %v truncated=%v, want %v", got, truncated, want)
	}
	if len(keys) == 0 || keys[0].Series < keys[len(keys)-1].Series {
		t.Errorf("keys must be ordered by series desc: %+v", keys)
	}
}

func isBudget(err error) bool {
	var b *budgetError
	return errors.As(err, &b)
}
