package telemetrystore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/polynomeer/montracer/internal/authz"
)

// metric 사전 한도 (ADR 0046).
const (
	// MaxMetricCatalogRange는 사전 조회 최대 범위다. 1분 rollup을 읽으므로 하루로 묶는다.
	MaxMetricCatalogRange = 24 * time.Hour
	// MaxMetricCatalogPage는 page당 최대 metric 이름 수다(D02 §19 limit 1,000).
	MaxMetricCatalogPage = 1000
	// MaxMetricLabelKeys는 label key 목록 상한이다. 넘으면 series가 많은 순으로 자르고 잘렸다고 알린다.
	MaxMetricLabelKeys = 200
)

// MetricCatalogQuery는 metric 사전 조회 조건이다.
type MetricCatalogQuery struct {
	Range TimeRange
	// Contains는 이름 부분 일치(대소문자 무시)다. 비어 있으면 전체.
	Contains string
	// After는 이전 page의 마지막 이름이다(keyset). 비어 있으면 처음부터.
	After string
	Limit int
}

// MetricVariant는 이름 하나에서 관측된 (유형, 단위, temporality, monotonic) 조합이다.
// 한 이름에 조합이 둘 이상이면 계측 충돌이다(D02 §10 "unit 충돌은 격리 metric"). 고르지 않고 모두 돌려준다.
type MetricVariant struct {
	Type        string // gauge | sum | histogram | exponential_histogram | summary
	Temporality string // unspecified | delta | cumulative
	Monotonic   bool
	Unit        string
	Series      int       // 범위 안에서 관측된 stream 수
	LastSeen    time.Time // 가장 늦은 1분 window의 끝
}

// MetricDescriptor는 metric 이름과 관측된 조합들이다.
type MetricDescriptor struct {
	Name     string
	Variants []MetricVariant
}

// MetricLabelKey는 metric의 label key다. Sources는 attribute(point)·resource 중 관측된 곳이다.
// metric 조회는 point 속성을 먼저, 없으면 resource 속성을 본다(ADR 0027).
type MetricLabelKey struct {
	Key     string
	Sources []string
	Series  int // 이 key를 가진 stream 수
}

func (q MetricCatalogQuery) validate() error {
	if err := q.Range.validate(MaxMetricCatalogRange); err != nil {
		return err
	}
	if q.Limit < 1 || q.Limit > MaxMetricCatalogPage {
		return invalidArg("limit", fmt.Sprintf("must be within [1, %d]", MaxMetricCatalogPage))
	}
	if len(q.Contains) > MaxLabelLen || !utf8.ValidString(q.Contains) {
		return invalidArg("q", "at most 256 bytes of UTF-8")
	}
	if len(q.After) > MaxLabelLen {
		return invalidArg("cursor", "invalid position")
	}
	return nil
}

// catalogScope는 사전·label 조회의 공통 mandatory predicate다(D02 §15).
// 삭제 tombstone predicate는 삭제 원장(F09) 구현 시 추가한다 (ADR 0018 §7, MetricBuckets와 같다).
// tenant·시간·expires_at, environment가 제한된 principal은 허용 environment의 stream만(MetricBuckets와 같다).
// 값은 모두 서버 측 parameter다(ADR 0032).
type catalogScope struct {
	args  []any
	where string
}

func newCatalogScope(p authz.Principal, r TimeRange, now time.Time) *catalogScope {
	s := &catalogScope{args: []any{
		clickhouse.Named("tenant", p.Tenant().String()),
		clickhouse.Named("from", seconds(r.From)), clickhouse.Named("to", seconds(r.To)),
		clickhouse.Named("now", seconds(now)),
	}}
	where := []string{
		"tenant_id = {tenant:UUID}",
		"window_start >= {from:DateTime('UTC')}", "window_start < {to:DateTime('UTC')}",
		"expires_at > {now:DateTime('UTC')}",
	}
	if envs := p.Environments(); envs != nil {
		where = append(where, "has("+s.arg("Array(String)", envs)+", JSONExtractString(resource_json, 'deployment.environment.name'))")
	}
	s.where = strings.Join(where, " AND ")
	return s
}

func (s *catalogScope) arg(typ string, v any) string {
	name := fmt.Sprintf("p%d", len(s.args))
	s.args = append(s.args, clickhouse.Named(name, v))
	return "{" + name + ":" + typ + "}"
}

