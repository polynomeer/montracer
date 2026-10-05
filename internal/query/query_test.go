package query

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

var (
	tenantA = mustTenant("11111111-1111-4111-8111-111111111111")
	now     = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
)

func mustTenant(s string) authz.TenantID {
	t, err := authz.ParseTenantID(s)
	if err != nil {
		panic(err)
	}
	return t
}

// keys는 테스트용 key 저장소다. 실제 KeyHasher로 발급·인증한다.
type keys struct {
	hasher authz.KeyHasher
	recs   map[string]authz.KeyRecord
}

func newKeys(t *testing.T) *keys {
	t.Helper()
	h, err := authz.NewKeyHasher(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return &keys{hasher: h, recs: map[string]authz.KeyRecord{}}
}

func (k *keys) issue(t *testing.T, kind authz.Kind, scopes []authz.Action, envs []string) string {
	t.Helper()
	g, err := k.hasher.Generate(kind, nil)
	if err != nil {
		t.Fatal(err)
	}
	k.recs[g.KeyID] = authz.KeyRecord{KeyID: g.KeyID, Tenant: tenantA, Kind: kind, Hash: g.Hash, Scopes: scopes,
		Environments: envs, IssuerRole: authz.RoleTenantAdmin, ExpiresAt: now.Add(time.Hour)}
	return g.Token
}

func (k *keys) authenticate(ctx context.Context, token string) (authz.Principal, error) {
	return k.hasher.Authenticate(ctx, token, authz.KindAPIKey, func(_ context.Context, id string) (authz.KeyRecord, error) {
		rec, ok := k.recs[id]
		if !ok {
			return authz.KeyRecord{}, authz.ErrKeyNotFound
		}
		return rec, nil
	}, now)
}

type fakeStore struct {
	recs []telemetrystore.SpanRecord
	err  error
	got  telemetrystore.TraceQuery
}

func (s *fakeStore) TraceSpanRecords(_ context.Context, p authz.Principal, q telemetrystore.TraceQuery, _ time.Time) ([]telemetrystore.SpanRecord, error) {
	s.got = q
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, err
	}
	return s.recs, s.err
}

type spanSpec struct {
	id, parent byte // 0 = 없음
	env        string
	name       string
	received   time.Time
}

func record(t *testing.T, sp spanSpec) telemetrystore.SpanRecord {
	t.Helper()
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	if sp.env != "" {
		rs.Resource().Attributes().PutStr("deployment.environment.name", sp.env)
	}
	s := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	var tid pcommon.TraceID
	b, _ := hex.DecodeString(traceID)
	copy(tid[:], b)
	s.SetTraceID(tid)
	s.SetSpanID(pcommon.SpanID{sp.id})
	if sp.parent != 0 {
		s.SetParentSpanID(pcommon.SpanID{sp.parent})
	}
	s.SetName(sp.name)
	s.SetKind(ptrace.SpanKindServer)
	s.Status().SetCode(ptrace.StatusCodeError)
	s.SetStartTimestamp(pcommon.NewTimestampFromTime(now.Add(-time.Minute)))
	s.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(-time.Minute + 20*time.Millisecond)))
	s.Attributes().PutInt("http.response.status_code", 500)
	s.Attributes().PutDouble("ratio", math.NaN())
	ev := s.Events().AppendEmpty()
	ev.SetName("exception")
	ev.Attributes().PutStr("exception.type", "java.lang.IllegalStateException")
	payload, err := (&ptrace.ProtoMarshaler{}).MarshalTraces(td)
	if err != nil {
		t.Fatal(err)
	}
	received := sp.received
	if received.IsZero() {
		received = now.Add(-30 * time.Second)
	}
	sid := pcommon.SpanID{sp.id}
	return telemetrystore.SpanRecord{
		Span: telemetrystore.Span{TraceID: traceID, SpanID: hex.EncodeToString(sid[:]), ServiceID: "5e000000-0000-4000-8000-000000000001",
			Name: sp.name, DurationNs: uint64(20 * time.Millisecond)},
		ReceivedAt: received,
		Payload:    payload,
	}
}

