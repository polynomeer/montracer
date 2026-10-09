package controlapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/httpapi"
	"github.com/polynomeer/montracer/internal/monitor"
)

// MonitorStore는 monitor 저장소다 (controldb.MonitorStore, ADR 0049).
type MonitorStore interface {
	CreateMonitor(ctx context.Context, p authz.Principal, w controldb.MonitorWrite, idem controldb.IdempotencyRequest, requestID string,
		respond func(controldb.Monitor) (controldb.StoredResponse, error)) (controldb.StoredResponse, bool, error)
	GetMonitor(ctx context.Context, p authz.Principal, id string) (controldb.Monitor, error)
	ListMonitors(ctx context.Context, p authz.Principal, after *controldb.MonitorPosition, limit int) ([]controldb.Monitor, []controldb.MonitorPosition, bool, error)
	UpdateMonitor(ctx context.Context, p authz.Principal, id string, ifMatch int64, w controldb.MonitorWrite, requestID string) (controldb.Monitor, error)
	DeleteMonitor(ctx context.Context, p authz.Principal, id string, ifMatch *int64, requestID string) (int64, error)
}

// MonitorItem은 응답의 monitor다. Spec은 정규형 MonitorSpec이다.
type MonitorItem struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Enabled   bool            `json:"enabled"`
	Revision  int64           `json:"revision"`
	Spec      json.RawMessage `json:"spec"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
	CreatedBy string          `json:"created_by"`
	UpdatedBy string          `json:"updated_by"`
}

// MutationMeta는 쓰기 응답 meta다. warnings는 저장은 됐지만 알릴 점이다(monitor.Warn*).
type MutationMeta struct {
	RequestID     string   `json:"request_id"`
	SchemaVersion int      `json:"schema_version"`
	Warnings      []string `json:"warnings"`
}

type monitorResponse struct {
	Data MonitorItem  `json:"data"`
	Meta MutationMeta `json:"meta"`
}

type monitorsResponse struct {
	Data       []MonitorItem `json:"data"`
	NextCursor *string       `json:"next_cursor"`
	Meta       Meta          `json:"meta"`
}

type validateResponse struct {
	Data struct {
		NormalizedSpec json.RawMessage `json:"normalized_spec"`
		Warnings       []string        `json:"warnings"`
		// DryRun은 24시간 dry-run 결과다(ADR 0052). 하지 못했으면 null이고 warnings에 이유(dry_run_unavailable·
		// dry_run_forbidden·dry_run_failed·dry_run_too_large)가 있다 — 결과가 "발화 없음"인 것처럼 보이지 않게 한다.
		DryRun json.RawMessage `json:"dry_run"`
	} `json:"data"`
	Meta Meta `json:"meta"`
}

type monitorPosition struct {
	Name string `json:"n"`
	ID   string `json:"i"`
}

var monitorIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// idempotencyKeyPattern: 1..255자의 보이는 ASCII (IETF Idempotency-Key header 초안의 문자열 값)
var idempotencyKeyPattern = regexp.MustCompile(`^[\x21-\x7e]{1,255}$`)

func (h *Handler) registerMonitors(b httpapi.Boundary) {
	h.mux.Handle("POST /api/v1/monitors/validate", b.Handle(h.authenticated(h.validateMonitor)))
	h.mux.Handle("POST /api/v1/monitors", b.Handle(h.authenticated(h.createMonitor)))
	h.mux.Handle("GET /api/v1/monitors", b.Handle(h.authenticated(h.listMonitors)))
	h.mux.Handle("GET /api/v1/monitors/{id}", b.Handle(h.authenticated(h.getMonitor)))
	h.mux.Handle("PUT /api/v1/monitors/{id}", b.Handle(h.authenticated(h.updateMonitor)))
	h.mux.Handle("DELETE /api/v1/monitors/{id}", b.Handle(h.authenticated(h.deleteMonitor)))
}

func monitorsUnavailable() error {
	return apierr.New(apierr.NotFound, "monitor API를 사용할 수 없습니다")
}

func monitorItem(m controldb.Monitor) MonitorItem {
	return MonitorItem{
		ID: m.ID, Name: m.Name, Enabled: m.Enabled, Revision: m.Revision, Spec: m.Spec,
		CreatedAt: m.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: m.UpdatedAt.Format(time.RFC3339Nano),
		CreatedBy: m.CreatedBy, UpdatedBy: m.UpdatedBy,
	}
}

func etag(revision int64) string { return `"` + strconv.FormatInt(revision, 10) + `"` }

// readSpec은 본문을 읽고 정규화한다. 형식 오류는 400, 아직 지원하지 않는 의미는 422다.
func readSpec(r *http.Request) ([]byte, monitor.Spec, []string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, monitor.MaxSpecBytes+1))
	if err != nil {
		return nil, monitor.Spec{}, nil, apierr.NewInvalidArgument("요청 본문을 읽을 수 없습니다", apierr.FieldViolation{Field: "body", Reason: "unreadable"})
	}
	if len(body) > monitor.MaxSpecBytes {
		// D02 §19: 413 = 본문 크기
		e := apierr.NewInvalidArgument("요청 본문이 너무 큽니다", apierr.FieldViolation{Field: "body", Reason: "at most 65536 bytes"})
		e.Status = http.StatusRequestEntityTooLarge
		return nil, monitor.Spec{}, nil, e
	}
	spec, warnings, err := monitor.Normalize(body)
	var ve *monitor.ValidationError
	if errors.As(err, &ve) {
		vs := make([]apierr.FieldViolation, len(ve.Violations))
		for i, v := range ve.Violations {
			vs[i] = apierr.FieldViolation{Field: v.Field, Reason: v.Reason}
		}
		e := apierr.NewInvalidArgument("monitor 정의가 올바르지 않습니다", vs...)
		if ve.Unsupported {
			e.Status = http.StatusUnprocessableEntity
		}
		return nil, monitor.Spec{}, nil, e
	}
	if err != nil {
		return nil, monitor.Spec{}, nil, err
	}
	return body, spec, warnings, nil
}

func writeWrite(spec monitor.Spec) controldb.MonitorWrite {
	return controldb.MonitorWrite{Name: spec.Name, Spec: spec.Canonical(), Enabled: spec.Enabled, EvaluationSeconds: spec.EvaluationSeconds}
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

// validateMonitor는 POST /api/v1/monitors/validate다 (D02 §20). 저장하지 않으므로 Idempotency-Key가 필요 없다.
func (h *Handler) validateMonitor(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Monitors == nil {
		return monitorsUnavailable()
	}
	if err := authz.AuthorizeTenantWide(p, authz.MonitorsWrite); err != nil {
		return err
	}
	_, spec, warnings, err := readSpec(r)
	if err != nil {
		return err
	}
	var resp validateResponse
	resp.Data.NormalizedSpec = spec.Canonical()
	resp.Data.DryRun = json.RawMessage("null")
	if out, warn := h.dryRun(r.Context(), p, spec); warn != "" {
		warnings = append(warnings, warn)
	} else {
		resp.Data.DryRun = out
	}
	resp.Data.Warnings = warnings
	resp.Meta = Meta{RequestID: httpapi.RequestIDFrom(r.Context()), SchemaVersion: SchemaVersion}
	return writeJSON(w, http.StatusOK, resp)
}

// createMonitor는 POST /api/v1/monitors다. Idempotency-Key가 필수다(D02 §20):
// 같은 key·같은 본문의 재시도는 처음 응답을 그대로, 다른 본문은 409다.
func (h *Handler) createMonitor(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Monitors == nil {
		return monitorsUnavailable()
	}
	// 권한을 먼저 본다(header·본문 검증 오류로 권한 없는 사용자에게 형식을 알려주지 않는다, 403 우선)
	if err := authz.AuthorizeTenantWide(p, authz.MonitorsWrite); err != nil {
		return err
	}
	key := r.Header.Get("Idempotency-Key")
	if !idempotencyKeyPattern.MatchString(key) {
		return apierr.NewInvalidArgument("Idempotency-Key가 필요합니다", apierr.FieldViolation{Field: "Idempotency-Key", Reason: "required header, 1..255 visible ASCII characters"})
	}
	body, spec, warnings, err := readSpec(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	requestID := httpapi.RequestIDFrom(r.Context())
	idem := controldb.IdempotencyRequest{Key: key, MethodPath: "POST /api/v1/monitors", RequestHash: sha256.Sum256(body)}
	stored, replayed, err := h.cfg.Monitors.CreateMonitor(ctx, p, writeWrite(spec), idem, requestID, func(m controldb.Monitor) (controldb.StoredResponse, error) {
		b, err := json.Marshal(monitorResponse{Data: monitorItem(m), Meta: MutationMeta{RequestID: requestID, SchemaVersion: SchemaVersion, Warnings: warnings}})
		return controldb.StoredResponse{Status: http.StatusCreated, Body: b}, err
	})
	if errors.Is(err, controldb.ErrIdempotencyConflict) {
		return apierr.New(apierr.Conflict, "같은 Idempotency-Key를 다른 요청에 쓸 수 없습니다")
	}
	if err != nil {
		return err
	}
	var item struct {
		Data MonitorItem `json:"data"`
	}
	if json.Unmarshal(stored.Body, &item) == nil && item.Data.ID != "" {
		w.Header().Set("ETag", etag(item.Data.Revision))
		w.Header().Set("Location", "/api/v1/monitors/"+item.Data.ID)
	}
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(stored.Status)
	_, err = w.Write(stored.Body)
	return err
}

// listMonitors는 GET /api/v1/monitors?limit&cursor다(이름순).
func (h *Handler) listMonitors(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Monitors == nil {
		return monitorsUnavailable()
	}
	q := r.URL.Query()
	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > controldb.MaxMonitorPage {
			return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "limit", Reason: "must be within [1, 200]"})
		}
		limit = n
	}
	binding := apicursor.Binding{
		Tenant:      p.Tenant().String(),
		Fingerprint: apicursor.Fingerprint(p.Kind().String(), p.Subject(), string(authz.MonitorsRead)),
		QueryHash:   apicursor.Fingerprint("monitors"),
	}
	var after *controldb.MonitorPosition
	if c := q.Get("cursor"); c != "" {
		claims, err := h.cfg.Cursor.Decode(c, binding)
		var pos monitorPosition
		if err != nil || json.Unmarshal(claims.Position, &pos) != nil || pos.ID == "" {
			return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "cursor", Reason: "invalid, expired, or not for this request"})
		}
		after = &controldb.MonitorPosition{NameNormalized: pos.Name, ID: pos.ID}
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	items, positions, more, err := h.cfg.Monitors.ListMonitors(ctx, p, after, limit)
	if err != nil {
		return err
	}
	resp := monitorsResponse{Data: make([]MonitorItem, 0, len(items)), Meta: Meta{RequestID: httpapi.RequestIDFrom(r.Context()), SchemaVersion: SchemaVersion}}
	for _, m := range items {
		resp.Data = append(resp.Data, monitorItem(m))
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

func monitorID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if !monitorIDPattern.MatchString(id) {
		return "", apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "id", Reason: "must be a lowercase UUID"})
	}
	return id, nil
}

func notFound(err error) error {
	if errors.Is(err, authz.ErrNotFound) {
		return apierr.New(apierr.NotFound, "monitor를 찾을 수 없습니다")
	}
	return err
}

// getMonitor는 GET /api/v1/monitors/{id}다. 없는 monitor, 다른 tenant, 삭제된 monitor는 같은 404다.
func (h *Handler) getMonitor(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Monitors == nil {
		return monitorsUnavailable()
	}
	id, err := monitorID(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	m, err := h.cfg.Monitors.GetMonitor(ctx, p, id)
	if err != nil {
		return notFound(err)
	}
	w.Header().Set("ETag", etag(m.Revision))
	return writeJSON(w, http.StatusOK, monitorResponse{Data: monitorItem(m), Meta: MutationMeta{RequestID: httpapi.RequestIDFrom(r.Context()), SchemaVersion: SchemaVersion, Warnings: []string{}}})
}

// parseIfMatch는 If-Match의 revision이다. `"3"`과 `3`을 받는다. 약한 ETag·`*`·여러 값은 받지 않는다.
func parseIfMatch(r *http.Request) (int64, bool, error) {
	v := strings.TrimSpace(r.Header.Get("If-Match"))
	if v == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(strings.Trim(v, `"`), 10, 64)
	if err != nil || n < 1 || strings.Count(v, `"`)%2 != 0 {
		return 0, true, apierr.NewInvalidArgument("If-Match가 올바르지 않습니다", apierr.FieldViolation{Field: "If-Match", Reason: `must be the revision ETag, e.g. "3"`})
	}
	return n, true, nil
}

