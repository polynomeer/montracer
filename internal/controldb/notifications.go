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

// 경보 알림 저장소 (D02 §17·§20, ADR 0053).
//   - notification policy: webhook 목적지와 암호화한 서명 secret. 변경은 security 감사·outbox와 한 트랜잭션, 삭제는 tombstone.
//   - 알림(notification_deliveries)은 경보 전이와 같은 트랜잭션에 쌓는다(CompleteEvaluation·endInstances).
//   - 전송 worker는 tenant를 가로질러 보낼 알림만 찾고(delivery_scan, id·시각만), 나머지는 tenant 트랜잭션이다.

// ErrNotificationPolicyNotFound는 monitor가 가리킨 policy가 이 tenant에 없거나 지워졌다는 뜻이다(저장 전 검증, D02 §14).
var ErrNotificationPolicyNotFound = errors.New("controldb: notification policy not found")

// ErrPolicyInUse는 살아 있는 monitor가 가리키는 policy를 지우려 했다는 뜻이다(409). 지우면 그 monitor의 알림이 조용히 끊긴다.
var ErrPolicyInUse = errors.New("controldb: notification policy is referenced by monitors")

// DeliveryRetention은 끝난 알림 기록 보존 기간이다.
const DeliveryRetention = 30 * 24 * time.Hour

