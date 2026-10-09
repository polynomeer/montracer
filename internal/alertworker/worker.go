// Package alertworker는 monitor 주기 평가 실행 경로다 (cmd/alert-worker, D02 §17, ADR 0051).
//
// 한 번의 tick: 평가할 차례인 monitor를 찾고(DueMonitors) → slot lease를 잡고 → tenant system principal로 metric을 조회해
// alerting.Evaluate로 상태를 계산하고 → 결과·상태·전이 outbox를 한 트랜잭션에 쓴다. 평가 의미는 internal/alerting(ADR 0050)이다.
package alertworker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/polynomeer/montracer/internal/alerting"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/monitor"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// Subject는 system principal의 주체 이름이고 outbox actor다.
const Subject = controldb.AlertActor

// Store는 평가 저장소다 (controldb.AlertStore).
type Store interface {
	DueMonitors(ctx context.Context, now time.Time, limit int) ([]controldb.DueMonitor, error)
	ClaimEvaluation(ctx context.Context, tenant authz.TenantID, monitorID string, now time.Time, lease time.Duration) (controldb.Claim, bool, error)
	CompleteEvaluation(ctx context.Context, c controldb.Claim, worker string, r controldb.Completion) error
}

// Metrics는 metric 조회다 (telemetrystore.QueryStore, query 계정·row policy).
type Metrics interface {
	MetricBuckets(ctx context.Context, p authz.Principal, q telemetrystore.MetricQuery, now time.Time) ([]telemetrystore.MetricBucket, error)
	RollupWatermark(ctx context.Context, p authz.Principal, window time.Duration, since, now time.Time) (time.Time, error)
}

// Observer는 운영 지표다. nil이면 세지 않는다.
type Observer interface {
	ObserveEvaluation(status string, transitions int, d time.Duration)
	ObserveLeaseLost()
	ObserveError(stage string) // StageScan | StageEvaluate
	ObserveScan(at time.Time)  // 할 일 찾기 성공(heartbeat: 멈추면 경보)
}

// 실패 단계 (운영 지표 label).
const (
	StageScan     = "scan"
	StageEvaluate = "evaluate"
)

// Config는 worker 설정이다.
type Config struct {
	Store   Store
	Metrics Metrics
	// Worker는 lease 소유자 이름이다(인스턴스마다 달라야 한다).
	Worker string
	// Lease는 평가 하나의 lease 길이다. 이보다 오래 걸리면 다른 worker가 가져갈 수 있다(결과는 ErrLeaseLost로 버려진다).
	// QueryTimeout + 2 × WriteTimeout보다 길어야 한다.
	Lease time.Duration
	// Interval은 tick 간격이다(평가 주기 30초보다 짧게).
	Interval time.Duration
	// Batch는 tick 하나에서 볼 monitor 수, Parallel은 동시 평가 수다.
	Batch, Parallel int
	// QueryTimeout은 평가 하나의 ClickHouse 조회(watermark·metric) 상한이다. 넘으면 EVALUATION_ERROR로 기록한다.
	QueryTimeout time.Duration
	// WriteTimeout은 claim·완료 저장 각각의 상한이다. 완료는 조회와 다른 context라 조회가 시간을 다 써도 오류 결과를 쓸 수 있다.
	WriteTimeout time.Duration
	Logger       *slog.Logger
	Observer     Observer
	Now          func() time.Time
}

// Worker는 alert-worker 평가 루프다.
type Worker struct{ cfg Config }

