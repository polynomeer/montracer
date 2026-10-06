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
// live rollup(Job)은 watermark − Recompute(1m은 10분, 1h는 1시간)만 다시 계산한다. 그보다 늦게 온 point, 처음 보는
// tenant의 과거 구간, 따라잡기 상한을 넘은 gap은 live rollup이 다루지 않는다. Backfill이 그 구간을 같은 계산(Job.compute)으로 채운다.
//
//   - 범위는 live 재계산 구간과 겹치지 않는다(to ≤ floor(now − MaxLateness) − Recompute). 겹치면 두 process가 같은
//     window를 서로 다른 revision으로 다투고, 아직 도착하지 않은 point가 빠진 값을 확정값처럼 쓸 수 있다.
//   - 원본(metric_points)의 dedup된 point만 쓴다. 원본 보존 경계 근처는 기준점 lookback이 만료돼 있으므로 여유를 둔다.
//   - revision은 저장된 최대값보다 크다.
//   - 비어 있는 window는 쓰지 않는다(값 없음을 0으로 만들지 않는다, 계약 6).
//   - chunk 단위로 읽고 chunk 사이에 쉰다. chunk당 읽는 point 수에 상한을 둔다(메모리).
//   - 이미 발송된 경보는 자동 취소하지 않는다(D02 §07).

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
	// RawRetention은 원본 보존 기간이다. 호출자가 수집 쪽 단일 원천(pipeline.DefaultRetention.Metrics)을 넘긴다.
	RawRetention time.Duration
	// ChunkWindows는 한 번에 계산하는 window 수다(기본: 1분 window 60개, 1시간 이상 window 1개).
	ChunkWindows int
	// MaxPoints는 chunk 하나에서 읽을 원본 point 상한이다(기본 2,000,000). 넘으면 쓰지 않고 멈춘다.
	MaxPoints int
	// Pause는 chunk 사이 쉬는 시간이다(기본 200ms, 음수면 쉬지 않음).
	Pause time.Duration
	// JobID는 로그·결과에 남길 식별자다.
	JobID  string
	Logger *slog.Logger
	Now    func() time.Time
}

// BackfillPlan은 검증·정규화된 실행 범위다.
type BackfillPlan struct {
	Tenant   string // 정규 UUID 문자열
	From, To time.Time
	Window   time.Duration
}

// BackfillResult는 실행 결과다.
type BackfillResult struct {
	BackfillPlan
	Chunks   int
	Points   int
	Windows  int // 값이 있어 쓴 (stream, window) 수
	Revision uint64
}

var (
	// ErrBackfillRange는 요청 범위가 허용 밖이라는 뜻이다.
	ErrBackfillRange = errors.New("rollup: backfill range not allowed")
	// ErrBackfillTooLarge는 chunk 하나의 원본이 MaxPoints를 넘었다는 뜻이다(범위를 나누거나 chunk를 줄인다).
	ErrBackfillTooLarge = errors.New("rollup: backfill chunk too large")
)

// rawRetentionMargin은 원본 보존 경계에서 두는 여유다(실행 중 만료·기준점 lookback 만료 방지).
const rawRetentionMargin = time.Hour

// PlanBackfill은 req를 cfg(live job 설정) 기준으로 검증하고 window 경계로 맞춘다. 실행 전에 모든 해상도를 확인하는 데 쓴다.
func PlanBackfill(cfg Config, req BackfillRequest, opt BackfillOptions) (BackfillPlan, error) {
	opt = opt.withDefaults(cfg)
	tid, err := authz.ParseTenantID(req.Tenant)
	if err != nil {
		return BackfillPlan{}, fmt.Errorf("%w: tenant must be a UUID", ErrBackfillRange)
	}
	j := New(cfg) // 기본값 채움 (Window, MaxLateness, Recompute, BaselineLookback, Retention)
	w := j.cfg.Window
	now := opt.Now().UTC()

	from := req.From.UTC().Truncate(w)
	to := req.To.UTC()
	if t := to.Truncate(w); t.Before(to) {
		to = t.Add(w)
	}
	// live job이 다시 계산하는 구간 = [floor(now − MaxLateness) − Recompute, ...). backfill은 그 앞까지만.
	liveFrom := now.Add(-j.cfg.MaxLateness).Truncate(w).Add(-j.cfg.Recompute)
	oldest := now.Add(-opt.RawRetention).Add(j.cfg.BaselineLookback + rawRetentionMargin)
	switch {
	case !from.Before(to):
		return BackfillPlan{}, fmt.Errorf("%w: from must be before to", ErrBackfillRange)
	case to.After(liveFrom):
		return BackfillPlan{}, fmt.Errorf("%w: to must be at or before %s (the live rollup recomputes after that)",
			ErrBackfillRange, liveFrom.Format(time.RFC3339))
	case from.Before(oldest):
		return BackfillPlan{}, fmt.Errorf("%w: from must be at or after %s (raw retention %s minus baseline lookback and margin)",
			ErrBackfillRange, oldest.Format(time.RFC3339), opt.RawRetention)
	}
	return BackfillPlan{Tenant: tid.String(), From: from, To: to, Window: w}, nil
}