// NotificationPolicy는 저장된 policy다. URL 전체·secret은 담지 않는다(암호문으로만 저장).
type NotificationPolicy struct {
	ID          string
	Name        string
	WebhookURL  string // 화면용 표시(scheme·host만, notify.RedactURL)
	SecretKeyID string
	// PreviousKeyID는 secret 교체 뒤 아직 함께 서명하는 이전 secret의 id다(겹침 기간이 끝났으면 nil, ADR 0055)
	PreviousKeyID        *string
	PreviousKeyExpiresAt *time.Time
	Enabled              bool
	Revision             int64
	CreatedBy            string
	UpdatedBy            string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// PolicyWrite는 저장할 policy다. URL은 평문으로 넘기지 않는다: Display(scheme·host)와 policy id에 묶어 암호화하는 SealURL이다.
type PolicyWrite struct {
	Name    string
	Display string // notify.RedactURL(검증된 URL)
	SealURL func(policyID string) (Sealed, error)
	Enabled bool
}

// Sealed는 policy id에 묶은 암호문과 그 master key 버전이다(notify.Sealer).
type Sealed struct {
	Ciphertext []byte
	Version    string
}

// SealedSecret은 policy id에 묶어 암호화한 서명 secret이다(notify.Sealer).
type SealedSecret struct {
	KeyID      string
	Ciphertext []byte
	SealingKey string
}

// PolicyPosition은 목록 keyset 위치다.
type PolicyPosition struct {
	NameNormalized string
	ID             string
}

// NotificationPolicyStore는 policy 저장소다.
type NotificationPolicyStore struct{ db *DB }

// NewNotificationPolicyStore는 저장소를 만든다.
func NewNotificationPolicyStore(db *DB) *NotificationPolicyStore {
	return &NotificationPolicyStore{db: db}
}

// MaxPolicyPage는 목록 page 상한이다.
const MaxPolicyPage = 200

// 겹침 기간이 끝난 이전 secret은 없는 것으로 보인다(행에는 다음 교체 때까지 남지만 서명에 쓰지 않는다)
const policyColumns = `id::text, name, webhook_display, secret_key_id,
	CASE WHEN previous_secret_expires_at > now() THEN previous_secret_key_id END,
	CASE WHEN previous_secret_expires_at > now() THEN previous_secret_expires_at END,
	enabled, revision, created_by, updated_by, created_at, updated_at`

func policyDest(p *NotificationPolicy) []any {
	return []any{&p.ID, &p.Name, &p.WebhookURL, &p.SecretKeyID, &p.PreviousKeyID, &p.PreviousKeyExpiresAt, &p.Enabled, &p.Revision, &p.CreatedBy, &p.UpdatedBy, &p.CreatedAt, &p.UpdatedAt}
}

func (p *NotificationPolicy) normalize() {
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	if p.PreviousKeyExpiresAt != nil {
		t := p.PreviousKeyExpiresAt.UTC()
		p.PreviousKeyExpiresAt = &t
	}
}

func scanPolicy(row pgx.Row) (NotificationPolicy, error) {
	var p NotificationPolicy
	if err := row.Scan(policyDest(&p)...); err != nil {
		return NotificationPolicy{}, err
	}
	p.normalize()
	return p, nil
}

// policyDetails는 감사 details다. URL은 path·query에 receiver token이 흔하므로 scheme·host만 둔다
// (salt 없는 hash도 두지 않는다 — 추측한 URL을 확인하는 데 쓰일 수 있다).
func policyDetails(revision int64, w PolicyWrite, deleted bool) ([]byte, error) {
	b, err := json.Marshal(map[string]any{"revision": revision, "enabled": w.Enabled, "deleted": deleted, "webhook": w.Display})
	if err != nil {
		return nil, fmt.Errorf("controldb: policy audit details: %w", err)
	}
	return b, nil
}

// CreatePolicy는 policy를 만든다. seal은 새 policy id로 서명 secret을 암호화한다(AAD에 id가 들어간다).
// Idempotency-Key 의미는 CreateMonitor와 같다. respond가 만든 응답(secret 평문 없음)이 key와 함께 저장된다.
func (s *NotificationPolicyStore) CreatePolicy(ctx context.Context, p authz.Principal, w PolicyWrite, seal func(policyID string) (SealedSecret, error),
	idem IdempotencyRequest, requestID string, respond func(NotificationPolicy) (StoredResponse, error)) (StoredResponse, bool, error) {
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return StoredResponse{}, false, err
	}
	if idem.Key == "" || idem.MethodPath == "" {
		return StoredResponse{}, false, errors.New("controldb: idempotency key is required")
	}
	for attempt := 0; ; attempt++ {
		var (
			resp     StoredResponse
			replayed bool
		)
		err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
			stored, found, err := lookupIdempotency(ctx, tx, p, idem)
			if err != nil {
				return err
			}
			if found {
				resp, replayed = stored, true
				return nil
			}
			id, err := newUUID()
			if err != nil {
				return err
			}
			sec, err := seal(id)
			if err != nil {
				return err
			}
			u, err := w.SealURL(id)
			if err != nil {
				return err
			}
			np, err := scanPolicy(tx.QueryRow(ctx, `
				INSERT INTO notification_policies (tenant_id, id, name, webhook_display, webhook_url_sealed, url_sealing_key,
					secret_key_id, secret_ciphertext, secret_sealing_key, enabled, created_by, updated_by)
				VALUES (app_tenant_id(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
				RETURNING `+policyColumns, id, w.Name, w.Display, u.Ciphertext, u.Version, sec.KeyID, sec.Ciphertext, sec.SealingKey, w.Enabled, p.Subject()))
			if err != nil {
				return classify("insert notification policy", err)
			}
			details, err := policyDetails(np.Revision, w, false)
			if err != nil {
				return err
			}
			if err := writeAuditAndOutbox(ctx, tx, p.Tenant(), change{
				action: "notification_policy.created", actorKind: p.Kind().String(), actorID: p.Subject(),
				resourceType: "notification_policy", resourceID: np.ID, revision: np.Revision, requestID: requestID, details: details,
			}); err != nil {
				return err
			}
			if resp, err = respond(np); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO idempotency_keys (tenant_id, principal, method_path, key, request_hash, status_code, response_body, expires_at)
				VALUES (app_tenant_id(), $1, $2, $3, $4, $5, $6, now() + $7::interval)`,
				principalKey(p), idem.MethodPath, idem.Key, idem.RequestHash[:], resp.Status, resp.Body, fmt.Sprintf("%d seconds", int(IdempotencyTTL/time.Second)))
			if isUniqueViolation(err) {
				return errIdempotencyRace
			}
			return classify("insert idempotency key", err)
		})
		if errors.Is(err, errIdempotencyRace) {
			if attempt < maxIdempotencyAttempts-1 {
				continue
			}
			return StoredResponse{}, false, &unavailableError{op: "idempotent create", err: err}
		}
		if err != nil {
			return StoredResponse{}, false, err
		}
		return resp, replayed, nil
	}
}

// GetPolicy는 삭제되지 않은 policy 하나다. 없으면 authz.ErrNotFound(존재를 숨긴다).
func (s *NotificationPolicyStore) GetPolicy(ctx context.Context, p authz.Principal, id string) (NotificationPolicy, error) {
	if err := authz.Authorize(p, authz.MonitorsRead); err != nil {
		return NotificationPolicy{}, err
	}
	var np NotificationPolicy
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		var err error
		np, err = scanPolicy(tx.QueryRow(ctx, `SELECT `+policyColumns+` FROM notification_policies WHERE id = $1 AND deleted_at IS NULL`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return authz.ErrNotFound
		}
		return classify("select notification policy", err)
	})
	return np, err
}

// ListPolicies는 삭제되지 않은 policy를 이름순으로 돌려준다.
func (s *NotificationPolicyStore) ListPolicies(ctx context.Context, p authz.Principal, after *PolicyPosition, limit int) ([]NotificationPolicy, []PolicyPosition, bool, error) {
	if err := authz.Authorize(p, authz.MonitorsRead); err != nil {
		return nil, nil, false, err
	}
	if limit < 1 || limit > MaxPolicyPage {
		return nil, nil, false, fmt.Errorf("controldb: policy page limit must be within [1, %d]", MaxPolicyPage)
	}
	sql := `SELECT ` + policyColumns + `, name_normalized FROM notification_policies WHERE tenant_id = app_tenant_id() AND deleted_at IS NULL`
	args := []any{limit + 1}
	if after != nil {
		sql += ` AND (name_normalized, id) > ($2, $3::uuid)`
		args = append(args, after.NameNormalized, after.ID)
	}
	sql += ` ORDER BY name_normalized, id LIMIT $1`
	var (
		out []NotificationPolicy
		pos []PolicyPosition
	)
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return classify("list notification policies", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				np NotificationPolicy
				nn string
			)
			if err := rows.Scan(append(policyDest(&np), &nn)...); err != nil {
				return classify("scan notification policy", err)
			}
			np.normalize()
			out = append(out, np)
			pos = append(pos, PolicyPosition{NameNormalized: nn, ID: np.ID})
		}
		return classify("list notification policies", rows.Err())
	})
	if err != nil {
		return nil, nil, false, err
	}
	more := len(out) > limit
	if more {
		out, pos = out[:limit], pos[:limit]
	}
	return out, pos, more, nil
}

// UpdatePolicy는 ifMatch revision이 맞을 때만 이름·URL·enabled를 바꾼다. secret은 그대로다.
// 이미 쌓인 알림은 보낼 때의 policy(URL·enabled)를 쓴다.
func (s *NotificationPolicyStore) UpdatePolicy(ctx context.Context, p authz.Principal, id string, ifMatch int64, w PolicyWrite, requestID string) (NotificationPolicy, error) {
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return NotificationPolicy{}, err
	}
	u, err := w.SealURL(id)
	if err != nil {
		return NotificationPolicy{}, err
	}
	var np NotificationPolicy
	err = s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		var err error
		np, err = scanPolicy(tx.QueryRow(ctx, `
			UPDATE notification_policies SET name = $3, webhook_display = $4, webhook_url_sealed = $5, url_sealing_key = $6, enabled = $7,
				revision = revision + 1, updated_by = $8, updated_at = now()
			WHERE id = $1 AND revision = $2 AND deleted_at IS NULL
			RETURNING `+policyColumns, id, ifMatch, w.Name, w.Display, u.Ciphertext, u.Version, w.Enabled, p.Subject()))
		if errors.Is(err, pgx.ErrNoRows) {
			return policyMissOrMismatch(ctx, tx, id)
		}
		if err != nil {
			return classify("update notification policy", err)
		}
		details, err := policyDetails(np.Revision, w, false)
		if err != nil {
			return err
		}
		return writeAuditAndOutbox(ctx, tx, p.Tenant(), change{
			action: "notification_policy.updated", actorKind: p.Kind().String(), actorID: p.Subject(),
			resourceType: "notification_policy", resourceID: np.ID, revision: np.Revision, requestID: requestID, details: details,
		})
	})
	return np, err
}

// DeletePolicy는 tombstone을 남긴다. 보내지 않은 알림은 보낼 때 취소된다(canceled).
// 살아 있는 monitor가 가리키면 ErrPolicyInUse다 — 지우면 그 monitor의 알림이 기록 없이 끊긴다. 먼저 monitor에서 떼야 한다.
// 행을 먼저 잠그므로(UPDATE) 같은 policy를 가리키는 monitor 저장(FOR SHARE)과 직렬화된다.
func (s *NotificationPolicyStore) DeletePolicy(ctx context.Context, p authz.Principal, id string, ifMatch *int64, requestID string) (int64, error) {
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return 0, err
	}
	var revision int64
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		var w PolicyWrite
		err := tx.QueryRow(ctx, `
			UPDATE notification_policies SET deleted_at = now(), revision = revision + 1, updated_by = $3, updated_at = now()
			WHERE id = $1 AND ($2::bigint IS NULL OR revision = $2) AND deleted_at IS NULL
			RETURNING revision, name, webhook_display, enabled`, id, ifMatch, p.Subject()).Scan(&revision, &w.Name, &w.Display, &w.Enabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return policyMissOrMismatch(ctx, tx, id)
		}
		if err != nil {
			return classify("delete notification policy", err)
		}
		var inUse bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM monitors WHERE notification_policy_id = $1 AND deleted_at IS NULL)`, id).Scan(&inUse); err != nil {
			return classify("select monitors using policy", err)
		}
		if inUse {
			return ErrPolicyInUse // rollback: tombstone도 남지 않는다
		}
		details, err := policyDetails(revision, w, true)
		if err != nil {
			return err
		}
		return writeAuditAndOutbox(ctx, tx, p.Tenant(), change{
			action: "notification_policy.deleted", actorKind: p.Kind().String(), actorID: p.Subject(),
			resourceType: "notification_policy", resourceID: id, revision: revision, requestID: requestID, details: details,
		})
	})
	return revision, err
}

