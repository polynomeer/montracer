package query

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/metricagg"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

type fakeMetricCatalog struct {
	all       []telemetrystore.MetricDescriptor
	queries   []telemetrystore.MetricCatalogQuery
	nows      []time.Time
	envs      [][]string
	keys      []telemetrystore.MetricLabelKey
	truncated bool
	gotMetric string
	gotRange  telemetrystore.TimeRange
}

func (f *fakeMetricCatalog) MetricCatalog(_ context.Context, p authz.Principal, q telemetrystore.MetricCatalogQuery, now time.Time) ([]telemetrystore.MetricDescriptor, bool, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, false, err
	}
	f.queries = append(f.queries, q)
	f.nows = append(f.nows, now)
	f.envs = append(f.envs, p.Environments())
	var out []telemetrystore.MetricDescriptor
	for _, d := range f.all {
		if d.Name > q.After {
			out = append(out, d)
		}
	}
	if len(out) > q.Limit {
		return out[:q.Limit], true, nil
	}
	return out, false, nil
}

func (f *fakeMetricCatalog) MetricLabelKeys(_ context.Context, p authz.Principal, metric string, r telemetrystore.TimeRange, _ time.Time) ([]telemetrystore.MetricLabelKey, bool, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, false, err
	}
	f.gotMetric, f.gotRange = metric, r
	return f.keys, f.truncated, nil
}

func catalogHandler(t *testing.T, k *keys, store MetricCatalogStore, clock *time.Time) *Handler {
	t.Helper()
	signer, _ := apicursor.NewSigner([]byte("0123456789abcdef0123456789abcdef"), 15*time.Minute, func() time.Time { return *clock })
	h, err := NewHandler(Config{Authenticate: k.authenticate, Store: &fakeStore{}, MetricCatalog: store, Cursor: signer,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return *clock }})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestAllowedAggregations(t *testing.T) {
	for _, c := range []struct {
		typ, temp string
		mono      bool
		want      string
	}{
		{"gauge", "unspecified", false, "[avg min max]"},
		{"sum", "cumulative", true, "[rate increase sum]"},
		{"sum", "delta", true, "[rate increase sum]"},
		{"sum", "cumulative", false, "[avg min max]"}, // up-down counter: 값. rate 금지(D02 §07)
		{"sum", "delta", false, "[sum]"},
		{"sum", "unspecified", false, "[avg min max]"}, // metricagg: unspecified는 원시 값
		{"sum", "unspecified", true, "[avg min max]"},  // 단조여도 차분하지 않는다 → rate는 영구 not_applicable
		{"histogram", "delta", false, "[p50 p90 p95 p99 count hist_sum]"},
		{"summary", "unspecified", false, "[]"}, // quantile 평균 금지 — rollup 조회 대상이 아니다
		{"exponential_histogram", "delta", false, "[]"},
	} {
		if got := fmt.Sprint(AllowedAggregations(c.typ, c.temp, c.mono)); got != c.want {
			t.Errorf("%s/%s/%v = %s, want %s", c.typ, c.temp, c.mono, got, c.want)
		}
	}
}

