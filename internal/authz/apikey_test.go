package authz

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

var testPepper = bytes.Repeat([]byte{0x42}, 32)

func mustHasher(t *testing.T) KeyHasher {
	t.Helper()
	h, err := NewKeyHasher(testPepper)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestNewKeyHasherRequiresPepper(t *testing.T) {
	if _, err := NewKeyHasher(make([]byte, 31)); err == nil {
		t.Fatal("want error for short pepper")
	}
}

func TestGenerateFormatAndUniqueness(t *testing.T) {
	h := mustHasher(t)
	a, err := h.Generate(KindIngestKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Generate(KindIngestKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.Token, "mti_") || len(a.Token) != tokenLen {
		t.Errorf("token format: %q", a.Token)
	}
	if a.Token == b.Token || a.KeyID == b.KeyID {
		t.Error("generated keys must differ")
	}
	if strings.Contains(string(a.Hash), a.Token) || bytes.Contains(a.Hash, []byte(a.Token[len(a.Token)-43:])) {
		t.Error("hash must not contain the secret")
	}
	if _, err := h.Generate(KindUser, nil); err == nil {
		t.Error("Generate(KindUser) must fail")
	}
}

type store map[string]KeyRecord

func (s store) lookup(_ context.Context, keyID string) (KeyRecord, error) {
	r, ok := s[keyID]
	if !ok {
		return KeyRecord{}, ErrKeyNotFound
	}
	return r, nil
}

func TestAuthenticate(t *testing.T) {
	h := mustHasher(t)
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)

	ingest, _ := h.Generate(KindIngestKey, nil)
	api, _ := h.Generate(KindAPIKey, nil)
	revoked, _ := h.Generate(KindIngestKey, nil)
	expired, _ := h.Generate(KindIngestKey, nil)
	noExpiry, _ := h.Generate(KindIngestKey, nil)
	badScope, _ := h.Generate(KindIngestKey, nil)
	noEnv, _ := h.Generate(KindIngestKey, nil)
	revokedAt := now.Add(-time.Second)

	s := store{
		ingest.KeyID:   {KeyID: ingest.KeyID, Tenant: tenantA, Kind: KindIngestKey, Hash: ingest.Hash, Scopes: []Action{IngestTraces}, Environments: []string{"production"}, ExpiresAt: now.Add(time.Hour)},
		api.KeyID:      {KeyID: api.KeyID, Tenant: tenantB, Kind: KindAPIKey, Hash: api.Hash, Scopes: []Action{TelemetryRead}, ExpiresAt: now.Add(time.Hour), IssuerRole: RoleTenantAdmin},
		revoked.KeyID:  {KeyID: revoked.KeyID, Tenant: tenantA, Kind: KindIngestKey, Hash: revoked.Hash, Scopes: []Action{IngestTraces}, Environments: []string{"production"}, ExpiresAt: now.Add(time.Hour), RevokedAt: &revokedAt},
		expired.KeyID:  {KeyID: expired.KeyID, Tenant: tenantA, Kind: KindIngestKey, Hash: expired.Hash, Scopes: []Action{IngestTraces}, Environments: []string{"production"}, ExpiresAt: now},
		noExpiry.KeyID: {KeyID: noExpiry.KeyID, Tenant: tenantA, Kind: KindIngestKey, Hash: noExpiry.Hash, Scopes: []Action{IngestTraces}},
		noEnv.KeyID:    {KeyID: noEnv.KeyID, Tenant: tenantA, Kind: KindIngestKey, Hash: noEnv.Hash, Scopes: []Action{IngestTraces}, ExpiresAt: now.Add(time.Hour)},
		badScope.KeyID: {KeyID: badScope.KeyID, Tenant: tenantA, Kind: KindIngestKey, Hash: badScope.Hash, Scopes: []Action{TelemetryRead}, ExpiresAt: now.Add(time.Hour)},
	}

	t.Run("valid ingest key yields its stored tenant", func(t *testing.T) {
		p, err := h.Authenticate(context.Background(), ingest.Token, KindIngestKey, s.lookup, now)
		if err != nil {
			t.Fatal(err)
		}
		if p.Tenant() != tenantA || p.Kind() != KindIngestKey || p.Subject() != ingest.KeyID {
			t.Errorf("principal = %+v", p)
		}
		if err := Authorize(p, IngestTraces); err != nil {
			t.Errorf("ingest.traces: %v", err)
		}
		if err := Authorize(p, IngestLogs); !errors.Is(err, ErrForbidden) {
			t.Errorf("ingest.logs outside scope: %v", err)
		}
		if err := Authorize(p, TelemetryRead); !errors.Is(err, ErrForbidden) {
			t.Errorf("ingest key reading telemetry: %v", err)
		}
		// staging key로 production을 쓰는 식의 environment 교차를 막는다 (D04 §02).
		if !p.AllowsEnvironment("production") || p.AllowsEnvironment("staging") || p.AllowsEnvironment("") {
			t.Error("ingest key environment scope not enforced")
		}
	})

	t.Run("api key cannot ingest", func(t *testing.T) {
		p, err := h.Authenticate(context.Background(), api.Token, KindAPIKey, s.lookup, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := Authorize(p, IngestTraces); !errors.Is(err, ErrForbidden) {
			t.Errorf("api key ingest: %v", err)
		}
	})

	unauthenticated := []struct {
		name  string
		token string
		want  Kind
	}{
		{"ingest key on query endpoint", ingest.Token, KindAPIKey},
		{"api key on ingest endpoint", api.Token, KindIngestKey},
		{"wrong secret", ingest.Token[:len(ingest.Token)-1] + flip(ingest.Token[len(ingest.Token)-1]), KindIngestKey},
		{"unknown key id", "mti_ffffffffffffffff_" + ingest.Token[len(ingest.Token)-43:], KindIngestKey},
		{"secret swapped onto other key id", "mti_" + revoked.KeyID + "_" + ingest.Token[len(ingest.Token)-43:], KindIngestKey},
		{"revoked", revoked.Token, KindIngestKey},
		{"expired at boundary", expired.Token, KindIngestKey},
		{"record without expiry", noExpiry.Token, KindIngestKey},
		{"stored scope violates kind", badScope.Token, KindIngestKey},
		{"ingest record without environment", noEnv.Token, KindIngestKey},
		{"empty", "", KindIngestKey},
		{"garbage", "Bearer x", KindIngestKey},
		{"uppercase key id", "mti_" + strings.ToUpper(ingest.KeyID) + "_" + ingest.Token[len(ingest.Token)-43:], KindIngestKey},
		{"oversized", ingest.Token + "A", KindIngestKey},
	}
	for _, tc := range unauthenticated {
		t.Run(tc.name, func(t *testing.T) {
			p, err := h.Authenticate(context.Background(), tc.token, tc.want, s.lookup, now)
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
			if p.Authenticated() {
				t.Fatal("principal must be zero on failure")
			}
		})
	}

	t.Run("store failure is not reported as unauthenticated", func(t *testing.T) {
		boom := errors.New("pg down")
		_, err := h.Authenticate(context.Background(), ingest.Token, KindIngestKey,
			func(context.Context, string) (KeyRecord, error) { return KeyRecord{}, boom }, now)
		if !errors.Is(err, boom) || !errors.Is(err, ErrBackendUnavailable) || errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrBackendUnavailable wrapping store error", err)
		}
	})

	t.Run("pepper change invalidates keys", func(t *testing.T) {
		other, _ := NewKeyHasher(bytes.Repeat([]byte{0x43}, 32))
		if _, err := other.Authenticate(context.Background(), ingest.Token, KindIngestKey, s.lookup, now); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v", err)
		}
	})
}

