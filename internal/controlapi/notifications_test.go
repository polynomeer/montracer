package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/notify"
)

// fakePolicies는 policy 저장소 계약(권한·idempotency·revision)을 메모리로 흉내 낸다. 실제 SQL·RLS는 controldb 통합 시험이 본다.
type fakePolicies struct {
	byID    map[string]*controldb.NotificationPolicy
	sealed  map[string]controldb.SealedSecret
	urls    map[string]controldb.Sealed
	inUse   map[string]bool
	idem    map[string]controldb.StoredResponse
	creates int
}

func newFakePolicies() *fakePolicies {
	return &fakePolicies{byID: map[string]*controldb.NotificationPolicy{}, sealed: map[string]controldb.SealedSecret{}, idem: map[string]controldb.StoredResponse{},
		urls: map[string]controldb.Sealed{}, inUse: map[string]bool{}}
}

func (f *fakePolicies) CreatePolicy(_ context.Context, p authz.Principal, w controldb.PolicyWrite, seal func(string) (controldb.SealedSecret, error),
	idem controldb.IdempotencyRequest, _ string, respond func(controldb.NotificationPolicy) (controldb.StoredResponse, error)) (controldb.StoredResponse, bool, error) {
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return controldb.StoredResponse{}, false, err
	}
	if r, ok := f.idem[idem.Key]; ok {
		return r, true, nil
	}
	f.creates++
	id := fmt.Sprintf("8d49d404-4115-42f8-9c59-f94ff40f%04d", f.creates)
	sec, err := seal(id)
	if err != nil {
		return controldb.StoredResponse{}, false, err
	}
	u, err := w.SealURL(id)
	if err != nil {
		return controldb.StoredResponse{}, false, err
	}
	f.urls[id] = u
	np := &controldb.NotificationPolicy{ID: id, Name: w.Name, WebhookURL: w.Display, SecretKeyID: sec.KeyID, Enabled: w.Enabled, Revision: 1,
		CreatedBy: p.Subject(), UpdatedBy: p.Subject(), CreatedAt: now, UpdatedAt: now}
	f.byID[id], f.sealed[id] = np, sec
	r, err := respond(*np)
	f.idem[idem.Key] = r
	return r, false, err
}

func (f *fakePolicies) GetPolicy(_ context.Context, p authz.Principal, id string) (controldb.NotificationPolicy, error) {
	if err := authz.Authorize(p, authz.MonitorsRead); err != nil {
		return controldb.NotificationPolicy{}, err
	}
	np, ok := f.byID[id]
	if !ok {
		return controldb.NotificationPolicy{}, authz.ErrNotFound
	}
	return *np, nil
}

func (f *fakePolicies) ListPolicies(_ context.Context, p authz.Principal, _ *controldb.PolicyPosition, _ int) ([]controldb.NotificationPolicy, []controldb.PolicyPosition, bool, error) {
	if err := authz.Authorize(p, authz.MonitorsRead); err != nil {
		return nil, nil, false, err
	}
	var out []controldb.NotificationPolicy
	for _, np := range f.byID {
		out = append(out, *np)
	}
	return out, nil, false, nil
}

func (f *fakePolicies) UpdatePolicy(_ context.Context, p authz.Principal, id string, ifMatch int64, w controldb.PolicyWrite, _ string) (controldb.NotificationPolicy, error) {
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return controldb.NotificationPolicy{}, err
	}
	np, ok := f.byID[id]
	if !ok {
		return controldb.NotificationPolicy{}, authz.ErrNotFound
	}
	if np.Revision != ifMatch {
		return controldb.NotificationPolicy{}, controldb.ErrRevisionMismatch
	}
	u, err := w.SealURL(id)
	if err != nil {
		return controldb.NotificationPolicy{}, err
	}
	f.urls[id] = u
	np.Name, np.WebhookURL, np.Enabled, np.Revision = w.Name, w.Display, w.Enabled, np.Revision+1
	return *np, nil
}

func (f *fakePolicies) DeletePolicy(_ context.Context, p authz.Principal, id string, _ *int64, _ string) (int64, error) {
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return 0, err
	}
	np, ok := f.byID[id]
	if !ok {
		return 0, authz.ErrNotFound
	}
	if f.inUse[id] {
		return 0, controldb.ErrPolicyInUse
	}
	delete(f.byID, id)
	return np.Revision + 1, nil
}

