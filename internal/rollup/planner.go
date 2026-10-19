package rollup

import (
	"log/slog"
	"time"
)

// Range는 tenant 하나의 이번 계산 범위 [From, To)다. 둘 다 window 경계다.
type Range struct{ From, To time.Time }

// PlannerConfig는 Planner 설정이다. 0이면 기본값(패키지 문서의 수식).
type PlannerConfig struct {
	Window      time.Duration // 기본 1분
	MaxLateness time.Duration // 기본 2분 (D02 §07·§21)
	IdleAfter   time.Duration // 기본 60초 (D02 §21)
	Recompute   time.Duration // 기본 10분 (D02 §07)
	MaxCatchUp  time.Duration // 기본 1시간. 넘으면 gap
	Logger      *slog.Logger
	// Subject는 gap 경고에 붙는 job 이름이다(예: "metric rollup").
	Subject string
}

// Planner는 tenant별 watermark와 이번 주기의 계산 범위를 정한다(패키지 문서의 수식).
// metric rollup(Job)과 service map edge rollup(internal/servicemap)이 같이 쓴다.
// 처리 완료 위치는 Done으로 알려야 다음 주기가 이어서 계산한다.
type Planner struct {
	cfg     PlannerConfig
	tenants map[string]*tenantState
}

// tenantState는 tenant별 watermark·진행 상태다.
type tenantState struct {
	maxObserved, advancedAt time.Time
	done                    time.Time // 쓰기를 마친 window의 끝
}

// NewPlanner는 Planner를 만든다.
func NewPlanner(cfg PlannerConfig) *Planner {
	if cfg.Window <= 0 {
		cfg.Window = time.Minute
	}
	if cfg.MaxLateness <= 0 {
		cfg.MaxLateness = 2 * time.Minute
	}
	if cfg.IdleAfter <= 0 {
		cfg.IdleAfter = time.Minute
	}
	if cfg.Recompute <= 0 {
		cfg.Recompute = 10 * time.Minute
	}
	if cfg.MaxCatchUp < cfg.Recompute {
		cfg.MaxCatchUp = time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Subject == "" {
		cfg.Subject = "rollup"
	}
	return &Planner{cfg: cfg, tenants: map[string]*tenantState{}}
}

// Restore는 저장소에서 읽은 tenant별 처리 완료 위치를 넣는다(재시작 시).
func (p *Planner) Restore(done map[string]time.Time) {
	for tenant, d := range done {
		p.state(tenant).done = d
	}
}

// Plan은 tenant별 최대 관측 시각으로 이번 계산 범위를 정한다. 따라잡기 상한을 넘어 건너뛴 tenant 수를 함께 돌려준다.
func (p *Planner) Plan(maxObs map[string]time.Time, now time.Time) (map[string]Range, int) {
	ranges := map[string]Range{}
	gaps := 0
	for tenant, obs := range maxObs {
		st := p.state(tenant)
		wm := p.watermark(st, obs, now)
		from := wm.Add(-p.cfg.Recompute)
		if !st.done.IsZero() && st.done.Before(from) {
			from = st.done // 이전에 처리하지 못한 window부터 (watermark가 뛰어도 빠뜨리지 않는다)
		}
		if limit := wm.Add(-p.cfg.MaxCatchUp); from.Before(limit) {
			gaps++
			p.cfg.Logger.Warn(p.cfg.Subject+" gap beyond catch-up limit; backfill required",
				slog.String("tenant_id", tenant), slog.Time("from", from), slog.Time("resume", limit))
			from = limit
		}
		if !from.Before(wm) {
			continue
		}
		ranges[tenant] = Range{from, wm}
	}
	return ranges, gaps
}

// Done은 ranges를 끝까지 썼다고 기록한다.
func (p *Planner) Done(ranges map[string]Range) {
	for tenant, r := range ranges {
		p.state(tenant).done = r.To
	}
}

func (p *Planner) state(tenant string) *tenantState {
	st, ok := p.tenants[tenant]
	if !ok {
		st = &tenantState{}
		p.tenants[tenant] = st
	}
	return st
}

// watermark는 tenant 하나의 watermark다 (D02 §07, §21).
// 수집이 밀려 관측 시각이 늦으면 window를 늦게 닫는다. 입력이 IdleAfter 넘게 멈추면 처리 시계로 닫는다.
// 처리 시계보다 앞서지는 않는다(미래 시각 point가 window를 미리 닫지 못하게).
func (p *Planner) watermark(st *tenantState, obs, now time.Time) time.Time {
	if obs.After(st.maxObserved) {
		st.maxObserved, st.advancedAt = obs, now
	}
	processing := now.Add(-p.cfg.MaxLateness)
	wm := st.maxObserved.Add(-p.cfg.MaxLateness)
	if now.Sub(st.advancedAt) > p.cfg.IdleAfter {
		wm = processing // idle: 입력이 멈춘 window도 닫는다
	}
	if wm.After(processing) {
		wm = processing
	}
	return wm.Truncate(p.cfg.Window)
}

// Span은 ranges 전체를 덮는 [from, to)다. ranges가 비면 zero 값이다.
func Span(ranges map[string]Range) (from, to time.Time) {
	for _, r := range ranges {
		if from.IsZero() || r.From.Before(from) {
			from = r.From
		}
		if r.To.After(to) {
			to = r.To
		}
	}
	return from, to
}
