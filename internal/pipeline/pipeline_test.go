package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetry/envelope"
)

var (
	tenantA = mustTenant("11111111-1111-4111-8111-111111111111")
	tenantB = mustTenant("22222222-2222-4222-8222-222222222222")
	now     = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
)

func mustTenant(s string) authz.TenantID {
	t, err := authz.ParseTenantID(s)
	if err != nil {
		panic(err)
	}
	return t
}

func envMeta(t authz.TenantID, received time.Time) envelope.Meta {
	return envelope.Meta{Tenant: t, ReceivedAt: received, PolicyVersion: 3, RoutingEpoch: 1}
}

// toMsgs는 envelope record를 Kafka가 돌려줄 형태(partition·offset 부여)로 바꾼다.
func toMsgs(partition int32, firstOffset int64, recs ...envelope.Record) []Message {
	out := make([]Message, len(recs))
	for i, r := range recs {
		hs := append([]envelope.Header(nil), r.Headers...)
		out[i] = Message{Topic: r.Topic, Partition: partition, Offset: firstOffset + int64(i), Key: r.Key, Value: r.Value, Headers: hs}
	}
	return out
}

func span(tid pcommon.TraceID, sid pcommon.SpanID, name string) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	rs.Resource().Attributes().PutStr("deployment.environment.name", "prod")
	s := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	s.SetTraceID(tid)
	s.SetSpanID(sid)
	s.SetParentSpanID(pcommon.SpanID{9})
	s.SetName(name)
	s.SetKind(ptrace.SpanKindServer)
	s.Status().SetCode(ptrace.StatusCodeError)
	s.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	s.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(250 * time.Millisecond)))
	s.Attributes().PutInt("http.response.status_code", 500)
	return td
}

func spanRecords(t *testing.T, td ptrace.Traces, m envelope.Meta) []envelope.Record {
	t.Helper()
	res, err := envelope.Traces(td, m)
	if err != nil {
		t.Fatal(err)
	}
	return res.Records
}

var builder = Builder{Retention: DefaultRetention, Now: func() time.Time { return now }}

func TestSpanNormalized(t *testing.T) {
	recs := spanRecords(t, span(pcommon.TraceID{0xab, 1}, pcommon.SpanID{1}, "GET /cart"), envMeta(tenantA, now))
	bs := builder.Build(toMsgs(0, 10, recs...))
	if len(bs) != 1 || len(bs[0].Spans) != 1 || len(bs[0].Quarantine) != 0 {
		t.Fatalf("batches = %+v", bs)
	}
	r := bs[0].Spans[0]
	if r.Tenant != tenantA || r.Name != "GET /cart" || r.DurationNs != uint64(250*time.Millisecond) ||
		r.Status != 2 || r.Kind != uint8(ptrace.SpanKindServer) || r.ParentSpanID != [8]byte{9} {
		t.Errorf("row = %+v", r)
	}
	if r.Attributes["http.response.status_code"] != "500" {
		t.Errorf("attributes = %v", r.Attributes)
	}
	if !bytes.Equal(r.Payload, recs[0].Value) || r.ExpiresAt != now.Add(7*24*time.Hour) || !r.ReceivedAt.Equal(now) {
		t.Errorf("payload/expires/received = %v %v", r.ExpiresAt, r.ReceivedAt)
	}
	if r.ServiceID != ServiceID(tenantA, ptraceResource(t, recs[0].Value)) {
		t.Errorf("service id mismatch")
	}
}

func ptraceResource(t *testing.T, v []byte) pcommon.Map {
	t.Helper()
	td, err := (&ptrace.ProtoUnmarshaler{}).UnmarshalTraces(v)
	if err != nil {
		t.Fatal(err)
	}
	return td.ResourceSpans().At(0).Resource().Attributes()
}

// 계약: dedup key는 (tenant, event_id)다. 다른 tenant가 같은 trace·span ID를 써도 서로를 지우지 않는다 (ADR 0020 §4).
func TestDedupKeyIncludesTenant(t *testing.T) {
	td := span(pcommon.TraceID{0xab, 1}, pcommon.SpanID{1}, "x")
	a := spanRecords(t, td, envMeta(tenantA, now))
	b := spanRecords(t, td, envMeta(tenantB, now))
	if a[0].EventID != b[0].EventID {
		t.Fatal("시험 전제: 두 tenant의 event_id가 같아야 한다")
	}
	bs := builder.Build(toMsgs(0, 0, a[0], b[0]))
	if len(bs[0].Spans) != 2 || bs[0].Duplicates != 0 {
		t.Fatalf("spans = %d, duplicates = %d", len(bs[0].Spans), bs[0].Duplicates)
	}
	if bs[0].Spans[0].Tenant == bs[0].Spans[1].Tenant {
		t.Error("두 행의 tenant가 같다")
	}
	if bs[0].Spans[0].ServiceID == bs[0].Spans[1].ServiceID {
		t.Error("service_id는 tenant마다 달라야 한다")
	}
}

