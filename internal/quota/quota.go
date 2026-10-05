// Package quota는 tenant·signal별 수집 rate limit이다 (D02 §04~05, D04 §08, ADR 0024).
//
// tenant마다 signal별로 record·byte 두 token bucket을 둔다. 요청은 두 bucket을 모두 통과해야 하며
// 하나라도 부족하면 아무것도 소비하지 않는다(all-or-nothing).
//
//	rate 초과        → 429 + Retry-After (client 재시도 = backpressure, 유실 없음)
//	요청 하나가 burst 초과 → 영원히 통과할 수 없으므로 413 (batch 분할 요구)
//
// rate는 cluster 전체 값이다. ingress replica마다 rate/replicas로 나눠 적용한다(global 전략).
// burst는 나누지 않는다 — 나누면 replica가 많을 때 burst가 최대 요청 크기보다 작아져 정상 batch가 413을 받는다.
// 기본값은 설정으로, tenant별 값은 overrides 파일로 바꾸며 파일은 주기적으로 다시 읽는다.
package quota

import (
	"math"
	"sync"
	"time"
)

// Limits는 tenant·signal 하나의 cluster 전체 한도다.
type Limits struct {
	RecordsPerSecond float64 `json:"records_per_second"`
	RecordsBurst     int     `json:"records_burst"`
	BytesPerSecond   float64 `json:"bytes_per_second"`
	BytesBurst       int     `json:"bytes_burst"`
}

// DefaultLimits는 tenant·signal별 기본 한도다 (ADR 0024 §2).
//   - record: 10,000/s, burst 200,000 (Mimir 기본 ingestion_rate·burst와 같은 규모)
//   - byte  : 15MB/s, burst 20MB (Tempo 기본 rate_limit_bytes·burst_size_bytes). burst는 최대 해제 본문(8MiB)보다 크고
//     replica 수로 나누지 않으므로 정상 최대 요청이 byte 때문에 413을 받지 않는다
var DefaultLimits = Limits{RecordsPerSecond: 10_000, RecordsBurst: 200_000, BytesPerSecond: 15_000_000, BytesBurst: 20_000_000}

func (l Limits) valid() bool {
	return l.RecordsPerSecond > 0 && l.RecordsBurst > 0 && l.BytesPerSecond > 0 && l.BytesBurst > 0
}

// Overrides는 tenant(UUID 문자열) → signal(traces·logs·metrics) → 한도다. 없는 항목은 기본값을 쓴다.
type Overrides map[string]map[string]Limits

// Outcome은 판정 결과다.
type Outcome uint8

const (
	// Allowed: 통과. token을 소비했다.
	Allowed Outcome = iota
	// RateLimited: 지금은 부족하다. RetryAfter 뒤에는 통과할 수 있다(429).
	RateLimited
	// OverBurst: 요청 하나가 burst보다 커서 기다려도 통과할 수 없다(413).
	OverBurst
)

// Decision은 판정과 근거다. 로그에 남겨 과부하(503)와 계약 초과(429·413)를 구분한다 (D01 §08).
type Decision struct {
	Outcome    Outcome
	RetryAfter time.Duration
	// Limit은 걸린 bucket이다: "records" 또는 "bytes". 통과면 빈 문자열.
	Limit string
	// 적용한 한도(걸린 bucket 기준). replica 수 설정 오류 같은 내부 원인을 사후에 구분하는 데 쓴다.
	RatePerReplica float64
	Burst          int
	Replicas       int
	Overridden     bool
}

// Config는 Limiter 설정이다.
type Config struct {
	Default Limits
	// Replicas는 ingress replica 수다. rate를 이 수로 나눠 각 replica에 적용한다(1 이상). burst는 나누지 않는다.
	Replicas int
	// Overrides는 현재 tenant별 한도를 돌려준다(파일 reload 결과). nil이면 기본값만.
	Overrides func() Overrides
	// IdleTTL 동안 쓰이지 않은 bucket은 지운다(기본 10분). 다시 오면 burst가 가득 찬 새 bucket이다.
	IdleTTL time.Duration
	// MinBytesBurst는 byte burst 하한이다(해제 본문 상한). overrides가 더 낮게 잡아도 이 값을 쓴다 —
	// 정상 최대 요청이 413(영구 거절)을 받지 않게 한다. tenant를 줄이려면 burst가 아니라 rate를 낮춘다.
	MinBytesBurst int
}

type key struct{ tenant, signal string }

// tokens는 token bucket 하나다. 잠금은 bucket이 가진다.
type tokens struct {
	rate  float64 // 초당 적립 (replica 몫)
	burst float64
	avail float64
}

func (t *tokens) refill(elapsed time.Duration) {
	if elapsed > 0 {
		t.avail = math.Min(t.burst, t.avail+elapsed.Seconds()*t.rate)
	}
}

