package controldb

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

// MaxAuditPage는 감사 조회 한 번의 최대 행 수다 (D02 §12 limit 최대 1,000).
const MaxAuditPage = 1000

// AuditEvent는 감사 이벤트 하나다. details는 저장된 JSON 그대로다(작성 시 PII·secret 금지).
type AuditEvent struct {
	ID           string
	OccurredAt   time.Time
	Category     string
	Action       string
	ActorKind    string
	ActorID      string
	ResourceType string
	ResourceID   string
	RequestID    *string
	Details      json.RawMessage
}

// AuditPosition은 keyset pagination 위치다. (occurred_at, id) 내림차순에서 이 위치보다 뒤(더 오래된) 행부터 읽는다.
type AuditPosition struct {
	OccurredAt time.Time
	ID         string
}

// AuditQuery는 감사 조회 조건이다. 시간은 [From, To).
type AuditQuery struct {
	From, To time.Time
	// Categories는 조회할 범주다. 비어 있으면 principal이 볼 수 있는 범주 전체다.
	// principal이 볼 수 없는 범주가 들어 있으면 ErrForbidden이다(조용히 빼지 않는다).
	Categories []string
	// Action이 있으면 그 action만(정확히 일치).
	Action string
	After  *AuditPosition
	Limit  int
}

// AuditStore는 audit_events 읽기 저장소다. 쓰기는 각 변경과 같은 트랜잭션에서 한다(keys.go writeAudit).
type AuditStore struct {
	db *DB
}

// NewAuditStore는 AuditStore를 만든다.
func NewAuditStore(db *DB) *AuditStore { return &AuditStore{db: db} }

// ListAuditEvents는 principal tenant의 감사 이벤트를 (occurred_at, id) 내림차순으로 돌려준다.
// 범주는 principal 권한으로 제한한다(ADR 0015 §4): audit.read는 전체, audit.operations.read는 operations만.
// limit+1행을 읽어 다음 page 유무를 함께 돌려준다.
func (s *AuditStore) ListAuditEvents(ctx context.Context, p authz.Principal, q AuditQuery) ([]AuditEvent, bool, error) {
	allowed, err := authz.AuditCategories(p)
	if err != nil {
		return nil, false, err
	}
	cats, err := restrictCategories(q.Categories, allowed)
	if err != nil {
		return nil, false, err
	}
	if !q.From.Before(q.To) {
		return nil, false, errors.New("controldb: audit query needs from < to")
	}
	if q.Limit <= 0 || q.Limit > MaxAuditPage {
		return nil, false, errors.New("controldb: audit query limit must be within [1, 1000]")
	}
	// tenant·시간·범주는 항상 predicate다(계약 4). RLS가 tenant를 한 번 더 막는다.
	// id::text는 별칭(id_text)으로 낸다. 같은 이름이면 ORDER BY id가 uuid 컬럼이 아니라 text 출력으로 해석되어
	// keyset 비교(uuid)와 정렬 순서가 collation에 따라 어긋날 수 있다.
	sql := `SELECT id::text AS id_text, occurred_at, category, action, actor_kind, actor_id, resource_type, resource_id, request_id, details
		FROM audit_events
		WHERE tenant_id = app_tenant_id() AND occurred_at >= $1 AND occurred_at < $2 AND category = ANY($3)
		  AND ($4::text = '' OR action = $4::text)`
	args := []any{q.From, q.To, cats, q.Action, q.Limit + 1}
	if q.After != nil {
		sql += ` AND (occurred_at, id) < ($6::timestamptz, $7::uuid)`
		args = append(args, q.After.OccurredAt, q.After.ID)
	}
	sql += ` ORDER BY occurred_at DESC, id DESC LIMIT $5`
	var out []AuditEvent
	err = s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return classify("list audit", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				e       AuditEvent
				details []byte
			)
			if err := rows.Scan(&e.ID, &e.OccurredAt, &e.Category, &e.Action, &e.ActorKind, &e.ActorID,
				&e.ResourceType, &e.ResourceID, &e.RequestID, &details); err != nil {
				return classify("scan audit", err)
			}
			e.OccurredAt = e.OccurredAt.UTC()
			e.Details = details
			out = append(out, e)
		}
		return classify("list audit", rows.Err())
	})
	if err != nil {
		return nil, false, err
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return out, more, nil
}

// restrictCategories는 요청 범주를 허용 범주 안으로 한정한다. 비면 허용 전체.
func restrictCategories(requested, allowed []string) ([]string, error) {
	if len(requested) == 0 {
		return allowed, nil
	}
	ok := map[string]bool{}
	for _, c := range allowed {
		ok[c] = true
	}
	for _, c := range requested {
		if !ok[c] {
			return nil, authz.ErrForbidden
		}
	}
	return requested, nil
}