// 같은 tenant의 재전송은 batch 안에서 한 행으로 줄이고, 먼저 수신한 값을 남긴다 (D02 §05, §21).
func TestDuplicateKeepsFirstReceived(t *testing.T) {
	first := spanRecords(t, span(pcommon.TraceID{1}, pcommon.SpanID{1}, "first"), envMeta(tenantA, now))
	resent := spanRecords(t, span(pcommon.TraceID{1}, pcommon.SpanID{1}, "first"), envMeta(tenantA, now.Add(time.Second)))
	changed := spanRecords(t, span(pcommon.TraceID{1}, pcommon.SpanID{1}, "changed"), envMeta(tenantA, now.Add(2*time.Second)))
	// 나중에 수신한 record가 먼저 offset에 놓여도(다른 ingress) 결과는 같아야 한다.
	bs := builder.Build(toMsgs(0, 0, changed[0], resent[0], first[0]))
	b := bs[0]
	if len(b.Spans) != 1 || b.Duplicates != 2 || b.Conflicts != 1 {
		t.Fatalf("spans=%d duplicates=%d conflicts=%d", len(b.Spans), b.Duplicates, b.Conflicts)
	}
	if b.Spans[0].Name != "first" || !b.Spans[0].ReceivedAt.Equal(now) {
		t.Errorf("kept = %s @ %v", b.Spans[0].Name, b.Spans[0].ReceivedAt)
	}
	// version: 먼저 수신할수록 크다 → 저장소에서도 최초 수신 행이 남는다
	if version(now, 99) <= version(now.Add(time.Millisecond), 0) {
		t.Error("version must decrease with later receipt")
	}
	// 같은 ms면 Kafka log에 먼저 append된 쪽(작은 offset)이 이긴다
	if version(now, 5) <= version(now, 6) {
		t.Error("same-ms tie must be broken by offset")
	}
}

// 같은 ms에 같은 span key로 다른 내용이 오면 먼저 append된 record가 남는다 (ADR 0021 §4).
func TestSameMillisecondTieBrokenByOffset(t *testing.T) {
	first := spanRecords(t, span(pcommon.TraceID{1}, pcommon.SpanID{1}, "first"), envMeta(tenantA, now))
	second := spanRecords(t, span(pcommon.TraceID{1}, pcommon.SpanID{1}, "second"), envMeta(tenantA, now))
	b := builder.Build(toMsgs(0, 0, first[0], second[0]))[0]
	if len(b.Spans) != 1 || b.Spans[0].Name != "first" || b.Conflicts != 1 {
		t.Fatalf("kept=%v conflicts=%d", b.Spans, b.Conflicts)
	}
}

func TestBatchPerPartitionAndToken(t *testing.T) {
	r1 := spanRecords(t, span(pcommon.TraceID{1}, pcommon.SpanID{1}, "a"), envMeta(tenantA, now))
	r2 := spanRecords(t, span(pcommon.TraceID{2}, pcommon.SpanID{1}, "b"), envMeta(tenantA, now))
	msgs := append(toMsgs(3, 100, r1[0]), toMsgs(1, 7, r2[0])...)
	msgs = append(msgs, toMsgs(3, 101, r2[0])...)
	bs := builder.Build(msgs)
	if len(bs) != 2 || bs[0].Partition != 1 || bs[1].Partition != 3 {
		t.Fatalf("batches = %+v", bs)
	}
	if bs[1].FirstOffset != 100 || bs[1].LastOffset != 101 || len(bs[1].Spans) != 2 {
		t.Errorf("partition 3 = %d-%d (%d spans)", bs[1].FirstOffset, bs[1].LastOffset, len(bs[1].Spans))
	}
	if got := bs[1].Token("spans_local"); !strings.HasPrefix(got, envelope.TopicTraces+"/3/100-101/spans_local/") || len(got) != len(envelope.TopicTraces+"/3/100-101/spans_local/")+32 {
		t.Errorf("token = %s", got)
	}
	// topic이 다시 만들어져 같은 offset에 다른 record가 오면 token이 달라야 한다(같으면 ClickHouse가 조용히 버려 유실된다)
	other := spanRecords(t, span(pcommon.TraceID{9}, pcommon.SpanID{9}, "after-topic-reset"), envMeta(tenantA, now))
	reused := builder.Build(append(toMsgs(3, 100, other[0]), toMsgs(3, 101, r2[0])...))
	if reused[0].Token("spans_local") == bs[1].Token("spans_local") {
		t.Error("different content at reused offsets must not share a token")
	}
	if bs[1].Token("spans_local") == bs[1].Token("logs_local") {
		t.Error("token must differ per table")
	}
	// 같은 범위를 다시 읽으면 같은 내용·같은 token이다(crash 후 재처리)
	again := builder.Build(msgs)
	if again[1].Token("spans_local") != bs[1].Token("spans_local") || again[1].Spans[1].Name != bs[1].Spans[1].Name {
		t.Error("batch is not deterministic")
	}
}

