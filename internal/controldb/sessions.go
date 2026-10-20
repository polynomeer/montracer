package controldb

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

// ErrSessionNotFound는 token hash에 맞는 session 행이 없다는 뜻이다. 인증에서는 미인증과 같다.
var ErrSessionNotFound = errors.New("controldb: session not found")

// ErrNotMember는 사용자가 그 tenant의 member가 아니라는 뜻이다.
var ErrNotMember = errors.New("controldb: user is not a member of the tenant")

// Membership은 사용자 한 명의 조직 하나와 역할이다.
type Membership struct {
	Tenant authz.TenantID
	Role   authz.Role
}

// NewSession은 만들 session이다. IDHash는 cookie token의 SHA-256이다(원문은 저장하지 않는다).
type NewSession struct {
	IDHash        [32]byte
	Tenant        authz.TenantID
	UserID        string
	DisplayName   string // "" = 없음
	Now           time.Time
	IdleExpiresAt time.Time
	ExpiresAt     time.Time
	RequestID     string
}

// SessionRecord는 저장된 session과 지금의 membership 역할이다.
type SessionRecord struct {
	Tenant        authz.TenantID
	UserID        string
	DisplayName   *string
	LastSeenAt    time.Time
	IdleExpiresAt time.Time
	ExpiresAt     time.Time
	StepUpAt      *time.Time
	RevokedAt     *time.Time
	// Role은 요청 시점의 membership 역할이다. member가 아니게 됐으면 ""다(session 무효).
	Role authz.Role
}

// SessionStore는 browser session 저장소다 (ADR 0064, migration 00013).
type SessionStore struct{ db *DB }

// NewSessionStore는 SessionStore를 만든다.
func NewSessionStore(db *DB) *SessionStore { return &SessionStore{db: db} }

// UserMemberships는 사용자가 속한 조직과 역할이다(로그인 때 들어갈 조직을 고른다). tenant id 순이다.
// tenant를 모르는 상태라 member_lookup 정책으로 그 사용자 행만 읽는다.
func (s *SessionStore) UserMemberships(ctx context.Context, userID string) ([]Membership, error) {
	if userID == "" {
		return nil, errors.New("controldb: user id is required")
	}
	var out []Membership
	err := s.db.inTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.member_lookup', $1, true)`, userID); err != nil {
			return classify("set member lookup", err)
		}
		rows, err := tx.Query(ctx, `SELECT tenant_id::text, role FROM memberships WHERE user_id = $1`, userID)
		if err != nil {
			return classify("select memberships", err)
		}
		defer rows.Close()
		for rows.Next() {
			var tenant, role string
			if err := rows.Scan(&tenant, &role); err != nil {
				return classify("scan membership", err)
			}
			id, err := authz.ParseTenantID(tenant)
			if err != nil {
				return fmt.Errorf("controldb: stored tenant id: %w", err)
			}
			out = append(out, Membership{Tenant: id, Role: authz.Role(role)})
		}
		return classify("select memberships", rows.Err())
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Tenant.String() < out[b].Tenant.String() })
	return out, nil
}

// CreateSession은 session을 만든다. 그 tenant의 member가 아니면 ErrNotMember다(같은 트랜잭션에서 확인).
// 같은 사용자의 만료된 session을 지우고, 로그인을 security 감사에 남긴다(token·hash는 남기지 않는다).
func (s *SessionStore) CreateSession(ctx context.Context, n NewSession) error {
	if n.UserID == "" || n.Now.IsZero() || n.IdleExpiresAt.After(n.ExpiresAt) || !n.IdleExpiresAt.After(n.Now) {
		return errors.New("controldb: invalid session")
	}
	var display *string
	if n.DisplayName != "" {
		display = &n.DisplayName
	}
	return s.db.WithTenant(ctx, n.Tenant, func(tx pgx.Tx) error {
		var role string
		err := tx.QueryRow(ctx, `SELECT role FROM memberships WHERE tenant_id = app_tenant_id() AND user_id = $1`, n.UserID).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotMember
		}
		if err != nil {
			return classify("select membership", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE tenant_id = app_tenant_id() AND user_id = $1 AND expires_at < $2`,
			n.UserID, n.Now); err != nil {
			return classify("delete expired sessions", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_sessions (id_hash, tenant_id, user_id, display_name, created_at, last_seen_at, idle_expires_at, expires_at)
			VALUES ($1, app_tenant_id(), $2, $3, $4, $4, $5, $6)`,
			n.IDHash[:], n.UserID, display, n.Now, n.IdleExpiresAt, n.ExpiresAt); err != nil {
			return classify("insert session", err)
		}
		details, _ := json.Marshal(map[string]any{"role": role, "expires_at": n.ExpiresAt.UTC().Format(time.RFC3339)})
		return writeAudit(ctx, tx, n.Tenant, change{action: "session.created", actorKind: "user", actorID: n.UserID,
			resourceType: "user", resourceID: n.UserID, requestID: n.RequestID, details: details})
	})
}

