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

// 시험이 작은 상한으로 잘림을 확인할 수 있게 변수로 둔다.
var (
	serviceHealthMonitorLimit = ServiceHealthMaxMonitors
	serviceHealthBucketLimit  = ServiceHealthMaxBuckets
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
		statuses map[string]MonitorStatus
		// 묶음을 상한에서 잘랐을 때 마지막(일부만 읽은) monitor의 id. 이 id 이상은 기여 계산에서 뺀다 — 일부만 읽은 monitor가
		// 덜 나쁘게 보이거나, 읽지 못한 monitor가 "group 없음"으로 잘못 분류되지 않게(ADR 0073 §4)
		cutFrom string
	)
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`, serviceHealthTimeout); err != nil {
			return classify("set statement timeout", err)
		}
		// 지운 monitor는 빼고 결정적 순서로 상한까지
		rows, err := tx.Query(ctx, `
			SELECT id::text, name, spec FROM monitors WHERE tenant_id = app_tenant_id() AND deleted_at IS NULL
			ORDER BY created_at, id LIMIT $1`, serviceHealthMonitorLimit+1)
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
			if len(monitors) == serviceHealthMonitorLimit {
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
			FROM alert_instances ai JOIN monitor_schedule s ON s.tenant_id = ai.tenant_id AND s.monitor_id = ai.monitor_id AND s.revision = ai.revision
			WHERE ai.tenant_id = app_tenant_id() AND ai.monitor_id = ANY($1::uuid[])
			GROUP BY 1, 2, 3, 4, 5 ORDER BY 1, 2, 3, 4, 5 LIMIT $2`, ids, serviceHealthBucketLimit+1)
		if err != nil {
			return classify("select service health groups", err)
		}
		n, last := 0, ""
		for rows.Next() {
			if n == serviceHealthBucketLimit {
				out.Warnings = append(out.Warnings, WarnGroupsTruncated)
				cutFrom = last
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
			last = id
			if i, ok := index[id]; ok {
				monitors[i].Buckets = append(monitors[i].Buckets, b)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return classify("select service health groups", err)
		}
		statuses, err = monitorStatusesTx(ctx, tx, ids, now)
		return err
	})
	if err != nil {
		return ServiceHealth{}, err
	}
	var contributions []servicehealth.Contribution
	for _, m := range monitors {
		// 묶음 순서는 monitor id다. 잘린 경계의 monitor와 그 뒤는 일부만 읽었거나 읽지 못했다
		if cutFrom != "" && m.ID >= cutFrom {
			continue
		}
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
	if len(out.Warnings) > 0 {
		for i := range out.Services {
			out.Services[i].Partial = true
		}
	}
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
