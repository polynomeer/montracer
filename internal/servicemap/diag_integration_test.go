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
		"A map setting off":   `SELECT attributes['peer.service'] FROM spans_local LIMIT 1 SETTINGS optimize_functions_to_subcolumns = 0`,
		"B arrayElement":      `SELECT arrayElement(attributes, 'peer.service') FROM spans_local LIMIT 1`,
		"C mapValues":         `SELECT mapValues(attributes)[indexOf(mapKeys(attributes), 'peer.service')] FROM spans_local LIMIT 1`,
		"D mapValues off":     `SELECT mapValues(attributes)[indexOf(mapKeys(attributes), 'peer.service')] FROM spans_local LIMIT 1 SETTINGS optimize_functions_to_subcolumns = 0`,
		"E whole attr":        `SELECT attributes FROM spans_local LIMIT 1`,
		"F subquery then key": `SELECT a['peer.service'] FROM (SELECT attributes AS a FROM spans_local) LIMIT 1`,
		"G analyzer off":      `SELECT attributes['peer.service'] FROM spans_local LIMIT 1 SETTINGS allow_experimental_analyzer = 0`,
		"H materialize":       `SELECT materialize(attributes)['peer.service'] FROM spans_local LIMIT 1`,
		"I mapContains":       `SELECT mapContains(attributes, 'peer.service') FROM spans_local LIMIT 1`,
		"J client off":        clientPairsSQL + ` SETTINGS optimize_functions_to_subcolumns = 0`,
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
