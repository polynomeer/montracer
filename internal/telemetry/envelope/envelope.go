// Package envelope는 검증·redaction을 마친 OTLP batch를 record 단위 Kafka envelope로 나눈다 (D02 §05, ADR 0020).
//
//	key     : tenant_id(16B) + 파티션 식별자(16B)  → 같은 trace·stream·source가 같은 partition으로 간다
//	value   : record 하나만 담은 OTLP Export*ServiceRequest protobuf (resource·scope 포함, 자기 완결)
//	headers : tenant_id, signal, schema_version, event_id, event_time, ingress_received_at, policy_version, routing_epoch
//
// tenant_id는 호출자가 인증 principal에서 넘긴 값만 쓴다. payload의 어떤 속성도 tenant로 해석하지 않는다.
package envelope

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/polynomeer/montracer/internal/authz"
)

// SchemaVersion은 envelope 형식 버전이다. 소비자는 unknown major를 quarantine한다 (D06 §07).
const SchemaVersion = 1

// MaxMessageBytes는 Kafka message 상한이다 (D02 §05).
const MaxMessageBytes = 1 << 20

// Topic 이름. 신호·처리 단계로 나누고 고객별 topic은 만들지 않는다 (D02 §05).
const (
	TopicTraces  = "telemetry.traces.raw.v1"
	TopicLogs    = "telemetry.logs.raw.v1"
	TopicMetrics = "telemetry.metrics.raw.v1"
)

// Header key.
const (
	HeaderTenant        = "mt-tenant-id"
	HeaderSignal        = "mt-signal"
	HeaderSchema        = "mt-schema-version"
	HeaderEventID       = "mt-event-id"
	HeaderEventTimeNs   = "mt-event-time-ns"
	HeaderReceivedAtMs  = "mt-ingress-received-at-ms"
	HeaderPolicyVersion = "mt-policy-version"
	HeaderRoutingEpoch  = "mt-routing-epoch"
)

// ReasonEnvelopeTooLarge는 직렬화한 envelope가 1MiB를 넘어 거절한 record의 사유다.
const ReasonEnvelopeTooLarge = "record_too_large"

// Header는 Kafka record header다.
type Header struct {
	Key   string
	Value []byte
}

// Record는 Kafka에 쓸 envelope 하나다.
type Record struct {
	Topic     string
	Key       []byte
	Value     []byte
	Headers   []Header
	EventID   string
	EventTime time.Time
}

// Meta는 요청 단위 메타데이터다. Tenant는 인증 principal에서만 온다.
type Meta struct {
	Tenant        authz.TenantID
	ReceivedAt    time.Time
	PolicyVersion int64
	RoutingEpoch  int64
}

func (m Meta) validate() error {
	if m.Tenant.IsZero() {
		return errors.New("envelope: tenant is required")
	}
	if m.ReceivedAt.IsZero() {
		return errors.New("envelope: received time is required")
	}
	return nil
}

func tenantBytes(t authz.TenantID) []byte {
	b, _ := hex.DecodeString(stripDashes(t.String()))
	return b
}