func setHeader(m *Message, key, val string) {
	for i := range m.Headers {
		if m.Headers[i].Key == key {
			m.Headers[i].Value = []byte(val)
			return
		}
	}
	m.Headers = append(m.Headers, envelope.Header{Key: key, Value: []byte(val)})
}

func dropHeader(m *Message, key string) {
	out := m.Headers[:0]
	for _, h := range m.Headers {
		if h.Key != key {
			out = append(out, h)
		}
	}
	m.Headers = out
}

// worker는 header만 믿는다. header·key·payload가 어긋나면 저장하지 않고 원문 없이 quarantine한다.
func TestQuarantine(t *testing.T) {
	recs := spanRecords(t, span(pcommon.TraceID{1}, pcommon.SpanID{1}, "a"), envMeta(tenantA, now))
	other := spanRecords(t, span(pcommon.TraceID{1}, pcommon.SpanID{2}, "a"), envMeta(tenantA, now))
	cases := []struct {
		name   string
		mutate func(m *Message)
		reason string
	}{
		{"unknown topic", func(m *Message) { m.Topic = "telemetry.unknown.raw.v1" }, ReasonUnknownTopic},
		{"no tenant header", func(m *Message) { dropHeader(m, envelope.HeaderTenant) }, ReasonMissingHeader},
		{"tenant header differs from key", func(m *Message) { setHeader(m, envelope.HeaderTenant, tenantB.String()) }, ReasonTenantKeyMismatch},
		{"duplicated tenant header", func(m *Message) {
			m.Headers = append(m.Headers, envelope.Header{Key: envelope.HeaderTenant, Value: []byte(tenantA.String())})
		}, ReasonMissingHeader},
		{"invalid tenant", func(m *Message) { setHeader(m, envelope.HeaderTenant, "not-a-uuid") }, ReasonInvalidHeader},
		{"unknown schema", func(m *Message) { setHeader(m, envelope.HeaderSchema, "2") }, ReasonUnknownSchema},
		{"signal mismatch", func(m *Message) { setHeader(m, envelope.HeaderSignal, "logs") }, ReasonSignalMismatch},
		{"bad received", func(m *Message) { setHeader(m, envelope.HeaderReceivedAtMs, "x") }, ReasonInvalidHeader},
		{"event id of another span", func(m *Message) { setHeader(m, envelope.HeaderEventID, other[0].EventID) }, ReasonEventIDMismatch},
		{"long event id", func(m *Message) { setHeader(m, envelope.HeaderEventID, strings.Repeat("a", 300)) }, ReasonInvalidHeader},
		{"corrupt value", func(m *Message) { m.Value = []byte{0xff, 0xff, 0xff} }, ReasonDecode},
		{"two spans in one value", func(m *Message) {
			td := span(pcommon.TraceID{1}, pcommon.SpanID{1}, "a")
			td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().AppendEmpty()
			m.Value, _ = (&ptrace.ProtoMarshaler{}).MarshalTraces(td)
		}, ReasonShape},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msgs := toMsgs(0, 42, recs[0])
			c.mutate(&msgs[0])
			bs := builder.Build(msgs)
			b := bs[0]
			if len(b.Spans) != 0 || len(b.Quarantine) != 1 {
				t.Fatalf("spans=%d quarantine=%d", len(b.Spans), len(b.Quarantine))
			}
			q := b.Quarantine[0]
			if q.Reason != c.reason || q.Offset != 42 || q.PayloadBytes != uint32(len(msgs[0].Value)) { //nolint:gosec // 시험 값
				t.Errorf("quarantine = %+v", q)
			}
			if !q.ExpiresAt.Equal(now.Add(24 * time.Hour)) {
				t.Errorf("expires = %v", q.ExpiresAt)
			}
		})
	}
}

