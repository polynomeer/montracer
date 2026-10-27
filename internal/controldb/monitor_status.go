package controldb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/notify"
)

// monitor 상태 조회 (S09, D05 §09, ADR 0054). 평가 저장소(ADR 0051)·알림(ADR 0053)을 읽기만 한다.
//   - 목록 요약은 group 상태 중 가장 나쁜 것이다. 평가된 적이 없거나 꺼진 monitor를 OK로 보이지 않는다(계약 6).
//   - 마지막 평가가 주기의 3배(최소 5분)보다 오래됐으면 stale이다 — 평가가 멈춘 monitor가 마지막 상태로 굳어 보이지 않게.

// 목록 요약 상태: 평가 상태(ADR 0050)에 더해.
const (
	SummaryNotEvaluated = "NOT_EVALUATED" // 켜져 있지만 아직 평가 기록이 없다
	SummaryDisabled     = "DISABLED"
)

// stateRank는 요약에 쓸 나쁜 순서다(작을수록 나쁘다). 결측·오류는 정상보다 나쁘다.
var stateRank = map[string]int{"ALERT": 0, "EVALUATION_ERROR": 1, "NO_DATA": 2, "RECOVERING": 3, "PENDING": 4, "OK": 5}

// MonitorTimelineMax는 timeline 종류마다 돌려주는 최대 행 수다(가장 최근부터).
const MonitorTimelineMax = 2000

// MaxMonitorTimelineRange는 timeline 조회 범위 상한이다(평가 기록 보존 7일과 같다).
const MaxMonitorTimelineRange = EvaluationRetention

// EvaluationSummary는 평가 한 건이다(monitor_evaluations).
type EvaluationSummary struct {
	Slot        time.Time
	Status      string // evaluated | no_data | error
	Reason      *string
	WindowStart *time.Time
	WindowEnd   *time.Time
	Groups      int
	Alerting    int
	DurationMS  int
}

// MonitorStatus는 monitor 하나의 지금 상태 요약이다.
type MonitorStatus struct {
	State          string         // ALERT … OK, NOT_EVALUATED, DISABLED
	GroupsByState  map[string]int // 이 revision의 group 상태별 수
	LastEvaluation *EvaluationSummary
	Stale          bool // 마지막 평가가 주기의 3배(최소 5분)보다 오래됐다. 한 번도 평가되지 않았으면 정의가 바뀐 뒤 그만큼 지났다(ADR 0073 §2)
	// StateSince는 요약 상태와 같은 group 중 가장 이른 상태 시작 시각이다(ADR 0073 §3). 그런 group 하나라도 모르거나
	// 요약이 group에서 오지 않았으면(평가 전·꺼짐·group 없는 결측·실패) nil이다.
	StateSince *time.Time
}

// GroupState는 group 하나의 지금 상태다(alert_instances).
type GroupState struct {
	GroupHash      string
	Labels         map[string]string
	State          string
	EpisodeID      *string
	EpisodeReason  *string
	ViolationSince *time.Time
	NoDataSince    *time.Time
	LastWindowEnd  *time.Time
	LastValue      *float64
	LastReason     *string
	UpdatedAt      time.Time
	StateSince     *time.Time // 이 상태가 된 시각(ADR 0073). 이 열 전의 행은 nil
}

// Transition은 outbox의 상태 전이 event 하나다(alert.state_changed).
type Transition struct {
	At        time.Time
	GroupHash string
	Labels    map[string]string
	From      string
	To        *string // 정의 변경으로 끝난 상태는 null
	Opened    bool
	Closed    bool
	EpisodeID *string
	Reason    *string
	Value     *float64
}

// NotificationAttempt는 알림 하나의 지금 상태다(notification_deliveries).
type NotificationAttempt struct {
	ID             string
	PolicyID       string
	EventType      string
	Sequence       int // 재알림 번호(1..3), 그 밖은 0
	GroupHash      string
	EpisodeID      string
	Status         string
	Attempts       int
	LastStatusCode *int
	LastError      *string
	CreatedAt      time.Time
	FinishedAt     *time.Time
}

