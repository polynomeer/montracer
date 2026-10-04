package otlp

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"google.golang.org/protobuf/encoding/protowire"
)

// fixture 기준 시각 2026-10-05T00:00:00Z (tests/fixtures/otlp/README.md)
var fixtureTime = time.Unix(1791158400, 0).UTC()

func fixture(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "fixtures", dir, name)) //nolint:gosec // 고정 fixture 경로
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decode(t *testing.T, body []byte, ct, ce string, sig Signal) Payload {
	t.Helper()
	p, err := Decode(bytes.NewReader(body), ct, ce, sig, DefaultLimits)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return p
}

// golden: JSON fixture 해석 결과가 기대 구조이고, protobuf로 바꿔도 같은 결과가 나온다.
func TestGoldenFixtures(t *testing.T) {
	t.Run("traces", func(t *testing.T) {
		js := decode(t, fixture(t, "otlp", "traces_checkout.json"), "application/json", "", SignalTraces).Traces
		if js.SpanCount() != 4 || js.ResourceSpans().Len() != 2 {
			t.Fatalf("spans=%d resources=%d", js.SpanCount(), js.ResourceSpans().Len())
		}
		root := js.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
		// OTLP JSON은 trace/span ID를 hex로 쓴다 (base64가 아님).
		if root.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || root.SpanID().String() != "00f067aa0ba902b7" {
			t.Errorf("ids = %s/%s", root.TraceID(), root.SpanID())
		}
		if root.Kind() != ptrace.SpanKindServer || root.Status().Code() != ptrace.StatusCodeError {
			t.Errorf("kind=%v status=%v", root.Kind(), root.Status().Code())
		}
		if v, _ := root.Attributes().Get("http.response.status_code"); v.Int() != 500 {
			t.Errorf("status_code attr = %v", v.AsRaw())
		}
		pb, err := (&ptrace.ProtoMarshaler{}).MarshalTraces(js)
		if err != nil {
			t.Fatal(err)
		}
		fromPB := decode(t, pb, "application/x-protobuf", "", SignalTraces).Traces
		again, _ := (&ptrace.ProtoMarshaler{}).MarshalTraces(fromPB)
		if !bytes.Equal(pb, again) {
			t.Error("protobuf round trip differs from JSON decode")
		}
	})
	t.Run("logs", func(t *testing.T) {
		ld := decode(t, fixture(t, "otlp", "logs_checkout.json"), "application/json", "", SignalLogs).Logs
		if ld.LogRecordCount() != 2 {
			t.Fatalf("logs = %d", ld.LogRecordCount())
		}
		lr := ld.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
		if lr.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || lr.SeverityNumber() != plog.SeverityNumberError {
			t.Errorf("trace=%s severity=%v", lr.TraceID(), lr.SeverityNumber())
		}
		pb, _ := (&plog.ProtoMarshaler{}).MarshalLogs(ld)
		if n := decode(t, pb, "application/x-protobuf", "", SignalLogs).Logs.LogRecordCount(); n != 2 {
			t.Errorf("protobuf logs = %d", n)
		}
	})
	t.Run("metrics", func(t *testing.T) {
		md := decode(t, fixture(t, "otlp", "metrics_checkout.json"), "application/json", "", SignalMetrics).Metrics
		if md.DataPointCount() != 3 {
			t.Fatalf("points = %d", md.DataPointCount())
		}
		h := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0)
		if h.Type() != pmetric.MetricTypeHistogram || h.Histogram().AggregationTemporality() != pmetric.AggregationTemporalityCumulative {
			t.Fatalf("histogram type=%v", h.Type())
		}
		if p := h.Histogram().DataPoints().At(0); p.Count() != 1000 || p.BucketCounts().Len() != 5 {
			t.Errorf("histogram point count=%d buckets=%d", p.Count(), p.BucketCounts().Len())
		}
		pb, _ := (&pmetric.ProtoMarshaler{}).MarshalMetrics(md)
		if n := decode(t, pb, "application/x-protobuf", "", SignalMetrics).Metrics.DataPointCount(); n != 3 {
			t.Errorf("protobuf points = %d", n)
		}
	})
}

