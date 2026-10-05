// Package rollup은 metric 1분 window를 dedup된 원본에서 재계산해 metric_1m에 쓰는 job이다
// (D02 §07, §10, §21~22, ADR 0025, 0026).
//
// tenant마다 (D02 §21: 입력이 멈춘 쪽은 따로 다룬다)
//
//	watermark = floor(그 tenant의 최대 관측 시각 − 2분)          (D02 §07)
//	  최대 관측 시각이 처리 시계로 60초 넘게 멈추면 now − 2분      (idle, D02 §21)
//	  now − 2분을 넘지 않는다                                     (미래 시각 point 방어)
//	계산 범위 = [min(처리 완료 위치, watermark − 10분), watermark)  (따라잡기 상한 1시간)
//
// 처리 완료 위치는 저장한다(재시작 시 metric_1m에서 복원). 그래서 watermark가 10분 넘게 뛰어도
// 사이 window를 빠뜨리지 않는다. 10분 안에 늦게 온 point는 그 window를 새 revision으로 다시 쓴다.
// 빈 window는 행을 쓰지 않는다 — 값 없음을 0으로 만들지 않는다(계약 6).
package rollup

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/polynomeer/montracer/internal/metricagg"
)

// RawPoint는 metric_points 한 행(dedup 후)이다.
type RawPoint struct {
	Tenant      string // UUID 문자열
	StreamID    [16]byte
	MetricName  string
	Unit        string
	Type        string // gauge | sum | histogram | exponential_histogram | summary
	Temporality string // unspecified | delta | cumulative
	Monotonic   bool
	// ResourceJSON·AttributesJSON은 stream label이다(정렬된 JSON, ADR 0021 §5). 조회의 filter·group_by에 쓴다.
	ResourceJSON, AttributesJSON string
	// Version은 ADR 0021 §4의 version이다(클수록 먼저 수신). 같은 관측 시각의 상충 값은 먼저 수신한 것을 쓴다.
	Version uint64
	Point   metricagg.Point
}

// Row는 metric_1m 한 행이다.
type Row struct {
	Tenant      string
	StreamID    [16]byte
	MetricName  string
	WindowStart time.Time
	Type        string
	Temporality string
	Monotonic   bool
	Unit        string
	// stream label (원본 15일보다 오래 남는 rollup을 그룹화하기 위해 함께 저장, ADR 0027)
	ResourceJSON, AttributesJSON string
	Agg                          metricagg.Aggregate
	Revision                     uint64
	ComputedAt                   time.Time
	ExpiresAt                    time.Time
}

// Progress는 저장된 rollup 진행 상태다(재시작 시 복원).
type Progress struct {
	// Done은 tenant별로 이미 쓴 가장 늦은 window의 끝이다.
	Done map[string]time.Time
	// MaxRevision은 저장된 최대 revision이다. 새 revision은 항상 이보다 크다.
	MaxRevision uint64
}

// Store는 rollup 저장소다(ClickHouse rollup 계정).
type Store interface {
	// MaxObserved는 end_time ∈ [since, until]인 원본의 tenant별 최대 관측 시각이다.
	MaxObserved(ctx context.Context, since, until time.Time) (map[string]time.Time, error)
	// LoadProgress는 저장된 진행 상태를 읽는다.
	LoadProgress(ctx context.Context, since time.Time) (Progress, error)
	// ReadPoints는 end_time ∈ [from, to)인 dedup된 원본 point를 읽는다.
	ReadPoints(ctx context.Context, from, to time.Time) ([]RawPoint, error)
	// WriteWindows는 window 행을 쓴다. 같은 rows를 다시 쓰면 무시된다(내용 기반 token).
	WriteWindows(ctx context.Context, rows []Row) error
}

// Observer는 rollup 결과를 운영 지표로 내보낸다.
type Observer interface {
	ObserveCycle(r CycleResult)
}

// CycleResult는 주기 하나의 결과다.
type CycleResult struct {
	OK       bool
	Points   int
	Windows  int            // 계산한 (stream, window) 수
	Written  int            // 내용이 바뀌어 쓴 행 수
	Flags    map[string]int // 쓴 행의 품질 사유별 수
	Gaps     int            // 따라잡기 상한을 넘어 건너뛴 tenant 수 (backfill 필요)
	Duration time.Duration
}