func policyMissOrMismatch(ctx context.Context, tx pgx.Tx, id string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM notification_policies WHERE id = $1 AND deleted_at IS NULL)`, id).Scan(&exists); err != nil {
		return classify("select notification policy", err)
	}
	if exists {
		return ErrRevisionMismatch
	}
	return authz.ErrNotFound
}

// policyAlive는 policy가 이 tenant에 살아 있는지 보고, 트랜잭션 끝까지 그 행을 공유 잠금한다.
// 동시에 지우는 트랜잭션(DeletePolicy의 UPDATE)과 직렬화되어 "확인한 뒤 지워진 policy"를 가리키는 일이 없다.
func policyAlive(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM notification_policies WHERE id = $1 AND deleted_at IS NULL FOR SHARE`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, classify("select notification policy", err)
	}
	return true, nil
}

// checkPolicy는 monitor가 가리킨 policy가 이 tenant에 살아 있는지 본다(monitor 저장과 같은 트랜잭션).
func checkPolicy(ctx context.Context, tx pgx.Tx, id *string) error {
	if id == nil {
		return nil
	}
	alive, err := policyAlive(ctx, tx, *id)
	if err != nil {
		return err
	}
	if !alive {
		return ErrNotificationPolicyNotFound
	}
	return nil
}

