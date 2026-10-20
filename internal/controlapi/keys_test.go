package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/session"
)

// fakeKeys는 시험용 key 저장소다.
type fakeKeys struct {
	keys    []controldb.KeyMetadata
	idem    map[string]controldb.StoredResponse
	revoked map[string]time.Time
	full    bool
}

func newFakeKeys() *fakeKeys {
	return &fakeKeys{idem: map[string]controldb.StoredResponse{}, revoked: map[string]time.Time{}}
}

func (f *fakeKeys) ListKeys(_ context.Context, p authz.Principal) ([]controldb.KeyMetadata, error) {
	if err := authz.AuthorizeTenantWide(p, authz.KeysRead); err != nil {
		return nil, err
	}
	return f.keys, nil
}

func (f *fakeKeys) CreateKeyIdempotent(_ context.Context, p authz.Principal, iss authz.KeyIssuance, gen authz.GeneratedKey, name string, expiresAt time.Time,
	idem controldb.IdempotencyRequest, _ string, respond func(controldb.KeyMetadata) (controldb.StoredResponse, error)) (controldb.StoredResponse, bool, error) {
	if err := authz.Authorize(p, authz.KeysManage); err != nil {
		return controldb.StoredResponse{}, false, err
	}
	if r, ok := f.idem[idem.Key]; ok {
		return r, true, nil
	}
	if f.full {
		return controldb.StoredResponse{}, false, controldb.ErrTooManyKeys
	}
	m := controldb.KeyMetadata{KeyID: gen.KeyID, Kind: iss.Kind(), Scopes: iss.Scopes(), Environments: iss.Environments(), IssuedBy: iss.IssuedBy(),
		ExpiresAt: expiresAt, CreatedAt: now, Revision: 1}
	if name != "" {
		m.Name = &name
	}
	f.keys = append(f.keys, m)
	r, err := respond(m)
	f.idem[idem.Key] = r
	return r, false, err
}

func (f *fakeKeys) RevokeKey(_ context.Context, p authz.Principal, keyID string, at time.Time, _ string) error {
	if err := authz.Authorize(p, authz.KeysManage); err != nil {
		return err
	}
	for _, k := range f.keys {
		if k.KeyID == keyID {
			f.revoked[keyID] = at
			return nil
		}
	}
	return authz.ErrNotFound
}

