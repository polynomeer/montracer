package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetry/envelope"
)

// Retention은 신호별 기본 보존 기간이다 (D04 §04). expires_at = event_time + 보존 기간.
// tenant별 entitlement 보존(D04 §10)은 entitlement 구현 시 이 값을 대체한다.
type Retention struct {
	Traces, Logs, Metrics time.Duration
}

// DefaultRetention은 D04 §04 기본값이다: trace·log hot 7일, metric 원본 15일.
var DefaultRetention = Retention{Traces: 7 * 24 * time.Hour, Logs: 7 * 24 * time.Hour, Metrics: 15 * 24 * time.Hour}

// version은 "최초 승인 값 유지"(D02 §05, §21)를 위한 ReplacingMergeTree version이다.
// 먼저 수신(ingress 기준)한 record일수록 큰 값이라, 같은 key에서는 최초 수신 행이 남고 query도 그 행을 고른다.
func version(receivedAt time.Time) uint64 {
	ms := receivedAt.UnixMilli()
	if ms < 0 {
		ms = 0
	}
	return math.MaxUint64 - uint64(ms) //nolint:gosec // 위에서 음수 제외
}

// expiresAt은 ClickHouse DateTime 범위(1970~2106)로 자른 만료 시각이다.
func expiresAt(eventTime time.Time, keep time.Duration) time.Time {
	t := eventTime.Add(keep).UTC().Truncate(time.Second)
	if t.Before(time.Unix(0, 0)) {
		return time.Unix(0, 0).UTC()
	}
	if maxDT := time.Unix(math.MaxUint32, 0).UTC(); t.After(maxDT) {
		return maxDT
	}
	return t
}

// ServiceID는 서비스 자연 키 (tenant, environment, service.namespace, service.name)의 결정적 UUID다 (D02 §08).
// service catalog가 생기기 전까지 이 값을 내부 service_id로 쓴다. 이름 변경 alias는 catalog가 맡는다.
// SHA-256 앞 128비트에 UUID version 8·variant 비트를 넣는다(RFC 9562 §5.8).
func ServiceID(tenant authz.TenantID, resource pcommon.Map) string {
	str := func(k, def string) string {
		if v, ok := resource.Get(k); ok && v.Type() == pcommon.ValueTypeStr && v.Str() != "" {
			return v.Str()
		}
		return def
	}
	h := sha256.New()
	for _, s := range []string{
		"montracer.service.v1",
		tenant.String(),
		str("deployment.environment.name", ""),
		str("service.namespace", ""),
		str("service.name", "unknown_service"), // OTel SDK 기본값과 같다
	} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(s), s)
	}
	var b [16]byte
	copy(b[:], h.Sum(nil))
	b[6] = (b[6] & 0x0f) | 0x80
	b[8] = (b[8] & 0x3f) | 0x80
	x := hex.EncodeToString(b[:])
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}

// stringMap은 검색용 Map(String,String) 복제 필드다. 타입 있는 원본은 payload에 남는다 (D02 §09).
func stringMap(m pcommon.Map) map[string]string {
	out := make(map[string]string, m.Len())
	m.Range(func(k string, v pcommon.Value) bool {
		out[k] = v.AsString()
		return true
	})
	return out
}

