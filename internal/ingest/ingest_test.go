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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/catalog"
	"github.com/polynomeer/montracer/internal/quota"
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
	return k.issueFor(t, tenantA, scopes, envs)
}

func (k *keyEnv) issueFor(t *testing.T, tenant authz.TenantID, scopes []authz.Action, envs []string) string {
	t.Helper()
	g, err := k.hasher.Generate(authz.KindIngestKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	k.store[g.KeyID] = authz.KeyRecord{KeyID: g.KeyID, Tenant: tenant, Kind: authz.KindIngestKey, Hash: g.Hash,
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

type recordingObserver struct{ results []RequestResult }

func (o *recordingObserver) ObserveRequest(r RequestResult) { o.results = append(o.results, r) }

// 운영 지표: accepted는 Kafka append가 확인된 record만, append 실패는 accepted 0 + ProduceFailed (D04 §10).
func TestObserverReportsOutcome(t *testing.T) {
	s := newSetup(t)
	obs := &recordingObserver{}
	s.h.cfg.Observer = obs
	tok := s.keys.issue(t, allSignals, []string{"production"})
	post(s, "/v1/traces", tok, "application/json", "", fixture(t, "otlp", "traces_checkout.json"))
	s.prod.err = errors.New("NOT_ENOUGH_REPLICAS")
	post(s, "/v1/traces", tok, "application/json", "", fixture(t, "otlp", "traces_checkout.json"))
	post(s, "/v1/logs", "", "application/json", "", []byte("{}"))
	if len(obs.results) != 3 {
		t.Fatalf("results = %d", len(obs.results))
	}
	ok, failed, unauth := obs.results[0], obs.results[1], obs.results[2]
	if ok.Signal != "traces" || ok.Status != 200 || ok.Accepted != 4 || !ok.ProduceAttempted || ok.ProduceFailed {
		t.Errorf("ok = %+v", ok)
	}
	if failed.Status != 503 || failed.Accepted != 0 || !failed.ProduceFailed {
		t.Errorf("failed = %+v", failed)
	}
	if unauth.Status != 401 || unauth.ProduceAttempted {
		t.Errorf("unauth = %+v", unauth)
	}
}

// client가 append 대기 중 연결을 끊으면 broker 장애(error)가 아니라 canceled로 센다 — 장애 경보 오탐 방지.
func TestObserverClientCancelIsNotProduceFailure(t *testing.T) {
	s := newSetup(t)
	obs := &recordingObserver{}
	s.h.cfg.Observer = obs
	s.h.cfg.Producer = blockingProducer{}
	tok := s.keys.issue(t, allSignals, []string{"production"})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/traces", bytes.NewReader(fixture(t, "otlp", "traces_checkout.json")))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	s.h.ServeHTTP(httptest.NewRecorder(), req)
	if len(obs.results) != 1 || !obs.results[0].ProduceCanceled || obs.results[0].ProduceFailed || obs.results[0].Accepted != 0 {
		t.Errorf("result = %+v", obs.results)
	}
}

// quota (ADR 0024): rate 초과는 429 + 계산된 Retry-After, 아무것도 append하지 않는다.
// 요청 하나가 burst를 넘으면 기다려도 통과할 수 없으므로 413.
func TestQuota(t *testing.T) {
	s := newSetup(t)
	obs := &recordingObserver{}
	s.h.cfg.Observer = obs
	s.h.cfg.Quota = quota.New(quota.Config{Default: quota.Limits{RecordsPerSecond: 2, RecordsBurst: 6, BytesPerSecond: 1e9, BytesBurst: 1e9}})
	tok := s.keys.issue(t, allSignals, []string{"production"})
	body := fixture(t, "otlp", "traces_checkout.json") // span 4개

	if rec := post(s, "/v1/traces", tok, "application/json", "", body); rec.Code != 200 {
		t.Fatalf("first status = %d", rec.Code)
	}
	rec := post(s, "/v1/traces", tok, "application/json", "", body) // 남은 token 2 < 4
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("second status = %d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), `"code":8`) { // google.rpc.Status RESOURCE_EXHAUSTED (요청과 같은 JSON)
		t.Errorf("body = %s", rec.Body)
	}
	if len(s.prod.records) != 4 {
		t.Errorf("produced = %d, want only the first request's 4", len(s.prod.records))
	}
	last := obs.results[len(obs.results)-1]
	if last.Accepted != 0 || last.Rejected[ReasonRateLimited] != 4 || last.ProduceAttempted {
		t.Errorf("observed = %+v", last)
	}
	if !strings.Contains(s.logBuf.String(), `"quota_limit":"records"`) {
		t.Error("quota decision must be logged (D01 §08)")
	}

	// burst 6보다 큰 요청(span 8개)은 413
	s2 := newSetup(t)
	s2.h.cfg.Quota = quota.New(quota.Config{Default: quota.Limits{RecordsPerSecond: 100, RecordsBurst: 6, BytesPerSecond: 1e9, BytesBurst: 1e9}})
	tok2 := s2.keys.issue(t, allSignals, []string{"production"})
	big := ptrace.NewTraces()
	for range 2 {
		td, _ := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(body)
		td.ResourceSpans().MoveAndAppendTo(big.ResourceSpans())
	}
	bigBody, _ := (&ptrace.JSONMarshaler{}).MarshalTraces(big)
	if rec := post(s2, "/v1/traces", tok2, "application/json", "", bigBody); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over burst status = %d", rec.Code)
	}
	if len(s2.prod.records) != 0 {
		t.Errorf("produced = %d", len(s2.prod.records))
	}
}

// 다른 tenant의 소진은 영향을 주지 않는다 (noisy neighbor).
func TestQuotaIsPerTenant(t *testing.T) {
	s := newSetup(t)
	s.h.cfg.Quota = quota.New(quota.Config{Default: quota.Limits{RecordsPerSecond: 1, RecordsBurst: 4, BytesPerSecond: 1e9, BytesBurst: 1e9}})
	tokA := s.keys.issue(t, allSignals, []string{"production"})
	body := fixture(t, "otlp", "traces_checkout.json")
	post(s, "/v1/traces", tokA, "application/json", "", body)
	if rec := post(s, "/v1/traces", tokA, "application/json", "", body); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("A second = %d", rec.Code)
	}
	tenantB, _ := authz.ParseTenantID("22222222-2222-4222-8222-222222222222")
	tokB := s.keys.issueFor(t, tenantB, allSignals, []string{"production"})
	if rec := post(s, "/v1/traces", tokB, "application/json", "", body); rec.Code != 200 {
		t.Errorf("B status = %d, want 200", rec.Code)
	}
}

// instance 동시 처리 상한을 넘으면 인증 전에 503 (과부하, 계약 초과 아님).
func TestInflightLimit(t *testing.T) {
	s := newSetup(t)
	authCalls := 0
	inner := s.h.cfg.Authenticate
	s.h.cfg.Authenticate = func(ctx context.Context, tok string) (authz.Principal, error) { authCalls++; return inner(ctx, tok) }
	s.h.inflight = make(chan struct{}, 1)
	s.h.inflight <- struct{}{} // 이미 한 요청이 처리 중
	tok := s.keys.issue(t, allSignals, []string{"production"})
	rec := post(s, "/v1/traces", tok, "application/json", "", fixture(t, "otlp", "traces_checkout.json"))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || authCalls != 0 {
		t.Errorf("status=%d retry-after=%q authCalls=%d", rec.Code, rec.Header().Get("Retry-After"), authCalls)
	}
	<-s.h.inflight
	if rec := post(s, "/v1/traces", tok, "application/json", "", fixture(t, "otlp", "traces_checkout.json")); rec.Code != 200 {
		t.Errorf("after release = %d", rec.Code)
	}
}

// metricsBody는 point 속성 집합마다 gauge point 하나를 만든다(environment production).
func metricsBody(t *testing.T, attrSets ...map[string]string) []byte {
	t.Helper()
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "checkout")
	rm.Resource().Attributes().PutStr(EnvironmentAttr, "production")
	g := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	g.SetName("queue.depth")
	gauge := g.SetEmptyGauge()
	for _, attrs := range attrSets {
		dp := gauge.DataPoints().AppendEmpty()
		dp.SetTimestamp(pcommon.NewTimestampFromTime(fixtureTime))
		dp.SetIntValue(1)
		for k, v := range attrs {
			dp.Attributes().PutStr(k, v)
		}
	}
	b, err := (&pmetric.JSONMarshaler{}).MarshalMetrics(md)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type fakeAdmitter struct {
	reject func(i int, r envelope.StreamRef) bool
	err    error
	seen   int
}

func (f *fakeAdmitter) Admit(_ context.Context, _ authz.TenantID, refs []envelope.StreamRef, _ time.Time) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.seen += len(refs)
	out := make([]string, len(refs))
	for i, r := range refs {
		if f.reject(i, r) {
			out[i] = quota.ReasonSeriesLimit
		}
	}
	return out, nil
}

