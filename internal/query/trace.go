// Package query는 조회 API다 (D02 §12~15, §19, §22).
//
// 저장소 접근은 internal/telemetrystore가 맡고, 이 패키지는 인가 범위 적용·응답 조립·HTTP를 맡는다.
// 응답은 missing·partial·sampled를 0이나 정상값으로 채우지 않는다 — 모르면 null이다 (CLAUDE.md 계약 6).
package query

import (
	"encoding/hex"
	"math"
	"sort"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// Trace 구조 불완전 사유 (D02 §22). 관측 범위 안의 구조만 판단하며 source의 모든 span 수집을 증명하지 않는다.
const (
	// ReasonMissingRoot: parent가 없는 span(root)이 보이지 않는다.
	ReasonMissingRoot = "missing_root"
	// ReasonMissingParent: parent_span_id가 가리키는 span이 보이지 않는다(미도착·sampling·권한 범위 밖 포함).
	ReasonMissingParent = "missing_parent"
	// ReasonSpanLimit: span 상한에 걸려 일부만 반환했다.
	ReasonSpanLimit = "span_limit_reached"
	// ReasonUndecodable: 저장된 payload를 해석하지 못한 span이 있다(그 span은 응답에서 뺀다).
	ReasonUndecodable = "payload_undecodable"
)

// SpanEvent는 span event다.
type SpanEvent struct {
	Name       string         `json:"name"`
	Time       time.Time      `json:"time"`
	Attributes map[string]any `json:"attributes"`
}

// SpanLink는 span link다.
type SpanLink struct {
	TraceID    string         `json:"trace_id"`
	SpanID     string         `json:"span_id"`
	Attributes map[string]any `json:"attributes"`
}

// TraceSpan은 응답 span이다. 시각은 RFC3339 UTC, 길이는 ns 단위를 이름에 붙인다 (D02 §12).
type TraceSpan struct {
	TraceID            string         `json:"trace_id"`
	SpanID             string         `json:"span_id"`
	ParentSpanID       *string        `json:"parent_span_id"` // root면 null
	ServiceID          string         `json:"service_id"`
	ServiceName        string         `json:"service_name"`
	Environment        *string        `json:"environment"` // resource에 없으면 null
	Name               string         `json:"name"`
	Kind               string         `json:"kind"`
	StatusCode         string         `json:"status_code"`
	StatusMessage      string         `json:"status_message"`
	StartTime          time.Time      `json:"start_time"`
	EndTime            time.Time      `json:"end_time"`
	DurationNs         uint64         `json:"duration_ns"`
	Attributes         map[string]any `json:"attributes"`
	ResourceAttributes map[string]any `json:"resource_attributes"`
	Events             []SpanEvent    `json:"events"`
	Links              []SpanLink     `json:"links"`
}

// Trace는 GET /traces/{trace_id}의 data다.
type Trace struct {
	TraceID   string      `json:"trace_id"`
	Spans     []TraceSpan `json:"spans"`
	SpanCount int         `json:"span_count"`
	Complete  bool        `json:"complete"`
	Reasons   []string    `json:"reasons"`
	// LastUpdatedAt은 보이는 span 중 가장 늦은 ingress 수신 시각이다 (D02 §22).
	LastUpdatedAt time.Time `json:"last_updated_at"`
}

const envAttr = "deployment.environment.name"

// rawMap은 타입 있는 속성을 JSON 값으로 바꾼다.
// JSON에 없는 NaN·±Inf는 문자열 "NaN"·"+Inf"·"-Inf"로 쓴다(값을 0이나 null로 바꾸지 않는다).
func rawMap(m pcommon.Map) map[string]any {
	out := m.AsRaw()
	for k, v := range out {
		out[k] = jsonSafe(v)
	}
	return out
}

func jsonSafe(v any) any {
	switch x := v.(type) {
	case float64:
		switch {
		case math.IsNaN(x):
			return "NaN"
		case math.IsInf(x, 1):
			return "+Inf"
		case math.IsInf(x, -1):
			return "-Inf"
		}
	case []any:
		for i := range x {
			x[i] = jsonSafe(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = jsonSafe(x[k])
		}
	}
	return v
}

// buildTrace는 저장소 record를 응답으로 조립한다.
//   - principal의 environment 범위 밖 span은 빼고, 그 존재를 따로 알리지 않는다 (D02 §13 "권한 밖 span과 그 구조는 노출하지 않는다").
//     그 결과 자식 span은 missing_parent로 보이며, 이는 미도착·sampling과 구별하지 않는다.
//   - 보이는 span이 없으면 ok=false (호출자는 404).
func buildTrace(p authz.Principal, traceID string, recs []telemetrystore.SpanRecord, limitHit bool) (Trace, bool) {
	t := Trace{TraceID: traceID, Spans: []TraceSpan{}, Reasons: []string{}}
	undecodable := false
	for _, rec := range recs {
		td, err := (&ptrace.ProtoUnmarshaler{}).UnmarshalTraces(rec.Payload)
		if err != nil || td.SpanCount() != 1 {
			undecodable = true
			continue
		}
		rs := td.ResourceSpans().At(0)
		res := rs.Resource().Attributes()
		var env *string
		if v, ok := res.Get(envAttr); ok && v.Type() == pcommon.ValueTypeStr && v.Str() != "" {
			e := v.Str()
			env = &e
		}
		// environment 제한 key는 환경이 표시되지 않은 span도 볼 수 없다(범위를 증명할 수 없음).
		if (env == nil && p.EnvironmentRestricted()) || (env != nil && !p.AllowsEnvironment(*env)) {
			continue
		}
		s := rs.ScopeSpans().At(0).Spans().At(0)
		svc := "unknown_service"
		if v, ok := res.Get("service.name"); ok && v.Str() != "" {
			svc = v.Str()
		}
		var parent *string
		if psid := s.ParentSpanID(); !psid.IsEmpty() {
			ps := hex.EncodeToString(psid[:])
			parent = &ps
		}
		ts := TraceSpan{
			TraceID:            rec.TraceID,
			SpanID:             rec.SpanID,
			ParentSpanID:       parent,
			ServiceID:          rec.ServiceID,
			ServiceName:        svc,
			Environment:        env,
			Name:               s.Name(),
			Kind:               spanKind(s.Kind()),
			StatusCode:         statusCode(s.Status().Code()),
			StatusMessage:      s.Status().Message(),
			StartTime:          s.StartTimestamp().AsTime().UTC(),
			EndTime:            s.EndTimestamp().AsTime().UTC(),
			DurationNs:         rec.DurationNs,
			Attributes:         rawMap(s.Attributes()),
			ResourceAttributes: rawMap(res),
			Events:             []SpanEvent{},
			Links:              []SpanLink{},
		}
		for i := 0; i < s.Events().Len(); i++ {
			e := s.Events().At(i)
			ts.Events = append(ts.Events, SpanEvent{Name: e.Name(), Time: e.Timestamp().AsTime().UTC(), Attributes: rawMap(e.Attributes())})
		}
		for i := 0; i < s.Links().Len(); i++ {
			l := s.Links().At(i)
			tid, sid := l.TraceID(), l.SpanID()
			ts.Links = append(ts.Links, SpanLink{TraceID: hex.EncodeToString(tid[:]), SpanID: hex.EncodeToString(sid[:]), Attributes: rawMap(l.Attributes())})
		}
		t.Spans = append(t.Spans, ts)
		if rec.ReceivedAt.After(t.LastUpdatedAt) {
			t.LastUpdatedAt = rec.ReceivedAt.UTC()
		}
	}
	if len(t.Spans) == 0 {
		return Trace{}, false
	}
	t.SpanCount = len(t.Spans)
	t.Reasons = completeness(t.Spans, limitHit, undecodable)
	t.Complete = len(t.Reasons) == 0
	return t, true
}

// completeness는 D02 §22의 구조 검사다. 사유는 정렬된 고정 코드다.
func completeness(spans []TraceSpan, limitHit, undecodable bool) []string {
	ids := make(map[string]bool, len(spans))
	for _, s := range spans {
		ids[s.SpanID] = true
	}
	hasRoot, missingParent := false, false
	for _, s := range spans {
		if s.ParentSpanID == nil {
			hasRoot = true
		} else if !ids[*s.ParentSpanID] {
			missingParent = true
		}
	}
	reasons := []string{}
	if !hasRoot {
		reasons = append(reasons, ReasonMissingRoot)
	}
	if missingParent {
		reasons = append(reasons, ReasonMissingParent)
	}
	if limitHit {
		reasons = append(reasons, ReasonSpanLimit)
	}
	if undecodable {
		reasons = append(reasons, ReasonUndecodable)
	}
	sort.Strings(reasons)
	return reasons
}

func spanKind(k ptrace.SpanKind) string {
	switch k {
	case ptrace.SpanKindInternal:
		return "internal"
	case ptrace.SpanKindServer:
		return "server"
	case ptrace.SpanKindClient:
		return "client"
	case ptrace.SpanKindProducer:
		return "producer"
	case ptrace.SpanKindConsumer:
		return "consumer"
	default:
		return "unspecified"
	}
}

func statusCode(c ptrace.StatusCode) string {
	switch c {
	case ptrace.StatusCodeOk:
		return "ok"
	case ptrace.StatusCodeError:
		return "error"
	default:
		return "unset"
	}
}
