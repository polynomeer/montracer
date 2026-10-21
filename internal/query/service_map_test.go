package query

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/servicemap"
	"github.com/polynomeer/montracer/internal/telemetrystore"
	"github.com/polynomeer/montracer/tests/contract"
)

const (
	mapCheckout = "aaaaaaaa-0000-4000-8000-000000000001"
	mapPayment  = "aaaaaaaa-0000-4000-8000-000000000002"
	mapStaging  = "aaaaaaaa-0000-4000-8000-000000000003" // staging environment
	mapLedger   = "aaaaaaaa-0000-4000-8000-000000000004"
)

type fakeEdges struct {
	edges     []telemetrystore.ServiceEdge
	truncated bool
	queries   []telemetrystore.ServiceEdgeQuery
	watermark time.Time
}

func (f *fakeEdges) ServiceEdges(_ context.Context, _ authz.Principal, q telemetrystore.ServiceEdgeQuery, _ time.Time) ([]telemetrystore.ServiceEdge, bool, error) {
	f.queries = append(f.queries, q)
	return f.edges, f.truncated, nil
}

// ServiceEdgeWatermark는 지정하지 않으면 now(범위 끝까지 계산됨)다.
func (f *fakeEdges) ServiceEdgeWatermark(context.Context, authz.Principal, time.Time, time.Time) (time.Time, error) {
	if f.watermark.IsZero() {
		return now, nil
	}
	return f.watermark, nil
}

func mapCatalog() *fakeServices {
	svc := func(id, name, env string) controldb.Service {
		return controldb.Service{ServiceID: id, Name: name, NameNormalized: strings.ToLower(name), Environment: env, Namespace: "shop",
			Status: "active", FirstSeen: now.Add(-time.Hour), LastSeen: now, Tags: []string{}}
	}
	return &fakeServices{
		all: []controldb.Service{svc(mapCheckout, "checkout", "prod"), svc(mapPayment, "payment", "prod"),
			svc(mapStaging, "payment", "staging"), svc(mapLedger, "ledger", "prod")},
		byName: map[string][]string{"payment": {mapPayment, mapStaging}, "ledger": {mapLedger}},
		envIDs: map[string][]string{"prod": {mapCheckout, mapPayment, mapLedger}, "staging": {mapStaging}},
	}
}

// edge는 n건, 경계 이하 분포가 buckets인 edge다.
func edge(src, dst, kind, conn string, n uint64, buckets ...uint64) telemetrystore.ServiceEdge {
	b := make([]uint64, len(servicemap.Bounds)+1)
	copy(b, buckets)
	return telemetrystore.ServiceEdge{Source: src, Destination: dst, DestinationKind: kind, Connection: conn, Requests: n,
		Bounds: servicemap.Bounds, Buckets: b, WaitBuckets: make([]uint64, len(servicemap.Bounds)+1), LastSeen: now.Add(-time.Minute)}
}

