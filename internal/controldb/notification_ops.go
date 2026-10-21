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

// notification policy의 시험 전송과 서명 secret 교체 (D05 §09, D02 §20, ADR 0055).
//   - 시험 전송은 실제 경보와 다른 종류(notification.test)의 알림 행이다. 보내는 일은 경보 알림과 같은 worker가 한다
//     (SSRF 방어·서명·secret 복호화를 한 곳에만 둔다). 다시 보내지 않는다(결과를 바로 보여 준다).
//   - 교체는 새 secret을 지금 secret으로 두고 이전 secret을 정한 시각까지 함께 서명한다(겹침 기간, 최대 24시간).

// ErrTestRateLimited는 policy 하나의 시험 전송이 시간당 상한을 넘었다는 뜻이다(429).
var ErrTestRateLimited = errors.New("controldb: notification test rate limited")

// 시험 전송 한도: policy 하나에 1시간 동안 20번. 진행 중인 시험이 있으면 새로 만들지 않고 그것을 돌려준다.
const (
	MaxTestsPerHour = 20
	// MaxSecretOverlap은 secret 교체 뒤 이전 secret을 함께 서명하는 최대 기간이다(D04 §02 key 교체 겹침 24시간과 같다).
	MaxSecretOverlap = 24 * time.Hour
)

// TestDelivery는 시험 전송 하나의 상태다. 결과 code·오류 code만 있다(응답 본문은 저장하지 않는다).
type TestDelivery struct {
	ID             string
	PolicyID       string
	Status         string // pending | delivered | failed | canceled
	Attempts       int
	LastStatusCode *int
	LastError      *string
	CreatedAt      time.Time
	FinishedAt     *time.Time
}

const testColumns = `id::text, policy_id::text, status, attempts, last_status_code, last_error, created_at, finished_at`

func scanTest(row pgx.Row) (TestDelivery, error) {
	var t TestDelivery
	if err := row.Scan(&t.ID, &t.PolicyID, &t.Status, &t.Attempts, &t.LastStatusCode, &t.LastError, &t.CreatedAt, &t.FinishedAt); err != nil {
		return TestDelivery{}, err
	}
	t.CreatedAt = t.CreatedAt.UTC()
	if t.FinishedAt != nil {
		f := t.FinishedAt.UTC()
		t.FinishedAt = &f
	}
	return t, nil
}

// SendTest는 policy에 시험 전송을 쌓는다(꺼진 policy에도 보낸다 — 켜기 전에 확인하는 용도다).
// 진행 중인 시험이 있으면 그것을 돌려준다(reused=true). 1시간에 MaxTestsPerHour를 넘으면 ErrTestRateLimited다.
// 받는 곳으로 데이터가 나가는 동작이므로 security 감사를 남긴다.
func (s *NotificationPolicyStore) SendTest(ctx context.Context, p authz.Principal, id, requestID string) (TestDelivery, bool, error) {
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return TestDelivery{}, false, err
	}
	var (
		out    TestDelivery
		reused bool
	)
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		var err error
		out, reused, err = sendTestTx(ctx, tx, p, id, requestID)
		return err
	})
	return out, reused, err
}

// SendTestIdempotent는 SendTest를 Idempotency-Key 아래에서 한다(D02 §20, ADR 0060 §1): 같은 key 재요청은 저장된 응답을
// 그대로 돌려주고(replayed=true) 새 시험을 쌓지 않는다. respond는 같은 트랜잭션에서 저장할 응답을 만든다.
func (s *NotificationPolicyStore) SendTestIdempotent(ctx context.Context, p authz.Principal, id, requestID string, idem IdempotencyRequest,
	respond func(t TestDelivery, reused bool) (StoredResponse, error)) (StoredResponse, bool, error) {
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return StoredResponse{}, false, err
	}
	return runIdempotent(ctx, s.db, p, idem, "idempotent notification test", func(tx pgx.Tx) (StoredResponse, error) {
		t, reused, err := sendTestTx(ctx, tx, p, id, requestID)
		if err != nil {
			return StoredResponse{}, err
		}
		return respond(t, reused)
	})
}

