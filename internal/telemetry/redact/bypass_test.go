package redact

import (
	"strings"
	"testing"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/polynomeer/montracer/internal/telemetry/otlp"
)

// spec-reviewer가 실제로 누출을 확인한 우회 입력. 모두 막혀야 한다 (ADR 0019).
func TestKeyNormalizationBypasses(t *testing.T) {
	for _, k := range []string{
		"userEmail", "accessToken", "customerId", "creditCardNumber", "api_key", "private_key",
		"X-Api-Key", " password", "app:password", "app/secret", "db.query.parameter.0", "db.query.parameter.name",
		"HTTPAuthorization", "clientSecret",
	} {
		if New(DefaultPolicy).c.action(k) != drop {
			t.Errorf("%q must be dropped", k)
		}
	}
	for _, k := range []string{"user_agent.original", "service.name", "http.route", "span.kind", "db.system.name", "pinpoint.agent", "tokenizer.model"} {
		if a := New(DefaultPolicy).c.action(k); a == drop {
			t.Errorf("%q must be kept", k)
		}
	}
}

func TestTextBypasses(t *testing.T) {
	cases := []struct {
		name, in       string
		mustNotContain []string
	}{
		{"email then encoded secret", "x@ex.com token%3DSECRETZZ", []string{"SECRETZZ"}},
		{"email then encoded email", "x@ex.com mail%3Dy%40z.com", []string{"y@z.com", "y%40z.com"}},
		{"plus kept in local part", "a%20b jiwoo.kim+tag@example.com", []string{"jiwoo.kim"}},
		{"invalid percent does not stop decode", "100% sure token%3DSECRETYY", []string{"SECRETYY"}},
		{"triple encoding", "a%252540b.com", []string{"a@b.com"}},
		{"json password", `{"password":"hunter2"}`, []string{"hunter2"}},
		{"single quoted", `password='hunter2'`, []string{"hunter2"}},
		{"yaml style", `password: "hunter2"`, []string{"hunter2"}},
		{"authorization token scheme", `Authorization: Token abc123`, []string{"abc123"}},
		{"card after number", "qty 2 4111 1111 1111 1111", []string{"4111 1111 1111 1111"}},
		{"card after id", "id 1 4111111111111111", []string{"4111111111111111"}},
		{"card glued to word", "card4111111111111111", []string{"4111111111111111"}},
		{"card dashed after number", "order 7 4111-1111-1111-1111", []string{"4111-1111-1111-1111"}},
		{"public ip in text", "login from 203.0.113.45 failed", []string{"203.0.113.45"}},
		{"public ipv6 in text", "peer 2001:db8:abcd:12::1 reset", []string{"2001:db8:abcd:12::1"}},
		{"foreign registration number", "id 900101-5234567", []string{"900101-5234567"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := scrubText(tc.in, counter{})
			for _, s := range tc.mustNotContain {
				if strings.Contains(out, s) {
					t.Fatalf("%q → %q still contains %q", tc.in, out, s)
				}
			}
		})
	}
}

// 오탐 고정: 조사에 필요한 값은 망가뜨리지 않는다.
func TestNoFalsePositives(t *testing.T) {
	for _, s := range []string{
		"ts=1759651200000",                // epoch ms (주민번호 오탐 금지)
		"basic auth failed for request",   // 일반 문장
		"order 1234567890123456",          // Luhn 불만족 숫자열
		"retry in 12:30:45",               // 시각 (IPv6 오탐 금지)
		"connect 10.0.3.7:5432 refused",   // 사설 IP는 인프라 식별자
		"GET /api/v1/items/{id} 200 12ms", // 일반 로그
		"duration_ms=8123",
	} {
		if out := scrubText(s, counter{}); out != s {
			t.Errorf("%q changed to %q", s, out)
		}
	}
}

func TestURLUserinfoWhenParseFails(t *testing.T) {
	for _, in := range []string{"http://admin:s3cret@host:abc/x", "https://u:p%ZZ@h/x?q=1"} {
		out := cleanURL(in, counter{})
		if strings.Contains(out, "s3cret") || strings.Contains(out, "u:p") || strings.Contains(out, "q=1") {
			t.Errorf("%q → %q", in, out)
		}
	}
}

