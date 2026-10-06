package controldb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

// 플랫폼 운영자의 break-glass 작업 (D04 §01, §11 RB03, ADR 0033).
//
// 모든 시도(성공·대상 없음·이미 폐기)는 대상 tenant의 security 감사에 actor_kind='operator'로 남는다.
// 고객은 audit.read로 지원 접근 이력을 본다. 사유·승인자·ticket은 감사에만 두고 outbox에는 퍼뜨리지 않는다.
// 접근은 WithTenant를 거쳐 RLS 안에서만 한다 — 운영자에게 cross-tenant 경로를 따로 열지 않는다.

const (
	actorOperator = "operator"
	// AuditBreakGlassKeysListed는 key metadata 목록 조회 감사 action이다.
	AuditBreakGlassKeysListed = "break_glass.keys.listed"
	// AuditBreakGlassKeyAlreadyRevoked는 이미 폐기된 key에 대한 폐기 시도다.
	AuditBreakGlassKeyAlreadyRevoked = "break_glass.key.already_revoked"
	// AuditBreakGlassKeyNotFound는 tenant에 없는 key ID에 대한 폐기 시도다.
	AuditBreakGlassKeyNotFound = "break_glass.key.not_found"
)

// breakGlassDetails는 감사 details다. 고객 데이터는 없다(사유는 운영자 입력, 길이·제어 문자 검증됨).
func breakGlassDetails(g authz.BreakGlassGrant, extra map[string]any) ([]byte, error) {
	d := map[string]any{
		"break_glass": map[string]any{
			"approver":   g.Approver(),
			"ticket":     g.Ticket(),
			"reason":     g.Reason(),
			"issued_at":  g.IssuedAt().Format(time.RFC3339),
			"expires_at": g.ExpiresAt().Format(time.RFC3339),
		},
	}
	for k, v := range extra {
		d[k] = v
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("controldb: audit details: %w", err)
	}
	return b, nil
}

