package servicemap

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/polynomeer/montracer/internal/rollup"
)

// Progress는 저장된 진행 상태다(재시작 시 복원).
type Progress struct {
	Done        map[string]time.Time // tenant별 이미 쓴 가장 늦은 window의 끝
	MaxRevision uint64
}

// Store는 edge rollup 저장소다(ClickHouse service map 계정).
type Store interface {
	// MaxObserved는 event_time ∈ [since, until]인 span의 tenant별 최대 시각이다.
	MaxObserved(ctx context.Context, since, until time.Time) (map[string]time.Time, error)
	LoadProgress(ctx context.Context, since time.Time) (Progress, error)
	// ReadPairs는 기준 span(client·producer, fallback은 server·consumer)의 시작 시각이 [from, to)인 pair다.
	ReadPairs(ctx context.Context, from, to time.Time) ([]Pair, error)
	// WriteWindows는 행을 쓴다. 같은 rows를 다시 쓰면 무시된다(내용 기반 token).
	WriteWindows(ctx context.Context, rows []Row) error
}

// CycleResult는 주기 하나의 결과다.
type CycleResult struct {
	OK       bool
	Pairs    int
	Dropped  int // 상대를 알 수 없거나 자기 호출이라 버린 pair
	Windows  int // 계산한 (tenant, window) 중 edge가 있는 수
	Written  int // 내용이 바뀌어 쓴 window 수
	Rows     int // 쓴 행 수
	Gaps     int
	Duration time.Duration
}

// Observer는 주기 결과를 운영 지표로 내보낸다.
type Observer interface {
	ObserveCycle(r CycleResult)
}

// Config는 Job 설정이다. 0이면 기본값.
type Config struct {
	Store    Store
	Logger   *slog.Logger
	Observer Observer
	Now      func() time.Time
	// Recompute는 늦은 span을 반영해 다시 계산하는 범위다(기본 10분, D02 §07과 같은 규칙).
	Recompute time.Duration
	// MaxCatchUp은 따라잡는 최대 범위다(기본 1시간).
	MaxCatchUp time.Duration
	// Chunk는 저장소 한 번에 읽는 범위다(기본 10분). span join의 메모리를 묶는다.
	Chunk time.Duration
	// Interval은 주기다(기본 30초).
	Interval time.Duration
	// Retention은 service_edges_1m 보존 기간이다(기본 30일, D02 §10).
	Retention time.Duration
}

// Job은 edge rollup 주기 실행기다. 한 cluster에서 하나만 돌린다(metric rollup과 같은 규칙, ADR 0026 §4).
type Job struct {
	cfg          Config
	planner      *rollup.Planner
	last         map[windowKey][32]byte // 마지막으로 쓴 window 내용 해시
	lastRevision uint64
	loaded       bool
}

type windowKey struct {
	tenant string
	start  int64
}

