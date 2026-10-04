package otlp

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// Reason은 record 단위 영구 거절 사유다. 응답 message와 metric label로 쓰며 값 내용을 담지 않는다.
type Reason string

const (
	ReasonInvalidTraceID    Reason = "invalid_trace_id"
	ReasonInvalidSpanID     Reason = "invalid_span_id"
	ReasonTimeOutOfRange    Reason = "timestamp_out_of_range"
	ReasonEndBeforeStart    Reason = "end_before_start"
	ReasonTooManyAttributes Reason = "too_many_attributes"
	ReasonAttributeTooLong  Reason = "attribute_too_long"
	ReasonRecordTooLarge    Reason = "record_too_large"
	ReasonLogBodyTooLarge   Reason = "log_body_too_large"
	ReasonMissingMetricName Reason = "missing_metric_name"
	ReasonInvalidHistogram  Reason = "invalid_histogram"
	// ReasonResourceInvalid는 resource·scope 속성이 한도를 넘어 그 아래 record 전체를 거절했다는 뜻이다.
	ReasonResourceInvalid Reason = "resource_attributes_invalid"
)

// Rules는 record 단위 한도다.
type Rules struct {
	MaxAttributes     int           // record당 속성 수 (D03 §02: 128)
	MaxAttrValueBytes int           // 문자열·bytes 속성 값 (D03 §02: 4KiB)
	MaxSpanBytes      int           // span 근사 크기 (D03 §02: 64KiB)
	MaxLogBodyBytes   int           // log body (D03 §02: 32KiB)
	MaxFuture         time.Duration // 수신 시각 대비 미래 허용 (D02 §07: 5분)
	MaxPast           time.Duration // 수신 시각 대비 과거 허용 (D02 §07: 24시간, 초과는 backfill 경로)
	// MaxEnvelopeBytes는 resource+scope+record 근사 크기 한도다. record마다 envelope 하나가
	// Kafka message가 되므로 D02 §05의 1MiB를 넘을 수 없다.
	MaxEnvelopeBytes int
}

// DefaultRules는 D02 §07, D03 §02의 시작값이다.
var DefaultRules = Rules{
	MaxAttributes:     128,
	MaxAttrValueBytes: 4 << 10,
	MaxSpanBytes:      64 << 10,
	MaxLogBodyBytes:   32 << 10,
	MaxFuture:         5 * time.Minute,
	MaxPast:           24 * time.Hour,
	MaxEnvelopeBytes:  1 << 20,
}

// elementOverhead는 크기 근사에서 값·원소마다 더하는 고정 비용이다. 빈 원소를 무한히 늘려
// 크기 검사를 통과하는 것을 막는다 (ADR 0017).
const elementOverhead = 8

// Result는 검증 결과다. Accepted+Rejected는 입력 record 수와 같다.
// record는 span, metric data point, log record다 (OTLP partial success 단위).
type Result struct {
	Accepted int
	Rejected int
	Reasons  map[Reason]int
}

func (r *Result) reject(reason Reason) {
	if r.Reasons == nil {
		r.Reasons = map[Reason]int{}
	}
	r.Rejected++
	r.Reasons[reason]++
}

// Message는 partial success error_message용 요약이다. 사유와 건수만 담는다(값 내용 없음).
func (r Result) Message() string {
	if r.Rejected == 0 {
		return ""
	}
	keys := make([]string, 0, len(r.Reasons))
	for k := range r.Reasons {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, r.Reasons[Reason(k)])
	}
	return "rejected " + strings.Join(parts, ",")
}

type checker struct {
	rules    Rules
	received time.Time
}

func (c checker) timeOK(ts pcommon.Timestamp) bool {
	t := ts.AsTime()
	return !t.After(c.received.Add(c.rules.MaxFuture)) && !t.Before(c.received.Add(-c.rules.MaxPast))
}

// attrs는 속성 수와 값 길이를 검사하고, 근사 크기(key+값 byte)를 더한다.
func (c checker) attrs(m pcommon.Map, size *int) (Reason, bool) {
	if m.Len() > c.rules.MaxAttributes {
		return ReasonTooManyAttributes, false
	}
	reason, ok := Reason(""), true
	m.Range(func(k string, v pcommon.Value) bool {
		*size += len(k) + elementOverhead
		if r, valid := c.value(v, size); !valid {
			reason, ok = r, false
			return false
		}
		return true
	})
	return reason, ok
}

