package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/httpapi"
	"github.com/polynomeer/montracer/internal/notify"
)

// notification policy 시험 전송과 서명 secret 교체 (D05 §09, D02 §20, ADR 0055). 둘 다 policies.write(Tenant Admin)다.
//   - POST /{id}/test: 시험 알림을 쌓고 202. 보내기는 alert-worker가 한다(SSRF 방어·secret 복호화를 한 곳에). 결과는 GET /{id}/tests/{test_id}.
//     진행 중인 시험이 있으면 그것을 돌려준다(경고 test_in_progress). 1시간 20번을 넘으면 429.
//   - POST /{id}/rotate-secret: If-Match 필수. 새 secret을 응답에서 한 번만 준다. 이전 secret은 previous_secret_expires_in_seconds
//     (기본·최대 86400, 0이면 바로 폐기) 동안 함께 서명한다.

// WarnTestInProgress는 진행 중인 시험을 돌려줬다는 경고다(새로 보내지 않았다).
const WarnTestInProgress = "test_in_progress"

// TestItem은 시험 전송 하나다.
type TestItem struct {
	ID             string  `json:"id"`
	PolicyID       string  `json:"policy_id"`
	Status         string  `json:"status"` // pending | delivered | failed | canceled
	Attempts       int     `json:"attempts"`
	LastStatusCode *int    `json:"last_status_code"`
	LastError      *string `json:"last_error"`
	CreatedAt      string  `json:"created_at"`
	FinishedAt     *string `json:"finished_at"`
}

type testResponse struct {
	Data TestItem     `json:"data"`
	Meta MutationMeta `json:"meta"`
}

func testItem(t controldb.TestDelivery) TestItem {
	out := TestItem{ID: t.ID, PolicyID: t.PolicyID, Status: t.Status, Attempts: t.Attempts, LastStatusCode: t.LastStatusCode, LastError: t.LastError,
		CreatedAt: t.CreatedAt.Format(time.RFC3339Nano)}
	if t.FinishedAt != nil {
		f := t.FinishedAt.Format(time.RFC3339Nano)
		out.FinishedAt = &f
	}
	return out
}

var testIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// sendTest는 POST /api/v1/notification-policies/{id}/test다.
func (h *Handler) sendTest(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if !h.policiesEnabled() {
		return policiesUnavailable()
	}
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return err
	}
	id, err := policyID(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	requestID := httpapi.RequestIDFrom(r.Context())
	t, reused, err := h.cfg.Policies.SendTest(ctx, p, id, requestID)
	if errors.Is(err, controldb.ErrTestRateLimited) {
		return apierr.NewRateLimited("시험 전송은 policy마다 1시간에 20번까지입니다", 300)
	}
	if err != nil {
		return policyMissing(err)
	}
	warnings := []string{}
	if reused {
		warnings = append(warnings, WarnTestInProgress)
	}
	w.Header().Set("Location", "/api/v1/notification-policies/"+id+"/tests/"+t.ID)
	return writeJSON(w, http.StatusAccepted, testResponse{Data: testItem(t), Meta: MutationMeta{RequestID: requestID, SchemaVersion: SchemaVersion, Warnings: warnings}})
}

// getTest는 GET /api/v1/notification-policies/{id}/tests/{test_id}다. 다른 policy·다른 tenant·경보 알림 id는 같은 404다.
func (h *Handler) getTest(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if !h.policiesEnabled() {
		return policiesUnavailable()
	}
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return err
	}
	id, err := policyID(r)
	if err != nil {
		return err
	}
	testID := r.PathValue("test_id")
	if !testIDPattern.MatchString(testID) {
		return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "test_id", Reason: "must be a lowercase UUID"})
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	t, err := h.cfg.Policies.GetTest(ctx, p, id, testID)
	if errors.Is(err, authz.ErrNotFound) {
		return apierr.New(apierr.NotFound, "시험 전송을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, testResponse{Data: testItem(t), Meta: MutationMeta{RequestID: httpapi.RequestIDFrom(r.Context()), SchemaVersion: SchemaVersion, Warnings: []string{}}})
}

type rotateRequest struct {
	PreviousExpiresInSeconds *int `json:"previous_secret_expires_in_seconds"`
}

// rotateSecret은 POST /api/v1/notification-policies/{id}/rotate-secret이다. If-Match 필수(없으면 428, 다르면 412).
// 두 번 눌러도 두 번 바뀌지 않는다(두 번째는 412). 새 secret은 이 응답에서만 볼 수 있다(재생 저장 없음, no-store).
func (h *Handler) rotateSecret(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if !h.policiesEnabled() {
		return policiesUnavailable()
	}
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return err
	}
	id, err := policyID(r)
	if err != nil {
		return err
	}
	rev, present, err := parseIfMatch(r)
	if err != nil {
		return err
	}
	if !present {
		return ifMatchRequired()
	}
	overlap := controldb.MaxSecretOverlap
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPolicyBody+1))
	if err != nil {
		return apierr.NewInvalidArgument("요청 본문을 읽을 수 없습니다", apierr.FieldViolation{Field: "body", Reason: "unreadable"})
	}
	if len(body) > maxPolicyBody {
		// 다른 쓰기 경로와 같은 413 (D02 §19, ADR 0060 §4)
		e := apierr.NewInvalidArgument("요청 본문이 너무 큽니다", apierr.FieldViolation{Field: "body", Reason: "at most 16384 bytes"})
		e.Status = http.StatusRequestEntityTooLarge
		return e
	}
	if len(bytes.TrimSpace(body)) > 0 {
		var req rotateRequest
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || dec.More() {
			return apierr.NewInvalidArgument("요청 본문이 올바르지 않습니다", apierr.FieldViolation{Field: "body", Reason: "must be empty or {\"previous_secret_expires_in_seconds\": N}"})
		}
		if req.PreviousExpiresInSeconds != nil {
			n := *req.PreviousExpiresInSeconds
			if n < 0 || time.Duration(n)*time.Second > controldb.MaxSecretOverlap {
				return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "previous_secret_expires_in_seconds", Reason: "0..86400"})
			}
			overlap = time.Duration(n) * time.Second
		}
	}
	secret, err := notify.NewSecret()
	if err != nil {
		return err
	}
	seal := func(policyID string) (controldb.SealedSecret, error) {
		sealed, err := h.cfg.Sealer.Seal(p.Tenant().String(), policyID, secret)
		return controldb.SealedSecret{KeyID: secret.KeyID, Ciphertext: sealed, SealingKey: h.cfg.Sealer.Version()}, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	requestID := httpapi.RequestIDFrom(r.Context())
	np, err := h.cfg.Policies.RotateSecret(ctx, p, id, rev, overlap, seal, requestID)
	if errors.Is(err, controldb.ErrRevisionMismatch) {
		return apierr.New(apierr.RevisionMismatch, "다른 변경이 먼저 저장되었습니다. 다시 불러온 뒤 교체하세요")
	}
	if err != nil {
		return policyMissing(err)
	}
	plain := secret.Plain()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("ETag", etag(np.Revision))
	return writeJSON(w, http.StatusOK, policyResponse{Data: policyItem(np), SigningSecret: &plain,
		Meta: MutationMeta{RequestID: requestID, SchemaVersion: SchemaVersion, Warnings: []string{}}})
}
