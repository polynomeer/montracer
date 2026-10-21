package servicemap

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// ErrTooPrivileged는 service map 계정이 edge 계산에 필요 없는 원본(span payload, log, metric)을 읽을 수 있을 때 반환한다.
// 전 tenant span을 읽는 시스템 job이라 필요한 컬럼만 준다 (migration 00006, ADR 0062).
var ErrTooPrivileged = errors.New("servicemap: account must not read span payload, logs, or metrics")

// ClickHouseStore는 service map 계정 연결이다.
type ClickHouseStore struct {
	conn driver.Conn
}

// OpenClickHouse는 service map 계정으로 연결하고 권한이 좁은지 확인한다.
func OpenClickHouse(ctx context.Context, dsn string) (*ClickHouseStore, error) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, errors.New("servicemap: invalid clickhouse dsn") // DSN에는 비밀번호가 있다
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("servicemap: clickhouse open: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("servicemap: clickhouse ping: %w", err)
	}
	// 읽을 컬럼을 SELECT 목록에 직접 쓴다. count()·WHERE 0 조건 안의 컬럼은 상수 접기로 빠져 권한 검사를 받지 않는다(PS-0015).
	for _, q := range []string{
		`SELECT payload FROM spans_local WHERE 0`,
		`SELECT name FROM spans_local WHERE 0`,
		`SELECT body FROM logs_local WHERE 0`,
		`SELECT value FROM metric_points WHERE 0`,
		`SELECT total FROM metric_1m WHERE 0`,
		`SELECT trace_id FROM trace_lookup WHERE 0`,
		`SELECT reason FROM ingest_quarantine WHERE 0`,
		`SELECT requests FROM service_edges_1m WHERE 0`,
	} {
		var n uint64
		err := conn.QueryRow(ctx, q).Scan(&n)
		if err == nil {
			_ = conn.Close()
			return nil, ErrTooPrivileged
		}
		var ex *clickhouse.Exception
		if !errors.As(err, &ex) || ex.Code != 497 { // ACCESS_DENIED
			_ = conn.Close()
			return nil, fmt.Errorf("servicemap: clickhouse grant check: %w", err)
		}
	}
	return &ClickHouseStore{conn: conn}, nil
}

// Close는 연결을 닫는다.
func (s *ClickHouseStore) Close() error { return s.conn.Close() }