// New는 기본값을 채운다.
func New(cfg Config) (*Worker, error) {
	if cfg.Store == nil || cfg.Metrics == nil || cfg.Worker == "" {
		return nil, errors.New("alertworker: Store, Metrics and Worker are required")
	}
	if cfg.Lease <= 0 {
		cfg.Lease = time.Minute
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 100
	}
	if cfg.Parallel <= 0 {
		cfg.Parallel = 4
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = 15 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 5 * time.Second
	}
	if cfg.Lease <= cfg.QueryTimeout+2*cfg.WriteTimeout {
		return nil, errors.New("alertworker: Lease must exceed QueryTimeout + 2 × WriteTimeout")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Worker{cfg: cfg}, nil
}

// Run은 ctx가 끝날 때까지 tick을 반복한다.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		if _, err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			w.cfg.Logger.Error("alert tick failed", slog.String("error", err.Error()))
			if w.cfg.Observer != nil {
				w.cfg.Observer.ObserveError(StageScan)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick은 평가할 차례인 monitor를 한 번 훑어 평가한다. 평가한 수를 돌려준다.
// monitor 하나의 실패는 다른 monitor 평가를 막지 않는다(로그·지표로 남기고 lease 만료 뒤 다시 시도된다).
func (w *Worker) Tick(ctx context.Context) (int, error) {
	at := w.cfg.Now()
	scanCtx, cancel := context.WithTimeout(ctx, w.cfg.WriteTimeout)
	due, err := w.cfg.Store.DueMonitors(scanCtx, at, w.cfg.Batch)
	cancel()
	if err != nil {
		return 0, err
	}
	if w.cfg.Observer != nil {
		w.cfg.Observer.ObserveScan(at)
	}
	var (
		mu        sync.Mutex
		evaluated int
		wg        sync.WaitGroup
		sem       = make(chan struct{}, w.cfg.Parallel)
	)
	for _, d := range due {
		if ctx.Err() != nil {
			break // 종료 중: 새 평가를 시작하지 않는다(남은 monitor는 다른 worker나 재시작 뒤 평가)
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(d controldb.DueMonitor) {
			defer wg.Done()
			defer func() { <-sem }()
			ok, err := w.evaluate(ctx, d)
			if err != nil {
				w.cfg.Logger.Error("monitor evaluation failed", slog.String("tenant", d.Tenant.String()), slog.String("monitor_id", d.MonitorID), slog.String("error", err.Error()))
				if w.cfg.Observer != nil {
					w.cfg.Observer.ObserveError(StageEvaluate)
				}
				return
			}
			if ok {
				mu.Lock()
				evaluated++
				mu.Unlock()
			}
		}(d)
	}
	wg.Wait()
	return evaluated, nil
}

// evaluate는 monitor 하나를 평가한다. 다른 worker가 이미 잡았으면 false.
func (w *Worker) evaluate(parent context.Context, d controldb.DueMonitor) (bool, error) {
	start := w.cfg.Now()
	claimCtx, cancel := context.WithTimeout(parent, w.cfg.WriteTimeout)
	claim, ok, err := w.cfg.Store.ClaimEvaluation(claimCtx, d.Tenant, d.MonitorID, start, w.cfg.Lease)
	cancel()
	if err != nil || !ok {
		return false, err
	}
	queryCtx, cancel := context.WithTimeout(parent, w.cfg.QueryTimeout)
	completion, transitions, err := w.compute(queryCtx, claim, start)
	cancel()
	if err != nil {
		return false, err
	}
	if parent.Err() != nil {
		// 종료로 조회가 취소된 것은 조회 실패가 아니다. 쓰지 않으면 lease 만료 뒤 다른 worker가 같은 slot을 평가한다
		// (거짓 EVALUATION_ERROR와 전이 event를 만들지 않는다).
		return false, nil
	}
	completion.Duration = w.cfg.Now().Sub(start)
	// 조회가 시간 상한을 다 써도 결과(EVALUATION_ERROR 포함)는 쓴다 — 쓰지 못하면 이전 상태가 그대로 남는다(계약 6)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), w.cfg.WriteTimeout)
	defer cancel()
	err = w.cfg.Store.CompleteEvaluation(writeCtx, claim, w.cfg.Worker, completion)
	if errors.Is(err, controldb.ErrLeaseLost) {
		// lease 만료 뒤 다른 worker가 가져갔거나 정의가 바뀌었다. 이 결과는 낡았으므로 쓰지 않는다.
		if w.cfg.Observer != nil {
			w.cfg.Observer.ObserveLeaseLost()
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if w.cfg.Observer != nil {
		w.cfg.Observer.ObserveEvaluation(completion.Status, transitions, completion.Duration)
	}
	return true, nil
}

// compute는 조회·평가로 완료 기록을 만든다. 조회 실패는 오류가 아니라 EVALUATION_ERROR 결과다(D02 §21).
func (w *Worker) compute(ctx context.Context, claim controldb.Claim, now time.Time) (controldb.Completion, int, error) {
	spec, _, err := monitor.Normalize(claim.Spec)
	if err != nil {
		// 저장된 정규형은 검증을 거쳤다. 여기서 실패하면 코드·schema 불일치다 — 평가 오류로 드러낸다
		c := invalidSpec(claim.Instances)
		return c, countChanged(c), nil
	}
	p, err := authz.NewSystemPrincipal(claim.Tenant, Subject, authz.TelemetryRead)
	if err != nil {
		return controldb.Completion{}, 0, err
	}
	in := alerting.Input{}
	// watermark를 하루 범위에서 찾는다: 하루 넘게 rollup이 없으면 "없음", 그 안에서 10분 넘게 멈췄으면 "멈춤"
	wm, err := w.cfg.Metrics.RollupWatermark(ctx, p, time.Minute, now.Add(-24*time.Hour), now)
	if err != nil {
		w.cfg.Logger.Warn("rollup watermark query failed", slog.String("monitor_id", claim.MonitorID), slog.String("error", err.Error()))
		in.QueryErr = alerting.ErrorQueryFailed
	} else {
		in.Window, in.WindowStatus = alerting.EvaluationWindow(spec, now, wm)
		if in.WindowStatus == alerting.WindowOK {
			buckets, err := w.cfg.Metrics.MetricBuckets(ctx, p, alerting.Query(spec, in.Window), now)
			if err != nil {
				w.cfg.Logger.Warn("monitor query failed", slog.String("monitor_id", claim.MonitorID), slog.String("error", err.Error()))
				in.QueryErr = alerting.ErrorQueryFailed
			} else {
				in.Result = alerting.Compute(spec, buckets)
			}
		}
	}
	prev, labels := fromStored(claim.Instances)
	ev := alerting.Evaluate(spec, prev, labels, in, now)
	episodes := map[string]*string{}
	for _, s := range claim.Instances {
		episodes[s.GroupKey] = s.EpisodeID
	}
	c := completionFor(ev, episodes, in.Window)
	return c, countChanged(c), nil
}

func countChanged(c controldb.Completion) int {
	n := 0
	for _, g := range c.Groups {
		if g.Changed {
			n++
		}
	}
	return n
}

// invalidSpec은 정의를 해석하지 못한 평가의 결과다. 저장된 group은 모두 EVALUATION_ERROR가 된다(이전 상태·값을 그대로 두지 않는다).
// 사건과 위반 시작은 유지하고 복구 연속은 끊는다(조회 실패와 같은 규칙, ADR 0050 §4).
func invalidSpec(ins []controldb.StoredInstance) controldb.Completion {
	const reason = "invalid_spec"
	c := controldb.Completion{Status: alerting.MonitorError, Reason: reason}
	for _, s := range ins {
		next := s
		next.State, next.OKStreak, next.LastValue = string(alerting.StateEvaluationError), 0, nil
		r := reason
		next.LastReason = &r
		c.Groups = append(c.Groups, controldb.GroupWrite{
			Instance: next, From: s.State, To: next.State, EpisodeOut: s.EpisodeID,
			Changed: s.State != next.State,
		})
	}
	return c
}

// completionFor는 평가 결과를 저장 형태로 바꾼다. 사건 id는 열릴 때 만들고 닫힐 때까지 유지한다(알림 중복 억제 key).
func completionFor(ev alerting.MonitorEval, episodes map[string]*string, w alerting.Window) controldb.Completion {
	c := controldb.Completion{Status: ev.Status, Reason: ev.Reason}
	if !w.End.IsZero() {
		start, end := w.Start, w.End
		c.WindowStart, c.WindowEnd = &start, &end
	}
	for _, g := range ev.Groups {
		prevEpisode := episodes[g.Key]
		episode := prevEpisode
		switch {
		case g.Transition.Opened:
			id := newUUID()
			episode = &id
		case !g.Next.EpisodeOpen:
			episode = nil
		}
		out := episode
		if g.Transition.Closed {
			out = prevEpisode
		}
		s := toStored(g, episode)
		gw := controldb.GroupWrite{
			Instance: s, From: string(g.Transition.From), To: string(g.Transition.To),
			Opened: g.Transition.Opened, Closed: g.Transition.Closed, EpisodeOut: out,
			Changed: !g.Transition.Repeated && (g.Transition.From != g.Transition.To || g.Transition.Opened || g.Transition.Closed),
		}
		if c.WindowEnd != nil {
			gw.WindowEnd = c.WindowEnd
		}
		c.Groups = append(c.Groups, gw)
	}
	return c
}

func toStored(g alerting.GroupEval, episode *string) controldb.StoredInstance {
	n := g.Next
	s := controldb.StoredInstance{
		GroupKey: g.Key, Labels: g.Labels, State: string(n.State), EpisodeID: episode,
		ViolationSince: n.ViolationSince, NoDataSince: n.NoDataSince, OKStreak: n.OKStreak, LastWindowEnd: n.LastWindowEnd,
		LastValue: g.Value,
	}
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	if episode != nil && n.EpisodeReason != "" {
		r := n.EpisodeReason
		s.EpisodeReason = &r
	}
	if g.Reason != "" {
		r := g.Reason
		s.LastReason = &r
	}
	return s
}

func fromStored(ins []controldb.StoredInstance) (map[string]alerting.Instance, map[string]map[string]string) {
	prev := make(map[string]alerting.Instance, len(ins))
	labels := make(map[string]map[string]string, len(ins))
	for _, s := range ins {
		in := alerting.Instance{
			State: alerting.State(s.State), EpisodeOpen: s.EpisodeID != nil,
			ViolationSince: s.ViolationSince, NoDataSince: s.NoDataSince, OKStreak: s.OKStreak, LastWindowEnd: s.LastWindowEnd,
		}
		if s.EpisodeReason != nil {
			in.EpisodeReason = *s.EpisodeReason
		}
		prev[s.GroupKey] = in
		labels[s.GroupKey] = s.Labels
	}
	return prev, labels
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
