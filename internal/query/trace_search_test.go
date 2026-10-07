package query

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

type fakeTraces struct {
	all     []telemetrystore.TraceSummary
	queries []telemetrystore.TraceSearchQuery
	envs    [][]string
}

func (f *fakeTraces) SearchTraces(_ context.Context, p authz.Principal, q telemetrystore.TraceSearchQuery, _ time.Time) ([]telemetrystore.TraceSummary, bool, error) {
	f.queries = append(f.queries, q)
	f.envs = append(f.envs, p.Environments())
	var out []telemetrystore.TraceSummary
	for _, t := range f.all {
		if q.After != nil && !t.Start.Before(q.After.Start) {
			continue
		}
		out = append(out, t)
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return out, more, nil
}

const (
	checkoutID = "aaaaaaaa-0000-4000-8000-000000000001"
	paymentID  = "aaaaaaaa-0000-4000-8000-000000000002"
)

func sampleTraces() []telemetrystore.TraceSummary {
	return []telemetrystore.TraceSummary{
		{TraceID: strings.Repeat("1", 32), Start: now.Add(-time.Minute), DurationMs: 842.5, SpanCount: 3, HasError: true, Roots: 1, RootServiceID: checkoutID, RootName: "POST /checkout"},
		{TraceID: strings.Repeat("2", 32), Start: now.Add(-2 * time.Minute), DurationMs: 10, SpanCount: 1, Roots: 0, MissingParent: true},
		{TraceID: strings.Repeat("3", 32), Start: now.Add(-3 * time.Minute), DurationMs: 30, SpanCount: 2, Roots: 1, RootServiceID: paymentID, RootName: "charge"},
	}
}

func traceHandler(t *testing.T, k *keys, traces TraceStore, services ServiceStore, clock *time.Time) *Handler {
	t.Helper()
	signer, err := apicursor.NewSigner([]byte("0123456789abcdef0123456789abcdef"), 15*time.Minute, func() time.Time { return *clock })
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(Config{Authenticate: k.authenticate, Store: &fakeStore{}, Traces: traces, Services: services, Cursor: signer,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return *clock }})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestSearchTraces(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	clock := now
	traces := &fakeTraces{all: sampleTraces()}
	// payment은 catalog에 아직 없다(root 서비스 이름 null, id는 남는다)
	services := &fakeServices{all: []controldb.Service{{ServiceID: checkoutID, Name: "checkout"}}, byName: map[string][]string{"checkout": {checkoutID}}}
	h := traceHandler(t, k, traces, services, &clock)

	rec := post(h, "/api/v1/query/traces", tok, `{"limit":2,"filter":{"op":"and","args":[
		{"field":"service.name","op":"eq","value":"checkout"},{"field":"has_error","op":"eq","value":true},{"field":"duration_ms","op":"gte","value":500}]}}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	b := decodeSearch(t, rec)
	if len(b.Data) != 2 || b.NextCursor == nil {
		t.Fatalf("page 1 = %+v", b)
	}
	first, orphan := b.Data[0], b.Data[1]
	if first["last_updated_at"] == nil {
		t.Errorf("last_updated_at missing: %v", first)
	}
	if first["trace_id"] != strings.Repeat("1", 32) || first["root_service"] != "checkout" || first["root_name"] != "POST /checkout" ||
		first["duration_ms"] != 842.5 || first["has_error"] != true || first["complete"] != true || first["span_count"] != float64(3) {
		t.Errorf("first row = %v", first)
	}
	if orphan["complete"] != false || fmt.Sprint(orphan["reasons"]) != "[missing_root missing_parent]" || orphan["root_service"] != nil ||
		orphan["root_name"] != nil || orphan["root_service_id"] != nil || orphan["has_error"] != false {
		t.Errorf("orphan row = %v (모르는 값은 null, 오류 없음 ≠ 성공은 화면 몫)", orphan)
	}
	// 저장소에 간 조건: span 단계(service.name → service_id 집합, 접두어 s)와 요약 단계(접두어 t)
	q := traces.queries[0]
	if !strings.Contains(q.SpanFilter.SQL, "service_id IN {s0:Array(UUID)}") || fmt.Sprint(q.SpanFilter.Params["s0"]) != "["+checkoutID+"]" {
		t.Errorf("span filter = %+v", q.SpanFilter)
	}
	if q.TraceFilter.SQL != "(has_error = {t0:Bool}) AND (duration_ms >= {t1:Int64})" || q.Limit != 2 || !q.ReceivedBefore.Equal(now) {
		t.Errorf("trace query = %+v", q)
	}
	// 다음 page: 같은 query + cursor, snapshot 유지. 다른 filter로 cursor 재사용은 400
	clock = now.Add(time.Minute)
	body2 := fmt.Sprintf(`{"limit":2,"cursor":%q,"filter":{"op":"and","args":[
		{"field":"service.name","op":"eq","value":"checkout"},{"field":"has_error","op":"eq","value":true},{"field":"duration_ms","op":"gte","value":500}]}}`, *b.NextCursor)
	b2 := decodeSearch(t, post(h, "/api/v1/query/traces", tok, body2))
	if len(b2.Data) != 1 || b2.Data[0]["root_service"] != nil || b2.Data[0]["root_service_id"] != paymentID || b2.NextCursor != nil {
		t.Errorf("page 2 = %+v", b2)
	}
	if q2 := traces.queries[1]; q2.After == nil || !q2.ReceivedBefore.Equal(now) {
		t.Errorf("page 2 query = %+v (snapshot은 첫 page 시각)", q2)
	}
	if rec := post(h, "/api/v1/query/traces", tok, fmt.Sprintf(`{"cursor":%q}`, *b.NextCursor)); rec.Code != 400 {
		t.Errorf("cursor for another query: %d", rec.Code)
	}
	// /query signal=traces도 같은 경로
	if rec := post(h, "/api/v1/query", tok, `{"signal":"traces","projection":["trace_id","complete"]}`); rec.Code != 200 {
		t.Errorf("/query signal=traces: %d %s", rec.Code, rec.Body)
	} else if row := decodeSearch(t, rec).Data[0]; len(row) != 2 {
		t.Errorf("projection row = %v", row)
	}
}

func TestSearchTracesRejects(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	clock := now
	traces := &fakeTraces{}
	h := traceHandler(t, k, traces, &fakeServices{}, &clock)
	for name, c := range map[string]struct {
		body   string
		status int
		field  string
	}{
		"order by duration": {`{"order":[{"field":"duration_ms","direction":"desc"}]}`, 422, "order"},
		"mixed levels in or": {`{"filter":{"op":"or","args":[{"field":"status","op":"eq","value":2},{"field":"has_error","op":"eq","value":true}]}}`,
			400, "filter"},
		"unknown field":      {`{"filter":{"field":"payload","op":"eq","value":"x"}}`, 400, "filter.field"},
		"bool as string":     {`{"filter":{"field":"has_error","op":"eq","value":"true"}}`, 400, "filter.value"},
		"unknown projection": {`{"projection":["payload"]}`, 400, "projection"},
		"limit too big":      {`{"limit":1001}`, 400, "limit"},
		"bad cursor":         {`{"cursor":"c1.x.y"}`, 400, "cursor"},
		// 한도는 단계별이 아니라 요청 filter 전체에 건다(나누면 20+20·깊이 +1이 될 수 있었다)
		"21 leaves across stages": {`{"filter":{"op":"and","args":[` + strings.Repeat(`{"field":"status","op":"eq","value":2},`, 11) +
			strings.Repeat(`{"field":"has_error","op":"eq","value":true},`, 9) + `{"field":"span_count","op":"gte","value":1}]}}`, 400, "filter"},
		"depth 5": {`{"filter":{"op":"and","args":[{"op":"or","args":[{"op":"and","args":[{"op":"or","args":[{"op":"and","args":[
			{"field":"status","op":"eq","value":2}]}]}]}]}]}}`, 400, "filter"},
		"empty and":      {`{"filter":{"op":"and","args":[]}}`, 400, "filter"},
		"and with field": {`{"filter":{"op":"and","field":"status","args":[{"field":"status","op":"eq","value":2}]}}`, 400, "filter"},
	} {
		rec := post(h, "/api/v1/query/traces", tok, c.body)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.field) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(traces.queries) != 0 {
		t.Errorf("rejected requests reached the store: %d", len(traces.queries))
	}
	// trace 저장소가 없으면 404 (log 검색과 독립)
	if rec := post(searchHandler(t, k, &fakeLogs{}, &clock), "/api/v1/query/traces", tok, `{}`); rec.Code != 404 {
		t.Errorf("no trace store: %d", rec.Code)
	}
}

// environment로 제한된 key는 catalog의 서비스 범위를 mandatory predicate로 넘긴다(ADR 0039와 같다).
func TestSearchTracesEnvironmentScopedKey(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, []string{"prod"})
	clock := now
	traces := &fakeTraces{}
	h := traceHandler(t, k, traces, &fakeServices{envIDs: map[string][]string{"prod": {checkoutID}}}, &clock)
	if rec := post(h, "/api/v1/query/traces", tok, `{}`); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if fmt.Sprint(traces.queries[0].EnvironmentServices) != "["+checkoutID+"]" {
		t.Errorf("scope = %v", traces.queries[0].EnvironmentServices)
	}
}
