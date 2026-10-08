//go:build integration

package telemetrystore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/polynomeer/montracer/internal/queryplan"
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
	// log 검색: 본문 contains 값·attribute key·value·trace_id는 사용자 검색 문자열이다(PII일 수 있다, ADR 0037).
	bodyNeedle := "needle-" + traceID[:12] + "@example.test"
	insertLogs(t, logRow{tenant, "uid:" + traceID, now.Add(-time.Minute), 17, traceID, "found " + bodyNeedle,
		map[string]string{labelKey: labelVal}, versionAt(now.Add(-time.Minute)), now.Add(time.Hour)})
	var lf queryplan.Node
	if err := json.Unmarshal([]byte(`{"op":"and","args":[{"field":"body","op":"contains","value":"`+bodyNeedle+`"},
		{"field":"attributes.`+labelKey+`","op":"eq","value":"`+labelVal+`"},{"field":"trace_id","op":"in","value":["`+traceID+`"]}]}`), &lf); err != nil {
		t.Fatal(err)
	}
	compiled, err := queryplan.Compile(&lf, queryplan.LogCatalog)
	if err != nil {
		t.Fatal(err)
	}
	logs, _, err := s.SearchLogs(ctx("logs"), pa, LogQuery{Range: TimeRange{now.Add(-time.Hour), now.Add(time.Minute)}, Filter: compiled, Limit: 10}, now)
	if err != nil || len(logs) != 1 {
		t.Fatalf("log search = %+v, %v", logs, err)
	}

	// trace 검색(ADR 0043): span 단계 trace_id·요약 단계 조건도 parameter다
	var tf queryplan.Node
	if err := json.Unmarshal([]byte(`{"op":"and","args":[{"field":"trace_id","op":"in","value":["`+traceID+`"]},{"field":"duration_ms","op":"gte","value":0}]}`), &tf); err != nil {
		t.Fatal(err)
	}
	spanF, sumF, err := queryplan.Split(&tf, queryplan.IsTraceSummaryField)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := queryplan.CompileWith(spanF, queryplan.TraceSpanCatalog, queryplan.Options{ParamPrefix: "s"})
	if err != nil {
		t.Fatal(err)
	}
	tc, err := queryplan.CompileWith(sumF, queryplan.TraceSummaryCatalog, queryplan.Options{ParamPrefix: "t"})
	if err != nil {
		t.Fatal(err)
	}
	traces, _, err := s.SearchTraces(ctx("traces"), pa, TraceSearchQuery{Range: TimeRange{now.Add(-time.Hour), now.Add(time.Minute)}, SpanFilter: sc, TraceFilter: tc, Limit: 10}, now)
	if err != nil || len(traces) != 1 || traces[0].TraceID != traceID {
		t.Fatalf("trace search = %+v, %v", traces, err)
	}

	// metric 사전(ADR 0046): 이름 검색어·metric 이름(label key 조회)·environment 범위도 parameter다
	cat, _, err := s.MetricCatalog(ctx("catalog"), pk, MetricCatalogQuery{Range: TimeRange{From: w0, To: w0.Add(10 * time.Minute)}, Contains: "it.metric", Limit: 10}, now)
	if err != nil || len(cat) != 1 || cat[0].Name != "it.metric" {
		t.Fatalf("metric catalog = %+v, %v", cat, err)
	}
	lk, _, err := s.MetricLabelKeys(ctx("labels"), pk, "it.metric", TimeRange{From: w0, To: w0.Add(10 * time.Minute)}, now)
	if err != nil || len(lk) == 0 {
		t.Fatalf("metric labels = %+v, %v", lk, err)
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
			"environment": env, "metric": "it.metric", "log body needle": bodyNeedle} {
			if strings.Contains(row, v) || strings.Contains(strings.ToUpper(row), strings.ToUpper(v)) {
				t.Errorf("query_log row %s (user %s) contains %s %q", id, user, name, v)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"trace", "metric", "watermark", "logs", "traces", "catalog", "labels"} {
		if !seen[name] {
			t.Errorf("query_log has no row for %s query (seen %v)", name, seen)
		}
	}
}