// Config는 Job 설정이다. 0이면 기본값.
type Config struct {
	Store    Store
	Logger   *slog.Logger
	Observer Observer
	Now      func() time.Time
	// Window는 집계 단위다(기본 1분).
	Window time.Duration
	// MaxLateness는 watermark = 최대 관측 시각 − MaxLateness다(기본 2분, D02 §07·§21).
	MaxLateness time.Duration
	// IdleAfter 동안 최대 관측 시각이 늘지 않으면 처리 시계로 window를 닫는다(기본 60초, D02 §21).
	IdleAfter time.Duration
	// Recompute는 늦은 point를 반영해 다시 계산하는 범위다(기본 10분, D02 §07).
	Recompute time.Duration
	// MaxCatchUp은 처리 완료 위치에서 따라잡는 최대 범위다(기본 1시간). 넘으면 gap으로 기록한다.
	MaxCatchUp time.Duration
	// BaselineLookback은 cumulative 기준점을 찾는 window 이전 범위다(기본 10분). 이보다 드문 stream은 missing_baseline.
	BaselineLookback time.Duration
	// Interval은 주기다(기본 30초).
	Interval time.Duration
	// Retention은 metric_1m 보존 기간이다(기본 90일, D02 §10).
	Retention time.Duration
}

// tenantState는 tenant별 watermark·진행 상태다.
type tenantState struct {
	maxObserved, advancedAt time.Time
	done                    time.Time // 쓰기를 마친 window의 끝
}

// Job은 rollup 주기 실행기다. 한 cluster에서 하나만 돌린다(ADR 0026 §4).
type Job struct {
	cfg          Config
	last         map[windowKey][32]byte // 마지막으로 쓴 내용 해시 (계산 범위 밖은 지운다)
	tenants      map[string]*tenantState
	lastRevision uint64
	loaded       bool
}

type streamKey struct {
	tenant string
	stream [16]byte
}

type windowKey struct {
	streamKey
	start int64
}

