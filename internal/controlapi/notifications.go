package controlapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/httpapi"
	"github.com/polynomeer/montracer/internal/notify"
)

// notification policy API (ADR 0053): webhook 목적지와 서명 secret.
//   - 쓰기는 policies.write(Tenant Admin): 고객 데이터가 나가는 목적지를 정한다. 조회는 monitors.read(monitor에 붙일 policy 고르기).
//   - 서명 secret은 서버가 만들고 생성 응답에서 한 번만 준다. Idempotency-Key 재생 응답에는 secret이 없다(저장한 응답에 평문을 두지 않는다).
//   - URL은 응답·감사에서 scheme·host만 보인다(path·query에 receiver token이 흔하다).

// PolicyStore는 notification policy 저장소다 (controldb.NotificationPolicyStore).
type PolicyStore interface {
	CreatePolicy(ctx context.Context, p authz.Principal, w controldb.PolicyWrite, seal func(policyID string) (controldb.SealedSecret, error),
		idem controldb.IdempotencyRequest, requestID string, respond func(controldb.NotificationPolicy) (controldb.StoredResponse, error)) (controldb.StoredResponse, bool, error)
	GetPolicy(ctx context.Context, p authz.Principal, id string) (controldb.NotificationPolicy, error)
	ListPolicies(ctx context.Context, p authz.Principal, after *controldb.PolicyPosition, limit int) ([]controldb.NotificationPolicy, []controldb.PolicyPosition, bool, error)
	UpdatePolicy(ctx context.Context, p authz.Principal, id string, ifMatch int64, w controldb.PolicyWrite, requestID string) (controldb.NotificationPolicy, error)
	DeletePolicy(ctx context.Context, p authz.Principal, id string, ifMatch *int64, requestID string) (int64, error)
}

// WarnSigningSecretNotReplayed는 Idempotency-Key 재생 응답에 secret이 없다는 경고다.
const WarnSigningSecretNotReplayed = "signing_secret_not_replayed"

// PolicyItem은 응답의 policy다.
type PolicyItem struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Enabled   bool          `json:"enabled"`
	Revision  int64         `json:"revision"`
	Webhook   PolicyWebhook `json:"webhook"`
	CreatedAt string        `json:"created_at"`
	UpdatedAt string        `json:"updated_at"`
	CreatedBy string        `json:"created_by"`
	UpdatedBy string        `json:"updated_by"`
}

// PolicyWebhook은 목적지다. URL은 scheme·host만 보인다.
type PolicyWebhook struct {
	URL   string `json:"url"`
	KeyID string `json:"signing_key_id"`
}

type policyResponse struct {
	Data PolicyItem `json:"data"`
	// SigningSecret은 생성 응답에만 있다. 다시 볼 수 없다.
	SigningSecret *string      `json:"signing_secret,omitempty"`
	Meta          MutationMeta `json:"meta"`
}

type policiesResponse struct {
	Data       []PolicyItem `json:"data"`
	NextCursor *string      `json:"next_cursor"`
	Meta       Meta         `json:"meta"`
}

type policyRequest struct {
	Name    string `json:"name"`
	Webhook *struct {
		URL string `json:"url"`
	} `json:"webhook"`
	Enabled *bool `json:"enabled"`
}

const maxPolicyBody = 16 << 10

func (h *Handler) registerPolicies(b httpapi.Boundary) {
	h.mux.Handle("POST /api/v1/notification-policies", b.Handle(h.authenticated(h.createPolicy)))
	h.mux.Handle("GET /api/v1/notification-policies", b.Handle(h.authenticated(h.listPolicies)))
	h.mux.Handle("GET /api/v1/notification-policies/{id}", b.Handle(h.authenticated(h.getPolicy)))
	h.mux.Handle("PUT /api/v1/notification-policies/{id}", b.Handle(h.authenticated(h.updatePolicy)))
	h.mux.Handle("DELETE /api/v1/notification-policies/{id}", b.Handle(h.authenticated(h.deletePolicy)))
}

func policiesUnavailable() error {
	return apierr.New(apierr.NotFound, "notification policy API를 사용할 수 없습니다")
}

func (h *Handler) policiesEnabled() bool { return h.cfg.Policies != nil && h.cfg.Sealer != nil }

func policyItem(np controldb.NotificationPolicy) PolicyItem {
	return PolicyItem{
		ID: np.ID, Name: np.Name, Enabled: np.Enabled, Revision: np.Revision,
		Webhook:   PolicyWebhook{URL: np.WebhookURL, KeyID: np.SecretKeyID},
		CreatedAt: np.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: np.UpdatedAt.Format(time.RFC3339Nano),
		CreatedBy: np.CreatedBy, UpdatedBy: np.UpdatedBy,
	}
}

