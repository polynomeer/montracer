package controlapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

// monitor 상태 조회 API (S09, D05 §09, ADR 0054). 읽기 전용이고 monitors.read다.
//   - 요약 상태는 NOT_EVALUATED·DISABLED·NO_DATA·EVALUATION_ERROR를 OK와 구분한다(계약 6). 평가가 멈췄으면 stale이다.
//   - 값이 없으면 null과 이유다(0으로 대체하지 않는다).

// StatusItem은 monitor 상태 요약이다.
type StatusItem struct {
	State          string          `json:"state"`
	GroupsByState  map[string]int  `json:"groups_by_state"`
	LastEvaluation *EvaluationItem `json:"last_evaluation"`
	Stale          bool            `json:"stale"`
}

// EvaluationItem은 평가 한 건이다.
type EvaluationItem struct {
	Slot        string  `json:"slot"`
	Status      string  `json:"status"`
	Reason      *string `json:"reason"`
	WindowStart *string `json:"window_start"`
	WindowEnd   *string `json:"window_end"`
	Groups      int     `json:"groups"`
	Alerting    int     `json:"alerting"`
	DurationMS  int     `json:"duration_ms"`
}

// GroupItem은 group 하나의 지금 상태다.
type GroupItem struct {
	GroupHash      string            `json:"group_hash"`
	Labels         map[string]string `json:"labels"`
	State          string            `json:"state"`
	EpisodeID      *string           `json:"episode_id"`
	EpisodeReason  *string           `json:"episode_reason"`
	ViolationSince *string           `json:"violation_since"`
	NoDataSince    *string           `json:"nodata_since"`
	LastWindowEnd  *string           `json:"last_window_end"`
	LastValue      *float64          `json:"last_value"`
	LastReason     *string           `json:"last_reason"`
	UpdatedAt      string            `json:"updated_at"`
}

// TransitionItem은 상태 전이 하나다.
type TransitionItem struct {
	At        string            `json:"at"`
	GroupHash string            `json:"group_hash"`
	Labels    map[string]string `json:"labels"`
	From      string            `json:"from"`
	To        *string           `json:"to"`
	Opened    bool              `json:"opened"`
	Closed    bool              `json:"closed"`
	EpisodeID *string           `json:"episode_id"`
	Reason    *string           `json:"reason"`
	Value     *float64          `json:"value"`
}

// NotificationItem은 알림 하나의 상태다.
type NotificationItem struct {
	ID             string  `json:"id"`
	PolicyID       string  `json:"policy_id"`
	EventType      string  `json:"event_type"`
	Sequence       *int    `json:"renotify_sequence"` // 재알림 번호, 그 밖은 null
	GroupHash      string  `json:"group_hash"`
	EpisodeID      string  `json:"episode_id"`
	Status         string  `json:"status"`
	Attempts       int     `json:"attempts"`
	LastStatusCode *int    `json:"last_status_code"`
	LastError      *string `json:"last_error"`
	CreatedAt      string  `json:"created_at"`
	FinishedAt     *string `json:"finished_at"`
}

// TimelineItem은 기간 안의 평가·전이·알림이다(각각 시간순, 가장 최근 2,000개).
type TimelineItem struct {
	From                   string             `json:"from"`
	To                     string             `json:"to"`
	Evaluations            []EvaluationItem   `json:"evaluations"`
	Transitions            []TransitionItem   `json:"transitions"`
	Notifications          []NotificationItem `json:"notifications"`
	EvaluationsTruncated   bool               `json:"evaluations_truncated"`
	TransitionsTruncated   bool               `json:"transitions_truncated"`
	NotificationsTruncated bool               `json:"notifications_truncated"`
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func tsPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := ts(*t)
	return &s
}

func evaluationItem(e controldb.EvaluationSummary) EvaluationItem {
	return EvaluationItem{Slot: ts(e.Slot), Status: e.Status, Reason: e.Reason, WindowStart: tsPtr(e.WindowStart), WindowEnd: tsPtr(e.WindowEnd),
		Groups: e.Groups, Alerting: e.Alerting, DurationMS: e.DurationMS}
}

// statusItem은 요약을 응답으로 바꾼다. 일정이 없으면(00008 이전 정의 등) NOT_EVALUATED다.
// WarnStatusForbidden: 정의는 볼 수 있지만 상태는 볼 수 없다(environment 제한 key, telemetry.read 없음).
// 상태는 tenant 전체 telemetry를 평가한 결과라서다(ADR 0054 §1). status는 null이고, 화면은 "상태를 볼 수 없음"을 보인다.
const WarnStatusForbidden = "status_forbidden"

