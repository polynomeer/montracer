package controldb

import (
	"context"
	"errors"
	"sort"
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
	// MaxServicesPerTenant는 tenant 하나의 catalog 항목 상한이다 (ADR 0038 §2). 넘는 새 서비스는 등록하지 않고 센다
	// (무작위 service.name이 공유 제어 DB와 목록을 오염시키지 않게 한다). 이미 있는 서비스의 갱신은 막지 않는다.
	// 여러 replica가 동시에 새 서비스를 넣으면 replica 수 × batch만큼 넘을 수 있다(근사 상한).
	MaxServicesPerTenant = 5000
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

// Observe는 관측한 서비스를 등록하거나 자동 필드(last_seen, language)를 갱신한다(멱등). 돌려주는 값은
// tenant 상한(MaxServicesPerTenant) 때문에 등록하지 않은 새 서비스 수다.
// 사용자 관리 필드(owner_team 등)는 건드리지 않는다 — 앱 role에는 그 column의 INSERT·UPDATE 권한이 없다.
// 같은 service_id가 여러 번 있으면 가장 늦은 관측과 알려진 language로 합친다.
func (s *ServiceStore) Observe(ctx context.Context, tenant authz.TenantID, obs []ServiceObservation) (int, error) {
	obs = mergeObservations(obs)
	if len(obs) == 0 {
		return 0, nil
	}
	rejected := 0
	err := s.db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		rejected = 0
		ids, _, _, _, langs, seen := observationColumns(obs)
		rows, err := tx.Query(ctx, `
			UPDATE services AS s SET
			  last_seen = GREATEST(s.last_seen, t.seen),
			  language  = COALESCE(t.lang, s.language)
			FROM unnest($1::uuid[], $2::text[], $3::timestamptz[]) AS t(id, lang, seen)
			WHERE s.tenant_id = app_tenant_id() AND s.service_id = t.id
			RETURNING s.service_id::text`,
			ids, langs, seen)
		if err != nil {
			return classify("service observe", err)
		}
		existing, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return classify("service observe", err)
		}
		known := make(map[string]bool, len(existing))
		for _, id := range existing {
			known[id] = true
		}
		var fresh []ServiceObservation
		for _, o := range obs {
			if !known[o.ServiceID] {
				fresh = append(fresh, o)
			}
		}
		if len(fresh) == 0 {
			return nil
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM services WHERE tenant_id = app_tenant_id()`).Scan(&count); err != nil {
			return classify("service count", err)
		}
		room := max(MaxServicesPerTenant-count, 0)
		if len(fresh) > room {
			rejected = len(fresh) - room
			fresh = fresh[:room]
		}
		if len(fresh) == 0 {
			return nil
		}
		ids, envs, nss, names, langs, seen := observationColumns(fresh)
		// 다른 replica가 먼저 넣었으면 그대로 둔다(그쪽 last_seen이 이미 최근이다).
		_, err = tx.Exec(ctx, `
			INSERT INTO services (tenant_id, service_id, environment, namespace, name, language, first_seen, last_seen)
			SELECT app_tenant_id(), id, env, ns, nm, lang, seen, seen
			FROM unnest($1::uuid[], $2::text[], $3::text[], $4::text[], $5::text[], $6::timestamptz[]) AS t(id, env, ns, nm, lang, seen)
			ON CONFLICT (tenant_id, service_id) DO NOTHING`,
			ids, envs, nss, names, langs, seen)
		return classify("service observe", err)
	})
	if err != nil {
		return 0, err
	}
	return rejected, nil
}

// mergeObservations는 같은 service_id를 하나로 합치고 이름순으로 정렬한다(상한에 걸릴 때 어떤 서비스가 남을지 결정적이다).
func mergeObservations(obs []ServiceObservation) []ServiceObservation {
	byID := make(map[string]ServiceObservation, len(obs))
	for _, o := range obs {
		cur, ok := byID[o.ServiceID]
		if !ok {
			byID[o.ServiceID] = o
			continue
		}
		if o.SeenAt.After(cur.SeenAt) {
			cur.SeenAt = o.SeenAt
		}
		if o.Language != "" && (cur.Language == "" || !o.SeenAt.Before(cur.SeenAt)) {
			cur.Language = o.Language
		}
		byID[o.ServiceID] = cur
	}
	out := make([]ServiceObservation, 0, len(byID))
	for _, o := range byID {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ServiceID < out[j].ServiceID
	})
	return out
}

func observationColumns(obs []ServiceObservation) (ids, envs, nss, names []string, langs []*string, seen []time.Time) {
	ids, envs, nss, names = make([]string, len(obs)), make([]string, len(obs)), make([]string, len(obs)), make([]string, len(obs))
	langs, seen = make([]*string, len(obs)), make([]time.Time, len(obs))
	for i, o := range obs {
		ids[i], envs[i], nss[i], names[i], seen[i] = o.ServiceID, o.Environment, o.Namespace, o.Name, o.SeenAt
		if o.Language != "" {
			l := o.Language
			langs[i] = &l
		}
	}
	return ids, envs, nss, names, langs, seen
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
	allowedEnvs := restrictedEnvironments(p) // nil이면 제한 없음
	archivedBefore := q.Now.Add(-ServiceArchivedAfter)
	var afterName, afterEnv string
	afterID := "00000000-0000-0000-0000-000000000000" // 위치 없음: 쓰이지 않지만 uuid로 bind된다
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
			  AND ($4 OR last_seen > $5)
			  AND (NOT $6 OR (name_normalized, environment, service_id) > ($7, $8, $9::uuid))
			ORDER BY name_normalized, environment, service_id
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

// MaxResolveNames는 ResolveServiceNames 한 번의 이름 상한이다(filter 조건 20 × in 값 100).
const MaxResolveNames = 2000

// ResolveServiceNames는 서비스 이름(대소문자 무시, name_normalized) → principal이 볼 수 있는 service_id 목록이다
// (log 검색의 service.name, ADR 0039). environment로 제한된 key는 허용된 environment의 서비스만 받는다.
// archived 서비스도 포함한다 — 조회 범위 안 원본이 있을 수 있다. 결과의 key는 입력 이름 그대로다. 없는 이름은 key가 없다.
func (s *ServiceStore) ResolveServiceNames(ctx context.Context, p authz.Principal, names []string) (map[string][]string, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, err
	}
	if len(names) > MaxResolveNames {
		return nil, errors.New("controldb: too many service names to resolve")
	}
	out := map[string][]string{}
	if len(names) == 0 {
		return out, nil
	}
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT n.name, s.service_id::text
			FROM unnest($1::text[]) AS n(name)
			JOIN services s ON s.tenant_id = app_tenant_id() AND s.name_normalized = lower(n.name)
			WHERE ($2::text[] IS NULL OR s.environment = ANY($2))
			ORDER BY n.name, s.service_id`,
			names, restrictedEnvironments(p))
		if err != nil {
			return classify("resolve service names", err)
		}
		defer rows.Close()
		for rows.Next() {
			var name, id string
			if err := rows.Scan(&name, &id); err != nil {
				return classify("scan service name", err)
			}
			out[name] = append(out[name], id)
		}
		return classify("resolve service names", rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// EnvironmentServiceIDs는 environment로 제한된 principal이 볼 수 있는 서비스(허용 environment의 catalog 항목)의 service_id다.
// service_id는 environment를 포함한 자연 키의 hash라(envelope.ServiceID) 이 집합에 있는 행은 허용 environment의 것이다.
// log처럼 행에 environment가 없는 signal의 mandatory predicate로 쓴다(ADR 0039). 제한 없는 principal에는 쓰지 않는다.
// catalog에 등록되지 않은 서비스의 행은 보이지 않는다(fail closed).
func (s *ServiceStore) EnvironmentServiceIDs(ctx context.Context, p authz.Principal) ([]string, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, err
	}
	envs := restrictedEnvironments(p)
	if envs == nil {
		return nil, errors.New("controldb: principal is not environment-restricted")
	}
	var ids []string
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT service_id::text FROM services
			WHERE tenant_id = app_tenant_id() AND environment = ANY($1)
			ORDER BY service_id`, envs)
		if err != nil {
			return classify("environment services", err)
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return classify("environment services", err)
	})
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// restrictedEnvironments는 environment로 제한된 principal의 허용 environment다. 제한이 없으면 nil이다.
func restrictedEnvironments(p authz.Principal) []string {
	if !p.EnvironmentRestricted() {
		return nil
	}
	envs := p.Environments()
	if envs == nil {
		envs = []string{} // 제한됐는데 비어 있으면 아무것도 보지 않는다(NULL = 제한 없음으로 바뀌지 않게)
	}
	return envs
}