// notice는 쌓을 알림 하나의 내용이다.
type notice struct {
	monitorID, monitorName string
	revision               int64
	groupHash              string
	labels                 map[string]string
	episodeID              string
	episodeReason          *string
	state                  *string
	value                  *float64
	reason                 *string
	windowEnd              *time.Time
	closeReason            *string
	at                     time.Time
	canceled               string // 비어 있지 않으면 보내지 않고 취소로 쌓는다(사유)
	sequence               int    // 재알림 번호(1..3). 열림·닫힘은 0
	openedAt               time.Time
	links                  notify.WebLinks
}

// 알림 deep link의 시간 범위: 사건이 열리기 30분 전부터 이 알림 15분 뒤까지(절대 시각, 언제 열어도 같은 구간, D05 §09)
const (
	linkLead  = 30 * time.Minute
	linkTrail = 15 * time.Minute
)

// enqueueOpened는 사건이 열린 알림을 쌓는다. 같은 사건은 한 번만(UNIQUE, D02 §17).
// policy는 지울 때 가리키는 monitor가 없어야 하므로(ErrPolicyInUse) 보통 살아 있다. 정의가 낡은 revision을 평가하는 동안
// 지워진 경우에도 조용히 버리지 않고 취소된 알림(policy_deleted)으로 남긴다(지표·RB04로 보인다).
func enqueueOpened(ctx context.Context, tx pgx.Tx, tenant authz.TenantID, policyID string, n notice) error {
	alive, err := policyAlive(ctx, tx, policyID)
	if err != nil {
		return err
	}
	if !alive {
		n.canceled = "policy_deleted"
	}
	return insertDelivery(ctx, tx, tenant, policyID, notify.EventOpened, n)
}

