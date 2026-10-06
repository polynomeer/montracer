//go:build integration

package telemetrystore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/queryplan"
)

// 통합 테스트는 migration이 적용된 ClickHouse가 필요하다 (make up && make migrate).
//
//	MONTRACER_TEST_CH_QUERY_DSN   query 계정 (테스트 대상)
//	MONTRACER_TEST_CH_INGEST_DSN  ingest 계정 (쓰기 경로)
//	MONTRACER_TEST_CH_ADMIN_DSN   관리자 (OpenQuery 거부 시험)
//
// 매 테스트는 새 무작위 tenant를 써서 기존 데이터와 섞이지 않는다.

func env(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("%s 필요 (make test-integration)", key)
	}
	return v
}

func rawConn(t *testing.T, key string) driver.Conn {
	t.Helper()
	opts, err := clickhouse.ParseDSN(env(t, key))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func openQuery(t *testing.T) *QueryStore {
	t.Helper()
	s, err := OpenQuery(context.Background(), env(t, "MONTRACER_TEST_CH_QUERY_DSN"))
	if err != nil {
		t.Fatalf("OpenQuery: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTenant(t *testing.T) (authz.TenantID, authz.Principal) {
	t.Helper()
	conn := rawConn(t, "MONTRACER_TEST_CH_QUERY_DSN")
	var id string
	if err := conn.QueryRow(context.Background(), `SELECT toString(generateUUIDv4())`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	tenant, err := authz.ParseTenantID(id)
	if err != nil {
		t.Fatal(err)
	}
	n := time.Now()
	p, err := authz.NewUserPrincipal(tenant, "viewer", authz.RoleViewer, time.Time{}, n)
	if err != nil {
		t.Fatal(err)
	}
	return tenant, p
}

type spanRow struct {
	tenant    authz.TenantID
	service   string
	traceID   string
	spanID    string
	eventTime time.Time
	expires   time.Time
	version   uint64
	name      string
}

// insertSpans는 ingest 계정으로 쓴다 (쓰기 권한 경로 검증 겸).
func insertSpans(t *testing.T, rows ...spanRow) {
	t.Helper()
	ctx := context.Background()
	conn := rawConn(t, "MONTRACER_TEST_CH_INGEST_DSN")
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO spans_local
		(tenant_id, service_id, trace_id, span_id, parent_span_id, name, event_time, duration_ns,
		 received_at, status, span_kind, attributes, payload, payload_hash, version, expires_at)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for _, r := range rows {
		tid, _ := hex.DecodeString(r.traceID)
		sid, _ := hex.DecodeString(r.spanID)
		if err := batch.Append(r.tenant.String(), r.service, string(tid), string(sid), string(make([]byte, 8)), r.name,
			r.eventTime, uint64(1_000_000), r.eventTime, uint8(0), uint8(2), map[string]string{"http.route": "/checkout"},
			"{}", string(make([]byte, 32)), r.version, r.expires); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send: %v", err)
	}
}

const (
	svcA  = "aaaaaaaa-0000-4000-8000-000000000001"
	trace = "4bf92f3577b34da6a3ce929d0e0e4736"
)

func TestTraceSpansTenantIsolation(t *testing.T) {
	s := openQuery(t)
	ta, pa := newTenant(t)
	tb, pb := newTenant(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	exp := now.Add(7 * 24 * time.Hour)
	// 두 tenant가 같은 trace_id를 쓴다 (trace id는 권한 정보가 아니다, D03 §02).
	insertSpans(t,
		spanRow{ta, svcA, trace, "00f067aa0ba902b7", now.Add(-time.Minute), exp, 1, "A root"},
		spanRow{ta, svcA, trace, "a1b2c3d4e5f60718", now.Add(-time.Minute + time.Millisecond), exp, 1, "A child"},
		spanRow{tb, svcA, trace, "00f067aa0ba902b7", now.Add(-time.Minute), exp, 1, "B secret"},
	)
	r := TimeRange{From: now.Add(-time.Hour), To: now.Add(time.Minute)}
	spans, err := s.TraceSpans(context.Background(), pa, trace, r, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 || spans[0].Name != "A root" || spans[0].TraceID != trace || spans[0].SpanID != "00f067aa0ba902b7" {
		t.Fatalf("tenant A spans = %+v", spans)
	}
	spans, err = s.TraceSpans(context.Background(), pb, trace, r, now)
	if err != nil || len(spans) != 1 || spans[0].Name != "B secret" {
		t.Fatalf("tenant B spans = %+v, %v", spans, err)
	}
}

func TestRowPolicyWithoutPredicate(t *testing.T) {
	ta, _ := newTenant(t)
	tb, _ := newTenant(t)
	now := time.Now().UTC()
	insertSpans(t,
		spanRow{ta, svcA, trace, "00f067aa0ba902b7", now, now.Add(time.Hour), 1, "a"},
		spanRow{tb, svcA, trace, "00f067aa0ba902b8", now, now.Add(time.Hour), 1, "b"},
	)
	conn := rawConn(t, "MONTRACER_TEST_CH_QUERY_DSN")
	count := func(ctx context.Context) uint64 {
		t.Helper()
		var n uint64
		// tenant predicate 없이 전체를 세어도 row policy가 tenant 행만 보여준다.
		if err := conn.QueryRow(ctx, `SELECT count() FROM spans_local WHERE tenant_id IN ($1, $2)`, ta.String(), tb.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(context.Background()); n != 0 {
		t.Errorf("without tenant setting: %d rows, want 0 (fail closed)", n)
	}
	if n := count(tenantContext(context.Background(), ta)); n != 1 {
		t.Errorf("tenant A setting: %d rows, want 1", n)
	}
	bad := clickhouse.Context(context.Background(), clickhouse.WithSettings(clickhouse.Settings{TenantSetting: clickhouse.CustomSetting{Value: "not-a-uuid"}}))
	if n := count(bad); n != 0 {
		t.Errorf("invalid tenant setting: %d rows, want 0", n)
	}
}

func TestExpiredRowsHiddenBeforeTTLMerge(t *testing.T) {
	s := openQuery(t)
	ta, pa := newTenant(t)
	now := time.Now().UTC()
	insertSpans(t,
		spanRow{ta, svcA, trace, "00f067aa0ba902b7", now.Add(-time.Minute), now.Add(-time.Second), 1, "expired"},
		spanRow{ta, svcA, trace, "a1b2c3d4e5f60718", now.Add(-time.Minute), now.Add(time.Hour), 1, "live"},
	)
	spans, err := s.TraceSpans(context.Background(), pa, trace, TimeRange{now.Add(-time.Hour), now.Add(time.Minute)}, now)
	if err != nil || len(spans) != 1 || spans[0].Name != "live" {
		t.Fatalf("spans = %+v, %v", spans, err)
	}
}

// 같은 span을 재전송(at-least-once)해도 version이 가장 큰 행 하나만 보인다 (merge 전, FINAL 없이).
func TestDuplicateSpansDeduplicatedAtQuery(t *testing.T) {
	s := openQuery(t)
	ta, pa := newTenant(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	row := spanRow{ta, svcA, trace, "00f067aa0ba902b7", now.Add(-time.Minute), now.Add(time.Hour), 1, "v1"}
	insertSpans(t, row)
	row.version, row.name = 2, "v2"
	insertSpans(t, row) // 별도 part
	row.version, row.name = 1, "v1"
	insertSpans(t, row) // 재전송
	// 최대 version(v2)이 merge 전에도 선택돼야 한다. 여러 번 조회해 우연히 맞는 경우를 배제한다.
	for range 5 {
		spans, err := s.TraceSpans(context.Background(), pa, trace, TimeRange{now.Add(-time.Hour), now.Add(time.Minute)}, now)
		if err != nil || len(spans) != 1 || spans[0].Name != "v2" {
			t.Fatalf("spans = %+v, %v (want only v2)", spans, err)
		}
	}
}

func TestTraceSpansInputValidation(t *testing.T) {
	s := openQuery(t)
	_, pa := newTenant(t)
	now := time.Now()
	ok := TimeRange{now.Add(-time.Hour), now}
	cases := []struct {
		name  string
		p     authz.Principal
		trace string
		r     TimeRange
		want  error
	}{
		{"unauthenticated", authz.Principal{}, trace, ok, authz.ErrUnauthenticated},
		{"uppercase trace id", pa, strings.ToUpper(trace), ok, nil},
		{"short trace id", pa, "abcd", ok, nil},
		{"all zero trace id", pa, strings.Repeat("0", 32), ok, nil},
		{"empty range", pa, trace, TimeRange{now, now}, nil},
		{"over 7 days", pa, trace, TimeRange{now.Add(-8 * 24 * time.Hour), now}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.TraceSpans(context.Background(), tc.p, tc.trace, tc.r, now)
			if err == nil {
				t.Fatal("want error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := s.TraceSpans(context.Background(), pa, trace, ok, time.Time{}); err == nil {
		t.Fatal("zero now must be rejected")
	}
	// Security Auditor는 telemetry 본문 권한이 없다 (D04 §01).
	ta, _ := newTenant(t)
	auditor, _ := authz.NewUserPrincipal(ta, "auditor", authz.RoleSecurityAuditor, time.Time{}, now)
	if _, err := s.TraceSpans(context.Background(), auditor, trace, ok, now); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("auditor: %v", err)
	}
}

func TestAccountPrivileges(t *testing.T) {
	ctx := context.Background()
	if _, err := OpenQuery(ctx, env(t, "MONTRACER_TEST_CH_ADMIN_DSN")); !errors.Is(err, ErrNotReadOnly) {
		t.Fatalf("OpenQuery(admin) = %v, want ErrNotReadOnly", err)
	}
	if _, err := OpenQuery(ctx, env(t, "MONTRACER_TEST_CH_INGEST_DSN")); !errors.Is(err, ErrNotReadOnly) {
		t.Fatalf("OpenQuery(ingest) = %v, want ErrNotReadOnly", err)
	}
	query := rawConn(t, "MONTRACER_TEST_CH_QUERY_DSN")
	ingest := rawConn(t, "MONTRACER_TEST_CH_INGEST_DSN")
	denied := []struct {
		name string
		conn driver.Conn
		sql  string
	}{
		{"query insert", query, `INSERT INTO spans_local (tenant_id) VALUES (generateUUIDv4())`},
		{"query drop", query, `DROP TABLE spans_local`},
		{"query alter", query, `ALTER TABLE spans_local DELETE WHERE 1`},
		{"query system log", query, `SELECT count() FROM system.query_log`},
		{"query readonly off", query, `SET readonly = 0`},
		{"query url table function", query, `SELECT * FROM url('http://127.0.0.1:8123/', 'CSV', 'x String')`},
		{"query remote", query, `SELECT * FROM remote('127.0.0.1', montracer.spans_local)`},
		{"query cluster", query, `SELECT * FROM cluster('default', montracer.spans_local)`},
		{"query file", query, `SELECT * FROM file('x.csv', 'CSV', 'x String')`},
		{"query s3", query, `SELECT * FROM s3('http://127.0.0.1:9000/montracer-bucket/x.csv', 'CSV', 'x String')`},
		{"query system processes", query, `SELECT * FROM system.processes`},
		{"query information_schema", query, `SELECT * FROM information_schema.tables`},
		{"query budget above max", query, `SET max_execution_time = 3600`},
		{"query relax overflow mode", query, `SET read_overflow_mode = 'break'`},
		{"ingest forge lookup", ingest, `INSERT INTO trace_lookup (tenant_id) VALUES (generateUUIDv4())`},
		{"ingest alter", ingest, `ALTER TABLE spans_local DELETE WHERE 1`},
		{"ingest drop", ingest, `DROP TABLE spans_local`},
		{"ingest select", ingest, `SELECT count() FROM spans_local`},
		{"ingest truncate", ingest, `TRUNCATE TABLE spans_local`},
		{"ingest system log", ingest, `SELECT count() FROM system.query_log`},
	}
	for _, tc := range denied {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.conn.Exec(ctx, tc.sql)
			var ex *clickhouse.Exception
			// ACCESS_DENIED(497), READONLY(164), SETTING_CONSTRAINT_VIOLATION(452)
			if !errors.As(err, &ex) || (ex.Code != 497 && ex.Code != 164 && ex.Code != 452) {
				t.Fatalf("%s: %v, want access denied", tc.sql, err)
			}
		})
	}
}

// row policy만 있을 때도(쿼리에 tenant predicate 없이) primary key로 granule을 건너뛰는지 본다.
// 3개 tenant × 10,000 span을 한 번에 넣어 tenant별로 다른 granule(8,192행)에 놓이게 한다.
func TestRowPolicyAloneUsesPrimaryKey(t *testing.T) {
	ta, _ := newTenant(t)
	tb, _ := newTenant(t)
	tc, _ := newTenant(t)
	now := time.Now().UTC()
	var rows []spanRow
	for _, ten := range []authz.TenantID{ta, tb, tc} {
		for i := range 10000 {
			rows = append(rows, spanRow{ten, svcA, trace, fmt.Sprintf("%016x", i+1), now.Add(-time.Duration(i) * time.Millisecond), now.Add(time.Hour), 1, "s"})
		}
	}
	insertSpans(t, rows...)
	conn := rawConn(t, "MONTRACER_TEST_CH_QUERY_DSN")
	r, err := conn.Query(tenantContext(context.Background(), ta), `EXPLAIN indexes = 1 SELECT count() FROM spans_local`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var plan strings.Builder
	for r.Next() {
		var line string
		if err := r.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	out := plan.String()
	m := regexp.MustCompile(`PrimaryKey[\s\S]*?Granules: (\d+)/(\d+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no primary key analysis in plan:\n%s", out)
	}
	selected, _ := strconv.Atoi(m[1])
	total, _ := strconv.Atoi(m[2])
	if selected >= total {
		t.Fatalf("row policy alone did not prune granules (%d/%d):\n%s", selected, total, out)
	}
	var n uint64
	if err := conn.QueryRow(tenantContext(context.Background(), ta), `SELECT count() FROM spans_local`).Scan(&n); err != nil || n != 10000 {
		t.Fatalf("count = %d, %v", n, err)
	}
}

// predicate 없이도 4개 테이블 모두 row policy가 tenant 행만 보인다.
func TestRowPolicyOnAllTables(t *testing.T) {
	ta, _ := newTenant(t)
	tb, _ := newTenant(t)
	now := time.Now().UTC()
	insertSpans(t,
		spanRow{ta, svcA, trace, "00f067aa0ba902b7", now, now.Add(time.Hour), 1, "a"},
		spanRow{tb, svcA, trace, "00f067aa0ba902b7", now, now.Add(time.Hour), 1, "b"},
	)
	ingest := rawConn(t, "MONTRACER_TEST_CH_INGEST_DSN")
	ctx := context.Background()
	for _, ten := range []authz.TenantID{ta, tb} {
		if err := ingest.Exec(ctx, `INSERT INTO logs_local (tenant_id, service_id, event_id, event_time, severity, trace_id, span_id, body, attributes, version, expires_at)
			VALUES ($1, $2, 'e1', now64(9), 9, unhex($3), unhex('00f067aa0ba902b7'), 'b', map(), 1, now() + 3600)`, ten.String(), svcA, trace); err != nil {
			t.Fatal(err)
		}
		if err := ingest.Exec(ctx, `INSERT INTO metric_points (tenant_id, stream_id, metric_name, unit, type, temporality, is_monotonic, start_time, end_time, point_hash, value, count, sum, bounds, buckets, payload, resource_json, attributes_json, version, expires_at)
			VALUES ($1, unhex('00000000000000000000000000000001'), 'm', '1', 'gauge', 'unspecified', false, now64(9), now64(9), unhex('00000000000000000000000000000001'), 1, 0, 0, [], [], '', '{}', '{}', 1, now() + 3600)`, ten.String()); err != nil {
			t.Fatal(err)
		}
	}
	query := rawConn(t, "MONTRACER_TEST_CH_QUERY_DSN")
	for _, table := range []string{"spans_local", "logs_local", "metric_points", "trace_lookup"} {
		count := func(qctx context.Context) uint64 {
			var n uint64
			if err := query.QueryRow(qctx, `SELECT count() FROM `+table+` WHERE tenant_id IN ($1, $2)`, ta.String(), tb.String()).Scan(&n); err != nil {
				t.Fatalf("%s: %v", table, err)
			}
			return n
		}
		if n := count(ctx); n != 0 {
			t.Errorf("%s without tenant: %d rows", table, n)
		}
		if n := count(tenantContext(ctx, ta)); n != 1 {
			t.Errorf("%s tenant A: %d rows, want 1", table, n)
		}
	}
}

// merge() table function은 query 계정에 허용되지만, 읽는 원본 테이블의 row policy가 그대로 적용돼야 한다.
func TestMergeTableFunctionRespectsRowPolicy(t *testing.T) {
	ta, _ := newTenant(t)
	tb, _ := newTenant(t)
	now := time.Now().UTC()
	insertSpans(t,
		spanRow{ta, svcA, trace, "00f067aa0ba902b7", now, now.Add(time.Hour), 1, "a"},
		spanRow{tb, svcA, trace, "00f067aa0ba902b7", now, now.Add(time.Hour), 1, "b"},
	)
	query := rawConn(t, "MONTRACER_TEST_CH_QUERY_DSN")
	count := func(ctx context.Context) uint64 {
		var n uint64
		if err := query.QueryRow(ctx, `SELECT count() FROM merge(currentDatabase(), '^spans_local$') WHERE tenant_id IN ($1, $2)`,
			ta.String(), tb.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(context.Background()); n != 0 {
		t.Errorf("merge() without tenant: %d rows, want 0", n)
	}
	if n := count(tenantContext(context.Background(), ta)); n != 1 {
		t.Errorf("merge() tenant A: %d rows, want 1", n)
	}
}

func TestTimeBoundariesAndLookupEdges(t *testing.T) {
	s := openQuery(t)
	ta, pa := newTenant(t)
	now := time.Now().UTC().Truncate(time.Second)
	// UTC 자정을 걸친 trace: lookup의 event_date가 둘이다.
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from, to := midnight.Add(-time.Minute), midnight.Add(time.Minute)
	exp := now.Add(time.Hour)
	insertSpans(t,
		spanRow{ta, svcA, trace, "0000000000000001", from, exp, 1, "at-from"},                       // 포함
		spanRow{ta, svcA, trace, "0000000000000002", midnight.Add(-time.Second), exp, 1, "day1"},    // 포함
		spanRow{ta, svcA, trace, "0000000000000003", midnight.Add(time.Second), exp, 1, "day2"},     // 포함
		spanRow{ta, svcA, trace, "0000000000000004", to, exp, 1, "at-to"},                           // 제외 [from,to)
		spanRow{ta, svcA, trace, "0000000000000005", midnight, now.Add(-time.Second), 1, "expired"}, // lookup은 살아 있으나 원본 만료
	)
	spans, err := s.TraceSpans(context.Background(), pa, trace, TimeRange{from, to}, now)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, sp := range spans {
		names = append(names, sp.Name)
	}
	if got := strings.Join(names, ","); got != "at-from,day1,day2" {
		t.Fatalf("spans = %s, want at-from,day1,day2", got)
	}
}

type logRow struct {
	tenant    authz.TenantID
	eventID   string
	eventTime time.Time
	severity  uint8
	traceID   string
	body      string
	attrs     map[string]string
	version   uint64
	expires   time.Time
}

func insertLogs(t *testing.T, rows ...logRow) {
	t.Helper()
	ctx := context.Background()
	conn := rawConn(t, "MONTRACER_TEST_CH_INGEST_DSN")
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO logs_local
		(tenant_id, service_id, event_id, event_time, severity, trace_id, span_id, body, attributes, version, expires_at)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for _, r := range rows {
		tid := make([]byte, 16)
		if r.traceID != "" {
			tid, _ = hex.DecodeString(r.traceID)
		}
		if err := batch.Append(r.tenant.String(), svcA, r.eventID, r.eventTime, r.severity, string(tid), string(make([]byte, 8)),
			r.body, r.attrs, r.version, r.expires); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send: %v", err)
	}
}

// versionAt은 pipeline과 같은 version(수신이 이를수록 크다)이다.
func versionAt(received time.Time) uint64 {
	return math.MaxUint64 - (uint64(received.UnixMilli()) << 20) //nolint:gosec // 시험 시각은 양수
}

func TestSearchLogs(t *testing.T) {
	s := openQuery(t)
	tenantA, viewerA := newTenant(t)
	tenantB, viewerB := newTenant(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	recv := base.Add(time.Second)
	exp := base.Add(24 * time.Hour)
	insertLogs(t,
		logRow{tenantA, "uid:1", base.Add(1 * time.Minute), 9, trace, "order received", map[string]string{"http.route": "/checkout"}, versionAt(recv), exp},
		logRow{tenantA, "uid:2", base.Add(2 * time.Minute), 17, trace, "charge failed: CARD declined", map[string]string{"http.route": "/charge"}, versionAt(recv), exp},
		logRow{tenantA, "uid:3", base.Add(3 * time.Minute), 17, "", "db timeout", nil, versionAt(recv), exp},
		// 같은 event_id 재전송(나중 수신 = 작은 version): 최초 수신 한 행만
		logRow{tenantA, "uid:2", base.Add(2 * time.Minute), 17, trace, "charge failed: CARD declined (resent)", nil, versionAt(recv.Add(time.Minute)), exp},
		// 만료
		logRow{tenantA, "uid:4", base.Add(4 * time.Minute), 17, "", "expired", nil, versionAt(recv), base.Add(-time.Minute)},
		// 다른 tenant
		logRow{tenantB, "uid:9", base.Add(2 * time.Minute), 17, trace, "tenant b charge failed", nil, versionAt(recv), exp},
	)
	rng := TimeRange{From: base, To: base.Add(10 * time.Minute)}
	compile := func(f string) queryplan.Compiled {
		t.Helper()
		var n *queryplan.Node
		if f != "" {
			n = &queryplan.Node{}
			if err := json.Unmarshal([]byte(f), n); err != nil {
				t.Fatal(err)
			}
		}
		c, err := queryplan.Compile(n, queryplan.LogCatalog)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ids := func(rs []LogRecord) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.EventID)
		}
		return strings.Join(out, ",")
	}
	now := time.Now()

	all, more, err := s.SearchLogs(ctx, viewerA, LogQuery{Range: rng, Filter: compile(""), Limit: 10}, now)
	if err != nil || more || ids(all) != "uid:3,uid:2,uid:1" {
		t.Fatalf("all = %s more=%v err=%v", ids(all), more, err)
	}
	if all[1].Body != "charge failed: CARD declined" || all[1].TraceID != trace || all[0].TraceID != "" {
		t.Errorf("dedup / ids: %+v", all[1])
	}
	// filter: severity ≥ 17 AND body contains (대소문자 무시) AND trace 연결
	got, _, err := s.SearchLogs(ctx, viewerA, LogQuery{Range: rng, Limit: 10, Filter: compile(`{"op":"and","args":[
		{"field":"severity_number","op":"gte","value":17},{"field":"body","op":"contains","value":"card"},
		{"field":"trace_id","op":"eq","value":"` + trace + `"},{"field":"attributes.http.route","op":"eq","value":"/charge"}]}`)}, now)
	if err != nil || ids(got) != "uid:2" {
		t.Fatalf("filtered = %s err=%v", ids(got), err)
	}
	// keyset: limit 1 → 다음 page는 마지막 위치 뒤
	p1, more, err := s.SearchLogs(ctx, viewerA, LogQuery{Range: rng, Filter: compile(""), Limit: 1}, now)
	if err != nil || !more || ids(p1) != "uid:3" {
		t.Fatalf("page1 = %s more=%v err=%v", ids(p1), more, err)
	}
	p2, _, err := s.SearchLogs(ctx, viewerA, LogQuery{Range: rng, Filter: compile(""), Limit: 5,
		After: &LogPosition{EventTime: p1[0].EventTime, EventID: p1[0].EventID}}, now)
	if err != nil || ids(p2) != "uid:2,uid:1" {
		t.Fatalf("page2 = %s err=%v", ids(p2), err)
	}
	// 수신 snapshot: recv 이전 수신만 보면 아무것도 없다
	none, _, err := s.SearchLogs(ctx, viewerA, LogQuery{Range: rng, Filter: compile(""), Limit: 5, ReceivedBefore: recv.Add(-time.Second)}, now)
	if err != nil || len(none) != 0 {
		t.Errorf("snapshot before receive = %s err=%v", ids(none), err)
	}
	// tenant 격리: B는 A의 log를 보지 않는다
	bLogs, _, err := s.SearchLogs(ctx, viewerB, LogQuery{Range: rng, Filter: compile(""), Limit: 10}, now)
	if err != nil || ids(bLogs) != "uid:9" {
		t.Errorf("tenant B = %s err=%v", ids(bLogs), err)
	}
	// 범위 상한과 environment 제한 key
	if _, _, err := s.SearchLogs(ctx, viewerA, LogQuery{Range: TimeRange{From: base, To: base.Add(25 * time.Hour)}, Filter: compile(""), Limit: 1}, now); err == nil {
		t.Error("25h range accepted")
	}
	if _, _, err := s.SearchLogs(ctx, keyPrincipal(t, tenantA, []string{"prod"}), LogQuery{Range: rng, Filter: compile(""), Limit: 1}, now); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("environment-scoped key = %v", err)
	}
}

// 컴파일된 SQL 형태 전부를 실제 ClickHouse에서 돌린다: in(trace·severity·service·attribute), neq, exists, service_id eq,
// 없는 값(map key·0 byte trace) 비교 제외, 수신 snapshot 경계, event_time이 다른 재전송의 dedup(리뷰에서 발견).
func TestSearchLogsSQLForms(t *testing.T) {
	s := openQuery(t)
	tenant, viewer := newTenant(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	early, late := base.Add(time.Second), base.Add(time.Minute)
	exp := base.Add(24 * time.Hour)
	other := "aaaaaaaabbbbccccddddeeeeffff0000"
	insertLogs(t,
		logRow{tenant, "uid:a", base.Add(1 * time.Minute), 9, trace, "a", map[string]string{"k": "x"}, versionAt(early), exp},
		logRow{tenant, "uid:b", base.Add(2 * time.Minute), 13, other, "b", map[string]string{"k": ""}, versionAt(early), exp},
		logRow{tenant, "uid:c", base.Add(3 * time.Minute), 17, "", "c", nil, versionAt(late), exp}, // 나중 수신, trace 없음, key 없음
		// uid:a의 재전송: 나중 수신(작은 version)이고 event_time이 더 크다 — 최초 수신 행(1분, 본문 "a")이 이겨야 한다
		logRow{tenant, "uid:a", base.Add(5 * time.Minute), 9, trace, "a (resent later, later timestamp)", nil, versionAt(late), exp},
	)
	rng := TimeRange{From: base, To: base.Add(10 * time.Minute)}
	run := func(filter string, received time.Time) string {
		t.Helper()
		var n *queryplan.Node
		if filter != "" {
			n = &queryplan.Node{}
			if err := json.Unmarshal([]byte(filter), n); err != nil {
				t.Fatal(err)
			}
		}
		c, err := queryplan.Compile(n, queryplan.LogCatalog)
		if err != nil {
			t.Fatal(err)
		}
		rs, _, err := s.SearchLogs(ctx, viewer, LogQuery{Range: rng, Filter: c, Limit: 10, ReceivedBefore: received}, time.Now())
		if err != nil {
			t.Fatalf("%s: %v", filter, err)
		}
		var out []string
		for _, r := range rs {
			out = append(out, r.EventID+"="+r.Body)
		}
		return strings.Join(out, ",")
	}
	for filter, want := range map[string]string{
		"": "uid:c=c,uid:b=b,uid:a=a",
		`{"field":"trace_id","op":"in","value":["` + trace + `","` + other + `"]}`: "uid:b=b,uid:a=a",
		`{"field":"trace_id","op":"neq","value":"` + trace + `"}`:                  "uid:b=b", // 0 byte(없음)는 neq에도 맞지 않는다
		`{"field":"severity_number","op":"in","value":[9,17]}`:                     "uid:c=c,uid:a=a",
		`{"field":"service_id","op":"eq","value":"` + svcA + `"}`:                  "uid:c=c,uid:b=b,uid:a=a",
		`{"field":"service_id","op":"in","value":["` + svcA + `"]}`:                "uid:c=c,uid:b=b,uid:a=a",
		`{"field":"attributes.k","op":"exists"}`:                                   "uid:b=b,uid:a=a",
		`{"field":"attributes.k","op":"eq","value":""}`:                            "uid:b=b", // key 없는 uid:c는 "" 와 같지 않다
		`{"field":"attributes.k","op":"neq","value":"x"}`:                          "uid:b=b",
		`{"field":"attributes.k","op":"in","value":["x","y"]}`:                     "uid:a=a",
	} {
		if got := run(filter, time.Time{}); got != want {
			t.Errorf("%s\n got %s\nwant %s", filter, got, want)
		}
	}
	// 수신 snapshot 경계: early와 late 사이 → 먼저 받은 행만(재전송·uid:c 제외), uid:a는 최초 수신 본문
	if got := run("", early.Add(time.Second)); got != "uid:b=b,uid:a=a" {
		t.Errorf("snapshot between receives = %s", got)
	}
}
