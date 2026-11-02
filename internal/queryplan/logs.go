package queryplan

// idOps·mapOps·rangeOps는 field 타입별 허용 연산자다.
var (
	idOps    = []string{"eq", "neq", "in"}
	mapOps   = []string{"eq", "neq", "in", "contains", "exists"}
	rangeOps = []string{"eq", "neq", "in", "gt", "gte", "lt", "lte"}
)

// LogCatalog는 log 검색 field다 (logs_local, D02 §09). column은 logs_local 기준이다.
//
//	service_id        UUID
//	service.name      서비스 이름(대소문자 무시). 서비스 catalog로 service_id 집합으로 풀린다(ADR 0039)
//	severity_number   0~24 (OTel SeverityNumber)
//	trace_id, span_id hex (trace·log 연결)
//	body              대소문자 무시 부분 문자열(contains)만
//	attributes.<key>  record 속성 (Map(String,String), 문자열 비교)
//	service.version, service.instance.id, k8s.namespace.name, k8s.pod.name
//	                  resource 속성 열(ADR 0075). 값 없는 행은 어떤 값 비교와도 맞지 않는다
var LogCatalog = NewCatalog(append([]Field{
	{Name: "service_id", Column: "service_id", Kind: UUID, Ops: idOps},
	{Name: "service.name", Column: "service_id", Kind: ServiceName, Ops: idOps},
	{Name: "severity_number", Column: "severity", Kind: Int, Ops: rangeOps, Min: 0, Max: 24},
	{Name: "trace_id", Column: "trace_id", Kind: TraceID, Ops: idOps},
	{Name: "span_id", Column: "span_id", Kind: SpanID, Ops: idOps},
	{Name: "body", Column: "body", Kind: Text, Ops: []string{"contains"}},
	{Name: "attributes", Column: "attributes", Kind: MapString, Ops: mapOps},
}, ResourceFields...)...)

// resourceOps는 resource 열 연산자다(ADR 0075 §3).
var resourceOps = []string{"eq", "neq", "in", "exists"}

// ResourceFields는 log·trace span 단계가 함께 쓰는 resource 속성 열이다(ADR 0075 §1).
var ResourceFields = []Field{
	{Name: "service.version", Column: "service_version", Kind: Resource, Ops: resourceOps},
	{Name: "service.instance.id", Column: "service_instance_id", Kind: Resource, Ops: resourceOps},
	{Name: "k8s.namespace.name", Column: "k8s_namespace_name", Kind: Resource, Ops: resourceOps},
	{Name: "k8s.pod.name", Column: "k8s_pod_name", Kind: Resource, Ops: resourceOps},
}