// cardinality (D02 §10, §22): 금지 dimension·label 과다·series 상한 초과 point만 거절하고 나머지는 append한다.
// 속성을 지워 통과시키지 않는다.
func TestMetricCardinality(t *testing.T) {
	s := newSetup(t)
	obs := &recordingObserver{}
	s.h.cfg.Observer = obs
	adm := &fakeAdmitter{reject: func(_ int, r envelope.StreamRef) bool {
		v, _ := r.Attributes.Get("shard")
		return v.Str() == "new" // 한도를 넘은 신규 series
	}}
	s.h.cfg.Series = adm
	tok := s.keys.issue(t, allSignals, []string{"production"})
	many := map[string]string{}
	for i := range 21 {
		many["k"+strconv.Itoa(i)] = "v"
	}
	body := metricsBody(t,
		map[string]string{"shard": "a"},
		map[string]string{"shard": "b", "user_id": "u-123"},
		many,
		map[string]string{"shard": "new"},
	)
	rec := post(s, "/v1/metrics", tok, "application/json", "", body)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if len(s.prod.records) != 1 {
		t.Fatalf("produced = %d, want only shard=a (user_id point는 redaction이 key를 지우기 전에 거절해야 한다)", len(s.prod.records))
	}
	if adm.seen != 2 {
		t.Errorf("series check saw %d points, want 2 (dimension 규칙으로 거절된 point는 등록부에 가지 않는다)", adm.seen)
	}
	r := obs.results[0]
	if r.Rejected[quota.ReasonForbiddenDimension] != 1 || r.Rejected[quota.ReasonTooManyLabels] != 1 || r.Rejected[quota.ReasonSeriesLimit] != 1 {
		t.Errorf("rejected = %v", r.Rejected)
	}
	if strings.Contains(s.logBuf.String(), "u-123") {
		t.Error("forbidden dimension value leaked into logs")
	}
	// 남은 point는 속성이 그대로다(지워서 합치지 않음)
	md, err := (&pmetric.ProtoUnmarshaler{}).UnmarshalMetrics(s.prod.records[0].Value)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Gauge().DataPoints().At(0).Attributes().Get("shard")
	if v.Str() != "a" {
		t.Errorf("kept point = %v", v.Str())
	}
}

