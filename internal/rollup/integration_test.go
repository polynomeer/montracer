//go:build integration

package rollup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// 통합 테스트: ClickHouse rollup 계정(migration 00003).
//
//	MONTRACER_TEST_CH_ROLLUP_DSN, MONTRACER_TEST_CH_ADMIN_DSN, MONTRACER_TEST_CH_QUERY_DSN

func env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("%s 필요 (make test-integration)", k)
	}
	return v
}

func open(t *testing.T, dsnEnv string) driver.Conn {
	t.Helper()
	opts, err := clickhouse.ParseDSN(env(t, dsnEnv))
	if err != nil {
		t.Fatal(err)
	}
	c, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func randomUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6], b[8] = (b[6]&0x0f)|0x40, (b[8]&0x3f)|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func TestRollupEndToEnd(t *testing.T) {
	ctx := context.Background()
	store, err := OpenClickHouse(ctx, env(t, "MONTRACER_TEST_CH_ROLLUP_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	admin := open(t, "MONTRACER_TEST_CH_ADMIN_DSN")

	// 원본: tenant A의 monotonic cumulative counter와 histogram, tenant B의 같은 stream ID counter
	tenantA, tenantB := randomUUID(t), randomUUID(t)
	var counter, histo [16]byte
	_, _ = rand.Read(counter[:])
	_, _ = rand.Read(histo[:])
	now := time.Now().UTC().Truncate(time.Second)
	watermark := now.Add(-2 * time.Minute).Truncate(time.Minute)
	w := watermark.Add(-2 * time.Minute) // 닫힌 window 하나
	start := w.Add(-time.Hour)
	insert := func(tenant string, stream [16]byte, typ, temp string, end time.Time, value float64, count uint64, bounds []float64, buckets []uint64) {
		t.Helper()
		var ph [16]byte
		_, _ = rand.Read(ph[:])
		batch, err := admin.PrepareBatch(ctx, `INSERT INTO metric_points (tenant_id, stream_id, metric_name, unit, type,
			temporality, is_monotonic, start_time, end_time, point_hash, value, count, sum, bounds, buckets, payload,
			resource_json, attributes_json, version, expires_at)`)
		if err != nil {
			t.Fatal(err)
		}
		if err := batch.Append(tenant, string(stream[:]), "http.requests", "1", typ, temp, true, start, end, string(ph[:]),
			value, count, float64(count), bounds, buckets, "", "{}", "{}", uint64(1), end.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := batch.Send(); err != nil {
			t.Fatal(err)
		}
	}
	insert(tenantA, counter, "sum", "cumulative", w.Add(-30*time.Second), 100, 0, []float64{}, []uint64{}) // 기준점
	insert(tenantA, counter, "sum", "cumulative", w.Add(15*time.Second), 130, 0, []float64{}, []uint64{})
	insert(tenantA, counter, "sum", "cumulative", w.Add(45*time.Second), 170, 0, []float64{}, []uint64{})
	insert(tenantA, histo, "histogram", "delta", w.Add(10*time.Second), math.NaN(), 3, []float64{10}, []uint64{2, 1})
	insert(tenantA, histo, "histogram", "delta", w.Add(40*time.Second), math.NaN(), 2, []float64{10}, []uint64{0, 2})
	insert(tenantB, counter, "sum", "cumulative", w.Add(15*time.Second), 5, 0, []float64{}, []uint64{}) // 기준점 없음

	// 정상 수집 모사: tenant마다 다른 stream의 최신 point로 그 tenant의 최대 관측 시각을 now 근처에 둔다
	// (watermark는 tenant별 = 최대 관측 − 2분, ADR 0026 §2). 이 point의 window는 아직 닫히지 않는다.
	for _, tenant := range []string{tenantA, tenantB} {
		var hb [16]byte
		_, _ = rand.Read(hb[:])
		insert(tenant, hb, "gauge", "unspecified", now.Add(-time.Second), 1, 0, []float64{}, []uint64{})
	}

	j := New(Config{Store: store, Now: func() time.Time { return now }})
	if err := j.Cycle(ctx); err != nil {
		t.Fatal(err)
	}

	type row struct {
		increase     float64
		hasIncrease  bool
		partial      bool
		count        uint64
		buckets      []uint64
		flags        []string
		hasHistogram bool
	}
	read := func(tenant string, stream [16]byte) (row, bool) {
		t.Helper()
		var r row
		err := admin.QueryRow(ctx, `SELECT increase, has_increase, partial, count, buckets, flags, has_histogram
			FROM metric_1m WHERE tenant_id = ? AND stream_id = ? AND window_start = ?
			ORDER BY revision DESC LIMIT 1`, tenant, string(stream[:]), w).
			Scan(&r.increase, &r.hasIncrease, &r.partial, &r.count, &r.buckets, &r.flags, &r.hasHistogram)
		if errors.Is(err, sql.ErrNoRows) {
			return row{}, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return r, true
	}
	c, ok := read(tenantA, counter)
	if !ok || !c.hasIncrease || c.increase != 70 || c.partial {
		t.Errorf("tenant A counter = %+v ok=%v (기준점 100 → 170)", c, ok)
	}
	h, ok := read(tenantA, histo)
	if !ok || !h.hasHistogram || h.count != 5 || len(h.buckets) != 2 || h.buckets[0] != 2 || h.buckets[1] != 3 {
		t.Errorf("tenant A histogram = %+v", h)
	}
	b, ok := read(tenantB, counter)
	if !ok || b.hasIncrease || !b.partial || len(b.flags) != 1 || b.flags[0] != "missing_baseline" {
		t.Errorf("tenant B counter = %+v (누적 5를 증가량으로 세지 않는다)", b)
	}

	// 같은 내용으로 다시 돌면 아무것도 쓰지 않는다(revision 행 수 불변)
	countRows := func() uint64 {
		var n uint64
		if err := admin.QueryRow(ctx, `SELECT count() FROM metric_1m WHERE tenant_id IN (?, ?)`, tenantA, tenantB).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countRows()
	if err := j.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if after := countRows(); after != before {
		t.Errorf("unchanged windows rewritten: %d → %d rows", before, after)
	}

	// query 계정은 metric_1m에서도 자기 tenant 행만 본다(row policy, ADR 0018)
	q := open(t, "MONTRACER_TEST_CH_QUERY_DSN")
	qctx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"SQL_montracer_tenant": clickhouse.CustomSetting{Value: tenantA},
	}))
	var visible uint64
	if err := q.QueryRow(qctx, `SELECT count() FROM metric_1m WHERE tenant_id IN (?, ?)`, tenantA, tenantB).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible == 0 || visible != countTenant(t, admin, tenantA) {
		t.Errorf("query account saw %d rows, want only tenant A's", visible)
	}
}

func countTenant(t *testing.T, c driver.Conn, tenant string) uint64 {
	t.Helper()
	var n uint64
	if err := c.QueryRow(context.Background(), `SELECT count() FROM metric_1m WHERE tenant_id = ?`, tenant).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// rollup 계정이 span을 읽을 수 있으면(관리자 DSN) 기동을 거부한다.
func TestRollupRejectsPrivilegedAccount(t *testing.T) {
	_, err := OpenClickHouse(context.Background(), env(t, "MONTRACER_TEST_CH_ADMIN_DSN"))
	if !errors.Is(err, ErrRollupTooPrivileged) {
		t.Fatalf("err = %v", err)
	}
}

// rollup 계정은 metric_1m을 읽을 수 없고 span에 쓸 수 없다(최소 권한).
func TestRollupAccountLeastPrivilege(t *testing.T) {
	c := open(t, "MONTRACER_TEST_CH_ROLLUP_DSN")
	ctx := context.Background()
	var n uint64
	// 진행 위치용 세 컬럼(tenant_id, window_start, revision)만 읽고 집계 값은 읽지 못한다
	var f float64
	if err := c.QueryRow(ctx, `SELECT increase FROM metric_1m LIMIT 1`).Scan(&f); err == nil {
		t.Error("rollup account can read metric_1m values")
	}
	if err := c.QueryRow(ctx, `SELECT count() FROM metric_1m WHERE window_start > now() - INTERVAL 1 DAY`).Scan(&n); err != nil {
		t.Errorf("rollup account cannot read progress columns: %v", err)
	}
	if err := c.Exec(ctx, `INSERT INTO logs_local (tenant_id) VALUES (generateUUIDv4())`); err == nil {
		t.Error("rollup account can write logs_local")
	}
}
