//go:build integration

package telemetrystore

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// 사용자 값은 서버 측 query parameter로 보내므로 system.query_log의 행에 남지 않는다 (D04 §10, ADR 0032).
// clickhouse-go의 위치 인자($1)는 값을 SQL 문자열에 끼워 보내 query 컬럼에 남긴다. 이 시험은 그 회귀를 잡는다.
func TestQueryLogHasNoBoundValues(t *testing.T) {
	s := openQuery(t)
	tenant, pa := newTenant(t)
	tag := rnd16()
	traceID := hex.EncodeToString(tag[:])
	labelKey := "it.key." + traceID[:8]
	labelVal := "leak-" + traceID[8:20] + "@example.test"
	env := "env-" + traceID[20:]
	queryIDPrefix := "it-querylog-" + traceID

	now := time.Now().UTC().Truncate(time.Millisecond)
	insertSpans(t, spanRow{tenant, svcA, traceID, "00f067aa0ba902b7", now.Add(-time.Minute), now.Add(time.Hour), 1, "root"})
	w0 := now.Truncate(time.Minute).Add(-5 * time.Minute)
	insertM1m(t, m1m{tenant: tenant.String(), stream: rnd16(), window: w0,
		resource: `{"deployment.environment.name":"` + env + `"}`, attrs: `{"` + labelKey + `":"` + labelVal + `"}`,
		typ: "sum", unit: "1", revision: 1, increase: 3, hasInc: true})

	ctx := func(name string) context.Context {
		return clickhouse.Context(context.Background(), clickhouse.WithQueryID(queryIDPrefix+"-"+name))
	}
	// trace 조회 (service 조건 포함)
	recs, err := s.TraceSpanRecords(ctx("trace"), pa, TraceQuery{TraceID: traceID, Range: TimeRange{now.Add(-time.Hour), now.Add(time.Minute)}, ServiceID: svcA}, now)
	if err != nil || len(recs) != 1 || recs[0].TraceID != traceID {
		t.Fatalf("trace records = %+v, %v", recs, err)
	}
	// label filter·group_by·environment 제한(Array parameter)이 있는 metric 조회. 값이 맞게 binding되는지도 본다.
	pk := keyPrincipal(t, tenant, []string{env})
	bs, err := s.MetricBuckets(ctx("metric"), pk, MetricQuery{Metric: "it.metric", StepSeconds: 600,
		Range:   TimeRange{From: w0, To: w0.Add(10 * time.Minute)},
		Filters: []LabelMatch{{Key: labelKey, Value: labelVal}}, GroupBy: []string{labelKey}}, now)
	if err != nil || len(bs) != 1 || bs[0].Group[0] != labelVal || bs[0].Increase != 3 {
		t.Fatalf("metric buckets = %+v, %v", bs, err)
	}
	if _, err := s.RollupWatermark(ctx("watermark"), pa, time.Minute, w0, now); err != nil {
		t.Fatal(err)
	}

	admin := rawConn(t, "MONTRACER_TEST_CH_ADMIN_DSN")
	bg := context.Background()
	if err := admin.Exec(bg, `SYSTEM FLUSH LOGS`); err != nil {
		t.Fatal(err)
	}
	// 행 전체(query, Settings, exception 등 모든 컬럼)를 문자열로 읽어 값이 어디에도 없는지 본다.
	rows, err := admin.Query(bg, `SELECT query_id, user, toString(tuple(*)) FROM system.query_log
		WHERE event_date >= yesterday() AND startsWith(query_id, {prefix:String})`, clickhouse.Named("prefix", queryIDPrefix))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	for rows.Next() {
		var id, user, row string
		if err := rows.Scan(&id, &user, &row); err != nil {
			t.Fatal(err)
		}
		seen[strings.TrimPrefix(id, queryIDPrefix+"-")] = true
		// query_id 자체에 trace_id가 들어 있으므로 비교 전에 지운다.
		row = strings.ReplaceAll(row, queryIDPrefix, "")
		for name, v := range map[string]string{"trace_id": traceID, "label key": labelKey, "label value": labelVal,
			"environment": env, "metric": "it.metric"} {
			if strings.Contains(row, v) || strings.Contains(strings.ToUpper(row), strings.ToUpper(v)) {
				t.Errorf("query_log row %s (user %s) contains %s %q", id, user, name, v)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"trace", "metric", "watermark"} {
		if !seen[name] {
			t.Errorf("query_log has no row for %s query (seen %v)", name, seen)
		}
	}
}
