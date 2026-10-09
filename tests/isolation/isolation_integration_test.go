//go:build integration

package isolation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controlapi"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/pipeline"
	"github.com/polynomeer/montracer/internal/query"
	"github.com/polynomeer/montracer/internal/telemetry/envelope"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// 환경 변수는 다른 통합 시험과 같다 (make test-isolation / make test-integration, CI 통합 job).
func env(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("%s 필요 (make test-isolation)", key)
	}
	return v
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func newUUID(t *testing.T) string {
	h := randomHex(t, 16)
	// version 4, variant 10
	b := []byte(h)
	b[12] = '4'
	b[16] = "89ab"[int(b[16])%4]
	return fmt.Sprintf("%s-%s-%s-%s-%s", b[0:8], b[8:12], b[12:16], b[16:20], b[20:32])
}

// stack은 한 시험의 두 API server와 저장소다.
type stack struct {
	db      *controldb.DB
	keys    *controldb.KeyStore
	hasher  authz.KeyHasher
	query   *httptest.Server
	control *httptest.Server
}

func newStack(t *testing.T) *stack {
	t.Helper()
	ctx := context.Background()
	db, err := controldb.Open(ctx, env(t, "MONTRACER_TEST_PG_APP_DSN"))
	if err != nil {
		t.Fatalf("control db: %v", err)
	}
	t.Cleanup(db.Close)
	pepper, _ := hex.DecodeString(randomHex(t, 32))
	hasher, err := authz.NewKeyHasher(pepper)
	if err != nil {
		t.Fatal(err)
	}
	keys := controldb.NewKeyStore(db)
	store, err := telemetrystore.OpenQuery(ctx, env(t, "MONTRACER_TEST_CH_QUERY_DSN"))
	if err != nil {
		t.Fatalf("query store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cursorKey, _ := hex.DecodeString(randomHex(t, 32))
	signer, err := apicursor.NewSigner(cursorKey, 15*time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	qh, err := query.NewHandler(query.Config{
		Authenticate: func(ctx context.Context, token string) (authz.Principal, error) {
			return hasher.Authenticate(ctx, token, authz.KindAPIKey, keys.LookupKey, time.Now())
		},
		Store: store, Metrics: store, MetricCatalog: store, Logs: store, Traces: store, Cursor: signer, Services: controldb.NewServiceStore(db),
	})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := controlapi.NewHandler(controlapi.Config{
		Authenticate: func(ctx context.Context, token string) (authz.Principal, error) {
			return hasher.Authenticate(ctx, token, authz.KindAPIKey, keys.LookupKey, time.Now())
		},
		Audit: controldb.NewAuditStore(db), Monitors: controldb.NewMonitorStore(db), Metrics: store, Cursor: signer,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &stack{db: db, keys: keys, hasher: hasher, query: httptest.NewServer(qh), control: httptest.NewServer(ch)}
	t.Cleanup(s.query.Close)
	t.Cleanup(s.control.Close)
	return s
}

// tenant는 provisioning된 tenant 하나와 그 admin principal이다.
type tenant struct {
	id    authz.TenantID
	admin authz.Principal
}

func (s *stack) newTenant(t *testing.T) tenant {
	t.Helper()
	ctx := context.Background()
	id := newUUID(t)
	tid, err := authz.ParseTenantID(id)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(ctx, env(t, "MONTRACER_TEST_PG_ADMIN_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()
	if _, err := admin.Exec(ctx, `INSERT INTO tenants (id, region, cell, status) VALUES ($1, 'local', 'cell-0', 'active')`, id); err != nil {
		t.Fatalf("provision tenant: %v", err)
	}
	err = s.db.WithTenant(ctx, tid, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, 'admin', 'tenant_admin')`, id)
		return err
	})
	if err != nil {
		t.Fatalf("membership: %v", err)
	}
	n := time.Now()
	p, err := authz.NewUserPrincipal(tid, "admin", authz.RoleTenantAdmin, n, n)
	if err != nil {
		t.Fatal(err)
	}
	return tenant{id: tid, admin: p}
}

// issue는 tenant admin이 key를 발급한다(감사 key.created가 함께 남는다).
func (s *stack) issue(t *testing.T, tn tenant, kind authz.Kind, scopes ...authz.Action) authz.GeneratedKey {
	t.Helper()
	var envs []string
	if kind == authz.KindIngestKey {
		envs = []string{"prod"}
	}
	iss, err := authz.ValidateKeyIssuance(tn.admin, kind, scopes, envs)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := s.hasher.Generate(kind, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.keys.CreateKey(context.Background(), iss, gen, time.Now().Add(time.Hour), ""); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	return gen
}

// issueEnv는 environment로 제한된 API key를 발급한다.
func (s *stack) issueEnv(t *testing.T, tn tenant, envs []string, scopes ...authz.Action) authz.GeneratedKey {
	t.Helper()
	iss, err := authz.ValidateKeyIssuance(tn.admin, authz.KindAPIKey, scopes, envs)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := s.hasher.Generate(authz.KindAPIKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.keys.CreateKey(context.Background(), iss, gen, time.Now().Add(time.Hour), ""); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	return gen
}

// writeTrace는 수집과 같은 경로(envelope → batch → ClickHouse sink, Kafka만 생략)로 trace 하나를 저장한다.
func writeTrace(t *testing.T, tn tenant, traceID string, at time.Time) {
	t.Helper()
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	rs.Resource().Attributes().PutStr("deployment.environment.name", "prod")
	ss := rs.ScopeSpans().AppendEmpty()
	var tid pcommon.TraceID
	b, _ := hex.DecodeString(traceID)
	copy(tid[:], b)
	var root pcommon.SpanID
	for i, name := range []string{"POST /checkout", "charge"} {
		sp := ss.Spans().AppendEmpty()
		var sid pcommon.SpanID
		sb, _ := hex.DecodeString(randomHex(t, 8))
		copy(sid[:], sb)
		sp.SetTraceID(tid)
		sp.SetSpanID(sid)
		if i == 0 {
			root = sid
		} else {
			sp.SetParentSpanID(root)
		}
		sp.SetName(name)
		sp.SetStartTimestamp(pcommon.NewTimestampFromTime(at.Add(time.Duration(i) * time.Millisecond)))
		sp.SetEndTimestamp(pcommon.NewTimestampFromTime(at.Add(time.Duration(i+5) * time.Millisecond)))
	}
	res, err := envelope.Traces(td, envelope.Meta{Tenant: tn.id, ReceivedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	writeRecords(t, res.Records)
}

// writeLog는 trace에 연결된 log 하나를 수집 경로로 저장한다.
func writeLog(t *testing.T, tn tenant, traceID, body string, at time.Time) {
	t.Helper()
	writeLogIn(t, tn, "prod", traceID, body, at)
}

// writeLogIn은 environment env의 checkout 서비스 log 하나를 저장한다.
func writeLogIn(t *testing.T, tn tenant, env, traceID, body string, at time.Time) {
	t.Helper()
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "checkout")
	rl.Resource().Attributes().PutStr("deployment.environment.name", env)
	lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	var tid pcommon.TraceID
	b, _ := hex.DecodeString(traceID)
	copy(tid[:], b)
	lr.SetTraceID(tid)
	lr.SetTimestamp(pcommon.NewTimestampFromTime(at))
	lr.SetSeverityNumber(plog.SeverityNumberError)
	lr.Body().SetStr(body)
	lr.Attributes().PutStr("log.record.uid", randomHex(t, 16))
	res, err := envelope.Logs(ld, envelope.Meta{Tenant: tn.id, ReceivedAt: at}, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeRecords(t, res.Records)
}

// writeRecords는 envelope record를 worker와 같은 batch·sink로 ClickHouse에 쓴다(Kafka만 생략).
func writeRecords(t *testing.T, records []envelope.Record) {
	t.Helper()
	var offset int64
	ob, _ := hex.DecodeString(randomHex(t, 3))
	for _, x := range ob {
		offset = offset<<8 | int64(x)
	}
	msgs := make([]pipeline.Message, len(records))
	for i, r := range records {
		msgs[i] = pipeline.Message{Topic: r.Topic, Partition: 0, Offset: offset + int64(i), Key: r.Key, Value: r.Value, Headers: r.Headers}
	}
	ctx := context.Background()
	sink, err := pipeline.OpenClickHouseSink(ctx, env(t, "MONTRACER_TEST_CH_INGEST_DSN"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.Close() }()
	for _, batch := range (pipeline.Builder{Retention: pipeline.DefaultRetention}).Build(msgs) {
		if err := sink.Write(ctx, batch); err != nil {
			t.Fatalf("sink: %v", err)
		}
	}
}

type apiResponse struct {
	status int
	body   map[string]any
	raw    string
}

func postJSON(t *testing.T, base, path, token, body string) apiResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Tenant-ID", "ignored") // tenant header는 의미가 없다
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	out := apiResponse{status: resp.StatusCode, raw: string(b)}
	_ = json.Unmarshal(b, &out.body)
	return out
}

// callPostJSON은 header를 더한 JSON POST다(tenant 위조 header 시험).
func callPostJSON(t *testing.T, base, path, token, body string, headers map[string]string) apiResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	out := apiResponse{status: resp.StatusCode, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func call(t *testing.T, base, path string, params url.Values, token string, headers map[string]string) apiResponse {
	t.Helper()
	u := base + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	out := apiResponse{status: resp.StatusCode, raw: string(b)}
	_ = json.Unmarshal(b, &out.body)
	return out
}

// errorShape는 request_id를 뺀 오류 응답이다. 다른 tenant의 ID와 없는 ID의 응답이 같아야 존재가 새지 않는다.
func errorShape(r apiResponse) string {
	e, _ := r.body["error"].(map[string]any)
	if e == nil {
		return fmt.Sprintf("%d %s", r.status, r.raw)
	}
	delete(e, "request_id")
	b, _ := json.Marshal(e)
	return fmt.Sprintf("%d %s", r.status, b)
}

func traceWindow(at time.Time) url.Values {
	return url.Values{"from": {at.Add(-time.Hour).Format(time.RFC3339)}, "to": {at.Add(time.Hour).Format(time.RFC3339)}}
}

// trace ID 재사용(IDOR): B는 A의 trace를 "없는 trace"와 똑같이 본다. header·query로 tenant를 바꿔도 마찬가지다.
func TestTraceIDReuseAcrossTenants(t *testing.T) {
	s := newStack(t)
	a, b := s.newTenant(t), s.newTenant(t)
	readA := s.issue(t, a, authz.KindAPIKey, authz.TelemetryRead)
	readB := s.issue(t, b, authz.KindAPIKey, authz.TelemetryRead)
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	traceA := randomHex(t, 16)
	writeTrace(t, a, traceA, at)

	// 대조군: A는 자기 trace를 본다(데이터가 실제로 있다는 증명 — 없으면 아래 시험은 공허하다).
	own := call(t, s.query.URL, "/api/v1/traces/"+traceA, traceWindow(at), readA.Token, nil)
	if own.status != 200 {
		t.Fatalf("tenant A own trace: %d %s", own.status, own.raw)
	}

	missing := call(t, s.query.URL, "/api/v1/traces/"+randomHex(t, 16), traceWindow(at), readB.Token, nil)
	if missing.status != 404 {
		t.Fatalf("unknown trace: %d %s", missing.status, missing.raw)
	}
	for name, h := range map[string]map[string]string{
		"plain":            nil,
		"X-Tenant-ID":      {"X-Tenant-ID": a.id.String()},
		"X-Scope-OrgID":    {"X-Scope-OrgID": a.id.String()},
		"Forwarded tenant": {"X-Forwarded-Tenant": a.id.String()},
	} {
		params := traceWindow(at)
		params.Set("tenant_id", a.id.String())
		got := call(t, s.query.URL, "/api/v1/traces/"+traceA, params, readB.Token, h)
		if errorShape(got) != errorShape(missing) {
			t.Errorf("%s: B reading A's trace = %s, want same as unknown trace %s", name, errorShape(got), errorShape(missing))
		}
		if strings.Contains(got.raw, traceA[:8]) && got.status == 200 {
			t.Errorf("%s: A's trace leaked to B", name)
		}
	}
}

// key 종류 재사용: ingest key는 조회·관리 API에 쓸 수 없고, 폐기된 key는 없는 key와 같은 응답이다.
func TestKeyKindAndRevokedKey(t *testing.T) {
	s := newStack(t)
	a := s.newTenant(t)
	ingest := s.issue(t, a, authz.KindIngestKey, authz.IngestTraces)
	read := s.issue(t, a, authz.KindAPIKey, authz.TelemetryRead, authz.AuditRead)
	at := time.Now().UTC()
	window := traceWindow(at)

	unknown := call(t, s.query.URL, "/api/v1/traces/"+randomHex(t, 16), window, "mta_"+randomHex(t, 8)+"_"+strings.Repeat("A", 43), nil)
	if unknown.status != 401 {
		t.Fatalf("unknown key: %d", unknown.status)
	}
	for name, base := range map[string]string{"query": s.query.URL + "/api/v1/traces/" + randomHex(t, 16), "control": s.control.URL + "/api/v1/audit-events"} {
		u, _ := url.Parse(base)
		got := call(t, u.Scheme+"://"+u.Host, u.Path, window, ingest.Token, nil)
		if got.status != 401 {
			t.Errorf("ingest key on %s API: %d %s", name, got.status, got.raw)
		}
	}
	if err := s.keys.RevokeKey(context.Background(), a.admin, read.KeyID, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	revoked := call(t, s.query.URL, "/api/v1/traces/"+randomHex(t, 16), window, read.Token, nil)
	if errorShape(revoked) != errorShape(unknown) {
		t.Errorf("revoked key = %s, want same as unknown key %s", errorShape(revoked), errorShape(unknown))
	}
}

func auditWindow() url.Values {
	n := time.Now().UTC()
	return url.Values{"from": {n.Add(-time.Hour).Format(time.RFC3339)}, "to": {n.Add(time.Minute).Format(time.RFC3339)}}
}

func auditResourceIDs(t *testing.T, r apiResponse) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	data, _ := r.body["data"].([]any)
	for _, d := range data {
		m, _ := d.(map[string]any)
		if id, ok := m["resource_id"].(string); ok {
			out[id] = true
		}
	}
	return out
}

// 감사: B는 A의 감사 행을 보지 못하고, A의 cursor를 B·다른 권한으로 재사용할 수 없다.
func TestAuditAndCursorReuseAcrossTenants(t *testing.T) {
	s := newStack(t)
	a, b := s.newTenant(t), s.newTenant(t)
	auditA := s.issue(t, a, authz.KindAPIKey, authz.AuditRead)
	opsA := s.issue(t, a, authz.KindAPIKey, authz.AuditOperationsRead)
	extra := s.issue(t, a, authz.KindAPIKey, authz.TelemetryRead) // A의 key.created 감사가 여러 건이 되게
	auditB := s.issue(t, b, authz.KindAPIKey, authz.AuditRead)

	ownA := call(t, s.control.URL, "/api/v1/audit-events", auditWindow(), auditA.Token, nil)
	idsA := auditResourceIDs(t, ownA)
	if ownA.status != 200 || !idsA[extra.KeyID] || !idsA[auditA.KeyID] {
		t.Fatalf("tenant A own audit: %d %s", ownA.status, ownA.raw)
	}
	ownB := call(t, s.control.URL, "/api/v1/audit-events", auditWindow(), auditB.Token, nil)
	for id := range auditResourceIDs(t, ownB) {
		if idsA[id] {
			t.Errorf("tenant B sees A's audit row for %s", id)
		}
	}

	params := auditWindow()
	params.Set("limit", "1")
	first := call(t, s.control.URL, "/api/v1/audit-events", params, auditA.Token, nil)
	cur, _ := first.body["next_cursor"].(string)
	if cur == "" {
		t.Fatalf("no cursor from A: %s", first.raw)
	}
	params.Set("cursor", cur)
	for name, token := range map[string]string{"tenant B": auditB.Token, "A operations-only key": opsA.Token} {
		got := call(t, s.control.URL, "/api/v1/audit-events", params, token, nil)
		e, _ := got.body["error"].(map[string]any)
		if got.status != 400 || e["code"] != "INVALID_ARGUMENT" {
			t.Errorf("%s replaying A's cursor: %d %s", name, got.status, got.raw)
		}
	}
	// 대조군: 같은 cursor를 A의 같은 key로는 이어 읽는다
	if next := call(t, s.control.URL, "/api/v1/audit-events", params, auditA.Token, nil); next.status != 200 {
		t.Errorf("A continuing own cursor: %d %s", next.status, next.raw)
	}
	// 운영 범주만 가진 key는 security 감사(key.created)를 보지 못한다
	ops := call(t, s.control.URL, "/api/v1/audit-events", auditWindow(), opsA.Token, nil)
	if ops.status != 200 || len(auditResourceIDs(t, ops)) != 0 {
		t.Errorf("operations-only key sees security audit: %d %s", ops.status, ops.raw)
	}
}

// log 검색: B는 A의 log를 같은 filter(A의 trace_id)로 찾아도 빈 결과이고, A의 cursor를 쓸 수 없다.
func TestLogSearchAcrossTenants(t *testing.T) {
	s := newStack(t)
	a, b := s.newTenant(t), s.newTenant(t)
	readA := s.issue(t, a, authz.KindAPIKey, authz.TelemetryRead)
	readB := s.issue(t, b, authz.KindAPIKey, authz.TelemetryRead)
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	traceA := randomHex(t, 16)
	for i := 0; i < 3; i++ {
		writeLog(t, a, traceA, fmt.Sprintf("tenant-a-secret-%d", i), at.Add(time.Duration(i)*time.Millisecond))
	}
	body := fmt.Sprintf(`{"range":{"from":%q,"to":%q},"filter":{"field":"trace_id","op":"eq","value":%q},"limit":1}`,
		at.Add(-time.Minute).Format(time.RFC3339Nano), at.Add(time.Minute).Format(time.RFC3339Nano), traceA)

	// 대조군: A는 자기 log를 trace_id로 찾는다(log↔trace 연결)
	own := postJSON(t, s.query.URL, "/api/v1/query/logs", readA.Token, body)
	data, _ := own.body["data"].([]any)
	cur, _ := own.body["next_cursor"].(string)
	if own.status != 200 || len(data) != 1 || cur == "" || !strings.Contains(own.raw, "tenant-a-secret") {
		t.Fatalf("tenant A own logs: %d %s", own.status, own.raw)
	}
	// B: 같은 filter로도 빈 결과(없는 것과 같다)
	other := postJSON(t, s.query.URL, "/api/v1/query/logs", readB.Token, body)
	odata, _ := other.body["data"].([]any)
	if other.status != 200 || len(odata) != 0 || strings.Contains(other.raw, "tenant-a-secret") || other.body["next_cursor"] != nil {
		t.Errorf("tenant B searching A's trace logs: %d %s", other.status, other.raw)
	}
	// B가 A의 cursor를 재사용 → 400
	replay := postJSON(t, s.query.URL, "/api/v1/query/logs", readB.Token, strings.TrimSuffix(body, "}")+fmt.Sprintf(`,"cursor":%q}`, cur))
	if replay.status != 400 || strings.Contains(replay.raw, "tenant-a-secret") {
		t.Errorf("tenant B replaying A's cursor: %d %s", replay.status, replay.raw)
	}
}

// trace 검색(ADR 0043): B는 A가 쓴 trace_id로 검색해도 빈 결과이고, A의 cursor를 쓸 수 없다.
// 같은 trace_id를 B도 쓰면 B는 자기 span만 요약해 본다(A의 span 수·이름이 섞이지 않는다).
func TestTraceSearchAcrossTenants(t *testing.T) {
	s := newStack(t)
	a, b := s.newTenant(t), s.newTenant(t)
	readA := s.issue(t, a, authz.KindAPIKey, authz.TelemetryRead)
	readB := s.issue(t, b, authz.KindAPIKey, authz.TelemetryRead)
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	traceA, traceA2 := randomHex(t, 16), randomHex(t, 16)
	writeTrace(t, a, traceA, at)
	writeTrace(t, a, traceA2, at.Add(time.Millisecond))
	rng := fmt.Sprintf(`"range":{"from":%q,"to":%q}`, at.Add(-time.Minute).Format(time.RFC3339Nano), at.Add(time.Minute).Format(time.RFC3339Nano))
	byID := fmt.Sprintf(`{%s,"filter":{"field":"trace_id","op":"eq","value":%q}}`, rng, traceA)

	// 대조군: A는 자기 trace를 찾고 요약(span 2개, root 이름)을 받는다
	own := postJSON(t, s.query.URL, "/api/v1/query/traces", readA.Token, byID)
	data, _ := own.body["data"].([]any)
	if own.status != 200 || len(data) != 1 || !strings.Contains(own.raw, `"span_count":2`) || !strings.Contains(own.raw, "POST /checkout") {
		t.Fatalf("tenant A own trace search: %d %s", own.status, own.raw)
	}
	// B: 같은 filter(위조 header 포함)로도 빈 결과
	for name, h := range map[string]map[string]string{"plain": nil, "X-Tenant-ID": {"X-Tenant-ID": a.id.String()}} {
		other := callPostJSON(t, s.query.URL, "/api/v1/query/traces", readB.Token, byID, h)
		odata, _ := other.body["data"].([]any)
		if other.status != 200 || len(odata) != 0 || strings.Contains(other.raw, traceA) || other.body["next_cursor"] != nil {
			t.Errorf("%s: tenant B searching A's trace: %d %s", name, other.status, other.raw)
		}
	}
	// B가 A의 cursor를 재사용 → 400
	page := postJSON(t, s.query.URL, "/api/v1/query/traces", readA.Token, fmt.Sprintf(`{%s,"limit":1}`, rng))
	cur, _ := page.body["next_cursor"].(string)
	if page.status != 200 || cur == "" {
		t.Fatalf("tenant A first page: %d %s", page.status, page.raw)
	}
	replay := postJSON(t, s.query.URL, "/api/v1/query/traces", readB.Token, fmt.Sprintf(`{%s,"limit":1,"cursor":%q}`, rng, cur))
	if replay.status != 400 || strings.Contains(replay.raw, traceA) || strings.Contains(replay.raw, traceA2) {
		t.Errorf("tenant B replaying A's cursor: %d %s", replay.status, replay.raw)
	}
	// 같은 trace_id를 B도 쓰면 B의 요약에는 B의 span만 있다
	writeTrace(t, b, traceA, at)
	bOwn := postJSON(t, s.query.URL, "/api/v1/query/traces", readB.Token, byID)
	bdata, _ := bOwn.body["data"].([]any)
	if bOwn.status != 200 || len(bdata) != 1 || !strings.Contains(bOwn.raw, `"span_count":2`) {
		t.Errorf("tenant B own trace with the same id: %d %s", bOwn.status, bOwn.raw)
	}
}

// 서비스 catalog: B는 A의 서비스를 목록에서 보지 않는다(이름·ID 모두).
func TestServiceCatalogAcrossTenants(t *testing.T) {
	s := newStack(t)
	a, b := s.newTenant(t), s.newTenant(t)
	readA := s.issue(t, a, authz.KindAPIKey, authz.TelemetryRead)
	readB := s.issue(t, b, authz.KindAPIKey, authz.TelemetryRead)
	secretName := "tenant-a-secret-" + randomHex(t, 4)
	sid := newUUID(t)
	if _, err := controldb.NewServiceStore(s.db).Observe(context.Background(), a.id, []controldb.ServiceObservation{
		{ServiceID: sid, Environment: "prod", Name: secretName, SeenAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	own := call(t, s.query.URL, "/api/v1/services", nil, readA.Token, nil)
	if own.status != 200 || !strings.Contains(own.raw, secretName) {
		t.Fatalf("tenant A own services: %d %s", own.status, own.raw)
	}
	other := call(t, s.query.URL, "/api/v1/services", nil, readB.Token, map[string]string{"X-Tenant-ID": a.id.String()})
	if other.status != 200 || strings.Contains(other.raw, secretName) || strings.Contains(other.raw, sid) {
		t.Errorf("tenant B sees A's service: %d %s", other.status, other.raw)
	}
	// 단건 조회(ADR 0042): A는 자기 서비스를 받고, B는 A의 id를 넣어도(X-Tenant-ID 위조 포함) 없는 id와 같은 404다.
	if got := call(t, s.query.URL, "/api/v1/services/"+sid, nil, readA.Token, nil); got.status != 200 || !strings.Contains(got.raw, secretName) {
		t.Errorf("tenant A own service: %d %s", got.status, got.raw)
	}
	cross := call(t, s.query.URL, "/api/v1/services/"+sid, nil, readB.Token, map[string]string{"X-Tenant-ID": a.id.String()})
	missing := call(t, s.query.URL, "/api/v1/services/"+newUUID(t), nil, readB.Token, nil)
	if cross.status != 404 || missing.status != 404 || strings.Contains(cross.raw, secretName) {
		t.Errorf("tenant B reading A's service by id: %d %s (missing: %d)", cross.status, cross.raw, missing.status)
	}
	if code := func(r apiResponse) any {
		e, _ := r.body["error"].(map[string]any)
		return e["code"]
	}; code(cross) != code(missing) || code(cross) != "NOT_FOUND" {
		t.Errorf("cross-tenant 404 differs from missing id: %v vs %v", code(cross), code(missing))
	}
}

// log 검색의 service.name과 environment 제한 key(ADR 0039):
// B가 같은 이름의 자기 서비스를 가져도 A의 log는 보지 않고, A의 staging 제한 key는 staging log만 본다(prod log 제외).
// 대조군(staging log·B의 자기 log)이 보여야 "범위가 비어서 0행"이 아니라 "범위 밖이라 제외"임을 증명한다.
func TestLogSearchByServiceAndEnvironment(t *testing.T) {
	s := newStack(t)
	a, b := s.newTenant(t), s.newTenant(t)
	readB := s.issue(t, b, authz.KindAPIKey, authz.TelemetryRead)
	prodA := s.issueEnv(t, a, []string{"prod"}, authz.TelemetryRead)
	stagingA := s.issueEnv(t, a, []string{"staging"}, authz.TelemetryRead)
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	writeLogIn(t, a, "prod", randomHex(t, 16), "tenant-a-prod-secret", at)
	writeLogIn(t, a, "staging", randomHex(t, 16), "tenant-a-staging-log", at.Add(time.Millisecond))
	writeLogIn(t, b, "prod", randomHex(t, 16), "tenant-b-own-log", at)
	store := controldb.NewServiceStore(s.db)
	register := func(tn tenant, env string) {
		t.Helper()
		id := envelope.ServiceIDOf(tn.id, envelope.ServiceKey{Environment: env, Name: "checkout"})
		if _, err := store.Observe(context.Background(), tn.id, []controldb.ServiceObservation{
			{ServiceID: id, Environment: env, Name: "checkout", SeenAt: time.Now()}}); err != nil {
			t.Fatal(err)
		}
	}
	register(a, "prod")
	register(a, "staging")
	register(b, "prod")
	rng := fmt.Sprintf(`"range":{"from":%q,"to":%q}`, at.Add(-time.Minute).Format(time.RFC3339Nano), at.Add(time.Minute).Format(time.RFC3339Nano))
	byName := `{` + rng + `,"filter":{"field":"service.name","op":"eq","value":"Checkout"}}`
	noFilter := `{` + rng + `}`
	search := func(tok, body string) (int, string, int) {
		t.Helper()
		r := postJSON(t, s.query.URL, "/api/v1/query/logs", tok, body)
		data, _ := r.body["data"].([]any)
		return r.status, r.raw, len(data)
	}

	// prod 제한 key: 이름(대소문자 무시)으로 prod log만
	if st, raw, n := search(prodA.Token, byName); st != 200 || n != 1 || !strings.Contains(raw, "tenant-a-prod-secret") || strings.Contains(raw, "staging-log") {
		t.Fatalf("tenant A prod key: %d %s", st, raw)
	}
	// staging 제한 key: staging log는 보이고(대조군) prod log는 보이지 않는다 — filter 유무 모두
	for _, q := range []string{byName, noFilter} {
		if st, raw, n := search(stagingA.Token, q); st != 200 || n != 1 || !strings.Contains(raw, "tenant-a-staging-log") || strings.Contains(raw, "tenant-a-prod-secret") {
			t.Errorf("staging key: %d %s", st, raw)
		}
	}
	// B: 같은 이름으로 자기 log는 찾고(대조군) A의 log는 없다
	if st, raw, n := search(readB.Token, byName); st != 200 || n != 1 || !strings.Contains(raw, "tenant-b-own-log") || strings.Contains(raw, "tenant-a-") {
		t.Errorf("tenant B by service name: %d %s", st, raw)
	}
}

// writeMetric1m은 metric 하나의 1분 rollup 결과 행을 쓴다. 사전 API(ADR 0046)는 metric_1m만 읽으므로
// rollup 주기(watermark 2분)를 기다리지 않고 결과 행을 admin 계정으로 직접 넣는다. 격리는 읽기 경로(query 계정·row policy·API)에서 본다.
func writeMetric1m(t *testing.T, tn tenant, name, labelKey string, at time.Time) {
	t.Helper()
	opts, err := clickhouse.ParseDSN(env(t, "MONTRACER_TEST_CH_ADMIN_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx := context.Background()
	b, err := conn.PrepareBatch(ctx, `INSERT INTO metric_1m (tenant_id, metric_name, stream_id, window_start,
		type, temporality, is_monotonic, unit, resource_json, attributes_json, samples, has_value, last, min, max, total,
		has_increase, increase, has_histogram, count, hist_sum, bounds, buckets, resets, flags, partial, revision,
		computed_at, expires_at)`)
	if err != nil {
		t.Fatal(err)
	}
	stream, _ := hex.DecodeString(randomHex(t, 16))
	w := at.UTC().Truncate(time.Minute)
	if err := b.Append(tn.id.String(), name, string(stream), w, "gauge", "unspecified", false, "1",
		`{"service.name":"checkout","deployment.environment.name":"prod"}`, `{"`+labelKey+`":"v"}`,
		uint32(1), true, 1.0, 1.0, 1.0, 1.0, false, 0.0, false, uint64(0), 0.0, []float64{}, []uint64{}, uint32(0), []string{}, false, uint64(1),
		time.Now(), w.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := b.Send(); err != nil {
		t.Fatal(err)
	}
}

// metric 사전(ADR 0046): B는 A의 metric 이름·label key를 보지 못하고(tenant header 무시), A의 사전 cursor를 쓸 수 없다.
func TestMetricCatalogAcrossTenants(t *testing.T) {
	s := newStack(t)
	a, b := s.newTenant(t), s.newTenant(t)
	readA := s.issue(t, a, authz.KindAPIKey, authz.TelemetryRead)
	readB := s.issue(t, b, authz.KindAPIKey, authz.TelemetryRead)
	at := time.Now().UTC().Add(-5 * time.Minute)
	tag := randomHex(t, 6)
	nameA1, nameA2, key := "tenant.a.secret."+tag+".one", "tenant.a.secret."+tag+".two", "tenant.a.key."+tag
	writeMetric1m(t, a, nameA1, key, at)
	writeMetric1m(t, a, nameA2, key, at)
	rng := url.Values{"from": {at.Add(-time.Hour).Format(time.RFC3339)}, "to": {at.Add(time.Hour).Format(time.RFC3339)}, "q": {tag}, "limit": {"1"}}

	// 대조군: A는 자기 metric을 보고 다음 page cursor를 받는다
	own := call(t, s.query.URL, "/api/v1/metrics", rng, readA.Token, nil)
	cur, _ := own.body["next_cursor"].(string)
	if own.status != 200 || !strings.Contains(own.raw, nameA1) || cur == "" {
		t.Fatalf("tenant A own catalog: %d %s", own.status, own.raw)
	}
	// B: tenant header로 A를 지정해도 빈 사전
	other := call(t, s.query.URL, "/api/v1/metrics", rng, readB.Token, map[string]string{"X-Tenant-ID": a.id.String()})
	odata, _ := other.body["data"].([]any)
	if other.status != 200 || len(odata) != 0 || strings.Contains(other.raw, "tenant.a.secret") {
		t.Errorf("tenant B catalog: %d %s", other.status, other.raw)
	}
	// B가 A의 cursor를 재사용 → 400, A의 이름이 새지 않는다
	replay := url.Values{}
	for k, v := range rng {
		replay[k] = v
	}
	replay.Set("cursor", cur)
	if r := call(t, s.query.URL, "/api/v1/metrics", replay, readB.Token, nil); r.status != 400 || strings.Contains(r.raw, "tenant.a.secret") {
		t.Errorf("tenant B replaying A's catalog cursor: %d %s", r.status, r.raw)
	}
	// label key: A는 보고 B는 같은 metric 이름으로도 빈 목록
	lq := url.Values{"metric": {nameA1}, "from": rng["from"], "to": rng["to"]}
	if r := call(t, s.query.URL, "/api/v1/metrics/labels", lq, readA.Token, nil); r.status != 200 || !strings.Contains(r.raw, key) {
		t.Fatalf("tenant A own labels: %d %s", r.status, r.raw)
	}
	if r := call(t, s.query.URL, "/api/v1/metrics/labels", lq, readB.Token, map[string]string{"X-Tenant-ID": a.id.String()}); r.status != 200 || strings.Contains(r.raw, key) {
		t.Errorf("tenant B labels of A's metric: %d %s", r.status, r.raw)
	}
}

// callMethod는 method·header를 지정한 요청이다(PUT·DELETE 시험).
func callMethod(t *testing.T, method, url, token, body string, headers map[string]string) apiResponse {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	out := apiResponse{status: resp.StatusCode, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// monitor(ADR 0049): B는 A의 monitor를 보거나 바꾸거나 지울 수 없고(없는 monitor와 같은 404, tenant header 무시),
// 같은 Idempotency-Key를 써도 A의 응답이 재생되지 않으며, A의 목록 cursor를 쓸 수 없다.
func TestMonitorsAcrossTenants(t *testing.T) {
	s := newStack(t)
	a, b := s.newTenant(t), s.newTenant(t)
	writeA := s.issue(t, a, authz.KindAPIKey, authz.MonitorsRead, authz.MonitorsWrite)
	writeB := s.issue(t, b, authz.KindAPIKey, authz.MonitorsRead, authz.MonitorsWrite)
	secret := "tenant-a-secret-monitor-" + randomHex(t, 4)
	body := func(name string) string {
		return `{"name":"` + name + `","query":{"kind":"error_ratio"},"window_seconds":300,"evaluation_seconds":60,` +
			`"condition":{"operator":"gt","threshold":0.02},"for_seconds":120,"no_data":{"action":"no_data"}}`
	}
	// 대조군: A는 만들고 읽는다
	for i := 0; i < 2; i++ {
		r := callPostJSON(t, s.control.URL, "/api/v1/monitors", writeA.Token, body(fmt.Sprintf("%s-%d", secret, i)), map[string]string{"Idempotency-Key": fmt.Sprintf("k%d", i)})
		if r.status != 201 {
			t.Fatalf("A create: %d %s", r.status, r.raw)
		}
	}
	listA := call(t, s.control.URL, "/api/v1/monitors", url.Values{"limit": {"1"}}, writeA.Token, nil)
	data, _ := listA.body["data"].([]any)
	cur, _ := listA.body["next_cursor"].(string)
	if listA.status != 200 || len(data) != 1 || cur == "" {
		t.Fatalf("A list: %d %s", listA.status, listA.raw)
	}
	id, _ := data[0].(map[string]any)["id"].(string)

	// B: 목록에 없음(tenant header 무시), 단건·수정·삭제는 없는 monitor와 같은 404
	listB := call(t, s.control.URL, "/api/v1/monitors", nil, writeB.Token, map[string]string{"X-Tenant-ID": a.id.String()})
	if listB.status != 200 || strings.Contains(listB.raw, secret) {
		t.Errorf("B list: %d %s", listB.status, listB.raw)
	}
	missing := newUUID(t)
	for _, c := range []struct{ method, body string }{
		{http.MethodGet, ""}, {http.MethodPut, body("hijack")}, {http.MethodDelete, ""},
	} {
		h := map[string]string{"If-Match": `"1"`, "X-Tenant-ID": a.id.String()}
		cross := callMethod(t, c.method, s.control.URL+"/api/v1/monitors/"+id, writeB.Token, c.body, h)
		none := callMethod(t, c.method, s.control.URL+"/api/v1/monitors/"+missing, writeB.Token, c.body, h)
		if cross.status != 404 || errorShape(cross) != errorShape(none) || strings.Contains(cross.raw, secret) {
			t.Errorf("B %s A's monitor: %s vs missing %s", c.method, errorShape(cross), errorShape(none))
		}
	}
	if got := callMethod(t, http.MethodGet, s.control.URL+"/api/v1/monitors/"+id, writeA.Token, "", nil); got.status != 200 || !strings.Contains(got.raw, secret) {
		t.Errorf("A's monitor changed by B attempts: %d %s", got.status, got.raw)
	}
	// B가 A와 같은 Idempotency-Key·같은 본문을 써도 B의 새 monitor다(A의 응답 재생 아님)
	r := callPostJSON(t, s.control.URL, "/api/v1/monitors", writeB.Token, body(secret+"-0"), map[string]string{"Idempotency-Key": "k0"})
	if r.status != 201 || r.raw == "" || strings.Contains(r.raw, id) {
		t.Errorf("B same idempotency key: %d %s", r.status, r.raw)
	}
	// B가 A의 목록 cursor를 재사용 → 400
	if got := call(t, s.control.URL, "/api/v1/monitors", url.Values{"cursor": {cur}}, writeB.Token, nil); got.status != 400 || strings.Contains(got.raw, secret) {
		t.Errorf("B replaying A's cursor: %d %s", got.status, got.raw)
	}
}

// writeRequests1m은 http.server.request.duration histogram의 1분 rollup 행(status별 요청 수)을 쓴다(writeMetric1m과 같은 이유로 직접).
func writeRequests1m(t *testing.T, tn tenant, from, to time.Time, perMinute map[string]uint64) {
	t.Helper()
	opts, err := clickhouse.ParseDSN(env(t, "MONTRACER_TEST_CH_ADMIN_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx := context.Background()
	b, err := conn.PrepareBatch(ctx, `INSERT INTO metric_1m (tenant_id, metric_name, stream_id, window_start,
		type, temporality, is_monotonic, unit, resource_json, attributes_json, samples, has_value, last, min, max, total,
		has_increase, increase, has_histogram, count, hist_sum, bounds, buckets, resets, flags, partial, revision,
		computed_at, expires_at)`)
	if err != nil {
		t.Fatal(err)
	}
	for status, n := range perMinute {
		stream, _ := hex.DecodeString(randomHex(t, 16))
		for w := from.UTC().Truncate(time.Minute); w.Before(to); w = w.Add(time.Minute) {
			if err := b.Append(tn.id.String(), "http.server.request.duration", string(stream), w, "histogram", "cumulative", true, "s",
				`{"service.name":"checkout","deployment.environment.name":"prod"}`, `{"http.response.status_code":"`+status+`"}`,
				uint32(1), false, 0.0, 0.0, 0.0, 0.0, false, 0.0, true, n, float64(n)*0.1, []float64{0.5}, []uint64{n, 0}, uint32(0), []string{}, false, uint64(1),
				time.Now(), w.Add(24*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := b.Send(); err != nil {
		t.Fatal(err)
	}
}

// monitor dry-run(ADR 0052): validate는 요청자 tenant의 telemetry만 다시 평가한다. 같은 서비스 이름·같은 시간대에 A는 오류 10%,
// B는 0%다. B의 dry-run은 0%만 보고(tenant header 무시), A의 dry-run은 같은 정의로 발화한다(대조군).
func TestMonitorDryRunAcrossTenants(t *testing.T) {
	s := newStack(t)
	a, b := s.newTenant(t), s.newTenant(t)
	keyA := s.issue(t, a, authz.KindAPIKey, authz.MonitorsWrite, authz.TelemetryRead)
	keyB := s.issue(t, b, authz.KindAPIKey, authz.MonitorsWrite, authz.TelemetryRead)
	end := time.Now().UTC().Truncate(time.Minute).Add(-5 * time.Minute)
	writeRequests1m(t, a, end.Add(-30*time.Minute), end, map[string]uint64{"200": 900, "500": 100})
	writeRequests1m(t, b, end.Add(-30*time.Minute), end, map[string]uint64{"200": 1000}) // B는 오류 0%
	body := `{"name":"dry-run","query":{"kind":"error_ratio","group_by":["service.name"]},"window_seconds":300,"evaluation_seconds":60,` +
		`"condition":{"operator":"gt","threshold":0.02},"for_seconds":0,"no_data":{"action":"no_data"}}`
	dryRun := func(token string, headers map[string]string) map[string]any {
		t.Helper()
		r := callPostJSON(t, s.control.URL, "/api/v1/monitors/validate", token, body, headers)
		data, _ := r.body["data"].(map[string]any)
		dr, _ := data["dry_run"].(map[string]any)
		if r.status != 200 || dr == nil {
			t.Fatalf("validate: %d %s", r.status, r.raw)
		}
		return dr
	}
	drA := dryRun(keyA.Token, nil)
	if groups, _ := drA["groups"].([]any); drA["groups_total"] != 1.0 || len(groups) != 1 ||
		len(groups[0].(map[string]any)["firing_intervals"].([]any)) != 1 {
		t.Fatalf("A dry-run: %+v", drA)
	}
	// B: 자기 데이터(0%)만 본다 — A의 오류 10%가 섞이면 max_value가 0보다 크고 발화한다(tenant header 무시)
	drB := dryRun(keyB.Token, map[string]string{"X-Tenant-ID": a.id.String()})
	groupsB, _ := drB["groups"].([]any)
	if drB["groups_total"] != 1.0 || len(groupsB) != 1 {
		t.Fatalf("B dry-run: %+v", drB)
	}
	gB := groupsB[0].(map[string]any)
	if gB["max_value"] != 0.0 || len(gB["firing_intervals"].([]any)) != 0 {
		t.Errorf("B dry-run sees A's data: %+v", gB)
	}
}