// ADR 0015 §1: API key 실효 권한 = 저장 scope ∩ 발급자의 현재 role.
func TestAPIKeyBoundToIssuerCurrentRole(t *testing.T) {
	h := mustHasher(t)
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	key, _ := h.Generate(KindAPIKey, nil)
	ingest, _ := h.Generate(KindIngestKey, nil)
	record := func(role Role) store {
		return store{
			key.KeyID: {KeyID: key.KeyID, Tenant: tenantA, Kind: KindAPIKey, Hash: key.Hash,
				Scopes: []Action{TelemetryRead, DashboardsWrite}, ExpiresAt: now.Add(time.Hour), IssuerRole: role},
			// ingest key는 발급자가 없어져도(IssuerRole 비어 있음) 계속 동작한다.
			ingest.KeyID: {KeyID: ingest.KeyID, Tenant: tenantA, Kind: KindIngestKey, Hash: ingest.Hash,
				Scopes: []Action{IngestTraces}, Environments: []string{"production"}, ExpiresAt: now.Add(time.Hour)},
		}
	}

	t.Run("issuer still developer keeps both scopes", func(t *testing.T) {
		p, err := h.Authenticate(context.Background(), key.Token, KindAPIKey, record(RoleDeveloper).lookup, now)
		if err != nil {
			t.Fatal(err)
		}
		if Authorize(p, TelemetryRead) != nil || Authorize(p, DashboardsWrite) != nil {
			t.Error("scopes within issuer role must be allowed")
		}
	})
	t.Run("issuer demoted to viewer loses write scope", func(t *testing.T) {
		p, err := h.Authenticate(context.Background(), key.Token, KindAPIKey, record(RoleViewer).lookup, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := Authorize(p, TelemetryRead); err != nil {
			t.Errorf("read: %v", err)
		}
		if err := Authorize(p, DashboardsWrite); !errors.Is(err, ErrForbidden) {
			t.Errorf("write after demotion: %v, want ErrForbidden", err)
		}
	})
	t.Run("issuer removed invalidates api key", func(t *testing.T) {
		_, err := h.Authenticate(context.Background(), key.Token, KindAPIKey, record("").lookup, now)
		if !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	})
	t.Run("ingest key is org-owned", func(t *testing.T) {
		if _, err := h.Authenticate(context.Background(), ingest.Token, KindIngestKey, record("").lookup, now); err != nil {
			t.Fatalf("ingest key must not depend on issuer: %v", err)
		}
	})
}

func TestGeneratedKeyRedaction(t *testing.T) {
	g, err := mustHasher(t).Generate(KindAPIKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	secret := g.Token[len(g.Token)-43:]
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("issued", "key", g)
	outputs := []string{fmt.Sprint(g), fmt.Sprintf("%v %+v %#v %s", g, g, g, g), buf.String()}
	for _, out := range outputs {
		if strings.Contains(out, secret) {
			t.Errorf("secret leaked: %s", out)
		}
		if !strings.Contains(out, g.KeyID) {
			t.Errorf("key id missing: %s", out)
		}
	}
}

func flip(c byte) string {
	if c == 'A' {
		return "B"
	}
	return "A"
}
