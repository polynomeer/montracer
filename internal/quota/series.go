package quota

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetry/envelope"
)

// metric cardinality 기본 한도 (D02 §10).
const (
	// DefaultActiveSeries는 조직별 활성 series 상한이다(최근 1시간 안에 관측).
	DefaultActiveSeries = 100_000
	// MaxMetricLabels는 series 하나의 point 속성(dimension) key 상한이다.
	MaxMetricLabels = 20
	// MaxMetricNameLen은 metric 이름 상한이다(등록부 컬럼 제약과 같다).
	MaxMetricNameLen = 255
)

// 거절 사유 (record 단위 partial success, D02 §22).
const (
	ReasonForbiddenDimension = "forbidden_metric_dimension"
	ReasonTooManyLabels      = "too_many_metric_labels"
	ReasonSeriesLimit        = "series_limit_exceeded"
	ReasonInvalidMetricName  = "invalid_metric_name"
)

// forbiddenDimensions는 metric dimension으로 쓸 수 없는 key다 (D02 §10: user_id·session_id·request_id·trace_id).
// OTel semantic convention 이름과 흔한 표기도 함께 막는다. 비교는 소문자로 한다.
var forbiddenDimensions = map[string]bool{
	"user_id": true, "user.id": true, "userid": true, "enduser.id": true,
	"session_id": true, "session.id": true, "sessionid": true,
	"request_id": true, "request.id": true, "requestid": true, "http.request.id": true,
	"trace_id": true, "trace.id": true, "traceid": true, "span_id": true, "span.id": true,
}

// CheckDimensions는 상태 없이 판정할 수 있는 dimension 규칙이다. 통과면 빈 문자열.
// 속성을 지워 통과시키지 않는다 — 지우면 다른 series와 합쳐진다 (D02 §10 "숨은 자동 attribute 삭제 금지").
func CheckDimensions(ref envelope.StreamRef) string {
	if ref.Metric == "" || len(ref.Metric) > MaxMetricNameLen {
		return ReasonInvalidMetricName
	}
	if hasForbidden(ref.Attributes) || hasForbidden(ref.Resource) || hasForbidden(ref.Scope) {
		return ReasonForbiddenDimension
	}
	if ref.Attributes.Len() > MaxMetricLabels {
		return ReasonTooManyLabels
	}
	return ""
}

func hasForbidden(m pcommon.Map) bool {
	found := false
	m.Range(func(k string, _ pcommon.Value) bool {
		found = forbiddenDimensions[strings.ToLower(k)]
		return !found
	})
	return found
}

// SeriesStore는 replica가 공유하는 활성 series 등록부다 (controldb.SeriesStore).
type SeriesStore interface {
	Known(ctx context.Context, tenant authz.TenantID, ids [][16]byte, since time.Time) (map[[16]byte]bool, error)
	ActiveCount(ctx context.Context, tenant authz.TenantID, since time.Time) (int, error)
	Touch(ctx context.Context, tenant authz.TenantID, ids [][16]byte, metrics []string, now time.Time) error
}

// SeriesConfig는 SeriesLimiter 설정이다.
type SeriesConfig struct {
	Store SeriesStore
	// Default는 기본 활성 series 상한이다(0이면 DefaultActiveSeries).
	Default int
	// Limit은 tenant별 상한이다(overrides). 0을 돌려주면 Default.
	Limit func(tenant string) int
	// ActiveWindow는 활성 판정 범위다(기본 1시간, D02 §10).
	ActiveWindow time.Duration
	// TouchEvery는 이미 아는 series의 last_seen을 다시 쓰는 간격이다(기본 10분).
	TouchEvery time.Duration
	// CountTTL은 tenant 활성 수를 다시 세는 간격이다(기본 15초). 그 사이 다른 replica의 신규 등록은 보이지 않으므로
	// 최악의 경우 한도를 (replica 수 − 1) × (15초 동안 한 replica가 받은 신규 series 수)만큼 넘을 수 있다(soft limit).
	CountTTL time.Duration
	// Timeout은 등록부 호출 하나의 시간 상한이다(기본 2초). 넘으면 503(재시도)이다.
	Timeout time.Duration
}

type tenantSeries struct {
	mu       sync.Mutex
	seen     map[[16]byte]time.Time // 마지막으로 등록부에 기록한 시각
	count    int
	countAt  time.Time
	lastUsed time.Time
}

// SeriesLimiter는 tenant 활성 series 상한을 적용한다 (D02 §10).
// 이미 활성인 series는 한도와 무관하게 계속 받고, 한도를 넘는 신규 series만 거절한다.
type SeriesLimiter struct {
	cfg     SeriesConfig
	mu      sync.Mutex
	tenants map[string]*tenantSeries
}

