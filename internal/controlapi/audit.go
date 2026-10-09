// Package controlapi는 관리 API다 (cmd/control-api, D02 §14, §20).
//
// 감사 조회 GET /api/v1/audit-events(ADR 0034)와 monitor 정의 API(ADR 0049)가 있다.
// tenant는 인증 principal에서만 오고, 감사 범주는 principal 권한으로 제한한다(ADR 0015 §4).
package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
	"unicode"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/httpapi"
)

// SchemaVersion은 관리 API 응답 schema 버전이다.
const SchemaVersion = 1

const (
	defaultLimit = 100
	// maxAuditRange는 한 번에 조회할 수 있는 시간 범위 상한이다(외부 입력 검증, ADR 0034).
	// 보안 audit 보존 365일(D04 §04 제안)에 경계 하루를 더한 값이다.
	maxAuditRange = 366 * 24 * time.Hour
	maxActionLen  = 128
)

// AuditStore는 감사 저장소다 (controldb.AuditStore).
type AuditStore interface {
	ListAuditEvents(ctx context.Context, p authz.Principal, q controldb.AuditQuery) ([]controldb.AuditEvent, bool, error)
}

// Config는 Handler 설정이다.
type Config struct {
	// Authenticate는 bearer token을 principal로 바꾼다. 관리 API는 API key만 받는다(ingest key 거절, D02 §12).
	Authenticate func(ctx context.Context, token string) (authz.Principal, error)
	Audit        AuditStore
	// Monitors가 nil이면 monitor API(/api/v1/monitors…)는 404다 (ADR 0049).
	Monitors MonitorStore
	Cursor   *apicursor.Signer
	Logger   *slog.Logger
	// Timeout은 요청 하나의 저장소 조회 상한이다(기본 5초).
	Timeout time.Duration
	Observe httpapi.Observe
	Now     func() time.Time
}

// Handler는 관리 API다.
type Handler struct {
	cfg Config
	mux *http.ServeMux
}

// NewHandler는 route를 등록한다.
func NewHandler(cfg Config) (*Handler, error) {
	if cfg.Authenticate == nil || cfg.Audit == nil || cfg.Cursor == nil {
		return nil, errors.New("controlapi: Authenticate, Audit and Cursor are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	b := httpapi.Boundary{Logger: cfg.Logger}
	h.mux.Handle("GET /api/v1/audit-events", b.Handle(h.authenticated(h.listAuditEvents)))
	h.registerMonitors(b)
	return h, nil
}

// ServeHTTP는 request ID를 붙여 route로 넘긴다.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	httpapi.RequestID(httpapi.Instrument(h.mux, h.cfg.Observe)).ServeHTTP(w, r)
}

type principalHandler func(w http.ResponseWriter, r *http.Request, p authz.Principal) error

// authenticated는 bearer token을 검증한다. 인증 저장소 장애는 503으로 fail closed한다.
func (h *Handler) authenticated(next principalHandler) httpapi.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		token, ok := httpapi.Bearer(r)
		if !ok {
			return authz.ErrUnauthenticated
		}
		p, err := h.cfg.Authenticate(r.Context(), token)
		if err != nil {
			return err
		}
		return next(w, r.WithContext(authz.WithPrincipal(r.Context(), p)), p)
	}
}

// Meta는 관리 API 목록 응답 meta다.
type Meta struct {
	RequestID     string `json:"request_id"`
	SchemaVersion int    `json:"schema_version"`
}