func TestDecodeEncodingsAndMediaTypes(t *testing.T) {
	body := fixture(t, "otlp", "traces_checkout.json")
	ok := []struct {
		name, ct, ce string
		b            []byte
	}{
		{"json", "application/json", "", body},
		{"json with charset", "application/json; charset=utf-8", "", body},
		{"identity", "application/json", "identity", body},
		{"gzip", "application/json", "gzip", gz(t, body)},
		{"gzip uppercase", "application/json", "GZIP", gz(t, body)},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			if p := decode(t, tc.b, tc.ct, tc.ce, SignalTraces); p.Traces.SpanCount() != 4 {
				t.Fatalf("spans = %d", p.Traces.SpanCount())
			}
		})
	}
	bad := []struct {
		name, ct, ce string
		b            []byte
		want         error
	}{
		{"text/plain", "text/plain", "", body, ErrUnsupportedMediaType},
		{"empty content type", "", "", body, ErrUnsupportedMediaType},
		{"brotli", "application/json", "br", body, ErrUnsupportedMediaType},
		{"declared gzip but plain", "application/json", "gzip", body, ErrMalformed},
		{"truncated gzip", "application/json", "gzip", gz(t, body)[:40], ErrMalformed},
		{"protobuf garbage", "application/x-protobuf", "", []byte{0xff, 0xff, 0xff, 0x01}, ErrMalformed},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(bytes.NewReader(tc.b), tc.ct, tc.ce, SignalTraces, DefaultLimits); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestBodyLimits(t *testing.T) {
	lim := Limits{MaxWireBytes: 1 << 10, MaxDecodedBytes: 4 << 10}
	t.Run("wire over limit", func(t *testing.T) {
		_, err := Decode(bytes.NewReader(make([]byte, 1<<10+1)), "application/json", "", SignalTraces, lim)
		if !errors.Is(err, ErrBodyTooLarge) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("gzip bomb stops at decoded limit", func(t *testing.T) {
		bomb := gz(t, bytes.Repeat([]byte{'0'}, 8<<20)) // 압축 후 수 KiB
		if len(bomb) > 1<<20 {
			t.Fatalf("bomb not small: %d", len(bomb))
		}
		_, err := Decode(bytes.NewReader(bomb), "application/json", "gzip", SignalTraces,
			Limits{MaxWireBytes: 1 << 20, MaxDecodedBytes: 64 << 10})
		if !errors.Is(err, ErrBodyTooLarge) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("uncompressed over decoded limit", func(t *testing.T) {
		_, err := Decode(bytes.NewReader(make([]byte, 5<<10)), "application/json", "", SignalTraces,
			Limits{MaxWireBytes: 8 << 10, MaxDecodedBytes: 4 << 10})
		if !errors.Is(err, ErrBodyTooLarge) {
			t.Fatalf("err = %v", err)
		}
	})
}

// 해석 오류에 요청 본문 조각이 실리면 로그로 PII가 샐 수 있다.
func TestMalformedErrorCarriesNoPayload(t *testing.T) {
	body := []byte(`{"resourceSpans":[{"resource":{"attributes":[{"key":"user.email","value":{"stringValue":"leak+MTXCANARY@example.com"}}]}, BROKEN`)
	_, err := Decode(bytes.NewReader(body), "application/json", "", SignalTraces, DefaultLimits)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "MTXCANARY") || strings.Contains(err.Error(), "example.com") {
		t.Fatalf("error leaks payload: %v", err)
	}
}

// PII fixture는 decode·검증을 통과해 redaction 단계까지 도달해야 한다 (제거는 redaction의 책임).
func TestPIIFixturesReachRedaction(t *testing.T) {
	td := decode(t, fixture(t, "pii", "traces_with_pii.json"), "application/json", "", SignalTraces).Traces
	if r := ValidateTraces(td, fixtureTime, DefaultRules); r.Accepted != 1 || r.Rejected != 0 {
		t.Errorf("pii trace: %+v", r)
	}
	ld := decode(t, fixture(t, "pii", "logs_with_pii.json"), "application/json", "", SignalLogs).Logs
	if r := ValidateLogs(ld, fixtureTime, DefaultRules); r.Accepted != 3 || r.Rejected != 0 {
		t.Errorf("pii logs: %+v", r)
	}
	md := decode(t, fixture(t, "pii", "metrics_with_pii.json"), "application/json", "", SignalMetrics).Metrics
	if r := ValidateMetrics(md, fixtureTime, DefaultRules); r.Accepted != 1 || r.Rejected != 0 {
		t.Errorf("pii metrics: %+v", r)
	}
}

func TestFixturesValidate(t *testing.T) {
	recv := fixtureTime.Add(time.Second)
	td := decode(t, fixture(t, "otlp", "traces_checkout.json"), "application/json", "", SignalTraces).Traces
	if r := ValidateTraces(td, recv, DefaultRules); r.Accepted != 4 || r.Rejected != 0 || r.Message() != "" {
		t.Errorf("traces: %+v", r)
	}
	ld := decode(t, fixture(t, "otlp", "logs_checkout.json"), "application/json", "", SignalLogs).Logs
	if r := ValidateLogs(ld, recv, DefaultRules); r.Accepted != 2 || r.Rejected != 0 {
		t.Errorf("logs: %+v", r)
	}
	md := decode(t, fixture(t, "otlp", "metrics_checkout.json"), "application/json", "", SignalMetrics).Metrics
	if r := ValidateMetrics(md, recv, DefaultRules); r.Accepted != 3 || r.Rejected != 0 {
		t.Errorf("metrics: %+v", r)
	}
}

func TestTimeWindow(t *testing.T) {
	cases := []struct {
		name     string
		received time.Time
		accepted int
	}{
		{"just received", fixtureTime.Add(time.Second), 4},
		{"23h late", fixtureTime.Add(23 * time.Hour), 4},
		{"past 24h → backfill path", fixtureTime.Add(25 * time.Hour), 0},
		{"more than 5m in future", fixtureTime.Add(-6 * time.Minute), 0},
		{"within 5m skew", fixtureTime.Add(-4 * time.Minute), 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			td := decode(t, fixture(t, "otlp", "traces_checkout.json"), "application/json", "", SignalTraces).Traces
			r := ValidateTraces(td, tc.received, DefaultRules)
			if r.Accepted != tc.accepted || r.Accepted+r.Rejected != 4 {
				t.Fatalf("result = %+v", r)
			}
			if tc.accepted == 0 && (r.Reasons[ReasonTimeOutOfRange] != 4 || td.ResourceSpans().Len() != 0) {
				t.Fatalf("rejected spans/resources must be removed: %+v, resources=%d", r, td.ResourceSpans().Len())
			}
		})
	}
}

// 단일 span trace를 만들고 mutate로 위반을 주입한다.
func oneSpan(mutate func(ptrace.Span)) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	s := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	s.SetTraceID(pcommon.TraceID{1})
	s.SetSpanID(pcommon.SpanID{1})
	s.SetName("op")
	s.SetStartTimestamp(pcommon.NewTimestampFromTime(fixtureTime))
	s.SetEndTimestamp(pcommon.NewTimestampFromTime(fixtureTime.Add(time.Millisecond)))
	mutate(s)
	return td
}

func TestSpanRules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(ptrace.Span)
		want   Reason
	}{
		{"zero trace id", func(s ptrace.Span) { s.SetTraceID(pcommon.TraceID{}) }, ReasonInvalidTraceID},
		{"zero span id", func(s ptrace.Span) { s.SetSpanID(pcommon.SpanID{}) }, ReasonInvalidSpanID},
		{"end before start", func(s ptrace.Span) {
			s.SetEndTimestamp(pcommon.NewTimestampFromTime(fixtureTime.Add(-time.Millisecond)))
		}, ReasonEndBeforeStart},
		{"129 attributes", func(s ptrace.Span) {
			for i := range 129 {
				s.Attributes().PutInt("k"+string(rune('A'+i%26))+strings.Repeat("x", i/26), int64(i))
			}
		}, ReasonTooManyAttributes},
		{"attribute value over 4KiB", func(s ptrace.Span) { s.Attributes().PutStr("big", strings.Repeat("a", 4<<10+1)) }, ReasonAttributeTooLong},
		{"nested slice value over 4KiB", func(s ptrace.Span) {
			s.Attributes().PutEmptySlice("list").AppendEmpty().SetStr(strings.Repeat("a", 4<<10+1))
		}, ReasonAttributeTooLong},
		{"event attribute over 4KiB", func(s ptrace.Span) {
			s.Events().AppendEmpty().Attributes().PutStr("stack", strings.Repeat("a", 4<<10+1))
		}, ReasonAttributeTooLong},
		{"span over 64KiB", func(s ptrace.Span) {
			for i := range 20 {
				s.Attributes().PutStr("chunk"+string(rune('a'+i)), strings.Repeat("a", 4<<10))
			}
		}, ReasonRecordTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := ValidateTraces(oneSpan(tc.mutate), fixtureTime, DefaultRules)
			if r.Rejected != 1 || r.Reasons[tc.want] != 1 {
				t.Fatalf("result = %+v, want %s", r, tc.want)
			}
		})
	}
	// 경계값은 통과한다.
	r := ValidateTraces(oneSpan(func(s ptrace.Span) { s.Attributes().PutStr("edge", strings.Repeat("a", 4<<10)) }), fixtureTime, DefaultRules)
	if r.Accepted != 1 {
		t.Fatalf("4KiB exactly must pass: %+v", r)
	}
}