// MaxObserved는 tenant별 최대 span 시작 시각이다 (tenant별 watermark).
func (s *ClickHouseStore) MaxObserved(ctx context.Context, since, until time.Time) (map[string]time.Time, error) {
	rows, err := s.conn.Query(ctx, `SELECT toString(tenant_id), max(event_time) FROM spans_local
		WHERE event_time >= {since:DateTime64(9, 'UTC')} AND event_time <= {until:DateTime64(9, 'UTC')} GROUP BY tenant_id`,
		clickhouse.Named("since", since), clickhouse.Named("until", until))
	if err != nil {
		return nil, fmt.Errorf("servicemap: max observed: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]time.Time{}
	for rows.Next() {
		var (
			tenant string
			t      time.Time
		)
		if err := rows.Scan(&tenant, &t); err != nil {
			return nil, fmt.Errorf("servicemap: scan max observed: %w", err)
		}
		out[tenant] = t.UTC()
	}
	return out, rows.Err()
}

// LoadProgress는 service_edges_1m에서 tenant별 마지막 window 끝과 최대 revision을 읽는다.
// 이 계정은 edges 테이블의 세 컬럼만 읽을 수 있다(집계 값은 읽지 못한다).
func (s *ClickHouseStore) LoadProgress(ctx context.Context, since time.Time) (Progress, error) {
	p := Progress{Done: map[string]time.Time{}}
	rows, err := s.conn.Query(ctx, `SELECT toString(tenant_id), max(window_start), max(revision) FROM service_edges_1m
		WHERE window_start >= {since:DateTime('UTC')} GROUP BY tenant_id`, clickhouse.Named("since", since.Truncate(time.Second)))
	if err != nil {
		return Progress{}, fmt.Errorf("servicemap: load progress: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			tenant string
			last   time.Time
			rev    uint64
		)
		if err := rows.Scan(&tenant, &last, &rev); err != nil {
			return Progress{}, fmt.Errorf("servicemap: scan progress: %w", err)
		}
		p.Done[tenant] = last.UTC().Add(time.Minute)
		p.MaxRevision = max(p.MaxRevision, rev)
	}
	if err := rows.Err(); err != nil {
		return Progress{}, err
	}
	var all uint64
	if err := s.conn.QueryRow(ctx, `SELECT max(revision) FROM service_edges_1m`).Scan(&all); err != nil {
		return Progress{}, fmt.Errorf("servicemap: max revision: %w", err)
	}
	p.MaxRevision = max(p.MaxRevision, all)
	return p, nil
}

// clientPairsSQL은 client·producer span과 그 자식 server·consumer span의 짝이다.
// 자식은 부모보다 1분 이르거나(시계 차이) 5분 늦게까지 찾는다. 같은 span의 중복 행(merge 전)은 LIMIT 1 BY로 하나만 센다.
// 자식은 (부모, 서비스)마다 하나다: 같은 서비스로의 재시도는 한 호출이고, pub/sub fan-out(consumer 서비스 B·C)은 edge 둘이다.
// 짝이 없으면(LEFT JOIN 기본값) matched = 0이고 상대 속성으로 destination을 정한다(Destination).
const clientPairsSQL = `
SELECT toString(c.tenant_id), toStartOfMinute(c.event_time) AS ws, toString(c.service_id), s.matched, toString(s.service_id),
       c.span_kind, c.peer, c.db, c.dbns, c.mq, c.host,
       count(), countIf(c.status = 2 OR (s.matched = 1 AND s.status = 2)),
       sumForEach(arrayMap(b -> toUInt64(if(c.span_kind = 4 AND s.matched = 1, s.duration_ns, c.duration_ns) / 1e6 <= b), {bounds:Array(Float64)})),
       countIf(c.span_kind = 4 AND s.matched = 1),
       sumForEachIf(arrayMap(b -> toUInt64(greatest(toInt64(toUnixTimestamp64Nano(s.event_time)) - toInt64(toUnixTimestamp64Nano(c.event_time)) - toInt64(c.duration_ns), 0) / 1e6 <= b), {bounds:Array(Float64)}),
                    c.span_kind = 4 AND s.matched = 1),
       max(c.event_time)
FROM (
  SELECT tenant_id, trace_id, span_id, service_id, span_kind, status, duration_ns, event_time,
         attributes['peer.service'] AS peer,
         if(attributes['db.system.name'] != '', attributes['db.system.name'], attributes['db.system']) AS db,
         if(attributes['db.namespace'] != '', attributes['db.namespace'], attributes['db.name']) AS dbns,
         attributes['messaging.system'] AS mq,
         multiIf(attributes['server.address'] != '', attributes['server.address'],
                 attributes['net.peer.name'] != '', attributes['net.peer.name'],
                 attributes['url.full'] != '', domain(attributes['url.full']),
                 domain(attributes['http.url'])) AS host
  FROM spans_local
  WHERE event_time >= {from:DateTime64(9, 'UTC')} AND event_time < {to:DateTime64(9, 'UTC')}
    AND span_kind IN (3, 4) AND expires_at > now()
  LIMIT 1 BY tenant_id, trace_id, span_id
) AS c
LEFT JOIN (
  SELECT tenant_id, trace_id, parent_span_id, service_id, status, duration_ns, event_time, toUInt8(1) AS matched
  FROM spans_local
  WHERE event_time >= {from:DateTime64(9, 'UTC')} - INTERVAL 1 MINUTE AND event_time < {to:DateTime64(9, 'UTC')} + INTERVAL 5 MINUTE
    AND span_kind IN (2, 5) AND expires_at > now()
  LIMIT 1 BY tenant_id, trace_id, parent_span_id, service_id
) AS s ON s.tenant_id = c.tenant_id AND s.trace_id = c.trace_id AND s.parent_span_id = c.span_id
GROUP BY c.tenant_id, ws, c.service_id, s.matched, s.service_id, c.span_kind, c.peer, c.db, c.dbns, c.mq, c.host`

// fallbackPairsSQL은 client span 없이 다른 서비스의 internal·server span 아래에서 시작한 server·consumer span이다
// (D02 §08 "client가 없을 때 server를 fallback으로"). 부모가 client·producer면 client 쪽에서 이미 셌으므로 뺀다.
const fallbackPairsSQL = `
SELECT toString(s.tenant_id), toStartOfMinute(s.event_time) AS ws, toString(p.service_id), toString(s.service_id), s.span_kind,
       count(), countIf(s.status = 2),
       sumForEach(arrayMap(b -> toUInt64(s.duration_ns / 1e6 <= b), {bounds:Array(Float64)})),
       max(s.event_time)
FROM (
  SELECT tenant_id, trace_id, span_id, parent_span_id, service_id, span_kind, status, duration_ns, event_time
  FROM spans_local
  WHERE event_time >= {from:DateTime64(9, 'UTC')} AND event_time < {to:DateTime64(9, 'UTC')}
    AND span_kind IN (2, 5) AND hex(parent_span_id) != '0000000000000000' AND expires_at > now()
  LIMIT 1 BY tenant_id, trace_id, span_id
) AS s
INNER JOIN (
  SELECT tenant_id, trace_id, span_id, service_id, span_kind
  FROM spans_local
  WHERE event_time >= {from:DateTime64(9, 'UTC')} - INTERVAL 5 MINUTE AND event_time < {to:DateTime64(9, 'UTC')} + INTERVAL 1 MINUTE
    AND expires_at > now()
    AND (tenant_id, trace_id, span_id) IN (
      SELECT tenant_id, trace_id, parent_span_id FROM spans_local
      WHERE event_time >= {from:DateTime64(9, 'UTC')} AND event_time < {to:DateTime64(9, 'UTC')} AND span_kind IN (2, 5))
  LIMIT 1 BY tenant_id, trace_id, span_id
) AS p ON p.tenant_id = s.tenant_id AND p.trace_id = s.trace_id AND p.span_id = s.parent_span_id
WHERE p.service_id != s.service_id AND p.span_kind NOT IN (3, 4)
GROUP BY s.tenant_id, ws, p.service_id, s.service_id, s.span_kind`

// ReadPairs는 [from, to)에서 시작한 기준 span의 pair를 읽는다.
func (s *ClickHouseStore) ReadPairs(ctx context.Context, from, to time.Time) ([]Pair, error) {
	args := []any{clickhouse.Named("from", from), clickhouse.Named("to", to), clickhouse.Named("bounds", Bounds)}
	// attributes['k']는 기본으로 Map 부분 열 읽기로 바뀌는데, 부분 열은 열 단위 GRANT로 허용되지 않아 표 전체 SELECT를
	// 요구한다(ClickHouse 26.8). 이 계정은 열만 받으므로 그 최적화를 끈다(PS-0015).
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"optimize_functions_to_subcolumns": 0}))
	var out []Pair
	rows, err := s.conn.Query(ctx, clientPairsSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("servicemap: client pairs: %w", err)
	}
	for rows.Next() {
		var (
			p       Pair
			matched uint8
		)
		if err := rows.Scan(&p.Tenant, &p.WindowStart, &p.Source, &matched, &p.Target, &p.Kind, &p.Peer, &p.DB, &p.DBNamespace,
			&p.Messaging, &p.Host, &p.Requests, &p.Errors, &p.LatencyLE, &p.WaitCount, &p.WaitLE, &p.LastSeen); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("servicemap: scan client pair: %w", err)
		}
		p.Matched = matched == 1
		if !p.Matched {
			p.Target = ""
		}
		p.WindowStart, p.LastSeen = p.WindowStart.UTC(), p.LastSeen.UTC()
		out = append(out, p)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, fmt.Errorf("servicemap: client pairs: %w", err)
	}
	rows, err = s.conn.Query(ctx, fallbackPairsSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("servicemap: fallback pairs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		p := Pair{Matched: true}
		if err := rows.Scan(&p.Tenant, &p.WindowStart, &p.Source, &p.Target, &p.Kind, &p.Requests, &p.Errors, &p.LatencyLE,
			&p.LastSeen); err != nil {
			return nil, fmt.Errorf("servicemap: scan fallback pair: %w", err)
		}
		p.WindowStart, p.LastSeen = p.WindowStart.UTC(), p.LastSeen.UTC()
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("servicemap: fallback pairs: %w", err)
	}
	return out, nil
}

// WriteWindows는 window 행을 한 번에 쓴다. token은 내용 기반이라 같은 rows를 재시도해도 한 번만 들어간다.
func (s *ClickHouseStore) WriteWindows(ctx context.Context, rows []Row) error {
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"insert_deduplicate":         1,
		"insert_deduplication_token": ContentToken(rows),
		"async_insert":               0,
	}))
	batch, err := s.conn.PrepareBatch(ctx, `INSERT INTO service_edges_1m (tenant_id, window_start, source_service_id, destination,
		destination_kind, connection, requests, errors, bounds, buckets, wait_count, wait_buckets, last_seen, revision, computed_at, expires_at)`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := batch.Append(r.Tenant, r.WindowStart, r.Source, r.Destination, r.DestinationKind, r.Connection, r.Requests, r.Errors,
			r.Bounds, r.Buckets, r.WaitCount, r.WaitBuckets, r.LastSeen, r.Revision, r.ComputedAt, r.ExpiresAt); err != nil {
			_ = batch.Abort()
			return errors.New("servicemap: append edge row failed") // 드라이버 문구에 값이 실릴 수 있다
		}
	}
	return batch.Send()
}

var _ Store = (*ClickHouseStore)(nil)