// LookupSession은 token hash로 session을 읽고, 그 tenant의 지금 membership 역할을 붙인다.
// 만료·폐기 판정은 호출자가 한다(시각을 한 곳에서 정한다). 없으면 ErrSessionNotFound다.
func (s *SessionStore) LookupSession(ctx context.Context, idHash [32]byte) (SessionRecord, error) {
	var rec SessionRecord
	err := s.db.inTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.session_lookup', $1, true)`, hex.EncodeToString(idHash[:])); err != nil {
			return classify("set session lookup", err)
		}
		var tenant string
		err := tx.QueryRow(ctx, `
			SELECT tenant_id::text, user_id, display_name, last_seen_at, idle_expires_at, expires_at, step_up_at, revoked_at
			FROM user_sessions WHERE id_hash = $1`, idHash[:]).
			Scan(&tenant, &rec.UserID, &rec.DisplayName, &rec.LastSeenAt, &rec.IdleExpiresAt, &rec.ExpiresAt, &rec.StepUpAt, &rec.RevokedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSessionNotFound
		}
		if err != nil {
			return classify("select session", err)
		}
		if rec.Tenant, err = authz.ParseTenantID(tenant); err != nil {
			return fmt.Errorf("controldb: stored tenant id: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
			return classify("set tenant", err)
		}
		var role string
		err = tx.QueryRow(ctx, `SELECT role FROM memberships WHERE tenant_id = app_tenant_id() AND user_id = $1`, rec.UserID).Scan(&role)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			rec.Role = "" // member가 아니게 됨: session 무효
		case err != nil:
			return classify("select membership", err)
		default:
			rec.Role = authz.Role(role)
		}
		return nil
	})
	if err != nil {
		return SessionRecord{}, err
	}
	rec.LastSeenAt, rec.IdleExpiresAt, rec.ExpiresAt = rec.LastSeenAt.UTC(), rec.IdleExpiresAt.UTC(), rec.ExpiresAt.UTC()
	return rec, nil
}

// TouchSession은 활동 시각과 idle 만료를 늦춘다. 폐기된 session은 바꾸지 않는다. idle 만료는 최대 만료를 넘지 않는다.
func (s *SessionStore) TouchSession(ctx context.Context, tenant authz.TenantID, idHash [32]byte, now, idleExpiresAt time.Time) error {
	return s.db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE user_sessions SET last_seen_at = $2, idle_expires_at = least($3, expires_at)
			WHERE tenant_id = app_tenant_id() AND id_hash = $1 AND revoked_at IS NULL`, idHash[:], now, idleExpiresAt)
		return classify("touch session", err)
	})
}

// RevokeSession은 session을 폐기하고 security 감사에 남긴다. 이미 폐기됐으면 아무것도 하지 않는다.
func (s *SessionStore) RevokeSession(ctx context.Context, tenant authz.TenantID, idHash [32]byte, userID, reason, requestID string, now time.Time) error {
	return s.db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE user_sessions SET revoked_at = $2
			WHERE tenant_id = app_tenant_id() AND id_hash = $1 AND revoked_at IS NULL`, idHash[:], now)
		if err != nil {
			return classify("revoke session", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		details, _ := json.Marshal(map[string]string{"reason": reason})
		return writeAudit(ctx, tx, tenant, change{action: "session.revoked", actorKind: "user", actorID: userID,
			resourceType: "user", resourceID: userID, requestID: requestID, details: details})
	})
}
