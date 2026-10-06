package rollup

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// ErrRollupTooPrivileged는 rollup 계정이 metric 밖의 원본(span 등)을 읽을 수 있을 때 반환한다.
// rollup은 전 tenant metric을 읽는 시스템 job이라 그 밖의 권한은 주지 않는다 (ADR 0026 §3).
var ErrRollupTooPrivileged = errors.New("rollup: account must not be able to read span or log tables")

// ClickHouseStore는 rollup 계정 연결이다.
type ClickHouseStore struct {
	conn  driver.Conn
	table string        // metric_1m | metric_1h (tableFor의 고정 목록)
	win   time.Duration // table의 window
}

// tableFor는 rollup이 쓰는 테이블이다. SQL에 들어가는 이름은 이 고정 목록에서만 고른다(사용자 입력 아님).
func tableFor(w time.Duration) (string, bool) {
	switch w {
	case time.Minute:
		return "metric_1m", true
	case time.Hour:
		return "metric_1h", true
	default:
		return "", false
	}
}

// ForWindow는 같은 연결로 window 크기에 맞는 테이블을 쓰는 store다.
func (s *ClickHouseStore) ForWindow(w time.Duration) (*ClickHouseStore, error) {
	t, ok := tableFor(w)
	if !ok {
		return nil, fmt.Errorf("rollup: no table for window %s", w)
	}
	return &ClickHouseStore{conn: s.conn, table: t, win: w}, nil
}

// OpenClickHouse는 rollup 계정으로 연결하고 권한을 확인한다.
func OpenClickHouse(ctx context.Context, dsn string) (*ClickHouseStore, error) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, errors.New("rollup: invalid clickhouse dsn") // DSN에는 비밀번호가 있다
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("rollup: clickhouse open: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("rollup: clickhouse ping: %w", err)
	}
	var n uint64
	err = conn.QueryRow(ctx, `SELECT count() FROM spans_local WHERE 0`).Scan(&n)
	if err == nil {
		_ = conn.Close()
		return nil, ErrRollupTooPrivileged
	}
	var ex *clickhouse.Exception
	if !errors.As(err, &ex) || ex.Code != 497 {
		_ = conn.Close()
		return nil, fmt.Errorf("rollup: clickhouse grant check: %w", err)
	}
	return &ClickHouseStore{conn: conn, table: "metric_1m", win: time.Minute}, nil
}

// Close는 연결을 닫는다.
func (s *ClickHouseStore) Close() error { return s.conn.Close() }

