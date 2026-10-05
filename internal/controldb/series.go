package controldb

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

// SeriesStore는 metric 활성 series 등록부다 (migration 00002, ADR 0029).
// 모든 작업은 WithTenant 안에서 RLS로 그 tenant의 행만 다룬다 (ADR 0016).
type SeriesStore struct {
	db *DB
}

// NewSeriesStore는 SeriesStore를 만든다.
func NewSeriesStore(db *DB) *SeriesStore { return &SeriesStore{db: db} }

// SeriesEntry는 등록할 series 하나다.
type SeriesEntry struct {
	StreamID [16]byte
	Metric   string
}

func byteIDs(ids [][16]byte) [][]byte {
	out := make([][]byte, len(ids))
	for i := range ids {
		out[i] = ids[i][:]
	}
	return out
}

// Known은 ids 중 since 이후 활성인 series를 돌려준다.
func (s *SeriesStore) Known(ctx context.Context, tenant authz.TenantID, ids [][16]byte, since time.Time) (map[[16]byte]bool, error) {
	known := map[[16]byte]bool{}
	err := s.db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT stream_id FROM metric_series
			WHERE tenant_id = app_tenant_id() AND stream_id = ANY($1) AND last_seen > $2`, byteIDs(ids), since)
		if err != nil {
			return classify("series known", err)
		}
		defer rows.Close()
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				return classify("series known scan", err)
			}
			var id [16]byte
			copy(id[:], b)
			known[id] = true
		}
		return classify("series known rows", rows.Err())
	})
	return known, err
}

// ActiveCount는 since 이후 활성인 series 수다.
func (s *SeriesStore) ActiveCount(ctx context.Context, tenant authz.TenantID, since time.Time) (int, error) {
	var n int
	err := s.db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		return classify("series count", tx.QueryRow(ctx, `SELECT count(*) FROM metric_series
			WHERE tenant_id = app_tenant_id() AND last_seen > $1`, since).Scan(&n))
	})
	return n, err
}

// Touch는 series를 등록하거나 last_seen을 갱신한다(멱등).
func (s *SeriesStore) Touch(ctx context.Context, tenant authz.TenantID, entries []SeriesEntry, now time.Time) error {
	if len(entries) == 0 {
		return nil
	}
	ids := make([][]byte, len(entries))
	names := make([]string, len(entries))
	for i, e := range entries {
		ids[i], names[i] = e.StreamID[:], e.Metric
	}
	return s.db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO metric_series (tenant_id, stream_id, metric_name, first_seen, last_seen)
			SELECT app_tenant_id(), id, name, $3, $3 FROM unnest($1::bytea[], $2::text[]) AS t(id, name)
			ON CONFLICT (tenant_id, stream_id) DO UPDATE SET last_seen = GREATEST(metric_series.last_seen, EXCLUDED.last_seen)`,
			ids, names, now)
		return classify("series touch", err)
	})
}

// Cleanup은 before 이전에 마지막으로 본 series를 지운다(판정에는 영향 없음, 저장 공간 정리).
func (s *SeriesStore) Cleanup(ctx context.Context, tenant authz.TenantID, before time.Time) (int64, error) {
	var n int64
	err := s.db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM metric_series WHERE tenant_id = app_tenant_id() AND last_seen < $1`, before)
		n = tag.RowsAffected()
		return classify("series cleanup", err)
	})
	return n, err
}