// New는 Job을 만든다.
func New(cfg Config) *Job {
	if cfg.Recompute <= 0 {
		cfg.Recompute = 10 * time.Minute
	}
	if cfg.MaxCatchUp < cfg.Recompute {
		cfg.MaxCatchUp = time.Hour
	}
	if cfg.Chunk <= 0 {
		cfg.Chunk = 10 * time.Minute
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 30 * 24 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	planner := rollup.NewPlanner(rollup.PlannerConfig{Recompute: cfg.Recompute, MaxCatchUp: cfg.MaxCatchUp,
		Logger: cfg.Logger, Subject: "service map edge rollup"})
	return &Job{cfg: cfg, planner: planner, last: map[windowKey][32]byte{}}
}

// Run은 ctx가 끝날 때까지 주기를 돈다. 주기 실패는 기록하고 다음 주기에 다시 계산한다(멱등).
func (j *Job) Run(ctx context.Context) {
	t := time.NewTicker(j.cfg.Interval)
	defer t.Stop()
	for {
		if err := j.Cycle(ctx); err != nil && ctx.Err() == nil {
			j.cfg.Logger.Warn("service map edge rollup cycle failed", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Cycle은 주기 하나를 실행한다.
//
// window 하나는 통째로 같은 revision으로 쓴다. 늦게 온 server span이 짝을 찾으면 같은 호출이 external에서
// service edge로 옮겨 가므로 key가 바뀐다 — 조회는 (tenant, window)마다 최대 revision의 행만 읽는다.
func (j *Job) Cycle(ctx context.Context) error {
	start := time.Now()
	var res CycleResult
	defer func() {
		res.Duration = time.Since(start)
		if j.cfg.Observer != nil {
			j.cfg.Observer.ObserveCycle(res)
		}
	}()
	now := j.cfg.Now().UTC()
	if !j.loaded {
		p, err := j.cfg.Store.LoadProgress(ctx, now.Add(-j.cfg.MaxCatchUp-j.cfg.Recompute))
		if err != nil {
			return fmt.Errorf("servicemap: load progress: %w", err)
		}
		j.planner.Restore(p.Done)
		j.lastRevision, j.loaded = p.MaxRevision, true
	}
	maxObs, err := j.cfg.Store.MaxObserved(ctx, now.Add(-time.Hour), now)
	if err != nil {
		return fmt.Errorf("servicemap: max observed: %w", err)
	}
	ranges, gaps := j.planner.Plan(maxObs, now)
	res.Gaps = gaps
	if len(ranges) == 0 {
		res.OK = true
		return nil
	}
	readFrom, readTo := rollup.Span(ranges)
	var pairs []Pair
	for f := readFrom; f.Before(readTo); f = f.Add(j.cfg.Chunk) {
		t := f.Add(j.cfg.Chunk)
		if t.After(readTo) {
			t = readTo
		}
		ps, err := j.cfg.Store.ReadPairs(ctx, f, t)
		if err != nil {
			return fmt.Errorf("servicemap: read pairs: %w", err)
		}
		for _, p := range ps {
			// tenant마다 자기 범위만 (다른 tenant의 범위로 읽힌 pair는 그 tenant 범위가 아니면 버린다)
			if r, ok := ranges[p.Tenant]; ok && !p.WindowStart.Before(r.From) && p.WindowStart.Before(r.To) {
				pairs = append(pairs, p)
			}
		}
	}
	res.Pairs = len(pairs)
	rows, dropped := Build(pairs)
	res.Dropped = dropped
	revision := max(uint64(now.UnixNano()), j.lastRevision+1) //nolint:gosec // 1970 이후 시각

	var changed []Row
	sums := map[windowKey][32]byte{}
	for i := 0; i < len(rows); {
		k := windowKey{rows[i].Tenant, rows[i].WindowStart.Unix()}
		end := i
		for end < len(rows) && rows[end].Tenant == k.tenant && rows[end].WindowStart.Unix() == k.start {
			end++
		}
		window := rows[i:end]
		i = end
		res.Windows++
		h := windowHash(window)
		if j.last[k] == h {
			continue
		}
		sums[k] = h
		for _, r := range window {
			r.Revision, r.ComputedAt, r.ExpiresAt = revision, now, r.WindowStart.Add(j.cfg.Retention)
			changed = append(changed, r)
		}
	}
	if len(changed) > 0 {
		if err := j.cfg.Store.WriteWindows(ctx, changed); err != nil {
			return fmt.Errorf("servicemap: write windows: %w", err)
		}
		j.lastRevision = revision
	}
	for k, h := range sums {
		j.last[k] = h
	}
	j.planner.Done(ranges)
	for k := range j.last {
		if r, ok := ranges[k.tenant]; !ok || k.start < r.To.Add(-j.cfg.Recompute).Unix() {
			delete(j.last, k) // 재계산 범위를 벗어난 window는 더 쓰지 않는다
		}
	}
	res.Written, res.Rows, res.OK = len(sums), len(changed), true
	return nil
}
