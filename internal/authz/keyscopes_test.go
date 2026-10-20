package authz

import "testing"

// key에 줄 수 있는 scope 목록이 발급 검증(validateKeyScopes)과 맞는다(ADR 0069).
func TestKeyScopeLists(t *testing.T) {
	for _, a := range IngestScopes {
		if err := validateKeyScopes(KindIngestKey, []Action{a}); err != nil {
			t.Errorf("ingest scope %s rejected: %v", a, err)
		}
	}
	for _, a := range APIKeyScopes {
		if err := validateKeyScopes(KindAPIKey, []Action{a}); err != nil {
			t.Errorf("api scope %s rejected: %v", a, err)
		}
		if !RoleTenantAdmin.Grants(a) && !RoleSecurityAuditor.Grants(a) {
			t.Errorf("api scope %s is granted to no role", a)
		}
	}
	for a := range stepUpActions {
		for _, s := range APIKeyScopes {
			if s == a {
				t.Errorf("step-up action %s listed as api key scope", a)
			}
		}
	}
}
