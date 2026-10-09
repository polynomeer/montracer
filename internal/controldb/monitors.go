package controldb

import (
	"bytes"
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

// monitor 저장소 (D02 §11·§14·§20, ADR 0049).
//   - 정의(monitors)와 revision 이력(monitor_revisions), 감사(operations), outbox를 한 트랜잭션에 쓴다.
//   - 수정은 If-Match revision이 맞을 때만 한다(마지막 쓰기가 덮어쓰지 않는다, D02 §11).
//   - 삭제는 tombstone이다.

// MaxMonitorPage는 목록 page 상한이다.
const MaxMonitorPage = 200

// IdempotencyTTL은 Idempotency-Key 응답 보관 기간이다 (D02 §20 "24시간 저장").
const IdempotencyTTL = 24 * time.Hour

var (
	// ErrRevisionMismatch는 If-Match revision이 현재 revision과 다르다(412).
	ErrRevisionMismatch = errors.New("controldb: revision mismatch")
	// ErrIdempotencyConflict는 같은 Idempotency-Key를 다른 요청 본문에 다시 썼다(409, D02 §20).
	ErrIdempotencyConflict = errors.New("controldb: idempotency key reused with a different request")
)

// Monitor는 저장된 monitor다. Spec은 정규형 JSON이다.
type Monitor struct {
	ID        string
	Name      string
	Spec      []byte
	Enabled   bool
	Revision  int64
	CreatedBy string
	UpdatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MonitorPosition은 목록 keyset 위치다(이름 정규형, id).
type MonitorPosition struct {
	NameNormalized string
	ID             string
}

// MonitorWrite는 저장할 정의다. Spec은 monitor.Normalize의 정규형이어야 한다.
type MonitorWrite struct {
	Name    string
	Spec    []byte
	Enabled bool
}

// IdempotencyRequest는 POST 한 건의 식별이다. RequestHash는 요청 본문(정규화 전 원문 bytes)의 SHA-256이다.
type IdempotencyRequest struct {
	Key         string
	MethodPath  string
	RequestHash [32]byte
}

// StoredResponse는 처음 응답이다. 같은 key·같은 본문의 재시도에 그대로 돌려준다.
type StoredResponse struct {
	Status int
	Body   []byte
}

// MonitorStore는 monitor 저장소다.
type MonitorStore struct{ db *DB }

// NewMonitorStore는 저장소를 만든다.
func NewMonitorStore(db *DB) *MonitorStore { return &MonitorStore{db: db} }

func principalKey(p authz.Principal) string { return p.Kind().String() + ":" + p.Subject() }

func specDetails(revision int64, w MonitorWrite, deleted bool) ([]byte, error) {
	// 감사에는 spec 본문(filter 값은 자유 입력)을 넣지 않고 hash만 둔다. threshold 등 내용은 revision 이력에 있다.
	sum := sha256.Sum256(w.Spec)
	b, err := json.Marshal(map[string]any{"revision": revision, "enabled": w.Enabled, "deleted": deleted, "spec_sha256": hex.EncodeToString(sum[:])})
	if err != nil {
		return nil, fmt.Errorf("controldb: monitor audit details: %w", err)
	}
	return b, nil
}

// CreateMonitor는 monitor를 만든다. Idempotency-Key가 이미 있으면(24시간 안):
//   - 같은 본문이면 처음 응답을 그대로 돌려준다(replayed = true, 새로 만들지 않는다).
//   - 다른 본문이면 ErrIdempotencyConflict다.
//
// respond는 만든 monitor로 응답을 만든다. 그 응답이 key와 함께 같은 트랜잭션에 저장된다.
// 같은 key의 동시 요청은 기본 키 충돌로 하나만 commit되고, 나머지는 다시 읽어 저장된 응답을 돌려준다.
func (s *MonitorStore) CreateMonitor(ctx context.Context, p authz.Principal, w MonitorWrite, idem IdempotencyRequest, requestID string,
	respond func(Monitor) (StoredResponse, error)) (StoredResponse, bool, error) {
	if err := authz.Authorize(p, authz.MonitorsWrite); err != nil {
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
			var m Monitor
			if err := tx.QueryRow(ctx, `
				INSERT INTO monitors (tenant_id, id, name, spec, enabled, created_by, updated_by)
				VALUES (app_tenant_id(), $1, $2, $3, $4, $5, $5)
				RETURNING id::text, name, spec, enabled, revision, created_by, updated_by, created_at, updated_at`,
				id, w.Name, w.Spec, w.Enabled, p.Subject()).Scan(&m.ID, &m.Name, &m.Spec, &m.Enabled, &m.Revision, &m.CreatedBy, &m.UpdatedBy, &m.CreatedAt, &m.UpdatedAt); err != nil {
				return classify("insert monitor", err)
			}
			if err := insertRevision(ctx, tx, m.ID, m.Revision, w, false, p.Subject()); err != nil {
				return err
			}
			details, err := specDetails(m.Revision, w, false)
			if err != nil {
				return err
			}
			if err := writeAuditAndOutbox(ctx, tx, p.Tenant(), change{
				category: "operations", action: "monitor.created", actorKind: p.Kind().String(), actorID: p.Subject(),
				resourceType: "monitor", resourceID: m.ID, revision: m.Revision, requestID: requestID, details: details,
			}); err != nil {
				return err
			}
			m.CreatedAt, m.UpdatedAt = m.CreatedAt.UTC(), m.UpdatedAt.UTC()
			if resp, err = respond(m); err != nil {
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
				continue // 같은 key의 다른 요청이 먼저 commit했다. 다시 읽어 그 응답을 돌려준다
			}
			// 경합이 계속되면 일시 장애로 본다(503, 재시도 가능). 변경은 모두 rollback됐다.
			return StoredResponse{}, false, &unavailableError{op: "idempotent create", err: err}
		}
		if err != nil {
			return StoredResponse{}, false, err
		}
		return resp, replayed, nil
	}
}

var errIdempotencyRace = errors.New("controldb: concurrent idempotent request")

// maxIdempotencyAttempts는 같은 key 동시 요청의 다시 읽기 횟수다.
const maxIdempotencyAttempts = 3

// lookupIdempotency는 살아 있는 key를 찾는다. 만료된 행은 지우고 없는 것으로 본다.
func lookupIdempotency(ctx context.Context, tx pgx.Tx, p authz.Principal, idem IdempotencyRequest) (StoredResponse, bool, error) {
	var (
		hash    []byte
		resp    StoredResponse
		expired bool
	)
	err := tx.QueryRow(ctx, `
		SELECT request_hash, status_code, response_body, expires_at <= now()
		FROM idempotency_keys WHERE principal = $1 AND method_path = $2 AND key = $3`,
		principalKey(p), idem.MethodPath, idem.Key).Scan(&hash, &resp.Status, &resp.Body, &expired)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return StoredResponse{}, false, nil
	case err != nil:
		return StoredResponse{}, false, classify("select idempotency key", err)
	case expired:
		if _, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE principal = $1 AND method_path = $2 AND key = $3`,
			principalKey(p), idem.MethodPath, idem.Key); err != nil {
			return StoredResponse{}, false, classify("delete expired idempotency key", err)
		}
		return StoredResponse{}, false, nil
	case !bytes.Equal(hash, idem.RequestHash[:]):
		return StoredResponse{}, false, ErrIdempotencyConflict
	}
	return resp, true, nil
}

func insertRevision(ctx context.Context, tx pgx.Tx, id string, revision int64, w MonitorWrite, deleted bool, actor string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO monitor_revisions (tenant_id, monitor_id, revision, spec, enabled, deleted, created_by)
		VALUES (app_tenant_id(), $1, $2, $3, $4, $5, $6)`, id, revision, w.Spec, w.Enabled, deleted, actor); err != nil {
		return classify("insert monitor revision", err)
	}
	return nil
}