func stripDashes(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '-' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

func (m Meta) record(topic, signal string, key16 []byte, value []byte, eventID string, eventTime time.Time) Record {
	key := append(tenantBytes(m.Tenant), key16...)
	return Record{
		Topic: topic,
		Key:   key,
		Value: value,
		Headers: []Header{
			{HeaderTenant, []byte(m.Tenant.String())},
			{HeaderSignal, []byte(signal)},
			{HeaderSchema, []byte(strconv.Itoa(SchemaVersion))},
			{HeaderEventID, []byte(eventID)},
			{HeaderEventTimeNs, []byte(strconv.FormatInt(eventTime.UnixNano(), 10))},
			{HeaderReceivedAtMs, []byte(strconv.FormatInt(m.ReceivedAt.UnixMilli(), 10))},
			{HeaderPolicyVersion, []byte(strconv.FormatInt(m.PolicyVersion, 10))},
			{HeaderRoutingEpoch, []byte(strconv.FormatInt(m.RoutingEpoch, 10))},
		},
		EventID:   eventID,
		EventTime: eventTime,
	}
}

// Result는 분할 결과다. TooLarge는 직렬화 후 1MiB를 넘어 버린 record 수다.
type Result struct {
	Records  []Record
	TooLarge int
}

// Traces는 span마다 envelope를 만든다. event_id = trace_id + span_id (D02 §05).
// 같은 span을 재전송하면 같은 event_id·key가 나와 worker가 중복을 제거할 수 있다.
func Traces(td ptrace.Traces, m Meta) (Result, error) {
	if err := m.validate(); err != nil {
		return Result{}, err
	}
	var res Result
	marshaler := &ptrace.ProtoMarshaler{}
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				s := ss.Spans().At(k)
				one := ptrace.NewTraces()
				ors := one.ResourceSpans().AppendEmpty()
				rs.Resource().CopyTo(ors.Resource())
				ors.SetSchemaUrl(rs.SchemaUrl())
				oss := ors.ScopeSpans().AppendEmpty()
				ss.Scope().CopyTo(oss.Scope())
				oss.SetSchemaUrl(ss.SchemaUrl())
				s.CopyTo(oss.Spans().AppendEmpty())
				value, err := marshaler.MarshalTraces(one)
				if err != nil {
					return Result{}, fmt.Errorf("envelope: marshal span: %w", err)
				}
				if len(value) > MaxMessageBytes {
					res.TooLarge++
					continue
				}
				tid, sid := s.TraceID(), s.SpanID()
				eventID := hex.EncodeToString(tid[:]) + hex.EncodeToString(sid[:])
				res.Records = append(res.Records, m.record(TopicTraces, "traces", tid[:], value, eventID, s.StartTimestamp().AsTime()))
			}
		}
	}
	return res, nil
}

// LogRecordUIDAttr는 SDK가 붙이는 안정 log ID 속성이다 (OTel semconv log.record.uid).
const LogRecordUIDAttr = "log.record.uid"

// Logs는 log record마다 envelope를 만든다.
// event_id는 log.record.uid가 있으면 그 값, 없으면 수신마다 새로 만든다 — 이 경우 client 재전송 중복은
// 제거할 수 없다 (D02 §09). partition은 resource fingerprint(같은 source의 순서 보존)로 정한다.
func Logs(ld plog.Logs, m Meta, random io.Reader) (Result, error) {
	if err := m.validate(); err != nil {
		return Result{}, err
	}
	if random == nil {
		random = rand.Reader
	}
	var res Result
	marshaler := &plog.ProtoMarshaler{}
	for i := 0; i < ld.ResourceLogs().Len(); i++ {
		rl := ld.ResourceLogs().At(i)
		source := fingerprint(func(h *hasher) {
			h.str(m.Tenant.String())
			h.attrs(rl.Resource().Attributes())
		})
		for j := 0; j < rl.ScopeLogs().Len(); j++ {
			sl := rl.ScopeLogs().At(j)
			for k := 0; k < sl.LogRecords().Len(); k++ {
				lr := sl.LogRecords().At(k)
				one := plog.NewLogs()
				orl := one.ResourceLogs().AppendEmpty()
				rl.Resource().CopyTo(orl.Resource())
				orl.SetSchemaUrl(rl.SchemaUrl())
				osl := orl.ScopeLogs().AppendEmpty()
				sl.Scope().CopyTo(osl.Scope())
				osl.SetSchemaUrl(sl.SchemaUrl())
				lr.CopyTo(osl.LogRecords().AppendEmpty())
				value, err := marshaler.MarshalLogs(one)
				if err != nil {
					return Result{}, fmt.Errorf("envelope: marshal log: %w", err)
				}
				if len(value) > MaxMessageBytes {
					res.TooLarge++
					continue
				}
				eventID := ""
				if v, ok := lr.Attributes().Get(LogRecordUIDAttr); ok && v.Type() == pcommon.ValueTypeStr && v.Str() != "" && len(v.Str()) <= 128 {
					eventID = "uid:" + v.Str()
				} else {
					var b [16]byte
					if _, err := io.ReadFull(random, b[:]); err != nil {
						return Result{}, fmt.Errorf("envelope: event id: %w", err)
					}
					eventID = "gen:" + hex.EncodeToString(b[:])
				}
				ts := lr.Timestamp()
				if ts == 0 {
					ts = lr.ObservedTimestamp()
				}
				res.Records = append(res.Records, m.record(TopicLogs, "logs", source[:], value, eventID, ts.AsTime()))
			}
		}
	}
	return res, nil
}

