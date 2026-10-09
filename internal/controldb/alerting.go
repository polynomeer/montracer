package controldb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

// alert-worker 저장소 (D02 §11·§17, ADR 0051).
//   - 할 일 찾기(DueMonitors)만 tenant를 가로지른다. monitor_schedule의 id·주기만 읽고(app.monitor_scan), 읽기 전용이다.
//   - 그 밖의 모든 읽기·쓰기는 대상 tenant의 WithTenant 트랜잭션이다(RLS).
//   - slot은 (tenant, monitor, slot) 한 번만 평가한다: lease(만료 시 다른 worker가 가져감) + monitor_evaluations PK.

// EvaluationRetention은 평가 기록 보존 기간이다(완료할 때 그 monitor의 오래된 기록을 지운다).
const EvaluationRetention = 7 * 24 * time.Hour

// AlertActor는 alert-worker가 쓰는 outbox event의 actor다(인스턴스 이름은 평가 기록에만 남긴다).
const AlertActor = "alert-worker"

// ErrLeaseLost는 완료하려 할 때 lease가 이미 다른 worker로 넘어갔다는 뜻이다. 결과를 쓰지 않는다.
var ErrLeaseLost = errors.New("controldb: evaluation lease lost")

// DueMonitor는 평가할 차례인 monitor다.
type DueMonitor struct {
	Tenant    authz.TenantID
	MonitorID string
}

// StoredInstance는 alert_instances 한 행이다. GroupKey는 원문 key(해시 전)다.
type StoredInstance struct {
	GroupKey       string
	Labels         map[string]string
	Revision       int64
	State          string
	EpisodeID      *string
	EpisodeReason  *string
	ViolationSince *time.Time
	NoDataSince    *time.Time
	OKStreak       int
	LastWindowEnd  *time.Time
	LastValue      *float64
	LastReason     *string
}

// Claim은 lease를 잡은 평가 한 건이다.
type Claim struct {
	Tenant    authz.TenantID
	MonitorID string
	// Token은 이 claim의 lease token이다(claim마다 새 UUID). 완료는 이 token으로만 된다 — worker 이름이 겹쳐도 다른 claim과 섞이지 않는다.
	Token     string
	Revision  int64
	Slot      time.Time
	Spec      []byte
	Instances []StoredInstance // 이 revision의 상태만
}

// GroupWrite는 group 하나의 평가 결과다.
type GroupWrite struct {
	Instance   StoredInstance
	Changed    bool // 상태가 바뀌었거나 사건이 열리고 닫혔다 → outbox
	From, To   string
	Opened     bool
	Closed     bool
	WindowEnd  *time.Time
	EpisodeOut *string // 이벤트에 실을 사건 id(닫힌 경우 닫히기 전 id)
}

// Completion은 평가 결과다.
type Completion struct {
	Status      string // evaluated | no_data | error
	Reason      string
	WindowStart *time.Time
	WindowEnd   *time.Time
	Groups      []GroupWrite
	Duration    time.Duration
}

// AlertStore는 alert-worker 저장소다.
type AlertStore struct{ db *DB }

// NewAlertStore는 저장소를 만든다.
func NewAlertStore(db *DB) *AlertStore { return &AlertStore{db: db} }

// GroupHash는 group key의 SHA-256 hex다(alert_instances PK).
func GroupHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

const slotExpr = `to_timestamp(floor(extract(epoch FROM $1::timestamptz) / evaluation_seconds) * evaluation_seconds)`