const monitorColumns = `id::text, name, spec, enabled, revision, created_by, updated_by, created_at, updated_at`

func scanMonitor(row pgx.Row) (Monitor, error) {
	var m Monitor
	if err := row.Scan(&m.ID, &m.Name, &m.Spec, &m.Enabled, &m.Revision, &m.CreatedBy, &m.UpdatedBy, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return Monitor{}, err
	}
	m.CreatedAt, m.UpdatedAt = m.CreatedAt.UTC(), m.UpdatedAt.UTC()
	return m, nil
}

// GetMonitor는 삭제되지 않은 monitor 하나다. 없거나 다른 tenant거나 삭제됐으면 authz.ErrNotFound다(존재를 숨긴다).
func (s *MonitorStore) GetMonitor(ctx context.Context, p authz.Principal, id string) (Monitor, error) {
	if err := authz.Authorize(p, authz.MonitorsRead); err != nil {
		return Monitor{}, err
	}
	var m Monitor
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		var err error
		m, err = scanMonitor(tx.QueryRow(ctx, `SELECT `+monitorColumns+` FROM monitors WHERE id = $1 AND deleted_at IS NULL`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return authz.ErrNotFound
		}
		return classify("select monitor", err)
	})
	return m, err
}

// ListMonitors는 삭제되지 않은 monitor를 이름순(대소문자 무시, 동률은 id)으로 돌려준다. more면 다음 page가 있다.
func (s *MonitorStore) ListMonitors(ctx context.Context, p authz.Principal, after *MonitorPosition, limit int) ([]Monitor, []MonitorPosition, bool, error) {
	if err := authz.Authorize(p, authz.MonitorsRead); err != nil {
		return nil, nil, false, err
	}
	if limit < 1 || limit > MaxMonitorPage {
		return nil, nil, false, fmt.Errorf("controldb: monitor page limit must be within [1, %d]", MaxMonitorPage)
	}
	sql := `SELECT ` + monitorColumns + `, name_normalized FROM monitors WHERE tenant_id = app_tenant_id() AND deleted_at IS NULL`
	args := []any{limit + 1}
	if after != nil {
		sql += ` AND (name_normalized, id) > ($2, $3::uuid)`
		args = append(args, after.NameNormalized, after.ID)
	}
	sql += ` ORDER BY name_normalized, id LIMIT $1`
	var (
		out []Monitor
		pos []MonitorPosition
	)
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return classify("list monitors", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				m  Monitor
				nn string
			)
			if err := rows.Scan(&m.ID, &m.Name, &m.Spec, &m.Enabled, &m.Revision, &m.CreatedBy, &m.UpdatedBy, &m.CreatedAt, &m.UpdatedAt, &nn); err != nil {
				return classify("scan monitor", err)
			}
			m.CreatedAt, m.UpdatedAt = m.CreatedAt.UTC(), m.UpdatedAt.UTC()
			out = append(out, m)
			pos = append(pos, MonitorPosition{NameNormalized: nn, ID: m.ID})
		}
		return classify("list monitors", rows.Err())
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

