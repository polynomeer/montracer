package rollup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
)

// Backfill은 tenant 하나의 과거 window를 원본에서 다시 계산한다 (D02 §07 "10분 이후는 backfill job만 허용", ADR 0035).
//
// live rollup(Job)은 watermark − 10분(1h는 1시간)만 다시 계산한다. 그보다 늦게 온 point, 처음 보는 tenant의 과거 구간,
// 따라잡기 상한을 넘은 gap은 live rollup이 다루지 않는다. Backfill이 그 구간을 같은 계산(Job.compute)으로 채운다.
//
//   - 원본(metric_points)의 dedup된 point만 쓴다. 원본 보존(15일) 밖은 거절한다(다시 만들 근거가 없다).
//   - revision은 저장된 최대값보다 크다. 같은 window를 live job이 나중에 다시 쓰면 그쪽 revision이 더 크다(시각 기반).
//   - 비어 있는 window는 쓰지 않는다(값 없음을 0으로 만들지 않는다, 계약 6).
//   - chunk 단위로 읽고 chunk 사이에 쉰다. live 처리 여유를 다 쓰지 않게 한다(D02 §05 replay 20%).
//   - 이미 발송된 경보는 자동 취소하지 않는다(D02 §07) — 경보 평가가 생기면 그 쪽 규칙이다.

// BackfillStore는 backfill에 필요한 저장소 작업이다(ClickHouseStore).
type BackfillStore interface {
	LoadProgress(ctx context.Context, since time.Time) (Progress, error)
	ReadTenantPoints(ctx context.Context, tenant string, from, to time.Time) ([]RawPoint, error)
	WriteWindows(ctx context.Context, rows []Row) error
}

// BackfillRequest는 backfill 범위다. 시간은 [From, To)이고 window 경계로 넓혀 맞춘다.
type BackfillRequest struct {
	Tenant   string
	From, To time.Time
}

// BackfillOptions는 실행 제한이다. 0이면 기본값.
type BackfillOptions struct {
	// RawRetention은 원본 보존 기간이다(기본 15일, D04 §04). From이 now − RawRetention보다 이르면 거절한다.
	RawRetention time.Duration
	// ChunkWindows는 한 번에 계산하는 window 수다(기본 60: 1분이면 1시간, 1시간이면 60시간).
	ChunkWindows int
	// Pause는 chunk 사이 쉬는 시간이다(기본 200ms).
	Pause time.Duration
	// JobID는 로그·결과에 남길 식별자다.
	JobID  string
	Logger *slog.Logger
	Now    func() time.Time
}

// BackfillResult는 실행 결과다.
type BackfillResult struct {
	From, To time.Time // 실제 계산 범위(window 경계)
	Chunks   int
	Points   int
	Windows  int // 값이 있어 쓴 (stream, window) 수
	Revision uint64
}

// ErrBackfillRange는 요청 범위가 허용 밖이라는 뜻이다.
var ErrBackfillRange = errors.New("rollup: backfill range not allowed")

// Backfill은 cfg(live job과 같은 Window·BaselineLookback·Retention)로 req 범위를 다시 계산해 쓴다.
func Backfill(ctx context.Context, store BackfillStore, cfg Config, req BackfillRequest, opt BackfillOptions) (BackfillResult, error) {
	if _, err := authz.ParseTenantID(req.Tenant); err != nil {
		return BackfillResult{}, fmt.Errorf("%w: tenant must be a UUID", ErrBackfillRange)
	}
	if opt.RawRetention <= 0 {
		opt.RawRetention = 15 * 24 * time.Hour
	}
	if opt.ChunkWindows <= 0 {
		opt.ChunkWindows = 60
	}
	if opt.Pause < 0 {
		opt.Pause = 0
	} else if opt.Pause == 0 {
		opt.Pause = 200 * time.Millisecond
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	j := New(cfg) // 기본값 채움 (Window, BaselineLookback, Retention)
	w := j.cfg.Window
	now := opt.Now().UTC()

	from := req.From.UTC().Truncate(w)
	to := req.To.UTC()
	if t := to.Truncate(w); t.Before(to) {
		to = t.Add(w)
	}
	switch {
	case !from.Before(to):
		return BackfillResult{}, fmt.Errorf("%w: from must be before to", ErrBackfillRange)
	case to.After(now.Truncate(w)):
		// 아직 닫히지 않은 window는 live job 몫이다(쓰다 만 window를 확정값처럼 만들지 않는다).
		return BackfillResult{}, fmt.Errorf("%w: to must not be after the current %s window start", ErrBackfillRange, w)
	case from.Before(now.Add(-opt.RawRetention)):
		return BackfillResult{}, fmt.Errorf("%w: from is older than raw retention (%s)", ErrBackfillRange, opt.RawRetention)
	}

	progress, err := store.LoadProgress(ctx, from)
	if err != nil {
		return BackfillResult{}, fmt.Errorf("rollup: backfill load progress: %w", err)
	}
	revision := max(uint64(now.UnixNano()), progress.MaxRevision+1) //nolint:gosec // 1970 이후 시각
	res := BackfillResult{From: from, To: to, Revision: revision}
	chunk := time.Duration(opt.ChunkWindows) * w
	log := opt.Logger.With(slog.String("job_id", opt.JobID), slog.String("tenant_id", req.Tenant), slog.String("resolution", w.String()))
	log.Info("metric backfill started", slog.Time("from", from), slog.Time("to", to))

	for cs := from; cs.Before(to); cs = cs.Add(chunk) {
		ce := cs.Add(chunk)
		if ce.After(to) {
			ce = to
		}
		points, err := store.ReadTenantPoints(ctx, req.Tenant, cs.Add(-j.cfg.BaselineLookback), ce)
		if err != nil {
			return res, fmt.Errorf("rollup: backfill read %s: %w", cs.Format(time.RFC3339), err)
		}
		// 모든 chunk가 같은 revision을 쓴다. chunk 경계가 겹치지 않아 한 window를 두 번 쓰지 않는다.
		rows := j.compute(points, map[string]span{req.Tenant: {from: cs, to: ce}}, revision, now)
		if len(rows) > 0 {
			if err := store.WriteWindows(ctx, rows); err != nil {
				return res, fmt.Errorf("rollup: backfill write %s: %w", cs.Format(time.RFC3339), err)
			}
		}
		res.Chunks++
		res.Points += len(points)
		res.Windows += len(rows)
		if ce.Before(to) && opt.Pause > 0 {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(opt.Pause):
			}
		}
	}
	log.Info("metric backfill finished", slog.Int("chunks", res.Chunks), slog.Int("points", res.Points), slog.Int("windows", res.Windows))
	return res, nil
}
