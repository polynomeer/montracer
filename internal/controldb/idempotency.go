package controldb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

// runIdempotent는 body를 Idempotency-Key 아래 한 트랜잭션에서 실행한다 (D02 §20, ADR 0060 §1).
//   - 살아 있는 같은 key가 있으면 body를 실행하지 않고 저장된 응답을 돌려준다(replayed=true). 다른 본문이면 ErrIdempotencyConflict.
//   - 처음이면 body의 부작용과 그 응답 저장을 같은 트랜잭션에 묶는다(둘 중 하나만 남지 않는다).
//   - 같은 key의 동시 요청은 transaction advisory lock으로 줄 세운다: 뒤 요청은 앞 요청이 끝난 뒤 key를 읽어 재생한다.
//     줄 세우지 않으면 뒤 요청이 부작용 단계에서 실패할 수 있다(예: secret 교체는 앞 요청이 revision을 올려 412) — 응답을 잃은
//     client가 412를 보고 다시 교체하면 두 번 바뀐다(리뷰에서 발견).
//   - 그래도 unique 위반이 나면(다른 경로의 같은 key 등) 다시 읽는다(maxIdempotencyAttempts번).
func runIdempotent(ctx context.Context, db *DB, p authz.Principal, idem IdempotencyRequest, op string,
	body func(tx pgx.Tx) (StoredResponse, error)) (StoredResponse, bool, error) {
	if idem.Key == "" || idem.MethodPath == "" {
		return StoredResponse{}, false, errors.New("controldb: idempotency key is required")
	}
	for attempt := 0; ; attempt++ {
		var (
			resp     StoredResponse
			replayed bool
		)
		err := db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
				p.Tenant().String()+"\n"+principalKey(p)+"\n"+idem.MethodPath+"\n"+idem.Key); err != nil {
				return classify("lock idempotency key", err)
			}
			stored, found, err := lookupIdempotency(ctx, tx, p, idem)
			if err != nil {
				return err
			}
			if found {
				resp, replayed = stored, true
				return nil
			}
			if resp, err = body(tx); err != nil {
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
			return StoredResponse{}, false, &unavailableError{op: op, err: err}
		}
		if err != nil {
			return StoredResponse{}, false, err
		}
		return resp, replayed, nil
	}
}