// UpdateMonitor는 ifMatch revision이 현재와 같을 때만 정의를 바꾼다. revision은 1 늘고 새 revision 이력이 쌓인다.
// 없거나 삭제됐으면 authz.ErrNotFound, revision이 다르면 ErrRevisionMismatch다.
func (s *MonitorStore) UpdateMonitor(ctx context.Context, p authz.Principal, id string, ifMatch int64, w MonitorWrite, requestID string) (Monitor, error) {
	if err := authz.Authorize(p, authz.MonitorsWrite); err != nil {
		return Monitor{}, err
	}
	var m Monitor
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		var err error
		m, err = scanMonitor(tx.QueryRow(ctx, `
			UPDATE monitors SET name = $3, spec = $4, enabled = $5, revision = revision + 1, updated_by = $6, updated_at = now()
			WHERE id = $1 AND revision = $2 AND deleted_at IS NULL
			RETURNING `+monitorColumns, id, ifMatch, w.Name, w.Spec, w.Enabled, p.Subject()))
		if errors.Is(err, pgx.ErrNoRows) {
			return s.missOrMismatch(ctx, tx, id)
		}
		if err != nil {
			return classify("update monitor", err)
		}
		if err := insertRevision(ctx, tx, m.ID, m.Revision, w, false, p.Subject()); err != nil {
			return err
		}
		details, err := specDetails(m.Revision, w, false)
		if err != nil {
			return err
		}
		return writeAuditAndOutbox(ctx, tx, p.Tenant(), change{
			category: "operations", action: "monitor.updated", actorKind: p.Kind().String(), actorID: p.Subject(),
			resourceType: "monitor", resourceID: m.ID, revision: m.Revision, requestID: requestID, details: details,
		})
	})
	return m, err
}

// DeleteMonitor는 tombstone을 남긴다(revision + 1, 평가 중지). ifMatch가 있으면 현재 revision과 같아야 한다.
// 반환값은 삭제 revision이다.
func (s *MonitorStore) DeleteMonitor(ctx context.Context, p authz.Principal, id string, ifMatch *int64, requestID string) (int64, error) {
	if err := authz.Authorize(p, authz.MonitorsWrite); err != nil {
		return 0, err
	}
	var revision int64
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		var (
			w   MonitorWrite
			err error
		)
		err = tx.QueryRow(ctx, `
			UPDATE monitors SET deleted_at = now(), revision = revision + 1, updated_by = $3, updated_at = now()
			WHERE id = $1 AND ($2::bigint IS NULL OR revision = $2) AND deleted_at IS NULL
			RETURNING revision, name, spec, enabled`, id, ifMatch, p.Subject()).Scan(&revision, &w.Name, &w.Spec, &w.Enabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return s.missOrMismatch(ctx, tx, id)
		}
		if err != nil {
			return classify("delete monitor", err)
		}
		if err := insertRevision(ctx, tx, id, revision, w, true, p.Subject()); err != nil {
			return err
		}
		details, err := specDetails(revision, w, true)
		if err != nil {
			return err
		}
		return writeAuditAndOutbox(ctx, tx, p.Tenant(), change{
			category: "operations", action: "monitor.deleted", actorKind: p.Kind().String(), actorID: p.Subject(),
			resourceType: "monitor", resourceID: id, revision: revision, requestID: requestID, details: details,
		})
	})
	return revision, err
}

// missOrMismatch는 조건부 갱신이 0행일 때 이유를 가린다: 살아 있는 monitor가 있으면 revision 불일치, 아니면 없음.
func (s *MonitorStore) missOrMismatch(ctx context.Context, tx pgx.Tx, id string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM monitors WHERE id = $1 AND deleted_at IS NULL)`, id).Scan(&exists); err != nil {
		return classify("select monitor", err)
	}
	if exists {
		return ErrRevisionMismatch
	}
	return authz.ErrNotFound
}