func TestLogs(t *testing.T) {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "checkout")
	sl := rl.ScopeLogs().AppendEmpty()
	withUID := sl.LogRecords().AppendEmpty()
	withUID.SetTimestamp(pcommon.NewTimestampFromTime(now))
	withUID.SetSeverityNumber(plog.SeverityNumberError)
	withUID.Body().SetStr("payment failed")
	withUID.Attributes().PutStr(envelope.LogRecordUIDAttr, "01JABCDEF")
	withUID.SetTraceID(pcommon.TraceID{7})
	noUID := sl.LogRecords().AppendEmpty()
	noUID.SetObservedTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Second)))
	noUID.Body().SetStr("tick")

	res, err := envelope.Logs(ld, envMeta(tenantA, now), nil)
	if err != nil {
		t.Fatal(err)
	}
	bs := builder.Build(toMsgs(0, 0, res.Records...))
	logs := bs[0].Logs
	if len(logs) != 2 || len(bs[0].Quarantine) != 0 {
		t.Fatalf("logs=%d quarantine=%+v", len(logs), bs[0].Quarantine)
	}
	if logs[0].EventID != "uid:01JABCDEF" || logs[0].Severity != uint8(plog.SeverityNumberError) ||
		logs[0].Body != "payment failed" || logs[0].TraceID != [16]byte{7} {
		t.Errorf("log0 = %+v", logs[0])
	}
	if !strings.HasPrefix(logs[1].EventID, "gen:") || !logs[1].EventTime.Equal(now.Add(time.Second)) {
		t.Errorf("log1 = %+v (timestamp 없으면 observed)", logs[1])
	}

	// header의 uid와 payload의 uid가 다르면 quarantine
	msgs := toMsgs(0, 0, res.Records[0])
	setHeader(&msgs[0], envelope.HeaderEventID, "uid:someone-else")
	if b := builder.Build(msgs)[0]; len(b.Quarantine) != 1 || b.Quarantine[0].Reason != ReasonEventIDMismatch {
		t.Errorf("uid mismatch not quarantined: %+v", b)
	}
}

func logWithUID(uid string) plog.Logs {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "checkout")
	lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetTimestamp(pcommon.NewTimestampFromTime(now))
	lr.Body().SetStr("payment failed")
	lr.Attributes().PutStr(envelope.LogRecordUIDAttr, uid)
	return ld
}

func allMetricTypes() pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "checkout")
	sm := rm.ScopeMetrics().AppendEmpty()
	ts := func(dp interface {
		SetStartTimestamp(pcommon.Timestamp)
		SetTimestamp(pcommon.Timestamp)
	}) {
		dp.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
		dp.SetTimestamp(pcommon.NewTimestampFromTime(now.Add(15 * time.Second)))
	}

	g := sm.Metrics().AppendEmpty()
	g.SetName("jvm.memory.used")
	g.SetUnit("By")
	gdp := g.SetEmptyGauge().DataPoints().AppendEmpty()
	gdp.SetIntValue(1024)
	ts(gdp)

	s := sm.Metrics().AppendEmpty()
	s.SetName("http.requests")
	sum := s.SetEmptySum()
	sum.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
	sum.SetIsMonotonic(true)
	sdp := sum.DataPoints().AppendEmpty()
	sdp.SetDoubleValue(3)
	sdp.Attributes().PutStr("http.route", "/cart")
	ts(sdp)

	h := sm.Metrics().AppendEmpty()
	h.SetName("http.server.request.duration")
	hh := h.SetEmptyHistogram()
	hh.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	hdp := hh.DataPoints().AppendEmpty()
	hdp.SetCount(3)
	hdp.ExplicitBounds().FromRaw([]float64{0.1, 1})
	hdp.BucketCounts().FromRaw([]uint64{1, 1, 1})
	ts(hdp) // sum 없음

	e := sm.Metrics().AppendEmpty()
	e.SetName("rpc.duration")
	edp := e.SetEmptyExponentialHistogram().DataPoints().AppendEmpty()
	edp.SetCount(2)
	edp.SetSum(1.5)
	edp.SetScale(3)
	edp.Positive().SetOffset(1)
	edp.Positive().BucketCounts().FromRaw([]uint64{2})
	ts(edp)

	q := sm.Metrics().AppendEmpty()
	q.SetName("legacy.latency")
	qdp := q.SetEmptySummary().DataPoints().AppendEmpty()
	qdp.SetCount(4)
	qdp.SetSum(2)
	qv := qdp.QuantileValues().AppendEmpty()
	qv.SetQuantile(0.99)
	qv.SetValue(0.9)
	ts(qdp)
	return md
}

