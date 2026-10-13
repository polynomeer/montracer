package alertworker

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/notify"
)

// fakeDeliveries는 알림 하나의 lease·결과를 메모리로 흉내 낸다.
type fakeDeliveries struct {
	mu        sync.Mutex
	d         controldb.Delivery
	claimed   bool
	results   []controldb.DeliveryResult
	loseLease bool
	prunes    int
}

func (f *fakeDeliveries) DueDeliveries(context.Context, time.Time, int, int) ([]controldb.DueDelivery, time.Time, error) {
	return []controldb.DueDelivery{{Tenant: f.d.Tenant, ID: f.d.ID}}, f.d.CreatedAt, nil
}

func (f *fakeDeliveries) ClaimDelivery(context.Context, authz.TenantID, string, time.Time, time.Duration) (controldb.Delivery, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimed {
		return controldb.Delivery{}, false, nil
	}
	return f.d, true, nil
}

func (f *fakeDeliveries) CompleteDelivery(ctx context.Context, _ controldb.Delivery, r controldb.DeliveryResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.loseLease {
		return controldb.ErrLeaseLost
	}
	f.results = append(f.results, r)
	return nil
}

func (f *fakeDeliveries) PruneFinished(context.Context, time.Time, string, int) (int64, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prunes++
	return 0, "", nil
}

type fakeSender struct {
	result notify.Result
	calls  []struct {
		url, eventID string
		key          []byte
		keyIDs       []string
		attempt      int
	}
	block bool
}

func (s *fakeSender) Deliver(ctx context.Context, rawURL string, keys []notify.Secret, eventID string, _ []byte, attempt int, _ time.Time) notify.Result {
	ids := make([]string, len(keys))
	for i, k := range keys {
		ids[i] = k.KeyID
	}
	s.calls = append(s.calls, struct {
		url, eventID string
		key          []byte
		keyIDs       []string
		attempt      int
	}{rawURL, eventID, keys[0].Key, ids, attempt})
	if s.block {
		<-ctx.Done()
		return notify.Result{ErrCode: notify.ErrCodeTimeout}
	}
	return s.result
}

type notifyCounter struct {
	outcomes map[string]int
	backlog  time.Duration
	errs     int
}

func (c *notifyCounter) ObserveDelivery(o string, _ time.Duration) { c.outcomes[o]++ }
func (c *notifyCounter) ObserveBacklog(a time.Duration)            { c.backlog = a }
func (c *notifyCounter) ObserveNotifyError(string)                 { c.errs++ }