func (c checker) value(v pcommon.Value, size *int) (Reason, bool) {
	*size += elementOverhead
	switch v.Type() {
	case pcommon.ValueTypeStr:
		n := len(v.Str())
		*size += n
		if n > c.rules.MaxAttrValueBytes {
			return ReasonAttributeTooLong, false
		}
	case pcommon.ValueTypeBytes:
		n := v.Bytes().Len()
		*size += n
		if n > c.rules.MaxAttrValueBytes {
			return ReasonAttributeTooLong, false
		}
	case pcommon.ValueTypeSlice:
		s := v.Slice()
		for i := 0; i < s.Len(); i++ {
			if r, ok := c.value(s.At(i), size); !ok {
				return r, false
			}
		}
	case pcommon.ValueTypeMap:
		return c.attrs(v.Map(), size)
	}
	return "", true
}

// container는 resource 또는 scope의 속성을 검사하고 근사 크기를 돌려준다.
// 위반이면 그 아래 record 전체를 거절한다(속성이 모든 envelope에 복제되기 때문).
func (c checker) container(attrs pcommon.Map, strs ...string) (int, bool) {
	size := 0
	for _, s := range strs {
		size += len(s)
	}
	_, ok := c.attrs(attrs, &size)
	return size, ok
}

func (c checker) envelopeOK(parent, record int) bool {
	return parent+record <= c.rules.MaxEnvelopeBytes
}

// ValidateTraces는 규칙을 어긴 span을 제거하고 빈 scope·resource를 정리한다.
func ValidateTraces(td ptrace.Traces, received time.Time, rules Rules) Result {
	c := checker{rules: rules, received: received}
	var res Result
	td.ResourceSpans().RemoveIf(func(rs ptrace.ResourceSpans) bool {
		rsize, rok := c.container(rs.Resource().Attributes(), rs.SchemaUrl())
		rs.ScopeSpans().RemoveIf(func(ss ptrace.ScopeSpans) bool {
			sc := ss.Scope()
			ssize, sok := c.container(sc.Attributes(), sc.Name(), sc.Version(), ss.SchemaUrl())
			ss.Spans().RemoveIf(func(s ptrace.Span) bool {
				if !rok || !sok {
					res.reject(ReasonResourceInvalid)
					return true
				}
				size, reason, ok := c.span(s)
				if ok && !c.envelopeOK(rsize+ssize, size) {
					reason, ok = ReasonRecordTooLarge, false
				}
				if !ok {
					res.reject(reason)
					return true
				}
				res.Accepted++
				return false
			})
			return ss.Spans().Len() == 0
		})
		return rs.ScopeSpans().Len() == 0
	})
	return res
}

// span은 규칙을 검사하고 근사 크기를 돌려준다.
func (c checker) span(s ptrace.Span) (int, Reason, bool) {
	if s.TraceID().IsEmpty() {
		return 0, ReasonInvalidTraceID, false
	}
	if s.SpanID().IsEmpty() {
		return 0, ReasonInvalidSpanID, false
	}
	if !c.timeOK(s.StartTimestamp()) || !c.timeOK(s.EndTimestamp()) {
		return 0, ReasonTimeOutOfRange, false
	}
	if s.EndTimestamp() < s.StartTimestamp() {
		return 0, ReasonEndBeforeStart, false
	}
	// 근사 크기: 모든 가변 필드 + 원소마다 고정 비용 (ADR 0017)
	size := 64 + len(s.Name()) + len(s.TraceState().AsRaw()) + len(s.Status().Message())
	if r, ok := c.attrs(s.Attributes(), &size); !ok {
		return 0, r, false
	}
	for i := 0; i < s.Events().Len(); i++ {
		e := s.Events().At(i)
		size += 16 + len(e.Name())
		if r, ok := c.attrs(e.Attributes(), &size); !ok {
			return 0, r, false
		}
	}
	for i := 0; i < s.Links().Len(); i++ {
		l := s.Links().At(i)
		size += 40 + len(l.TraceState().AsRaw())
		if r, ok := c.attrs(l.Attributes(), &size); !ok {
			return 0, r, false
		}
	}
	if size > c.rules.MaxSpanBytes {
		return 0, ReasonRecordTooLarge, false
	}
	return size, "", true
}