// AuditEvent는 응답의 감사 이벤트다.
type AuditEvent struct {
	ID           string          `json:"id"`
	OccurredAt   string          `json:"occurred_at"`
	Category     string          `json:"category"`
	Action       string          `json:"action"`
	ActorKind    string          `json:"actor_kind"`
	ActorID      string          `json:"actor_id"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	RequestID    *string         `json:"request_id"`
	Details      json.RawMessage `json:"details"`
}

// auditResponse는 D02 §19 공통 성공 응답(data, meta, next_cursor)이다. next_cursor는 더 없으면 null.
type auditResponse struct {
	Data       []AuditEvent `json:"data"`
	NextCursor *string      `json:"next_cursor"`
	Meta       Meta         `json:"meta"`
}

// auditPosition은 cursor에 담는 위치다(응답에 이미 있는 값만).
type auditPosition struct {
	OccurredAt string `json:"at"`
	ID         string `json:"id"`
}

// listAuditEvents는 GET /api/v1/audit-events?from&to[&category][&action][&limit][&cursor]다.
func (h *Handler) listAuditEvents(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	q := r.URL.Query()
	from, err := parseTime(q.Get("from"), "from")
	if err != nil {
		return err
	}
	to, err := parseTime(q.Get("to"), "to")
	if err != nil {
		return err
	}
	if !from.Before(to) || to.Sub(from) > maxAuditRange {
		return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다",
			apierr.FieldViolation{Field: "to", Reason: "must be after from, within 366 days"})
	}
	var categories []string
	switch c := q.Get("category"); c {
	case "":
	case authz.AuditCategoryOperations, authz.AuditCategorySecurity:
		categories = []string{c}
	default:
		return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다",
			apierr.FieldViolation{Field: "category", Reason: "must be operations or security"})
	}
	action := q.Get("action")
	if !validAction(action) {
		return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다",
			apierr.FieldViolation{Field: "action", Reason: "at most 128 printable characters"})
	}
	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > controldb.MaxAuditPage {
			return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다",
				apierr.FieldViolation{Field: "limit", Reason: "must be within [1, 1000]"})
		}
		limit = n
	}

	// 권한 fingerprint: 이 principal이 볼 수 있는 범주가 바뀌면(강등 등) 이전 cursor는 쓸 수 없다.
	allowed, err := authz.AuditCategories(p)
	if err != nil {
		return err
	}
	binding := apicursor.Binding{
		Tenant:      p.Tenant().String(),
		Fingerprint: apicursor.Fingerprint(append([]string{p.Kind().String(), p.Subject()}, allowed...)...),
		QueryHash: apicursor.Fingerprint(from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano),
			q.Get("category"), action),
	}

	now := h.cfg.Now().UTC()
	snapshot := now
	var after *controldb.AuditPosition
	if c := q.Get("cursor"); c != "" {
		pos, err := h.decodeCursor(c, binding)
		if err != nil {
			return err
		}
		snapshot, after = pos.snapshot, &pos.AuditPosition
	}
	// snapshot 이후에 생긴 행은 이 page 묶음에서 읽지 않는다(첫 page 시각 고정, D02 §12).
	upper := to
	if snapshot.Before(upper) {
		upper = snapshot
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	resp := auditResponse{Data: []AuditEvent{}, Meta: Meta{RequestID: httpapi.RequestIDFrom(r.Context()), SchemaVersion: SchemaVersion}}
	if from.Before(upper) {
		events, more, err := h.cfg.Audit.ListAuditEvents(ctx, p, controldb.AuditQuery{
			From: from, To: upper, Categories: categories, Action: action, After: after, Limit: limit,
		})
		if err != nil {
			return err
		}
		for _, e := range events {
			ev, err := present(e)
			if err != nil {
				return err
			}
			resp.Data = append(resp.Data, ev)
		}
		if more && len(events) > 0 {
			last := events[len(events)-1]
			tok, err := h.cfg.Cursor.Encode(binding, snapshot, auditPosition{OccurredAt: last.OccurredAt.Format(time.RFC3339Nano), ID: last.ID})
			if err != nil {
				return err
			}
			resp.NextCursor = &tok
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	return json.NewEncoder(w).Encode(resp)
}

// operatorActor는 고객 응답에서 플랫폼 운영자 대신 보여 주는 actor_id다.
const operatorActor = "montracer-operator"

// present는 저장된 감사 행을 고객 응답으로 바꾼다.
// 플랫폼 운영자(break-glass) 행은 직원 개인 식별자(운영자·승인자 ID)를 가린다. 사유·ticket·시각은 그대로 둔다
// (Google Access Transparency처럼 접근 사유는 보이고 직원 신원은 보이지 않게, ADR 0034 §2).
// 내부 조사용 원본은 제어 DB에 그대로 남는다.
func present(e controldb.AuditEvent) (AuditEvent, error) {
	details := e.Details
	if len(details) == 0 {
		details = json.RawMessage("{}")
	}
	actorID := e.ActorID
	if e.ActorKind == "operator" {
		actorID = operatorActor
		var d map[string]any
		if err := json.Unmarshal(details, &d); err != nil {
			return AuditEvent{}, errors.New("controlapi: operator audit details are not an object")
		}
		if bg, ok := d["break_glass"].(map[string]any); ok {
			delete(bg, "approver")
		}
		b, err := json.Marshal(d)
		if err != nil {
			return AuditEvent{}, errors.New("controlapi: encode audit details")
		}
		details = b
	}
	return AuditEvent{
		ID: e.ID, OccurredAt: e.OccurredAt.Format(time.RFC3339Nano), Category: e.Category, Action: e.Action,
		ActorKind: e.ActorKind, ActorID: actorID, ResourceType: e.ResourceType, ResourceID: e.ResourceID,
		RequestID: e.RequestID, Details: details,
	}, nil
}

type cursorPosition struct {
	controldb.AuditPosition
	snapshot time.Time
}

// decodeCursor는 cursor를 검증해 위치와 snapshot을 돌려준다. 실패 원인은 구분하지 않는다.
func (h *Handler) decodeCursor(token string, b apicursor.Binding) (cursorPosition, error) {
	bad := apierr.NewInvalidArgument("요청 값이 올바르지 않습니다",
		apierr.FieldViolation{Field: "cursor", Reason: "invalid, expired, or not for this request"})
	claims, err := h.cfg.Cursor.Decode(token, b)
	if err != nil {
		return cursorPosition{}, bad
	}
	var pos auditPosition
	if json.Unmarshal(claims.Position, &pos) != nil || pos.ID == "" {
		return cursorPosition{}, bad
	}
	at, err := time.Parse(time.RFC3339Nano, pos.OccurredAt)
	if err != nil {
		return cursorPosition{}, bad
	}
	return cursorPosition{AuditPosition: controldb.AuditPosition{OccurredAt: at, ID: pos.ID}, snapshot: claims.Snapshot}, nil
}

func parseTime(v, field string) (time.Time, error) {
	if v == "" {
		return time.Time{}, apierr.NewInvalidArgument("요청 값이 올바르지 않습니다",
			apierr.FieldViolation{Field: field, Reason: "required (RFC3339 UTC)"})
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, apierr.NewInvalidArgument("요청 값이 올바르지 않습니다",
			apierr.FieldViolation{Field: field, Reason: "must be RFC3339"})
	}
	return t.UTC(), nil
}

func validAction(s string) bool {
	if len(s) > maxActionLen {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
