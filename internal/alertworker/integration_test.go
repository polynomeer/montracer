//go:build integration

package alertworker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/alerting"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/monitor"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// 실제 PostgreSQL(정의·lease·상태·outbox) + ClickHouse(metric_1m, query 계정·row policy)로 worker 한 주기를 돌린다.
//
//	MONTRACER_TEST_PG_APP_DSN · MONTRACER_TEST_PG_ADMIN_DSN · MONTRACER_TEST_CH_QUERY_DSN · MONTRACER_TEST_CH_ADMIN_DSN

func env(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("%s 필요 (make test-integration)", key)
	}
	return v
}

// tenantOnly는 실제 AlertStore를 쓰되 할 일 목록을 이 테스트의 tenant로 좁힌다(같은 DB를 쓰는 다른 테스트의 monitor를 건드리지 않게).
type tenantOnly struct {
	*controldb.AlertStore
	tenants map[authz.TenantID]bool
}

func (s tenantOnly) DueMonitors(ctx context.Context, now time.Time, limit int) ([]controldb.DueMonitor, error) {
	due, err := s.AlertStore.DueMonitors(ctx, now, 1000)
	var out []controldb.DueMonitor
	for _, d := range due {
		if s.tenants[d.Tenant] {
			out = append(out, d)
		}
	}
	return out, err
}

func provision(t *testing.T, db *controldb.DB) authz.TenantID {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, env(t, "MONTRACER_TEST_PG_ADMIN_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()
	id := newUUID()
	if _, err := admin.Exec(ctx, `INSERT INTO tenants (id, region, cell, status) VALUES ($1, 'local', 'cell-0', 'active')`, id); err != nil {
		t.Fatal(err)
	}
	tenant, _ := authz.ParseTenantID(id)
	if err := db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, 'dev', 'developer')`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return tenant
}

func createErrorRatioMonitor(t *testing.T, db *controldb.DB, tenant authz.TenantID) {
	t.Helper()
	ctx := context.Background()
	raw := []byte(`{"name":"checkout 오류율","query":{"kind":"error_ratio","group_by":["service.name"]},"window_seconds":300,
		"evaluation_seconds":60,"condition":{"operator":"gt","threshold":0.02},"for_seconds":0,"no_data":{"action":"no_data"}}`)
	s, _, err := monitor.Normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	n := time.Now()
	dev, err := authz.NewUserPrincipal(tenant, "dev", authz.RoleDeveloper, n, n)
	if err != nil {
		t.Fatal(err)
	}
	store := controldb.NewMonitorStore(db)
	_, _, err = store.CreateMonitor(ctx, dev, controldb.MonitorWrite{Name: s.Name, Spec: s.Canonical(), Enabled: true, EvaluationSeconds: s.EvaluationSeconds},
		controldb.IdempotencyRequest{Key: "k", MethodPath: "POST /api/v1/monitors", RequestHash: sha256.Sum256(raw)}, "req",
		func(controldb.Monitor) (controldb.StoredResponse, error) {
			return controldb.StoredResponse{Status: 201, Body: []byte(`{}`)}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
}

// insertRequests는 window 5개(1분)에 걸쳐 status별 요청 수를 http.server.request.duration histogram으로 넣는다.
func insertRequests(t *testing.T, tenant authz.TenantID, end time.Time, ok, errors uint64) {
	t.Helper()
	opts, err := clickhouse.ParseDSN(env(t, "MONTRACER_TEST_CH_ADMIN_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx := context.Background()
	b, err := conn.PrepareBatch(ctx, `INSERT INTO metric_1m (tenant_id, metric_name, stream_id, window_start,
		type, temporality, is_monotonic, unit, resource_json, attributes_json, samples, has_value, last, min, max, total,
		has_increase, increase, has_histogram, count, hist_sum, bounds, buckets, resets, flags, partial, revision,
		computed_at, expires_at)`)
	if err != nil {
		t.Fatal(err)
	}
	resource := `{"deployment.environment.name":"prod","service.name":"checkout"}`
	for _, s := range []struct {
		status string
		n      uint64
	}{{"200", ok}, {"500", errors}} {
		var stream [16]byte
		_, _ = rand.Read(stream[:])
		per := s.n / 5
		for i := 1; i <= 5; i++ {
			w := end.Add(-time.Duration(i) * time.Minute)
			if err := b.Append(tenant.String(), "http.server.request.duration", string(stream[:]), w, "histogram", "cumulative", true, "s",
				resource, `{"http.response.status_code":"`+s.status+`"}`, uint32(1), false, 0.0, 0.0, 0.0, 0.0, false, 0.0,
				true, per, float64(per)*0.1, []float64{0.5}, []uint64{per, 0}, uint32(0), []string{}, false, uint64(1),
				time.Now(), w.Add(24*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := b.Send(); err != nil {
		t.Fatal(err)
	}
}

func alertEvents(t *testing.T, db *controldb.DB, tenant authz.TenantID) []map[string]any {
	t.Helper()
	ctx := context.Background()
	var out []map[string]any
	if err := db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT payload FROM outbox WHERE type = 'alert.state_changed' ORDER BY occurred_at, event_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			var p map[string]any
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func instanceState(t *testing.T, db *controldb.DB, tenant authz.TenantID) (states []string) {
	t.Helper()
	ctx := context.Background()
	if err := db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT state FROM alert_instances`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			states = append(states, s)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return states
}