func TestPartialSuccessKeepsValidRecords(t *testing.T) {
	td := decode(t, fixture(t, "otlp", "traces_checkout.json"), "application/json", "", SignalTraces).Traces
	// payment resource의 DB span 하나만 위반으로 만든다.
	td.ResourceSpans().At(1).ScopeSpans().At(0).Spans().At(1).SetTraceID(pcommon.TraceID{})
	r := ValidateTraces(td, fixtureTime.Add(time.Second), DefaultRules)
	if r.Accepted != 3 || r.Rejected != 1 || td.SpanCount() != 3 {
		t.Fatalf("result = %+v, remaining spans = %d", r, td.SpanCount())
	}
	if r.Message() != "rejected invalid_trace_id=1" {
		t.Errorf("message = %q", r.Message())
	}
}

func TestLogRules(t *testing.T) {
	newLog := func(mutate func(plog.LogRecord)) plog.Logs {
		ld := plog.NewLogs()
		lr := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
		lr.SetTimestamp(pcommon.NewTimestampFromTime(fixtureTime))
		lr.Body().SetStr("ok")
		mutate(lr)
		return ld
	}
	cases := []struct {
		name   string
		mutate func(plog.LogRecord)
		want   Reason // ""이면 통과
	}{
		{"body over 32KiB", func(lr plog.LogRecord) { lr.Body().SetStr(strings.Repeat("a", 32<<10+1)) }, ReasonLogBodyTooLarge},
		{"structured body over 32KiB", func(lr plog.LogRecord) {
			m := lr.Body().SetEmptyMap()
			for i := range 9 {
				m.PutStr("f"+string(rune('a'+i)), strings.Repeat("a", 4<<10))
			}
		}, ReasonLogBodyTooLarge},
		// 연결만 불가능할 뿐 고객 log를 버리지 않는다 (ADR 0017).
		{"span id without trace id is accepted", func(lr plog.LogRecord) { lr.SetSpanID(pcommon.SpanID{1}) }, ""},
		{"future beyond 5m", func(lr plog.LogRecord) {
			lr.SetTimestamp(pcommon.NewTimestampFromTime(fixtureTime.Add(5*time.Minute + time.Nanosecond)))
		}, ReasonTimeOutOfRange},
		{"too old", func(lr plog.LogRecord) {
			lr.SetTimestamp(pcommon.NewTimestampFromTime(fixtureTime.Add(-25 * time.Hour)))
		}, ReasonTimeOutOfRange},
		{"observed time used when timestamp absent", func(lr plog.LogRecord) {
			lr.SetTimestamp(0)
			lr.SetObservedTimestamp(pcommon.NewTimestampFromTime(fixtureTime.Add(-25 * time.Hour)))
		}, ReasonTimeOutOfRange},
		{"no timestamps at all is accepted", func(lr plog.LogRecord) { lr.SetTimestamp(0) }, ""},
		{"body exactly 32KiB", func(lr plog.LogRecord) { lr.Body().SetStr(strings.Repeat("a", 32<<10)) }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := ValidateLogs(newLog(tc.mutate), fixtureTime, DefaultRules)
			if tc.want == "" {
				if r.Accepted != 1 {
					t.Fatalf("want accepted: %+v", r)
				}
				return
			}
			if r.Rejected != 1 || r.Reasons[tc.want] != 1 {
				t.Fatalf("result = %+v, want %s", r, tc.want)
			}
		})
	}
}