// MetricCatalog는 범위 안에서 관측된 metric 이름과 유형·단위·temporality를 이름순으로 돌려준다(D05 §08 metric dictionary).
// more가 참이면 다음 page가 있다. 1분 rollup(metric_1m)을 읽는다 — 원본(metric_points)보다 행이 적고 query 계정이 읽을 수 있다.
func (s *QueryStore) MetricCatalog(ctx context.Context, p authz.Principal, q MetricCatalogQuery, now time.Time) ([]MetricDescriptor, bool, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, false, err
	}
	if now.IsZero() {
		return nil, false, errors.New("telemetrystore: now is required")
	}
	if err := q.validate(); err != nil {
		return nil, false, err
	}
	sc := newCatalogScope(p, q.Range, now)
	names := sc.where
	if q.Contains != "" {
		names += " AND positionCaseInsensitiveUTF8(metric_name, " + sc.arg("String", q.Contains) + ") > 0"
	}
	if q.After != "" {
		names += " AND metric_name > " + sc.arg("String", q.After)
	}
	limit := sc.arg("UInt32", q.Limit+1)
	query := `
		SELECT metric_name, toString(type), toString(temporality), is_monotonic, unit, uniqExact(stream_id), max(window_start)
		FROM metric_1m
		WHERE ` + sc.where + ` AND metric_name IN (
			SELECT DISTINCT metric_name FROM metric_1m WHERE ` + names + ` ORDER BY metric_name LIMIT ` + limit + `
		)
		GROUP BY metric_name, type, temporality, is_monotonic, unit
		ORDER BY metric_name, type, unit, temporality, is_monotonic`
	rows, err := s.conn.Query(tenantContext(ctx, p.Tenant()), query, sc.args...)
	if err != nil {
		return nil, false, classify("metric catalog", err)
	}
	defer func() { _ = rows.Close() }()
	var out []MetricDescriptor
	for rows.Next() {
		var (
			name   string
			v      MetricVariant
			series uint64
		)
		if err := rows.Scan(&name, &v.Type, &v.Temporality, &v.Monotonic, &v.Unit, &series, &v.LastSeen); err != nil {
			return nil, false, fmt.Errorf("telemetrystore: scan metric catalog: %w", err)
		}
		v.Series = int(series) //nolint:gosec // stream 수는 tenant 활성 series 상한(100k) 안이다
		v.LastSeen = v.LastSeen.UTC().Add(time.Minute)
		if n := len(out); n > 0 && out[n-1].Name == name {
			out[n-1].Variants = append(out[n-1].Variants, v)
			continue
		}
		out = append(out, MetricDescriptor{Name: name, Variants: []MetricVariant{v}})
	}
	if err := rows.Err(); err != nil {
		return nil, false, classify("metric catalog", err)
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return out, more, nil
}

// MetricLabelKeys는 metric 하나의 label key를 series가 많은 순으로 돌려준다(group_by·filter 선택지).
// stream마다 범위 안 가장 늦은 window의 label을 쓴다(metric 조회와 같은 label 원천, ADR 0027 §4).
// truncated가 참이면 MaxMetricLabelKeys에서 잘렸다. 값은 돌려주지 않는다.
func (s *QueryStore) MetricLabelKeys(ctx context.Context, p authz.Principal, metric string, r TimeRange, now time.Time) ([]MetricLabelKey, bool, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, false, err
	}
	if now.IsZero() {
		return nil, false, errors.New("telemetrystore: now is required")
	}
	if metric == "" || len(metric) > MaxLabelLen {
		return nil, false, invalidArg("metric", "required, at most 256 bytes")
	}
	if err := r.validate(MaxMetricCatalogRange); err != nil {
		return nil, false, err
	}
	sc := newCatalogScope(p, r, now)
	m := sc.arg("String", metric)
	limit := sc.arg("UInt32", MaxMetricLabelKeys+1)
	query := `
		SELECT tupleElement(kv, 1) AS key, groupUniqArray(tupleElement(kv, 2)) AS sources, uniqExact(stream_id) AS n
		FROM (
			SELECT stream_id, argMax(attributes_json, window_start) AS a, argMax(resource_json, window_start) AS r
			FROM metric_1m
			WHERE ` + sc.where + ` AND metric_name = ` + m + `
			GROUP BY stream_id
		)
		ARRAY JOIN arrayConcat(arrayMap(k -> (k, 'attribute'), JSONExtractKeys(a)), arrayMap(k -> (k, 'resource'), JSONExtractKeys(r))) AS kv
		GROUP BY key
		ORDER BY n DESC, key
		LIMIT ` + limit
	rows, err := s.conn.Query(tenantContext(ctx, p.Tenant()), query, sc.args...)
	if err != nil {
		return nil, false, classify("metric labels", err)
	}
	defer func() { _ = rows.Close() }()
	var out []MetricLabelKey
	for rows.Next() {
		var (
			k MetricLabelKey
			n uint64
		)
		if err := rows.Scan(&k.Key, &k.Sources, &n); err != nil {
			return nil, false, fmt.Errorf("telemetrystore: scan metric labels: %w", err)
		}
		k.Series = int(n) //nolint:gosec // stream 수는 tenant 활성 series 상한(100k) 안이다
		slices.Sort(k.Sources)
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, false, classify("metric labels", err)
	}
	truncated := len(out) > MaxMetricLabelKeys
	if truncated {
		out = out[:MaxMetricLabelKeys]
	}
	return out, truncated, nil
}