func TestMetrics(t *testing.T) {
	res, err := envelope.Metrics(allMetricTypes(), envMeta(tenantA, now))
	if err != nil || len(res.Records) != 5 {
		t.Fatalf("records = %d, %v", len(res.Records), err)
	}
	var msgs []Message
	for i, r := range res.Records {
		msgs = append(msgs, toMsgs(0, int64(i), r)...)
	}
	b := builder.Build(msgs)[0]
	if len(b.Metrics) != 5 || len(b.Quarantine) != 0 {
		t.Fatalf("metrics=%d quarantine=%+v", len(b.Metrics), b.Quarantine)
	}
	byName := map[string]MetricRow{}
	for _, r := range b.Metrics {
		byName[r.Name] = r
	}
	g := byName["jvm.memory.used"]
	if g.Type != "gauge" || g.Value != 1024 || g.Unit != "By" || !math.IsNaN(g.Sum) || g.Payload != "" {
		t.Errorf("gauge = %+v", g)
	}
	s := byName["http.requests"]
	if s.Type != "sum" || s.Temporality != "delta" || !s.IsMonotonic || s.Value != 3 || s.AttributesJSON != `{"http.route":"/cart"}` {
		t.Errorf("sum = %+v", s)
	}
	if s.ResourceJSON != `{"service.name":"checkout"}` || !s.EndTime.Equal(now.Add(15*time.Second)) ||
		!s.ExpiresAt.Equal(now.Add(15*time.Second).Add(15*24*time.Hour)) {
		t.Errorf("sum resource/time = %s %v %v", s.ResourceJSON, s.EndTime, s.ExpiresAt)
	}
	h := byName["http.server.request.duration"]
	if h.Type != "histogram" || h.Count != 3 || !math.IsNaN(h.Sum) || !math.IsNaN(h.Value) || len(h.Buckets) != 3 || len(h.Bounds) != 2 {
		t.Errorf("histogram (sum 없음 → NaN, 0 아님) = %+v", h)
	}
	e := byName["rpc.duration"]
	if e.Type != "exponential_histogram" || e.Sum != 1.5 || !strings.Contains(e.Payload, `"scale":3`) {
		t.Errorf("exp histogram = %+v", e)
	}
	q := byName["legacy.latency"]
	if q.Type != "summary" || q.Count != 4 || !strings.Contains(q.Payload, "quantileValues") {
		t.Errorf("summary = %+v", q)
	}
	for _, r := range b.Metrics {
		if r.StreamID == ([16]byte{}) || r.PointHash == ([16]byte{}) {
			t.Errorf("%s: empty stream/point hash", r.Name)
		}
	}

	// event_id의 시각이 payload와 다르면 quarantine
	m := toMsgs(0, 0, res.Records[0])
	parts := strings.Split(res.Records[0].EventID, "-")
	setHeader(&m[0], envelope.HeaderEventID, parts[0]+"-1-2-"+parts[3])
	if qb := builder.Build(m)[0]; len(qb.Quarantine) != 1 || qb.Quarantine[0].Reason != ReasonEventIDMismatch {
		t.Errorf("metric event id mismatch not quarantined")
	}
}