func keyGenerator(t *testing.T) KeyGenerator {
	t.Helper()
	h, err := authz.NewKeyHasher(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return func(kind authz.Kind) (authz.GeneratedKey, error) { return h.Generate(kind, nil) }
}

func TestKeysAPI(t *testing.T) {
	store := newSessionStore()
	alice, _ := authz.OIDCUserID(issuer, "alice")
	dave, _ := authz.OIDCUserID(issuer, "dave")
	store.members[[2]string{tenantA.String(), alice}] = authz.RoleTenantAdmin
	store.members[[2]string{tenantA.String(), dave}] = authz.RoleViewer
	clock := now
	idp := &fakeIdP{codes: map[string]session.Identity{}}
	h := authHandlerACR(t, store, idp, &clock, "")
	keys := h.cfg.Keys.(*fakeKeys)
	a := &browser{t: t, h: h, cookies: map[string]string{}}
	a.login(idp, "alice", "/")
	csrf := decodeSession(t, a.do(http.MethodGet, "/api/v1/auth/session", "", nil)).Data.CSRFToken
	write := func(extra map[string]string) map[string]string {
		hdr := map[string]string{session.CSRFHeader: csrf}
		for k, v := range extra {
			hdr[k] = v
		}
		return hdr
	}

	// 조회는 Tenant Admin(step-up 불필요), 조회자는 403
	if res := a.do(http.MethodGet, "/api/v1/keys", "", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %s", res.StatusCode, res.Body)
	}
	d := &browser{t: t, h: h, cookies: map[string]string{}}
	d.login(idp, "dave", "/")
	if res := d.do(http.MethodGet, "/api/v1/keys", "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer lists keys: %d", res.StatusCode)
	}

	issueBody := `{"kind":"api","name":"ci reader","scopes":["telemetry.read"],"expires_in_days":90}`
	// 발급은 step-up이 필요하다
	res := a.do(http.MethodPost, "/api/v1/keys", issueBody, write(map[string]string{"Idempotency-Key": "k-1"}))
	if res.StatusCode != http.StatusForbidden || decodeErr(t, res).Error.Details["step_up_required"] != true {
		t.Fatalf("create without step-up: %d %s", res.StatusCode, res.Body)
	}
	a.stepUp(idp, csrf, session.Identity{Issuer: issuer, Subject: "alice", AuthTime: clock, AMR: []string{"mfa"}}, "/")

	// 원문은 한 번만, 재생에는 없다
	res = a.do(http.MethodPost, "/api/v1/keys", issueBody, write(map[string]string{"Idempotency-Key": "k-1"}))
	var created struct {
		Data  KeyItem      `json:"data"`
		Token *string      `json:"token"`
		Meta  MutationMeta `json:"meta"`
	}
	if res.StatusCode != http.StatusCreated || json.Unmarshal(res.Body, &created) != nil || created.Token == nil ||
		!strings.HasPrefix(*created.Token, "mta_"+created.Data.KeyID+"_") || created.Data.TokenHint != "mta_"+created.Data.KeyID+"_…" ||
		created.Data.Kind != "api" || created.Data.Name == nil || *created.Data.Name != "ci reader" || created.Data.IssuedBy != alice ||
		created.Data.ExpiresAt != now.Add(90*24*time.Hour).Format(time.RFC3339Nano) || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d %s", res.StatusCode, res.Body)
	}
	replay := a.do(http.MethodPost, "/api/v1/keys", issueBody, write(map[string]string{"Idempotency-Key": "k-1"}))
	if replay.StatusCode != http.StatusCreated || replay.Header.Get("Idempotent-Replayed") != "true" || strings.Contains(string(replay.Body), *created.Token) ||
		!strings.Contains(string(replay.Body), WarnTokenNotReplayed) || len(keys.keys) != 1 {
		t.Fatalf("replay: %d %s", replay.StatusCode, replay.Body)
	}
	// 목록에는 원문이 없다
	if res := a.do(http.MethodGet, "/api/v1/keys", "", nil); res.StatusCode != http.StatusOK || strings.Contains(string(res.Body), *created.Token) ||
		!strings.Contains(string(res.Body), created.Data.KeyID) {
		t.Fatalf("list after create: %d %s", res.StatusCode, res.Body)
	}
	// ingest key: environment 필수, mti_ 접두어
	res = a.do(http.MethodPost, "/api/v1/keys", `{"kind":"ingest","scopes":["ingest.traces","ingest.logs"],"environments":["prod"],"expires_in_days":365}`,
		write(map[string]string{"Idempotency-Key": "k-2"}))
	if res.StatusCode != http.StatusCreated || !strings.Contains(string(res.Body), `"token":"mti_`) {
		t.Fatalf("create ingest: %d %s", res.StatusCode, res.Body)
	}

	// 검증
	for name, body := range map[string]string{
		"no key kind":            `{"kind":"root","scopes":["telemetry.read"],"expires_in_days":1}`,
		"ingest without env":     `{"kind":"ingest","scopes":["ingest.traces"],"expires_in_days":30}`,
		"ingest with api scope":  `{"kind":"ingest","scopes":["telemetry.read"],"environments":["prod"],"expires_in_days":30}`,
		"api with ingest scope":  `{"kind":"api","scopes":["ingest.traces"],"expires_in_days":30}`,
		"api with step-up scope": `{"kind":"api","scopes":["members.manage"],"expires_in_days":30}`,
		"duplicate scope":        `{"kind":"api","scopes":["telemetry.read","telemetry.read"],"expires_in_days":30}`,
		"no scope":               `{"kind":"api","scopes":[],"expires_in_days":30}`,
		"zero lifetime":          `{"kind":"api","scopes":["telemetry.read"],"expires_in_days":0}`,
		"too long lifetime":      `{"kind":"api","scopes":["telemetry.read"],"expires_in_days":366}`,
		"bad environment":        `{"kind":"ingest","scopes":["ingest.traces"],"environments":["prod env"],"expires_in_days":30}`,
		"blank name":             `{"kind":"api","name":" ","scopes":["telemetry.read"],"expires_in_days":30}`,
	} {
		if res := a.do(http.MethodPost, "/api/v1/keys", body, write(map[string]string{"Idempotency-Key": "v-" + name})); res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, res.StatusCode, res.Body)
		}
	}
	if res := a.do(http.MethodPost, "/api/v1/keys", issueBody, write(nil)); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("create without Idempotency-Key: %d", res.StatusCode)
	}
	keys.full = true
	if res := a.do(http.MethodPost, "/api/v1/keys", issueBody, write(map[string]string{"Idempotency-Key": "k-full"})); res.StatusCode != http.StatusConflict {
		t.Fatalf("over limit: %d", res.StatusCode)
	}

	// 폐기: 바로 또는 겹침(최대 24시간)
	path := "/api/v1/keys/" + created.Data.KeyID
	if res := a.do(http.MethodDelete, path+"?grace_seconds=3600", "", write(nil)); res.StatusCode != http.StatusNoContent ||
		!keys.revoked[created.Data.KeyID].Equal(now.Add(time.Hour)) {
		t.Fatalf("revoke with grace: %d %v", res.StatusCode, keys.revoked)
	}
	if res := a.do(http.MethodDelete, path, "", write(nil)); res.StatusCode != http.StatusNoContent || !keys.revoked[created.Data.KeyID].Equal(now) {
		t.Fatalf("revoke now: %d", res.StatusCode)
	}
	for _, bad := range []string{path + "?grace_seconds=86401", path + "?grace_seconds=-1", "/api/v1/keys/NOTHEX0000000000"} {
		if res := a.do(http.MethodDelete, bad, "", write(nil)); res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, res.StatusCode)
		}
	}
	if res := a.do(http.MethodDelete, "/api/v1/keys/0123456789abcdef", "", write(nil)); res.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke unknown: %d", res.StatusCode)
	}
	if res := a.do(http.MethodDelete, path, "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("revoke without csrf: %d", res.StatusCode)
	}

	// key API를 끄면 404
	h.cfg.Keys = nil
	if res := a.do(http.MethodGet, "/api/v1/keys", "", nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("keys off: %d", res.StatusCode)
	}
}
