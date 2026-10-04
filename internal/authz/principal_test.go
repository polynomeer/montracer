package authz

import (
	"context"
	"errors"
	"testing"
	"time"
)

var (
	tenantA = mustTenant("11111111-1111-4111-8111-111111111111")
	tenantB = mustTenant("22222222-2222-4222-8222-222222222222")
)

func mustTenant(s string) TenantID {
	t, err := ParseTenantID(s)
	if err != nil {
		panic(err)
	}
	return t
}

var testNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// mustUser: stepUp이면 1분 전 MFA step-up을 한 사용자.
func mustUser(t *testing.T, tenant TenantID, role Role, stepUp bool) Principal {
	t.Helper()
	var stepUpAt time.Time
	if stepUp {
		stepUpAt = testNow.Add(-time.Minute)
	}
	p, err := NewUserPrincipal(tenant, "user-1", role, stepUpAt, testNow)
	if err != nil {
		t.Fatalf("NewUserPrincipal: %v", err)
	}
	return p
}

func TestNewUserPrincipalRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name    string
		tenant  TenantID
		subject string
		role    Role
	}{
		{"zero tenant", TenantID{}, "u", RoleViewer},
		{"empty subject", tenantA, "", RoleViewer},
		{"long subject", tenantA, string(make([]byte, 257)), RoleViewer},
		{"unknown role", tenantA, "u", Role("superuser")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewUserPrincipal(tc.tenant, tc.subject, tc.role, time.Time{}, testNow); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestAuthorizeRoleMatrix(t *testing.T) {
	cases := []struct {
		role   Role
		action Action
		want   error
	}{
		{RoleViewer, TelemetryRead, nil},
		{RoleViewer, DashboardsWrite, ErrForbidden},
		{RoleViewer, MonitorsRead, ErrForbidden}, // D04 §01 Viewer 조회는 telemetry·dashboard
		{RoleDeveloper, MonitorsRead, nil},
		{RoleDeveloper, MonitorsWrite, nil},
		{RoleDeveloper, SilencesWrite, ErrForbidden},
		{RoleOperator, SilencesWrite, nil},
		{RoleOperator, PoliciesWrite, ErrForbidden},
		{RoleOperator, AuditRead, ErrForbidden},  // 보안 감사 전체는 불가
		{RoleOperator, AuditOperationsRead, nil}, // 운영 범주 감사만 (ADR 0015 §4)
		{RoleDeveloper, AuditOperationsRead, ErrForbidden},
		{RoleSecurityAuditor, AuditOperationsRead, nil},
		{RoleTenantAdmin, KeysRead, nil}, // key 조회는 step-up 불필요 (ADR 0015 §3)
		{RoleOperator, KeysRead, ErrForbidden},
		{RoleTenantAdmin, PoliciesWrite, nil},
		{RoleTenantAdmin, UsageRead, nil},
		{RoleSecurityAuditor, AuditRead, nil},
		{RoleSecurityAuditor, TelemetryRead, ErrForbidden}, // telemetry 본문 권한 없음
		{RoleSecurityAuditor, DashboardsRead, ErrForbidden},
		// 사람은 어떤 role이어도 수집 action을 가질 수 없다.
		{RoleTenantAdmin, IngestTraces, ErrForbidden},
	}
	for _, tc := range cases {
		t.Run(string(tc.role)+"/"+string(tc.action), func(t *testing.T) {
			p := mustUser(t, tenantA, tc.role, false)
			if err := Authorize(p, tc.action); !errors.Is(err, tc.want) { // errors.Is(nil, nil) == true
				t.Fatalf("Authorize = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAuthorizeStepUp(t *testing.T) {
	for _, a := range []Action{MembersManage, KeysManage, DeletionRequest} {
		if err := Authorize(mustUser(t, tenantA, RoleTenantAdmin, false), a); !errors.Is(err, ErrStepUpRequired) {
			t.Errorf("%s without step-up: %v, want ErrStepUpRequired", a, err)
		}
		if err := Authorize(mustUser(t, tenantA, RoleTenantAdmin, true), a); err != nil {
			t.Errorf("%s with step-up: %v", a, err)
		}
	}
	if err := Authorize(mustUser(t, tenantA, RoleTenantAdmin, false), KeysRead); err != nil {
		t.Errorf("KeysRead without step-up: %v", err)
	}
	// step-up은 권한 자체를 넓히지 않는다.
	if err := Authorize(mustUser(t, tenantA, RoleViewer, true), KeysManage); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer with step-up KeysManage: %v, want ErrForbidden", err)
	}
}

func TestStepUpWindow(t *testing.T) {
	cases := []struct {
		name     string
		stepUpAt time.Time
		want     error
	}{
		{"never", time.Time{}, ErrStepUpRequired},
		{"just now", testNow, nil},
		{"at window boundary", testNow.Add(-StepUpWindow), nil},
		{"just past window", testNow.Add(-StepUpWindow - time.Second), ErrStepUpRequired},
		{"future (clock skew)", testNow.Add(time.Second), ErrStepUpRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewUserPrincipal(tenantA, "admin", RoleTenantAdmin, tc.stepUpAt, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if err := Authorize(p, KeysManage); !errors.Is(err, tc.want) {
				t.Fatalf("Authorize = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestZeroPrincipalIsUnauthenticated(t *testing.T) {
	var p Principal
	if err := Authorize(p, TelemetryRead); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("Authorize(zero) = %v", err)
	}
	if err := CheckTenant(p, tenantA); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("CheckTenant(zero) = %v", err)
	}
}

func TestCheckTenantHidesOtherTenants(t *testing.T) {
	p := mustUser(t, tenantA, RoleTenantAdmin, true)
	if err := CheckTenant(p, tenantA); err != nil {
		t.Fatalf("same tenant: %v", err)
	}
	// 다른 tenant는 forbidden이 아니라 not found — 존재 여부를 누출하지 않는다.
	if err := CheckTenant(p, tenantB); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross tenant: %v, want ErrNotFound", err)
	}
	if err := CheckTenant(p, TenantID{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("zero resource tenant: %v, want ErrNotFound", err)
	}
}

func TestValidateKeyIssuance(t *testing.T) {
	admin := mustUser(t, tenantA, RoleTenantAdmin, true)
	prod := []string{"production"}
	cases := []struct {
		name    string
		issuer  Principal
		kind    Kind
		scopes  []Action
		envs    []string
		wantErr bool
	}{
		{"admin issues read key", admin, KindAPIKey, []Action{TelemetryRead}, nil, false},
		{"admin issues ingest key", admin, KindIngestKey, []Action{IngestTraces, IngestLogs}, prod, false},
		{"ingest key without environment", admin, KindIngestKey, []Action{IngestTraces}, nil, true},
		{"empty environment name", admin, KindIngestKey, []Action{IngestTraces}, []string{""}, true},
		{"no step-up", mustUser(t, tenantA, RoleTenantAdmin, false), KindAPIKey, []Action{TelemetryRead}, nil, true},
		{"developer cannot issue", mustUser(t, tenantA, RoleDeveloper, true), KindAPIKey, []Action{TelemetryRead}, nil, true},
		{"api key with ingest scope", admin, KindAPIKey, []Action{IngestTraces}, nil, true},
		{"ingest key with read scope", admin, KindIngestKey, []Action{TelemetryRead}, prod, true},
		{"api key with step-up scope", admin, KindAPIKey, []Action{KeysManage}, nil, true},
		{"empty scopes", admin, KindAPIKey, nil, nil, true},
		{"user kind", admin, KindUser, []Action{TelemetryRead}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateKeyIssuance(tc.issuer, tc.kind, tc.scopes, tc.envs)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			// 발급 tenant는 항상 issuer tenant다.
			if err == nil && (!got.Valid() || got.Tenant() != tc.issuer.Tenant() || got.IssuedBy() != tc.issuer.Subject()) {
				t.Fatalf("issuance not bound to issuer: %+v", got)
			}
		})
	}
}

func TestZeroKeyIssuanceIsInvalid(t *testing.T) {
	if (KeyIssuance{}).Valid() {
		t.Fatal("zero KeyIssuance must be invalid")
	}
}

func TestCheckRequestTenant(t *testing.T) {
	p := mustUser(t, tenantA, RoleViewer, false)
	if err := CheckRequestTenant(p, tenantA); err != nil {
		t.Fatalf("same tenant: %v", err)
	}
	// URL tenant 불일치는 403 (D02 §12) — resource 검사(404)와 다르다.
	if err := CheckRequestTenant(p, tenantB); !errors.Is(err, ErrForbidden) {
		t.Errorf("mismatch: %v, want ErrForbidden", err)
	}
	if err := CheckRequestTenant(Principal{}, tenantA); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("zero principal: %v", err)
	}
}

func TestContextRoundTrip(t *testing.T) {
	if _, err := FromContext(context.Background()); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("empty context: %v", err)
	}
	if _, err := FromContext(WithPrincipal(context.Background(), Principal{})); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("zero principal in context: %v", err)
	}
	p := mustUser(t, tenantA, RoleViewer, false)
	got, err := FromContext(WithPrincipal(context.Background(), p))
	if err != nil || got.Tenant() != tenantA || got.Kind() != KindUser {
		t.Errorf("round trip = %+v, %v", got, err)
	}
}