func TestListMetrics(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	clock := now
	last := now.Add(-time.Minute)
	store := &fakeMetricCatalog{all: []telemetrystore.MetricDescriptor{
		{Name: "a.gauge", Variants: []telemetrystore.MetricVariant{{Type: "gauge", Temporality: "unspecified", Unit: "1", Series: 2, LastSeen: last}}},
		{Name: "b.mixed", Variants: []telemetrystore.MetricVariant{
			{Type: "histogram", Temporality: "delta", Unit: "ms", Series: 1, LastSeen: last},
			{Type: "histogram", Temporality: "delta", Unit: "s", Series: 3, LastSeen: last},
		}},
		{Name: "c.counter", Variants: []telemetrystore.MetricVariant{{Type: "sum", Temporality: "cumulative", Monotonic: true, Unit: "{request}", Series: 5, LastSeen: last}}},
	}}
	h := catalogHandler(t, k, store, &clock)
	from, to := now.Add(-time.Hour).Format(time.RFC3339), now.Format(time.RFC3339)
	params := url.Values{"from": {from}, "to": {to}, "limit": {"2"}, "q": {"."}}

	var items []MetricItem
	for page := 0; page < 3; page++ {
		rec := get(h, "/api/v1/metrics?"+params.Encode(), tok)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, rec.Code, rec.Body)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Error("tenant data must not be cached")
		}
		var body struct {
			Data       []MetricItem `json:"data"`
			NextCursor *string      `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		items = append(items, body.Data...)
		if body.NextCursor == nil {
			break
		}
		params.Set("cursor", *body.NextCursor)
		clock = clock.Add(time.Minute)
	}
	if len(items) != 3 || items[0].Name != "a.gauge" || items[2].Name != "c.counter" {
		t.Fatalf("items = %+v", items)
	}
	if items[0].Conflict || !items[1].Conflict || len(items[1].Variants) != 2 {
		t.Errorf("conflict flags: %+v", items)
	}
	if fmt.Sprint(items[2].Variants[0].Aggregations) != "[rate increase sum]" || items[2].Variants[0].Series != 5 ||
		items[2].Variants[0].LastSeen != last.Format(time.RFC3339Nano) {
		t.Errorf("counter variant = %+v", items[2].Variants[0])
	}
	q := store.queries[len(store.queries)-1]
	if q.Contains != "." || q.After != "b.mixed" || q.Limit != 2 || !store.nows[len(store.nows)-1].Equal(now) {
		t.Errorf("last query = %+v now=%v (snapshot must stay at the first page)", q, store.nows)
	}
	// 다른 검색어로 cursor 재사용 → 400
	params.Set("q", "other")
	if rec := get(h, "/api/v1/metrics?"+params.Encode(), tok); rec.Code != http.StatusBadRequest {
		t.Errorf("cursor with other q: %d", rec.Code)
	}
}

func TestListMetricsRejects(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	clock := now
	store := &fakeMetricCatalog{}
	h := catalogHandler(t, k, store, &clock)
	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Format(time.RFC3339)
	for _, c := range []struct {
		path string
		code int
	}{
		{"/api/v1/metrics", http.StatusBadRequest},                                                                 // 범위 필수
		{"/api/v1/metrics?from=" + to + "&to=" + from, http.StatusBadRequest},                                      // 거꾸로
		{"/api/v1/metrics?from=" + now.Add(-25*time.Hour).Format(time.RFC3339) + "&to=" + to, 422},                 // 24시간 초과
		{"/api/v1/metrics?from=" + from + "&to=" + to + "&limit=1001", http.StatusBadRequest},                      // page 한도
		{"/api/v1/metrics?from=" + from + "&to=" + to + "&cursor=bogus", http.StatusBadRequest},                    // 위조 cursor
		{"/api/v1/metrics/labels?from=" + from + "&to=" + to, http.StatusBadRequest},                               // metric 필수
		{"/api/v1/metrics/labels?metric=x&from=" + now.Add(-48*time.Hour).Format(time.RFC3339) + "&to=" + to, 422}, // 24시간 초과
	} {
		if rec := get(h, c.path, tok); rec.Code != c.code {
			t.Errorf("%s: %d, want %d (%s)", c.path, rec.Code, c.code, rec.Body)
		}
	}
	if len(store.queries) != 0 || store.gotMetric != "" {
		t.Errorf("rejected requests must not reach the store: %+v %q", store.queries, store.gotMetric)
	}
	if rec := get(h, "/api/v1/metrics?from="+from+"&to="+to, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", rec.Code)
	}
	ingest := k.issue(t, authz.KindIngestKey, []authz.Action{authz.IngestTraces}, nil)
	if rec := get(h, "/api/v1/metrics?from="+from+"&to="+to, ingest); rec.Code == http.StatusOK {
		t.Errorf("ingest key must not read the dictionary: %d", rec.Code)
	}
}

// environment로 제한된 key의 범위는 저장소로 그대로 간다(저장소가 mandatory predicate로 건다).
func TestListMetricsEnvironmentScopedKey(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, []string{"prod"})
	clock := now
	store := &fakeMetricCatalog{}
	h := catalogHandler(t, k, store, &clock)
	rec := get(h, "/api/v1/metrics?from="+now.Add(-time.Hour).Format(time.RFC3339)+"&to="+now.Format(time.RFC3339), tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if fmt.Sprint(store.envs[0]) != "[prod]" {
		t.Errorf("principal envs = %v", store.envs[0])
	}
}

func TestListMetricLabels(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	clock := now
	store := &fakeMetricCatalog{
		keys:      []telemetrystore.MetricLabelKey{{Key: "service.name", Sources: []string{"resource"}, Series: 4}, {Key: "http.route", Sources: []string{"attribute"}, Series: 2}},
		truncated: true,
	}
	h := catalogHandler(t, k, store, &clock)
	from, to := now.Add(-time.Hour), now
	params := url.Values{"metric": {"http.server.request.duration"}, "from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}
	rec := get(h, "/api/v1/metrics/labels?"+params.Encode(), tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var body struct {
		Data struct {
			Metric string            `json:"metric"`
			Keys   []MetricLabelItem `json:"keys"`
		} `json:"data"`
		Meta Meta `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data.Metric != "http.server.request.duration" || len(body.Data.Keys) != 2 || body.Data.Keys[0].Key != "service.name" ||
		fmt.Sprint(body.Meta.Warnings) != "[label_keys_truncated]" {
		t.Errorf("body = %+v", body)
	}
	if store.gotMetric != "http.server.request.duration" || !store.gotRange.From.Equal(from) || !store.gotRange.To.Equal(to) {
		t.Errorf("store got %q %+v", store.gotMetric, store.gotRange)
	}
}

