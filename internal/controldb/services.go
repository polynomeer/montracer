package controldb

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

// 서비스 catalog (migration 00006, D02 §08, ADR 0038).

// 상태 기준 (D02 §08: 24시간 미관측은 inactive, 30일 뒤 archived). 저장하지 않고 조회 시 last_seen으로 계산한다.
const (
	ServiceInactiveAfter = 24 * time.Hour
	ServiceArchivedAfter = 30 * 24 * time.Hour
	// MaxServicePage는 서비스 목록 한 page 상한이다 (D02 §12: limit 최대 1,000).
	MaxServicePage = 1000
)

// ServiceObservation은 수집에서 본 서비스 하나다. 자동 관측 필드만 담는다.
type ServiceObservation struct {
	ServiceID, Environment, Namespace, Name string
	Language                                string // "" = 모름
	SeenAt                                  time.Time
}

// Service는 catalog 항목이다.
type Service struct {
	ServiceID, Environment, Namespace, Name string
	// NameNormalized는 DB의 검색용 정규형이다(keyset 위치에 그대로 쓴다).
	NameNormalized                       string
	Language                             *string
	FirstSeen, LastSeen                  time.Time
	Status                               string // active | inactive | archived
	OwnerTeam, RepositoryURL, RunbookURL *string
	Tier                                 *string
	Tags                                 []string
}

// ServicePosition은 (name_normalized, environment, service_id) keyset 위치다.
type ServicePosition struct {
	NameNormalized, Environment, ServiceID string
}

// ServiceQuery는 서비스 목록 조건이다.
type ServiceQuery struct {
	Environment     string // 선택: 이 environment만
	OwnerTeam       string // 선택: 이 owner_team만
	IncludeArchived bool
	After           *ServicePosition
	Limit           int
	Now             time.Time
}

// ServiceStore는 services 저장소다.
type ServiceStore struct{ db *DB }

// NewServiceStore는 ServiceStore를 만든다.
func NewServiceStore(db *DB) *ServiceStore { return &ServiceStore{db: db} }

// Observe는 관측한 서비스를 등록하거나 자동 필드(last_seen, language)를 갱신한다(멱등).
// 사용자 관리 필드(owner_team 등)는 건드리지 않는다 — 앱 role에는 그 column의 UPDATE 권한도 없다.
func (s *ServiceStore) Observe(ctx context.Context, tenant authz.TenantID, obs []ServiceObservation) error {
	if len(obs) == 0 {
		return nil
	}
	ids, envs, nss, names, langs := make([]string, len(obs)), make([]string, len(obs)), make([]string, len(obs)), make([]string, len(obs)), make([]*string, len(obs))
	seen := make([]time.Time, len(obs))
	for i, o := range obs {
		ids[i], envs[i], nss[i], names[i], seen[i] = o.ServiceID, o.Environment, o.Namespace, o.Name, o.SeenAt
		if o.Language != "" {
			l := o.Language
			langs[i] = &l
		}
	}
	return s.db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO services (tenant_id, service_id, environment, namespace, name, language, first_seen, last_seen)
			SELECT app_tenant_id(), id::uuid, env, ns, nm, lang, seen, seen
			FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::timestamptz[]) AS t(id, env, ns, nm, lang, seen)
			ON CONFLICT (tenant_id, service_id) DO UPDATE SET
			  last_seen = GREATEST(services.last_seen, EXCLUDED.last_seen),
			  language  = COALESCE(EXCLUDED.language, services.language)`,
			ids, envs, nss, names, langs, seen)
		return classify("service observe", err)
	})
}

// ListServices는 principal이 볼 수 있는 서비스를 이름순(name_normalized, environment, service_id)으로 돌려준다.
// environment로 제한된 key는 허용된 environment의 서비스만 본다(mandatory predicate, D04 §01).
func (s *ServiceStore) ListServices(ctx context.Context, p authz.Principal, q ServiceQuery) ([]Service, bool, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, false, err
	}
	if q.Limit <= 0 || q.Limit > MaxServicePage {
		return nil, false, errors.New("controldb: service page limit must be within [1, 1000]")
	}
	if q.Now.IsZero() {
		return nil, false, errors.New("controldb: now is required")
	}
	var allowedEnvs []string // nil이면 제한 없음
	if p.EnvironmentRestricted() {
		allowedEnvs = p.Environments()
	}
	archivedBefore := q.Now.Add(-ServiceArchivedAfter)
	var afterName, afterEnv, afterID string
	hasAfter := q.After != nil
	if hasAfter {
		afterName, afterEnv, afterID = q.After.NameNormalized, q.After.Environment, q.After.ServiceID
	}
	var out []Service
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT service_id::text, environment, namespace, name, name_normalized, language, first_seen, last_seen,
			       owner_team, repository_url, runbook_url, tier, tags
			FROM services
			WHERE tenant_id = app_tenant_id()
			  AND ($1::text[] IS NULL OR environment = ANY($1))
			  AND ($2 = '' OR environment = $2)
			  AND ($3 = '' OR owner_team = $3)
			  AND ($4 OR last_seen >= $5)
			  AND (NOT $6 OR (name_normalized, environment, service_id::text) > ($7, $8, $9))
			ORDER BY name_normalized, environment, service_id::text
			LIMIT $10`,
			allowedEnvs, q.Environment, q.OwnerTeam, q.IncludeArchived, archivedBefore, hasAfter, afterName, afterEnv, afterID, q.Limit+1)
		if err != nil {
			return classify("list services", err)
		}
		defer rows.Close()
		for rows.Next() {
			var sv Service
			if err := rows.Scan(&sv.ServiceID, &sv.Environment, &sv.Namespace, &sv.Name, &sv.NameNormalized, &sv.Language, &sv.FirstSeen, &sv.LastSeen,
				&sv.OwnerTeam, &sv.RepositoryURL, &sv.RunbookURL, &sv.Tier, &sv.Tags); err != nil {
				return classify("scan service", err)
			}
			sv.FirstSeen, sv.LastSeen = sv.FirstSeen.UTC(), sv.LastSeen.UTC()
			sv.Status = serviceStatus(sv.LastSeen, q.Now)
			if sv.Tags == nil {
				sv.Tags = []string{}
			}
			out = append(out, sv)
		}
		return classify("list services", rows.Err())
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

func serviceStatus(lastSeen, now time.Time) string {
	switch age := now.Sub(lastSeen); {
	case age >= ServiceArchivedAfter:
		return "archived"
	case age >= ServiceInactiveAfter:
		return "inactive"
	default:
		return "active"
	}
}
