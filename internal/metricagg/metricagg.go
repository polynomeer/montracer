// Package metricagg는 metric window 집계의 순수 계산이다 (D02 §07, §10, §22, ADR 0025).
//
// 저장소·시계와 무관하다. 한 stream의 [start,end) window에 속한 dedup 완료 point와,
// cumulative 계산용 직전 point(baseline)를 받아 window 집계를 돌려준다. 여러 stream의 histogram은
// bucket을 먼저 병합한 뒤 percentile을 구한다 — percentile을 평균하지 않는다 (CLAUDE.md 계약 5).
//
// 모르는 값을 0으로 채우지 않는다 (계약 6): 값이 없으면 Has* 가 false이고, 불완전하면 Partial과 사유(Flags)를 둔다.
package metricagg

import (
	"errors"
	"math"
	"slices"
	"sort"
	"time"
)

// Type은 OTel metric 유형이다.
type Type uint8

// 유형.
const (
	Gauge Type = iota + 1
	Sum
	Histogram
	ExponentialHistogram
	Summary
)

// Temporality는 집계 시간성이다.
type Temporality uint8

// 시간성.
const (
	Unspecified Temporality = iota
	Delta
	Cumulative
)

// Stream은 stream identity 중 집계 의미를 정하는 부분이다 (D02 §07).
type Stream struct {
	Type        Type
	Temporality Temporality
	Monotonic   bool
	Unit        string
}

// Point는 dedup된 원본 point 하나다(metric_points 한 행).
type Point struct {
	Start, End time.Time
	Value      float64 // gauge·sum
	Count      uint64  // histogram
	Sum        float64 // histogram (HasSum일 때만 의미)
	HasSum     bool
	Bounds     []float64
	Buckets    []uint64
}

// Window는 [Start, End) 구간이다. point는 End(관측 시각)로 window에 속한다.
type Window struct {
	Start, End time.Time
}

func (w Window) contains(t time.Time) bool { return !t.Before(w.Start) && t.Before(w.End) }

// Flag는 집계 품질 사유다. 고정 문자열이며 응답·저장에 그대로 쓴다.
const (
	// FlagMissingBaseline: cumulative의 직전 point가 없어 첫 point를 기준점으로만 썼다 (D02 §07 "첫 cumulative는 기준점만").
	FlagMissingBaseline = "missing_baseline"
	// FlagReset: start_time 변경 또는 값 감소로 reset을 감지했다.
	FlagReset = "reset"
	// FlagNaN·FlagInf: 값이 NaN·±Inf인 point를 계산에서 뺐다.
	FlagNaN = "nan_value"
	FlagInf = "inf_value"
	// FlagNegativeDelta: monotonic delta가 음수라 뺐다 (D02 §10 격리 대상).
	FlagNegativeDelta = "negative_delta"
	// FlagCountBucketMismatch: histogram count와 bucket 합이 달라 뺐다 (D02 §10).
	FlagCountBucketMismatch = "count_bucket_mismatch"
	// FlagBoundsChanged: histogram 경계가 바뀌어 차분하지 못하고 새 기준점으로 삼았다.
	FlagBoundsChanged = "bounds_changed"
	// FlagDuplicateTimestamp: 같은 관측 시각에 다른 값이 있어 먼저 온 것만 썼다.
	FlagDuplicateTimestamp = "duplicate_timestamp"
	// FlagUnsupported: 이 유형은 window 집계를 하지 않는다(summary, exponential histogram — 원본 표시만).
	FlagUnsupported = "unsupported_type"
)

// Aggregate는 한 stream·한 window의 집계다. 유형에 해당 없는 값은 Has*가 false다.
type Aggregate struct {
	Samples int // window 안에서 계산에 쓴 point 수

	// gauge, non-monotonic cumulative sum: 값 통계
	HasValue              bool
	Last, Min, Max, Total float64 // Total은 평균용 합(Total/Samples)

	// monotonic sum(delta·cumulative), non-monotonic delta sum: window 동안의 증가량(순변화)
	HasIncrease bool
	Increase    float64

	// histogram: window 동안의 분포 (delta 합 또는 cumulative 차분)
	HasHistogram bool
	Count        uint64
	HistSum      float64
	HasHistSum   bool
	Bounds       []float64
	Buckets      []uint64

	Resets  int
	Flags   []string // 정렬·중복 제거
	Partial bool     // 기준점 누락·미지원 등으로 window 값이 불완전하다
}