// 같은 stream·같은 시각에 다른 값이 오면 둘 다 저장하되 충돌로 센다 (D02 §05).
func TestMetricConflictCounted(t *testing.T) {
	gauge := func(v int64) pmetric.Metrics {
		md := pmetric.NewMetrics()
		m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
		m.SetName("queue.depth")
		dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
		dp.SetIntValue(v)
		dp.SetTimestamp(pcommon.NewTimestampFromTime(now))
		return md
	}
	a, _ := envelope.Metrics(gauge(1), envMeta(tenantA, now))
	same, _ := envelope.Metrics(gauge(1), envMeta(tenantA, now))
	b, _ := envelope.Metrics(gauge(2), envMeta(tenantA, now))
	other, _ := envelope.Metrics(gauge(3), envMeta(tenantB, now)) // 다른 tenant는 충돌이 아니다
	bt := builder.Build(toMsgs(0, 0, a.Records[0], same.Records[0], b.Records[0], other.Records[0]))[0]
	if len(bt.Metrics) != 2 || bt.Duplicates != 1 || bt.Conflicts != 1 {
		t.Fatalf("metrics=%d duplicates=%d conflicts=%d", len(bt.Metrics), bt.Duplicates, bt.Conflicts)
	}
	// Prometheus·Mimir처럼 먼저 받은 값(1)을 남기고 나중 값(2)은 사유와 함께 버린다
	if bt.Metrics[0].Value != 1 || len(bt.Quarantine) != 1 || bt.Quarantine[0].Reason != ReasonConflictingPoint ||
		bt.Quarantine[0].Offset != 2 || bt.Quarantine[0].EventID != b.Records[0].EventID {
		t.Errorf("kept=%v quarantine=%+v", bt.Metrics[0].Value, bt.Quarantine)
	}

	// 나중에 수신한 값이 offset상 먼저 와도 최초 수신 값이 남는다
	late, _ := envelope.Metrics(gauge(9), envMeta(tenantA, now.Add(time.Second)))
	early, _ := envelope.Metrics(gauge(1), envMeta(tenantA, now))
	bt = builder.Build(toMsgs(0, 0, late.Records[0], early.Records[0]))[0]
	if len(bt.Metrics) != 1 || bt.Metrics[0].Value != 1 || len(bt.Quarantine) != 1 || bt.Quarantine[0].Offset != 0 {
		t.Fatalf("kept=%v quarantine=%+v", bt.Metrics, bt.Quarantine)
	}
}

// sink가 행 하나를 결정적으로 거부하면 그 행만 quarantine으로 돌리고 나머지는 저장한다 (ADR 0021 §7).
type rejectingSink struct {
	writes [][]string
}

func (s *rejectingSink) Write(_ context.Context, b *Batch) error {
	for i, r := range b.Spans {
		if r.Name == "poison" {
			return &RowError{Table: "spans_local", Index: i}
		}
	}
	var names []string
	for _, r := range b.Spans {
		names = append(names, r.Name)
	}
	s.writes = append(s.writes, names)
	return nil
}

func TestPoisonRowQuarantinedPartitionContinues(t *testing.T) {
	var recs []envelope.Record
	for i, name := range []string{"ok-1", "poison", "ok-2"} {
		recs = append(recs, spanRecords(t, span(pcommon.TraceID{3}, pcommon.SpanID{byte(i + 1)}, name), envMeta(tenantA, now))...)
	}
	b := builder.Build(toMsgs(0, 50, recs...))[0]
	sink := &rejectingSink{}
	if err := quietWorker(sink, time.Second).write(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if len(sink.writes) != 1 || strings.Join(sink.writes[0], ",") != "ok-1,ok-2" {
		t.Fatalf("writes = %v", sink.writes)
	}
	if b.Rejected != 1 || len(b.Quarantine) != 1 || b.Quarantine[0].Reason != ReasonSinkRejected || b.Quarantine[0].Offset != 51 {
		t.Errorf("quarantine = %+v", b.Quarantine)
	}
	// quarantine 행 자체가 거부되면 돌릴 곳이 없다
	if b.Reject("ingest_quarantine", 0, now) {
		t.Error("quarantine row must not be rejectable")
	}
}

func TestServiceID(t *testing.T) {
	attrs := func(kv ...string) pcommon.Map {
		m := pcommon.NewMap()
		for i := 0; i+1 < len(kv); i += 2 {
			m.PutStr(kv[i], kv[i+1])
		}
		return m
	}
	base := ServiceID(tenantA, attrs("service.name", "checkout", "deployment.environment.name", "prod"))
	if base != ServiceID(tenantA, attrs("deployment.environment.name", "prod", "service.name", "checkout", "service.version", "2")) {
		t.Error("version·속성 순서는 서비스 정체성이 아니다 (D02 §08)")
	}
	for _, other := range []string{
		ServiceID(tenantB, attrs("service.name", "checkout", "deployment.environment.name", "prod")),
		ServiceID(tenantA, attrs("service.name", "checkout", "deployment.environment.name", "stage")),
		ServiceID(tenantA, attrs("service.name", "checkout", "deployment.environment.name", "prod", "service.namespace", "shop")),
	} {
		if other == base {
			t.Error("tenant·environment·namespace가 다르면 다른 서비스다")
		}
	}
	if len(base) != 36 || base[14] != '8' {
		t.Errorf("uuid v8 형식이 아니다: %s", base)
	}
}

func TestExpiresAtClamped(t *testing.T) {
	if got := expiresAt(time.Unix(0, 0), -time.Hour); !got.Equal(time.Unix(0, 0)) {
		t.Errorf("lower clamp = %v", got)
	}
	if got := expiresAt(time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC), time.Hour); got.Unix() != math.MaxUint32 {
		t.Errorf("upper clamp = %v", got)
	}
}

