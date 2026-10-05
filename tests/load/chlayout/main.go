// chlayout은 ClickHouse 원본 테이블 layout 실험이다 (작업계획서 §5.1 기술 검증 #3, D06 §03).
//
// 관리자 계정으로 합성 span을 생성하고, query 계정(row policy 적용)으로 대표 쿼리를 반복 실행해
// 지연 분포·읽은 행/바이트·저장 크기를 Markdown 표로 출력한다.
//
//	MONTRACER_CH_ADMIN_DSN=... MONTRACER_CH_QUERY_DSN=... go run ./tests/load/chlayout -spans 20000000
//
// 결과는 로컬 단일 node 수치다. production SLO 근거가 아니다 (D01 §02).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// 실험 tenant는 고정 접두어로 만들어 정리할 수 있게 한다.
func tenantID(i int) string  { return fmt.Sprintf("ee000000-0000-4000-8000-%012x", i) }
func serviceID(i int) string { return fmt.Sprintf("5e000000-0000-4000-8000-%012x", i) }

func main() {
	spans := flag.Int("spans", 20_000_000, "생성할 span 수")
	tenants := flag.Int("tenants", 10, "tenant 수 (D01 §02 MVP 10)")
	services := flag.Int("services", 100, "tenant당 서비스 수 (D01 §02 MVP 100)")
	window := flag.Duration("window", 24*time.Hour, "span 분포 시간 범위")
	runs := flag.Int("runs", 50, "쿼리별 반복 횟수")
	batch := flag.Int("batch", 1_000_000, "insert 한 번의 row 수 (part 수 결정)")
	skipGen := flag.Bool("skip-generate", false, "기존 실험 데이터 재사용")
	cleanup := flag.Bool("cleanup", false, "실험 데이터 삭제 후 종료")
	flag.Parse()
	if *spans <= 0 || *tenants <= 0 || *services <= 0 || *window <= 0 || *runs <= 0 || *batch <= 0 {
		fail(errors.New("spans·tenants·services·window·runs·batch는 양수여야 한다"))
	}

	ctx := context.Background()
	admin := mustOpen(ctx, "MONTRACER_CH_ADMIN_DSN")
	query := mustOpen(ctx, "MONTRACER_CH_QUERY_DSN")

	if *cleanup {
		check(admin.Exec(ctx, `ALTER TABLE spans_local DELETE WHERE startsWith(toString(tenant_id), 'ee000000') SETTINGS mutations_sync = 1`))
		check(admin.Exec(ctx, `ALTER TABLE trace_lookup DELETE WHERE startsWith(toString(tenant_id), 'ee000000') SETTINGS mutations_sync = 1`))
		fmt.Println("cleanup done")
		return
	}
	end := time.Now().UTC().Truncate(time.Minute)
	if !*skipGen {
		generate(ctx, admin, *spans, *tenants, *services, *window, *batch, end)
	}
	report(ctx, admin, query, *tenants, *services, *runs, end)
}

// generate는 INSERT SELECT FROM numbers()로 서버에서 데이터를 만든다.
// trace당 평균 10 span (D06 §03), 1% error, payload 약 400B, 속성 6개.
func generate(ctx context.Context, admin driver.Conn, total, tenants, services int, window time.Duration, batch int, end time.Time) {
	start := time.Now()
	windowNs := window.Nanoseconds()
	for off := 0; off < total; off += batch {
		n := min(batch, total-off)
		q := fmt.Sprintf(`
INSERT INTO spans_local
SELECT
  toUUID(concat('ee000000-0000-4000-8000-', leftPad(lower(hex(t)), 12, '0'))) AS tenant_id,
  toUUID(concat('5e000000-0000-4000-8000-', leftPad(lower(hex(s)), 12, '0'))) AS service_id,
  toFixedString(concat(reinterpretAsFixedString(cityHash64(tr, 1)), reinterpretAsFixedString(cityHash64(tr, 2))), 16) AS trace_id,
  toFixedString(reinterpretAsFixedString(cityHash64(n, 3)), 8) AS span_id,
  if(n %% 10 = 0, toFixedString(repeat('\0', 8), 8), toFixedString(reinterpretAsFixedString(cityHash64(n - 1, 3)), 8)) AS parent_span_id,
  concat('GET /api/v', toString(s %% 3 + 1), '/items/{id}') AS name,
  fromUnixTimestamp64Nano(toInt64(%[3]d - (cityHash64(tr) %% %[4]d))) AS event_time,
  10000000 + cityHash64(n, 5) %% 2000000000 AS duration_ns,
  now64(3) AS received_at,
  if(cityHash64(n, 6) %% 100 = 0, 2, 0) AS status,
  if(n %% 10 = 0, 2, 3) AS span_kind,
  map('http.route', concat('/api/v1/items/', toString(s)), 'http.request.method', 'GET',
      'http.response.status_code', if(cityHash64(n, 6) %% 100 = 0, '500', '200'),
      'net.peer.name', concat('svc-', toString(s), '.shop.svc'), 'k8s.pod.name', concat('pod-', toString(cityHash64(n) %% 50)),
      'deployment.environment.name', 'production') AS attributes,
  concat('{"events":[],"links":[],"status":{"code":', toString(if(cityHash64(n, 6) %% 100 = 0, 2, 0)),
         '},"resource":{"service.version":"1.', toString(s %% 7), '.0","host.name":"node-', toString(cityHash64(n) %% 20),
         '"},"attrs":"', hex(cityHash64(n, 7)), repeat('x', 280), '"}') AS payload,
  toFixedString(concat(reinterpretAsFixedString(cityHash64(n, 8)), reinterpretAsFixedString(cityHash64(n, 9)), reinterpretAsFixedString(cityHash64(n, 10)), reinterpretAsFixedString(cityHash64(n, 11))), 32) AS payload_hash,
  1 AS version,
  toDateTime(%[3]d / 1000000000 + 7 * 86400) AS expires_at
FROM (
  SELECT number + %[1]d AS n, intDiv(n, 10) AS tr, tr %% %[5]d AS t, intDiv(tr, %[5]d) %% %[6]d AS s
  FROM numbers(%[2]d)
)
SETTINGS max_threads = 2, max_insert_threads = 1, max_block_size = 65536`, off, n, end.UnixNano(), windowNs, tenants, services)
		check(admin.Exec(ctx, q))
		fmt.Fprintf(os.Stderr, "inserted %d/%d\n", off+n, total)
	}
	fmt.Fprintf(os.Stderr, "generate: %d spans in %s\n", total, time.Since(start).Round(time.Millisecond))
}

