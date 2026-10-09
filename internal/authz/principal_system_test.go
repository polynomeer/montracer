package authz

import (
	"errors"
	"testing"
)

func TestSystemPrincipal(t *testing.T) {
	tenant, _ := ParseTenantID("11111111-1111-4111-8111-111111111111")
	p, err := NewSystemPrincipal(tenant, "alert-worker", TelemetryRead)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind() != KindSystem || p.Kind().String() != "system" || p.Tenant() != tenant || p.Subject() != "alert-worker" || p.EnvironmentRestricted() {
		t.Errorf("principal = %+v", p)
	}
	if err := Authorize(p, TelemetryRead); err != nil {
		t.Errorf("telemetry.read: %v", err)
	}
	// 읽기 외 action은 없다: 쓰기·키·감사·삭제는 거절
	for _, a := range []Action{MonitorsWrite, KeysManage, AuditRead, DeletionRequest, IngestTraces} {
		if err := Authorize(p, a); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: %v", a, err)
		}
	}
	// 만들 때도 읽기 외 action·tenant 없음·subject 없음은 거절
	if _, err := NewSystemPrincipal(tenant, "w", MonitorsWrite); err == nil {
		t.Error("system principal must not hold write actions")
	}
	if _, err := NewSystemPrincipal(TenantID{}, "w", TelemetryRead); err == nil {
		t.Error("zero tenant must be rejected")
	}
	if _, err := NewSystemPrincipal(tenant, "", TelemetryRead); err == nil {
		t.Error("empty subject must be rejected")
	}
	if _, err := NewSystemPrincipal(tenant, "w"); err == nil {
		t.Error("no action must be rejected")
	}
}