// series 등록부(제어 DB) 장애면 판정할 수 없으므로 받지 않는다: 503 + Retry-After.
type unavailableErr struct{}

func (unavailableErr) Error() string     { return "pg down" }
func (unavailableErr) Unavailable() bool { return true }

func TestMetricSeriesRegistryUnavailable(t *testing.T) {
	s := newSetup(t)
	s.h.cfg.Series = &fakeAdmitter{err: unavailableErr{}}
	tok := s.keys.issue(t, allSignals, []string{"production"})
	rec := post(s, "/v1/metrics", tok, "application/json", "", metricsBody(t, map[string]string{"shard": "a"}))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || len(s.prod.records) != 0 {
		t.Errorf("status=%d produced=%d", rec.Code, len(s.prod.records))
	}
	if strings.Contains(rec.Body.String(), "pg down") {
		t.Error("internal error leaked")
	}
}

// 재시도해도 같은 결과인 등록부 오류(제약 위반 등)는 503이 아니라 500이다 — client가 같은 batch를 끝없이 재전송하지 않게.
func TestMetricSeriesRegistryDefectIs500(t *testing.T) {
	s := newSetup(t)
	s.h.cfg.Series = &fakeAdmitter{err: errors.New("check constraint violated")}
	tok := s.keys.issue(t, allSignals, []string{"production"})
	rec := post(s, "/v1/metrics", tok, "application/json", "", metricsBody(t, map[string]string{"shard": "a"}))
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Retry-After") != "" {
		t.Errorf("status = %d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

type fakeCatalog struct {
	calls [][]catalog.Sighting
}

func (f *fakeCatalog) Observe(_ authz.TenantID, s []catalog.Sighting, _ time.Time) {
	f.calls = append(f.calls, s)
}

// ACK한 요청의 서비스만 catalog에 알린다. service_id는 worker가 원본 행에 쓰는 값과 같다.
func TestCatalogSightingsAfterAck(t *testing.T) {
	s := newSetup(t)
	cat := &fakeCatalog{}
	s.h.cfg.Catalog = cat
	tok := s.keys.issue(t, allSignals, []string{"production"})
	body := fixture(t, "otlp", "traces_checkout.json")
	if rec := post(s, "/v1/traces", tok, "application/json", "", body); rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(cat.calls) != 1 || len(cat.calls[0]) == 0 {
		t.Fatalf("sightings = %+v", cat.calls)
	}
	td, _ := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(body)
	want := envelope.ServiceID(tenantA, td.ResourceSpans().At(0).Resource().Attributes())
	if got := cat.calls[0][0]; got.ServiceID != want || got.Environment != "production" || got.Name == "" {
		t.Errorf("sighting = %+v, want service_id %s", got, want)
	}
	// Kafka append 실패(ACK 없음)면 알리지 않는다
	s.prod.err = errors.New("kafka down")
	if rec := post(s, "/v1/traces", tok, "application/json", "", body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(cat.calls) != 1 {
		t.Errorf("sighting reported without ACK: %d calls", len(cat.calls))
	}
}
