package controlapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/dashboard"
	"github.com/polynomeer/montracer/internal/httpapi"
)

// dashboard API (D02 §14, D05 §10~11, ADR 0057): GET/POST /api/v1/dashboards, GET/PUT/DELETE /api/v1/dashboards/{id}.
//   - 조회 dashboards.read(Viewer 이상), 쓰기 dashboards.write(Developer 이상).
//   - 생성 Idempotency-Key 필수, 수정 If-Match 필수(없으면 428, 다르면 412), 삭제 tombstone(If-Match 선택).
//   - 형식 오류 400, 아직 지원하지 않는 의미(histogram 위젯·team 변수·or filter) 422. 최대 50 위젯.
//   - 목록은 요약(제목·위젯 수·revision)만, 단건·쓰기 응답은 정규형 spec을 싣는다.

// DashboardStore는 dashboard 저장소다 (controldb.DashboardStore).
type DashboardStore interface {
	CreateDashboard(ctx context.Context, p authz.Principal, w controldb.DashboardWrite, idem controldb.IdempotencyRequest, requestID string,
		respond func(controldb.Dashboard) (controldb.StoredResponse, error)) (controldb.StoredResponse, bool, error)
	GetDashboard(ctx context.Context, p authz.Principal, id string) (controldb.Dashboard, error)
	ListDashboards(ctx context.Context, p authz.Principal, after *controldb.DashboardPosition, limit int) ([]controldb.Dashboard, []controldb.DashboardPosition, bool, error)
	UpdateDashboard(ctx context.Context, p authz.Principal, id string, ifMatch int64, w controldb.DashboardWrite, requestID string) (controldb.Dashboard, error)
	DeleteDashboard(ctx context.Context, p authz.Principal, id string, ifMatch *int64, requestID string) (int64, error)
}