// DueMonitors는 지금 slot을 아직 평가하지 않았고 lease가 없거나 만료된 monitor를 돌려준다(오래 밀린 것부터).
// tenant를 가로지르는 유일한 조회다: monitor_schedule의 tenant·id만 읽는다(읽기 전용 트랜잭션, app.monitor_scan).
func (s *AlertStore) DueMonitors(ctx context.Context, now time.Time, limit int) ([]DueMonitor, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("controldb: due limit must be within [1, 1000]")
	}
	var out []DueMonitor
	err := s.db.inTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.monitor_scan', 'on', true)`); err != nil {
			return classify("set monitor scan", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT tenant_id::text, monitor_id::text FROM monitor_schedule
			WHERE active AND (last_slot IS NULL OR last_slot < `+slotExpr+`)
			  AND (lease_until IS NULL OR lease_until < $1)
			ORDER BY last_slot NULLS FIRST LIMIT $2`, now, limit)
		if err != nil {
			return classify("due monitors", err)
		}
		defer rows.Close()
		for rows.Next() {
			var tenant, id string
			if err := rows.Scan(&tenant, &id); err != nil {
				return classify("scan due monitor", err)
			}
			t, err := authz.ParseTenantID(tenant)
			if err != nil {
				return fmt.Errorf("controldb: stored tenant id: %w", err)
			}
			out = append(out, DueMonitor{Tenant: t, MonitorID: id})
		}
		return classify("due monitors", rows.Err())
	})
	return out, err
}

// ClaimEvaluation은 지금 slot의 lease를 잡고 평가에 필요한 정의·상태를 읽는다. 이미 끝났거나 다른 worker가 잡았으면 ok=false.
// now는 worker 시각이다. worker 간 시계 차이는 lease보다 훨씬 작다고 가정한다(NTP, ADR 0051 §2).
func (s *AlertStore) ClaimEvaluation(ctx context.Context, tenant authz.TenantID, monitorID string, now time.Time, lease time.Duration) (Claim, bool, error) {
	token, err := newUUID()
	if err != nil {
		return Claim{}, false, err
	}
	c := Claim{Tenant: tenant, MonitorID: monitorID, Token: token}
	ok := false
	err = s.db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE monitor_schedule SET lease_token = $3, lease_until = $1::timestamptz + $4::interval
			WHERE monitor_id = $2 AND active AND (last_slot IS NULL OR last_slot < `+slotExpr+`)
			  AND (lease_until IS NULL OR lease_until < $1)
			RETURNING revision, `+slotExpr,
			now, monitorID, token, fmt.Sprintf("%d milliseconds", lease.Milliseconds())).Scan(&c.Revision, &c.Slot)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return classify("claim evaluation", err)
		}
		c.Slot = c.Slot.UTC()
		if err := tx.QueryRow(ctx, `SELECT spec FROM monitor_revisions WHERE monitor_id = $1 AND revision = $2`, monitorID, c.Revision).Scan(&c.Spec); err != nil {
			return classify("select monitor revision", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT labels, revision, state, episode_id::text, episode_reason, violation_since, nodata_since, ok_streak,
			       last_window_end, last_value, last_reason, group_hash
			FROM alert_instances WHERE monitor_id = $1 AND revision = $2`, monitorID, c.Revision)
		if err != nil {
			return classify("select alert instances", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				in     StoredInstance
				labels []byte
				hash   string
			)
			if err := rows.Scan(&labels, &in.Revision, &in.State, &in.EpisodeID, &in.EpisodeReason, &in.ViolationSince, &in.NoDataSince,
				&in.OKStreak, &in.LastWindowEnd, &in.LastValue, &in.LastReason, &hash); err != nil {
				return classify("scan alert instance", err)
			}
			var stored struct {
				Key    string            `json:"key"`
				Labels map[string]string `json:"labels"`
			}
			if err := json.Unmarshal(labels, &stored); err != nil {
				return fmt.Errorf("controldb: alert instance labels: %w", err)
			}
			in.GroupKey, in.Labels = stored.Key, stored.Labels
			c.Instances = append(c.Instances, in)
		}
		if err := rows.Err(); err != nil {
			return classify("select alert instances", err)
		}
		ok = true
		return nil
	})
	return c, ok && err == nil, err
}

// CompleteEvaluation은 결과를 한 트랜잭션에 쓴다: lease 확인·해제 → group 상태 → 전이 outbox → 평가 기록 → 오래된 기록 정리.
// lease를 잃었으면(만료 뒤 다른 claim이 가져감, 정의 변경, 이미 끝난 slot) ErrLeaseLost이고 아무것도 쓰지 않는다. worker는 평가 기록에 남길 인스턴스 이름이다.
func (s *AlertStore) CompleteEvaluation(ctx context.Context, c Claim, worker string, r Completion) error {
	return s.db.WithTenant(ctx, c.Tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE monitor_schedule SET last_slot = $3, lease_token = NULL, lease_until = NULL
			WHERE monitor_id = $1 AND lease_token = $2 AND revision = $4 AND (last_slot IS NULL OR last_slot < $3)`, c.MonitorID, c.Token, c.Slot, c.Revision)
		if err != nil {
			return classify("release evaluation lease", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrLeaseLost // lease 만료·정의 변경(revision)·이미 끝난 slot — 이 결과는 낡았다
		}
		alerting := 0
		for _, g := range r.Groups {
			if err := writeInstance(ctx, tx, c, g.Instance); err != nil {
				return err
			}
			if g.Instance.State == "ALERT" {
				alerting++
			}
			if g.Changed {
				payload, err := json.Marshal(map[string]any{
					"group_hash": GroupHash(g.Instance.GroupKey), "labels": g.Instance.Labels, "from": g.From, "to": g.To,
					"opened": g.Opened, "closed": g.Closed, "episode_id": g.EpisodeOut, "episode_reason": g.Instance.EpisodeReason,
					"value": g.Instance.LastValue, "reason": g.Instance.LastReason, "window_end": g.WindowEnd, "slot": c.Slot,
				})
				if err != nil {
					return fmt.Errorf("controldb: alert event payload: %w", err)
				}
				if err := writeOutbox(ctx, tx, c.Tenant, "alert.state_changed", c.MonitorID, c.Revision, AlertActor, payload); err != nil {
					return err
				}
			}
		}
		var reason *string
		if r.Reason != "" {
			reason = &r.Reason
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO monitor_evaluations (tenant_id, monitor_id, slot_start, revision, status, reason, window_start, window_end, groups, alerting, worker, duration_ms)
			VALUES (app_tenant_id(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			c.MonitorID, c.Slot, c.Revision, r.Status, reason, r.WindowStart, r.WindowEnd, len(r.Groups), alerting, worker, r.Duration.Milliseconds()); err != nil {
			return classify("insert monitor evaluation", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM monitor_evaluations WHERE monitor_id = $1 AND slot_start < $2`, c.MonitorID, c.Slot.Add(-EvaluationRetention)); err != nil {
			return classify("prune monitor evaluations", err)
		}
		return nil
	})
}

func writeInstance(ctx context.Context, tx pgx.Tx, c Claim, in StoredInstance) error {
	labels, err := json.Marshal(map[string]any{"key": in.GroupKey, "labels": in.Labels})
	if err != nil {
		return fmt.Errorf("controldb: alert instance labels: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO alert_instances (tenant_id, monitor_id, group_hash, labels, revision, state, episode_id, episode_reason,
			violation_since, nodata_since, ok_streak, last_window_end, last_value, last_reason)
		VALUES (app_tenant_id(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (tenant_id, monitor_id, group_hash) DO UPDATE SET
			labels = EXCLUDED.labels, revision = EXCLUDED.revision, state = EXCLUDED.state, episode_id = EXCLUDED.episode_id,
			episode_reason = EXCLUDED.episode_reason, violation_since = EXCLUDED.violation_since, nodata_since = EXCLUDED.nodata_since,
			ok_streak = EXCLUDED.ok_streak, last_window_end = EXCLUDED.last_window_end, last_value = EXCLUDED.last_value,
			last_reason = EXCLUDED.last_reason, version = alert_instances.version + 1, updated_at = now()`,
		c.MonitorID, GroupHash(in.GroupKey), labels, c.Revision, in.State, in.EpisodeID, in.EpisodeReason,
		in.ViolationSince, in.NoDataSince, in.OKStreak, in.LastWindowEnd, in.LastValue, in.LastReason); err != nil {
		return classify("upsert alert instance", err)
	}
	return nil
}

// writeOutbox는 감사 없이 outbox event만 쓴다(시스템 상태 전이: 사람의 변경이 아니고 양이 많다).
func writeOutbox(ctx context.Context, tx pgx.Tx, tenant authz.TenantID, typ, resourceID string, revision int64, actorID string, payload []byte) error {
	eventID, err := newUUID()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (tenant_id, event_id, type, resource_id, revision, actor_id, schema_version, payload)
		VALUES ($1, $2, $3, $4, $5, $6, 1, $7)`, tenant.String(), eventID, typ, resourceID, revision, actorID, payload); err != nil {
		return classify("insert outbox", err)
	}
	return nil
}

// endInstances는 정의가 바뀌거나(revision) 꺼지거나 지워질 때 그 monitor의 경보 상태를 끝낸다(MonitorStore가 같은 트랜잭션에서 부른다).
// 열린 사건은 닫힘 event(alert.state_changed, closed, reason)를 내고 행을 지운다 — 사건이 조용히 남지 않는다.
func endInstances(ctx context.Context, tx pgx.Tx, tenant authz.TenantID, monitorID string, revision int64, actorID, reason string) error {
	rows, err := tx.Query(ctx, `SELECT group_hash, labels, state, episode_id::text, episode_reason FROM alert_instances WHERE monitor_id = $1`, monitorID)
	if err != nil {
		return classify("select alert instances", err)
	}
	type open struct {
		hash, state   string
		labels        []byte
		episode       *string
		episodeReason *string
	}
	var opens []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.hash, &o.labels, &o.state, &o.episode, &o.episodeReason); err != nil {
			rows.Close()
			return classify("scan alert instance", err)
		}
		if o.episode != nil {
			opens = append(opens, o)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return classify("select alert instances", err)
	}
	for _, o := range opens {
		var stored struct {
			Labels map[string]string `json:"labels"`
		}
		_ = json.Unmarshal(o.labels, &stored)
		payload, err := json.Marshal(map[string]any{
			"group_hash": o.hash, "labels": stored.Labels, "from": o.state, "to": nil, "opened": false, "closed": true,
			"episode_id": o.episode, "episode_reason": o.episodeReason, "reason": reason,
		})
		if err != nil {
			return fmt.Errorf("controldb: alert event payload: %w", err)
		}
		if err := writeOutbox(ctx, tx, tenant, "alert.state_changed", monitorID, revision, actorID, payload); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM alert_instances WHERE monitor_id = $1`, monitorID); err != nil {
		return classify("delete alert instances", err)
	}
	return nil
}