func policyHandler(t *testing.T, store PolicyStore, sealer *notify.Sealer, ps principals) *Handler {
	t.Helper()
	signer, err := apicursor.NewSigner([]byte("0123456789abcdef0123456789abcdef"), time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(Config{
		Authenticate: func(_ context.Context, token string) (authz.Principal, error) {
			if p, ok := ps[token]; ok {
				return p, nil
			}
			return authz.Principal{}, authz.ErrUnauthenticated
		},
		Audit: &fakeAudit{}, Monitors: newFakeMonitors(), Policies: store, Sealer: sealer, Cursor: signer, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func testSealer(t *testing.T) *notify.Sealer {
	t.Helper()
	s, err := notify.NewSealer("v1", bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const policyBody = `{"name":"On-call","webhook":{"url":"https://hooks.example.com/services/T0/B0/receiver-token?x=1"}}`

type policyEnvelope struct {
	Data          PolicyItem   `json:"data"`
	SigningSecret *string      `json:"signing_secret"`
	Meta          MutationMeta `json:"meta"`
}

func decodePolicy(t *testing.T, body []byte) policyEnvelope {
	t.Helper()
	var env policyEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return env
}

// 생성: 서명 secret은 이 응답에서만 보이고, 저장된 암호문으로 같은 secret이 풀린다. URL은 scheme·host만 보인다.
// 같은 Idempotency-Key 재생은 새로 만들지 않고 secret 없이(경고와 함께) 돌려준다.
func TestPolicyCreateSecretOnce(t *testing.T) {
	store, sealer := newFakePolicies(), testSealer(t)
	h := policyHandler(t, store, sealer, principals{"admin": user(t, authz.RoleTenantAdmin)})
	idem := map[string]string{"Idempotency-Key": "p1"}
	rec := do(t, h, http.MethodPost, "/api/v1/notification-policies", "admin", policyBody, idem)
	env := decodePolicy(t, rec.Body.Bytes())
	if rec.Code != http.StatusCreated || env.SigningSecret == nil || !strings.HasPrefix(*env.SigningSecret, "whsec_") ||
		env.Data.Webhook.URL != "https://hooks.example.com/…" || !env.Data.Enabled || env.Data.Webhook.KeyID == "" || len(env.Meta.Warnings) != 0 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "receiver-token") || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("ETag") != `"1"` {
		t.Errorf("create leaks URL or headers wrong: %s %v", rec.Body, rec.Header())
	}
	sec, err := sealer.Open(tenantA.String(), env.Data.ID, env.Data.Webhook.KeyID, "v1", store.sealed[env.Data.ID].Ciphertext)
	if err != nil || (notify.Secret{KeyID: sec.KeyID, Key: sec.Key}).Plain() != *env.SigningSecret {
		t.Errorf("stored secret does not match the shown one: %v", err)
	}
	// URL은 policy에 묶인 암호문으로만 저장된다(평문 전체 URL은 저장소로 가지 않는다)
	if u, err := sealer.OpenURL(tenantA.String(), env.Data.ID, store.urls[env.Data.ID].Version, store.urls[env.Data.ID].Ciphertext); err != nil ||
		u != "https://hooks.example.com/services/T0/B0/receiver-token?x=1" || store.byID[env.Data.ID].WebhookURL != "https://hooks.example.com/…" {
		t.Errorf("stored url: %q %v", u, err)
	}
	replay := do(t, h, http.MethodPost, "/api/v1/notification-policies", "admin", policyBody, idem)
	renv := decodePolicy(t, replay.Body.Bytes())
	if replay.Code != http.StatusCreated || renv.SigningSecret != nil || renv.Data.ID != env.Data.ID || store.creates != 1 ||
		replay.Header().Get("Idempotent-Replayed") != "true" || len(renv.Meta.Warnings) != 1 || renv.Meta.Warnings[0] != WarnSigningSecretNotReplayed {
		t.Errorf("replay: %d %s", replay.Code, replay.Body)
	}
	// 저장된(재생) 응답에 secret 평문이 없다
	if bytes.Contains(store.idem["p1"].Body, []byte(*env.SigningSecret)) {
		t.Error("stored idempotent response contains the secret")
	}
}

func TestPolicyValidationAndPermissions(t *testing.T) {
	store := newFakePolicies()
	h := policyHandler(t, store, testSealer(t), principals{"admin": user(t, authz.RoleTenantAdmin), "dev": user(t, authz.RoleDeveloper), "viewer": user(t, authz.RoleViewer)})
	idem := map[string]string{"Idempotency-Key": "k"}
	for _, body := range []string{
		`{"name":"x","webhook":{"url":"http://hooks.example.com/x"}}`,
		`{"name":"x","webhook":{"url":"https://169.254.169.254/latest/meta-data"}}`,
		`{"name":"x","webhook":{"url":"https://localhost/x"}}`,
		`{"name":"x","webhook":{"url":"https://user:pw@hooks.example.com/x"}}`,
		`{"name":"","webhook":{"url":"https://hooks.example.com/x"}}`,
		`{"name":"x"}`,
		`{"name":"x","webhook":{"url":"https://hooks.example.com/x"},"secret":"mine"}`,
	} {
		if r := do(t, h, http.MethodPost, "/api/v1/notification-policies", "admin", body, idem); r.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, r.Code, r.Body)
		}
	}
	if store.creates != 0 {
		t.Errorf("invalid bodies created %d policies", store.creates)
	}
	// 목적지를 정하는 쓰기는 Tenant Admin(policies.write)만. Developer는 고르기(조회)만 한다. Viewer는 조회도 못 한다(monitors.read 없음)
	if r := do(t, h, http.MethodPost, "/api/v1/notification-policies", "dev", policyBody, idem); r.Code != http.StatusForbidden {
		t.Errorf("developer create: %d", r.Code)
	}
	if r := do(t, h, http.MethodGet, "/api/v1/notification-policies", "dev", "", nil); r.Code != http.StatusOK {
		t.Errorf("developer list: %d", r.Code)
	}
	if r := do(t, h, http.MethodGet, "/api/v1/notification-policies", "viewer", "", nil); r.Code != http.StatusForbidden {
		t.Errorf("viewer list: %d", r.Code)
	}
	if r := do(t, h, http.MethodPost, "/api/v1/notification-policies", "admin", policyBody, nil); r.Code != http.StatusBadRequest {
		t.Errorf("missing Idempotency-Key: %d", r.Code)
	}
}

func TestPolicyUpdateDelete(t *testing.T) {
	store := newFakePolicies()
	h := policyHandler(t, store, testSealer(t), principals{"admin": user(t, authz.RoleTenantAdmin)})
	env := decodePolicy(t, do(t, h, http.MethodPost, "/api/v1/notification-policies", "admin", policyBody, map[string]string{"Idempotency-Key": "k"}).Body.Bytes())
	path := "/api/v1/notification-policies/" + env.Data.ID
	update := `{"name":"Renamed","webhook":{"url":"https://other.example.com/hook"},"enabled":false}`
	if r := do(t, h, http.MethodPut, path, "admin", update, nil); r.Code != http.StatusPreconditionRequired {
		t.Errorf("no If-Match: %d", r.Code)
	}
	if r := do(t, h, http.MethodPut, path, "admin", update, map[string]string{"If-Match": `"9"`}); r.Code != http.StatusPreconditionFailed {
		t.Errorf("stale If-Match: %d", r.Code)
	}
	r := do(t, h, http.MethodPut, path, "admin", update, map[string]string{"If-Match": `"1"`})
	u := decodePolicy(t, r.Body.Bytes())
	if r.Code != http.StatusOK || u.Data.Revision != 2 || u.Data.Enabled || u.Data.Webhook.URL != "https://other.example.com/…" || u.SigningSecret != nil ||
		u.Data.Webhook.KeyID != env.Data.Webhook.KeyID {
		t.Errorf("update: %d %s", r.Code, r.Body)
	}
	// monitor가 가리키는 policy는 지울 수 없다(409) — 지우면 그 monitor의 알림이 기록 없이 끊긴다
	store.inUse[env.Data.ID] = true
	if r := do(t, h, http.MethodDelete, path, "admin", "", nil); r.Code != http.StatusConflict {
		t.Errorf("delete in use: %d %s", r.Code, r.Body)
	}
	store.inUse[env.Data.ID] = false
	if r := do(t, h, http.MethodDelete, path, "admin", "", nil); r.Code != http.StatusNoContent {
		t.Errorf("delete: %d", r.Code)
	}
	if r := do(t, h, http.MethodGet, path, "admin", "", nil); r.Code != http.StatusNotFound {
		t.Errorf("get deleted: %d", r.Code)
	}
}

// master key가 없으면 policy API는 없다(404). 저장소만 있고 Sealer가 없어도 같다.
func TestPolicyAPIDisabled(t *testing.T) {
	h := policyHandler(t, newFakePolicies(), nil, principals{"admin": user(t, authz.RoleTenantAdmin)})
	if r := do(t, h, http.MethodPost, "/api/v1/notification-policies", "admin", policyBody, map[string]string{"Idempotency-Key": "k"}); r.Code != http.StatusNotFound {
		t.Errorf("disabled: %d", r.Code)
	}
}