type result struct {
	name                string
	p50, p95, p99       time.Duration
	rowsRead, bytesRead uint64
	note                string
}

func report(ctx context.Context, admin, query driver.Conn, tenants, services, runs int, end time.Time) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // 재현 가능한 실험용 고정 seed, 보안 용도 아님
	var traceIDs []string
	{
		rows, err := admin.Query(ctx, `SELECT hex(trace_id) FROM spans_local WHERE tenant_id = toUUID($1) AND event_time >= $2 LIMIT 200`,
			tenantID(0), end.Add(-time.Hour))
		check(err)
		for rows.Next() {
			var s string
			check(rows.Scan(&s))
			traceIDs = append(traceIDs, strings.ToLower(s))
		}
		check(rows.Close())
	}
	type q struct {
		name, sql string
		tenant    func() int
		note      string
	}
	queries := []q{
		{
			name: "trace search: 1 service, 최근 1h, 최신 100",
			sql: `SELECT hex(trace_id), max(duration_ns) AS d FROM spans_local
			      WHERE tenant_id = toUUID($1) AND service_id = toUUID($2) AND event_time >= $3 AND event_time < $4 AND expires_at > now()
			      GROUP BY trace_id ORDER BY d DESC LIMIT 100`,
			tenant: func() int { return rng.IntN(tenants) },
			note:   "D01 §08 검색 지연 측정 조건(최근 1h, 단일 서비스)",
		},
		{
			name: "error trace search: tenant 전체, 최근 1h",
			sql: `SELECT hex(trace_id), name, duration_ns FROM spans_local
			      WHERE tenant_id = toUUID($1) AND event_time >= $3 AND event_time < $4 AND status = 2 AND expires_at > now() AND $2 != ''
			      ORDER BY event_time DESC LIMIT 100`,
			tenant: func() int { return rng.IntN(tenants) },
			note:   "서비스 미지정 — 정렬 키 2번째(service_id)를 건너뛴다",
		},
		{
			name: "service RED: 1 service, 최근 1h, 1분 버킷",
			sql: `SELECT toStartOfMinute(event_time) AS m, count(), countIf(status = 2), quantileTDigest(0.95)(duration_ns)
			      FROM spans_local
			      WHERE tenant_id = toUUID($1) AND service_id = toUUID($2) AND event_time >= $3 AND event_time < $4 AND span_kind = 2 AND expires_at > now()
			      GROUP BY m ORDER BY m`,
			tenant: func() int { return rng.IntN(tenants) },
			note:   "참고용: SLO 원천은 SDK metric이다 (ADR 0004 후보, D02 §02)",
		},
		{
			name: "trace by id (lookup → 원본)",
			sql: `WITH loc AS (SELECT event_date, service_id FROM trace_lookup
			        WHERE tenant_id = toUUID($1) AND trace_id = unhex($2) AND event_date >= toDate($3) AND event_date <= toDate($4) AND expires_at > now()
			        GROUP BY event_date, service_id)
			      SELECT hex(span_id), name, event_time FROM spans_local
			      WHERE tenant_id = toUUID($1) AND (toDate(event_time), service_id) IN (SELECT event_date, service_id FROM loc)
			        AND trace_id = unhex($2) AND event_time >= $3 AND event_time < $4 AND expires_at > now()
			      ORDER BY event_time LIMIT 1 BY span_id`,
			tenant: func() int { return 0 },
			note:   "internal/telemetrystore.TraceSpans와 같은 형태",
		},
	}
	var results []result
	for qi, qq := range queries {
		var lat []time.Duration
		tag := fmt.Sprintf("chlayout-%d-%d", time.Now().UnixNano(), qi)
		for i := 0; i < runs; i++ {
			t := qq.tenant()
			arg2 := serviceID(rng.IntN(services))
			from, to := end.Add(-time.Hour), end.Add(time.Minute)
			if strings.HasPrefix(qq.name, "trace by id") {
				if len(traceIDs) == 0 {
					break
				}
				arg2 = traceIDs[rng.IntN(len(traceIDs))]
				from = end.Add(-7 * 24 * time.Hour)
			}
			qctx := clickhouse.Context(ctx,
				clickhouse.WithSettings(clickhouse.Settings{"SQL_montracer_tenant": clickhouse.CustomSetting{Value: tenantID(t)}}),
				clickhouse.WithQueryID(fmt.Sprintf("%s-%d", tag, i)))
			startQ := time.Now()
			rows, err := query.Query(qctx, qq.sql, tenantID(t), arg2, from, to)
			check(err)
			for rows.Next() {
			}
			check(rows.Err())
			check(rows.Close())
			lat = append(lat, time.Since(startQ))
		}
		if len(lat) == 0 {
			continue
		}
		check(admin.Exec(ctx, `SYSTEM FLUSH LOGS`))
		var rowsRead, bytesRead uint64
		check(admin.QueryRow(ctx, `SELECT toUInt64(avg(read_rows)), toUInt64(avg(read_bytes)) FROM system.query_log
			WHERE type = 'QueryFinish' AND startsWith(query_id, $1)`, tag).Scan(&rowsRead, &bytesRead))
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		pct := func(p float64) time.Duration { return lat[min(len(lat)-1, int(p*float64(len(lat))))] }
		results = append(results, result{qq.name, pct(0.5), pct(0.95), pct(0.99), rowsRead, bytesRead, qq.note})
	}

	var totalRows, compressed, uncompressed, parts uint64
	check(admin.QueryRow(ctx, `SELECT sum(rows), sum(data_compressed_bytes), sum(data_uncompressed_bytes), count()
		FROM system.parts WHERE database = currentDatabase() AND table = 'spans_local' AND active`).Scan(&totalRows, &compressed, &uncompressed, &parts))
	var version string
	check(admin.QueryRow(ctx, `SELECT version()`).Scan(&version))

	fmt.Printf("## 측정 결과 (%s, ClickHouse %s)\n\n", time.Now().UTC().Format("2006-01-02"), version)
	fmt.Printf("- 데이터: spans_local %d rows, active parts %d, tenant %d × 서비스 %d\n", totalRows, parts, tenants, services)
	fmt.Printf("- 저장: 압축 %.1f MiB / 비압축 %.1f MiB (압축률 %.1fx), span당 압축 %.0f B\n",
		float64(compressed)/(1<<20), float64(uncompressed)/(1<<20), float64(uncompressed)/float64(max(compressed, 1)), float64(compressed)/float64(max(totalRows, 1)))
	fmt.Printf("- 반복: 쿼리별 %d회, query 계정(row policy 적용), 클라이언트 측 지연(결과 수신 포함)\n\n", runs)
	fmt.Println("| 쿼리 | p50 | p95 | p99 | 평균 읽은 행 | 평균 읽은 바이트 | 비고 |")
	fmt.Println("|---|---|---|---|---|---|---|")
	for _, r := range results {
		fmt.Printf("| %s | %s | %s | %s | %d | %.1f MiB | %s |\n", r.name, ms(r.p50), ms(r.p95), ms(r.p99), r.rowsRead, float64(r.bytesRead)/(1<<20), r.note)
	}

	fmt.Println("\n### 컬럼별 저장 크기 (상위 8)")
	fmt.Println("\n| 컬럼 | 압축 MiB | 비압축 MiB | 압축률 |")
	fmt.Println("|---|---|---|---|")
	rows, err := admin.Query(ctx, `SELECT column, sum(column_data_compressed_bytes) c, sum(column_data_uncompressed_bytes) u
		FROM system.parts_columns WHERE database = currentDatabase() AND table = 'spans_local' AND active
		GROUP BY column ORDER BY c DESC LIMIT 8`)
	check(err)
	for rows.Next() {
		var col string
		var c, u uint64
		check(rows.Scan(&col, &c, &u))
		fmt.Printf("| %s | %.1f | %.1f | %.1fx |\n", col, float64(c)/(1<<20), float64(u)/(1<<20), float64(u)/float64(max(c, 1)))
	}
	check(rows.Close())
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }

func mustOpen(ctx context.Context, env string) driver.Conn {
	dsn := os.Getenv(env)
	if dsn == "" {
		fail(errors.New(env + " 필요"))
	}
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		fail(errors.New(env + ": invalid dsn"))
	}
	opts.ReadTimeout = 10 * time.Minute
	conn, err := clickhouse.Open(opts)
	check(err)
	check(conn.Ping(ctx))
	return conn
}

func check(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "chlayout:", err)
	os.Exit(1)
}
