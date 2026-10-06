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

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/collector/pdata/pcommon"
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

	qh, err := query.NewHandler(query.Config{
		Authenticate: func(ctx context.Context, token string) (authz.Principal, error) {
			return hasher.Authenticate(ctx, token, authz.KindAPIKey, keys.LookupKey, time.Now())
		},
		Store: store, Metrics: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	cursorKey, _ := hex.DecodeString(randomHex(t, 32))
	signer, err := apicursor.NewSigner(cursorKey, 15*time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := controlapi.NewHandler(controlapi.Config{
		Authenticate: func(ctx context.Context, token string) (authz.Principal, error) {
			return hasher.Authenticate(ctx, token, authz.KindAPIKey, keys.LookupKey, time.Now())
		},
		Audit: controldb.NewAuditStore(db), Cursor: signer,
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
	var offset int64
	ob, _ := hex.DecodeString(randomHex(t, 3))
	for _, x := range ob {
		offset = offset<<8 | int64(x)
	}
	msgs := make([]pipeline.Message, len(res.Records))
	for i, r := range res.Records {
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