// readPolicy는 본문을 읽고 검증한다. 모르는 field·형식 오류는 400이다. 검증한 URL은 따로 돌려준다(저장은 암호문으로).
func readPolicy(r *http.Request) ([]byte, controldb.PolicyWrite, string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPolicyBody+1))
	if err != nil {
		return nil, controldb.PolicyWrite{}, "", apierr.NewInvalidArgument("요청 본문을 읽을 수 없습니다", apierr.FieldViolation{Field: "body", Reason: "unreadable"})
	}
	if len(body) > maxPolicyBody {
		e := apierr.NewInvalidArgument("요청 본문이 너무 큽니다", apierr.FieldViolation{Field: "body", Reason: "at most 16384 bytes"})
		e.Status = http.StatusRequestEntityTooLarge
		return nil, controldb.PolicyWrite{}, "", e
	}
	var req policyRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		return nil, controldb.PolicyWrite{}, "", apierr.NewInvalidArgument("요청 본문이 올바르지 않습니다", apierr.FieldViolation{Field: "body", Reason: "must be a JSON object with name, webhook.url, enabled"})
	}
	var vs []apierr.FieldViolation
	name := strings.TrimSpace(req.Name)
	if name == "" || len([]rune(name)) > 200 {
		vs = append(vs, apierr.FieldViolation{Field: "name", Reason: "required, 1..200 characters"})
	}
	w := controldb.PolicyWrite{Name: name, Enabled: req.Enabled == nil || *req.Enabled}
	if req.Webhook == nil {
		vs = append(vs, apierr.FieldViolation{Field: "webhook.url", Reason: "required"})
	} else if err := notify.ValidateURL(req.Webhook.URL); err != nil {
		vs = append(vs, apierr.FieldViolation{Field: "webhook.url", Reason: err.Error()})
	} else {
		w.Display = notify.RedactURL(req.Webhook.URL)
	}
	if len(vs) > 0 {
		return nil, controldb.PolicyWrite{}, "", apierr.NewInvalidArgument("notification policy가 올바르지 않습니다", vs...)
	}
	return body, w, req.Webhook.URL, nil
}

// sealURL은 검증한 URL을 policy id에 묶어 암호화하는 함수다(저장소가 id를 정한 뒤 부른다).
func (h *Handler) sealURL(p authz.Principal, rawURL string) func(string) (controldb.Sealed, error) {
	return func(policyID string) (controldb.Sealed, error) {
		c, err := h.cfg.Sealer.SealURL(p.Tenant().String(), policyID, rawURL)
		return controldb.Sealed{Ciphertext: c, Version: h.cfg.Sealer.Version()}, err
	}
}

// createPolicy는 POST /api/v1/notification-policies다. 201 응답에 서명 secret을 한 번만 싣는다.
func (h *Handler) createPolicy(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if !h.policiesEnabled() {
		return policiesUnavailable()
	}
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return err
	}
	key := r.Header.Get("Idempotency-Key")
	if !idempotencyKeyPattern.MatchString(key) {
		return apierr.NewInvalidArgument("Idempotency-Key가 필요합니다", apierr.FieldViolation{Field: "Idempotency-Key", Reason: "required header, 1..255 visible ASCII characters"})
	}
	body, pw, rawURL, err := readPolicy(r)
	if err != nil {
		return err
	}
	pw.SealURL = h.sealURL(p, rawURL)
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
	idem := controldb.IdempotencyRequest{Key: key, MethodPath: "POST /api/v1/notification-policies", RequestHash: sha256.Sum256(body)}
	var created *controldb.NotificationPolicy
	stored, replayed, err := h.cfg.Policies.CreatePolicy(ctx, p, pw, seal, idem, requestID, func(np controldb.NotificationPolicy) (controldb.StoredResponse, error) {
		created = &np
		// 저장하는 응답(재생용)에는 secret이 없다
		b, err := json.Marshal(policyResponse{Data: policyItem(np), Meta: MutationMeta{RequestID: requestID, SchemaVersion: SchemaVersion, Warnings: []string{WarnSigningSecretNotReplayed}}})
		return controldb.StoredResponse{Status: http.StatusCreated, Body: b}, err
	})
	if errors.Is(err, controldb.ErrIdempotencyConflict) {
		return apierr.New(apierr.Conflict, "같은 Idempotency-Key를 다른 요청에 쓸 수 없습니다")
	}
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	if replayed || created == nil {
		var item struct {
			Data PolicyItem `json:"data"`
		}
		if json.Unmarshal(stored.Body, &item) == nil && item.Data.ID != "" {
			w.Header().Set("ETag", etag(item.Data.Revision))
			w.Header().Set("Location", "/api/v1/notification-policies/"+item.Data.ID)
		}
		w.Header().Set("Idempotent-Replayed", "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stored.Status)
		_, err := w.Write(stored.Body)
		return err
	}
	plain := secret.Plain()
	w.Header().Set("ETag", etag(created.Revision))
	w.Header().Set("Location", "/api/v1/notification-policies/"+created.ID)
	return writeJSON(w, http.StatusCreated, policyResponse{Data: policyItem(*created), SigningSecret: &plain,
		Meta: MutationMeta{RequestID: requestID, SchemaVersion: SchemaVersion, Warnings: []string{}}})
}

func policyID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if !monitorIDPattern.MatchString(id) {
		return "", apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "id", Reason: "must be a lowercase UUID"})
	}
	return id, nil
}