func TestMetricCatalogDisabled(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	h := newHandler(t, k, &fakeStore{})
	q := "?from=" + now.Add(-time.Hour).Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339)
	for _, p := range []string{"/api/v1/metrics" + q, "/api/v1/metrics/labels" + q + "&metric=x"} {
		if rec := get(h, p, tok); rec.Code != http.StatusNotFound {
			t.Errorf("%s without catalog: %d", p, rec.Code)
		}
	}
}

// 사전의 허용 연산은 rollup이 실제로 계산하는 값과 같아야 한다(ADR 0046 §1). 증가량을 만드는 조합에만 rate·increase,
// 원시 값을 만드는 조합에만 avg·min·max, histogram 상태를 만드는 조합에만 분위수다. 두 규칙이 어긋나면 화면 기본 연산이
// 영구 not_applicable이 된다(리뷰 P1).
func TestAllowedAggregationsMatchRollup(t *testing.T) {
	w := metricagg.Window{Start: now, End: now.Add(time.Minute)}
	types := map[string]metricagg.Type{"gauge": metricagg.Gauge, "sum": metricagg.Sum, "histogram": metricagg.Histogram, "summary": metricagg.Summary}
	temps := map[string]metricagg.Temporality{"unspecified": metricagg.Unspecified, "delta": metricagg.Delta, "cumulative": metricagg.Cumulative}
	for tn, typ := range types {
		for tpn, tp := range temps {
			for _, mono := range []bool{false, true} {
				p := metricagg.Point{Start: now, End: now.Add(30 * time.Second), Value: 5, Count: 2, Sum: 3, HasSum: true, Bounds: []float64{1}, Buckets: []uint64{1, 1}}
				base := metricagg.Point{Start: now, End: now.Add(-time.Second), Value: 1, Count: 1, Sum: 1, HasSum: true, Bounds: []float64{1}, Buckets: []uint64{1, 0}}
				a := metricagg.Compute(metricagg.Stream{Type: typ, Temporality: tp, Monotonic: mono}, w, &base, []metricagg.Point{p})
				allowed := AllowedAggregations(tn, tpn, mono)
				has := func(op string) bool { return slices.Contains(allowed, op) }
				if has("rate") && (!a.HasIncrease || !mono) {
					t.Errorf("%s/%s/mono=%v: rate allowed but rollup increase=%v", tn, tpn, mono, a.HasIncrease)
				}
				if has("sum") != a.HasIncrease {
					t.Errorf("%s/%s/mono=%v: sum allowed=%v, rollup increase=%v", tn, tpn, mono, has("sum"), a.HasIncrease)
				}
				if has("avg") != a.HasValue {
					t.Errorf("%s/%s/mono=%v: avg allowed=%v, rollup value=%v", tn, tpn, mono, has("avg"), a.HasValue)
				}
				if has("p95") != a.HasHistogram {
					t.Errorf("%s/%s/mono=%v: p95 allowed=%v, rollup histogram=%v", tn, tpn, mono, has("p95"), a.HasHistogram)
				}
			}
		}
	}
}