// sendTestTx는 SendTest의 트랜잭션 본문이다.
func sendTestTx(ctx context.Context, tx pgx.Tx, p authz.Principal, id, requestID string) (TestDelivery, bool, error) {
	var (
		out    TestDelivery
		reused bool
	)
	err := func() error {
		// 행을 잠가 같은 policy의 동시 시험 요청을 줄 세운다(진행 중 확인과 한도 확인이 경합하지 않게)
		var (
			name     string
			revision int64
		)
		err := tx.QueryRow(ctx, `SELECT name, revision FROM notification_policies WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&name, &revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return authz.ErrNotFound
		}
		if err != nil {
			return classify("lock notification policy", err)
		}
		out, err = scanTest(tx.QueryRow(ctx, `SELECT `+testColumns+` FROM notification_deliveries
			WHERE policy_id = $1 AND event_type = 'notification.test' AND status = 'pending' ORDER BY created_at DESC LIMIT 1`, id))
		if err == nil {
			reused = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return classify("select pending test", err)
		}
		var recent int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries
			WHERE policy_id = $1 AND event_type = 'notification.test' AND created_at > now() - interval '1 hour'`, id).Scan(&recent); err != nil {
			return classify("count recent tests", err)
		}
		if recent >= MaxTestsPerHour {
			return ErrTestRateLimited
		}
		deliveryID, err := newUUID()
		if err != nil {
			return err
		}
		ev := notify.Event{EventID: deliveryID, EventType: notify.EventTest, SchemaVersion: 1, TenantPublicID: p.Tenant().String(),
			OccurredAt: time.Now().UTC(), ResourceID: id, Revision: revision,
			Payload: notify.TestPayload{Test: true, PolicyID: id, PolicyName: name,
				Message: "Montracer 시험 전송입니다. 실제 경보가 아닙니다. This is a test notification, not an alert."}}
		body, err := ev.Marshal()
		if err != nil {
			return err
		}
		out, err = scanTest(tx.QueryRow(ctx, `
			INSERT INTO notification_deliveries (tenant_id, id, policy_id, event_type, status, next_attempt_at, created_at)
			VALUES (app_tenant_id(), $1, $2, 'notification.test', 'pending', now(), now())
			RETURNING `+testColumns, deliveryID, id))
		if err != nil {
			return classify("insert notification test", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO notification_delivery_bodies (tenant_id, delivery_id, body) VALUES (app_tenant_id(), $1, $2)`, deliveryID, body); err != nil {
			return classify("insert notification test body", err)
		}
		details, err := json.Marshal(map[string]any{"revision": revision, "delivery_id": deliveryID})
		if err != nil {
			return fmt.Errorf("controldb: test audit details: %w", err)
		}
		return writeAudit(ctx, tx, p.Tenant(), change{
			action: "notification_policy.tested", actorKind: p.Kind().String(), actorID: p.Subject(),
			resourceType: "notification_policy", resourceID: id, revision: revision, requestID: requestID, details: details,
		})
	}()
	return out, reused, err
}

// GetTest는 시험 전송 하나의 상태다. 보낸 사람과 같은 권한(policies.write)이 본다 — 받는 곳의 응답 code가 담겨 있다.
func (s *NotificationPolicyStore) GetTest(ctx context.Context, p authz.Principal, policyID, deliveryID string) (TestDelivery, error) {
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return TestDelivery{}, err
	}
	var out TestDelivery
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		var err error
		out, err = scanTest(tx.QueryRow(ctx, `SELECT `+testColumns+` FROM notification_deliveries
			WHERE id = $1 AND policy_id = $2 AND event_type = 'notification.test'`, deliveryID, policyID))
		if errors.Is(err, pgx.ErrNoRows) {
			return authz.ErrNotFound
		}
		return classify("select notification test", err)
	})
	return out, err
}

// RotateSecret은 ifMatch revision이 맞을 때 서명 secret을 새것으로 바꾼다. seal은 새 secret을 policy id에 묶어 암호화한다.
// overlap이 0보다 크면 지금 secret을 이전 secret으로 옮겨 그 기간 동안 함께 서명하고(receiver가 새 secret을 배포할 시간),
// 0이면 이전 secret 없이 바로 바꾼다(유출 대응).
// 겹침 중(이전 secret이 아직 유효)에 다시 교체하면 이전 secret과 그 만료는 그대로 두고 지금 secret만 바꾼다:
// 응답을 잃어 다시 교체해도 receiver가 가진 secret(교체 전)이 바로 끊기지 않는다. 그 사이의 secret은 버려진다.
// 감사는 security이고 secret 내용은 남기지 않는다(key id만).
func (s *NotificationPolicyStore) RotateSecret(ctx context.Context, p authz.Principal, id string, ifMatch int64, overlap time.Duration,
	seal func(policyID string) (SealedSecret, error), requestID string) (NotificationPolicy, error) {
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return NotificationPolicy{}, err
	}
	if overlap < 0 || overlap > MaxSecretOverlap {
		return NotificationPolicy{}, fmt.Errorf("controldb: secret overlap must be within [0, %s]", MaxSecretOverlap)
	}
	sec, err := seal(id)
	if err != nil {
		return NotificationPolicy{}, err
	}
	var np NotificationPolicy
	err = s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		var err error
		np, err = rotateSecretTx(ctx, tx, p, id, ifMatch, overlap, sec, requestID)
		return err
	})
	return np, err
}

// RotateSecretIdempotent는 RotateSecret을 Idempotency-Key 아래에서 한다(D02 §20, ADR 0060 §1). 같은 key 재요청은 저장된 응답을
// 돌려준다 — 저장 응답에는 새 secret이 없다(respond가 뺀다). 응답을 잃은 client는 다시 교체해야 한다(policy 생성과 같다).
func (s *NotificationPolicyStore) RotateSecretIdempotent(ctx context.Context, p authz.Principal, id string, ifMatch int64, overlap time.Duration,
	seal func(policyID string) (SealedSecret, error), requestID string, idem IdempotencyRequest,
	respond func(NotificationPolicy) (StoredResponse, error)) (StoredResponse, bool, error) {
	if err := authz.AuthorizeTenantWide(p, authz.PoliciesWrite); err != nil {
		return StoredResponse{}, false, err
	}
	if overlap < 0 || overlap > MaxSecretOverlap {
		return StoredResponse{}, false, fmt.Errorf("controldb: secret overlap must be within [0, %s]", MaxSecretOverlap)
	}
	sec, err := seal(id)
	if err != nil {
		return StoredResponse{}, false, err
	}
	return runIdempotent(ctx, s.db, p, idem, "idempotent secret rotation", func(tx pgx.Tx) (StoredResponse, error) {
		np, err := rotateSecretTx(ctx, tx, p, id, ifMatch, overlap, sec, requestID)
		if err != nil {
			return StoredResponse{}, err
		}
		return respond(np)
	})
}

// rotateSecretTx는 RotateSecret의 트랜잭션 본문이다.
func rotateSecretTx(ctx context.Context, tx pgx.Tx, p authz.Principal, id string, ifMatch int64, overlap time.Duration,
	sec SealedSecret, requestID string) (NotificationPolicy, error) {
	var np NotificationPolicy
	err := func() error {
		var oldKeyID string
		if err := tx.QueryRow(ctx, `SELECT secret_key_id FROM notification_policies WHERE id = $1 AND revision = $2 AND deleted_at IS NULL FOR UPDATE`,
			id, ifMatch).Scan(&oldKeyID); errors.Is(err, pgx.ErrNoRows) {
			return policyMissOrMismatch(ctx, tx, id)
		} else if err != nil {
			return classify("lock notification policy", err)
		}
		// SET의 오른쪽은 바꾸기 전 값이다: 지금 secret을 이전 secret 열로 옮기고 새 secret을 넣는다
		var err error
		np, err = scanPolicy(tx.QueryRow(ctx, `
			UPDATE notification_policies SET
				previous_secret_key_id      = CASE WHEN $4 > 0 AND previous_secret_expires_at > now() THEN previous_secret_key_id
				                                   WHEN $4 > 0 THEN secret_key_id END,
				previous_secret_ciphertext  = CASE WHEN $4 > 0 AND previous_secret_expires_at > now() THEN previous_secret_ciphertext
				                                   WHEN $4 > 0 THEN secret_ciphertext END,
				previous_secret_sealing_key = CASE WHEN $4 > 0 AND previous_secret_expires_at > now() THEN previous_secret_sealing_key
				                                   WHEN $4 > 0 THEN secret_sealing_key END,
				previous_secret_expires_at  = CASE WHEN $4 > 0 AND previous_secret_expires_at > now() THEN previous_secret_expires_at
				                                   WHEN $4 > 0 THEN now() + $4 * interval '1 millisecond' END,
				secret_key_id = $5, secret_ciphertext = $6, secret_sealing_key = $7,
				revision = revision + 1, updated_by = $3, updated_at = now()
			WHERE id = $1 AND revision = $2 AND deleted_at IS NULL
			RETURNING `+policyColumns, id, ifMatch, p.Subject(), overlap.Milliseconds(), sec.KeyID, sec.Ciphertext, sec.SealingKey))
		if err != nil {
			return classify("rotate notification secret", err)
		}
		details, err := json.Marshal(map[string]any{"revision": np.Revision, "signing_key_id": np.SecretKeyID,
			"retired_key_id": oldKeyID, "previous_expires_at": np.PreviousKeyExpiresAt})
		if err != nil {
			return fmt.Errorf("controldb: rotate audit details: %w", err)
		}
		return writeAuditAndOutbox(ctx, tx, p.Tenant(), change{
			action: "notification_policy.secret_rotated", actorKind: p.Kind().String(), actorID: p.Subject(),
			resourceType: "notification_policy", resourceID: id, revision: np.Revision, requestID: requestID, details: details,
		})
	}()
	return np, err
}