// Metrics는 data point마다 envelope를 만든다.
// stream_id = fingerprint(resource, scope, name, type, unit, temporality, monotonic, point attributes) (D02 §07).
// event_id = stream_id + start + end + point hash (D02 §05).
func Metrics(md pmetric.Metrics, m Meta) (Result, error) {
	if err := m.validate(); err != nil {
		return Result{}, err
	}
	var res Result
	marshaler := &pmetric.ProtoMarshaler{}
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		rm := md.ResourceMetrics().At(i)
		for j := 0; j < rm.ScopeMetrics().Len(); j++ {
			sm := rm.ScopeMetrics().At(j)
			for k := 0; k < sm.Metrics().Len(); k++ {
				met := sm.Metrics().At(k)
				for _, p := range points(met) {
					stream := streamID(m.Tenant, rm, sm, met, p)
					one := pmetric.NewMetrics()
					orm := one.ResourceMetrics().AppendEmpty()
					rm.Resource().CopyTo(orm.Resource())
					orm.SetSchemaUrl(rm.SchemaUrl())
					osm := orm.ScopeMetrics().AppendEmpty()
					sm.Scope().CopyTo(osm.Scope())
					osm.SetSchemaUrl(sm.SchemaUrl())
					p.copyTo(met, osm.Metrics().AppendEmpty())
					value, err := marshaler.MarshalMetrics(one)
					if err != nil {
						return Result{}, fmt.Errorf("envelope: marshal metric: %w", err)
					}
					if len(value) > MaxMessageBytes {
						res.TooLarge++
						continue
					}
					// point hash는 직렬화 바이트가 아니라 값 필드의 canonical 해시다(속성 순서와 무관).
					pointHash := fingerprint(func(h *hasher) {
						h.buf = append(h.buf, stream[:]...)
						h.buf = binary.BigEndian.AppendUint64(h.buf, p.start)
						h.buf = binary.BigEndian.AppendUint64(h.buf, p.end)
						p.hash(h)
					})
					eventID := fmt.Sprintf("%s-%d-%d-%s", hex.EncodeToString(stream[:]), p.start, p.end, hex.EncodeToString(pointHash[:]))
					res.Records = append(res.Records, m.record(TopicMetrics, "metrics", stream[:], value, eventID, nsTime(p.end)))
				}
			}
		}
	}
	return res, nil
}

// streamID는 D02 §07 stream identity다 (tenant 포함). Metrics와 MetricStreams가 같은 함수를 쓴다.
func streamID(tenant authz.TenantID, rm pmetric.ResourceMetrics, sm pmetric.ScopeMetrics, met pmetric.Metric, p point) [16]byte {
	return fingerprint(func(h *hasher) {
		h.str(tenant.String()) // D02 §07: tenant 포함
		h.attrs(rm.Resource().Attributes())
		h.str(rm.SchemaUrl())
		h.str(sm.Scope().Name())
		h.str(sm.Scope().Version())
		h.attrs(sm.Scope().Attributes())
		h.str(sm.SchemaUrl())
		h.str(met.Name())
		h.str(met.Type().String())
		h.str(met.Unit())
		h.str(p.temporality)
		h.str(strconv.FormatBool(p.monotonic))
		h.attrs(p.attrs)
	})
}