func policyMissing(err error) error {
	if errors.Is(err, authz.ErrNotFound) {
		return apierr.New(apierr.NotFound, "notification policy를 찾을 수 없습니다")
	}
	return err
}

// listPolicies는 GET /api/v1/notification-policies?limit&cursor다(이름순).
func (h *Handler) listPolicies(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if !h.policiesEnabled() {
		return policiesUnavailable()
	}
	q := r.URL.Query()
	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > controldb.MaxPolicyPage {
			return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "limit", Reason: "must be within [1, 200]"})
		}
		limit = n
	}
	binding := apicursor.Binding{
		Tenant:      p.Tenant().String(),
		Fingerprint: apicursor.Fingerprint(p.Kind().String(), p.Subject(), string(authz.MonitorsRead)),
		QueryHash:   apicursor.Fingerprint("notification-policies"),
	}
	var after *controldb.PolicyPosition
	if c := q.Get("cursor"); c != "" {
		claims, err := h.cfg.Cursor.Decode(c, binding)
		var pos monitorPosition
		if err != nil || json.Unmarshal(claims.Position, &pos) != nil || pos.ID == "" {
			return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "cursor", Reason: "invalid, expired, or not for this request"})
		}
		after = &controldb.PolicyPosition{NameNormalized: pos.Name, ID: pos.ID}
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	items, positions, more, err := h.cfg.Policies.ListPolicies(ctx, p, after, limit)
	if err != nil {
		return err
	}
	resp := policiesResponse{Data: make([]PolicyItem, 0, len(items)), Meta: Meta{RequestID: httpapi.RequestIDFrom(r.Context()), SchemaVersion: SchemaVersion}}
	for _, np := range items {
		resp.Data = append(resp.Data, policyItem(np))
	}
	if more && len(positions) > 0 {
		last := positions[len(positions)-1]
		tok, err := h.cfg.Cursor.Encode(binding, h.cfg.Now(), monitorPosition{Name: last.NameNormalized, ID: last.ID})
		if err != nil {
			return err
		}
		resp.NextCursor = &tok
	}
	return writeJSON(w, http.StatusOK, resp)
}

// getPolicy는 GET /api/v1/notification-policies/{id}다. 없는·다른 tenant·삭제된 policy는 같은 404다.
func (h *Handler) getPolicy(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if !h.policiesEnabled() {
		return policiesUnavailable()
	}
	id, err := policyID(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	np, err := h.cfg.Policies.GetPolicy(ctx, p, id)
	if err != nil {
		return policyMissing(err)
	}
	w.Header().Set("ETag", etag(np.Revision))
	return writeJSON(w, http.StatusOK, policyResponse{Data: policyItem(np), Meta: MutationMeta{RequestID: httpapi.RequestIDFrom(r.Context()), SchemaVersion: SchemaVersion, Warnings: []string{}}})
}

// updatePolicy는 PUT /api/v1/notification-policies/{id}다. If-Match 필수(없으면 428, 다르면 412). secret은 바뀌지 않는다.
func (h *Handler) updatePolicy(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if !h.policiesEnabled() {
		return policiesUnavailable()
	}
	id, err := policyID(r)
	if err != nil {
		return err
	}
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return err
	}
	rev, present, err := parseIfMatch(r)
	if err != nil {
		return err
	}
	if !present {
		e := apierr.New(apierr.RevisionMismatch, "If-Match revision이 필요합니다")
		e.Status = http.StatusPreconditionRequired
		return e
	}
	_, pw, rawURL, err := readPolicy(r)
	if err != nil {
		return err
	}
	pw.SealURL = h.sealURL(p, rawURL)
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	requestID := httpapi.RequestIDFrom(r.Context())
	np, err := h.cfg.Policies.UpdatePolicy(ctx, p, id, rev, pw, requestID)
	if errors.Is(err, controldb.ErrRevisionMismatch) {
		return apierr.New(apierr.RevisionMismatch, "다른 변경이 먼저 저장되었습니다. 다시 불러와 비교한 뒤 저장하세요")
	}
	if err != nil {
		return policyMissing(err)
	}
	w.Header().Set("ETag", etag(np.Revision))
	return writeJSON(w, http.StatusOK, policyResponse{Data: policyItem(np), Meta: MutationMeta{RequestID: requestID, SchemaVersion: SchemaVersion, Warnings: []string{}}})
}

// deletePolicy는 DELETE /api/v1/notification-policies/{id}다(tombstone, 204). 보내지 않은 알림은 취소된다.
func (h *Handler) deletePolicy(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
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
	var ifMatch *int64
	if present {
		ifMatch = &rev
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	_, err = h.cfg.Policies.DeletePolicy(ctx, p, id, ifMatch, httpapi.RequestIDFrom(r.Context()))
	if errors.Is(err, controldb.ErrPolicyInUse) {
		return apierr.New(apierr.Conflict, "이 notification policy를 쓰는 monitor가 있습니다. monitor에서 먼저 떼어 내세요")
	}
	if errors.Is(err, controldb.ErrRevisionMismatch) {
		return apierr.New(apierr.RevisionMismatch, "다른 변경이 먼저 저장되었습니다")
	}
	if err != nil {
		return policyMissing(err)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
	return nil
}