// enqueueResolved는 사건이 닫힌 알림을 쌓는다. 열림 알림을 받은 policy에만 보낸다(열림을 못 받은 곳에 닫힘만 가지 않는다).
func enqueueResolved(ctx context.Context, tx pgx.Tx, tenant authz.TenantID, n notice) error {
	rows, err := tx.Query(ctx, `
		SELECT policy_id::text, min(created_at) FROM notification_deliveries
		WHERE monitor_id = $1 AND group_hash = $2 AND episode_id = $3 AND event_type = 'alert.opened'
		GROUP BY policy_id`, n.monitorID, n.groupHash, n.episodeID)
	if err != nil {
		return classify("select opened notifications", err)
	}
	type opened struct {
		policy string
		at     time.Time
	}
	var policies []opened
	for rows.Next() {
		var o opened
		if err := rows.Scan(&o.policy, &o.at); err != nil {
			rows.Close()
			return classify("scan opened notification", err)
		}
		policies = append(policies, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return classify("select opened notifications", err)
	}
	for _, o := range policies {
		m := n
		m.openedAt = o.at
		if err := insertDelivery(ctx, tx, tenant, o.policy, notify.EventResolved, m); err != nil {
			return err
		}
	}
	return nil
}

// reminder는 재알림 후보다: 이번 평가에서 ALERT로 남은 열린 사건(이번에 새로 열린 것은 빼고).
type reminder struct {
	n notice
}

// enqueueReminders는 열린 사건이 ALERT로 남아 있으면 30분마다 최대 3번 다시 알린다(D02 §17, ADR 0055).
// 열림 알림을 받은 policy에만 보낸다(취소된 열림은 빼고). k번째 재알림은 열림 알림을 쌓은 지 30분×k가 지났고 마지막 알림(열림 또는
// 직전 재알림)에서도 30분이 지났을 때 하나만 쌓는다(저장 종류에 번호가 있다(alert.renotified.k) — 재평가·중복 scheduler에도 UNIQUE로 하나).
// 번호는 차례로만 쌓이므로 쌓인 수가 마지막 번호다. RECOVERING·NO_DATA·EVALUATION_ERROR인 동안은 쌓지 않고, ALERT로 돌아오면
// 다음 번호를 보낸다 — 마지막 알림에서 30분 간격이므로 몰아서 여러 번 보내지 않는다. 3번 뒤 escalation은 F11(G1)이다.
func enqueueReminders(ctx context.Context, tx pgx.Tx, tenant authz.TenantID, monitorID string, cands []reminder) error {
	if len(cands) == 0 {
		return nil
	}
	byEpisode := make(map[string]notice, len(cands))
	episodes := make([]string, 0, len(cands))
	for _, c := range cands {
		byEpisode[c.n.groupHash+"|"+c.n.episodeID] = c.n
		episodes = append(episodes, c.n.episodeID)
	}
	rows, err := tx.Query(ctx, `
		SELECT o.group_hash, o.episode_id::text, o.policy_id::text, o.created_at, now(), r.sent, coalesce(r.last_at, o.created_at)
		FROM notification_deliveries o
		CROSS JOIN LATERAL (
			SELECT count(*) AS sent, max(created_at) AS last_at FROM notification_deliveries r
			WHERE r.policy_id = o.policy_id AND r.monitor_id = o.monitor_id AND r.group_hash = o.group_hash
			  AND r.episode_id = o.episode_id AND r.event_type LIKE 'alert.renotified.%') r
		WHERE o.monitor_id = $1 AND o.event_type = 'alert.opened' AND o.status <> 'canceled' AND o.episode_id = ANY($2::uuid[])`,
		monitorID, episodes)
	if err != nil {
		return classify("select renotify candidates", err)
	}
	type due struct {
		policy string
		n      notice
	}
	var todo []due
	for rows.Next() {
		var (
			hash, episode, policy string
			openedAt, dbNow       time.Time
			lastAt                time.Time
			sent                  int
		)
		if err := rows.Scan(&hash, &episode, &policy, &openedAt, &dbNow, &sent, &lastAt); err != nil {
			rows.Close()
			return classify("scan renotify candidate", err)
		}
		n, ok := byEpisode[hash+"|"+episode]
		next := sent + 1
		if !ok || next > notify.RenotifyMax || dbNow.Before(openedAt.Add(time.Duration(next)*notify.RenotifyInterval)) ||
			dbNow.Before(lastAt.Add(notify.RenotifyInterval)) {
			continue
		}
		n.sequence, n.openedAt = next, openedAt
		todo = append(todo, due{policy: policy, n: n})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return classify("select renotify candidates", err)
	}
	for _, d := range todo {
		if err := insertDelivery(ctx, tx, tenant, d.policy, notify.RenotifyKind(d.n.sequence), d.n); err != nil {
			return err
		}
	}
	return nil
}

// insertDelivery는 알림 하나를 쌓는다. kind는 저장 종류(중복 억제 key)이고, webhook event_type은 재알림이면 alert.renotified다.
func insertDelivery(ctx context.Context, tx pgx.Tx, tenant authz.TenantID, policyID, kind string, n notice) error {
	eventType := kind
	if _, ok := notify.ParseRenotifyKind(kind); ok {
		eventType = notify.EventRenotified
	}
	id, err := newUUID()
	if err != nil {
		return err
	}
	opened := n.openedAt
	if opened.IsZero() {
		opened = n.at // 열림 알림 자신
	}
	payload := notify.AlertPayload{MonitorID: n.monitorID, MonitorName: n.monitorName, GroupHash: n.groupHash, Labels: n.labels,
		EpisodeID: n.episodeID, EpisodeReason: n.episodeReason, State: n.state, Value: n.value, Reason: n.reason, WindowEnd: n.windowEnd,
		CloseReason: n.closeReason, Links: n.links.Monitor(tenant.String(), n.monitorID, opened.Add(-linkLead), n.at.Add(linkTrail))}
	if n.sequence > 0 {
		seq := n.sequence
		payload.RenotifySequence = &seq
	}
	ev := notify.Event{
		EventID: id, EventType: eventType, SchemaVersion: 1, TenantPublicID: tenant.String(), OccurredAt: n.at.UTC(),
		ResourceID: n.monitorID, Revision: n.revision, Payload: payload,
	}
	body, err := ev.Marshal()
	if err != nil {
		// labels가 커서 본문 상한을 넘으면 labels를 빼고 보낸다(group_hash로 찾는다). 경보 저장을 막지 않는다
		payload.Labels, payload.LabelsTruncated = nil, true
		ev.Payload = payload
		if body, err = ev.Marshal(); err != nil {
			return err
		}
	}
	status, lastError := "pending", (*string)(nil)
	if n.canceled != "" {
		status, lastError = "canceled", &n.canceled
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO notification_deliveries (tenant_id, id, policy_id, monitor_id, group_hash, episode_id, event_type, status, last_error,
			next_attempt_at, created_at, finished_at)
		VALUES (app_tenant_id(), $1, $2, $3, $4, $5, $6, $7, $8, now(), now(), CASE WHEN $7 = 'pending' THEN NULL ELSE now() END)
		ON CONFLICT (tenant_id, policy_id, monitor_id, group_hash, episode_id, event_type) DO NOTHING`,
		id, policyID, n.monitorID, n.groupHash, n.episodeID, kind, status, lastError)
	if err != nil {
		return classify("insert notification delivery", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // 같은 사건의 알림이 이미 있다
	}
	if _, err := tx.Exec(ctx, `INSERT INTO notification_delivery_bodies (tenant_id, delivery_id, body) VALUES (app_tenant_id(), $1, $2)`, id, body); err != nil {
		return classify("insert notification body", err)
	}
	return nil
}

// DueDelivery는 보낼 차례인 알림이다. PolicyID로 한 목적지가 tick을 독차지하지 않게 나눈다.
type DueDelivery struct {
	Tenant   authz.TenantID
	ID       string
	PolicyID string
}

// Delivery는 lease를 잡은 알림 하나와 보낼 곳이다.
type Delivery struct {
	Tenant    authz.TenantID
	ID        string
	Token     string
	PolicyID  string
	EventType string // 시험 전송(notify.EventTest)은 다시 보내지 않고, 꺼진 policy에도 보낸다
	Body      []byte
	Attempts  int // 이번 시도를 포함한 시도 수(claim에서 늘린다)
	CreatedAt time.Time
	// 보낼 때의 policy(암호문)
	URLSealed     []byte
	URLSealingKey string
	SecretKeyID   string
	Sealed        []byte
	SealingKey    string
	// 이전 secret(교체 뒤 겹침 기간 안일 때만). 함께 서명한다
	PreviousKeyID      *string
	PreviousSealed     []byte
	PreviousSealingKey *string
	PolicyDisabled     bool // 꺼졌거나 지워졌다 → 경보 알림 취소
	PolicyDeleted      bool // 지워졌다 → 시험 전송도 취소
}

// DeliveryResult는 시도 하나의 결과다. Status: delivered | pending(재시도) | failed(포기, DLQ) | canceled.
type DeliveryResult struct {
	Status        string
	NextAttemptAt time.Time
	StatusCode    int
	Error         string
}

// DeliveryStore는 전송 worker 저장소다.
type DeliveryStore struct{ db *DB }

// NewDeliveryStore는 저장소를 만든다.
func NewDeliveryStore(db *DB) *DeliveryStore { return &DeliveryStore{db: db} }

// DueDeliveries는 보낼 차례이고 lease가 없거나 만료된 알림이다(오래 기다린 것부터). tenant를 가로지르고 tenant·id·policy만 읽는다.
// policy 하나는 perPolicy개까지만 고른다 — 한 policy에 알림이 몰려도(큰 monitor가 한꺼번에 열림) 다른 tenant의 알림이 목록에서 밀려나지 않는다.
// oldestUnattempted는 한 번도 시도하지 않은 알림 중 가장 오래된 것의 생성 시각이다(없으면 zero) — 전송 지연 경보에 쓴다.
func (s *DeliveryStore) DueDeliveries(ctx context.Context, now time.Time, limit, perPolicy int) (due []DueDelivery, oldestUnattempted time.Time, err error) {
	if limit < 1 || limit > 1000 {
		return nil, time.Time{}, errors.New("controldb: due limit must be within [1, 1000]")
	}
	err = s.db.inTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.delivery_scan', 'on', true)`); err != nil {
			return classify("set delivery scan", err)
		}
		var oldest *time.Time
		if err := tx.QueryRow(ctx, `SELECT min(created_at) FROM notification_deliveries WHERE status = 'pending' AND attempts = 0`).Scan(&oldest); err != nil {
			return classify("oldest delivery", err)
		}
		if oldest != nil {
			oldestUnattempted = oldest.UTC()
		}
		rows, err := tx.Query(ctx, `
			SELECT tenant_id::text, id::text, policy_id::text FROM (
				SELECT tenant_id, id, policy_id, next_attempt_at,
				       row_number() OVER (PARTITION BY tenant_id, policy_id ORDER BY next_attempt_at, id) AS rn
				FROM notification_deliveries
				WHERE status = 'pending' AND next_attempt_at <= $1 AND (lease_until IS NULL OR lease_until < $1)
			) due WHERE rn <= $3
			ORDER BY next_attempt_at LIMIT $2`, now, limit, max(perPolicy, 1))
		if err != nil {
			return classify("due deliveries", err)
		}
		defer rows.Close()
		for rows.Next() {
			var tenant, id, policy string
			if err := rows.Scan(&tenant, &id, &policy); err != nil {
				return classify("scan due delivery", err)
			}
			t, err := authz.ParseTenantID(tenant)
			if err != nil {
				return fmt.Errorf("controldb: stored tenant id: %w", err)
			}
			due = append(due, DueDelivery{Tenant: t, ID: id, PolicyID: policy})
		}
		return classify("due deliveries", rows.Err())
	})
	return due, oldestUnattempted, err
}

