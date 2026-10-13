package controlapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

func createdPolicy(t *testing.T, h http.Handler) policyEnvelope {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/api/v1/notification-policies", "admin", policyBody, map[string]string{"Idempotency-Key": "k"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	return decodePolicy(t, rec.Body.Bytes())
}

// 시험 전송: Tenant Admin만, 202와 상태 주소. 진행 중이면 같은 시험을 돌려주고(경고), 한도를 넘으면 429 + Retry-After.
// 결과 조회는 같은 policy의 시험만(다른 policy 경로·형식 오류는 404·400).
func TestPolicyTestSend(t *testing.T) {
	store := newFakePolicies()
	h := policyHandler(t, store, testSealer(t), principals{"admin": user(t, authz.RoleTenantAdmin), "dev": user(t, authz.RoleDeveloper)})
	env := createdPolicy(t, h)
	path := "/api/v1/notification-policies/" + env.Data.ID + "/test"
	if r := do(t, h, http.MethodPost, path, "dev", "", nil); r.Code != http.StatusForbidden {
		t.Errorf("developer test send: %d", r.Code)
	}
	rec := do(t, h, http.MethodPost, path, "admin", "", nil)
	var first struct {
		Data TestItem     `json:"data"`
		Meta MutationMeta `json:"meta"`
	}
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &first) != nil || first.Data.Status != "pending" || len(first.Meta.Warnings) != 0 {
		t.Fatalf("test send: %d %s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); loc != "/api/v1/notification-policies/"+env.Data.ID+"/tests/"+first.Data.ID {
		t.Errorf("Location = %q", loc)
	}
	again := do(t, h, http.MethodPost, path, "admin", "", nil)
	if again.Code != http.StatusAccepted || !strings.Contains(again.Body.String(), first.Data.ID) || !strings.Contains(again.Body.String(), WarnTestInProgress) {
		t.Errorf("second send while pending: %d %s", again.Code, again.Body)
	}
	got := do(t, h, http.MethodGet, "/api/v1/notification-policies/"+env.Data.ID+"/tests/"+first.Data.ID, "admin", "", nil)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"last_status_code":null`) {
		t.Errorf("get test: %d %s", got.Code, got.Body)
	}
	if r := do(t, h, http.MethodGet, "/api/v1/notification-policies/8d49d404-4115-42f8-9c59-f94ff40f9999/tests/"+first.Data.ID, "admin", "", nil); r.Code != http.StatusNotFound {
		t.Errorf("test under other policy: %d", r.Code)
	}
	if r := do(t, h, http.MethodGet, "/api/v1/notification-policies/"+env.Data.ID+"/tests/NOT-A-UUID", "admin", "", nil); r.Code != http.StatusBadRequest {
		t.Errorf("malformed test id: %d", r.Code)
	}
	// 진행 중인 시험을 끝내고 한도까지 채운다
	for id, td := range store.tests {
		td.Status = "delivered"
		store.tests[id] = td
	}
	for len(store.tests) < controldb.MaxTestsPerHour {
		store.tests[fmt.Sprintf("old-%d", len(store.tests))] = controldb.TestDelivery{PolicyID: env.Data.ID, Status: "failed"}
	}
	limited := do(t, h, http.MethodPost, path, "admin", "", nil)
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Errorf("rate limit: %d %v", limited.Code, limited.Header())
	}
}

// secret 교체: If-Match 필수(428)·낡으면 412. 새 secret은 이 응답에서만, 이전 key id와 만료가 보인다(기본 24시간).
// 0이면 이전 secret을 바로 버린다. 범위 밖·모르는 field는 400.
func TestPolicyRotateSecret(t *testing.T) {
	store := newFakePolicies()
	h := policyHandler(t, store, testSealer(t), principals{"admin": user(t, authz.RoleTenantAdmin), "dev": user(t, authz.RoleDeveloper)})
	env := createdPolicy(t, h)
	path := "/api/v1/notification-policies/" + env.Data.ID + "/rotate-secret"
	if r := do(t, h, http.MethodPost, path, "admin", "", nil); r.Code != http.StatusPreconditionRequired {
		t.Errorf("no If-Match: %d", r.Code)
	}
	if r := do(t, h, http.MethodPost, path, "dev", "", map[string]string{"If-Match": `"1"`}); r.Code != http.StatusForbidden {
		t.Errorf("developer rotate: %d", r.Code)
	}
	for _, body := range []string{`{"previous_secret_expires_in_seconds":86401}`, `{"previous_secret_expires_in_seconds":-1}`, `{"x":1}`} {
		if r := do(t, h, http.MethodPost, path, "admin", body, map[string]string{"If-Match": `"1"`}); r.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, r.Code)
		}
	}
	rec := do(t, h, http.MethodPost, path, "admin", "", map[string]string{"If-Match": `"1"`})
	rot := decodePolicy(t, rec.Body.Bytes())
	if rec.Code != http.StatusOK || rot.SigningSecret == nil || !strings.HasPrefix(*rot.SigningSecret, "whsec_") || rot.Data.Webhook.KeyID == env.Data.Webhook.KeyID ||
		rot.Data.Webhook.PreviousKeyID == nil || *rot.Data.Webhook.PreviousKeyID != env.Data.Webhook.KeyID || rot.Data.Webhook.PreviousKeyExpiresAt == nil ||
		store.overlap != 24*time.Hour || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("ETag") != `"2"` {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	if *rot.SigningSecret == *env.SigningSecret {
		t.Error("same secret after rotation")
	}
	if r := do(t, h, http.MethodPost, path, "admin", "", map[string]string{"If-Match": `"1"`}); r.Code != http.StatusPreconditionFailed {
		t.Errorf("stale If-Match: %d", r.Code)
	}
	now0 := do(t, h, http.MethodPost, path, "admin", `{"previous_secret_expires_in_seconds":0}`, map[string]string{"If-Match": `"2"`})
	if env := decodePolicy(t, now0.Body.Bytes()); now0.Code != http.StatusOK || env.Data.Webhook.PreviousKeyID != nil || store.overlap != 0 {
		t.Errorf("immediate rotate: %d %s", now0.Code, now0.Body)
	}
}