// MonitorTimeline은 기간 안의 평가·전이·알림이다. Truncated면 그 종류가 상한(가장 최근 MonitorTimelineMax개)에서 잘렸다.
type MonitorTimeline struct {
	Evaluations            []EvaluationSummary
	Transitions            []Transition
	Notifications          []NotificationAttempt
	EvaluationsTruncated   bool
	TransitionsTruncated   bool
	NotificationsTruncated bool
}

func staleAfter(evaluationSeconds int) time.Duration {
	return max(3*time.Duration(evaluationSeconds)*time.Second, 5*time.Minute)
}

// MonitorStatuses는 monitor들의 상태 요약이다(목록·단건). 없는 id는 결과에 없다.
// 상태 요약은 group 상태 수만 담고 값·labels는 없지만, 그것도 tenant 전체 telemetry에서 나온 판정이다 — 같은 규칙으로 막는다.
func (s *MonitorStore) MonitorStatuses(ctx context.Context, p authz.Principal, ids []string, now time.Time) (map[string]MonitorStatus, error) {
	if err := AuthorizeMonitorState(p); err != nil {
		return nil, err
	}
	out := make(map[string]MonitorStatus, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		var err error
		out, err = monitorStatusesTx(ctx, tx, ids, now)
		return err
	})
	return out, err
}

