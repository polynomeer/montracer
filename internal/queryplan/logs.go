package queryplan

// idOps·mapOps·rangeOps는 field 타입별 허용 연산자다.
var (
	idOps    = []string{"eq", "neq", "in"}
	mapOps   = []string{"eq", "neq", "in", "contains", "exists"}
	rangeOps = []string{"eq", "neq", "in", "gt", "gte", "lt", "lte"}
)

// LogCatalog는 log 검색 field다 (logs_local, D02 §09). column은 logs_local 기준이다.
//
//	service_id        UUID (서비스 catalog 전이라 이름 대신 ID, ADR 0037 §4)
//	severity_number   0~24 (OTel SeverityNumber)
//	trace_id, span_id hex (trace·log 연결)
//	body              대소문자 무시 부분 문자열(contains)만
//	attributes.<key>  record 속성 (Map(String,String), 문자열 비교)
var LogCatalog = NewCatalog(
	Field{Name: "service_id", Column: "service_id", Kind: UUID, Ops: idOps},
	Field{Name: "severity_number", Column: "severity", Kind: Int, Ops: rangeOps, Min: 0, Max: 24},
	Field{Name: "trace_id", Column: "trace_id", Kind: TraceID, Ops: idOps},
	Field{Name: "span_id", Column: "span_id", Kind: SpanID, Ops: idOps},
	Field{Name: "body", Column: "body", Kind: Text, Ops: []string{"contains"}},
	Field{Name: "attributes", Column: "attributes", Kind: MapString, Ops: mapOps},
)