type flakySink struct {
	failures int
	calls    int
	err      error
}

func (s *flakySink) Write(context.Context, *Batch) error {
	s.calls++
	if s.calls <= s.failures {
		return s.err
	}
	return nil
}

func quietWorker(sink Sink, budget time.Duration) *Worker {
	return &Worker{cfg: Config{Sink: sink, RetryBudget: budget, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
}

func TestWriteRetriesThenSucceeds(t *testing.T) {
	sink := &flakySink{failures: 2, err: errors.New("connection refused")}
	w := quietWorker(sink, 10*time.Second)
	b := &Batch{Spans: []SpanRow{{}}}
	if err := w.write(context.Background(), b); err != nil || sink.calls != 3 {
		t.Fatalf("err=%v calls=%d", err, sink.calls)
	}
}

func TestWriteGivesUpWithinBudget(t *testing.T) {
	sink := &flakySink{failures: 1 << 30, err: errors.New("connection refused")}
	w := quietWorker(sink, 500*time.Millisecond)
	start := time.Now()
	err := w.write(context.Background(), &Batch{Topic: "t", Spans: []SpanRow{{}}})
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v elapsed=%v", err, time.Since(start))
	}
}

func TestEmptyBatchNotWritten(t *testing.T) {
	sink := &flakySink{}
	if err := quietWorker(sink, time.Second).write(context.Background(), &Batch{}); err != nil || sink.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, sink.calls)
	}
}

type recordingObserver struct {
	batches []BatchResult
	errors  []string
}

func (o *recordingObserver) ObserveBatch(b BatchResult) { o.batches = append(o.batches, b) }
func (o *recordingObserver) ObserveSinkError(k string)  { o.errors = append(o.errors, k) }
func (o *recordingObserver) ObserveCommit(bool)         {}

// stage 회계 (D02 §21, D04 §10): 소비한 record = 저장 + 중복 + quarantine. ingress accepted와 같은 단위다.
func TestObserverAccountingInvariant(t *testing.T) {
	a := spanRecords(t, span(pcommon.TraceID{4}, pcommon.SpanID{1}, "a"), envMeta(tenantA, now.Add(-2*time.Minute)))
	b := spanRecords(t, span(pcommon.TraceID{4}, pcommon.SpanID{2}, "b"), envMeta(tenantA, now))
	msgs := toMsgs(0, 0, a[0], a[0], b[0])
	msgs = append(msgs, Message{Topic: envelope.TopicTraces, Partition: 0, Offset: 3, Value: []byte{1}}) // header 없음
	batch := builder.Build(msgs)[0]
	obs := &recordingObserver{}
	w := quietWorker(&flakySink{failures: 1, err: errors.New("connection refused")}, 5*time.Second)
	w.cfg.Observer = obs
	if err := w.write(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	w.observe(batch, time.Millisecond)
	r := obs.batches[0]
	quarantined := 0
	for _, n := range r.Quarantined {
		quarantined += n
	}
	if r.Records != 4 || r.Stored+r.Duplicates+quarantined != r.Records || r.Duplicates != 1 || r.Quarantined[ReasonMissingHeader] != 1 {
		t.Errorf("result = %+v", r)
	}
	if r.Signal != "traces" || r.OldestAge < 2*time.Minute {
		t.Errorf("signal=%s oldest age=%v (가장 이른 수신 기준)", r.Signal, r.OldestAge)
	}
	if len(obs.errors) != 1 || obs.errors[0] != "transient" {
		t.Errorf("sink errors = %v", obs.errors)
	}
}