// statuses는 목록·단건에 붙일 상태다. 상태 권한이 없으면 실패가 아니라 nil과 경고다 — 정의 조회는 그대로 된다.
func (h *Handler) statuses(ctx context.Context, p authz.Principal, ids []string) (map[string]controldb.MonitorStatus, []string, error) {
	st, err := h.cfg.Monitors.MonitorStatuses(ctx, p, ids, h.cfg.Now())
	if errors.Is(err, authz.ErrForbidden) {
		return nil, []string{WarnStatusForbidden}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return st, []string{}, nil
}

func statusItem(s controldb.MonitorStatus) *StatusItem {
	out := &StatusItem{State: s.State, GroupsByState: s.GroupsByState, Stale: s.Stale}
	if out.State == "" {
		out.State = controldb.SummaryNotEvaluated
	}
	if out.GroupsByState == nil {
		out.GroupsByState = map[string]int{}
	}
	if s.LastEvaluation != nil {
		e := evaluationItem(*s.LastEvaluation)
		out.LastEvaluation = &e
	}
	return out
}

type groupsResponse struct {
	Data []GroupItem `json:"data"`
	Meta struct {
		Meta
		Truncated bool `json:"truncated"`
	} `json:"meta"`
}

// monitorGroups는 GET /api/v1/monitors/{id}/groups?limit다(나쁜 상태 먼저, 최대 1,000).
func (h *Handler) monitorGroups(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Monitors == nil {
		return monitorsUnavailable()
	}
	id, err := monitorID(r)
	if err != nil {
		return err
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "limit", Reason: "must be within [1, 1000]"})
		}
		limit = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	groups, more, err := h.cfg.Monitors.MonitorGroups(ctx, p, id, limit)
	if err != nil {
		return notFound(err)
	}
	var resp groupsResponse
	resp.Data = make([]GroupItem, 0, len(groups))
	for _, g := range groups {
		resp.Data = append(resp.Data, GroupItem{GroupHash: g.GroupHash, Labels: g.Labels, State: g.State, EpisodeID: g.EpisodeID, EpisodeReason: g.EpisodeReason,
			ViolationSince: tsPtr(g.ViolationSince), NoDataSince: tsPtr(g.NoDataSince), LastWindowEnd: tsPtr(g.LastWindowEnd),
			LastValue: g.LastValue, LastReason: g.LastReason, UpdatedAt: ts(g.UpdatedAt)})
	}
	resp.Meta.Meta = newMeta(r)
	resp.Meta.Truncated = more
	return writeJSON(w, http.StatusOK, resp)
}

type timelineResponse struct {
	Data TimelineItem `json:"data"`
	Meta Meta         `json:"meta"`
}

// monitorTimeline은 GET /api/v1/monitors/{id}/timeline?from&to다. 기본은 지난 24시간, 최대 7일(평가 기록 보존 기간).
func (h *Handler) monitorTimeline(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Monitors == nil {
		return monitorsUnavailable()
	}
	id, err := monitorID(r)
	if err != nil {
		return err
	}
	now := h.cfg.Now()
	to, from := now, now.Add(-24*time.Hour)
	q := r.URL.Query()
	parse := func(field string, def time.Time) (time.Time, error) {
		v := q.Get(field)
		if v == "" {
			return def, nil
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: field, Reason: "must be RFC 3339 with a time zone"})
		}
		return t, nil
	}
	if from, err = parse("from", from); err != nil {
		return err
	}
	if to, err = parse("to", to); err != nil {
		return err
	}
	if !from.Before(to) || to.Sub(from) > controldb.MaxMonitorTimelineRange {
		return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "from", Reason: "range must be positive and at most 7 days"})
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	tl, err := h.cfg.Monitors.MonitorTimelineFor(ctx, p, id, from, to)
	if err != nil {
		return notFound(err)
	}
	item := TimelineItem{From: ts(from), To: ts(to), Evaluations: make([]EvaluationItem, 0, len(tl.Evaluations)),
		Transitions: make([]TransitionItem, 0, len(tl.Transitions)), Notifications: make([]NotificationItem, 0, len(tl.Notifications)),
		EvaluationsTruncated: tl.EvaluationsTruncated, TransitionsTruncated: tl.TransitionsTruncated, NotificationsTruncated: tl.NotificationsTruncated}
	for _, e := range tl.Evaluations {
		item.Evaluations = append(item.Evaluations, evaluationItem(e))
	}
	for _, t := range tl.Transitions {
		item.Transitions = append(item.Transitions, TransitionItem{At: ts(t.At), GroupHash: t.GroupHash, Labels: t.Labels, From: t.From, To: t.To,
			Opened: t.Opened, Closed: t.Closed, EpisodeID: t.EpisodeID, Reason: t.Reason, Value: t.Value})
	}
	for _, n := range tl.Notifications {
		var seq *int
		if n.Sequence > 0 {
			s := n.Sequence
			seq = &s
		}
		item.Notifications = append(item.Notifications, NotificationItem{ID: n.ID, PolicyID: n.PolicyID, EventType: n.EventType, Sequence: seq, GroupHash: n.GroupHash,
			EpisodeID: n.EpisodeID, Status: n.Status, Attempts: n.Attempts, LastStatusCode: n.LastStatusCode, LastError: n.LastError,
			CreatedAt: ts(n.CreatedAt), FinishedAt: tsPtr(n.FinishedAt)})
	}
	return writeJSON(w, http.StatusOK, timelineResponse{Data: item, Meta: newMeta(r)})
}
