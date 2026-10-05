package envelope

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/polynomeer/montracer/internal/authz"
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

func meta(t authz.TenantID) Meta {
	return Meta{Tenant: t, ReceivedAt: now, PolicyVersion: 1, RoutingEpoch: 1}
}

func header(r Record, k string) string {
	for _, h := range r.Headers {
		if h.Key == k {
			return string(h.Value)
		}
	}
	return ""
}

func twoSpans() ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	// payload의 tenant 속성은 무시돼야 한다 (변경 불가 계약 1)
	rs.Resource().Attributes().PutStr("tenant_id", tenantB.String())
	ss := rs.ScopeSpans().AppendEmpty()
	for i := range 2 {
		s := ss.Spans().AppendEmpty()
		s.SetTraceID(pcommon.TraceID{0xab, 1})
		s.SetSpanID(pcommon.SpanID{byte(i + 1)})
		s.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	}
	return td
}

func TestTracesEnvelope(t *testing.T) {
	res, err := Traces(twoSpans(), meta(tenantA))
	if err != nil || len(res.Records) != 2 {
		t.Fatalf("records = %d, %v", len(res.Records), err)
	}
	r := res.Records[0]
	if r.Topic != TopicTraces || header(r, HeaderTenant) != tenantA.String() || header(r, HeaderSignal) != "traces" {
		t.Fatalf("headers = %+v", r.Headers)
	}
	if r.EventID != "ab010000000000000000000000000000"+"0100000000000000" {
		t.Errorf("event id = %s", r.EventID)
	}
	// 같은 trace의 span은 같은 partition key
	if !bytes.Equal(r.Key, res.Records[1].Key) || len(r.Key) != 32 {
		t.Errorf("keys differ or wrong length")
	}
	// value는 자기 완결: span 하나 + resource
	one, err := (&ptrace.ProtoUnmarshaler{}).UnmarshalTraces(r.Value)
	if err != nil || one.SpanCount() != 1 {
		t.Fatalf("value: %v spans=%d", err, one.SpanCount())
	}
	if v, _ := one.ResourceSpans().At(0).Resource().Attributes().Get("service.name"); v.Str() != "checkout" {
		t.Error("resource not carried")
	}
	for _, k := range []string{HeaderSchema, HeaderEventTimeNs, HeaderReceivedAtMs, HeaderPolicyVersion, HeaderRoutingEpoch, HeaderEventID} {
		if header(r, k) == "" {
			t.Errorf("missing header %s", k)
		}
	}
}

// 재전송(at-least-once)되면 같은 event_id·key가 나와야 worker가 dedup할 수 있다.
func TestRetryProducesSameIdentity(t *testing.T) {
	a, _ := Traces(twoSpans(), meta(tenantA))
	b, _ := Traces(twoSpans(), meta(tenantA))
	for i := range a.Records {
		if a.Records[i].EventID != b.Records[i].EventID || !bytes.Equal(a.Records[i].Key, b.Records[i].Key) {
			t.Fatal("identity not stable across retries")
		}
	}
	// 다른 tenant의 같은 trace는 다른 partition key
	c, _ := Traces(twoSpans(), meta(tenantB))
	if bytes.Equal(a.Records[0].Key, c.Records[0].Key) {
		t.Error("tenant must be part of the key")
	}
}

func TestMetaRequired(t *testing.T) {
	if _, err := Traces(twoSpans(), Meta{ReceivedAt: now}); err == nil {
		t.Error("zero tenant accepted")
	}
	if _, err := Traces(twoSpans(), Meta{Tenant: tenantA}); err == nil {
		t.Error("zero received time accepted")
	}
}

func TestLogsEventID(t *testing.T) {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "checkout")
	lrs := rl.ScopeLogs().AppendEmpty().LogRecords()
	withUID := lrs.AppendEmpty()
	withUID.Attributes().PutStr(LogRecordUIDAttr, "01J9ZK")
	withUID.SetTimestamp(pcommon.NewTimestampFromTime(now))
	lrs.AppendEmpty().SetObservedTimestamp(pcommon.NewTimestampFromTime(now))

	a, err := Logs(ld, meta(tenantA), bytes.NewReader(bytes.Repeat([]byte{7}, 64)))
	if err != nil || len(a.Records) != 2 {
		t.Fatalf("%v %d", err, len(a.Records))
	}
	if a.Records[0].EventID != "uid:01J9ZK" || !strings.HasPrefix(a.Records[1].EventID, "gen:") {
		t.Errorf("event ids = %s, %s", a.Records[0].EventID, a.Records[1].EventID)
	}
	if !a.Records[1].EventTime.Equal(now) {
		t.Errorf("observed time not used: %v", a.Records[1].EventTime)
	}
	// 같은 source(resource)의 log는 같은 partition key (순서 보존)
	if !bytes.Equal(a.Records[0].Key, a.Records[1].Key) {
		t.Error("same resource must share a key")
	}
}

