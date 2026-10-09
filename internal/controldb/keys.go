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

// MaxRevokeDelay는 key 교체 시 이전 key를 유지할 수 있는 최대 겹침 기간이다 (D04 §02).
const MaxRevokeDelay = 24 * time.Hour

// maxListKeys는 key 목록 조회 상한이다. cursor pagination은 API 구현 시 추가한다.
const maxListKeys = 1000

// ErrKeyIDCollision은 무작위 key_id가 기존 key와 겹쳤다는 뜻이다. 호출자는 새 key를 생성해 재시도한다.
var ErrKeyIDCollision = errors.New("controldb: key id collision")

// KeyStore는 api_keys 저장소다.
type KeyStore struct {
	db *DB
}

// NewKeyStore는 KeyStore를 만든다.
func NewKeyStore(db *DB) *KeyStore { return &KeyStore{db: db} }

// KeyMetadata는 key 조회 결과다. hash와 원문은 포함하지 않는다.
type KeyMetadata struct {
	KeyID        string
	Kind         authz.Kind
	Scopes       []authz.Action
	Environments []string
	IssuedBy     string
	ExpiresAt    time.Time
	RevokedAt    *time.Time
	CreatedAt    time.Time
}

func kindToDB(k authz.Kind) (string, error) {
	switch k {
	case authz.KindIngestKey:
		return "ingest", nil
	case authz.KindAPIKey:
		return "api", nil
	default:
		return "", fmt.Errorf("controldb: unsupported key kind %s", k)
	}
}

func kindFromDB(s string) authz.Kind {
	switch s {
	case "ingest":
		return authz.KindIngestKey
	case "api":
		return authz.KindAPIKey
	default:
		return 0 // authz가 거절한다
	}
}

func actionsToDB(as []authz.Action) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = string(a)
	}
	return out
}

func actionsFromDB(ss []string) []authz.Action {
	out := make([]authz.Action, len(ss))
	for i, s := range ss {
		out[i] = authz.Action(s)
	}
	return out
}

// LookupKey는 인증용 key 기록을 읽는다. authz.KeyLookup을 구현한다.
//
// tenant를 모르는 상태이므로 app.key_lookup으로 해당 key_id 한 행만 연다 (ADR 0016 §4).
// 그 뒤 key의 tenant로 context를 설정해 발급자의 현재 role을 읽는다 (ADR 0015 §1).
func (s *KeyStore) LookupKey(ctx context.Context, keyID string) (authz.KeyRecord, error) {
	var rec authz.KeyRecord
	err := s.db.inTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.key_lookup', $1, true)`, keyID); err != nil {
			return classify("set key lookup", err)
		}
		var (
			tenantStr, kind, issuedBy string
			scopes, envs              []string
		)
		err := tx.QueryRow(ctx, `
			SELECT tenant_id::text, kind, hash, scopes, environments, issued_by, expires_at, revoked_at
			FROM api_keys WHERE key_id = $1`, keyID).
			Scan(&tenantStr, &kind, &rec.Hash, &scopes, &envs, &issuedBy, &rec.ExpiresAt, &rec.RevokedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return authz.ErrKeyNotFound
		}
		if err != nil {
			return classify("select key", err)
		}
		tenant, err := authz.ParseTenantID(tenantStr)
		if err != nil {
			return fmt.Errorf("controldb: stored tenant id: %w", err)
		}
		rec.KeyID, rec.Tenant, rec.Kind = keyID, tenant, kindFromDB(kind)
		rec.Scopes, rec.Environments = actionsFromDB(scopes), envs

		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantStr); err != nil {
			return classify("set tenant", err)
		}
		var role string
		err = tx.QueryRow(ctx, `SELECT role FROM memberships WHERE tenant_id = $1 AND user_id = $2`,
			tenantStr, issuedBy).Scan(&role)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			rec.IssuerRole = "" // 발급자 제거: API key 무효 (ADR 0015 §1)
		case err != nil:
			return classify("select issuer role", err)
		default:
			rec.IssuerRole = authz.Role(role)
		}
		return nil
	})
	if err != nil {
		return authz.KeyRecord{}, err
	}
	return rec, nil
}

// CreateKey는 검증된 발급 요청을 저장한다. key, 감사, outbox를 한 트랜잭션에 쓴다.
// iss는 authz.ValidateKeyIssuance의 결과여야 한다 (tenant가 issuer에 고정됨). zero value는 거절한다.
func (s *KeyStore) CreateKey(ctx context.Context, iss authz.KeyIssuance, gen authz.GeneratedKey, expiresAt time.Time, requestID string) error {
	if !iss.Valid() {
		return errors.New("controldb: key issuance must come from authz.ValidateKeyIssuance")
	}
	kind, err := kindToDB(iss.Kind())
	if err != nil {
		return err
	}
	if !expiresAt.After(now()) {
		return errors.New("controldb: key expiry must be in the future")
	}
	details, err := json.Marshal(map[string]any{
		"kind": kind, "scopes": actionsToDB(iss.Scopes()), "environments": iss.Environments(),
	})
	if err != nil {
		return fmt.Errorf("controldb: audit details: %w", err)
	}
	envs := iss.Environments()
	if envs == nil {
		envs = []string{}
	}
	return s.db.WithTenant(ctx, iss.Tenant(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO api_keys (tenant_id, key_id, kind, hash, scopes, environments, issued_by, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			iss.Tenant().String(), gen.KeyID, kind, gen.Hash, actionsToDB(iss.Scopes()), envs, iss.IssuedBy(), expiresAt)
		if isUniqueViolation(err) {
			return ErrKeyIDCollision
		}
		if err != nil {
			return classify("insert key", err)
		}
		return writeAuditAndOutbox(ctx, tx, iss.Tenant(), change{
			action: "key.created", actorKind: "user", actorID: iss.IssuedBy(),
			resourceType: "api_key", resourceID: gen.KeyID, revision: 1,
			requestID: requestID, details: details,
		})
	})
}