// New는 Job을 만든다.
func New(cfg Config) *Job {
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
	if cfg.BaselineLookback <= 0 {
		cfg.BaselineLookback = 10 * time.Minute
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 90 * 24 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Job{cfg: cfg, last: map[windowKey][32]byte{}, tenants: map[string]*tenantState{}}
}

// Run은 ctx가 끝날 때까지 주기를 돈다. 주기 실패는 기록하고 다음 주기에 다시 계산한다(멱등).
func (j *Job) Run(ctx context.Context) {
	t := time.NewTicker(j.cfg.Interval)
	defer t.Stop()
	for {
		if err := j.Cycle(ctx); err != nil && ctx.Err() == nil {
			j.cfg.Logger.Warn("metric rollup cycle failed", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// span은 tenant 하나의 이번 계산 범위다.
type span struct{ from, to time.Time }

// Cycle은 주기 하나를 실행한다.
func (j *Job) Cycle(ctx context.Context) error {
	start := time.Now()
	res := CycleResult{Flags: map[string]int{}}
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
			return fmt.Errorf("rollup: load progress: %w", err)
		}
		for tenant, done := range p.Done {
			j.state(tenant).done = done
		}
		j.lastRevision, j.loaded = p.MaxRevision, true
	}
	maxObs, err := j.cfg.Store.MaxObserved(ctx, now.Add(-time.Hour), now)
	if err != nil {
		return fmt.Errorf("rollup: max observed: %w", err)
	}
	spans := map[string]span{}
	var readFrom, readTo time.Time
	for tenant, obs := range maxObs {
		st := j.state(tenant)
		wm := j.watermark(st, obs, now)
		from := wm.Add(-j.cfg.Recompute)
		if !st.done.IsZero() && st.done.Before(from) {
			from = st.done // 이전에 처리하지 못한 window부터 (watermark가 뛰어도 빠뜨리지 않는다)
		}
		if limit := wm.Add(-j.cfg.MaxCatchUp); from.Before(limit) {
			res.Gaps++
			j.cfg.Logger.Warn("metric rollup gap beyond catch-up limit; backfill required",
				slog.String("tenant_id", tenant), slog.Time("from", from), slog.Time("resume", limit))
			from = limit
		}
		if !from.Before(wm) {
			continue
		}
		spans[tenant] = span{from, wm}
		if readFrom.IsZero() || from.Before(readFrom) {
			readFrom = from
		}
		if wm.After(readTo) {
			readTo = wm
		}
	}
	if len(spans) == 0 {
		res.OK = true
		return nil
	}
	points, err := j.cfg.Store.ReadPoints(ctx, readFrom.Add(-j.cfg.BaselineLookback), readTo)
	if err != nil {
		return fmt.Errorf("rollup: read points: %w", err)
	}
	res.Points = len(points)
	revision := max(uint64(now.UnixNano()), j.lastRevision+1) //nolint:gosec // 1970 이후 시각
	rows := j.compute(points, spans, revision, now)
	res.Windows = len(rows)
	var changed []Row
	var sums [][32]byte
	for _, r := range rows {
		h := contentHash(r)
		if j.last[windowKey{streamKey{r.Tenant, r.StreamID}, r.WindowStart.Unix()}] == h {
			continue
		}
		changed = append(changed, r)
		sums = append(sums, h)
	}
	if len(changed) > 0 {
		if err := j.cfg.Store.WriteWindows(ctx, changed); err != nil {
			return fmt.Errorf("rollup: write windows: %w", err)
		}
		j.lastRevision = revision
	}
	for i, r := range changed {
		j.last[windowKey{streamKey{r.Tenant, r.StreamID}, r.WindowStart.Unix()}] = sums[i]
		for _, f := range r.Agg.Flags {
			res.Flags[f]++
		}
	}
	for tenant, sp := range spans {
		j.tenants[tenant].done = sp.to
	}
	for k := range j.last {
		if sp, ok := spans[k.tenant]; !ok || k.start < sp.to.Add(-j.cfg.Recompute).Unix() {
			delete(j.last, k) // 재계산 범위를 벗어난 window는 더 쓰지 않는다
		}
	}
	res.Written, res.OK = len(changed), true
	return nil
}

func (j *Job) state(tenant string) *tenantState {
	st, ok := j.tenants[tenant]
	if !ok {
		st = &tenantState{}
		j.tenants[tenant] = st
	}
	return st
}

// watermark는 tenant 하나의 watermark다 (D02 §07, §21).
// 수집이 밀려 관측 시각이 늦으면 window를 늦게 닫는다. 입력이 IdleAfter 넘게 멈추면 처리 시계로 닫는다.
// 처리 시계보다 앞서지는 않는다(미래 시각 point가 window를 미리 닫지 못하게).
func (j *Job) watermark(st *tenantState, obs, now time.Time) time.Time {
	if obs.After(st.maxObserved) {
		st.maxObserved, st.advancedAt = obs, now
	}
	processing := now.Add(-j.cfg.MaxLateness)
	wm := st.maxObserved.Add(-j.cfg.MaxLateness)
	if now.Sub(st.advancedAt) > j.cfg.IdleAfter {
		wm = processing // idle: 입력이 멈춘 window도 닫는다
	}
	if wm.After(processing) {
		wm = processing
	}
	return wm.Truncate(j.cfg.Window)
}

// compute는 stream × window 집계 행을 만든다. tenant마다 자기 범위만 계산하고, point가 없는 window는 행이 없다.
func (j *Job) compute(points []RawPoint, spans map[string]span, revision uint64, now time.Time) []Row {
	byStream := map[streamKey][]RawPoint{}
	for _, p := range points {
		if _, ok := spans[p.Tenant]; !ok {
			continue
		}
		k := streamKey{p.Tenant, p.StreamID}
		byStream[k] = append(byStream[k], p)
	}
	keys := make([]streamKey, 0, len(byStream))
	for k := range byStream {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].tenant != keys[b].tenant {
			return keys[a].tenant < keys[b].tenant
		}
		return string(keys[a].stream[:]) < string(keys[b].stream[:])
	})
	var rows []Row
	for _, k := range keys {
		ps := byStream[k]
		// 관측 시각 순, 같은 시각이면 먼저 수신한 것(version 큰 것)부터 — metricagg가 첫 값을 쓴다(ADR 0021 §4)
		sort.SliceStable(ps, func(a, b int) bool {
			if !ps[a].Point.End.Equal(ps[b].Point.End) {
				return ps[a].Point.End.Before(ps[b].Point.End)
			}
			return ps[a].Version > ps[b].Version
		})
		meta := ps[len(ps)-1] // 이름·단위·유형은 최신 point 기준 (단위가 바뀌면 stream이 바뀐다, D02 §07)
		stream := metricagg.Stream{Type: parseType(meta.Type), Temporality: parseTemporality(meta.Temporality),
			Monotonic: meta.Monotonic, Unit: meta.Unit}
		all := make([]metricagg.Point, len(ps))
		for i := range ps {
			all[i] = ps[i].Point
		}
		sp := spans[k.tenant]
		for ws := sp.from; ws.Before(sp.to); ws = ws.Add(j.cfg.Window) {
			w := metricagg.Window{Start: ws, End: ws.Add(j.cfg.Window)}
			var baseline *metricagg.Point
			inWindow := false
			for i := range all {
				switch {
				case all[i].End.Before(ws):
					baseline = &all[i]
				case all[i].End.Before(w.End):
					inWindow = true
				}
			}
			if !inWindow {
				continue
			}
			rows = append(rows, Row{
				Tenant: k.tenant, StreamID: k.stream, MetricName: meta.MetricName, WindowStart: ws,
				Type: meta.Type, Temporality: meta.Temporality, Monotonic: meta.Monotonic, Unit: meta.Unit,
				ResourceJSON: meta.ResourceJSON, AttributesJSON: meta.AttributesJSON,
				Agg: metricagg.Compute(stream, w, baseline, all), Revision: revision, ComputedAt: now,
				ExpiresAt: ws.Add(j.cfg.Retention),
			})
		}
	}
	return rows
}

func parseType(s string) metricagg.Type {
	switch s {
	case "gauge":
		return metricagg.Gauge
	case "sum":
		return metricagg.Sum
	case "histogram":
		return metricagg.Histogram
	case "exponential_histogram":
		return metricagg.ExponentialHistogram
	default:
		return metricagg.Summary
	}
}

func parseTemporality(s string) metricagg.Temporality {
	switch s {
	case "delta":
		return metricagg.Delta
	case "cumulative":
		return metricagg.Cumulative
	default:
		return metricagg.Unspecified
	}
}

// contentHash는 revision·계산 시각을 뺀 window 내용의 해시다. 같으면 다시 쓰지 않는다.
func contentHash(r Row) [32]byte {
	h := sha256.New()
	w := func(s string) { _, _ = h.Write([]byte(s)); _, _ = h.Write([]byte{0}) }
	f := func(v float64) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], math.Float64bits(v))
		_, _ = h.Write(b[:])
	}
	u := func(v uint64) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], v)
		_, _ = h.Write(b[:])
	}
	a := r.Agg
	w(r.Tenant)
	_, _ = h.Write(r.StreamID[:])
	w(r.MetricName)
	w(r.Type)
	w(r.Temporality)
	w(r.Unit)
	w(r.ResourceJSON)
	w(r.AttributesJSON)
	u(uint64(r.WindowStart.Unix())) //nolint:gosec // 1970 이후 시각
	u(uint64(a.Samples))            //nolint:gosec // 0 이상
	for _, b := range []bool{a.HasValue, a.HasIncrease, a.HasHistogram, a.HasHistSum, a.Partial, r.Monotonic} {
		if b {
			u(1)
		} else {
			u(0)
		}
	}
	for _, v := range []float64{a.Last, a.Min, a.Max, a.Total, a.Increase, a.HistSum} {
		f(v)
	}
	u(a.Count)
	u(uint64(a.Resets)) //nolint:gosec // 0 이상
	for _, v := range a.Bounds {
		f(v)
	}
	w("|")
	for _, v := range a.Buckets {
		u(v)
	}
	w("|")
	for _, fl := range a.Flags {
		w(fl)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ContentToken은 rows 전체의 insert dedup token이다(재시도해도 같은 내용이면 한 번만 들어간다).
func ContentToken(rows []Row) string {
	h := sha256.New()
	for _, r := range rows {
		c := contentHash(r)
		_, _ = h.Write(c[:])
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], r.Revision)
		_, _ = h.Write(b[:])
	}
	return fmt.Sprintf("rollup/%x", h.Sum(nil)[:16]) // token은 테이블마다 따로 비교된다
}