// updateMonitor는 PUT /api/v1/monitors/{id}다. If-Match가 없으면 428, 현재 revision과 다르면 412다(D02 §20).
func (h *Handler) updateMonitor(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Monitors == nil {
		return monitorsUnavailable()
	}
	id, err := monitorID(r)
	if err != nil {
		return err
	}
	if err := authz.AuthorizeTenantWide(p, authz.MonitorsWrite); err != nil {
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
	_, spec, warnings, err := readSpec(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	requestID := httpapi.RequestIDFrom(r.Context())
	m, err := h.cfg.Monitors.UpdateMonitor(ctx, p, id, rev, writeWrite(spec), requestID)
	if errors.Is(err, controldb.ErrRevisionMismatch) {
		return apierr.New(apierr.RevisionMismatch, "다른 변경이 먼저 저장되었습니다. 다시 불러와 비교한 뒤 저장하세요")
	}
	if err != nil {
		return notFound(err)
	}
	w.Header().Set("ETag", etag(m.Revision))
	return writeJSON(w, http.StatusOK, monitorResponse{Data: monitorItem(m), Meta: MutationMeta{RequestID: requestID, SchemaVersion: SchemaVersion, Warnings: warnings}})
}

// deleteMonitor는 DELETE /api/v1/monitors/{id}다. tombstone을 남긴다(204). If-Match가 있으면 현재 revision이어야 한다.
func (h *Handler) deleteMonitor(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Monitors == nil {
		return monitorsUnavailable()
	}
	if err := authz.AuthorizeTenantWide(p, authz.MonitorsWrite); err != nil {
		return err
	}
	id, err := monitorID(r)
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
	_, err = h.cfg.Monitors.DeleteMonitor(ctx, p, id, ifMatch, httpapi.RequestIDFrom(r.Context()))
	if errors.Is(err, controldb.ErrRevisionMismatch) {
		return apierr.New(apierr.RevisionMismatch, "다른 변경이 먼저 저장되었습니다")
	}
	if err != nil {
		return notFound(err)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
	return nil
}
