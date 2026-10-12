package alertworker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/notify"
)

// 알림 전송 루프 (D02 §17·§20, ADR 0053).
// tick마다 보낼 차례인 알림을 찾고(DueDeliveries) → lease를 잡고 → secret을 풀어 webhook을 서명해 보내고 → 결과를 쓴다.
// 전송은 at-least-once다: 보낸 뒤 결과를 쓰기 전에 죽으면 lease 만료 뒤 다시 보낸다(같은 event_id, receiver가 거른다).

// DeliveryStore는 알림 저장소다 (controldb.DeliveryStore).
type DeliveryStore interface {
	DueDeliveries(ctx context.Context, now time.Time, limit, perPolicy int) ([]controldb.DueDelivery, time.Time, error)
	ClaimDelivery(ctx context.Context, tenant authz.TenantID, id string, now time.Time, lease time.Duration) (controldb.Delivery, bool, error)
	CompleteDelivery(ctx context.Context, d controldb.Delivery, r controldb.DeliveryResult) error
	PruneFinished(ctx context.Context, now time.Time, after string, maxTenants int) (int64, string, error)
}

// 전송 루프 실패 단계 (운영 지표 label).
const (
	NotifyStageScan    = "scan"    // 보낼 알림 찾기
	NotifyStageDeliver = "deliver" // lease 잡기·결과 저장
	NotifyStagePrune   = "prune"   // 보존 기간 정리
)

// Sender는 webhook 전송이다 (notify.Client).
type Sender interface {
	Deliver(ctx context.Context, rawURL string, sec notify.Secret, eventID string, body []byte, attempt int, now time.Time) notify.Result
}

// NotifyObserver는 전송 지표다. nil이면 세지 않는다.
type NotifyObserver interface {
	// ObserveDelivery는 결과(delivered·retry·failed·canceled)와 전송 시간(보내지 않았으면 0)을 센다.
	ObserveDelivery(outcome string, d time.Duration)
	// ObserveBacklog는 한 번도 시도하지 않은 알림 중 가장 오래된 것의 나이다(0이면 없음).
	ObserveBacklog(age time.Duration)
	ObserveNotifyError(stage string)
}

// NotifierConfig는 전송 루프 설정이다.
type NotifierConfig struct {
	Store DeliveryStore
	// Sealer·Sender가 없으면(master key 없음) 보내지 않고 backlog 지표만 낸다 — 알림이 쌓이는 것이 경보로 드러난다.
	Sealer   *notify.Sealer
	Sender   Sender
	Interval time.Duration // 기본 2초
	Batch    int           // 기본 100
	Parallel int           // 기본 8
	// Lease는 알림 하나의 lease다. 전송 상한(10초)과 저장보다 길어야 한다. 기본 1분
	Lease time.Duration
	// PerPolicy는 tick 하나에서 한 policy로 보내는 알림 수다(기본 5). 한 policy의 알림은 차례로 보낸다 —
	// 응답 없는 endpoint 하나가 동시 전송 자리를 모두 잡아 다른 tenant의 알림을 막지 않게 한다.
	PerPolicy int
	// PruneEvery는 보존 기간 정리 간격이다(기본 10분).
	PruneEvery time.Duration
	Logger     *slog.Logger
	Observer   NotifyObserver
	Now        func() time.Time
}

// Notifier는 알림 전송 루프다.
type Notifier struct {
	cfg         NotifierConfig
	lastPrune   time.Time
	pruneCursor string
}