// StreamRef는 metric data point 하나의 stream identity와 dimension이다. cardinality quota가 쓴다 (D02 §10, ADR 0029).
type StreamRef struct {
	StreamID   [16]byte
	Metric     string
	Attributes pcommon.Map // point 속성 (dimension)
	Resource   pcommon.Map
	Scope      pcommon.Map // scope 속성 (identity에 포함)
}

// MetricStreams는 data point마다 StreamRef를 Metrics와 같은 순서로 돌려준다.
// envelope 전에(D02 §04 순서: quota → envelope) 같은 identity로 cardinality를 판정하기 위해서다.
func MetricStreams(md pmetric.Metrics, tenant authz.TenantID) []StreamRef {
	var out []StreamRef
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		rm := md.ResourceMetrics().At(i)
		for j := 0; j < rm.ScopeMetrics().Len(); j++ {
			sm := rm.ScopeMetrics().At(j)
			for k := 0; k < sm.Metrics().Len(); k++ {
				met := sm.Metrics().At(k)
				for _, p := range points(met) {
					out = append(out, StreamRef{StreamID: streamID(tenant, rm, sm, met, p), Metric: met.Name(),
						Attributes: p.attrs, Resource: rm.Resource().Attributes(), Scope: sm.Scope().Attributes()})
				}
			}
		}
	}
	return out
}

// RemovePoints는 MetricStreams와 같은 순서의 i번째 data point를 drop(i)가 true면 지운다. 지운 수를 돌려준다.
// 속성을 지워 다른 series와 합치지 않는다 — point 전체를 거절한다 (D02 §10 "숨은 자동 attribute 삭제 금지").
func RemovePoints(md pmetric.Metrics, drop func(i int) bool) int {
	idx, removed := 0, 0
	next := func() bool {
		d := drop(idx)
		idx++
		if d {
			removed++
		}
		return d
	}
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		rm := md.ResourceMetrics().At(i)
		for j := 0; j < rm.ScopeMetrics().Len(); j++ {
			sm := rm.ScopeMetrics().At(j)
			for k := 0; k < sm.Metrics().Len(); k++ {
				met := sm.Metrics().At(k)
				switch met.Type() {
				case pmetric.MetricTypeGauge:
					met.Gauge().DataPoints().RemoveIf(func(pmetric.NumberDataPoint) bool { return next() })
				case pmetric.MetricTypeSum:
					met.Sum().DataPoints().RemoveIf(func(pmetric.NumberDataPoint) bool { return next() })
				case pmetric.MetricTypeHistogram:
					met.Histogram().DataPoints().RemoveIf(func(pmetric.HistogramDataPoint) bool { return next() })
				case pmetric.MetricTypeExponentialHistogram:
					met.ExponentialHistogram().DataPoints().RemoveIf(func(pmetric.ExponentialHistogramDataPoint) bool { return next() })
				case pmetric.MetricTypeSummary:
					met.Summary().DataPoints().RemoveIf(func(pmetric.SummaryDataPoint) bool { return next() })
				}
			}
		}
	}
	return removed
}

// point는 metric data point 하나를 다루기 위한 공통 뷰다.
type point struct {
	attrs       pcommon.Map
	start, end  uint64
	temporality string
	monotonic   bool
	copyTo      func(src pmetric.Metric, dst pmetric.Metric)
	hash        func(h *hasher) // 값 필드 (exemplar·flags 제외)
}

// nsTime은 OTLP uint64 nanosecond를 시각으로 바꾼다. int64 범위를 넘는 값은 상한으로 자른다(검증 단계에서 이미 거절됨).
func nsTime(ns uint64) time.Time {
	if ns > math.MaxInt64 {
		ns = math.MaxInt64
	}
	return time.Unix(0, int64(ns)).UTC() //nolint:gosec // 위에서 MaxInt64로 제한
}