func (a *Aggregate) flag(f string) {
	if !slices.Contains(a.Flags, f) {
		a.Flags = append(a.Flags, f)
		sort.Strings(a.Flags)
	}
}

// Compute는 window 집계를 계산한다.
// points는 window 안팎이 섞여 있어도 되고 순서도 상관없다(관측 시각으로 정렬·선별한다).
// baseline은 cumulative 차분용으로 window 시작 전 마지막 point다. 없으면 nil.
func Compute(s Stream, w Window, baseline *Point, points []Point) Aggregate {
	var a Aggregate
	in := make([]Point, 0, len(points))
	for _, p := range points {
		if w.contains(p.End) {
			in = append(in, p)
		}
	}
	sort.SliceStable(in, func(i, j int) bool { return in[i].End.Before(in[j].End) })
	in = dedupTimestamps(in, &a)

	switch s.Type {
	case Gauge:
		values(&a, in)
	case Sum:
		switch {
		case s.Temporality == Delta:
			deltaSum(&a, in, s.Monotonic)
		case s.Temporality == Cumulative && s.Monotonic:
			cumulativeSum(&a, baseline, in)
		default:
			// non-monotonic cumulative(또는 unspecified): 원시 값 의미. 임의 rate·차분을 적용하지 않는다 (D02 §07).
			values(&a, in)
		}
	case Histogram:
		if s.Temporality == Cumulative {
			cumulativeHistogram(&a, baseline, in)
		} else {
			deltaHistogram(&a, in)
		}
	default:
		a.flag(FlagUnsupported)
		a.Partial = true
	}
	return a
}

// dedupTimestamps는 같은 관측 시각의 point를 하나로 줄인다. 값이 다르면 먼저 온 것을 쓰고 표시한다.
// (worker가 상충 값을 quarantine하지만 batch를 넘는 경우가 남을 수 있다, ADR 0021 §4.)
func dedupTimestamps(in []Point, a *Aggregate) []Point {
	out := in[:0]
	for i, p := range in {
		if i > 0 && p.End.Equal(out[len(out)-1].End) {
			if !samePoint(p, out[len(out)-1]) {
				a.flag(FlagDuplicateTimestamp)
			}
			continue
		}
		out = append(out, p)
	}
	return out
}

func samePoint(x, y Point) bool {
	return x.Start.Equal(y.Start) && (x.Value == y.Value || (math.IsNaN(x.Value) && math.IsNaN(y.Value))) &&
		x.Count == y.Count && x.Sum == y.Sum && slices.Equal(x.Bounds, y.Bounds) && slices.Equal(x.Buckets, y.Buckets)
}

// finite는 NaN·Inf를 걸러 표시한다.
func finite(v float64, a *Aggregate) bool {
	switch {
	case math.IsNaN(v):
		a.flag(FlagNaN)
		return false
	case math.IsInf(v, 0):
		a.flag(FlagInf)
		return false
	}
	return true
}

func values(a *Aggregate, in []Point) {
	for _, p := range in {
		if !finite(p.Value, a) {
			continue
		}
		if !a.HasValue {
			a.HasValue, a.Min, a.Max = true, p.Value, p.Value
		}
		a.Last = p.Value
		a.Min = math.Min(a.Min, p.Value)
		a.Max = math.Max(a.Max, p.Value)
		a.Total += p.Value
		a.Samples++
	}
}

func deltaSum(a *Aggregate, in []Point, monotonic bool) {
	for _, p := range in {
		if !finite(p.Value, a) {
			continue
		}
		if monotonic && p.Value < 0 {
			a.flag(FlagNegativeDelta)
			continue
		}
		a.HasIncrease = true
		a.Increase += p.Value
		a.Samples++
	}
}