// canonicalJSON은 key가 정렬된 JSON이다 (encoding/json은 map key를 정렬한다).
func canonicalJSON(m pcommon.Map) (string, error) {
	b, err := json.Marshal(m.AsRaw())
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// SpanRow는 spans_local 한 행이다.
type SpanRow struct {
	Tenant       authz.TenantID
	ServiceID    string
	TraceID      [16]byte
	SpanID       [8]byte
	ParentSpanID [8]byte
	Name         string
	EventTime    time.Time
	DurationNs   uint64
	ReceivedAt   time.Time
	Status       uint8
	Kind         uint8
	Attributes   map[string]string
	Payload      []byte // 단일 span OTLP protobuf (envelope value 그대로)
	PayloadHash  [32]byte
	Version      uint64
	ExpiresAt    time.Time
}

// LogRow는 logs_local 한 행이다.
type LogRow struct {
	Tenant     authz.TenantID
	ServiceID  string
	EventID    string
	EventTime  time.Time
	Severity   uint8
	TraceID    [16]byte
	SpanID     [8]byte
	Body       string
	Attributes map[string]string
	Version    uint64
	ExpiresAt  time.Time
}

// MetricRow는 metric_points 한 행이다.
// 해당 유형에 없는 값(gauge의 count, histogram의 value, 보내지 않은 sum)은 0이 아니라 NaN이다 —
// 없는 값을 정상값 0으로 바꾸지 않는다(CLAUDE.md 계약 6).
type MetricRow struct {
	Tenant         authz.TenantID
	StreamID       [16]byte
	Name           string
	Unit           string
	Type           string
	Temporality    string
	IsMonotonic    bool
	StartTime      time.Time
	EndTime        time.Time
	PointHash      [16]byte
	Value          float64
	Count          uint64
	Sum            float64
	Bounds         []float64
	Buckets        []uint64
	Payload        string
	ResourceJSON   string
	AttributesJSON string
	Version        uint64
	ExpiresAt      time.Time
}

func nsTime(ts pcommon.Timestamp) time.Time { return ts.AsTime().UTC() }

func singleTraces(value []byte) (ptrace.ResourceSpans, ptrace.ScopeSpans, ptrace.Span, error) {
	td, err := (&ptrace.ProtoUnmarshaler{}).UnmarshalTraces(value)
	if err != nil {
		return ptrace.ResourceSpans{}, ptrace.ScopeSpans{}, ptrace.Span{}, reject(ReasonDecode)
	}
	if td.ResourceSpans().Len() != 1 || td.ResourceSpans().At(0).ScopeSpans().Len() != 1 ||
		td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().Len() != 1 {
		return ptrace.ResourceSpans{}, ptrace.ScopeSpans{}, ptrace.Span{}, reject(ReasonShape)
	}
	rs := td.ResourceSpans().At(0)
	ss := rs.ScopeSpans().At(0)
	return rs, ss, ss.Spans().At(0), nil
}

func normalizeSpan(md meta, value []byte, ret Retention) (SpanRow, error) {
	rs, _, s, err := singleTraces(value)
	if err != nil {
		return SpanRow{}, err
	}
	tid, sid := s.TraceID(), s.SpanID()
	if md.eventID != hex.EncodeToString(tid[:])+hex.EncodeToString(sid[:]) {
		return SpanRow{}, reject(ReasonEventIDMismatch)
	}
	start, end := s.StartTimestamp(), s.EndTimestamp()
	if end < start {
		return SpanRow{}, reject(ReasonDecode) // ingress 검증(ADR 0017)이 거절했어야 하는 값
	}
	eventTime := nsTime(start)
	return SpanRow{
		Tenant:       md.tenant,
		ServiceID:    ServiceID(md.tenant, rs.Resource().Attributes()),
		TraceID:      tid,
		SpanID:       sid,
		ParentSpanID: s.ParentSpanID(),
		Name:         s.Name(),
		EventTime:    eventTime,
		DurationNs:   uint64(end - start),
		ReceivedAt:   md.receivedAt,
		Status:       uint8(s.Status().Code()), //nolint:gosec // 0~2의 enum
		Kind:         uint8(s.Kind()),          //nolint:gosec // 0~5의 enum
		Attributes:   stringMap(s.Attributes()),
		Payload:      value,
		PayloadHash:  sha256.Sum256(value),
		Version:      version(md.receivedAt),
		ExpiresAt:    expiresAt(eventTime, ret.Traces),
	}, nil
}

func normalizeLog(md meta, value []byte, ret Retention) (LogRow, error) {
	ld, err := (&plog.ProtoUnmarshaler{}).UnmarshalLogs(value)
	if err != nil {
		return LogRow{}, reject(ReasonDecode)
	}
	if ld.ResourceLogs().Len() != 1 || ld.ResourceLogs().At(0).ScopeLogs().Len() != 1 ||
		ld.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().Len() != 1 {
		return LogRow{}, reject(ReasonShape)
	}
	rl := ld.ResourceLogs().At(0)
	lr := rl.ScopeLogs().At(0).LogRecords().At(0)
	// event_id 형식: uid:<log.record.uid> 이면 payload의 uid와 같아야 하고, gen:<hex 32>는 수신 시 생성한 값이다.
	switch {
	case strings.HasPrefix(md.eventID, "uid:"):
		v, ok := lr.Attributes().Get(envelope.LogRecordUIDAttr)
		if !ok || v.Type() != pcommon.ValueTypeStr || "uid:"+v.Str() != md.eventID {
			return LogRow{}, reject(ReasonEventIDMismatch)
		}
	case strings.HasPrefix(md.eventID, "gen:"):
		if b, err := hex.DecodeString(md.eventID[4:]); err != nil || len(b) != 16 {
			return LogRow{}, reject(ReasonEventIDMismatch)
		}
	default:
		return LogRow{}, reject(ReasonEventIDMismatch)
	}
	ts := lr.Timestamp()
	if ts == 0 {
		ts = lr.ObservedTimestamp()
	}
	eventTime := nsTime(ts)
	return LogRow{
		Tenant:     md.tenant,
		ServiceID:  ServiceID(md.tenant, rl.Resource().Attributes()),
		EventID:    md.eventID,
		EventTime:  eventTime,
		Severity:   uint8(lr.SeverityNumber()), //nolint:gosec // 0~24 (ingress 검증)
		TraceID:    lr.TraceID(),
		SpanID:     lr.SpanID(),
		Body:       lr.Body().AsString(),
		Attributes: stringMap(lr.Attributes()),
		Version:    version(md.receivedAt),
		ExpiresAt:  expiresAt(eventTime, ret.Logs),
	}, nil
}

// parseMetricEventID는 "<stream hex32>-<start_ns>-<end_ns>-<point hash hex32>"를 해석한다 (ADR 0020 §4).
func parseMetricEventID(id string) (stream, point [16]byte, start, end uint64, ok bool) {
	parts := strings.Split(id, "-")
	if len(parts) != 4 {
		return stream, point, 0, 0, false
	}
	sb, err1 := hex.DecodeString(parts[0])
	pb, err2 := hex.DecodeString(parts[3])
	s, err3 := strconv.ParseUint(parts[1], 10, 64)
	e, err4 := strconv.ParseUint(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || len(sb) != 16 || len(pb) != 16 {
		return stream, point, 0, 0, false
	}
	copy(stream[:], sb)
	copy(point[:], pb)
	return stream, point, s, e, true
}

func numberValue(dp pmetric.NumberDataPoint) float64 {
	if dp.ValueType() == pmetric.NumberDataPointValueTypeInt {
		return float64(dp.IntValue())
	}
	return dp.DoubleValue()
}

func temporality(t pmetric.AggregationTemporality) string {
	switch t {
	case pmetric.AggregationTemporalityDelta:
		return "delta"
	case pmetric.AggregationTemporalityCumulative:
		return "cumulative"
	default:
		return "unspecified"
	}
}

func normalizeMetric(md meta, value []byte, ret Retention) (MetricRow, error) {
	mdata, err := (&pmetric.ProtoUnmarshaler{}).UnmarshalMetrics(value)
	if err != nil {
		return MetricRow{}, reject(ReasonDecode)
	}
	if mdata.ResourceMetrics().Len() != 1 || mdata.ResourceMetrics().At(0).ScopeMetrics().Len() != 1 ||
		mdata.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().Len() != 1 || mdata.DataPointCount() != 1 {
		return MetricRow{}, reject(ReasonShape)
	}
	rm := mdata.ResourceMetrics().At(0)
	met := rm.ScopeMetrics().At(0).Metrics().At(0)
	stream, pointHash, idStart, idEnd, ok := parseMetricEventID(md.eventID)
	if !ok {
		return MetricRow{}, reject(ReasonEventIDMismatch)
	}
	resJSON, err := canonicalJSON(rm.Resource().Attributes())
	if err != nil {
		return MetricRow{}, reject(ReasonDecode)
	}
	row := MetricRow{
		Tenant:       md.tenant,
		StreamID:     stream,
		Name:         met.Name(),
		Unit:         met.Unit(),
		PointHash:    pointHash,
		Value:        math.NaN(),
		Sum:          math.NaN(),
		Temporality:  "unspecified",
		ResourceJSON: resJSON,
		Version:      version(md.receivedAt),
	}
	var (
		attrs      pcommon.Map
		start, end pcommon.Timestamp
		keepRaw    bool
	)
	switch met.Type() {
	case pmetric.MetricTypeGauge:
		dp := met.Gauge().DataPoints().At(0)
		row.Type, row.Value = "gauge", numberValue(dp)
		attrs, start, end = dp.Attributes(), dp.StartTimestamp(), dp.Timestamp()
	case pmetric.MetricTypeSum:
		sum := met.Sum()
		dp := sum.DataPoints().At(0)
		row.Type, row.Value = "sum", numberValue(dp)
		row.Temporality, row.IsMonotonic = temporality(sum.AggregationTemporality()), sum.IsMonotonic()
		attrs, start, end = dp.Attributes(), dp.StartTimestamp(), dp.Timestamp()
	case pmetric.MetricTypeHistogram:
		h := met.Histogram()
		dp := h.DataPoints().At(0)
		row.Type, row.Temporality = "histogram", temporality(h.AggregationTemporality())
		row.Count = dp.Count()
		if dp.HasSum() {
			row.Sum = dp.Sum()
		}
		row.Bounds = dp.ExplicitBounds().AsRaw()
		row.Buckets = dp.BucketCounts().AsRaw()
		keepRaw = dp.HasMin() || dp.HasMax() // min·max 컬럼이 없어 원본으로 보존
		attrs, start, end = dp.Attributes(), dp.StartTimestamp(), dp.Timestamp()
	case pmetric.MetricTypeExponentialHistogram:
		h := met.ExponentialHistogram()
		dp := h.DataPoints().At(0)
		row.Type, row.Temporality = "exponential_histogram", temporality(h.AggregationTemporality())
		row.Count = dp.Count()
		if dp.HasSum() {
			row.Sum = dp.Sum()
		}
		keepRaw = true // scale·offset·양/음 bucket (D02 §10)
		attrs, start, end = dp.Attributes(), dp.StartTimestamp(), dp.Timestamp()
	case pmetric.MetricTypeSummary:
		dp := met.Summary().DataPoints().At(0)
		row.Type = "summary"
		row.Count, row.Sum = dp.Count(), dp.Sum()
		keepRaw = true // quantile
		attrs, start, end = dp.Attributes(), dp.StartTimestamp(), dp.Timestamp()
	default:
		return MetricRow{}, reject(ReasonShape)
	}
	if uint64(start) != idStart || uint64(end) != idEnd {
		return MetricRow{}, reject(ReasonEventIDMismatch)
	}
	if row.AttributesJSON, err = canonicalJSON(attrs); err != nil {
		return MetricRow{}, reject(ReasonDecode)
	}
	if keepRaw {
		b, err := (&pmetric.JSONMarshaler{}).MarshalMetrics(mdata)
		if err != nil {
			return MetricRow{}, reject(ReasonDecode)
		}
		row.Payload = string(b)
	}
	row.StartTime, row.EndTime = nsTime(start), nsTime(end)
	row.ExpiresAt = expiresAt(row.EndTime, ret.Metrics)
	return row, nil
}