// DashboardItem은 응답의 dashboard다. Spec은 단건·쓰기 응답에만 있다(목록은 요약).
type DashboardItem struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Revision    int64           `json:"revision"`
	WidgetCount int             `json:"widget_count"`
	Spec        json.RawMessage `json:"spec,omitempty"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	CreatedBy   string          `json:"created_by"`
	UpdatedBy   string          `json:"updated_by"`
}

type dashboardResponse struct {
	Data DashboardItem `json:"data"`
	Meta MutationMeta  `json:"meta"`
}

type dashboardsResponse struct {
	Data       []DashboardItem `json:"data"`
	NextCursor *string         `json:"next_cursor"`
	Meta       Meta            `json:"meta"`
}

type dashboardPosition struct {
	Title string `json:"t"`
	ID    string `json:"i"`
}

func (h *Handler) registerDashboards(b httpapi.Boundary) {
	h.mux.Handle("POST /api/v1/dashboards", b.Handle(h.authenticated(h.createDashboard)))
	h.mux.Handle("GET /api/v1/dashboards", b.Handle(h.authenticated(h.listDashboards)))
	h.mux.Handle("GET /api/v1/dashboards/{id}", b.Handle(h.authenticated(h.getDashboard)))
	h.mux.Handle("PUT /api/v1/dashboards/{id}", b.Handle(h.authenticated(h.updateDashboard)))
	h.mux.Handle("DELETE /api/v1/dashboards/{id}", b.Handle(h.authenticated(h.deleteDashboard)))
}

func dashboardsUnavailable() error {
	return apierr.New(apierr.NotFound, "dashboard API를 사용할 수 없습니다")
}

func dashboardItem(d controldb.Dashboard, withSpec bool) DashboardItem {
	item := DashboardItem{
		ID: d.ID, Title: d.Title, Revision: d.Revision, WidgetCount: d.WidgetCount,
		CreatedAt: d.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: d.UpdatedAt.Format(time.RFC3339Nano),
		CreatedBy: d.CreatedBy, UpdatedBy: d.UpdatedBy,
	}
	if withSpec {
		item.Spec = d.Spec
	}
	return item
}

// readDashboard는 본문을 읽고 정규화한다. 형식 오류는 400, 아직 지원하지 않는 의미뿐이면 422, 너무 크면 413이다.
func readDashboard(r *http.Request) ([]byte, controldb.DashboardWrite, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, dashboard.MaxSpecBytes+1))
	if err != nil {
		return nil, controldb.DashboardWrite{}, apierr.NewInvalidArgument("요청 본문을 읽을 수 없습니다", apierr.FieldViolation{Field: "body", Reason: "unreadable"})
	}
	if len(body) > dashboard.MaxSpecBytes {
		e := apierr.NewInvalidArgument("요청 본문이 너무 큽니다", apierr.FieldViolation{Field: "body", Reason: "at most 262144 bytes"})
		e.Status = http.StatusRequestEntityTooLarge
		return nil, controldb.DashboardWrite{}, e
	}
	spec, err := dashboard.Normalize(body)
	var ve *dashboard.ValidationError
	if errors.As(err, &ve) {
		vs := make([]apierr.FieldViolation, len(ve.Violations))
		for i, v := range ve.Violations {
			vs[i] = apierr.FieldViolation{Field: v.Field, Reason: v.Reason}
		}
		e := apierr.NewInvalidArgument("dashboard 정의가 올바르지 않습니다", vs...)
		if ve.Unsupported {
			e.Status = http.StatusUnprocessableEntity
		}
		return nil, controldb.DashboardWrite{}, e
	}
	if err != nil {
		return nil, controldb.DashboardWrite{}, err
	}
	return body, controldb.DashboardWrite{Title: spec.Title, Spec: spec.Canonical(), WidgetCount: len(spec.Widgets)}, nil
}

func dashboardID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if !monitorIDPattern.MatchString(id) {
		return "", apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "id", Reason: "must be a lowercase UUID"})
	}
	return id, nil
}

func dashboardMissing(err error) error {
	if errors.Is(err, authz.ErrNotFound) {
		return apierr.New(apierr.NotFound, "dashboard를 찾을 수 없습니다")
	}
	return err
}

// createDashboard는 POST /api/v1/dashboards다. 같은 Idempotency-Key·같은 본문은 처음 응답, 다른 본문은 409다.
func (h *Handler) createDashboard(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Dashboards == nil {
		return dashboardsUnavailable()
	}
	if err := authz.Authorize(p, authz.DashboardsWrite); err != nil {
		return err
	}
	key := r.Header.Get("Idempotency-Key")
	if !idempotencyKeyPattern.MatchString(key) {
		return apierr.NewInvalidArgument("Idempotency-Key가 필요합니다", apierr.FieldViolation{Field: "Idempotency-Key", Reason: "required header, 1..255 visible ASCII characters"})
	}
	body, dw, err := readDashboard(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	requestID := httpapi.RequestIDFrom(r.Context())
	idem := controldb.IdempotencyRequest{Key: key, MethodPath: "POST /api/v1/dashboards", RequestHash: sha256.Sum256(body)}
	stored, replayed, err := h.cfg.Dashboards.CreateDashboard(ctx, p, dw, idem, requestID, func(d controldb.Dashboard) (controldb.StoredResponse, error) {
		b, err := json.Marshal(dashboardResponse{Data: dashboardItem(d, true), Meta: MutationMeta{RequestID: requestID, SchemaVersion: SchemaVersion, Warnings: []string{}}})
		return controldb.StoredResponse{Status: http.StatusCreated, Body: b}, err
	})
	if errors.Is(err, controldb.ErrIdempotencyConflict) {
		return apierr.New(apierr.Conflict, "같은 Idempotency-Key를 다른 요청에 쓸 수 없습니다")
	}
	if err != nil {
		return err
	}
	var item struct {
		Data DashboardItem `json:"data"`
	}
	if json.Unmarshal(stored.Body, &item) == nil && item.Data.ID != "" {
		w.Header().Set("ETag", etag(item.Data.Revision))
		w.Header().Set("Location", "/api/v1/dashboards/"+item.Data.ID)
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

// listDashboards는 GET /api/v1/dashboards?limit&cursor다(제목순, 요약만).
func (h *Handler) listDashboards(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Dashboards == nil {
		return dashboardsUnavailable()
	}
	q := r.URL.Query()
	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > controldb.MaxDashboardPage {
			return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "limit", Reason: "must be within [1, 200]"})
		}
		limit = n
	}
	binding := apicursor.Binding{
		Tenant:      p.Tenant().String(),
		Fingerprint: apicursor.Fingerprint(p.Kind().String(), p.Subject(), string(authz.DashboardsRead)),
		QueryHash:   apicursor.Fingerprint("dashboards"),
	}
	var after *controldb.DashboardPosition
	if c := q.Get("cursor"); c != "" {
		claims, err := h.cfg.Cursor.Decode(c, binding)
		var pos dashboardPosition
		if err != nil || json.Unmarshal(claims.Position, &pos) != nil || pos.ID == "" {
			return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "cursor", Reason: "invalid, expired, or not for this request"})
		}
		after = &controldb.DashboardPosition{TitleNormalized: pos.Title, ID: pos.ID}
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	items, positions, more, err := h.cfg.Dashboards.ListDashboards(ctx, p, after, limit)
	if err != nil {
		return err
	}
	resp := dashboardsResponse{Data: make([]DashboardItem, 0, len(items)), Meta: Meta{RequestID: httpapi.RequestIDFrom(r.Context()), SchemaVersion: SchemaVersion}}
	for _, d := range items {
		resp.Data = append(resp.Data, dashboardItem(d, false))
	}
	if more && len(positions) > 0 {
		last := positions[len(positions)-1]
		tok, err := h.cfg.Cursor.Encode(binding, h.cfg.Now(), dashboardPosition{Title: last.TitleNormalized, ID: last.ID})
		if err != nil {
			return err
		}
		resp.NextCursor = &tok
	}
	return writeJSON(w, http.StatusOK, resp)
}

// getDashboard는 GET /api/v1/dashboards/{id}다. 없는·다른 tenant·삭제된 dashboard는 같은 404다.
func (h *Handler) getDashboard(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Dashboards == nil {
		return dashboardsUnavailable()
	}
	id, err := dashboardID(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	d, err := h.cfg.Dashboards.GetDashboard(ctx, p, id)
	if err != nil {
		return dashboardMissing(err)
	}
	w.Header().Set("ETag", etag(d.Revision))
	return writeJSON(w, http.StatusOK, dashboardResponse{Data: dashboardItem(d, true), Meta: MutationMeta{RequestID: httpapi.RequestIDFrom(r.Context()), SchemaVersion: SchemaVersion, Warnings: []string{}}})
}

// updateDashboard는 PUT /api/v1/dashboards/{id}다. If-Match가 없으면 428, 현재 revision과 다르면 412다(D02 §11·§14).
func (h *Handler) updateDashboard(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Dashboards == nil {
		return dashboardsUnavailable()
	}
	id, err := dashboardID(r)
	if err != nil {
		return err
	}
	if err := authz.Authorize(p, authz.DashboardsWrite); err != nil {
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
	_, dw, err := readDashboard(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	requestID := httpapi.RequestIDFrom(r.Context())
	d, err := h.cfg.Dashboards.UpdateDashboard(ctx, p, id, rev, dw, requestID)
	if errors.Is(err, controldb.ErrRevisionMismatch) {
		return apierr.New(apierr.RevisionMismatch, "다른 변경이 먼저 저장되었습니다. 다시 불러와 비교한 뒤 저장하세요")
	}
	if err != nil {
		return dashboardMissing(err)
	}
	w.Header().Set("ETag", etag(d.Revision))
	return writeJSON(w, http.StatusOK, dashboardResponse{Data: dashboardItem(d, true), Meta: MutationMeta{RequestID: requestID, SchemaVersion: SchemaVersion, Warnings: []string{}}})
}

// deleteDashboard는 DELETE /api/v1/dashboards/{id}다. tombstone을 남긴다(204). If-Match가 있으면 현재 revision이어야 한다.
func (h *Handler) deleteDashboard(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Dashboards == nil {
		return dashboardsUnavailable()
	}
	if err := authz.Authorize(p, authz.DashboardsWrite); err != nil {
		return err
	}
	id, err := dashboardID(r)
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
	_, err = h.cfg.Dashboards.DeleteDashboard(ctx, p, id, ifMatch, httpapi.RequestIDFrom(r.Context()))
	if errors.Is(err, controldb.ErrRevisionMismatch) {
		return apierr.New(apierr.RevisionMismatch, "다른 변경이 먼저 저장되었습니다")
	}
	if err != nil {
		return dashboardMissing(err)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
	return nil
}
