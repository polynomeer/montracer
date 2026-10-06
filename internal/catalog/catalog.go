// Package catalog은 수집에서 본 서비스를 서비스 catalog(제어 DB)에 등록한다 (D02 §08, ADR 0038).
//
// catalog는 metadata라 수집 경로를 막지 않는다: ingress는 Kafka ACK 뒤 sighting을 queue에 넣기만 하고(즉시 반환),
// background flusher가 tenant별로 모아 일정 주기로 쓴다. queue가 차면 sighting을 버리고 센다(다음 요청이 다시 넣는다).
// 같은 서비스는 TouchEvery 안에 다시 쓰지 않는다(replica별 cache) — last_seen은 그만큼 늦을 수 있다(inactive 판정은 24시간).
package catalog

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

// Store는 catalog 저장소다 (controldb.ServiceStore).
type Store interface {
	// Observe는 tenant 상한 때문에 등록하지 않은 새 서비스 수를 돌려준다.
	Observe(ctx context.Context, tenant authz.TenantID, obs []controldb.ServiceObservation) (int, error)
}

// Sighting은 요청 하나에서 본 서비스다.
type Sighting struct {
	ServiceID, Environment, Namespace, Name, Language string
}

// Observer는 운영 지표다.
type Observer interface {
	// written: 쓴 서비스, dropped: queue 초과로 버림, overLimit: tenant 상한으로 등록하지 않음, failed: batch 쓰기 실패.
	ObserveCatalog(written, dropped, overLimit int, failed bool)
}

// Config는 Registrar 설정이다. 0이면 기본값.
type Config struct {
	Store    Store
	Logger   *slog.Logger
	Observer Observer
	Now      func() time.Time
	// QueueSize는 아직 쓰지 않은 요청 묶음 상한이다(기본 4,096).
	QueueSize int
	// FlushEvery는 쓰기 주기다(기본 5초).
	FlushEvery time.Duration
	// TouchEvery 안에 쓴 서비스는 다시 쓰지 않는다(기본 5분).
	TouchEvery time.Duration
	// Timeout은 쓰기 한 번의 상한이다(기본 5초).
	Timeout time.Duration
	// ShutdownTimeout은 종료 시 마지막 flush 전체의 상한이다(기본 10초, 제어 DB 장애가 종료를 끌지 않게).
	ShutdownTimeout time.Duration
	// MaxCached는 replica cache 항목 상한이다(기본 100,000). 넘으면 새 항목은 cache하지 않는다(쓰기는 계속, 메모리 상한).
	MaxCached int
}

type batch struct {
	tenant    authz.TenantID
	sightings []Sighting
	at        time.Time
}

type cacheKey struct {
	tenant  authz.TenantID
	service string
}

// Registrar는 sighting을 모아 catalog에 쓴다.
type Registrar struct {
	cfg     Config
	queue   chan batch
	mu      sync.Mutex
	written map[cacheKey]time.Time // 마지막으로 쓴 시각
}

// New는 Registrar를 만든다. Run으로 flusher를 돌린다.
func New(cfg Config) *Registrar {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 4096
	}
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = 5 * time.Second
	}
	if cfg.TouchEvery <= 0 {
		cfg.TouchEvery = 5 * time.Minute
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 10 * time.Second
	}
	if cfg.MaxCached <= 0 {
		cfg.MaxCached = 100_000
	}
	return &Registrar{cfg: cfg, queue: make(chan batch, cfg.QueueSize), written: map[cacheKey]time.Time{}}
}

// Observe는 ACK한 요청의 서비스를 넣는다. 막지 않는다: queue가 차면 버리고 센다.
func (r *Registrar) Observe(tenant authz.TenantID, sightings []Sighting, at time.Time) {
	if len(sightings) == 0 {
		return
	}
	select {
	case r.queue <- batch{tenant: tenant, sightings: sightings, at: at}:
	default:
		if r.cfg.Observer != nil {
			r.cfg.Observer.ObserveCatalog(0, len(sightings), 0, false)
		}
	}
}

// Run은 ctx가 끝날 때까지 주기적으로 쓴다. 끝날 때 남은 것을 한 번 더 쓴다.
func (r *Registrar) Run(ctx context.Context) {
	t := time.NewTicker(r.cfg.FlushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), r.cfg.ShutdownTimeout)
			r.Flush(sctx)
			cancel()
			return
		case <-t.C:
			r.Flush(ctx)
		}
	}
}

// Flush는 queue에 쌓인 sighting을 tenant별로 모아 쓴다(시험·종료용으로도 쓴다).
func (r *Registrar) Flush(ctx context.Context) {
	pending := map[authz.TenantID]map[string]controldb.ServiceObservation{}
drain:
	for {
		select {
		case b := <-r.queue:
			m := pending[b.tenant]
			if m == nil {
				m = map[string]controldb.ServiceObservation{}
				pending[b.tenant] = m
			}
			for _, s := range b.sightings {
				cur, ok := m[s.ServiceID]
				if ok && !b.at.After(cur.SeenAt) && (cur.Language != "" || s.Language == "") {
					continue
				}
				lang := s.Language
				if lang == "" {
					lang = cur.Language
				}
				m[s.ServiceID] = controldb.ServiceObservation{ServiceID: s.ServiceID, Environment: s.Environment,
					Namespace: s.Namespace, Name: s.Name, Language: lang, SeenAt: maxTime(b.at, cur.SeenAt)}
			}
		default:
			break drain
		}
	}
	now := r.cfg.Now()
	for tenant, m := range pending {
		var obs []controldb.ServiceObservation
		r.mu.Lock()
		for id, o := range m {
			if last, ok := r.written[cacheKey{tenant, id}]; ok && now.Sub(last) < r.cfg.TouchEvery {
				continue // 최근에 썼다(replica cache)
			}
			obs = append(obs, o)
		}
		r.mu.Unlock()
		if len(obs) == 0 {
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
		overLimit, err := r.cfg.Store.Observe(wctx, tenant, obs)
		cancel()
		if err != nil {
			// 다음 sighting이 다시 넣는다(cache를 갱신하지 않는다). 수집에는 영향이 없다.
			r.cfg.Logger.Warn("service catalog write failed", slog.String("tenant_id", tenant.String()), slog.String("error", err.Error()))
			if r.cfg.Observer != nil {
				r.cfg.Observer.ObserveCatalog(0, 0, 0, true)
			}
			continue
		}
		// 상한에 걸린 서비스도 cache한다: TouchEvery마다 한 번만 다시 시도한다.
		r.mu.Lock()
		for _, o := range obs {
			k := cacheKey{tenant, o.ServiceID}
			if _, ok := r.written[k]; ok || len(r.written) < r.cfg.MaxCached {
				r.written[k] = now
			}
		}
		r.mu.Unlock()
		if r.cfg.Observer != nil {
			r.cfg.Observer.ObserveCatalog(len(obs)-overLimit, 0, overLimit, false)
		}
	}
	r.prune(now)
}

// prune은 TouchEvery의 두 배 지난 cache 항목을 지운다(메모리 상한). Flush 끝에 한 번 부른다.
func (r *Registrar) prune(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, at := range r.written {
		if now.Sub(at) > 2*r.cfg.TouchEvery {
			delete(r.written, k)
		}
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