// reset은 직전 point 대비 reset인지 본다: start_time이 바뀌었거나(D02 §07) 값이 줄었다(Prometheus 규칙).
func resetSum(prev, cur Point) bool {
	return !cur.Start.Equal(prev.Start) || cur.Value < prev.Value
}

// cumulativeSum은 차분을 더한다. 기준점이 없으면 첫 point는 기준점으로만 쓴다(D02 §07).
// reset 뒤의 point는 0에서 시작한 것으로 보고 값 전체를 더한다(Prometheus increase와 같다).
func cumulativeSum(a *Aggregate, baseline *Point, in []Point) {
	var prev *Point
	if baseline != nil && finite(baseline.Value, a) {
		b := *baseline
		prev = &b
	}
	for i := range in {
		p := in[i]
		if !finite(p.Value, a) {
			continue
		}
		a.Samples++
		if prev == nil {
			a.flag(FlagMissingBaseline)
			a.Partial = true
			prev = &in[i]
			continue
		}
		a.HasIncrease = true
		if resetSum(*prev, p) {
			a.Resets++
			a.flag(FlagReset)
			a.Increase += p.Value
		} else {
			a.Increase += p.Value - prev.Value
		}
		prev = &in[i]
	}
}

// validHistogram은 bucket 수와 count 합을 확인한다.
func validHistogram(p Point, a *Aggregate) bool {
	if len(p.Buckets) != len(p.Bounds)+1 {
		a.flag(FlagCountBucketMismatch)
		return false
	}
	var total uint64
	for _, b := range p.Buckets {
		total += b
	}
	if total != p.Count {
		a.flag(FlagCountBucketMismatch)
		return false
	}
	if p.HasSum && !finite(p.Sum, a) {
		return false
	}
	return true
}

func (a *Aggregate) addHistogram(bounds []float64, buckets []uint64, count uint64, sum float64, hasSum bool) {
	if !a.HasHistogram {
		a.HasHistogram = true
		a.Bounds = slices.Clone(bounds)
		a.Buckets = make([]uint64, len(buckets))
		a.HasHistSum = true
	} else if !slices.Equal(a.Bounds, bounds) {
		// window 안에서 경계가 바뀌면 같은 bucket으로 더할 수 없다. 앞부분만 남기고 표시한다.
		a.flag(FlagBoundsChanged)
		a.Partial = true
		return
	}
	for i, b := range buckets {
		a.Buckets[i] += b
	}
	a.Count += count
	a.HistSum += sum
	a.HasHistSum = a.HasHistSum && hasSum
}

func deltaHistogram(a *Aggregate, in []Point) {
	for _, p := range in {
		if !validHistogram(p, a) {
			continue
		}
		a.addHistogram(p.Bounds, p.Buckets, p.Count, p.Sum, p.HasSum)
		a.Samples++
	}
	if a.HasHistogram && !a.HasHistSum {
		a.HistSum = math.NaN() // 보내지 않은 sum을 0으로 두지 않는다
	}
}

// resetHistogram은 cumulative histogram reset이다: start_time 변경, count 감소, 어떤 bucket이든 감소.
func resetHistogram(prev, cur Point) bool {
	if !cur.Start.Equal(prev.Start) || cur.Count < prev.Count {
		return true
	}
	for i := range cur.Buckets {
		if cur.Buckets[i] < prev.Buckets[i] {
			return true
		}
	}
	return false
}

func cumulativeHistogram(a *Aggregate, baseline *Point, in []Point) {
	var prev *Point
	if baseline != nil {
		var probe Aggregate
		if validHistogram(*baseline, &probe) {
			b := *baseline
			prev = &b
		}
	}
	for i := range in {
		p := in[i]
		if !validHistogram(p, a) {
			continue
		}
		a.Samples++
		switch {
		case prev == nil:
			a.flag(FlagMissingBaseline)
			a.Partial = true
		case !slices.Equal(prev.Bounds, p.Bounds):
			// 경계가 바뀌면 차분할 수 없다. 이 point를 새 기준점으로 삼는다.
			a.flag(FlagBoundsChanged)
			a.Partial = true
		case resetHistogram(*prev, p):
			a.Resets++
			a.flag(FlagReset)
			a.addHistogram(p.Bounds, p.Buckets, p.Count, p.Sum, p.HasSum)
		default:
			diff := make([]uint64, len(p.Buckets))
			for j := range p.Buckets {
				diff[j] = p.Buckets[j] - prev.Buckets[j]
			}
			a.addHistogram(p.Bounds, diff, p.Count-prev.Count, p.Sum-prev.Sum, p.HasSum && prev.HasSum)
		}
		prev = &in[i]
	}
	if a.HasHistogram && !a.HasHistSum {
		a.HistSum = math.NaN()
	}
}