func newHandler(t *testing.T, k *keys, store Store) *Handler {
	t.Helper()
	h, err := NewHandler(Config{Authenticate: k.authenticate, Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

const validRange = "?from=2026-10-05T11:00:00Z&to=2026-10-05T12:00:00Z"

func get(h http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type errorBody struct {
	Error struct {
		Code      string           `json:"code"`
		RequestID string           `json:"request_id"`
		Details   map[string]any   `json:"details"`
		Fields    []map[string]any `json:"field_violations"`
	} `json:"error"`
}

func TestGetTrace(t *testing.T) {
	k := newKeys(t)
	token := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	store := &fakeStore{recs: []telemetrystore.SpanRecord{
		record(t, spanSpec{id: 1, env: "prod", name: "GET /cart", received: now.Add(-40 * time.Second)}),
		record(t, spanSpec{id: 2, parent: 1, env: "prod", name: "SELECT", received: now.Add(-10 * time.Second)}),
	}}
	h := newHandler(t, k, store)
	rec := get(h, "/api/v1/traces/"+traceID+validRange+"&service_id=5e000000-0000-4000-8000-000000000001", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("tenant data must not be cached")
	}
	if store.got.TraceID != traceID || store.got.ServiceID != "5e000000-0000-4000-8000-000000000001" ||
		!store.got.Range.From.Equal(now.Add(-time.Hour)) || store.got.Limit != telemetrystore.MaxTraceSpans {
		t.Errorf("query = %+v", store.got)
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	data := raw["data"].(map[string]any)
	if data["complete"] != true || len(data["reasons"].([]any)) != 0 || data["span_count"].(float64) != 2 {
		t.Errorf("data = %v", data)
	}
	if data["last_updated_at"] != "2026-10-05T11:59:50Z" {
		t.Errorf("last_updated_at = %v", data["last_updated_at"])
	}
	spans := data["spans"].([]any)
	root := spans[0].(map[string]any)
	if root["parent_span_id"] != nil || root["service_name"] != "checkout" || root["environment"] != "prod" ||
		root["kind"] != "server" || root["status_code"] != "error" || root["duration_ns"].(float64) != 2e7 {
		t.Errorf("root = %v", root)
	}
	attrs := root["attributes"].(map[string]any)
	if attrs["http.response.status_code"].(float64) != 500 || attrs["ratio"] != "NaN" {
		t.Errorf("attributes = %v (타입 보존, NaN은 문자열)", attrs)
	}
	if ev := root["events"].([]any); len(ev) != 1 || ev[0].(map[string]any)["name"] != "exception" {
		t.Errorf("events = %v", ev)
	}
	if spans[1].(map[string]any)["parent_span_id"] != "0100000000000000" {
		t.Errorf("child parent = %v", spans[1].(map[string]any)["parent_span_id"])
	}
	// 모르는 meta 값은 null이다(0·true로 채우지 않는다)
	meta := raw["meta"].(map[string]any)
	for _, k := range []string{"watermark", "sampled", "coverage", "resolution_seconds", "scan_bytes"} {
		if v, ok := meta[k]; !ok || v != nil {
			t.Errorf("meta.%s = %v, want null", k, v)
		}
	}
	if meta["request_id"] == "" || meta["schema_version"].(float64) != 1 || meta["partial"] != false {
		t.Errorf("meta = %v", meta)
	}
}

func TestCompleteness(t *testing.T) {
	k := newKeys(t)
	token := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	cases := []struct {
		name  string
		specs []spanSpec
		want  []string
	}{
		{"root만", []spanSpec{{id: 1}}, []string{}},
		{"root 없음", []spanSpec{{id: 2, parent: 1}}, []string{ReasonMissingParent, ReasonMissingRoot}},
		{"중간 parent 없음", []spanSpec{{id: 1}, {id: 3, parent: 2}}, []string{ReasonMissingParent}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var recs []telemetrystore.SpanRecord
			for _, sp := range c.specs {
				recs = append(recs, record(t, sp))
			}
			rec := get(newHandler(t, k, &fakeStore{recs: recs}), "/api/v1/traces/"+traceID+validRange, token)
			var body traceResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if strings.Join(body.Data.Reasons, ",") != strings.Join(c.want, ",") || body.Data.Complete != (len(c.want) == 0) {
				t.Errorf("reasons = %v complete = %v", body.Data.Reasons, body.Data.Complete)
			}
		})
	}
	// span 상한과 해석 불가 payload
	recs := []telemetrystore.SpanRecord{record(t, spanSpec{id: 1}), {Span: telemetrystore.Span{SpanID: "ff"}, Payload: []byte{0xff, 0xff}}}
	tr, ok := buildTrace(mustKeyPrincipal(t, k, token), traceID, recs, true)
	if !ok || tr.SpanCount != 1 || strings.Join(tr.Reasons, ",") != ReasonUndecodable+","+ReasonSpanLimit {
		t.Errorf("reasons = %v", tr.Reasons)
	}
}

func mustKeyPrincipal(t *testing.T, k *keys, token string) authz.Principal {
	t.Helper()
	p, err := k.authenticate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// environment가 제한된 key는 범위 밖 span을 볼 수 없고, 그 존재도 따로 알 수 없다 (D02 §13, D04 §02).
func TestEnvironmentScope(t *testing.T) {
	k := newKeys(t)
	staging := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, []string{"staging"})
	recs := []telemetrystore.SpanRecord{
		record(t, spanSpec{id: 1, env: "prod", name: "root"}),
		record(t, spanSpec{id: 2, parent: 1, env: "staging", name: "child"}),
		record(t, spanSpec{id: 3, parent: 1, name: "no-env"}),
	}
	h := newHandler(t, k, &fakeStore{recs: recs})
	rec := get(h, "/api/v1/traces/"+traceID+validRange, staging)
	var body traceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("status=%d err=%v", rec.Code, err)
	}
	if body.Data.SpanCount != 1 || body.Data.Spans[0].Name != "child" {
		t.Fatalf("visible = %+v", body.Data.Spans)
	}
	if strings.Contains(rec.Body.String(), `"name":"root"`) || strings.Contains(rec.Body.String(), `"name":"no-env"`) ||
		strings.Contains(rec.Body.String(), `"prod"`) {
		t.Errorf("범위 밖 span 내용이 응답에 있다: %s", rec.Body)
	}
	// 보이는 span이 하나도 없으면 존재하지 않는 trace와 같은 404
	onlyProd := newHandler(t, k, &fakeStore{recs: recs[:1]})
	if rec := get(onlyProd, "/api/v1/traces/"+traceID+validRange, staging); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	// 제한 없는 key는 environment가 없는 span도 본다
	all := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	if rec := get(h, "/api/v1/traces/"+traceID+validRange, all); !strings.Contains(rec.Body.String(), "no-env") {
		t.Error("unrestricted key must see spans without environment")
	}
}

func TestErrors(t *testing.T) {
	k := newKeys(t)
	token := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	noRead := k.issue(t, authz.KindAPIKey, []authz.Action{authz.DashboardsRead}, nil)
	ingestKey := k.issue(t, authz.KindIngestKey, []authz.Action{authz.IngestTraces}, []string{"prod"})
	ok := &fakeStore{recs: []telemetrystore.SpanRecord{record(t, spanSpec{id: 1})}}
	cases := []struct {
		name   string
		store  *fakeStore
		path   string
		token  string
		status int
		code   string
	}{
		{"no token", ok, "/api/v1/traces/" + traceID + validRange, "", 401, "UNAUTHENTICATED"},
		{"ingest key rejected", ok, "/api/v1/traces/" + traceID + validRange, ingestKey, 401, "UNAUTHENTICATED"},
		{"garbage token", ok, "/api/v1/traces/" + traceID + validRange, "mt_api_nope", 401, "UNAUTHENTICATED"},
		{"no telemetry.read", ok, "/api/v1/traces/" + traceID + validRange, noRead, 403, "FORBIDDEN"},
		{"missing from", ok, "/api/v1/traces/" + traceID + "?to=2026-10-05T12:00:00Z", token, 400, "INVALID_ARGUMENT"},
		{"bad to", ok, "/api/v1/traces/" + traceID + "?from=2026-10-05T11:00:00Z&to=yesterday", token, 400, "INVALID_ARGUMENT"},
		{"not found", &fakeStore{}, "/api/v1/traces/" + traceID + validRange, token, 404, "NOT_FOUND"},
		{"store unavailable", &fakeStore{err: unavailableErr{}}, "/api/v1/traces/" + traceID + validRange, token, 503, "UNAVAILABLE"},
		{"store invalid arg", &fakeStore{err: invalidErr{}}, "/api/v1/traces/xyz" + validRange, token, 400, "INVALID_ARGUMENT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := get(newHandler(t, k, c.store), c.path, c.token)
			var body errorBody
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != c.status || body.Error.Code != c.code || body.Error.RequestID == "" {
				t.Errorf("status=%d code=%s body=%s", rec.Code, body.Error.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "clickhouse") || strings.Contains(rec.Body.String(), "SELECT") {
				t.Error("내부 오류 문구가 응답에 있다")
			}
		})
	}
}

type unavailableErr struct{}

func (unavailableErr) Error() string     { return "clickhouse: connection refused" }
func (unavailableErr) Unavailable() bool { return true }

type invalidErr struct{}

func (invalidErr) Error() string { return "invalid trace_id" }
func (invalidErr) InvalidArgument() (string, string) {
	return "trace_id", "must be 32 lowercase hex characters"
}

func TestAuthBackendFailsClosed(t *testing.T) {
	h, err := NewHandler(Config{
		Authenticate: func(context.Context, string) (authz.Principal, error) {
			return authz.Principal{}, errors.Join(authz.ErrBackendUnavailable, errors.New("pg down"))
		},
		Store:  &fakeStore{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := get(h, "/api/v1/traces/"+traceID+validRange, "mt_api_x"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}
