package telemetrystore

import (
	"context"
	"errors"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/polynomeer/montracer/internal/authz"
)

const (
	// MaxServiceMapRange는 service map 조회 최대 범위다(metric과 같은 7일, D02 §13).
	MaxServiceMapRange = 7 * 24 * time.Hour
	// MaxServiceEdgeRows는 한 번에 읽는 집계 edge 상한이다(query 계정 기본 결과 행 예산 10,000 안).
	// 넘으면 호출자가 잘렸다고 표시한다.
	MaxServiceEdgeRows = 9999
)

// ServiceEdgeQuery는 service map edge 조회 조건이다.
type ServiceEdgeQuery struct {
	// Range는 window 시작 시각 범위다. From은 분 경계로 내린다.
	Range TimeRange
	// EnvironmentServices는 environment로 제한된 principal이 볼 수 있는 service_id 집합이다(controldb.EnvironmentServiceIDs).
	// 제한된 principal이면 필수이고 source 서비스의 mandatory predicate가 된다. 제한 없으면 무시한다.
	EnvironmentServices []string
}

// ServiceEdge는 범위 안 window를 합친 edge 하나다(servicemap.Row의 합).
type ServiceEdge struct {
	Source          string
	Destination     string
	DestinationKind string // service | database | queue | external | peer
	Connection      string // rpc | messaging
	Requests        uint64
	Errors          uint64
	Bounds          []float64
	Buckets         []uint64
	WaitCount       uint64
	WaitBuckets     []uint64
	LastSeen        time.Time
}

// ServiceEdges는 범위 안 edge를 합쳐 읽는다. (tenant, window)마다 최대 revision의 행만 쓴다 —
// 늦은 span으로 다시 계산한 window는 key가 바뀔 수 있어 옛 revision 행을 섞으면 같은 호출을 두 번 센다.
// histogram 경계가 다른 window는 다른 행으로 돌려준다(호출자가 합치지 않는다). 요청 수가 많은 edge부터 상한까지 읽고,
// 넘으면 truncated=true다.
func (s *QueryStore) ServiceEdges(ctx context.Context, p authz.Principal, q ServiceEdgeQuery, now time.Time) ([]ServiceEdge, bool, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, false, err
	}
	scoped, scope := uint8(0), []string{}
	if p.EnvironmentRestricted() {
		if q.EnvironmentServices == nil {
			return nil, false, ErrEnvironmentScoped
		}
		if len(q.EnvironmentServices) > MaxEnvironmentServices {
			return nil, false, errors.New("telemetrystore: environment service scope too large")
		}
		scoped, scope = 1, q.EnvironmentServices
	}
	if now.IsZero() {
		return nil, false, errors.New("telemetrystore: now is required")
	}
	if err := q.Range.validate(MaxServiceMapRange); err != nil {
		return nil, false, err
	}
	tenant := p.Tenant()
	rows, err := s.conn.Query(tenantContext(ctx, tenant), `
		WITH latest AS (
			SELECT window_start, max(revision) AS revision FROM service_edges_1m
			WHERE tenant_id = {tenant:UUID} AND window_start >= {from:DateTime('UTC')} AND window_start < {to:DateTime('UTC')}
			  AND expires_at > {now:DateTime('UTC')}
			GROUP BY window_start
		)
		SELECT toString(source_service_id), destination, toString(destination_kind), toString(connection),
		       sum(requests), sum(errors), bounds, sumForEach(buckets), sum(wait_count), sumForEach(wait_buckets), max(last_seen)
		FROM service_edges_1m
		WHERE tenant_id = {tenant:UUID} AND window_start >= {from:DateTime('UTC')} AND window_start < {to:DateTime('UTC')}
		  AND expires_at > {now:DateTime('UTC')}
		  AND (window_start, revision) IN (SELECT window_start, revision FROM latest)
		  AND destination_kind != 'window'
		  AND ({scoped:UInt8} = 0 OR has({scope:Array(UUID)}, source_service_id))
		GROUP BY source_service_id, destination, destination_kind, connection, bounds
		ORDER BY sum(requests) DESC, source_service_id, destination, connection
		LIMIT {limit:UInt32}`,
		clickhouse.Named("tenant", tenant.String()),
		clickhouse.Named("from", seconds(q.Range.From.Truncate(time.Minute))), clickhouse.Named("to", seconds(q.Range.To)),
		clickhouse.Named("now", seconds(now)), clickhouse.Named("scoped", scoped), clickhouse.Named("scope", scope),
		clickhouse.Named("limit", MaxServiceEdgeRows+1))
	if err != nil {
		return nil, false, classify("service edges", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ServiceEdge
	for rows.Next() {
		var e ServiceEdge
		if err := rows.Scan(&e.Source, &e.Destination, &e.DestinationKind, &e.Connection, &e.Requests, &e.Errors, &e.Bounds,
			&e.Buckets, &e.WaitCount, &e.WaitBuckets, &e.LastSeen); err != nil {
			return nil, false, classify("scan service edge", err)
		}
		e.LastSeen = e.LastSeen.UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, classify("service edges", err)
	}
	truncated := len(out) > MaxServiceEdgeRows
	if truncated {
		out = out[:MaxServiceEdgeRows]
	}
	return out, truncated, nil
}

// ServiceEdgeWatermark는 tenant의 edge rollup이 마지막으로 계산한 window의 끝이다(edge가 없는 window도 marker 행으로 센다).
// 범위 안에 행이 없으면 zero time.
func (s *QueryStore) ServiceEdgeWatermark(ctx context.Context, p authz.Principal, since, now time.Time) (time.Time, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return time.Time{}, err
	}
	tenant := p.Tenant()
	var (
		last time.Time
		n    uint64
	)
	if err := s.conn.QueryRow(tenantContext(ctx, tenant), `SELECT max(window_start), count() FROM service_edges_1m
		WHERE tenant_id = {tenant:UUID} AND window_start >= {since:DateTime('UTC')} AND expires_at > {now:DateTime('UTC')}`,
		clickhouse.Named("tenant", tenant.String()), clickhouse.Named("since", seconds(since)), clickhouse.Named("now", seconds(now))).Scan(&last, &n); err != nil {
		return time.Time{}, classify("service edge watermark", err)
	}
	if n == 0 {
		return time.Time{}, nil
	}
	return last.UTC().Add(time.Minute), nil
}
