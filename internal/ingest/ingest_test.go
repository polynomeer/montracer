package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetry/envelope"
	"github.com/polynomeer/montracer/internal/telemetry/redact"
)

// fixture 기준 시각 (tests/fixtures/otlp/README.md)
var fixtureTime = time.Unix(1791158400, 0).UTC()

var tenantA = func() authz.TenantID {
	t, _ := authz.ParseTenantID("11111111-1111-4111-8111-111111111111")
	return t
}()

type fakeProducer struct {
	mu      sync.Mutex
	err     error
	records []envelope.Record
}

func (f *fakeProducer) ProduceSync(_ context.Context, rs []envelope.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.records = append(f.records, rs...)
	return nil
}

// keyEnv: 실제 KeyHasher로 key를 만들고 메모리 저장소로 인증한다.
type keyEnv struct {
	hasher authz.KeyHasher
	store  map[string]authz.KeyRecord
}

func newKeyEnv(t *testing.T) *keyEnv {
	t.Helper()
	h, err := authz.NewKeyHasher(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return &keyEnv{hasher: h, store: map[string]authz.KeyRecord{}}
}

func (k *keyEnv) issue(t *testing.T, scopes []authz.Action, envs []string) string {
	t.Helper()
	g, err := k.hasher.Generate(authz.KindIngestKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	k.store[g.KeyID] = authz.KeyRecord{KeyID: g.KeyID, Tenant: tenantA, Kind: authz.KindIngestKey, Hash: g.Hash,
		Scopes: scopes, Environments: envs, ExpiresAt: fixtureTime.Add(24 * time.Hour)}
	return g.Token
}

func (k *keyEnv) auth(ctx context.Context, token string) (authz.Principal, error) {
	return k.hasher.Authenticate(ctx, token, authz.KindIngestKey, func(_ context.Context, id string) (authz.KeyRecord, error) {
		r, ok := k.store[id]
		if !ok {
			return authz.KeyRecord{}, authz.ErrKeyNotFound
		}
		return r, nil
	}, fixtureTime)
}

type setup struct {
	h      *Handler
	prod   *fakeProducer
	keys   *keyEnv
	logBuf *bytes.Buffer
}

func newSetup(t *testing.T) setup {
	t.Helper()
	keys := newKeyEnv(t)
	prod := &fakeProducer{}
	var buf bytes.Buffer
	h, err := NewHandler(Config{
		Authenticate: keys.auth,
		Producer:     prod,
		Redactor:     redact.New(redact.DefaultPolicy),
		RoutingEpoch: 1,
		Logger:       slog.New(slog.NewJSONHandler(&buf, nil)),
		Now:          func() time.Time { return fixtureTime.Add(time.Second) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return setup{h: h, prod: prod, keys: keys, logBuf: &buf}
}

func fixture(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", dir, name)) //nolint:gosec // 고정 fixture 경로
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func post(s setup, path, token, ct, ce string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", ct)
	if ce != "" {
		req.Header.Set("Content-Encoding", ce)
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec
}

var allSignals = []authz.Action{authz.IngestTraces, authz.IngestLogs, authz.IngestMetrics}

func TestTracesAcceptedAndEnveloped(t *testing.T) {
	s := newSetup(t)
	tok := s.keys.issue(t, allSignals, []string{"production"})
	rec := post(s, "/v1/traces", tok, "application/json", "", fixture(t, "otlp", "traces_checkout.json"))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	resp := ptraceotlp.NewExportResponse()
	if err := resp.UnmarshalJSON(rec.Body.Bytes()); err != nil || resp.PartialSuccess().RejectedSpans() != 0 {
		t.Fatalf("response = %s, %v", rec.Body.String(), err)
	}
	if len(s.prod.records) != 4 {
		t.Fatalf("produced = %d", len(s.prod.records))
	}
	for _, r := range s.prod.records {
		if r.Topic != envelope.TopicTraces || string(headerVal(r, envelope.HeaderTenant)) != tenantA.String() {
			t.Fatalf("record = %+v", r)
		}
	}
}

// payload의 tenant 속성·header는 무시하고 key의 tenant만 쓴다 (변경 불가 계약 1).
func TestTenantComesOnlyFromKey(t *testing.T) {
	s := newSetup(t)
	tok := s.keys.issue(t, allSignals, []string{"production"})
	td, _ := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(fixture(t, "otlp", "traces_checkout.json"))
	td.ResourceSpans().At(0).Resource().Attributes().PutStr("tenant_id", "22222222-2222-4222-8222-222222222222")
	body, _ := (&ptrace.ProtoMarshaler{}).MarshalTraces(td)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Tenant-ID", "22222222-2222-4222-8222-222222222222")
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, r := range s.prod.records {
		if string(headerVal(r, envelope.HeaderTenant)) != tenantA.String() {
			t.Fatal("tenant must come from the key")
		}
	}
}

// PII는 Kafka에 쓰기 전에 제거된다 (변경 불가 계약 3).
func TestRedactedBeforeProduce(t *testing.T) {
	s := newSetup(t)
	tok := s.keys.issue(t, allSignals, []string{"production"})
	rec := post(s, "/v1/logs", tok, "application/json", "", fixture(t, "pii", "logs_with_pii.json"))
	if rec.Code != 200 || len(s.prod.records) != 3 {
		t.Fatalf("status=%d produced=%d body=%s", rec.Code, len(s.prod.records), rec.Body.String())
	}
	for _, r := range s.prod.records {
		for _, secret := range []string{"900101-1234567", "4111 1111 1111 1111", "MTXCANARY05", "MTXCANARY17secret"} {
			if bytes.Contains(r.Value, []byte(secret)) {
				t.Fatalf("produced record contains %q", secret)
			}
		}
	}
}

func TestPartialSuccess(t *testing.T) {
	s := newSetup(t)
	// key는 production만 허용 → staging resource의 record는 거절
	tok := s.keys.issue(t, allSignals, []string{"production"})
	ld := plog.NewLogs()
	for _, env := range []string{"production", "staging", ""} {
		rl := ld.ResourceLogs().AppendEmpty()
		if env != "" {
			rl.Resource().Attributes().PutStr(EnvironmentAttr, env)
		}
		rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr("hello " + env)
	}
	body, _ := (&plog.ProtoMarshaler{}).MarshalLogs(ld)
	rec := post(s, "/v1/logs", tok, "application/x-protobuf", "", body)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	b := rec.Body.Bytes()
	// ExportLogsServiceResponse{partial_success=1{rejected_log_records=1, error_message=2}}
	ps := field(t, b, 1)
	if n := varintField(t, ps, 1); n != 2 {
		t.Errorf("rejected = %d, want 2", n)
	}
	if msg := string(field(t, ps, 2)); msg != "rejected environment_not_allowed=2" {
		t.Errorf("message = %q", msg)
	}
	if len(s.prod.records) != 1 {
		t.Errorf("produced = %d, want 1", len(s.prod.records))
	}
}

func TestAuthFailures(t *testing.T) {
	s := newSetup(t)
	tracesOnly := s.keys.issue(t, []authz.Action{authz.IngestTraces}, []string{"production"})
	body := fixture(t, "otlp", "logs_checkout.json")
	cases := []struct {
		name, token string
		want        int
	}{
		{"missing", "", 401},
		{"garbage", "not-a-key", 401},
		{"api key format", "mta_" + strings.Repeat("0", 16) + "_" + strings.Repeat("A", 43), 401},
		{"wrong signal", tracesOnly, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(s, "/v1/logs", tc.token, "application/json", "", body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
	if len(s.prod.records) != 0 {
		t.Fatal("nothing may be produced on auth failure")
	}
}

func TestAuthBackendUnavailableIsRetryable(t *testing.T) {
	s := newSetup(t)
	s.h.cfg.Authenticate = func(context.Context, string) (authz.Principal, error) {
		return authz.Principal{}, errors.Join(authz.ErrBackendUnavailable, errors.New("pg down"))
	}
	rec := post(s, "/v1/traces", "x", "application/json", "", []byte(`{}`))
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// ACK는 Kafka append가 모두 확인된 뒤에만 (ADR 0002). 실패하면 503 + Retry-After, 200을 주지 않는다.
func TestProduceFailureIsNotAcked(t *testing.T) {
	s := newSetup(t)
	tok := s.keys.issue(t, allSignals, []string{"production"})
	s.prod.err = errors.New("NOT_ENOUGH_REPLICAS")
	rec := post(s, "/v1/traces", tok, "application/x-protobuf", "", mustProto(t))
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d", rec.Code)
	}
	// google.rpc.Status{code=14 UNAVAILABLE}
	if code := varintField(t, rec.Body.Bytes(), 1); code != 14 {
		t.Errorf("rpc code = %d", code)
	}
	if strings.Contains(rec.Body.String(), "NOT_ENOUGH_REPLICAS") {
		t.Error("internal error leaked to client")
	}
}

func TestRequestLevelErrors(t *testing.T) {
	s := newSetup(t)
	tok := s.keys.issue(t, allSignals, []string{"production"})
	var bomb bytes.Buffer
	zw := gzip.NewWriter(&bomb)
	_, _ = zw.Write(bytes.Repeat([]byte("0"), 9<<20))
	_ = zw.Close()
	cases := []struct {
		name, ct, ce string
		body         []byte
		want         int
	}{
		{"text/plain", "text/plain", "", []byte("x"), 415},
		{"brotli", "application/json", "br", []byte("x"), 415},
		{"malformed json", "application/json", "", []byte(`{"resourceSpans":[`), 400},
		{"gzip bomb", "application/json", "gzip", bomb.Bytes(), 413},
		{"empty body", "application/x-protobuf", "", nil, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := post(s, "/v1/traces", tok, tc.ct, tc.ce, tc.body); rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestLogsNeverContainPayloadOrKey(t *testing.T) {
	s := newSetup(t)
	tok := s.keys.issue(t, allSignals, []string{"production"})
	post(s, "/v1/logs", tok, "application/json", "", fixture(t, "pii", "logs_with_pii.json"))
	post(s, "/v1/logs", "mti_0000000000000000_"+strings.Repeat("Z", 43), "application/json", "", []byte(`{}`))
	out := s.logBuf.String()
	for _, s := range []string{tok, "MTXCANARY", "Bearer", "900101"} {
		if strings.Contains(out, s) {
			t.Fatalf("log contains %q: %s", s, out)
		}
	}
	if !strings.Contains(out, `"accepted":3`) {
		t.Errorf("access log missing counts: %s", out)
	}
}

func mustProto(t *testing.T) []byte {
	t.Helper()
	td, err := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(fixture(t, "otlp", "traces_checkout.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := (&ptrace.ProtoMarshaler{}).MarshalTraces(td)
	return b
}

func headerVal(r envelope.Record, k string) []byte {
	for _, h := range r.Headers {
		if h.Key == k {
			return h.Value
		}
	}
	return nil
}

// field는 protobuf 메시지에서 length-delimited 필드 값을 찾는다.
func field(t *testing.T, b []byte, num protowire.Number) []byte {
	t.Helper()
	for len(b) > 0 {
		n, typ, l := protowire.ConsumeTag(b)
		b = b[l:]
		if typ == protowire.BytesType {
			v, l := protowire.ConsumeBytes(b)
			if n == num {
				return v
			}
			b = b[l:]
			continue
		}
		b = b[protowire.ConsumeFieldValue(n, typ, b):]
	}
	t.Fatalf("field %d not found", num)
	return nil
}

func varintField(t *testing.T, b []byte, num protowire.Number) uint64 {
	t.Helper()
	for len(b) > 0 {
		n, typ, l := protowire.ConsumeTag(b)
		b = b[l:]
		if typ == protowire.VarintType {
			v, l := protowire.ConsumeVarint(b)
			if n == num {
				return v
			}
			b = b[l:]
			continue
		}
		b = b[protowire.ConsumeFieldValue(n, typ, b):]
	}
	return 0
}

var _ = io.EOF