func TestMetricStreamIdentity(t *testing.T) {
	build := func(attrOrder []string, value float64) pmetric.Metrics {
		md := pmetric.NewMetrics()
		rm := md.ResourceMetrics().AppendEmpty()
		rm.Resource().Attributes().PutStr("service.name", "checkout")
		m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
		m.SetName("http.server.request.errors")
		s := m.SetEmptySum()
		s.SetIsMonotonic(true)
		s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		p := s.DataPoints().AppendEmpty()
		for _, k := range attrOrder {
			p.Attributes().PutStr(k, "v-"+k)
		}
		p.SetStartTimestamp(pcommon.NewTimestampFromTime(now.Add(-time.Minute)))
		p.SetTimestamp(pcommon.NewTimestampFromTime(now))
		p.SetDoubleValue(value)
		return md
	}
	a, _ := Metrics(build([]string{"route", "method"}, 1), meta(tenantA))
	b, _ := Metrics(build([]string{"method", "route"}, 1), meta(tenantA)) // 속성 순서만 다름
	c, _ := Metrics(build([]string{"method", "route"}, 2), meta(tenantA)) // 값만 다름
	if !bytes.Equal(a.Records[0].Key, b.Records[0].Key) || a.Records[0].EventID != b.Records[0].EventID {
		t.Error("attribute order must not change stream identity or event id")
	}
	if !bytes.Equal(a.Records[0].Key, c.Records[0].Key) {
		t.Error("same stream must share partition key")
	}
	if a.Records[0].EventID == c.Records[0].EventID {
		t.Error("different point value must change point hash")
	}
	one, err := (&pmetric.ProtoUnmarshaler{}).UnmarshalMetrics(a.Records[0].Value)
	if err != nil || one.DataPointCount() != 1 || !one.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Sum().IsMonotonic() {
		t.Fatal("metric envelope must keep sum semantics")
	}
}

func TestOversizedEnvelopeRejected(t *testing.T) {
	td := twoSpans()
	td.ResourceSpans().At(0).Resource().Attributes().PutStr("huge", strings.Repeat("x", MaxMessageBytes))
	res, err := Traces(td, meta(tenantA))
	if err != nil || res.TooLarge != 2 || len(res.Records) != 0 {
		t.Fatalf("res = %+v, %v", res, err)
	}
}

func TestAllMetricTypesRoundTrip(t *testing.T) {
	md := pmetric.NewMetrics()
	ms := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics()
	ts := pcommon.NewTimestampFromTime(now)
	g := ms.AppendEmpty()
	g.SetName("g")
	g.SetEmptyGauge().DataPoints().AppendEmpty().SetTimestamp(ts)
	h := ms.AppendEmpty()
	h.SetName("h")
	hp := h.SetEmptyHistogram().DataPoints().AppendEmpty()
	hp.SetTimestamp(ts)
	hp.SetCount(2)
	hp.BucketCounts().FromRaw([]uint64{1, 1})
	hp.ExplicitBounds().FromRaw([]float64{1})
	e := ms.AppendEmpty()
	e.SetName("e")
	ep := e.SetEmptyExponentialHistogram().DataPoints().AppendEmpty()
	ep.SetTimestamp(ts)
	ep.Positive().BucketCounts().FromRaw([]uint64{3})
	s := ms.AppendEmpty()
	s.SetName("s")
	sp := s.SetEmptySummary().DataPoints().AppendEmpty()
	sp.SetTimestamp(ts)
	sp.QuantileValues().AppendEmpty().SetValue(0.2)

	res, err := Metrics(md, meta(tenantA))
	if err != nil || len(res.Records) != 4 {
		t.Fatalf("records = %d, %v", len(res.Records), err)
	}
	for i, want := range []pmetric.MetricType{pmetric.MetricTypeGauge, pmetric.MetricTypeHistogram, pmetric.MetricTypeExponentialHistogram, pmetric.MetricTypeSummary} {
		one, err := (&pmetric.ProtoUnmarshaler{}).UnmarshalMetrics(res.Records[i].Value)
		if err != nil || one.DataPointCount() != 1 {
			t.Fatalf("record %d: %v", i, err)
		}
		if got := one.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Type(); got != want {
			t.Errorf("record %d type = %v, want %v", i, got, want)
		}
		if !res.Records[i].EventTime.Equal(now) {
			t.Errorf("record %d event time = %v", i, res.Records[i].EventTime)
		}
	}
}
