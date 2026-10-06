package authz

// 감사 이벤트 범주 (ADR 0015 §4). 제어 DB audit_events.category와 같은 값이다.
const (
	AuditCategoryOperations = "operations"
	AuditCategorySecurity   = "security"
)

// AuditCategories는 principal이 조회할 수 있는 감사 범주다 (ADR 0015 §4).
//   - audit.read: operations, security 모두
//   - audit.operations.read만: operations
//   - 둘 다 없으면 ErrForbidden(인증 안 됨은 ErrUnauthenticated)
func AuditCategories(p Principal) ([]string, error) {
	if err := Authorize(p, AuditRead); err == nil {
		return []string{AuditCategoryOperations, AuditCategorySecurity}, nil
	}
	if err := Authorize(p, AuditOperationsRead); err != nil {
		return nil, err
	}
	return []string{AuditCategoryOperations}, nil
}
