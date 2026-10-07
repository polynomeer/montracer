package queryplan

// TraceSpanCatalog는 trace 검색의 span 단계 field다 (spans_local, D02 §09, ADR 0043).
// "이 조건에 맞는 span이 하나라도 있는 trace"를 고른다(Tempo·Jaeger 검색과 같은 의미).
//
//	service_id        UUID
//	service.name      서비스 이름(대소문자 무시, catalog로 service_id 집합, ADR 0039)
//	trace_id          hex 32
//	name              span 이름 (eq·neq·in·contains)
//	status            OTLP status code 0=unset·1=ok·2=error
//	span_kind         OTLP span kind 0~5
//	attributes.<key>  span 속성 (Map(String,String), 문자열 비교)
var TraceSpanCatalog = NewCatalog(
	Field{Name: "service_id", Column: "service_id", Kind: UUID, Ops: idOps},
	Field{Name: "service.name", Column: "service_id", Kind: ServiceName, Ops: idOps},
	Field{Name: "trace_id", Column: "trace_id", Kind: TraceID, Ops: idOps},
	Field{Name: "name", Column: "name", Kind: String, Ops: []string{"eq", "neq", "in", "contains"}},
	Field{Name: "status", Column: "status", Kind: Int, Ops: rangeOps, Min: 0, Max: 2},
	Field{Name: "span_kind", Column: "span_kind", Kind: Int, Ops: rangeOps, Min: 0, Max: 5},
	Field{Name: "attributes", Column: "attributes", Kind: MapString, Ops: mapOps},
)

// MaxTraceDurationMs는 duration_ms 비교 값 상한이다(검색 범위 24h + 앞뒤 여유).
const MaxTraceDurationMs = 3 * 24 * 60 * 60 * 1000

// TraceSummaryCatalog는 trace 요약 단계 field다. column은 저장소가 계산한 trace 요약 alias다.
//
//	duration_ms  trace 시작(가장 이른 span)부터 끝(가장 늦게 끝난 span)까지, 밀리초 (비교 값은 정수)
//	has_error    status=error인 span이 있다
//	span_count   span 수
var TraceSummaryCatalog = NewCatalog(
	Field{Name: "duration_ms", Column: "duration_ms", Kind: Int, Ops: []string{"gt", "gte", "lt", "lte"}, Min: 0, Max: MaxTraceDurationMs},
	Field{Name: "has_error", Column: "has_error", Kind: Bool, Ops: []string{"eq", "neq"}},
	Field{Name: "span_count", Column: "span_count", Kind: Int, Ops: []string{"eq", "gt", "gte", "lt", "lte"}, Min: 0, Max: 1_000_000},
)

// TraceCatalog는 두 단계 field를 합친 검증용 catalog다. 나누기 전에 요청 filter 전체의 한도(깊이·조건 수·구조)를 검사한다.
var TraceCatalog = NewCatalog(append(TraceSpanCatalog.all(), TraceSummaryCatalog.all()...)...)

// IsTraceSummaryField는 trace 요약 단계 field인지 본다(Split의 later).
func IsTraceSummaryField(name string) bool {
	_, _, ok := TraceSummaryCatalog.lookup(name)
	return ok
}