func TestMetricRules(t *testing.T) {
	newHist := func(count uint64, buckets []uint64, bounds []float64) pmetric.Metrics {
		md := pmetric.NewMetrics()
		m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
		m.SetName("http.server.request.duration")
		p := m.SetEmptyHistogram().DataPoints().AppendEmpty()
		p.SetTimestamp(pcommon.NewTimestampFromTime(fixtureTime))
		p.SetCount(count)
		p.BucketCounts().FromRaw(buckets)
		p.ExplicitBounds().FromRaw(bounds)
		return md
	}
	cases := []struct {
		name string
		md   pmetric.Metrics
		want Reason
	}{
		{"valid", newHist(10, []uint64{5, 5}, []float64{1}), ""},
		{"count only", newHist(10, nil, nil), ""},
		{"bucket sum != count", newHist(11, []uint64{5, 5}, []float64{1}), ReasonInvalidHistogram},
		{"bucket/bound length mismatch", newHist(10, []uint64{10}, []float64{1}), ReasonInvalidHistogram},
		{"bounds not ascending", newHist(3, []uint64{1, 1, 1}, []float64{2, 1}), ReasonInvalidHistogram},
		{"bounds without buckets", newHist(0, nil, []float64{1}), ReasonInvalidHistogram},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := ValidateMetrics(tc.md, fixtureTime, DefaultRules)
			if tc.want == "" {
				if r.Accepted != 1 {
					t.Fatalf("want accepted: %+v", r)
				}
				return
			}
			if r.Rejected != 1 || r.Reasons[tc.want] != 1 || tc.md.ResourceMetrics().Len() != 0 {
				t.Fatalf("result = %+v (resources left %d), want %s", r, tc.md.ResourceMetrics().Len(), tc.want)
			}
		})
	}
	t.Run("missing name rejects every point", func(t *testing.T) {
		md := pmetric.NewMetrics()
		g := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty().SetEmptyGauge()
		for range 3 {
			g.DataPoints().AppendEmpty().SetTimestamp(pcommon.NewTimestampFromTime(fixtureTime))
		}
		if r := ValidateMetrics(md, fixtureTime, DefaultRules); r.Reasons[ReasonMissingMetricName] != 3 {
			t.Fatalf("result = %+v", r)
		}
	})
}