// RevokeKey는 key를 revokeAt 시각부터 무효로 만든다. revokeAt은 지금부터 MaxRevokeDelay 이내다.
// 이미 그 시각 이전에 폐기된 key는 변경 없이 성공한다(멱등). 다른 tenant의 key는 ErrNotFound다.
func (s *KeyStore) RevokeKey(ctx context.Context, p authz.Principal, keyID string, revokeAt time.Time, requestID string) error {
	if err := authz.Authorize(p, authz.KeysManage); err != nil {
		return err
	}
	t := now()
	if revokeAt.Before(t.Add(-time.Minute)) || revokeAt.After(t.Add(MaxRevokeDelay)) {
		return errors.New("controldb: revoke time must be within [now, now+24h]")
	}
	return s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		var revision int64
		err := tx.QueryRow(ctx, `
			UPDATE api_keys SET revoked_at = $2, revision = revision + 1
			WHERE key_id = $1 AND (revoked_at IS NULL OR revoked_at > $2)
			RETURNING revision`, keyID, revokeAt).Scan(&revision)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys WHERE key_id = $1)`, keyID).Scan(&exists); err != nil {
				return classify("select key", err)
			}
			if !exists {
				return authz.ErrNotFound
			}
			return nil // 이미 더 이른 시각에 폐기됨
		}
		if err != nil {
			return classify("revoke key", err)
		}
		details, err := json.Marshal(map[string]any{"revoke_at": revokeAt.UTC().Format(time.RFC3339Nano)})
		if err != nil {
			return fmt.Errorf("controldb: audit details: %w", err)
		}
		return writeAuditAndOutbox(ctx, tx, p.Tenant(), change{
			action: "key.revoked", actorKind: p.Kind().String(), actorID: p.Subject(),
			resourceType: "api_key", resourceID: keyID, revision: revision,
			requestID: requestID, details: details,
		})
	})
}

// ListKeys는 principal tenant의 key metadata를 최신순으로 반환한다.
func (s *KeyStore) ListKeys(ctx context.Context, p authz.Principal) ([]KeyMetadata, error) {
	if err := authz.Authorize(p, authz.KeysRead); err != nil {
		return nil, err
	}
	var out []KeyMetadata
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
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
		return classify("list keys", rows.Err())
	})
	return out, err
}

// change는 감사·outbox에 함께 기록할 변경이다.
type change struct {
	// category는 감사 범주다(ADR 0015 §4). 비면 security. monitor 변경처럼 운영 설정은 operations다.
	category                   string
	action, actorKind, actorID string
	resourceType, resourceID   string
	revision                   int64
	requestID                  string
	details                    []byte // JSON. PII·secret 금지
	// outboxPayload가 있으면 outbox에는 details 대신 이것을 쓴다.
	// break-glass 사유처럼 감사에만 둘 내용을 하류 consumer에 퍼뜨리지 않기 위해서다 (ADR 0033).
	outboxPayload []byte
}

// writeAuditAndOutbox는 같은 트랜잭션에 security 감사와 outbox event를 쓴다 (D02 §11, §14).
func writeAuditAndOutbox(ctx context.Context, tx pgx.Tx, tenant authz.TenantID, c change) error {
	if err := writeAudit(ctx, tx, tenant, c); err != nil {
		return err
	}
	eventID, err := newUUID()
	if err != nil {
		return err
	}
	payload := c.details
	if c.outboxPayload != nil {
		payload = c.outboxPayload
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (tenant_id, event_id, type, resource_id, revision, actor_id, schema_version, payload)
		VALUES ($1, $2, $3, $4, $5, $6, 1, $7)`,
		tenant.String(), eventID, c.action, c.resourceID, c.revision, c.actorID, payload); err != nil {
		return classify("insert outbox", err)
	}
	return nil
}

// writeAudit은 security 감사 한 건을 쓴다. 상태 변경이 없는 접근(조회)은 outbox 없이 이것만 쓴다.
func writeAudit(ctx context.Context, tx pgx.Tx, tenant authz.TenantID, c change) error {
	auditID, err := newUUID()
	if err != nil {
		return err
	}
	var requestID *string
	if c.requestID != "" {
		requestID = &c.requestID
	}
	category := c.category
	if category == "" {
		category = "security"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_events (tenant_id, id, category, action, actor_kind, actor_id, resource_type, resource_id, request_id, details)
		VALUES ($1, $2, $10, $3, $4, $5, $6, $7, $8, $9)`,
		tenant.String(), auditID, c.action, c.actorKind, c.actorID, c.resourceType, c.resourceID, requestID, c.details, category); err != nil {
		return classify("insert audit", err)
	}
	return nil
}