// ClaimDelivery는 알림 하나의 lease를 잡고 보낼 곳을 읽는다. 다른 worker가 잡았거나 이미 끝났으면 ok=false.
func (s *DeliveryStore) ClaimDelivery(ctx context.Context, tenant authz.TenantID, id string, now time.Time, lease time.Duration) (Delivery, bool, error) {
	token, err := newUUID()
	if err != nil {
		return Delivery{}, false, err
	}
	d := Delivery{Tenant: tenant, ID: id, Token: token}
	ok := false
	err = s.db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		// 시도 수는 잡을 때 늘린다: 보낸 뒤 결과를 쓰기 전에 죽어도 다음 시도의 번호가 커져 receiver가 재전송임을 안다
		err := tx.QueryRow(ctx, `
			UPDATE notification_deliveries SET lease_token = $3, lease_until = $2::timestamptz + $4::interval, attempts = attempts + 1
			WHERE id = $1 AND status = 'pending' AND next_attempt_at <= $2 AND (lease_until IS NULL OR lease_until < $2)
			RETURNING policy_id::text, event_type, attempts, created_at`,
			id, now, token, fmt.Sprintf("%d milliseconds", lease.Milliseconds())).Scan(&d.PolicyID, &d.EventType, &d.Attempts, &d.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return classify("claim delivery", err)
		}
		d.CreatedAt = d.CreatedAt.UTC()
		if err := tx.QueryRow(ctx, `SELECT body FROM notification_delivery_bodies WHERE delivery_id = $1`, id).Scan(&d.Body); err != nil {
			return classify("select notification body", err)
		}
		if err := tx.QueryRow(ctx, `
			SELECT webhook_url_sealed, url_sealing_key, secret_key_id, secret_ciphertext, secret_sealing_key,
			       CASE WHEN previous_secret_expires_at > $2 THEN previous_secret_key_id END,
			       CASE WHEN previous_secret_expires_at > $2 THEN previous_secret_ciphertext END,
			       CASE WHEN previous_secret_expires_at > $2 THEN previous_secret_sealing_key END,
			       NOT enabled OR deleted_at IS NOT NULL, deleted_at IS NOT NULL
			FROM notification_policies WHERE id = $1`, d.PolicyID, now).Scan(&d.URLSealed, &d.URLSealingKey, &d.SecretKeyID, &d.Sealed, &d.SealingKey,
			&d.PreviousKeyID, &d.PreviousSealed, &d.PreviousSealingKey, &d.PolicyDisabled, &d.PolicyDeleted); err != nil {
			return classify("select notification policy", err)
		}
		ok = true
		return nil
	})
	return d, ok && err == nil, err
}