// NewNotifier는 기본값을 채운다.
func NewNotifier(cfg NotifierConfig) (*Notifier, error) {
	if cfg.Store == nil || (cfg.Sealer == nil) != (cfg.Sender == nil) {
		return nil, errors.New("alertworker: notifier needs Store, and Sealer with Sender (or neither: observe only)")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 100
	}
	if cfg.Parallel <= 0 {
		cfg.Parallel = 8
	}
	if cfg.Lease <= 0 {
		cfg.Lease = time.Minute
	}
	if cfg.PerPolicy <= 0 {
		cfg.PerPolicy = 5
	}
	if cfg.PruneEvery <= 0 {
		cfg.PruneEvery = 10 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Notifier{cfg: cfg}, nil
}

// Run은 ctx가 끝날 때까지 tick을 반복한다.
func (n *Notifier) Run(ctx context.Context) {
	t := time.NewTicker(n.cfg.Interval)
	defer t.Stop()
	for {
		if _, err := n.Tick(ctx); err != nil && ctx.Err() == nil {
			n.cfg.Logger.Error("notification tick failed", slog.String("error", err.Error()))
			if n.cfg.Observer != nil {
				n.cfg.Observer.ObserveNotifyError(NotifyStageScan)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick은 보낼 차례인 알림을 한 번 훑어 보낸다. 끝낸(결과를 쓴) 수를 돌려준다.
func (n *Notifier) Tick(ctx context.Context) (int, error) {
	now := n.cfg.Now()
	due, oldest, err := n.cfg.Store.DueDeliveries(ctx, now, n.cfg.Batch, n.cfg.PerPolicy)
	if err != nil {
		return 0, err
	}
	if n.cfg.Observer != nil {
		var age time.Duration
		if !oldest.IsZero() {
			age = max(now.Sub(oldest), 0)
		}
		n.cfg.Observer.ObserveBacklog(age)
	}
	if n.cfg.Sealer == nil {
		return 0, nil // 보내지 않는다(master key 없음): backlog 지표만
	}
	n.prune(ctx, now)
	// policy별 차례 목록(한 policy는 PerPolicy개까지, 차례로). policy끼리는 동시에 보낸다
	var (
		order  []string
		queues = map[string][]controldb.DueDelivery{}
	)
	for _, d := range due {
		q := queues[d.PolicyID]
		if len(q) >= n.cfg.PerPolicy {
			continue // 다음 tick에
		}
		if len(q) == 0 {
			order = append(order, d.PolicyID)
		}
		queues[d.PolicyID] = append(q, d)
	}
	var (
		mu   sync.Mutex
		done int
		wg   sync.WaitGroup
		sem  = make(chan struct{}, n.cfg.Parallel)
	)
	for _, policy := range order {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(q []controldb.DueDelivery) {
			defer wg.Done()
			defer func() { <-sem }()
			for _, d := range q {
				if ctx.Err() != nil {
					return
				}
				ok, err := n.deliver(ctx, d)
				if err != nil {
					n.cfg.Logger.Error("notification delivery failed", slog.String("tenant", d.Tenant.String()), slog.String("delivery_id", d.ID), slog.String("error", err.Error()))
					if n.cfg.Observer != nil {
						n.cfg.Observer.ObserveNotifyError(NotifyStageDeliver)
					}
					continue
				}
				if ok {
					mu.Lock()
					done++
					mu.Unlock()
				}
			}
		}(queues[policy])
	}
	wg.Wait()
	return done, nil
}

// prune은 PruneEvery마다 보존 기간이 지난 알림을 지운다(알림이 끊긴 tenant도 지워지게 tick에서 따로 돈다).
func (n *Notifier) prune(ctx context.Context, now time.Time) {
	if now.Sub(n.lastPrune) < n.cfg.PruneEvery {
		return
	}
	n.lastPrune = now
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, next, err := n.cfg.Store.PruneFinished(pctx, now, n.pruneCursor, 100)
	n.pruneCursor = next
	if err != nil {
		n.cfg.Logger.Error("notification prune failed", slog.String("error", err.Error()))
		if n.cfg.Observer != nil {
			n.cfg.Observer.ObserveNotifyError(NotifyStagePrune)
		}
	}
}

// deliver는 알림 하나를 보낸다. 다른 worker가 잡았으면 false.
func (n *Notifier) deliver(parent context.Context, due controldb.DueDelivery) (bool, error) {
	claimCtx, cancel := context.WithTimeout(parent, 5*time.Second)
	d, ok, err := n.cfg.Store.ClaimDelivery(claimCtx, due.Tenant, due.ID, n.cfg.Now(), n.cfg.Lease)
	cancel()
	if err != nil || !ok {
		return false, err
	}
	var (
		res     controldb.DeliveryResult
		outcome string
		took    time.Duration
	)
	sec, secErr := n.cfg.Sealer.Open(d.Tenant.String(), d.PolicyID, d.SecretKeyID, d.SealingKey, d.Sealed)
	url, urlErr := n.cfg.Sealer.OpenURL(d.Tenant.String(), d.PolicyID, d.URLSealingKey, d.URLSealed)
	switch {
	case d.PolicyDisabled:
		// policy가 꺼졌거나 지워졌다: 보내지 않는다
		res, outcome = controldb.DeliveryResult{Status: "canceled", Error: "policy_disabled"}, "canceled"
	case secErr != nil || urlErr != nil:
		// 모르는 master key 버전이거나 손상: 다시 해도 같다(회전 절차는 RB04). secret·URL 내용은 로그에 남기지 않는다
		res, outcome = controldb.DeliveryResult{Status: "failed", Error: "secret_unavailable"}, "failed"
	default:
		start := n.cfg.Now()
		r := n.cfg.Sender.Deliver(parent, url, sec, d.ID, d.Body, d.Attempts, start)
		took = max(n.cfg.Now().Sub(start), time.Nanosecond)
		res, outcome = n.result(d, r)
	}
	if parent.Err() != nil && outcome != "delivered" {
		// 종료로 끊긴 전송: 결과를 쓰지 않고 lease 만료 뒤 다시 보낸다(24시간 창의 마지막 시도라도 종료를 포기로 기록하지 않는다)
		return false, nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	if err := n.cfg.Store.CompleteDelivery(writeCtx, d, res); err != nil {
		if errors.Is(err, controldb.ErrLeaseLost) {
			return false, nil // 다른 worker가 가져갔다(이미 보냈을 수 있다 — at-least-once)
		}
		return false, err
	}
	if n.cfg.Observer != nil {
		n.cfg.Observer.ObserveDelivery(outcome, took)
	}
	return true, nil
}

// result는 전송 결과를 저장 형태로 바꾼다: 2xx는 delivered, 영구 실패는 failed, 그 밖은 지수 backoff로 재시도(24시간까지).
func (n *Notifier) result(d controldb.Delivery, r notify.Result) (controldb.DeliveryResult, string) {
	out := controldb.DeliveryResult{StatusCode: r.StatusCode, Error: r.ErrCode}
	switch {
	case r.ErrCode == "":
		out.Status = "delivered"
		return out, "delivered"
	case r.Permanent:
		out.Status = "failed"
		return out, "failed"
	}
	next, ok := notify.NextAttempt(d.CreatedAt, n.cfg.Now(), d.Attempts)
	if !ok {
		out.Status = "failed" // 24시간 동안 실패: 포기(DLQ, 경보)
		return out, "failed"
	}
	out.Status, out.NextAttemptAt = "pending", next
	return out, "retry"
}