// 5% 오류(tenant A)는 경보가 되고, 정확히 2%(tenant B, gt 0.02)는 경보가 아니다. 각 tenant의 monitor는 자기 데이터만 본다.
func TestWorkerEndToEnd(t *testing.T) {
	ctx := context.Background()
	db, err := controldb.Open(ctx, env(t, "MONTRACER_TEST_PG_APP_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	ch, err := telemetrystore.OpenQuery(ctx, env(t, "MONTRACER_TEST_CH_QUERY_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	a, b := provision(t, db), provision(t, db)
	createErrorRatioMonitor(t, db, a)
	createErrorRatioMonitor(t, db, b)
	end := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	insertRequests(t, a, end, 950, 50)
	insertRequests(t, b, end, 980, 20)

	clock := end.Add(20 * time.Second)
	w, err := New(Config{Store: tenantOnly{controldb.NewAlertStore(db), map[authz.TenantID]bool{a: true, b: true}}, Metrics: ch,
		Worker: "it-worker", Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := w.Tick(ctx); err != nil || n != 2 {
		t.Fatalf("tick: %d %v", n, err)
	}
	if got := instanceState(t, db, a); len(got) != 1 || got[0] != "ALERT" {
		t.Errorf("A states = %v", got)
	}
	if got := instanceState(t, db, b); len(got) != 1 || got[0] != "OK" {
		t.Errorf("B states = %v", got)
	}
	evA := alertEvents(t, db, a)
	if len(evA) != 1 || evA[0]["to"] != "ALERT" || evA[0]["opened"] != true || evA[0]["episode_id"] == nil {
		t.Fatalf("A events = %+v", evA)
	}
	if labels, _ := evA[0]["labels"].(map[string]any); labels["service.name"] != "checkout" {
		t.Errorf("A labels = %+v", evA[0]["labels"])
	}
	if v, _ := evA[0]["value"].(float64); v < 0.0499 || v > 0.0501 {
		t.Errorf("A value = %v", evA[0]["value"])
	}
	// 첫 평가의 OK는 전이가 아니다(초기 상태 OK) → event 없음
	if evB := alertEvents(t, db, b); len(evB) != 0 {
		t.Errorf("B events = %+v", evB)
	}
	// 같은 slot은 다시 평가하지 않는다
	if n, err := w.Tick(ctx); err != nil || n != 0 {
		t.Errorf("same slot re-evaluated: %d %v", n, err)
	}
}

// row1m은 metric_1m 결과 행 하나다(rollup 주기를 기다리지 않고 admin 계정으로 직접 넣는다).
type row1m struct {
	metric, typ, service, status string
	stream                       [16]byte
	at                           time.Time
	gauge                        *[3]float64 // total(=last), min, max
	count                        uint64
	bounds                       []float64
	buckets                      []uint64
}

func insert1m(t *testing.T, tenant authz.TenantID, rows ...row1m) {
	t.Helper()
	opts, err := clickhouse.ParseDSN(env(t, "MONTRACER_TEST_CH_ADMIN_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx := context.Background()
	b, err := conn.PrepareBatch(ctx, `INSERT INTO metric_1m (tenant_id, metric_name, stream_id, window_start,
		type, temporality, is_monotonic, unit, resource_json, attributes_json, samples, has_value, last, min, max, total,
		has_increase, increase, has_histogram, count, hist_sum, bounds, buckets, resets, flags, partial, revision,
		computed_at, expires_at)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		attrs := `{}`
		if r.status != "" {
			attrs = `{"http.response.status_code":"` + r.status + `"}`
		}
		var total, lo, hi float64
		if r.gauge != nil {
			total, lo, hi = r.gauge[0], r.gauge[1], r.gauge[2]
		}
		bounds, buckets := r.bounds, r.buckets
		if bounds == nil {
			bounds, buckets = []float64{}, []uint64{}
		}
		if err := b.Append(tenant.String(), r.metric, string(r.stream[:]), r.at, r.typ, "cumulative", r.typ != "gauge", "s",
			`{"deployment.environment.name":"prod","service.name":"`+r.service+`"}`, attrs, uint32(1), r.gauge != nil, total, lo, hi, total,
			false, 0.0, r.typ == "histogram", r.count, float64(r.count)*0.1, bounds, buckets, uint32(0), []string{}, false, uint64(1),
			time.Now(), r.at.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Send(); err != nil {
		t.Fatal(err)
	}
}

func stream() [16]byte {
	var s [16]byte
	_, _ = rand.Read(s[:])
	return s
}

func querySpec(t *testing.T, query string) monitor.Spec {
	t.Helper()
	s, _, err := monitor.Normalize([]byte(`{"name":"dry-run","query":` + query + `,"window_seconds":300,"evaluation_seconds":60,` +
		`"condition":{"operator":"gt","threshold":0.02},"for_seconds":0,"no_data":{"action":"no_data"}}`))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// liveAndMerged는 같은 window를 주기 평가의 window 조회와 dry-run의 1분 bucket 합치기로 각각 계산한다.
func liveAndMerged(t *testing.T, ch *telemetrystore.QueryStore, p authz.Principal, s monitor.Spec, now, wm time.Time) (alerting.Result, alerting.Result, alerting.DryRunPlan, []telemetrystore.MetricBucket) {
	t.Helper()
	ctx := context.Background()
	w, status := alerting.EvaluationWindow(s, now, wm)
	if status != alerting.WindowOK {
		t.Fatal(status)
	}
	live, err := ch.MetricBuckets(ctx, p, alerting.Query(s, w), now)
	if err != nil {
		t.Fatal(err)
	}
	plan := alerting.PlanDryRun(s, now, wm, telemetrystore.Budget{MaxResultRows: alerting.DryRunMaxBuckets, MaxExecutionTime: 10 * time.Second})
	var minute []telemetrystore.MetricBucket
	for _, q := range plan.Queries {
		bs, err := ch.MetricBuckets(ctx, p, q, now)
		if err != nil {
			t.Fatal(err)
		}
		minute = append(minute, bs...)
	}
	if last := plan.Ends[len(plan.Ends)-1]; !last.Equal(w.End) {
		t.Fatalf("last dry-run point %v != live window end %v", last, w.End)
	}
	byGroup := map[string][]telemetrystore.MetricBucket{}
	for _, b := range minute {
		if !b.StepStart.Before(w.Start) && b.StepStart.Before(w.End) {
			k := fmt.Sprint(b.Group)
			byGroup[k] = append(byGroup[k], b)
		}
	}
	var merged []telemetrystore.MetricBucket
	for _, bs := range byGroup {
		m := alerting.MergeBuckets(bs)
		m.StepStart = w.Start
		merged = append(merged, m)
	}
	return alerting.Compute(s, live), alerting.Compute(s, merged), plan, minute
}

func sameResult(t *testing.T, name string, live, dry alerting.Result) {
	t.Helper()
	if len(live.Groups) != len(dry.Groups) || len(live.Groups) == 0 {
		t.Fatalf("%s: groups live=%+v dry=%+v", name, live.Groups, dry.Groups)
	}
	for i := range live.Groups {
		l, d := live.Groups[i], dry.Groups[i]
		if l.Key != d.Key || l.Reason != d.Reason || l.Partial != d.Partial || (l.Value == nil) != (d.Value == nil) ||
			(l.Value != nil && math.Abs(*l.Value-*d.Value) > 1e-9) {
			t.Errorf("%s group %s: live %+v (%v) != dry-run %+v (%v)", name, l.Key, l, deref(l.Value), d, deref(d.Value))
		}
	}
}

func deref(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func systemPrincipal(t *testing.T) (authz.TenantID, authz.Principal) {
	t.Helper()
	tenant, err := authz.ParseTenantID(newUUID())
	if err != nil {
		t.Fatal(err)
	}
	p, err := authz.NewSystemPrincipal(tenant, Subject, authz.TelemetryRead)
	if err != nil {
		t.Fatal(err)
	}
	return tenant, p
}

func openCH(t *testing.T) *telemetrystore.QueryStore {
	t.Helper()
	ch, err := telemetrystore.OpenQuery(context.Background(), env(t, "MONTRACER_TEST_CH_QUERY_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	return ch
}

// dry-run(ADR 0052)은 1분 bucket을 받아 window마다 합친다. 실제 ClickHouse에서 그 값·사유·partial이 주기 평가의 window 조회와 같아야 한다
// (같은 데이터로 dry-run과 경보가 다른 판정을 하지 않는다). 마지막 dry-run 시점은 주기 평가의 window와 같다.
//   - error_ratio: status별 count, 서비스 20개(1분 step 24시간 결과가 interactive 기본 예산 10,000행을 넘는다 — 예산을 실어야 한다)
//   - metric p95·count: histogram, stream 셋(하나는 계속, 둘은 window 중간에 교체 → partial), 서비스 하나는 경계가 다른 분이 섞임
//   - metric avg·min·max: gauge, stream 둘·stream 교체(값 통계는 교체로 partial이 되지 않는다 — 저장소 조회와 같게)
func TestDryRunMatchesWindowQuery(t *testing.T) {
	ch := openCH(t)
	tenant, p := systemPrincipal(t)
	end := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	var rows []row1m
	for i := 0; i < 20; i++ {
		svc := fmt.Sprintf("svc-%02d", i)
		ok, bad := stream(), stream()
		for m := 1; m <= 300; m++ { // 20 서비스 × status 2 × 300분 = 12,000행
			at := end.Add(-time.Duration(m) * time.Minute)
			errs := uint64(i % 5)
			rows = append(rows, row1m{metric: "http.server.request.duration", typ: "histogram", service: svc, status: "200", stream: ok, at: at, count: 200 - errs, bounds: []float64{0.5}, buckets: []uint64{200 - errs, 0}},
				row1m{metric: "http.server.request.duration", typ: "histogram", service: svc, status: "500", stream: bad, at: at, count: errs, bounds: []float64{0.5}, buckets: []uint64{errs, 0}})
		}
	}
	steady, first, second, odd := stream(), stream(), stream(), stream()
	for m := 1; m <= 10; m++ {
		at := end.Add(-time.Duration(m) * time.Minute)
		n := uint64(10 * m)
		rows = append(rows, row1m{metric: "it.latency", typ: "histogram", service: "checkout", stream: steady, at: at, count: n, bounds: []float64{0.1, 1}, buckets: []uint64{n / 2, n / 4, n - n/2 - n/4}})
		rotating := first
		if m <= 2 {
			rotating = second // window 마지막 2분은 다른 stream(pod 교체)
		}
		rows = append(rows, row1m{metric: "it.latency", typ: "histogram", service: "cart", stream: rotating, at: at, count: n, bounds: []float64{0.1, 1}, buckets: []uint64{0, n, 0}})
		bounds := []float64{0.1, 1}
		if m == 3 {
			bounds = []float64{0.2, 1}
		}
		rows = append(rows, row1m{metric: "it.latency", typ: "histogram", service: "search", stream: odd, at: at, count: n, bounds: bounds, buckets: []uint64{n, 0, 0}})
		g := float64(m)
		rows = append(rows, row1m{metric: "it.gauge", typ: "gauge", service: "checkout", stream: steady, at: at, gauge: &[3]float64{g * 10, g, g * 3}},
			row1m{metric: "it.gauge", typ: "gauge", service: "checkout", stream: first, at: at, gauge: &[3]float64{g * 20, g / 2, g * 5}},
			row1m{metric: "it.gauge", typ: "gauge", service: "cart", stream: rotating, at: at, gauge: &[3]float64{g, g, g}}) // 교체: gauge는 partial 아님
	}
	insert1m(t, tenant, rows...)

	now := end.Add(20 * time.Second)
	wm, err := ch.RollupWatermark(context.Background(), p, time.Minute, now.Add(-24*time.Hour), now)
	if err != nil || !wm.Equal(end) {
		t.Fatalf("watermark %v %v", wm, err)
	}
	errorRatio := querySpec(t, `{"kind":"error_ratio","group_by":["service.name"]}`)
	live, dry, plan, minute := liveAndMerged(t, ch, p, errorRatio, now, wm)
	sameResult(t, "error_ratio", live, dry)
	if len(live.Groups) != 20 || len(minute) < 10_000 {
		t.Errorf("groups %d, minute buckets %d (want > interactive 10,000-row budget)", len(live.Groups), len(minute))
	}
	for _, agg := range []string{"p95", "count", "p50"} {
		live, dry, _, _ := liveAndMerged(t, ch, p, querySpec(t, `{"kind":"metric","metric":"it.latency","aggregation":"`+agg+`","group_by":["service.name"]}`), now, wm)
		sameResult(t, agg, live, dry)
		for _, g := range live.Groups {
			switch g.Labels["service.name"] {
			case "cart":
				if !g.Partial {
					t.Errorf("%s cart: stream rotation not partial in live query", agg)
				}
			case "search":
				if agg != "count" && g.Reason != "bounds_mismatch" {
					t.Errorf("%s search: %+v", agg, g)
				}
			}
		}
	}
	for _, agg := range []string{"avg", "min", "max"} {
		live, dry, _, _ := liveAndMerged(t, ch, p, querySpec(t, `{"kind":"metric","metric":"it.gauge","aggregation":"`+agg+`","group_by":["service.name"]}`), now, wm)
		sameResult(t, agg, live, dry)
	}

	// 전체 dry-run: error_ratio 서비스 중 오류 2건 이상(1%↑)… 4/200 = 2%는 발화 안 함, 그 외 0~1.5%도 안 함 → 발화 없음, 결측 아님
	r, err := alerting.DryRun(context.Background(), errorRatio, plan, minute)
	if err != nil || r.GroupsTotal != 20 || r.Coverage.Evaluated != r.Evaluations || r.Reason != "" {
		t.Fatalf("dry-run = %+v %v", r, err)
	}
	for _, g := range r.Groups {
		if len(g.Intervals) != 0 {
			t.Errorf("%s fired at <= 2%%: %+v", g.Key, g)
		}
	}
	// 예산 없이(interactive 기본 10,000행) 같은 조회는 저장소가 거절한다 — dry-run이 예산을 실어야 하는 이유
	q := plan.Queries[0]
	q.Budget = telemetrystore.Budget{}
	var budget interface{ BudgetExceeded() map[string]any }
	if _, err := ch.MetricBuckets(context.Background(), p, q, now); !errors.As(err, &budget) {
		t.Errorf("default budget: %v", err)
	}
}