// wait는 n개가 모일 때까지 걸리는 시간이다.
func (t *tokens) wait(n float64) time.Duration {
	if t.avail >= n {
		return 0
	}
	return time.Duration((n - t.avail) / t.rate * float64(time.Second))
}

type bucket struct {
	mu             sync.Mutex
	records, bytes tokens
	limits         Limits // 적용 중인 cluster 한도
	overridden     bool
	last           time.Time // 마지막 적립 시각 (단조 증가)
	lastUsed       time.Time
}

// Limiter는 tenant·signal별 bucket 모음이다. 동시에 써도 안전하다.
type Limiter struct {
	cfg       Config
	mu        sync.Mutex
	buckets   map[key]*bucket
	lastSweep time.Time
}

// New는 Limiter를 만든다.
func New(cfg Config) *Limiter {
	if !cfg.Default.valid() {
		cfg.Default = DefaultLimits
	}
	if cfg.Replicas < 1 {
		cfg.Replicas = 1
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = 10 * time.Minute
	}
	return &Limiter{cfg: cfg, buckets: map[key]*bucket{}}
}

func (l *Limiter) limitsFor(tenant, signal string) (Limits, bool) {
	if l.cfg.Overrides != nil {
		if o, ok := l.cfg.Overrides()[tenant][signal]; ok && o.valid() {
			return o, true
		}
	}
	return l.cfg.Default, false
}

// Allow는 records·bytes만큼 token을 소비할 수 있는지 판정한다.
// 확인과 차감을 bucket 잠금 하나 안에서 하므로, 통과하지 못하면 어느 bucket도 소비하지 않는다(all-or-nothing).
func (l *Limiter) Allow(tenant, signal string, records, bytes int, now time.Time) Decision {
	want, overridden := l.limitsFor(tenant, signal)
	b := l.bucketFor(tenant, signal, now)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limits != want {
		l.apply(b, want)
	}
	b.overridden = overridden
	// 동시 요청의 시각이 뒤섞여도 적립 시각은 되돌리지 않는다(되돌리면 같은 구간이 두 번 적립된다).
	if now.After(b.last) {
		b.records.refill(now.Sub(b.last))
		b.bytes.refill(now.Sub(b.last))
		b.last = now
	}
	b.lastUsed = now
	decision := func(o Outcome, limit string, t *tokens, wait time.Duration) Decision {
		return Decision{Outcome: o, Limit: limit, RetryAfter: wait, RatePerReplica: t.rate, Burst: int(t.burst),
			Replicas: l.cfg.Replicas, Overridden: overridden}
	}
	r, by := float64(records), float64(bytes)
	if r > b.records.burst {
		return decision(OverBurst, "records", &b.records, 0)
	}
	if by > b.bytes.burst {
		return decision(OverBurst, "bytes", &b.bytes, 0)
	}
	rw, bw := b.records.wait(r), b.bytes.wait(by)
	switch {
	case rw == 0 && bw == 0:
		b.records.avail -= r
		b.bytes.avail -= by
		return Decision{Outcome: Allowed}
	case bw > rw:
		return decision(RateLimited, "bytes", &b.bytes, bw)
	default:
		return decision(RateLimited, "records", &b.records, rw)
	}
}

// apply는 한도를 바꾼다. 남은 token은 새 burst를 넘지 않는 범위에서 유지한다. b.mu를 잡고 부른다.
func (l *Limiter) apply(b *bucket, want Limits) {
	set := func(t *tokens, rate float64, burst int) {
		t.rate, t.burst = rate/float64(l.cfg.Replicas), float64(burst)
		t.avail = math.Min(t.avail, t.burst)
	}
	set(&b.records, want.RecordsPerSecond, want.RecordsBurst)
	set(&b.bytes, want.BytesPerSecond, max(want.BytesBurst, l.cfg.MinBytesBurst))
	b.limits = want
}

func (l *Limiter) bucketFor(tenant, signal string, now time.Time) *bucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) > time.Minute {
		for k, b := range l.buckets {
			b.mu.Lock()
			idle := now.Sub(b.lastUsed) > l.cfg.IdleTTL
			b.mu.Unlock()
			if idle {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}
	k := key{tenant, signal}
	b, ok := l.buckets[k]
	if !ok {
		want, _ := l.limitsFor(tenant, signal)
		b = &bucket{last: now, lastUsed: now}
		l.apply(b, want)
		b.records.avail, b.bytes.avail = b.records.burst, b.bytes.burst // 새 bucket은 가득 찬 상태
		l.buckets[k] = b
	}
	return b
}

// RetryAfterSeconds는 Retry-After header 값이다: 올림, 최소 1초, 최대 60초.
func RetryAfterSeconds(d time.Duration) int {
	s := int(math.Ceil(d.Seconds()))
	return min(max(s, 1), 60)
}