// ValidateLogs는 규칙을 어긴 log record를 제거한다.
//
// event 시각은 Timestamp, 없으면 ObservedTimestamp다. 둘 다 없으면 OTel 데이터 모델에 따라
// ObservedTimestamp를 수신 시각으로 채운다(이후 envelope의 event_time은 0이 되지 않는다).
// span_id만 있고 trace_id가 없는 record는 연결만 불가능할 뿐 거절하지 않는다 (ADR 0017).
func ValidateLogs(ld plog.Logs, received time.Time, rules Rules) Result {
	c := checker{rules: rules, received: received}
	var res Result
	ld.ResourceLogs().RemoveIf(func(rl plog.ResourceLogs) bool {
		rsize, rok := c.container(rl.Resource().Attributes(), rl.SchemaUrl())
		rl.ScopeLogs().RemoveIf(func(sl plog.ScopeLogs) bool {
			sc := sl.Scope()
			ssize, sok := c.container(sc.Attributes(), sc.Name(), sc.Version(), sl.SchemaUrl())
			sl.LogRecords().RemoveIf(func(lr plog.LogRecord) bool {
				if !rok || !sok {
					res.reject(ReasonResourceInvalid)
					return true
				}
				size, reason, ok := c.log(lr)
				if ok && !c.envelopeOK(rsize+ssize, size) {
					reason, ok = ReasonRecordTooLarge, false
				}
				if !ok {
					res.reject(reason)
					return true
				}
				res.Accepted++
				return false
			})
			return sl.LogRecords().Len() == 0
		})
		return rl.ScopeLogs().Len() == 0
	})
	return res
}

func (c checker) log(lr plog.LogRecord) (int, Reason, bool) {
	ts := lr.Timestamp()
	if ts == 0 {
		ts = lr.ObservedTimestamp()
	}
	if ts == 0 {
		lr.SetObservedTimestamp(pcommon.NewTimestampFromTime(c.received))
	} else if !c.timeOK(ts) {
		return 0, ReasonTimeOutOfRange, false
	}
	var body int
	switch lr.Body().Type() {
	case pcommon.ValueTypeStr:
		body = len(lr.Body().Str())
	case pcommon.ValueTypeBytes:
		body = lr.Body().Bytes().Len()
	default:
		// 구조화 body(map/slice)는 key·값 byte와 원소별 고정 비용의 합으로 잰다.
		// 개별 값 한도 대신 body 전체 한도(32KiB)를 적용한다.
		lenient := c
		lenient.rules.MaxAttrValueBytes = c.rules.MaxLogBodyBytes
		lenient.rules.MaxAttributes = int(^uint(0) >> 1)
		if _, ok := lenient.value(lr.Body(), &body); !ok {
			return 0, ReasonLogBodyTooLarge, false
		}
	}
	if body > c.rules.MaxLogBodyBytes {
		return 0, ReasonLogBodyTooLarge, false
	}
	size := 64 + body + len(lr.SeverityText()) + len(lr.EventName())
	if r, ok := c.attrs(lr.Attributes(), &size); !ok {
		return 0, r, false
	}
	return size, "", true
}

// ValidateMetrics는 규칙을 어긴 data point를 제거하고 빈 metric·scope·resource를 정리한다.
func ValidateMetrics(md pmetric.Metrics, received time.Time, rules Rules) Result {
	c := checker{rules: rules, received: received}
	var res Result
	md.ResourceMetrics().RemoveIf(func(rm pmetric.ResourceMetrics) bool {
		rsize, rok := c.container(rm.Resource().Attributes(), rm.SchemaUrl())
		rm.ScopeMetrics().RemoveIf(func(sm pmetric.ScopeMetrics) bool {
			sc := sm.Scope()
			ssize, sok := c.container(sc.Attributes(), sc.Name(), sc.Version(), sm.SchemaUrl())
			sm.Metrics().RemoveIf(func(m pmetric.Metric) bool {
				if !rok || !sok {
					for range dataPointCount(m) {
						res.reject(ReasonResourceInvalid)
					}
					return true
				}
				return c.metric(m, rsize+ssize, &res)
			})
			return sm.Metrics().Len() == 0
		})
		return rm.ScopeMetrics().Len() == 0
	})
	return res
}