// ---- pre-scan: 증폭·깊이 (ADR 0017) ----

func protoMsg(field protowire.Number, payload []byte) []byte {
	b := protowire.AppendTag(nil, field, protowire.BytesType)
	return protowire.AppendBytes(b, payload)
}

// deepLogProto는 kvlist를 levels단 중첩한 body를 가진 log 요청을 만든다.
// 깊이: request→ResourceLogs→ScopeLogs→LogRecord→AnyValue(4) + 단계마다 KeyValueList→KeyValue→AnyValue(3).
func deepLogProto(levels int) []byte {
	any := protoMsg(1, []byte("leaf")) // AnyValue{string_value}
	for range levels {
		kv := append(protoMsg(1, []byte("k")), protoMsg(2, any)...) // KeyValue{key, value}
		any = protoMsg(6, protoMsg(1, kv))                          // AnyValue{kvlist_value{values}}
	}
	lr := protoMsg(5, any)
	return protoMsg(1, protoMsg(2, protoMsg(2, lr)))
}

func deepLogJSON(levels int) []byte {
	body := `{"stringValue":"leaf"}`
	for range levels {
		body = fmt.Sprintf(`{"kvlistValue":{"values":[{"key":"k","value":%s}]}}`, body)
	}
	return []byte(`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":` + body + `}]}]}]}`)
}