// LiveRecomputeStart는 live job이 다시 계산하기 시작하는 시각이다(backfill의 to 상한).
func LiveRecomputeStart(cfg Config, now time.Time) time.Time {
	j := New(cfg)
	return now.UTC().Add(-j.cfg.MaxLateness).Truncate(j.cfg.Window).Add(-j.cfg.Recompute)
}

func (o BackfillOptions) withDefaults(cfg Config) BackfillOptions {
	if o.RawRetention <= 0 {
		o.RawRetention = 15 * 24 * time.Hour
	}
	if o.ChunkWindows <= 0 {
		o.ChunkWindows = 60
		if cfg.Window >= time.Hour {
			o.ChunkWindows = 1 // 1시간 window 하나도 원본 2시간(lookback 포함) 분량이다
		}
	}
	if o.MaxPoints <= 0 {
		o.MaxPoints = 2_000_000
	}
	if o.Pause == 0 {
		o.Pause = 200 * time.Millisecond
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Backfill은 cfg(live job과 같은 설정)로 req 범위를 다시 계산해 쓴다.
func Backfill(ctx context.Context, store BackfillStore, cfg Config, req BackfillRequest, opt BackfillOptions) (BackfillResult, error) {
	plan, err := PlanBackfill(cfg, req, opt)
	if err != nil {
		return BackfillResult{}, err
	}
	opt = opt.withDefaults(cfg)
	j := New(cfg)
	now := opt.Now().UTC()

	progress, err := store.LoadProgress(ctx, plan.From)
	if err != nil {
		return BackfillResult{}, fmt.Errorf("rollup: backfill load progress: %w", err)
	}
	revision := max(uint64(now.UnixNano()), progress.MaxRevision+1) //nolint:gosec // 1970 이후 시각
	res := BackfillResult{BackfillPlan: plan, Revision: revision}
	chunk := time.Duration(opt.ChunkWindows) * plan.Window
	log := opt.Logger.With(slog.String("job_id", opt.JobID), slog.String("tenant_id", plan.Tenant), slog.String("resolution", plan.Window.String()))
	log.Info("metric backfill started", slog.Time("from", plan.From), slog.Time("to", plan.To), slog.Uint64("revision", revision))

	for cs := plan.From; cs.Before(plan.To); cs = cs.Add(chunk) {
		ce := cs.Add(chunk)
		if ce.After(plan.To) {
			ce = plan.To
		}
		points, err := store.ReadTenantPoints(ctx, plan.Tenant, cs.Add(-j.cfg.BaselineLookback), ce)
		if err != nil {
			return res, fmt.Errorf("rollup: backfill read %s (done up to %s): %w", cs.Format(time.RFC3339), cs.Format(time.RFC3339), err)
		}
		if len(points) > opt.MaxPoints {
			return res, fmt.Errorf("%w: %d points in chunk %s (max %d, done up to %s) — split the range",
				ErrBackfillTooLarge, len(points), cs.Format(time.RFC3339), opt.MaxPoints, cs.Format(time.RFC3339))
		}
		// 모든 chunk가 같은 revision을 쓴다. chunk 경계가 겹치지 않아 한 window를 두 번 쓰지 않는다.
		rows := j.compute(points, map[string]span{plan.Tenant: {from: cs, to: ce}}, revision, now)
		if len(rows) > 0 {
			if err := store.WriteWindows(ctx, rows); err != nil {
				return res, fmt.Errorf("rollup: backfill write %s (done up to %s): %w", cs.Format(time.RFC3339), cs.Format(time.RFC3339), err)
			}
		}
		res.Chunks++
		res.Points += len(points)
		res.Windows += len(rows)
		// 재시작 지점: 실패하면 마지막 "done_to"부터 다시 돌리면 된다(재실행은 같은 값).
		log.Info("metric backfill chunk", slog.Time("done_to", ce), slog.Int("points", len(points)), slog.Int("windows", len(rows)))
		if ce.Before(plan.To) && opt.Pause > 0 {
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
