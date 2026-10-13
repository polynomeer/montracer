package controldb

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
)

// 경보 상태·값은 tenant 전체 telemetry를 평가한 결과다: monitors.read + telemetry.read, environment 제한 없음(ADR 0054 §1).
func TestAuthorizeMonitorState(t *testing.T) {
	now := time.Now()
	tenant, _ := authz.ParseTenantID("0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f00")
	key := func(envs []string, scopes ...authz.Action) authz.Principal {
		t.Helper()
		h, _ := authz.NewKeyHasher(bytes.Repeat([]byte{3}, 32))
		g, _ := h.Generate(authz.KindAPIKey, nil)
		rec := authz.KeyRecord{KeyID: g.KeyID, Tenant: tenant, Kind: authz.KindAPIKey, Hash: g.Hash, Scopes: scopes, Environments: envs,
			IssuerRole: authz.RoleTenantAdmin, ExpiresAt: now.Add(time.Hour)}
		p, err := h.Authenticate(context.Background(), g.Token, authz.KindAPIKey, func(context.Context, string) (authz.KeyRecord, error) { return rec, nil }, now)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	dev, err := authz.NewUserPrincipal(tenant, "dev", authz.RoleDeveloper, time.Time{}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		p    authz.Principal
		ok   bool
	}{
		{"developer", dev, true},
		{"tenant-wide key with telemetry.read", key(nil, authz.MonitorsRead, authz.TelemetryRead), true},
		{"environment-restricted key", key([]string{"staging"}, authz.MonitorsRead, authz.TelemetryRead), false},
		{"key without telemetry.read", key(nil, authz.MonitorsRead), false},
		{"key without monitors.read", key(nil, authz.TelemetryRead), false},
	} {
		if err := AuthorizeMonitorState(c.p); (err == nil) != c.ok || (err != nil && !errors.Is(err, authz.ErrForbidden)) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
