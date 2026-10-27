package controldb

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/monitor"
	"github.com/polynomeer/montracer/internal/servicehealth"
)

// 서비스 상태 조회의 비용 상한(ADR 0073 §4). 넘으면 그만 읽고 경고를 돌려준다 — 화면은 응답에 없는 서비스를
// "판정 없음"이 아니라 "확인할 수 없음"으로 보인다.
const (
	ServiceHealthMaxMonitors = 1000
	ServiceHealthMaxBuckets  = 20000
	serviceHealthTimeout     = "5s"
)

// 잘림 경고(meta.warnings).
const (
	WarnMonitorsTruncated = "monitors_truncated"
	WarnGroupsTruncated   = "groups_truncated"
)

// ServiceHealth는 서비스마다 걸린 monitor의 합친 상태다.
type ServiceHealth struct {
	Services     []servicehealth.Service
	Unattributed []servicehealth.MonitorRef // 서비스를 알 수 없는 monitor(group_by service.name만, group 없음), 나쁜 순
	Warnings     []string
}

// MaxUnattributed는 돌려줄 "서비스를 알 수 없는 monitor" 수다.
const MaxUnattributed = 20

// ServiceHealth는 tenant의 monitor를 서비스에 연결해 합친다(ADR 0073). 권한은 상태 조회 권한이다(tenant 전체 —
// environment 제한 principal은 거절). environment가 있으면 그 환경과 전체 환경 monitor의 기여를 그 환경 행으로 합친다.
func (s *MonitorStore) ServiceHealth(ctx context.Context, p authz.Principal, environment *string, now time.Time) (ServiceHealth, error) {
	if err := AuthorizeMonitorState(p); err != nil {
		return ServiceHealth{}, err
	}
	var (
		out      ServiceHealth
		monitors []servicehealth.Monitor
		index    = map[string]int{}
	)
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+serviceHealthTimeout+"'"); err != nil {
			return classify("set statement timeout", err)
		}
		// 지운 monitor는 빼고 결정적 순서로 상한까지
		rows, err := tx.Query(ctx, `
			SELECT id::text, name, spec FROM monitors WHERE deleted_at IS NULL
			ORDER BY created_at, id LIMIT $1`, ServiceHealthMaxMonitors+1)
		if err != nil {
			return classify("select monitors for service health", err)
		}
		for rows.Next() {
			var (
				id, name string
				raw      []byte
			)
			if err := rows.Scan(&id, &name, &raw); err != nil {
				rows.Close()
				return classify("scan monitor for service health", err)
			}
			if len(monitors) == ServiceHealthMaxMonitors {
				out.Warnings = append(out.Warnings, WarnMonitorsTruncated)
				break
			}
			var spec monitor.Spec
			if err := json.Unmarshal(raw, &spec); err != nil {
				rows.Close()
				return fmt.Errorf("controldb: monitor spec: %w", err)
			}
			index[id] = len(monitors)
			monitors = append(monitors, servicehealth.Monitor{ID: id, Name: name, Filter: spec.Query.Filter, GroupBy: spec.Query.GroupBy})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return classify("select monitors for service health", err)
		}
		if len(monitors) == 0 {
			return nil
		}
		ids := make([]string, len(monitors))
		for i, m := range monitors {
			ids[i] = m.ID
		}
		// 지금 revision의 group을 서비스 label·상태로 묶어 읽는다(행 수 대신 묶음 수)
		rows, err = tx.Query(ctx, `
			SELECT ai.monitor_id::text,
			       ai.labels->'labels'->>'service.namespace', ai.labels->'labels'->>'service.name',
			       ai.labels->'labels'->>'deployment.environment.name',
			       ai.state, count(*), min(ai.state_since), count(*) FILTER (WHERE ai.state_since IS NULL)
			FROM alert_instances ai JOIN monitor_schedule s ON s.monitor_id = ai.monitor_id AND s.revision = ai.revision
			WHERE ai.monitor_id = ANY($1::uuid[])
			GROUP BY 1, 2, 3, 4, 5 ORDER BY 1, 2, 3, 4, 5 LIMIT $2`, ids, ServiceHealthMaxBuckets+1)
		if err != nil {
			return classify("select service health groups", err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			if n == ServiceHealthMaxBuckets {
				out.Warnings = append(out.Warnings, WarnGroupsTruncated)
				break
			}
			n++
			var (
				id       string
				b        servicehealth.Bucket
				earliest *time.Time
				unknown  int
			)
			if err := rows.Scan(&id, &b.Namespace, &b.Name, &b.Environment, &b.State, &b.Count, &earliest, &unknown); err != nil {
				return classify("scan service health group", err)
			}
			b.EarliestSince, b.UnknownSince = utcPtr(earliest), unknown > 0
			if i, ok := index[id]; ok {
				monitors[i].Buckets = append(monitors[i].Buckets, b)
			}
		}
		return classify("select service health groups", rows.Err())
	})
	if err != nil {
		return ServiceHealth{}, err
	}
	if len(monitors) == 0 {
		return out, nil
	}
	ids := make([]string, len(monitors))
	for i, m := range monitors {
		ids[i] = m.ID
	}
	statuses, err := s.MonitorStatuses(ctx, p, ids, now)
	if err != nil {
		return ServiceHealth{}, err
	}
	var contributions []servicehealth.Contribution
	for _, m := range monitors {
		st, ok := statuses[m.ID]
		if !ok {
			continue // schedule이 없다(만드는 중)
		}
		m.Summary, m.Stale = st.State, st.Stale
		cs, unattributed := servicehealth.Contributions(m)
		contributions = append(contributions, cs...)
		if unattributed {
			out.Unattributed = append(out.Unattributed, servicehealth.MonitorRef{ID: m.ID, Name: m.Name, State: m.Summary, Stale: m.Stale})
		}
	}
	out.Services = servicehealth.Aggregate(contributions, environment)
	sortRefs(out.Unattributed)
	if len(out.Unattributed) > MaxUnattributed {
		out.Unattributed = out.Unattributed[:MaxUnattributed]
	}
	return out, nil
}

func sortRefs(refs []servicehealth.MonitorRef) {
	sort.Slice(refs, func(i, j int) bool {
		a, b := refs[i], refs[j]
		if ra, rb := servicehealth.Rank(a.State, a.Stale), servicehealth.Rank(b.State, b.Stale); ra != rb {
			return ra < rb
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
}