// ErrBoundsMismatch는 경계가 다른 histogram을 병합하려 했다는 뜻이다. 임의로 재분배하지 않는다.
var ErrBoundsMismatch = errors.New("metricagg: histogram bounds differ")

// ErrUnitMismatch는 단위가 다른 stream을 합치려 했다는 뜻이다(단위 변경은 다른 stream이다, D02 §07).
var ErrUnitMismatch = errors.New("metricagg: units differ")

// HistogramState는 병합 가능한 histogram 상태다(window 집계 또는 여러 stream의 합).
type HistogramState struct {
	Unit    string
	Bounds  []float64
	Buckets []uint64
	Count   uint64
	Sum     float64 // NaN이면 일부가 sum을 보내지 않았다
}

// Merge는 여러 histogram을 bucket 단위로 더한다(여러 서비스·instance의 p95를 구하기 전에, D02 §07, R8).
// 경계나 단위가 다르면 오류다 — 재분배·평균으로 숨기지 않는다.
func Merge(states ...HistogramState) (HistogramState, error) {
	var out HistogramState
	for i, s := range states {
		if len(s.Buckets) != len(s.Bounds)+1 {
			return HistogramState{}, ErrBoundsMismatch
		}
		if i == 0 {
			out = HistogramState{Unit: s.Unit, Bounds: slices.Clone(s.Bounds), Buckets: make([]uint64, len(s.Buckets))}
		} else {
			if s.Unit != out.Unit {
				return HistogramState{}, ErrUnitMismatch
			}
			if !slices.Equal(s.Bounds, out.Bounds) {
				return HistogramState{}, ErrBoundsMismatch
			}
		}
		for j, b := range s.Buckets {
			out.Buckets[j] += b
		}
		out.Count += s.Count
		out.Sum += s.Sum
	}
	return out, nil
}

// Quantile은 q(0~1) 분위수를 bucket 안 선형 보간으로 추정한다 (Prometheus histogram_quantile과 같은 방식).
//   - 첫 bucket의 하한은 첫 경계가 양수면 0, 아니면 첫 경계 자체다.
//   - 마지막(+Inf) bucket에 걸리면 가장 큰 유한 경계를 돌려준다.
//   - 관측이 없으면 ok=false다(0을 돌려주지 않는다).
func Quantile(q float64, bounds []float64, buckets []uint64) (float64, bool) {
	if math.IsNaN(q) || q < 0 || q > 1 || len(buckets) != len(bounds)+1 {
		return math.NaN(), false
	}
	var total uint64
	for _, b := range buckets {
		total += b
	}
	if total == 0 {
		return math.NaN(), false
	}
	rank := q * float64(total)
	var cum float64
	for i, b := range buckets {
		if b == 0 {
			continue
		}
		if cum+float64(b) < rank {
			cum += float64(b)
			continue
		}
		if i == len(bounds) { // +Inf bucket: 상한을 모르므로 가장 큰 유한 경계
			if len(bounds) == 0 {
				return math.NaN(), false
			}
			return bounds[len(bounds)-1], true
		}
		upper := bounds[i]
		lower := 0.0
		switch {
		case i > 0:
			lower = bounds[i-1] //nolint:gosec // 0 < i < len(bounds) (+Inf bucket은 위에서 반환)
		case upper <= 0:
			return upper, true
		}
		return lower + (upper-lower)*((rank-cum)/float64(b)), true
	}
	return math.NaN(), false
}