func f64(h *hasher, v float64) { h.buf = binary.BigEndian.AppendUint64(h.buf, math.Float64bits(v)) }

func numberHash(dp pmetric.NumberDataPoint) func(*hasher) {
	return func(h *hasher) {
		h.buf = append(h.buf, byte(dp.ValueType())) //nolint:gosec // 0~2의 enum
		if dp.ValueType() == pmetric.NumberDataPointValueTypeInt {
			h.buf = binary.AppendVarint(h.buf, dp.IntValue())
		} else {
			f64(h, dp.DoubleValue())
		}
	}
}

func copyHeader(src, dst pmetric.Metric) {
	dst.SetName(src.Name())
	dst.SetDescription(src.Description())
	dst.SetUnit(src.Unit())
	src.Metadata().CopyTo(dst.Metadata())
}

func points(met pmetric.Metric) []point {
	var out []point
	switch met.Type() {
	case pmetric.MetricTypeGauge:
		dps := met.Gauge().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			dp := dps.At(i)
			out = append(out, point{attrs: dp.Attributes(), start: uint64(dp.StartTimestamp()), end: uint64(dp.Timestamp()), hash: numberHash(dp),
				copyTo: func(src, dst pmetric.Metric) {
					copyHeader(src, dst)
					dp.CopyTo(dst.SetEmptyGauge().DataPoints().AppendEmpty())
				}})
		}
	case pmetric.MetricTypeSum:
		sum := met.Sum()
		dps := sum.DataPoints()
		for i := 0; i < dps.Len(); i++ {
			dp := dps.At(i)
			out = append(out, point{attrs: dp.Attributes(), start: uint64(dp.StartTimestamp()), end: uint64(dp.Timestamp()),
				temporality: sum.AggregationTemporality().String(), monotonic: sum.IsMonotonic(), hash: numberHash(dp),
				copyTo: func(src, dst pmetric.Metric) {
					copyHeader(src, dst)
					s := dst.SetEmptySum()
					s.SetAggregationTemporality(sum.AggregationTemporality())
					s.SetIsMonotonic(sum.IsMonotonic())
					dp.CopyTo(s.DataPoints().AppendEmpty())
				}})
		}
	case pmetric.MetricTypeHistogram:
		h := met.Histogram()
		dps := h.DataPoints()
		for i := 0; i < dps.Len(); i++ {
			dp := dps.At(i)
			out = append(out, point{attrs: dp.Attributes(), start: uint64(dp.StartTimestamp()), end: uint64(dp.Timestamp()),
				temporality: h.AggregationTemporality().String(),
				hash: func(hs *hasher) {
					hs.buf = binary.BigEndian.AppendUint64(hs.buf, dp.Count())
					f64(hs, dp.Sum())
					for j := 0; j < dp.BucketCounts().Len(); j++ {
						hs.buf = binary.BigEndian.AppendUint64(hs.buf, dp.BucketCounts().At(j))
					}
					for j := 0; j < dp.ExplicitBounds().Len(); j++ {
						f64(hs, dp.ExplicitBounds().At(j))
					}
				},
				copyTo: func(src, dst pmetric.Metric) {
					copyHeader(src, dst)
					d := dst.SetEmptyHistogram()
					d.SetAggregationTemporality(h.AggregationTemporality())
					dp.CopyTo(d.DataPoints().AppendEmpty())
				}})
		}
	case pmetric.MetricTypeExponentialHistogram:
		h := met.ExponentialHistogram()
		dps := h.DataPoints()
		for i := 0; i < dps.Len(); i++ {
			dp := dps.At(i)
			out = append(out, point{attrs: dp.Attributes(), start: uint64(dp.StartTimestamp()), end: uint64(dp.Timestamp()),
				temporality: h.AggregationTemporality().String(),
				hash: func(hs *hasher) {
					hs.buf = binary.BigEndian.AppendUint64(hs.buf, dp.Count())
					f64(hs, dp.Sum())
					hs.buf = binary.AppendVarint(hs.buf, int64(dp.Scale()))
					hs.buf = binary.BigEndian.AppendUint64(hs.buf, dp.ZeroCount())
					for _, b := range []pmetric.ExponentialHistogramDataPointBuckets{dp.Positive(), dp.Negative()} {
						hs.buf = binary.AppendVarint(hs.buf, int64(b.Offset()))
						for j := 0; j < b.BucketCounts().Len(); j++ {
							hs.buf = binary.BigEndian.AppendUint64(hs.buf, b.BucketCounts().At(j))
						}
					}
				},
				copyTo: func(src, dst pmetric.Metric) {
					copyHeader(src, dst)
					d := dst.SetEmptyExponentialHistogram()
					d.SetAggregationTemporality(h.AggregationTemporality())
					dp.CopyTo(d.DataPoints().AppendEmpty())
				}})
		}
	case pmetric.MetricTypeSummary:
		dps := met.Summary().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			dp := dps.At(i)
			out = append(out, point{attrs: dp.Attributes(), start: uint64(dp.StartTimestamp()), end: uint64(dp.Timestamp()),
				hash: func(hs *hasher) {
					hs.buf = binary.BigEndian.AppendUint64(hs.buf, dp.Count())
					f64(hs, dp.Sum())
					for j := 0; j < dp.QuantileValues().Len(); j++ {
						f64(hs, dp.QuantileValues().At(j).Quantile())
						f64(hs, dp.QuantileValues().At(j).Value())
					}
				},
				copyTo: func(src, dst pmetric.Metric) {
					copyHeader(src, dst)
					dp.CopyTo(dst.SetEmptySummary().DataPoints().AppendEmpty())
				}})
		}
	}
	return out
}