// BreakGlassListKeys는 grant tenant의 key metadata(hash·원문 없음)를 최신순으로 반환하고, 조회를 감사에 남긴다.
func (s *KeyStore) BreakGlassListKeys(ctx context.Context, g authz.BreakGlassGrant, requestID string) ([]KeyMetadata, error) {
	if err := g.Check(authz.BreakGlassKeysList, now()); err != nil {
		return nil, err
	}
	var out []KeyMetadata
	err := s.db.WithTenant(ctx, g.Tenant(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT key_id, kind, scopes, environments, issued_by, expires_at, revoked_at, created_at
			FROM api_keys ORDER BY created_at DESC, key_id LIMIT $1`, maxListKeys)
		if err != nil {
			return classify("list keys", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				m      KeyMetadata
				kind   string
				scopes []string
			)
			if err := rows.Scan(&m.KeyID, &kind, &scopes, &m.Environments, &m.IssuedBy, &m.ExpiresAt, &m.RevokedAt, &m.CreatedAt); err != nil {
				return classify("scan key", err)
			}
			m.Kind, m.Scopes = kindFromDB(kind), actionsFromDB(scopes)
			out = append(out, m)
		}
		if err := rows.Err(); err != nil {
			return classify("list keys", err)
		}
		details, err := breakGlassDetails(g, map[string]any{"keys_returned": len(out)})
		if err != nil {
			return err
		}
		// 같은 트랜잭션: 감사가 실패하면 조회 결과도 돌려주지 않는다(감사 없는 접근 금지).
		return writeAudit(ctx, tx, g.Tenant(), change{
			action: AuditBreakGlassKeysListed, actorKind: actorOperator, actorID: g.Operator(),
			resourceType: "api_key", resourceID: "*", requestID: requestID, details: details,
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RevokeOutcome은 break-glass 폐기 결과다.
type RevokeOutcome int

const (
	// RevokeApplied는 지금 폐기했다는 뜻이다(key.revoked outbox event 발행).
	RevokeApplied RevokeOutcome = iota + 1
	// RevokeAlreadyRevoked는 이미 지금 이전에 폐기돼 변경이 없다는 뜻이다.
	RevokeAlreadyRevoked
)

// BreakGlassRevokeKey는 grant tenant의 key를 즉시 폐기한다.
//   - 폐기하면 tenant 관리자 폐기와 같은 key.revoked 감사·outbox를 쓴다. 지금 인증은 DB를 직접 조회하므로 즉시 거절된다.
//     인증 cache·outbox dispatcher가 생기면 같은 event로 무효화한다(ADR 0033 §4).
//     actor는 operator이고, 사유·승인자·ticket은 감사 details에만 둔다.
//   - 이미 폐기됐거나 key가 없어도 시도를 감사에 남긴다. 없는 key는 authz.ErrNotFound이고 감사 resource_id는 "unknown"이다.
func (s *KeyStore) BreakGlassRevokeKey(ctx context.Context, g authz.BreakGlassGrant, keyID, requestID string) (RevokeOutcome, error) {
	t := now()
	if err := g.Check(authz.BreakGlassKeysRevoke, t); err != nil {
		return 0, err
	}
	// 형식 밖 값은 DB·감사에 닿지 않는다(운영자가 친 임의 문자열이 고객 감사에 남지 않게).
	if !authz.ValidKeyID(keyID) {
		return 0, errors.New("controldb: key id must be 16 lowercase hex characters")
	}
	var (
		outcome  RevokeOutcome
		notFound bool
	)
	err := s.db.WithTenant(ctx, g.Tenant(), func(tx pgx.Tx) error {
		var revision int64
		err := tx.QueryRow(ctx, `
			UPDATE api_keys SET revoked_at = $2, revision = revision + 1
			WHERE key_id = $1 AND (revoked_at IS NULL OR revoked_at > $2)
			RETURNING revision`, keyID, t).Scan(&revision)
		switch {
		case err == nil:
			outcome = RevokeApplied
			details, err := breakGlassDetails(g, map[string]any{"revoke_at": t.UTC().Format(time.RFC3339Nano)})
			if err != nil {
				return err
			}
			// outbox에는 actor_kind만 더한다: actor_id만으로는 운영자와 tenant 사용자를 구분할 수 없다.
			payload, err := json.Marshal(map[string]any{"revoke_at": t.UTC().Format(time.RFC3339Nano), "actor_kind": actorOperator})
			if err != nil {
				return fmt.Errorf("controldb: outbox payload: %w", err)
			}
			return writeAuditAndOutbox(ctx, tx, g.Tenant(), change{
				action: "key.revoked", actorKind: actorOperator, actorID: g.Operator(),
				resourceType: "api_key", resourceID: keyID, revision: revision,
				requestID: requestID, details: details, outboxPayload: payload,
			})
		case !errors.Is(err, pgx.ErrNoRows):
			return classify("revoke key", err)
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys WHERE key_id = $1)`, keyID).Scan(&exists); err != nil {
			return classify("select key", err)
		}
		action, resourceID := AuditBreakGlassKeyAlreadyRevoked, keyID
		if !exists {
			// 없는 key ID는 고객 감사에 남기지 않는다. 다른 tenant의 key ID를 잘못 넣었을 때
			// 그 식별자가 이 tenant 고객에게 보이지 않게 한다(D04 §01 다른 조직 존재 누출 금지).
			action, resourceID, notFound = AuditBreakGlassKeyNotFound, "unknown", true
		} else {
			outcome = RevokeAlreadyRevoked
		}
		details, err := breakGlassDetails(g, nil)
		if err != nil {
			return err
		}
		return writeAudit(ctx, tx, g.Tenant(), change{
			action: action, actorKind: actorOperator, actorID: g.Operator(),
			resourceType: "api_key", resourceID: resourceID, requestID: requestID, details: details,
		})
	})
	if err != nil {
		return 0, err
	}
	if notFound {
		return 0, authz.ErrNotFound
	}
	return outcome, nil
}