// NewSeriesLimiter는 SeriesLimiter를 만든다.
func NewSeriesLimiter(cfg SeriesConfig) *SeriesLimiter {
	if cfg.Default <= 0 {
		cfg.Default = DefaultActiveSeries
	}
	if cfg.ActiveWindow <= 0 {
		cfg.ActiveWindow = time.Hour
	}
	if cfg.TouchEvery <= 0 {
		cfg.TouchEvery = 10 * time.Minute
	}
	if cfg.CountTTL <= 0 {
		cfg.CountTTL = 15 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	return &SeriesLimiter{cfg: cfg, tenants: map[string]*tenantSeries{}}
}

func (l *SeriesLimiter) state(tenant string, now time.Time) *tenantSeries {
	l.mu.Lock()
	defer l.mu.Unlock()
	ts, ok := l.tenants[tenant]
	if !ok {
		ts = &tenantSeries{seen: map[[16]byte]time.Time{}}
		l.tenants[tenant] = ts
	}
	ts.lastUsed = now
	return ts
}

func (l *SeriesLimiter) limit(tenant string) int {
	if l.cfg.Limit != nil {
		if n := l.cfg.Limit(tenant); n > 0 {
			return n
		}
	}
	return l.cfg.Default
}

// Admit은 refs마다 거절 여부를 돌려준다. 등록부 오류는 오류로 돌려준다(호출자는 503, fail closed).
func (l *SeriesLimiter) Admit(ctx context.Context, tenant authz.TenantID, refs []envelope.StreamRef, now time.Time) ([]bool, error) {
	rejected := make([]bool, len(refs))
	if len(refs) == 0 {
		return rejected, nil
	}
	ts := l.state(tenant.String(), now)
	// 등록부 호출은 잠금 밖에서 하고 시간 상한을 둔다. 잠금을 잡은 채 DB를 기다리면 DB가 느릴 때 그 tenant의
	// 요청이 모두 줄을 서 instance 동시 처리 상한을 채운다(다른 tenant까지 503).
	ctx, cancel := context.WithTimeout(ctx, l.cfg.Timeout)
	defer cancel()

	// 요청 안의 고유 series (처음 나온 순서)
	firstIdx := map[[16]byte]int{}
	var order [][16]byte
	for i, r := range refs {
		if _, ok := firstIdx[r.StreamID]; !ok {
			firstIdx[r.StreamID] = i
			order = append(order, r.StreamID)
		}
	}
	ts.mu.Lock()
	var check [][16]byte // cache로 판정하지 못한 series
	for _, id := range order {
		if t, ok := ts.seen[id]; !ok || now.Sub(t) > l.cfg.TouchEvery {
			check = append(check, id)
		}
	}
	needCount := ts.countAt.IsZero() || now.Sub(ts.countAt) > l.cfg.CountTTL
	ts.mu.Unlock()
	if len(check) == 0 {
		return rejected, nil
	}

	since := now.Add(-l.cfg.ActiveWindow)
	known, err := l.cfg.Store.Known(ctx, tenant, check, since)
	if err != nil {
		return nil, fmt.Errorf("quota: series registry: %w", err)
	}
	var touch, fresh [][16]byte
	var touchNames []string
	for _, id := range check {
		if known[id] {
			touch = append(touch, id)
			touchNames = append(touchNames, refs[firstIdx[id]].Metric)
		} else {
			fresh = append(fresh, id)
		}
	}
	admitted := 0
	if len(fresh) > 0 {
		fetched, fetchedOK := 0, false
		if needCount {
			n, err := l.cfg.Store.ActiveCount(ctx, tenant, since)
			if err != nil {
				return nil, fmt.Errorf("quota: series registry: %w", err)
			}
			fetched, fetchedOK = n, true
		}
		// 자리 판정은 잠금 안에서 한다(같은 replica의 동시 요청이 같은 자리를 두 번 쓰지 않게).
		ts.mu.Lock()
		if fetchedOK && now.After(ts.countAt) {
			ts.count, ts.countAt = fetched, now
		}
		room := max(l.limit(tenant.String())-ts.count, 0)
		admitted = min(room, len(fresh))
		ts.count += admitted
		ts.mu.Unlock()
		over := map[[16]byte]bool{}
		for i, id := range fresh {
			if i < admitted {
				touch = append(touch, id)
				touchNames = append(touchNames, refs[firstIdx[id]].Metric)
			} else {
				over[id] = true
			}
		}
		for j, r := range refs { // 한도를 넘은 신규 series의 모든 point를 거절
			rejected[j] = over[r.StreamID]
		}
	}
	if err := l.cfg.Store.Touch(ctx, tenant, touch, touchNames, now); err != nil {
		ts.mu.Lock()
		ts.count -= admitted // 등록하지 못한 자리를 돌려준다(다음 요청이 너무 일찍 거절되지 않게)
		ts.mu.Unlock()
		return nil, fmt.Errorf("quota: series registry: %w", err)
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, id := range touch {
		ts.seen[id] = now
	}
	if len(ts.seen) > 2*l.limit(tenant.String()) {
		for id, t := range ts.seen { // cache가 한도보다 크게 자라지 않게 비활성 항목 정리
			if now.Sub(t) > l.cfg.ActiveWindow {
				delete(ts.seen, id)
			}
		}
	}
	return rejected, nil
}

// Tenants는 최근 1시간 안에 다룬 tenant 목록이다(등록부 정리 대상).
func (l *SeriesLimiter) Tenants(now time.Time) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for t, ts := range l.tenants {
		if now.Sub(ts.lastUsed) > l.cfg.ActiveWindow {
			delete(l.tenants, t)
			continue
		}
		out = append(out, t)
	}
	return out
}