// hasher는 속성 집합의 canonical(정렬·타입 포함) 직렬화를 해시한다.
type hasher struct{ buf []byte }

func (h *hasher) str(s string) {
	h.buf = binary.AppendUvarint(h.buf, uint64(len(s)))
	h.buf = append(h.buf, s...)
}

func (h *hasher) value(v pcommon.Value) {
	h.buf = append(h.buf, byte(v.Type())) //nolint:gosec // 0~6의 enum
	switch v.Type() {
	case pcommon.ValueTypeStr:
		h.str(v.Str())
	case pcommon.ValueTypeInt:
		h.buf = binary.AppendVarint(h.buf, v.Int())
	case pcommon.ValueTypeDouble:
		h.buf = binary.BigEndian.AppendUint64(h.buf, math.Float64bits(v.Double()))
	case pcommon.ValueTypeBool:
		if v.Bool() {
			h.buf = append(h.buf, 1)
		} else {
			h.buf = append(h.buf, 0)
		}
	case pcommon.ValueTypeBytes:
		h.str(string(v.Bytes().AsRaw()))
	case pcommon.ValueTypeSlice:
		s := v.Slice()
		h.buf = binary.AppendUvarint(h.buf, uint64(s.Len())) //nolint:gosec // 길이는 0 이상
		for i := 0; i < s.Len(); i++ {
			h.value(s.At(i))
		}
	case pcommon.ValueTypeMap:
		h.attrs(v.Map())
	}
}

func (h *hasher) attrs(m pcommon.Map) {
	keys := make([]string, 0, m.Len())
	m.Range(func(k string, _ pcommon.Value) bool { keys = append(keys, k); return true })
	sort.Strings(keys)
	h.buf = binary.AppendUvarint(h.buf, uint64(len(keys)))
	for _, k := range keys {
		v, _ := m.Get(k)
		h.str(k)
		h.value(v)
	}
}

// fingerprint는 128-bit 식별자다 (D02 §07: canonical sorted attributes의 128-bit fingerprint).
func fingerprint(fill func(*hasher)) [16]byte {
	var h hasher
	fill(&h)
	sum := sha256.Sum256(h.buf)
	var out [16]byte
	copy(out[:], sum[:16])
	return out
}
