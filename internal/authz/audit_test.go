package authz

import (
	"errors"
	"reflect"
	"testing"
)

func TestAuditCategories(t *testing.T) {
	for role, want := range map[Role][]string{
		RoleTenantAdmin:     {AuditCategoryOperations, AuditCategorySecurity},
		RoleSecurityAuditor: {AuditCategoryOperations, AuditCategorySecurity},
		RoleOperator:        {AuditCategoryOperations},
	} {
		got, err := AuditCategories(mustUser(t, tenantA, role, false))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v %v, want %v", role, got, err, want)
		}
	}
	for _, role := range []Role{RoleViewer, RoleDeveloper} {
		if _, err := AuditCategories(mustUser(t, tenantA, role, false)); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: %v, want ErrForbidden", role, err)
		}
	}
	if _, err := AuditCategories(Principal{}); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("zero principal: %v", err)
	}
}