// CompleteDelivery는 시도 결과를 쓴다. lease를 잃었으면 ErrLeaseLost이고 쓰지 않는다.
func (s *DeliveryStore) CompleteDelivery(ctx context.Context, d Delivery, r DeliveryResult) error {
	switch r.Status {
	case "delivered", "pending", "failed", "canceled":
	default:
		return fmt.Errorf("controldb: unknown delivery status %q", r.Status)
	}
	var (
		code   *int
		reason *string
	)
	if r.StatusCode != 0 {
		code = &r.StatusCode
	}
	if r.Error != "" {
		reason = &r.Error
	}
	next := r.NextAttemptAt
	if r.Status != "pending" || next.IsZero() {
		next = time.Now()
	}
	return s.db.WithTenant(ctx, d.Tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE notification_deliveries SET status = $3, next_attempt_at = $4, lease_token = NULL, lease_until = NULL,
				last_status_code = coalesce($5, last_status_code), last_error = $6,
				finished_at = CASE WHEN $3 = 'pending' THEN NULL ELSE now() END
			WHERE id = $1 AND lease_token = $2`, d.ID, d.Token, r.Status, next, code, reason)
		if err != nil {
			return classify("complete delivery", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		return nil
	})
}

// PruneFinished는 보존 기간(30일)이 지난 끝난 알림을 지운다(본문은 CASCADE). 아직 열린 사건의 열림 기록은 남긴다
// (닫힘 알림을 어디로 보낼지 정한다). 지울 것이 있는 tenant를 after 다음부터 maxTenants개 찾아(scan, tenant id만)
// tenant 트랜잭션에서 지운다. 지운 수와 다음 cursor를 돌려준다(끝까지 돌았으면 "" — 다음 호출은 처음부터).
// 열린 사건 때문에 매번 대상이 되는 tenant가 있어도 cursor로 넘어가므로 다른 tenant가 굶지 않는다.
func (s *DeliveryStore) PruneFinished(ctx context.Context, now time.Time, after string, maxTenants int) (int64, string, error) {
	cutoff := now.Add(-DeliveryRetention)
	var tenants []authz.TenantID
	err := s.db.inTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.delivery_scan', 'on', true)`); err != nil {
			return classify("set delivery scan", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT tenant_id::text FROM notification_deliveries
			WHERE finished_at < $1 AND ($3 = '' OR tenant_id > $3::uuid)
			ORDER BY 1 LIMIT $2`, cutoff, maxTenants, after)
		if err != nil {
			return classify("prunable tenants", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return classify("scan prunable tenant", err)
			}
			t, err := authz.ParseTenantID(id)
			if err != nil {
				return fmt.Errorf("controldb: stored tenant id: %w", err)
			}
			tenants = append(tenants, t)
		}
		return classify("prunable tenants", rows.Err())
	})
	if err != nil {
		return 0, after, err
	}
	next := ""
	if len(tenants) == maxTenants {
		next = tenants[len(tenants)-1].String()
	}
	var total int64
	for _, t := range tenants {
		err := s.db.WithTenant(ctx, t, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				DELETE FROM notification_deliveries d WHERE finished_at < $1
				  AND NOT EXISTS (SELECT 1 FROM alert_instances ai WHERE ai.monitor_id = d.monitor_id AND ai.episode_id = d.episode_id)`, cutoff)
			total += tag.RowsAffected()
			return classify("prune deliveries", err)
		})
		if err != nil {
			return total, after, err
		}
	}
	return total, next, nil
}