func notifierFixture(t *testing.T, attempts int, created time.Time, result notify.Result) (*fakeDeliveries, *fakeSender, *notifyCounter, *Notifier, notify.Secret) {
	t.Helper()
	sealer, err := notify.NewSealer("v1", bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := notify.NewSecret()
	sealed, err := sealer.Seal(tenant.String(), "p1", sec)
	if err != nil {
		t.Fatal(err)
	}
	url, err := sealer.SealURL(tenant.String(), "p1", "https://hooks.example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeDeliveries{d: controldb.Delivery{Tenant: tenant, ID: "d1", Token: "tok", PolicyID: "p1", Body: []byte(`{}`), Attempts: attempts,
		CreatedAt: created, URLSealed: url, URLSealingKey: "v1", SecretKeyID: sec.KeyID, Sealed: sealed, SealingKey: "v1"}}
	sender := &fakeSender{result: result}
	obs := &notifyCounter{outcomes: map[string]int{}}
	n, err := NewNotifier(NotifierConfig{Store: store, Sealer: sealer, Sender: sender, Observer: obs, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	return store, sender, obs, n, sec
}

// 2xx: delivered. 풀린 URL·secret으로 서명해 event id = 알림 id, 시도 번호 = claim이 늘린 시도 수로 보낸다
func TestNotifierDelivered(t *testing.T) {
	store, sender, obs, n, sec := notifierFixture(t, 3, t0.Add(-time.Minute), notify.Result{StatusCode: 204})
	if done, err := n.Tick(context.Background()); err != nil || done != 1 {
		t.Fatalf("tick: %d %v", done, err)
	}
	c := sender.calls[0]
	if c.url != "https://hooks.example.com/x" || c.eventID != "d1" || !bytes.Equal(c.key, sec.Key) || c.attempt != 3 {
		t.Errorf("call = %+v", c)
	}
	if r := store.results[0]; r.Status != "delivered" || r.StatusCode != 204 || obs.outcomes["delivered"] != 1 || obs.backlog != time.Minute {
		t.Errorf("result = %+v obs = %+v", r, obs)
	}
}

// 일시 실패: 지수 backoff로 다시 시도. 24시간을 넘기면 포기(failed = DLQ). 영구 실패(사설 주소)는 바로 failed.
func TestNotifierRetryAndGiveUp(t *testing.T) {
	store, _, obs, n, _ := notifierFixture(t, 1, t0, notify.Result{StatusCode: 503, ErrCode: notify.ErrCodeStatus})
	if _, err := n.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := store.results[0]; r.Status != "pending" || !r.NextAttemptAt.Equal(t0.Add(notify.FirstRetry)) || r.Error != notify.ErrCodeStatus || obs.outcomes["retry"] != 1 {
		t.Errorf("retry = %+v", r)
	}
	store, _, obs, n, _ = notifierFixture(t, 30, t0.Add(-23*time.Hour-30*time.Minute), notify.Result{ErrCode: notify.ErrCodeConnect})
	if _, err := n.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := store.results[0]; r.Status != "failed" || obs.outcomes["failed"] != 1 {
		t.Errorf("give up = %+v", r)
	}
	store, _, _, n, _ = notifierFixture(t, 1, t0, notify.Result{ErrCode: notify.ErrCodeBlocked, Permanent: true})
	if _, err := n.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := store.results[0]; r.Status != "failed" || r.Error != notify.ErrCodeBlocked {
		t.Errorf("blocked = %+v", r)
	}
}

// 꺼진·지운 policy는 보내지 않고 취소(시도 수에 넣지 않음). 풀 수 없는 secret은 보내지 않고 실패.
func TestNotifierCanceledAndSecretUnavailable(t *testing.T) {
	store, sender, obs, n, _ := notifierFixture(t, 0, t0, notify.Result{StatusCode: 200})
	store.d.PolicyDisabled = true
	if _, err := n.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := store.results[0]; r.Status != "canceled" || len(sender.calls) != 0 || obs.outcomes["canceled"] != 1 {
		t.Errorf("canceled = %+v calls=%d", r, len(sender.calls))
	}
	store, sender, _, n, _ = notifierFixture(t, 0, t0, notify.Result{StatusCode: 200})
	store.d.SealingKey = "v2" // 다른 master key 버전
	if _, err := n.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := store.results[0]; r.Status != "failed" || r.Error != "secret_unavailable" || len(sender.calls) != 0 {
		t.Errorf("secret unavailable = %+v", r)
	}
}

// 다른 worker가 잡았으면 보내지 않는다. lease를 잃은 결과는 쓰지 않는다. 종료로 끊긴 전송은 결과를 쓰지 않는다(lease 만료 뒤 다시).
func TestNotifierLeaseAndShutdown(t *testing.T) {
	store, sender, _, n, _ := notifierFixture(t, 0, t0, notify.Result{StatusCode: 200})
	store.claimed = true
	if done, _ := n.Tick(context.Background()); done != 0 || len(sender.calls) != 0 {
		t.Errorf("claimed elsewhere: done=%d calls=%d", done, len(sender.calls))
	}
	store.claimed, store.loseLease = false, true
	if done, err := n.Tick(context.Background()); done != 0 || err != nil || len(store.results) != 0 {
		t.Errorf("lease lost: done=%d err=%v", done, err)
	}
	store, sender, obs, n, _ := notifierFixture(t, 0, t0, notify.Result{})
	sender.block = true
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	if done, err := n.Tick(ctx); done != 0 || err != nil || len(store.results) != 0 || obs.errs != 0 {
		t.Errorf("shutdown: done=%d err=%v results=%+v", done, err, store.results)
	}
}

// 24시간 창의 마지막 시도가 종료로 끊겨도 포기(DLQ)로 기록하지 않는다
func TestNotifierShutdownOnLastAttempt(t *testing.T) {
	store, sender, _, n, _ := notifierFixture(t, 30, t0.Add(-23*time.Hour-50*time.Minute), notify.Result{})
	sender.block = true
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	if _, err := n.Tick(ctx); err != nil || len(store.results) != 0 {
		t.Errorf("shutdown on last attempt recorded %+v (%v)", store.results, err)
	}
}

// master key가 없으면 보내지 않고 backlog만 낸다(쌓이는 알림이 경보로 드러난다)
func TestNotifierObserveOnly(t *testing.T) {
	store := &fakeDeliveries{d: controldb.Delivery{Tenant: tenant, ID: "d1", CreatedAt: t0.Add(-10 * time.Minute)}}
	obs := &notifyCounter{outcomes: map[string]int{}}
	n, err := NewNotifier(NotifierConfig{Store: store, Observer: obs, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	if done, err := n.Tick(context.Background()); err != nil || done != 0 || obs.backlog != 10*time.Minute || len(store.results) != 0 {
		t.Errorf("observe only: done=%d err=%v backlog=%v", done, err, obs.backlog)
	}
	if _, err := NewNotifier(NotifierConfig{Store: store, Sealer: &notify.Sealer{}}); err == nil {
		t.Error("sealer without sender accepted")
	}
}

// 한 policy의 알림은 tick마다 PerPolicy개까지 차례로 보낸다(응답 없는 endpoint가 다른 tenant를 막지 않게). 정리는 주기마다 돈다.
func TestNotifierPerPolicyCap(t *testing.T) {
	store, sender, _, n, _ := notifierFixture(t, 1, t0, notify.Result{StatusCode: 200})
	many := &manyDue{fakeDeliveries: store, n: 12}
	n.cfg.Store, n.cfg.PerPolicy = many, 3
	if done, err := n.Tick(context.Background()); err != nil || done != 3 || len(sender.calls) != 3 {
		t.Errorf("per policy: done=%d calls=%d err=%v", done, len(sender.calls), err)
	}
	if store.prunes != 1 {
		t.Errorf("prunes = %d", store.prunes)
	}
	if _, err := n.Tick(context.Background()); err != nil || store.prunes != 1 {
		t.Errorf("pruned again within interval: %d", store.prunes)
	}
}

// manyDue는 같은 policy의 알림 n개를 돌려준다(claim은 늘 성공).
type manyDue struct {
	*fakeDeliveries
	n int
}

func (m *manyDue) DueDeliveries(context.Context, time.Time, int, int) ([]controldb.DueDelivery, time.Time, error) {
	var out []controldb.DueDelivery
	for i := 0; i < m.n; i++ {
		out = append(out, controldb.DueDelivery{Tenant: tenant, ID: fmt.Sprint("d", i), PolicyID: "p1"})
	}
	return out, time.Time{}, nil
}

// 시험 전송: 실패해도 다시 보내지 않고(결과를 바로 보인다), 꺼진 policy에도 보낸다(켜기 전 확인). 지운 policy면 취소.
// 결과는 운영 경보(포기한 알림)에 세지 않는다(outcome test).
func TestNotifierTestDelivery(t *testing.T) {
	store, sender, obs, n, _ := notifierFixture(t, 1, t0, notify.Result{StatusCode: 503, ErrCode: notify.ErrCodeStatus})
	store.d.EventType, store.d.PolicyDisabled = notify.EventTest, true
	if _, err := n.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := store.results[0]; r.Status != "failed" || r.StatusCode != 503 || len(sender.calls) != 1 || obs.outcomes["test"] != 1 || obs.outcomes["failed"] != 0 {
		t.Errorf("test failure = %+v obs=%v", r, obs.outcomes)
	}
	store, sender, obs, n, _ = notifierFixture(t, 1, t0, notify.Result{StatusCode: 200})
	store.d.EventType, store.d.PolicyDisabled, store.d.PolicyDeleted = notify.EventTest, true, true
	if _, err := n.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := store.results[0]; r.Status != "canceled" || len(sender.calls) != 0 || obs.outcomes["canceled"] != 1 {
		t.Errorf("deleted policy test = %+v", r)
	}
}

// secret 교체 겹침 기간: 이전 secret도 함께 서명한다(지금 secret 먼저). 이전 secret을 풀 수 없으면 지금 secret만으로 보낸다.
func TestNotifierSignsWithPreviousSecret(t *testing.T) {
	store, sender, _, n, sec := notifierFixture(t, 1, t0, notify.Result{StatusCode: 200})
	sealer, _ := notify.NewSealer("v1", bytes.Repeat([]byte{5}, 32))
	prev, _ := notify.NewSecret()
	sealed, err := sealer.Seal(tenant.String(), "p1", prev)
	if err != nil {
		t.Fatal(err)
	}
	v1 := "v1"
	store.d.PreviousKeyID, store.d.PreviousSealed, store.d.PreviousSealingKey = &prev.KeyID, sealed, &v1
	if _, err := n.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ids := sender.calls[0].keyIDs; len(ids) != 2 || ids[0] != sec.KeyID || ids[1] != prev.KeyID {
		t.Errorf("signing keys = %v", ids)
	}
	store, sender, _, n, sec = notifierFixture(t, 1, t0, notify.Result{StatusCode: 200})
	bad := "v9"
	store.d.PreviousKeyID, store.d.PreviousSealed, store.d.PreviousSealingKey = &prev.KeyID, sealed, &bad
	if _, err := n.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ids := sender.calls[0].keyIDs; len(ids) != 1 || ids[0] != sec.KeyID || store.results[0].Status != "delivered" {
		t.Errorf("unreadable previous secret: keys=%v result=%+v", ids, store.results[0])
	}
}