// MaxObserved는 tenant별 최대 관측 시각이다 (tenant별 watermark, ADR 0026 §2).
func (s *ClickHouseStore) MaxObserved(ctx context.Context, since, until time.Time) (map[string]time.Time, error) {
	rows, err := s.conn.Query(ctx, `SELECT toString(tenant_id), max(end_time) FROM metric_points
		WHERE end_time >= $1 AND end_time <= $2 GROUP BY tenant_id`, since, until)
	if err != nil {
		return nil, fmt.Errorf("rollup: max observed: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]time.Time{}
	for rows.Next() {
		var (
			tenant string
			t      time.Time
		)
		if err := rows.Scan(&tenant, &t); err != nil {
			return nil, fmt.Errorf("rollup: scan max observed: %w", err)
		}
		out[tenant] = t.UTC()
	}
	return out, rows.Err()
}

// LoadProgress는 rollup 테이블에서 tenant별 마지막 window 끝과 최대 revision을 읽는다.
// rollup 계정은 metric_1m의 이 세 컬럼만 읽을 수 있다(값은 읽지 못한다, migration 00003).
func (s *ClickHouseStore) LoadProgress(ctx context.Context, since time.Time) (Progress, error) {
	p := Progress{Done: map[string]time.Time{}}
	rows, err := s.conn.Query(ctx, `SELECT toString(tenant_id), max(window_start), max(revision) FROM `+s.table+`
		WHERE window_start >= $1 GROUP BY tenant_id`, since)
	if err != nil {
		return Progress{}, fmt.Errorf("rollup: load progress: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			tenant string
			last   time.Time
			rev    uint64
		)
		if err := rows.Scan(&tenant, &last, &rev); err != nil {
			return Progress{}, fmt.Errorf("rollup: scan progress: %w", err)
		}
		p.Done[tenant] = last.UTC().Add(s.win)
		p.MaxRevision = max(p.MaxRevision, rev)
	}
	if err := rows.Err(); err != nil {
		return Progress{}, err
	}
	// 전 기간 최대 revision (since 밖에 더 큰 값이 있으면 새 revision이 그보다 작아질 수 있다)
	var all uint64
	if err := s.conn.QueryRow(ctx, `SELECT max(revision) FROM `+s.table).Scan(&all); err != nil {
		return Progress{}, fmt.Errorf("rollup: max revision: %w", err)
	}
	p.MaxRevision = max(p.MaxRevision, all)
	return p, nil
}

// ReadPoints는 end_time ∈ [from, to)이고 만료되지 않은 point를 point identity별 최신 version 하나로 읽는다
// (ReplacingMergeTree merge 전 중복 제거, FINAL 미사용 — ADR 0018 §5).
func (s *ClickHouseStore) ReadPoints(ctx context.Context, from, to time.Time) ([]RawPoint, error) {
	return s.readPoints(ctx, "", from, to)
}

// ReadTenantPoints는 tenant 하나의 end_time ∈ [from, to)인 dedup된 원본 point를 읽는다(backfill).
func (s *ClickHouseStore) ReadTenantPoints(ctx context.Context, tenant string, from, to time.Time) ([]RawPoint, error) {
	if tenant == "" {
		return nil, fmt.Errorf("rollup: tenant is required")
	}
	return s.readPoints(ctx, tenant, from, to)
}

// readPoints는 tenant가 비면 전체(live rollup), 있으면 그 tenant만(backfill) 읽는다. 값은 모두 bound parameter다.
func (s *ClickHouseStore) readPoints(ctx context.Context, tenant string, from, to time.Time) ([]RawPoint, error) {
	rows, err := s.conn.Query(ctx, `
		SELECT toString(tenant_id), stream_id, metric_name, unit, toString(type), toString(temporality), is_monotonic,
		       resource_json, attributes_json, version, start_time, end_time, value, count, sum, bounds, buckets
		FROM metric_points
		WHERE end_time >= $1 AND end_time < $2 AND expires_at > now()
		  AND ($3 = '' OR tenant_id = toUUIDOrZero($3))
		ORDER BY tenant_id, stream_id, end_time, point_hash, version DESC
		LIMIT 1 BY tenant_id, stream_id, end_time, point_hash`, from, to, tenant)
	if err != nil {
		return nil, fmt.Errorf("rollup: query points: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []RawPoint
	for rows.Next() {
		var (
			p      RawPoint
			stream string
			sum    float64
		)
		if err := rows.Scan(&p.Tenant, &stream, &p.MetricName, &p.Unit, &p.Type, &p.Temporality, &p.Monotonic,
			&p.ResourceJSON, &p.AttributesJSON, &p.Version, &p.Point.Start, &p.Point.End, &p.Point.Value, &p.Point.Count, &sum, &p.Point.Bounds, &p.Point.Buckets); err != nil {
			return nil, fmt.Errorf("rollup: scan point: %w", err)
		}
		copy(p.StreamID[:], stream)
		// worker는 보내지 않은 sum을 NaN으로 저장한다(ADR 0021 §5)
		p.Point.Sum, p.Point.HasSum = sum, !math.IsNaN(sum)
		out = append(out, p)
	}
	return out, rows.Err()
}

// WriteWindows는 window 행을 한 번에 쓴다. token은 내용 기반이라 같은 rows를 재시도해도 한 번만 들어간다.
func (s *ClickHouseStore) WriteWindows(ctx context.Context, rows []Row) error {
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"insert_deduplicate":         1,
		"insert_deduplication_token": ContentToken(rows),
		"async_insert":               0,
	}))
	batch, err := s.conn.PrepareBatch(ctx, `INSERT INTO `+s.table+` (tenant_id, metric_name, stream_id, window_start, type,
		temporality, is_monotonic, unit, resource_json, attributes_json, samples, has_value, last, min, max, total, has_increase, increase, has_histogram,
		count, hist_sum, bounds, buckets, resets, flags, partial, revision, computed_at, expires_at)`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		a := r.Agg
		bounds, buckets, flags := a.Bounds, a.Buckets, a.Flags
		if bounds == nil {
			bounds = []float64{}
		}
		if buckets == nil {
			buckets = []uint64{}
		}
		if flags == nil {
			flags = []string{}
		}
		if err := batch.Append(r.Tenant, r.MetricName, string(r.StreamID[:]), r.WindowStart, r.Type, r.Temporality,
			r.Monotonic, r.Unit, r.ResourceJSON, r.AttributesJSON, uint32(min(a.Samples, math.MaxUint32)), a.HasValue, a.Last, a.Min, a.Max, a.Total, //nolint:gosec // 상한으로 자름
			a.HasIncrease, a.Increase, a.HasHistogram, a.Count, a.HistSum, bounds, buckets,
			uint32(min(a.Resets, math.MaxUint32)), flags, a.Partial, r.Revision, r.ComputedAt, r.ExpiresAt); err != nil { //nolint:gosec // 상한으로 자름
			_ = batch.Abort()
			return errors.New("rollup: append window row failed") // 드라이버 문구에 값이 실릴 수 있다
		}
	}
	return batch.Send()
}

var _ Store = (*ClickHouseStore)(nil)
