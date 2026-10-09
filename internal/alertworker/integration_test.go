//go:build integration

package alertworker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5"

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
