//go:build integration

package servicemap

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// 임시 진단: 어떤 query 모양이 표 전체 SELECT를 요구하는지 본다.
func TestDiagQueryShapes(t *testing.T) {
	ctx := context.Background()
	opts, _ := clickhouse.ParseDSN(env(t, "MONTRACER_TEST_CH_SERVICEMAP_DSN"))
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var ver string
	_ = conn.QueryRow(ctx, `SELECT version()`).Scan(&ver)
	now := time.Now()
	args := []any{clickhouse.Named("from", now.Add(-time.Hour)), clickhouse.Named("to", now), clickhouse.Named("bounds", Bounds)}
	shapes := map[string]string{
		"01 col":        `SELECT tenant_id FROM spans_local LIMIT 1`,
		"02 map":        `SELECT attributes['peer.service'] FROM spans_local LIMIT 1`,
		"03 domain":     `SELECT domain(attributes['url.full']) FROM spans_local LIMIT 1`,
		"04 limitby":    `SELECT tenant_id, trace_id, span_id FROM spans_local LIMIT 1 BY tenant_id, trace_id, span_id`,
		"05 expires":    `SELECT tenant_id FROM spans_local WHERE expires_at > now() LIMIT 1`,
		"06 count":      `SELECT count() FROM spans_local`,
		"07 join":       `SELECT count() FROM (SELECT tenant_id, trace_id, span_id FROM spans_local) AS c LEFT JOIN (SELECT tenant_id, trace_id, parent_span_id FROM spans_local) AS s ON s.tenant_id = c.tenant_id AND s.trace_id = c.trace_id AND s.parent_span_id = c.span_id`,
		"08 joinconst":  `SELECT count() FROM (SELECT tenant_id, trace_id, span_id FROM spans_local) AS c LEFT JOIN (SELECT tenant_id, trace_id, parent_span_id, toUInt8(1) AS matched FROM spans_local) AS s ON s.tenant_id = c.tenant_id AND s.trace_id = c.trace_id AND s.parent_span_id = c.span_id`,
		"09 param":      `SELECT tenant_id FROM spans_local WHERE event_time >= {from:DateTime64(9, 'UTC')} AND event_time < {to:DateTime64(9, 'UTC')} - INTERVAL 1 MINUTE LIMIT 1`,
		"10 client":     clientPairsSQL,
		"11 fallback":   fallbackPairsSQL,
		"12 in":         `SELECT count() FROM spans_local WHERE (tenant_id, trace_id, span_id) IN (SELECT tenant_id, trace_id, parent_span_id FROM spans_local)`,
		"13 sumforeach": `SELECT sumForEach(arrayMap(b -> toUInt64(duration_ns / 1e6 <= b), {bounds:Array(Float64)})) FROM spans_local`,
	}
	var out []string
	for name, q := range shapes {
		rows, err := conn.Query(ctx, q, args...)
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			_ = rows.Close()
		}
		msg := "ok"
		if err != nil {
			msg = err.Error()
			if len(msg) > 200 {
				msg = msg[:200]
			}
		}
		out = append(out, fmt.Sprintf("%s: %s", name, msg))
	}
	t.Fatalf("version %s\n%s", ver, strings.Join(out, "\n"))
}