func TestSQLLiterals(t *testing.T) {
	cases := []struct{ in, want string }{
		{`SELECT * FROM t WHERE name = 'kim jiwoo AND id = $1`, `SELECT * FROM t WHERE name = ?`}, // 잘린 문장
		{`SELECT * FROM t WHERE n = 'a\'b jiwoo' AND x = 1`, `SELECT * FROM t WHERE n = ? AND x = ?`},
		{`SELECT * FROM t WHERE n = 'it''s'`, `SELECT * FROM t WHERE n = ?`},
		{`SELECT * FROM "users" WHERE note = "secret text"`, `SELECT * FROM "users" WHERE note = ?`},
		{`SELECT $tag$jiwoo$tag$, $$x$$`, `SELECT ?, ?`},
		{`SELECT * FROM t WHERE b = 0x1F2E AND h = X'AB'`, `SELECT * FROM t WHERE b = ? AND h = X?`},
		{`SELECT * FROM table1 WHERE id = $1 AND name = :name AND v = ?`, `SELECT * FROM table1 WHERE id = $1 AND name = :name AND v = ?`},
		{`SELECT * FROM t LIMIT 10 OFFSET 20`, `SELECT * FROM t LIMIT ? OFFSET ?`},
	}
	for _, tc := range cases {
		if got := cleanSQL(tc.in, counter{}); got != tc.want {
			t.Errorf("cleanSQL(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

func TestIPAttributes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"203.0.113.45:8080", "203.0.113.0/24"},
		{"[2001:db8::1]:443", "2001:db8::/48"},
		{"203.0.113.45, 10.0.0.1", "203.0.113.0/24, 10.0.0.1"},
		{"fe80::1%eth0", "fe80::1"},
		{"not-an-ip", "[REDACTED:ip]"},
		{"10.0.0.5", "10.0.0.5"},
	}
	for _, tc := range cases {
		if got := truncateIP(tc.in, counter{}); got != tc.want {
			t.Errorf("truncateIP(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNonStringValuesForSpecialKeysAreDropped(t *testing.T) {
	td := ptrace.NewTraces()
	s := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	s.Attributes().PutEmptyMap("url.full").PutStr("q", "token=x")
	s.Attributes().PutEmptyBytes("db.query.text").FromRaw([]byte("SELECT 'x'"))
	s.Attributes().PutEmptySlice("client.address").AppendEmpty().SetStr("203.0.113.9")
	s.Links().AppendEmpty().Attributes().PutStr("note", "a@b.co")
	res := New(DefaultPolicy).Traces(td)
	if _, ok := s.Attributes().Get("url.full"); ok {
		t.Error("map value for url key must be dropped")
	}
	if _, ok := s.Attributes().Get("db.query.text"); ok {
		t.Error("bytes value for sql key must be dropped")
	}
	if v, _ := s.Attributes().Get("client.address"); v.Slice().At(0).Str() != "203.0.113.0/24" {
		t.Errorf("slice ip = %v", v.AsRaw())
	}
	if v, _ := s.Links().At(0).Attributes().Get("note"); v.Str() != "[REDACTED:email]" {
		t.Errorf("link attr = %q", v.Str())
	}
	if res.Redactions[KindNonString] != 2 {
		t.Errorf("counts = %v", res.Redactions)
	}
}

// 재전송(at-least-once) 시 같은 record를 다시 redaction해도 결과가 같다.
func TestIdempotent(t *testing.T) {
	ld := decode(t, "logs_with_pii.json", otlp.SignalLogs).Logs
	r := New(DefaultPolicy)
	r.Logs(ld)
	first, _ := (&plog.ProtoMarshaler{}).MarshalLogs(ld)
	r.Logs(ld)
	second, _ := (&plog.ProtoMarshaler{}).MarshalLogs(ld)
	if string(first) != string(second) {
		t.Fatal("second redaction changed the output")
	}
}

func TestFailureInjectionLogsMetricsScope(t *testing.T) {
	failNth := func(r *Redactor, nth int) {
		calls := 0
		r.failHook = func() {
			calls++
			if calls == nth {
				panic("x MTXCANARY98")
			}
		}
	}
	// logs: resource(1), scope(2), record a(3), record b(4) → b 실패
	ld := plog.NewLogs()
	lrs := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()
	lrs.AppendEmpty().Body().SetStr("a")
	lrs.AppendEmpty().Body().SetStr("b MTXCANARY98")
	r := New(DefaultPolicy)
	failNth(r, 4)
	if res := r.Logs(ld); res.Failed != 1 || ld.LogRecordCount() != 1 {
		t.Errorf("logs: %+v", res)
	}
	// metrics: resource(1), scope(2), metadata(3), point(4) → point 실패
	md := pmetric.NewMetrics()
	m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("m")
	m.SetEmptyGauge().DataPoints().AppendEmpty().Attributes().PutStr("k", "v")
	r = New(DefaultPolicy)
	failNth(r, 4)
	if res := r.Metrics(md); res.Failed != 1 || md.DataPointCount() != 0 {
		t.Errorf("metrics: %+v", res)
	}
	// scope 실패 → 그 아래 span 전체 실패
	td := ptrace.NewTraces()
	ss := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty()
	ss.Spans().AppendEmpty()
	ss.Spans().AppendEmpty()
	r = New(DefaultPolicy)
	failNth(r, 2)
	if res := r.Traces(td); res.Failed != 2 || td.SpanCount() != 0 {
		t.Errorf("scope failure: %+v", res)
	}
}

func BenchmarkScrubText32KiB(b *testing.B) {
	inputs := map[string]string{
		"plain":   strings.Repeat("request processed in handler checkout ok ", 800)[:32<<10],
		"numeric": strings.Repeat("id=1234567 ts=1759651200000 n=42 ", 1000)[:32<<10],
		"percent": strings.Repeat("path=%2Fapi%2Fv1 q=a%20b ", 1400)[:32<<10],
	}
	for name, in := range inputs {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			for b.Loop() {
				scrubText(in, counter{})
			}
		})
	}
}