func TestPrescanRejectsAmplification(t *testing.T) {
	// 빈 ResourceSpans(0x0a 0x00)만 반복한 본문: 바이트는 작아도 decode 후 메모리가 수십 배로 커진다.
	n := DefaultMaxElements + 1
	body := bytes.Repeat([]byte{0x0a, 0x00}, n)
	_, err := Decode(bytes.NewReader(body), "application/x-protobuf", "", SignalTraces, DefaultLimits)
	if !errors.Is(err, ErrTooComplex) {
		t.Fatalf("proto amplification: %v", err)
	}
	js := []byte(`{"resourceSpans":[` + strings.TrimSuffix(strings.Repeat("{},", n), ",") + `]}`)
	_, err = Decode(bytes.NewReader(js), "application/json", "", SignalTraces, Limits{MaxWireBytes: 16 << 20, MaxDecodedBytes: 16 << 20})
	if !errors.Is(err, ErrTooComplex) {
		t.Fatalf("json amplification: %v", err)
	}
	// 같은 모양이라도 한도 안이면 통과한다.
	ok := bytes.Repeat([]byte{0x0a, 0x00}, 1000)
	if _, err := Decode(bytes.NewReader(ok), "application/x-protobuf", "", SignalTraces, DefaultLimits); err != nil {
		t.Fatalf("1000 empty resources: %v", err)
	}
}

func TestPrescanRejectsDeepNesting(t *testing.T) {
	cases := []struct {
		name string
		ct   string
		body []byte
		want error
	}{
		{"proto 5 levels ok", "application/x-protobuf", deepLogProto(5), nil},
		{"proto 1000 levels", "application/x-protobuf", deepLogProto(1000), ErrTooComplex},
		{"json 5 levels ok", "application/json", deepLogJSON(5), nil},
		{"json 1000 levels", "application/json", deepLogJSON(1000), ErrTooComplex},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Decode(bytes.NewReader(tc.body), tc.ct, "", SignalLogs, DefaultLimits)
			if !errors.Is(err, tc.want) { // errors.Is(nil, nil) == true
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want == nil && p.Logs.LogRecordCount() != 1 {
				t.Fatalf("records = %d", p.Logs.LogRecordCount())
			}
		})
	}
}

func TestPrescanMalformedProto(t *testing.T) {
	// 길이가 본문보다 긴 length-delimited 필드
	_, err := Decode(bytes.NewReader([]byte{0x0a, 0x7f, 0x01}), "application/x-protobuf", "", SignalTraces, DefaultLimits)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v", err)
	}
}

// ---- 요청 경계 ----