// monitorStatusesTx는 tenant 트랜잭션 안의 상태 요약이다(서비스 상태가 같은 트랜잭션에서 쓴다 — revision이 바뀌는 사이에 섞이지 않게).
func monitorStatusesTx(ctx context.Context, tx pgx.Tx, ids []string, now time.Time) (map[string]MonitorStatus, error) {
	out := make(map[string]MonitorStatus, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	err := func() error {
		// 일정(주기·active·revision)과 마지막 평가
		rows, err := tx.Query(ctx, `
			SELECT s.monitor_id::text, s.evaluation_seconds, s.active, s.revision, m.updated_at,
			       e.slot_start, e.status, e.reason, e.window_start, e.window_end, e.groups, e.alerting, e.duration_ms
			FROM monitor_schedule s JOIN monitors m ON m.tenant_id = s.tenant_id AND m.id = s.monitor_id
			LEFT JOIN LATERAL (
				SELECT slot_start, status, reason, window_start, window_end, groups, alerting, duration_ms
				FROM monitor_evaluations ev WHERE ev.monitor_id = s.monitor_id AND ev.revision = s.revision ORDER BY slot_start DESC LIMIT 1
			) e ON true
			WHERE s.monitor_id = ANY($1::uuid[])`, ids)
		if err != nil {
			return classify("select monitor status", err)
		}
		revisions := map[string]int64{}
		for rows.Next() {
			var (
				id                string
				seconds           int
				active            bool
				revision          int64
				updatedAt         time.Time
				slot              *time.Time
				status            *string
				reason            *string
				windowStart, wEnd *time.Time
				groups, alerting  *int
				durationMS        *int
			)
			if err := rows.Scan(&id, &seconds, &active, &revision, &updatedAt, &slot, &status, &reason, &windowStart, &wEnd, &groups, &alerting, &durationMS); err != nil {
				rows.Close()
				return classify("scan monitor status", err)
			}
			st := MonitorStatus{GroupsByState: map[string]int{}}
			if slot != nil {
				st.LastEvaluation = &EvaluationSummary{Slot: slot.UTC(), Status: *status, Reason: reason, WindowStart: utcPtr(windowStart), WindowEnd: utcPtr(wEnd),
					Groups: *groups, Alerting: *alerting, DurationMS: *durationMS}
				st.Stale = active && now.Sub(*slot) > staleAfter(seconds)
			} else {
				// 평가기가 한 번도 집어 가지 않았다: 정의(revision)가 바뀐 뒤 같은 시간이 지났으면 멈춘 것이다
				st.Stale = active && now.Sub(updatedAt) > staleAfter(seconds)
			}
			switch {
			case !active:
				st.State = SummaryDisabled
			case slot == nil:
				st.State = SummaryNotEvaluated
			}
			out[id] = st
			revisions[id] = revision
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return classify("select monitor status", err)
		}
		// group 상태별 수(이 revision의 상태만)
		rows, err = tx.Query(ctx, `
			SELECT monitor_id::text, revision, state, count(*), min(state_since), count(*) FILTER (WHERE state_since IS NULL)
			FROM alert_instances
			WHERE monitor_id = ANY($1::uuid[]) GROUP BY monitor_id, revision, state`, ids)
		if err != nil {
			return classify("select group states", err)
		}
		defer rows.Close()
		type since struct {
			at      *time.Time
			unknown bool
		}
		sinces := map[string]map[string]since{}
		for rows.Next() {
			var (
				id, state  string
				revision   int64
				n, unknown int
				earliest   *time.Time
			)
			if err := rows.Scan(&id, &revision, &state, &n, &earliest, &unknown); err != nil {
				return classify("scan group states", err)
			}
			st, ok := out[id]
			if !ok || revisions[id] != revision {
				continue
			}
			st.GroupsByState[state] += n
			out[id] = st
			if sinces[id] == nil {
				sinces[id] = map[string]since{}
			}
			sinces[id][state] = since{at: utcPtr(earliest), unknown: unknown > 0}
		}
		if err := rows.Err(); err != nil {
			return classify("select group states", err)
		}
		for id, st := range out {
			if st.State != "" {
				continue
			}
			worst, rank := "", len(stateRank)
			for state, n := range st.GroupsByState {
				if r, ok := stateRank[state]; ok && n > 0 && r < rank {
					worst, rank = state, r
				}
			}
			if worst != "" {
				if sc := sinces[id][worst]; !sc.unknown {
					st.StateSince = sc.at
				}
			}
			if worst == "" {
				// 평가는 됐지만 group이 없다(group_by 결과 없음·평가 오류만): 마지막 평가 결과로 요약한다 — OK로 보이지 않게
				switch st.LastEvaluation.Status {
				case "error":
					worst = "EVALUATION_ERROR"
				case "no_data":
					worst = "NO_DATA"
				default:
					worst = "NO_DATA" // series가 하나도 없다
				}
			}
			st.State = worst
			out[id] = st
		}
		return nil
	}()
	return out, err
}

// AuthorizeMonitorState는 경보 상태·값 조회 권한이다: monitors.read + telemetry.read, environment 제한이 없을 것.
// 상태와 값은 monitor가 tenant 전체 telemetry를 평가한 결과다(ADR 0051 §3). environment 제한 key가 보면
// 자기 범위 밖 environment의 값·labels를 보게 된다(D04 §01: key는 발급 범위보다 강해질 수 없다).
func AuthorizeMonitorState(p authz.Principal) error {
	if err := authz.AuthorizeTenantWide(p, authz.MonitorsRead); err != nil {
		return err
	}
	return authz.Authorize(p, authz.TelemetryRead)
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// MonitorGroups는 monitor의 지금 group 상태다(나쁜 상태 먼저, 같은 상태는 오래된 것 먼저). 삭제된 monitor는 authz.ErrNotFound.
func (s *MonitorStore) MonitorGroups(ctx context.Context, p authz.Principal, id string, limit int) ([]GroupState, bool, error) {
	if err := AuthorizeMonitorState(p); err != nil {
		return nil, false, err
	}
	if limit < 1 || limit > 1000 {
		return nil, false, errors.New("controldb: group limit must be within [1, 1000]")
	}
	var out []GroupState
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		if err := monitorExists(ctx, tx, id); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT ai.group_hash, ai.labels, ai.state, ai.episode_id::text, ai.episode_reason, ai.violation_since, ai.nodata_since,
			       ai.last_window_end, ai.last_value, ai.last_reason, ai.updated_at, ai.state_since
			FROM alert_instances ai JOIN monitor_schedule s ON s.monitor_id = ai.monitor_id AND s.revision = ai.revision
			WHERE ai.monitor_id = $1
			ORDER BY CASE ai.state WHEN 'ALERT' THEN 0 WHEN 'EVALUATION_ERROR' THEN 1 WHEN 'NO_DATA' THEN 2 WHEN 'RECOVERING' THEN 3 WHEN 'PENDING' THEN 4 ELSE 5 END,
			         ai.updated_at, ai.group_hash
			LIMIT $2`, id, limit+1)
		if err != nil {
			return classify("select monitor groups", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				g      GroupState
				labels []byte
			)
			if err := rows.Scan(&g.GroupHash, &labels, &g.State, &g.EpisodeID, &g.EpisodeReason, &g.ViolationSince, &g.NoDataSince,
				&g.LastWindowEnd, &g.LastValue, &g.LastReason, &g.UpdatedAt, &g.StateSince); err != nil {
				return classify("scan monitor group", err)
			}
			var stored struct {
				Labels map[string]string `json:"labels"`
			}
			if err := json.Unmarshal(labels, &stored); err != nil {
				return fmt.Errorf("controldb: alert instance labels: %w", err)
			}
			g.Labels = stored.Labels
			if g.Labels == nil {
				g.Labels = map[string]string{}
			}
			g.ViolationSince, g.NoDataSince, g.LastWindowEnd = utcPtr(g.ViolationSince), utcPtr(g.NoDataSince), utcPtr(g.LastWindowEnd)
			g.StateSince = utcPtr(g.StateSince)
			g.UpdatedAt = g.UpdatedAt.UTC()
			out = append(out, g)
		}
		return classify("select monitor groups", rows.Err())
	})
	if err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// MonitorTimelineFor는 [from, to) 안의 평가·전이·알림이다(종류마다 가장 최근 MonitorTimelineMax개, 시간순).
func (s *MonitorStore) MonitorTimelineFor(ctx context.Context, p authz.Principal, id string, from, to time.Time) (MonitorTimeline, error) {
	if err := AuthorizeMonitorState(p); err != nil {
		return MonitorTimeline{}, err
	}
	if !from.Before(to) || to.Sub(from) > MaxMonitorTimelineRange {
		return MonitorTimeline{}, fmt.Errorf("controldb: timeline range must be within (0, %s]", MaxMonitorTimelineRange)
	}
	var tl MonitorTimeline
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		if err := monitorExists(ctx, tx, id); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT slot_start, status, reason, window_start, window_end, groups, alerting, duration_ms FROM monitor_evaluations
			WHERE monitor_id = $1 AND slot_start >= $2 AND slot_start < $3 ORDER BY slot_start DESC LIMIT $4`, id, from, to, MonitorTimelineMax+1)
		if err != nil {
			return classify("select evaluations", err)
		}
		for rows.Next() {
			var e EvaluationSummary
			if err := rows.Scan(&e.Slot, &e.Status, &e.Reason, &e.WindowStart, &e.WindowEnd, &e.Groups, &e.Alerting, &e.DurationMS); err != nil {
				rows.Close()
				return classify("scan evaluation", err)
			}
			e.Slot, e.WindowStart, e.WindowEnd = e.Slot.UTC(), utcPtr(e.WindowStart), utcPtr(e.WindowEnd)
			tl.Evaluations = append(tl.Evaluations, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return classify("select evaluations", err)
		}
		rows, err = tx.Query(ctx, `
			SELECT occurred_at, payload FROM outbox
			WHERE type = 'alert.state_changed' AND resource_id = $1 AND occurred_at >= $2 AND occurred_at < $3
			ORDER BY occurred_at DESC, event_id LIMIT $4`, id, from, to, MonitorTimelineMax+1)
		if err != nil {
			return classify("select transitions", err)
		}
		for rows.Next() {
			var (
				at  time.Time
				raw []byte
			)
			if err := rows.Scan(&at, &raw); err != nil {
				rows.Close()
				return classify("scan transition", err)
			}
			var pl struct {
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
			if err := json.Unmarshal(raw, &pl); err != nil {
				rows.Close()
				return fmt.Errorf("controldb: transition payload: %w", err)
			}
			if pl.Labels == nil {
				pl.Labels = map[string]string{}
			}
			tl.Transitions = append(tl.Transitions, Transition{At: at.UTC(), GroupHash: pl.GroupHash, Labels: pl.Labels, From: pl.From, To: pl.To,
				Opened: pl.Opened, Closed: pl.Closed, EpisodeID: pl.EpisodeID, Reason: pl.Reason, Value: pl.Value})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return classify("select transitions", err)
		}
		rows, err = tx.Query(ctx, `
			SELECT id::text, policy_id::text, event_type, group_hash, episode_id::text, status, attempts, last_status_code, last_error, created_at, finished_at
			FROM notification_deliveries
			WHERE monitor_id = $1 AND created_at >= $2 AND created_at < $3 ORDER BY created_at DESC, id LIMIT $4`, id, from, to, MonitorTimelineMax+1)
		if err != nil {
			return classify("select notifications", err)
		}
		defer rows.Close()
		for rows.Next() {
			var n NotificationAttempt
			if err := rows.Scan(&n.ID, &n.PolicyID, &n.EventType, &n.GroupHash, &n.EpisodeID, &n.Status, &n.Attempts, &n.LastStatusCode, &n.LastError,
				&n.CreatedAt, &n.FinishedAt); err != nil {
				return classify("scan notification", err)
			}
			n.CreatedAt, n.FinishedAt = n.CreatedAt.UTC(), utcPtr(n.FinishedAt)
			if k, ok := notify.ParseRenotifyKind(n.EventType); ok {
				n.EventType, n.Sequence = notify.EventRenotified, k // 저장 종류(alert.renotified.k)를 webhook과 같은 이름 + 번호로
			}
			tl.Notifications = append(tl.Notifications, n)
		}
		return classify("select notifications", rows.Err())
	})
	if err != nil {
		return MonitorTimeline{}, err
	}
	tl.Evaluations, tl.EvaluationsTruncated = trimReverse(tl.Evaluations)
	tl.Transitions, tl.TransitionsTruncated = trimReverse(tl.Transitions)
	tl.Notifications, tl.NotificationsTruncated = trimReverse(tl.Notifications)
	return tl, nil
}

// trimReverse는 최근순으로 읽은 MonitorTimelineMax+1개를 상한으로 자르고 시간순으로 뒤집는다.
func trimReverse[T any](xs []T) ([]T, bool) {
	truncated := len(xs) > MonitorTimelineMax
	if truncated {
		xs = xs[:MonitorTimelineMax]
	}
	for i, j := 0, len(xs)-1; i < j; i, j = i+1, j-1 {
		xs[i], xs[j] = xs[j], xs[i]
	}
	if xs == nil {
		xs = []T{}
	}
	return xs, truncated
}

// monitorExists는 살아 있는 monitor인지 본다(없거나 삭제·다른 tenant면 authz.ErrNotFound).
func monitorExists(ctx context.Context, tx pgx.Tx, id string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM monitors WHERE id = $1 AND deleted_at IS NULL)`, id).Scan(&exists); err != nil {
		return classify("select monitor", err)
	}
	if !exists {
		return authz.ErrNotFound
	}
	return nil
}
