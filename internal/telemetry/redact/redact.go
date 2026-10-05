package redact

import (
	"fmt"
	"sort"
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// ReasonRedactionFailed는 redaction 중 실패해 저장하지 않은 record의 거절 사유다 (D04 §03).
const ReasonRedactionFailed = "redaction_failed"

// Redactor는 정책을 적용한다. 동시 사용에 안전하다(상태 없음).
type Redactor struct {
	c compiled
	// failHook은 테스트에서 record 처리 중 실패를 주입한다.
	failHook func()
}

// New는 정책으로 Redactor를 만든다.
func New(p Policy) *Redactor { return &Redactor{c: compile(p)} }

// PolicyVersion은 적용한 정책 버전이다.
func (r *Redactor) PolicyVersion() int64 { return r.c.version }

// Result는 redaction 결과다. Failed는 저장하지 않고 제거한 record 수다.
type Result struct {
	Records int
	Failed  int
	// Redactions는 치환 사유별 건수다(값 내용 없음). metric으로 내보낸다.
	Redactions map[Kind]int
}

// Message는 partial success용 요약이다.
func (r Result) Message() string {
	if r.Failed == 0 {
		return ""
	}
	return fmt.Sprintf("rejected %s=%d", ReasonRedactionFailed, r.Failed)
}

func (r *Result) merge(n counter) {
	if len(n) == 0 {
		return
	}
	if r.Redactions == nil {
		r.Redactions = map[Kind]int{}
	}
	for k, v := range n {
		r.Redactions[k] += v
	}
}

// guard는 record 하나의 redaction을 실행한다. panic이 나면 실패로 보고 원문을 남기지 않는다.
// 실패한 record는 호출자가 제거한다. recover 값은 원문을 담을 수 있어 버린다.
func (r *Redactor) guard(fn func(n counter)) (n counter, ok bool) {
	n = counter{}
	defer func() {
		if recover() != nil {
			n, ok = nil, false
		}
	}()
	if r.failHook != nil {
		r.failHook()
	}
	fn(n)
	return n, true
}

func (r *Redactor) attrs(m pcommon.Map, n counter) {
	m.RemoveIf(func(k string, v pcommon.Value) bool {
		switch r.c.action(k) {
		case drop:
			if strings.HasPrefix(strings.ToLower(k), reqHeaderPrefix) || strings.HasPrefix(strings.ToLower(k), respHeaderPrefix) {
				n[KindHeader]++
			} else {
				n[KindDeniedKey]++
			}
			return true
		case scrubURL:
			return !r.mapStrings(v, n, func(s string) string { return cleanURL(s, n) })
		case scrubSQL:
			return !r.mapStrings(v, n, func(s string) string { return scrubText(cleanSQL(s, n), n) })
		case truncIP:
			return !r.mapStrings(v, n, func(s string) string { return truncateIP(s, n) })
		default:
			r.value(v, n)
		}
		return false
	})
}

// value는 값 안의 문자열에 패턴 치환을 하고, 중첩 map에는 key 규칙을 적용한다.
func (r *Redactor) value(v pcommon.Value, n counter) {
	switch v.Type() {
	case pcommon.ValueTypeStr:
		v.SetStr(scrubText(v.Str(), n))
	case pcommon.ValueTypeBytes:
		// 구조를 모르는 bytes는 텍스트로 보고 탐지되면 통째로 지운다.
		before := sum(n)
		_ = scrubText(string(v.Bytes().AsRaw()), n)
		if sum(n) > before {
			v.SetEmptyBytes()
		}
	case pcommon.ValueTypeSlice:
		s := v.Slice()
		for i := 0; i < s.Len(); i++ {
			r.value(s.At(i), n)
		}
	case pcommon.ValueTypeMap:
		r.attrs(v.Map(), n)
	}
}

// mapStrings는 문자열 또는 문자열 배열(header·URL 목록 등)에 f를 적용한다.
// URL·SQL·IP 규칙을 적용할 수 없는 타입(map·bytes 등)이면 false를 반환하고 호출자가 필드를 지운다.
func (r *Redactor) mapStrings(v pcommon.Value, n counter, f func(string) string) bool {
	switch v.Type() {
	case pcommon.ValueTypeStr:
		v.SetStr(f(v.Str()))
		return true
	case pcommon.ValueTypeSlice:
		s := v.Slice()
		for i := 0; i < s.Len(); i++ {
			if !r.mapStrings(s.At(i), n, f) {
				return false
			}
		}
		return true
	case pcommon.ValueTypeInt, pcommon.ValueTypeDouble, pcommon.ValueTypeBool, pcommon.ValueTypeEmpty:
		return true
	default:
		n[KindNonString]++
		return false
	}
}

func (r *Redactor) scope(sc pcommon.InstrumentationScope, n counter) { r.attrs(sc.Attributes(), n) }

// Traces는 span을 redaction한다. 실패한 span은 제거한다.
// resource·scope 속성 처리가 실패하면 그 아래 span 전체를 실패로 본다.
func (r *Redactor) Traces(td ptrace.Traces) Result {
	var res Result
	td.ResourceSpans().RemoveIf(func(rs ptrace.ResourceSpans) bool {
		rn, rok := r.guard(func(n counter) { r.attrs(rs.Resource().Attributes(), n) })
		res.merge(rn)
		rs.ScopeSpans().RemoveIf(func(ss ptrace.ScopeSpans) bool {
			sn, sok := r.guard(func(n counter) { r.scope(ss.Scope(), n) })
			res.merge(sn)
			ss.Spans().RemoveIf(func(s ptrace.Span) bool {
				res.Records++
				n, ok := r.guard(func(n counter) {
					s.SetName(scrubText(s.Name(), n))
					s.Status().SetMessage(scrubText(s.Status().Message(), n))
					r.attrs(s.Attributes(), n)
					for i := 0; i < s.Events().Len(); i++ {
						e := s.Events().At(i)
						e.SetName(scrubText(e.Name(), n))
						r.attrs(e.Attributes(), n)
					}
					for i := 0; i < s.Links().Len(); i++ {
						r.attrs(s.Links().At(i).Attributes(), n)
					}
				})
				if !ok || !rok || !sok {
					res.Failed++
					return true
				}
				res.merge(n)
				return false
			})
			return ss.Spans().Len() == 0
		})
		return rs.ScopeSpans().Len() == 0
	})
	return res
}

// Logs는 log record를 redaction한다. body는 문자열·구조화 모두 처리한다.
func (r *Redactor) Logs(ld plog.Logs) Result {
	var res Result
	ld.ResourceLogs().RemoveIf(func(rl plog.ResourceLogs) bool {
		rn, rok := r.guard(func(n counter) { r.attrs(rl.Resource().Attributes(), n) })
		res.merge(rn)
		rl.ScopeLogs().RemoveIf(func(sl plog.ScopeLogs) bool {
			sn, sok := r.guard(func(n counter) { r.scope(sl.Scope(), n) })
			res.merge(sn)
			sl.LogRecords().RemoveIf(func(lr plog.LogRecord) bool {
				res.Records++
				n, ok := r.guard(func(n counter) {
					r.value(lr.Body(), n)
					r.attrs(lr.Attributes(), n)
					lr.SetSeverityText(scrubText(lr.SeverityText(), n))
					lr.SetEventName(scrubText(lr.EventName(), n))
				})
				if !ok || !rok || !sok {
					res.Failed++
					return true
				}
				res.merge(n)
				return false
			})
			return sl.LogRecords().Len() == 0
		})
		return rl.ScopeLogs().Len() == 0
	})
	return res
}

// Metrics는 data point와 exemplar 속성을 redaction한다. metric 이름·단위는 스키마이므로 그대로 둔다.
func (r *Redactor) Metrics(md pmetric.Metrics) Result {
	var res Result
	md.ResourceMetrics().RemoveIf(func(rm pmetric.ResourceMetrics) bool {
		rn, rok := r.guard(func(n counter) { r.attrs(rm.Resource().Attributes(), n) })
		res.merge(rn)
		rm.ScopeMetrics().RemoveIf(func(sm pmetric.ScopeMetrics) bool {
			sn, sok := r.guard(func(n counter) { r.scope(sm.Scope(), n) })
			res.merge(sn)
			sm.Metrics().RemoveIf(func(m pmetric.Metric) bool {
				mn, mok := r.guard(func(n counter) { r.attrs(m.Metadata(), n) })
				res.merge(mn)
				point := func(attrs pcommon.Map, ex pmetric.ExemplarSlice) bool {
					res.Records++
					n, ok := r.guard(func(n counter) {
						r.attrs(attrs, n)
						for i := 0; i < ex.Len(); i++ {
							r.attrs(ex.At(i).FilteredAttributes(), n)
						}
					})
					if !ok || !rok || !sok || !mok {
						res.Failed++
						return true
					}
					res.merge(n)
					return false
				}
				switch m.Type() {
				case pmetric.MetricTypeGauge:
					m.Gauge().DataPoints().RemoveIf(func(p pmetric.NumberDataPoint) bool { return point(p.Attributes(), p.Exemplars()) })
					return m.Gauge().DataPoints().Len() == 0
				case pmetric.MetricTypeSum:
					m.Sum().DataPoints().RemoveIf(func(p pmetric.NumberDataPoint) bool { return point(p.Attributes(), p.Exemplars()) })
					return m.Sum().DataPoints().Len() == 0
				case pmetric.MetricTypeHistogram:
					m.Histogram().DataPoints().RemoveIf(func(p pmetric.HistogramDataPoint) bool { return point(p.Attributes(), p.Exemplars()) })
					return m.Histogram().DataPoints().Len() == 0
				case pmetric.MetricTypeExponentialHistogram:
					m.ExponentialHistogram().DataPoints().RemoveIf(func(p pmetric.ExponentialHistogramDataPoint) bool {
						return point(p.Attributes(), p.Exemplars())
					})
					return m.ExponentialHistogram().DataPoints().Len() == 0
				case pmetric.MetricTypeSummary:
					m.Summary().DataPoints().RemoveIf(func(p pmetric.SummaryDataPoint) bool {
						return point(p.Attributes(), pmetric.NewExemplarSlice())
					})
					return m.Summary().DataPoints().Len() == 0
				default:
					return true
				}
			})
			return sm.Metrics().Len() == 0
		})
		return rm.ScopeMetrics().Len() == 0
	})
	return res
}

// SortedKinds는 결과 출력용으로 치환 사유를 정렬해 반환한다.
func (r Result) SortedKinds() []Kind {
	ks := make([]Kind, 0, len(r.Redactions))
	for k := range r.Redactions {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })
	return ks
}