func TestEmptyBodyIsEmptyRequest(t *testing.T) {
	for _, ct := range []string{"application/json", "application/x-protobuf"} {
		p, err := Decode(bytes.NewReader(nil), ct, "", SignalTraces, DefaultLimits)
		if err != nil || p.Traces.SpanCount() != 0 {
			t.Errorf("%s: spans=%d err=%v", ct, p.Traces.SpanCount(), err)
		}
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestBodyReadError(t *testing.T) {
	_, err := Decode(failingReader{}, "application/json", "", SignalTraces, DefaultLimits)
	if !errors.Is(err, ErrBodyRead) {
		t.Fatalf("err = %v", err)
	}
}

func TestExactLimitsPass(t *testing.T) {
	body := fixture(t, "otlp", "traces_checkout.json")
	n := int64(len(body))
	if _, err := Decode(bytes.NewReader(body), "application/json", "", SignalTraces, Limits{MaxWireBytes: n, MaxDecodedBytes: n}); err != nil {
		t.Fatalf("wire == decoded == limit: %v", err)
	}
	zipped := gz(t, body)
	if _, err := Decode(bytes.NewReader(zipped), "application/json", "gzip", SignalTraces,
		Limits{MaxWireBytes: int64(len(zipped)), MaxDecodedBytes: n}); err != nil {
		t.Fatalf("gzip at exact limits: %v", err)
	}
	if _, err := Decode(bytes.NewReader(body), "application/json", "", SignalTraces, Limits{MaxWireBytes: 0, MaxDecodedBytes: n}); err == nil {
		t.Fatal("non-positive limit must be a configuration error")
	}
}

func TestGzipBombWithDefaultLimits(t *testing.T) {
	bomb := gz(t, bytes.Repeat([]byte{'0'}, 9<<20))
	_, err := Decode(bytes.NewReader(bomb), "application/json", "gzip", SignalTraces, DefaultLimits)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

// ---- record 경계·크기 ----

func TestSpanBoundaries(t *testing.T) {
	r := ValidateTraces(oneSpan(func(s ptrace.Span) {
		for i := range 128 {
			s.Attributes().PutInt(fmt.Sprintf("k%03d", i), int64(i))
		}
	}), fixtureTime, DefaultRules)
	if r.Accepted != 1 {
		t.Fatalf("exactly 128 attributes must pass: %+v", r)
	}
	for _, tc := range []struct {
		name string
		at   time.Time
		ok   bool
	}{
		{"exactly +5m", fixtureTime.Add(5 * time.Minute), true},
		{"exactly -24h", fixtureTime.Add(-24 * time.Hour), true},
		{"-24h-1ns", fixtureTime.Add(-24*time.Hour - time.Nanosecond), false},
	} {
		td := oneSpan(func(s ptrace.Span) {
			s.SetStartTimestamp(pcommon.NewTimestampFromTime(tc.at))
			s.SetEndTimestamp(pcommon.NewTimestampFromTime(tc.at))
		})
		if got := ValidateTraces(td, fixtureTime, DefaultRules).Accepted == 1; got != tc.ok {
			t.Errorf("%s: accepted=%v want %v", tc.name, got, tc.ok)
		}
	}
}

func TestSpanSizeCountsAllVariableFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(ptrace.Span)
	}{
		{"huge status message", func(s ptrace.Span) { s.Status().SetMessage(strings.Repeat("m", 65<<10)) }},
		{"huge trace state", func(s ptrace.Span) { s.TraceState().FromRaw(strings.Repeat("a", 65<<10)) }},
		{"many empty events", func(s ptrace.Span) {
			for range 5000 {
				s.Events().AppendEmpty()
			}
		}},
		{"many empty links", func(s ptrace.Span) {
			for range 2000 {
				s.Links().AppendEmpty()
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := ValidateTraces(oneSpan(tc.mutate), fixtureTime, DefaultRules)
			if r.Reasons[ReasonRecordTooLarge] != 1 {
				t.Fatalf("result = %+v", r)
			}
		})
	}
}

func TestResourceAndScopeAttributes(t *testing.T) {
	td := decode(t, fixture(t, "otlp", "traces_checkout.json"), "application/json", "", SignalTraces).Traces
	td.ResourceSpans().At(1).Resource().Attributes().PutStr("huge", strings.Repeat("r", 4<<10+1))
	td.ResourceSpans().At(0).ScopeSpans().At(0).Scope().Attributes().PutStr("ok", "small")
	r := ValidateTraces(td, fixtureTime.Add(time.Second), DefaultRules)
	// payment resource 아래 span 2개 전체 거절, checkout은 유지
	if r.Accepted != 2 || r.Reasons[ReasonResourceInvalid] != 2 || td.ResourceSpans().Len() != 1 {
		t.Fatalf("result = %+v, resources=%d", r, td.ResourceSpans().Len())
	}
}

// resource와 scope 속성이 모든 envelope에 복제되므로 합이 1MiB(D02 §05)를 넘으면 record를 거절한다.
func TestEnvelopeLimit(t *testing.T) {
	fill := func(m pcommon.Map, prefix string) {
		for i := range 128 {
			m.PutStr(fmt.Sprintf("%s%03d", prefix, i), strings.Repeat("v", 4<<10))
		}
	}
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	fill(rl.Resource().Attributes(), "r")
	sl := rl.ScopeLogs().AppendEmpty()
	fill(sl.Scope().Attributes(), "s")
	lr := sl.LogRecords().AppendEmpty()
	lr.SetTimestamp(pcommon.NewTimestampFromTime(fixtureTime))
	fill(lr.Attributes(), "a")
	r := ValidateLogs(ld, fixtureTime, DefaultRules)
	if r.Reasons[ReasonRecordTooLarge] != 1 {
		t.Fatalf("result = %+v", r)
	}
}

func TestLogWithoutTimestampsGetsObservedTime(t *testing.T) {
	ld := plog.NewLogs()
	lr := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.Body().SetStr("no time")
	if r := ValidateLogs(ld, fixtureTime, DefaultRules); r.Accepted != 1 {
		t.Fatalf("result = %+v", r)
	}
	if got := lr.ObservedTimestamp().AsTime(); !got.Equal(fixtureTime) || lr.Timestamp() != 0 {
		t.Fatalf("observed=%v timestamp=%v", got, lr.Timestamp())
	}
}

func TestStructuredBodyCountsEmptyElements(t *testing.T) {
	ld := plog.NewLogs()
	lr := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetTimestamp(pcommon.NewTimestampFromTime(fixtureTime))
	s := lr.Body().SetEmptySlice()
	for range 5000 { // 빈 문자열 원소도 고정 비용이 있다
		s.AppendEmpty().SetStr("")
	}
	if r := ValidateLogs(ld, fixtureTime, DefaultRules); r.Reasons[ReasonLogBodyTooLarge] != 1 {
		t.Fatalf("result = %+v", r)
	}
}

func TestMetricOtherTypesAndFuture(t *testing.T) {
	md := pmetric.NewMetrics()
	ms := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics()
	eh := ms.AppendEmpty()
	eh.SetName("latency.exp")
	ep := eh.SetEmptyExponentialHistogram().DataPoints().AppendEmpty()
	ep.SetTimestamp(pcommon.NewTimestampFromTime(fixtureTime))
	ep.Positive().BucketCounts().FromRaw([]uint64{1, 2, 3})
	sm := ms.AppendEmpty()
	sm.SetName("latency.summary")
	sp := sm.SetEmptySummary().DataPoints().AppendEmpty()
	sp.SetTimestamp(pcommon.NewTimestampFromTime(fixtureTime))
	sp.QuantileValues().AppendEmpty().SetQuantile(0.95)
	g := ms.AppendEmpty()
	g.SetName("future.gauge")
	g.SetEmptyGauge().DataPoints().AppendEmpty().SetTimestamp(pcommon.NewTimestampFromTime(fixtureTime.Add(6 * time.Minute)))
	big := ms.AppendEmpty()
	big.SetName("huge.exp")
	bp := big.SetEmptyExponentialHistogram().DataPoints().AppendEmpty()
	bp.SetTimestamp(pcommon.NewTimestampFromTime(fixtureTime))
	bp.Positive().BucketCounts().FromRaw(make([]uint64, 200_000)) // 1.6MB 근사 > 1MiB envelope

	r := ValidateMetrics(md, fixtureTime, DefaultRules)
	if r.Accepted != 2 || r.Reasons[ReasonTimeOutOfRange] != 1 || r.Reasons[ReasonRecordTooLarge] != 1 {
		t.Fatalf("result = %+v", r)
	}
}