// metric은 data point를 걸러내고, 남은 point가 없으면 true(metric 제거)를 반환한다.
func (c checker) metric(m pmetric.Metric, parent int, res *Result) bool {
	if m.Name() == "" {
		for range dataPointCount(m) {
			res.reject(ReasonMissingMetricName)
		}
		return true
	}
	// metric 이름·설명·단위·metadata도 각 point envelope에 복제된다.
	base := parent + len(m.Name()) + len(m.Description()) + len(m.Unit())
	if _, ok := c.attrs(m.Metadata(), &base); !ok {
		for range dataPointCount(m) {
			res.reject(ReasonResourceInvalid)
		}
		return true
	}
	point := func(ts pcommon.Timestamp, attrs pcommon.Map, exemplars pmetric.ExemplarSlice, extra int) (Reason, bool) {
		if !c.timeOK(ts) {
			return ReasonTimeOutOfRange, false
		}
		size := 64 + extra
		if r, ok := c.attrs(attrs, &size); !ok {
			return r, false
		}
		for i := 0; i < exemplars.Len(); i++ {
			size += 48
			if r, ok := c.attrs(exemplars.At(i).FilteredAttributes(), &size); !ok {
				return r, false
			}
		}
		if !c.envelopeOK(base, size) {
			return ReasonRecordTooLarge, false
		}
		return "", true
	}
	keep := func(reason Reason, ok bool) bool {
		if !ok {
			res.reject(reason)
			return true
		}
		res.Accepted++
		return false
	}
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		m.Gauge().DataPoints().RemoveIf(func(p pmetric.NumberDataPoint) bool {
			return keep(point(p.Timestamp(), p.Attributes(), p.Exemplars(), 0))
		})
		return m.Gauge().DataPoints().Len() == 0
	case pmetric.MetricTypeSum:
		m.Sum().DataPoints().RemoveIf(func(p pmetric.NumberDataPoint) bool {
			return keep(point(p.Timestamp(), p.Attributes(), p.Exemplars(), 0))
		})
		return m.Sum().DataPoints().Len() == 0
	case pmetric.MetricTypeHistogram:
		m.Histogram().DataPoints().RemoveIf(func(p pmetric.HistogramDataPoint) bool {
			extra := 8 * (p.BucketCounts().Len() + p.ExplicitBounds().Len())
			if r, ok := point(p.Timestamp(), p.Attributes(), p.Exemplars(), extra); !ok {
				return keep(r, false)
			}
			return keep(ReasonInvalidHistogram, histogramOK(p))
		})
		return m.Histogram().DataPoints().Len() == 0
	case pmetric.MetricTypeExponentialHistogram:
		m.ExponentialHistogram().DataPoints().RemoveIf(func(p pmetric.ExponentialHistogramDataPoint) bool {
			extra := 8 * (p.Positive().BucketCounts().Len() + p.Negative().BucketCounts().Len())
			return keep(point(p.Timestamp(), p.Attributes(), p.Exemplars(), extra))
		})
		return m.ExponentialHistogram().DataPoints().Len() == 0
	case pmetric.MetricTypeSummary:
		m.Summary().DataPoints().RemoveIf(func(p pmetric.SummaryDataPoint) bool {
			extra := 16 * p.QuantileValues().Len()
			return keep(point(p.Timestamp(), p.Attributes(), pmetric.NewExemplarSlice(), extra))
		})
		return m.Summary().DataPoints().Len() == 0
	default:
		return true // 빈 metric(type 없음)은 data point가 없으므로 거절 건수도 없다
	}
}

// histogramOK: bucket 수 = 경계 수 + 1, 경계 오름차순, bucket 합 = count (OTLP 규격 MUST, D06 §04 metric oracle 전제).
// bucket이 없는 point(count·sum만)는 허용한다.
func histogramOK(p pmetric.HistogramDataPoint) bool {
	buckets, bounds := p.BucketCounts(), p.ExplicitBounds()
	if buckets.Len() == 0 {
		return bounds.Len() == 0
	}
	if buckets.Len() != bounds.Len()+1 {
		return false
	}
	for i := 1; i < bounds.Len(); i++ {
		if bounds.At(i) <= bounds.At(i-1) {
			return false
		}
	}
	var sum uint64
	for i := 0; i < buckets.Len(); i++ {
		sum += buckets.At(i)
	}
	return sum == p.Count()
}

func dataPointCount(m pmetric.Metric) int {
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		return m.Gauge().DataPoints().Len()
	case pmetric.MetricTypeSum:
		return m.Sum().DataPoints().Len()
	case pmetric.MetricTypeHistogram:
		return m.Histogram().DataPoints().Len()
	case pmetric.MetricTypeExponentialHistogram:
		return m.ExponentialHistogram().DataPoints().Len()
	case pmetric.MetricTypeSummary:
		return m.Summary().DataPoints().Len()
	default:
		return 0
	}
}