func mapHandler(t *testing.T, k *keys, edges ServiceMapStore, services ServiceStore) *Handler {
	t.Helper()
	h, err := NewHandler(Config{Authenticate: k.authenticate, Store: &fakeStore{}, Services: services, ServiceMap: edges,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type mapBody struct {
	Data struct {
		Nodes        []MapNode  `json:"nodes"`
		Edges        []MapEdge  `json:"edges"`
		OmittedCount mapOmitted `json:"omitted_count"`
		Range        TimeRange  `json:"range"`
	} `json:"data"`
	Meta Meta `json:"meta"`
}

const mapRange = `"range":{"from":"2026-10-05T11:00:30Z","to":"2026-10-05T12:00:00Z"}`

func queryMap(t *testing.T, h http.Handler, token, body string) mapBody {
	t.Helper()
	rec := post(h, "/api/v1/service-map/query", token, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var b mapBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func findEdge(b mapBody, src, dst string) *MapEdge {
	for i := range b.Data.Edges {
		if b.Data.Edges[i].Source == src && b.Data.Edges[i].Destination == dst {
			return &b.Data.Edges[i]
		}
	}
	return nil
}

func TestServiceMap(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	msg := edge(mapCheckout, mapLedger, "service", "messaging", 4, 0, 4)
	msg.WaitCount, msg.WaitBuckets[3] = 4, 4
	store := &fakeEdges{watermark: now.Add(-2 * time.Minute), edges: []telemetrystore.ServiceEdge{
		edge(mapCheckout, mapPayment, "service", "rpc", 100, 10, 80, 10), // observed
		edge(mapPayment, "db:postgresql/orders", "database", "rpc", 50, 50),
		edge(mapCheckout, "peer:ledger", "peer", "rpc", 3, 3),         // 같은 environment 이름 하나 → estimated
		edge(mapCheckout, "peer:legacy-billing", "peer", "rpc", 2, 2), // catalog에 없는 이름 → external
		edge(mapCheckout, "host:stripe.com", "external", "rpc", 7, 0, 0, 7),
		edge(mapCheckout, "peer:host/build-box-7", "peer", "rpc", 1, 1), // 내부 host 이름, catalog에 없음 → 이름을 숨긴다
		msg,
	}}
	h := mapHandler(t, k, store, mapCatalog())
	b := queryMap(t, h, tok, `{`+mapRange+`}`)
	if raw, _ := json.Marshal(b); strings.Contains(string(raw), "build-box") {
		t.Error("internal host name leaked into the map")
	}

	if len(store.queries) != 1 || store.queries[0].EnvironmentServices != nil {
		t.Fatalf("store query = %+v", store.queries)
	}
	if b.Data.Range.From != "2026-10-05T11:00:00Z" || b.Data.Range.To != "2026-10-05T12:00:00Z" {
		t.Errorf("range = %+v (from must be floored to the minute)", b.Data.Range)
	}
	if b.Meta.Sampled == nil || !*b.Meta.Sampled || b.Meta.ResolutionSeconds == nil || *b.Meta.ResolutionSeconds != 60 ||
		b.Meta.Watermark == nil || *b.Meta.Watermark != "2026-10-05T11:58:00Z" {
		t.Errorf("meta = %+v", b.Meta)
	}
	e := findEdge(b, mapCheckout, mapPayment)
	if e == nil || e.Confidence != "observed" || *e.Requests != 100 || e.Latency == nil || e.QueueWait != nil {
		t.Fatalf("checkout → payment = %+v", e)
	}
	if *e.Latency.P50Ms <= 0 || *e.Latency.P50Ms > 5 || *e.Latency.P99Ms <= *e.Latency.P50Ms {
		t.Errorf("latency = p50 %v p99 %v", *e.Latency.P50Ms, *e.Latency.P99Ms)
	}
	if e := findEdge(b, mapPayment, "db:postgresql/orders@prod"); e == nil || *e.Requests != 50 {
		t.Errorf("payment → database = %+v", e)
	}
	// 이름으로만 맞춘 서비스: 정확한 수를 붙이지 않는다(D05 §07)
	if e := findEdge(b, mapCheckout, mapLedger); e == nil {
		t.Error("estimated edge missing")
	}
	var estimated, messaging *MapEdge
	for i := range b.Data.Edges {
		switch x := &b.Data.Edges[i]; {
		case x.Destination == mapLedger && x.Confidence == "estimated":
			estimated = x
		case x.Destination == mapLedger && x.Connection == "messaging":
			messaging = x
		}
	}
	if estimated == nil || estimated.Requests != nil || estimated.Errors != nil || estimated.Latency != nil {
		t.Errorf("estimated edge = %+v", estimated)
	}
	if messaging == nil || messaging.Confidence != "observed" || messaging.QueueWait == nil || *messaging.QueueWait.P50Ms <= 10 {
		t.Errorf("messaging edge = %+v", messaging)
	}
	if e := findEdge(b, mapCheckout, "peer:legacy-billing@prod"); e == nil || e.Confidence != "observed" {
		t.Errorf("unknown peer = %+v", e)
	}
	kinds := map[string]string{}
	for _, n := range b.Data.Nodes {
		kinds[n.NodeID] = n.Kind
		if n.Kind == "service" && (n.ServiceID == nil || *n.ServiceID != n.NodeID || n.Namespace == nil) {
			t.Errorf("service node = %+v", n)
		}
		if n.Kind != "service" && (n.ServiceID != nil || n.Environment != "prod") {
			t.Errorf("external node = %+v", n)
		}
	}
	want := map[string]string{mapCheckout: "service", mapPayment: "service", mapLedger: "service",
		"db:postgresql/orders@prod": "database", "peer:legacy-billing@prod": "external", "host:stripe.com@prod": "external", "host:internal@prod": "external"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Errorf("nodes = %v, want %v", kinds, want)
	}
	// watermark(11:58)가 범위 끝(12:00)보다 앞이다: 마지막 2분은 미계산이라고 알린다(0이 아님)
	if b.Data.OmittedCount != (mapOmitted{}) || fmt.Sprint(b.Meta.Warnings) != "[range_after_watermark]" {
		t.Errorf("omitted = %+v warnings = %v", b.Data.OmittedCount, b.Meta.Warnings)
	}
}

// environment가 다른 서비스 사이 edge는 숨기고 경고한다. 볼 수 없는 서비스에 닿는 edge는 흔적 없이 뺀다.
func TestServiceMapEnvironmentScope(t *testing.T) {
	k := newKeys(t)
	store := &fakeEdges{edges: []telemetrystore.ServiceEdge{
		edge(mapCheckout, mapPayment, "service", "rpc", 5, 5),
		edge(mapCheckout, mapStaging, "service", "rpc", 1, 1), // prod → staging
		edge(mapStaging, "db:redis", "database", "rpc", 9, 9),
		edge(mapCheckout, "peer:payment", "peer", "rpc", 2, 2), // payment가 prod·staging에 있지만 prod는 하나
	}}
	h := mapHandler(t, k, store, mapCatalog())

	all := queryMap(t, h, k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil), `{`+mapRange+`}`)
	if findEdge(all, mapCheckout, mapStaging) != nil || len(all.Meta.Warnings) != 1 || all.Meta.Warnings[0] != warnCrossEnvironment {
		t.Errorf("cross-environment edge = %+v warnings %v", all.Data.Edges, all.Meta.Warnings)
	}
	if findEdge(all, mapStaging, "db:redis@staging") == nil {
		t.Error("staging edge missing for an unrestricted key")
	}

	// prod로 제한된 key: 저장소에 source 범위를 넘기고, staging 서비스는 node로도 나오지 않는다
	prod := queryMap(t, h, k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, []string{"prod"}), `{`+mapRange+`}`)
	q := store.queries[len(store.queries)-1]
	if fmt.Sprint(q.EnvironmentServices) != fmt.Sprint([]string{mapCheckout, mapPayment, mapLedger}) {
		t.Errorf("environment services = %v", q.EnvironmentServices)
	}
	for _, n := range prod.Data.Nodes {
		if n.Environment != "prod" {
			t.Errorf("node outside scope: %+v", n)
		}
	}
	if len(prod.Meta.Warnings) != 0 { // 볼 수 없는 서비스로 가는 edge가 있다는 사실도 알리지 않는다
		t.Errorf("warnings = %v", prod.Meta.Warnings)
	}

	// environment·service_ids로 좁힌다
	narrowed := queryMap(t, h, k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil),
		`{`+mapRange+`,"environment":"staging"}`)
	if len(narrowed.Data.Edges) != 1 || narrowed.Data.Edges[0].Source != mapStaging {
		t.Errorf("environment filter = %+v", narrowed.Data.Edges)
	}
	focused := queryMap(t, h, k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil),
		`{`+mapRange+`,"service_ids":["`+mapPayment+`"]}`)
	for _, e := range focused.Data.Edges {
		if e.Source != mapPayment && e.Destination != mapPayment {
			t.Errorf("service_ids filter kept %+v", e)
		}
	}
	if len(focused.Data.Edges) != 2 { // observed checkout → payment, estimated checkout → payment(peer)
		t.Errorf("focused edges = %+v", focused.Data.Edges)
	}
}

func TestServiceMapLimits(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	store := &fakeEdges{truncated: true}
	for i := range MaxMapNodes + 10 {
		store.edges = append(store.edges, edge(mapCheckout, fmt.Sprintf("host:h%04d.example", i), "external", "rpc", uint64(i+1), 1))
	}
	h := mapHandler(t, k, store, mapCatalog())
	b := queryMap(t, h, tok, `{`+mapRange+`}`)
	if len(b.Data.Nodes) != MaxMapNodes || b.Data.OmittedCount.Nodes != 11 || b.Data.OmittedCount.Edges != 11 {
		t.Fatalf("nodes = %d omitted = %+v", len(b.Data.Nodes), b.Data.OmittedCount)
	}
	// 요청 수가 많은 쪽을 남긴다: 가장 작은 h0000은 빠진다
	if findEdge(b, mapCheckout, "host:h0000.example@prod") != nil || findEdge(b, mapCheckout, "host:h0509.example@prod") == nil {
		t.Error("kept low-traffic edge over high-traffic edge")
	}
	if len(b.Meta.Warnings) != 1 || b.Meta.Warnings[0] != warnEdgesTruncated {
		t.Errorf("warnings = %v", b.Meta.Warnings)
	}
}

func TestServiceMapBoundsMismatch(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	odd := edge(mapCheckout, mapPayment, "service", "rpc", 1)
	odd.Bounds, odd.Buckets = []float64{1}, []uint64{1, 0}
	store := &fakeEdges{edges: []telemetrystore.ServiceEdge{edge(mapCheckout, mapPayment, "service", "rpc", 5, 5), odd}}
	b := queryMap(t, mapHandler(t, k, store, mapCatalog()), tok, `{`+mapRange+`}`)
	e := findEdge(b, mapCheckout, mapPayment)
	if e == nil || *e.Requests != 6 || e.Latency != nil || len(b.Meta.Warnings) != 1 || b.Meta.Warnings[0] != warnBoundsMismatch {
		t.Fatalf("edge = %+v warnings = %v", e, b.Meta.Warnings)
	}
}

func TestServiceMapRejects(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	h := mapHandler(t, k, &fakeEdges{}, mapCatalog())
	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"범위 없음", `{}`, 400, "INVALID_ARGUMENT"},
		{"뒤집힌 범위", `{"range":{"from":"2026-10-05T12:00:00Z","to":"2026-10-05T11:00:00Z"}}`, 400, "INVALID_ARGUMENT"},
		{"7일 초과", `{"range":{"from":"2026-09-01T00:00:00Z","to":"2026-10-05T00:00:00Z"}}`, 422, "QUERY_BUDGET_EXCEEDED"},
		{"대문자 UUID", `{` + mapRange + `,"service_ids":["AAAAAAAA-0000-4000-8000-000000000001"]}`, 400, "INVALID_ARGUMENT"},
		{"모르는 필드", `{` + mapRange + `,"team":"x"}`, 400, "INVALID_ARGUMENT"},
		{"긴 environment", `{` + mapRange + `,"environment":"` + strings.Repeat("e", 256) + `"}`, 400, "INVALID_ARGUMENT"},
		{"본문 초과", `{` + mapRange + `,"environment":"` + strings.Repeat("e", 70<<10) + `"}`, 413, "INVALID_ARGUMENT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(h, "/api/v1/service-map/query", tok, c.body)
			var b errorBody
			_ = json.Unmarshal(rec.Body.Bytes(), &b)
			if rec.Code != c.status || b.Error.Code != c.code {
				t.Fatalf("status = %d code = %s: %s", rec.Code, b.Error.Code, rec.Body)
			}
		})
	}
	// ingest key는 조회하지 못한다
	ingest := k.issue(t, authz.KindIngestKey, []authz.Action{authz.IngestTraces}, nil)
	if rec := post(h, "/api/v1/service-map/query", ingest, `{`+mapRange+`}`); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("ingest key status = %d", rec.Code)
	}
}

// flag off(ServiceMap nil)면 경로가 404다(ADR 0062 Rollback).
func TestServiceMapDisabled(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	h := mapHandler(t, k, nil, mapCatalog())
	if rec := post(h, "/api/v1/service-map/query", tok, `{`+mapRange+`}`); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

// 본문이 JSON이 아니면 415다(ADR 0060 §6): form·text로 조회 API를 부르는 browser 경로를 막는다.
func TestNonJSONBodyIs415(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	h := mapHandler(t, k, &fakeEdges{}, mapCatalog())
	for _, ctype := range []string{"text/plain", "application/x-www-form-urlencoded"} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/service-map/query", strings.NewReader(`{`+mapRange+`}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", ctype)
		rec := httptest.NewRecorder()
		contract.Serve(h, rec, req)
		var b errorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &b)
		if rec.Code != http.StatusUnsupportedMediaType || b.Error.Code != "INVALID_ARGUMENT" || !strings.Contains(rec.Body.String(), `"field":"Content-Type"`) {
			t.Errorf("%s: %d %s", ctype, rec.Code, rec.Body)
		}
	}
}
