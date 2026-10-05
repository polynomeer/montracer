package redact

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/polynomeer/montracer/internal/telemetry/otlp"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "fixtures", "pii", name)) //nolint:gosec // 고정 fixture 경로
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decode(t *testing.T, name string, sig otlp.Signal) otlp.Payload {
	t.Helper()
	p, err := otlp.Decode(bytes.NewReader(fixture(t, name)), "application/json", "", sig, otlp.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// 회귀 시험의 판정 기준: catalog 항목별로 저장 경로에서 사라져야 하는 민감 부분 문자열.
// D04 §03 "fixture가 ingress 이후 저장소에서 검색되지 않아야 한다".
var mustDisappear = map[string][]string{
	"email":                {"jiwoo.kim+MTXCANARY01@example.com"},
	"phone_kr":             {"010-0000-0000"},
	"card_test_pan":        {"4111 1111 1111 1111"},
	"rrn_fake":             {"900101-1234567"},
	"password_field":       {"pw-MTXCANARY02-Hunter2!"},
	"authorization_header": {"eyJhbGciOiJIUzI1NiJ9", "MTXCANARY03"},
	"cookie_header":        {"MTXCANARY04abcdef"},
	"api_key_like":         {"MTXCANARY05"},
	"ip_v4":                {"203.0.113.45"},
	"ip_v6":                {"2001:db8::1234"},
	"name_ko":              {"김지우"},
	"url_query_token":      {"MTXCANARY08tok", "jiwoo%40example.com"},
	"url_encoded_email":    {"MTXCANARY09"},
	"nested_json":          {"nested+MTXCANARY10@example.com", "5555555555554444"},
	"multipart":            {"000-00-0000"},
	"sql_literal":          {"lit+MTXCANARY13@example.com"},
	"stack_secret":         {"MTXCANARY14pw"},
	"double_encoded_email": {"MTXCANARY15"},
	"escaped_at_email":     {"MTXCANARY16"},
	"secret_after_4k":      {"MTXCANARY17secret"},
	"span_name_email":      {"name+MTXCANARY18@example.com"},
	"resource_owner_email": {"owner+MTXCANARY19@example.com"},
	"metric_attr_user":     {"이서연"},
	"exemplar_attr_email":  {"ex+MTXCANARY21@example.com"},
}

// 알려진 한계 (ADR 0019): 패턴이 없는 자유 텍스트 속 사람 이름, PII가 아닌 표식 문자열.
var knownLimitations = map[string]string{
	"name_ja":     "log body 자유 텍스트 속 이름은 패턴으로 탐지할 수 없다 (key 규칙·본문 수집 정책으로 막는다)",
	"long_string": "PII가 아닌 길이 시험용 표식",
}

func TestCatalogCoverage(t *testing.T) {
	var cat struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(fixture(t, "catalog.json"), &cat); err != nil {
		t.Fatal(err)
	}
	for _, it := range cat.Items {
		_, a := mustDisappear[it.ID]
		_, b := knownLimitations[it.ID]
		if !a && !b {
			t.Errorf("catalog item %q has no expectation — add it to mustDisappear or knownLimitations", it.ID)
		}
	}
}

func TestPIIFixturesAreRedacted(t *testing.T) {
	r := New(DefaultPolicy)
	var stored bytes.Buffer

	td := decode(t, "traces_with_pii.json", otlp.SignalTraces).Traces
	if res := r.Traces(td); res.Failed != 0 || res.Records != 1 {
		t.Fatalf("traces: %+v", res)
	}
	b, _ := (&ptrace.ProtoMarshaler{}).MarshalTraces(td)
	stored.Write(b)

	ld := decode(t, "logs_with_pii.json", otlp.SignalLogs).Logs
	if res := r.Logs(ld); res.Failed != 0 || res.Records != 3 {
		t.Fatalf("logs: %+v", res)
	}
	b, _ = (&plog.ProtoMarshaler{}).MarshalLogs(ld)
	stored.Write(b)

	md := decode(t, "metrics_with_pii.json", otlp.SignalMetrics).Metrics
	if res := r.Metrics(md); res.Failed != 0 || res.Records != 1 {
		t.Fatalf("metrics: %+v", res)
	}
	b, _ = (&pmetric.ProtoMarshaler{}).MarshalMetrics(md)
	stored.Write(b)

	out := stored.String()
	for id, needles := range mustDisappear {
		for _, s := range needles {
			if strings.Contains(out, s) {
				t.Errorf("%s: %q survived redaction", id, s)
			}
		}
	}
	// 저장할 구조는 유지된다 (trace 연결·서비스 식별).
	if !strings.Contains(out, "checkout") {
		t.Error("service.name must be kept")
	}
	span := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	if span.TraceID().String() != "5bf92f3577b34da6a3ce929d0e0e4737" {
		t.Error("trace id must not change")
	}
}

func newSpan(attrs map[string]any) ptrace.Traces {
	td := ptrace.NewTraces()
	s := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	s.SetName("GET /api/v1/items/{id}")
	_ = s.Attributes().FromRaw(attrs)
	s.SetStartTimestamp(pcommon.NewTimestampFromTime(time.Unix(0, 0)))
	return td
}

func attrOf(td ptrace.Traces, k string) (pcommon.Value, bool) {
	return td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Attributes().Get(k)
}

func TestKeyRules(t *testing.T) {
	td := newSpan(map[string]any{
		"http.request.header.content-type":  []any{"application/json"},
		"http.request.header.authorization": []any{"Bearer abc"},
		"http.request.header.x-custom":      []any{"anything"}, // header 기본 deny
		"app.password":                      "p",
		"enduser.id":                        "u-1",
		"session.id":                        "s-1",
		"url.query":                         "a=1",
		"service.name":                      "checkout", // deny 토큰 아님
		"http.route":                        "/users/{id}",
		"db.system.name":                    "postgresql",
	})
	res := New(DefaultPolicy).Traces(td)
	for _, k := range []string{"http.request.header.authorization", "http.request.header.x-custom", "app.password", "enduser.id", "session.id", "url.query"} {
		if _, ok := attrOf(td, k); ok {
			t.Errorf("%s must be dropped", k)
		}
	}
	for _, k := range []string{"http.request.header.content-type", "service.name", "http.route", "db.system.name"} {
		if _, ok := attrOf(td, k); !ok {
			t.Errorf("%s must be kept", k)
		}
	}
	if res.Redactions[KindHeader] != 2 || res.Redactions[KindDeniedKey] != 4 {
		t.Errorf("counts = %v", res.Redactions)
	}
}

func TestValueRules(t *testing.T) {
	cases := []struct {
		key, in, want string
	}{
		{"url.full", "https://u:p@shop.example.com/orders/42?token=x#frag", "https://shop.example.com/orders/42"},
		{"db.query.text", "SELECT * FROM t WHERE email = 'a@b.co' AND age > 30", "SELECT * FROM t WHERE email = ? AND age > ?"},
		{"client.address", "203.0.113.45", "203.0.113.0/24"},
		{"client.address", "2001:db8:abcd:12::1", "2001:db8:abcd::/48"},
		{"exception.message", "login failed password=hunter2 for x", "login failed password=[REDACTED:secret] for x"},
		{"exception.message", "card 4111-1111-1111-1111 declined", "card [REDACTED:payment_card] declined"},
		// Luhn을 만족하지 않는 숫자열은 카드가 아니다 (주문 번호 등 오탐 방지)
		{"order.ref", "order 1234567890123456", "order 1234567890123456"},
		{"http.route", "/api/v1/items/{id}", "/api/v1/items/{id}"},
		{"note", "mail me at a.b+c@example.org", "mail me at [REDACTED:email]"},
		{"note", "call 010-1234-5678", "call [REDACTED:phone]"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"/"+tc.in, func(t *testing.T) {
			td := newSpan(map[string]any{tc.key: tc.in})
			New(DefaultPolicy).Traces(td)
			v, ok := attrOf(td, tc.key)
			if !ok || v.Str() != tc.want {
				t.Fatalf("got %q (present=%v), want %q", v.Str(), ok, tc.want)
			}
		})
	}
}

// redaction 실패 경로 (D04 §03): 실패한 record는 저장하지 않고, 원문은 결과·오류 어디에도 남지 않는다.
func TestRedactionFailureDropsRecord(t *testing.T) {
	td := ptrace.NewTraces()
	spans := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans()
	for i, secret := range []string{"ok-1 a@example.com", "FAIL leak+MTXCANARY99@example.com", "ok-3"} {
		s := spans.AppendEmpty()
		s.SetName(secret)
		s.SetSpanID(pcommon.SpanID{byte(i + 1)})
	}
	r := New(DefaultPolicy)
	calls := 0
	r.failHook = func() {
		calls++
		if calls == 3 { // resource·scope guard 다음 두 번째 span
			panic("boom: FAIL leak+MTXCANARY99@example.com")
		}
	}
	res := r.Traces(td)
	if res.Records != 3 || res.Failed != 1 || td.SpanCount() != 2 {
		t.Fatalf("result = %+v, spans left = %d", res, td.SpanCount())
	}
	b, _ := (&ptrace.ProtoMarshaler{}).MarshalTraces(td)
	if bytes.Contains(b, []byte("MTXCANARY99")) || strings.Contains(res.Message(), "MTXCANARY99") {
		t.Fatal("failed record content leaked")
	}
	if res.Message() != "rejected redaction_failed=1" {
		t.Errorf("message = %q", res.Message())
	}
}

func TestResourceFailureDropsAllRecordsBelow(t *testing.T) {
	td := ptrace.NewTraces()
	ss := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty()
	ss.Spans().AppendEmpty().SetName("a")
	ss.Spans().AppendEmpty().SetName("b")
	r := New(DefaultPolicy)
	first := true
	r.failHook = func() {
		if first {
			first = false
			panic("resource")
		}
	}
	res := r.Traces(td)
	if res.Failed != 2 || td.ResourceSpans().Len() != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestPolicyVersion(t *testing.T) {
	if New(DefaultPolicy).PolicyVersion() != 1 {
		t.Fatal("policy version")
	}
}
