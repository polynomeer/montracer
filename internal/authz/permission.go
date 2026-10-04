package authz

// Action은 권한 검사 단위다 (D04 §01, §12).
type Action string

// 조회·운영 action.
const (
	TelemetryRead    Action = "telemetry.read"
	DashboardsRead   Action = "dashboards.read"
	MonitorsRead     Action = "monitors.read"
	SavedSearchWrite Action = "saved_searches.write"
	DashboardsWrite  Action = "dashboards.write"
	MonitorsWrite    Action = "monitors.write"
	SilencesWrite    Action = "silences.write"
	PoliciesPropose  Action = "policies.propose"
	DeploymentsWrite Action = "deployments.write"
	PoliciesRead     Action = "policies.read"
	PoliciesWrite    Action = "policies.write"
	MembersManage    Action = "members.manage"
	KeysRead         Action = "keys.read"
	KeysManage       Action = "keys.manage"
	DeletionRequest  Action = "deletion.request"
	AuditRead        Action = "audit.read"
	// AuditOperationsRead는 운영 범주 감사 이벤트만 조회한다 (ADR 0015 §4).
	AuditOperationsRead Action = "audit.operations.read"
	UsageRead           Action = "usage.read"
)

// 수집 action. ingest key에만 허용되며 사람·query API key에는 부여하지 않는다 (D02 §12).
const (
	IngestTraces  Action = "ingest.traces"
	IngestMetrics Action = "ingest.metrics"
	IngestLogs    Action = "ingest.logs"
)

func isIngestAction(a Action) bool {
	return a == IngestTraces || a == IngestMetrics || a == IngestLogs
}

// stepUpActions는 MFA step-up 이후에만 허용한다 (D04 §01 Tenant Admin, §02).
var stepUpActions = map[Action]bool{
	MembersManage:   true,
	KeysManage:      true,
	DeletionRequest: true,
}

// Role은 조직 단위 역할이다 (D04 §01 권한 모델 표).
type Role string

const (
	RoleViewer          Role = "viewer"
	RoleDeveloper       Role = "developer"
	RoleOperator        Role = "operator"
	RoleTenantAdmin     Role = "tenant_admin"
	RoleSecurityAuditor Role = "security_auditor"
)

var (
	viewerActions    = []Action{TelemetryRead, DashboardsRead, SavedSearchWrite}
	developerActions = append(append([]Action{}, viewerActions...), DashboardsWrite, MonitorsRead, MonitorsWrite)
	// Operator의 "범위 내 운영 감사"(D04 §01)는 운영 범주 감사만이다 (ADR 0015 §4).
	operatorActions = append(append([]Action{}, developerActions...),
		SilencesWrite, PoliciesPropose, DeploymentsWrite, AuditOperationsRead)
	adminActions = append(append([]Action{}, operatorActions...),
		PoliciesRead, PoliciesWrite, MembersManage, KeysRead, KeysManage, DeletionRequest, AuditRead, UsageRead)
	// Security Auditor는 telemetry 본문 권한이 없다 (D04 §01).
	auditorActions = []Action{AuditRead, AuditOperationsRead, PoliciesRead}
)

var roleGrants = map[Role]map[Action]bool{
	RoleViewer:          toSet(viewerActions),
	RoleDeveloper:       toSet(developerActions),
	RoleOperator:        toSet(operatorActions),
	RoleTenantAdmin:     toSet(adminActions),
	RoleSecurityAuditor: toSet(auditorActions),
}

func toSet(actions []Action) map[Action]bool {
	s := make(map[Action]bool, len(actions))
	for _, a := range actions {
		s[a] = true
	}
	return s
}

// Valid는 정의된 role인지 보고한다.
func (r Role) Valid() bool {
	_, ok := roleGrants[r]
	return ok
}

// Grants는 role이 action을 허용하는지 보고한다.
func (r Role) Grants(a Action) bool {
	return roleGrants[r][a]
}
