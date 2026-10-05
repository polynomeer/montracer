package metricagg

// metric oracle (D06 §04): cumulative reset, out-of-order, duplicate delta, missing baseline,
// histogram 경계 차이, 단위 변경, NaN/Inf, 빈 bucket, 알려진 분포의 p95, percentile 평균이면 실패하는 fixture.

import (
	"errors"
	"math"
	"slices"
	"testing"
	"time"
)

var (
	t0  = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	win = Window{Start: t0, End: t0.Add(time.Minute)}
)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

func counter(start time.Time, endSec int, v float64) Point {
	return Point{Start: start, End: at(endSec), Value: v}
}

var monoCum = Stream{Type: Sum, Temporality: Cumulative, Monotonic: true, Unit: "1"}

func TestCumulativeIncreaseWithBaseline(t *testing.T) {
	st := t0.Add(-time.Hour)
	base := counter(st, -15, 100)
	a := Compute(monoCum, win, &base, []Point{counter(st, 0, 110), counter(st, 15, 130), counter(st, 30, 160)})
	if !a.HasIncrease || a.Increase != 60 || a.Partial || a.Resets != 0 || a.Samples != 3 {
		t.Fatalf("= %+v", a)
	}
}

// reset: start_time 변경과 값 감소 모두 reset이고, reset 뒤 값은 0부터 쌓인 것으로 본다.
func TestCumulativeReset(t *testing.T) {
	st, restarted := t0.Add(-time.Hour), t0.Add(20*time.Second)
	base := counter(st, -15, 100)
	a := Compute(monoCum, win, &base, []Point{
		counter(st, 0, 110),        // +10
		counter(st, 15, 120),       // +10
		counter(restarted, 30, 5),  // start_time 변경 → reset, +5
		counter(restarted, 45, 12), // +7
	})
	if a.Increase != 32 || a.Resets != 1 || !slices.Contains(a.Flags, FlagReset) {
		t.Fatalf("start change = %+v", a)
	}
	// start_time 없이 값만 감소(Prometheus식 reset)
	b := Compute(monoCum, win, &base, []Point{counter(st, 0, 110), counter(st, 15, 3), counter(st, 30, 8)})
	if b.Increase != 10+3+5 || b.Resets != 1 {
		t.Fatalf("decrease = %+v", b)
	}
}

// out-of-order: 도착 순서와 무관하게 관측 시각 순서로 계산한다.
func TestOutOfOrder(t *testing.T) {
	st := t0.Add(-time.Hour)
	base := counter(st, -15, 100)
	ordered := []Point{counter(st, 0, 110), counter(st, 15, 130), counter(st, 30, 160)}
	shuffled := []Point{ordered[2], ordered[0], ordered[1]}
	a, b := Compute(monoCum, win, &base, ordered), Compute(monoCum, win, &base, shuffled)
	if a.Increase != b.Increase || b.Resets != 0 {
		t.Fatalf("ordered=%v shuffled=%+v (순서가 바뀌면 거짓 reset이 생기면 안 된다)", a.Increase, b)
	}
}

// duplicate delta: 같은 point가 두 번 와도 한 번만 더한다.
func TestDuplicateDelta(t *testing.T) {
	s := Stream{Type: Sum, Temporality: Delta, Monotonic: true}
	p := Point{Start: at(0), End: at(10), Value: 5}
	a := Compute(s, win, nil, []Point{p, p, {Start: at(10), End: at(20), Value: 3}})
	if a.Increase != 8 || a.Samples != 2 {
		t.Fatalf("= %+v", a)
	}
	// 같은 관측 시각에 다른 값: 먼저 온 것만, 표시
	b := Compute(s, win, nil, []Point{p, {Start: at(0), End: at(10), Value: 9}})
	if b.Increase != 5 || !slices.Contains(b.Flags, FlagDuplicateTimestamp) {
		t.Fatalf("conflict = %+v", b)
	}
}

// missing baseline: 첫 cumulative는 기준점으로만 쓰고 불완전으로 표시한다 (D02 §07).
func TestMissingBaseline(t *testing.T) {
	st := t0.Add(-time.Hour)
	a := Compute(monoCum, win, nil, []Point{counter(st, 0, 1000), counter(st, 30, 1010)})
	if a.Increase != 10 || !a.Partial || !slices.Contains(a.Flags, FlagMissingBaseline) {
		t.Fatalf("= %+v (누적 1000을 증가량으로 세면 안 된다)", a)
	}
	// 기준점도 없고 point 하나뿐이면 증가량을 모른다 — 0이 아니다
	b := Compute(monoCum, win, nil, []Point{counter(st, 0, 1000)})
	if b.HasIncrease || !b.Partial {
		t.Fatalf("single = %+v", b)
	}
}

func TestNegativeDeltaExcluded(t *testing.T) {
	s := Stream{Type: Sum, Temporality: Delta, Monotonic: true}
	a := Compute(s, win, nil, []Point{{End: at(0), Value: 5}, {End: at(10), Value: -3}})
	if a.Increase != 5 || !slices.Contains(a.Flags, FlagNegativeDelta) {
		t.Fatalf("= %+v", a)
	}
	// non-monotonic delta는 음수가 정상이다(순변화)
	b := Compute(Stream{Type: Sum, Temporality: Delta}, win, nil, []Point{{End: at(0), Value: 5}, {End: at(10), Value: -3}})
	if b.Increase != 2 || len(b.Flags) != 0 {
		t.Fatalf("non-monotonic = %+v", b)
	}
}

// NaN·Inf는 계산에서 빼고 표시한다. 결측을 0으로 채우지 않는다.
func TestNaNInfAndEmptyGauge(t *testing.T) {
	g := Stream{Type: Gauge}
	a := Compute(g, win, nil, []Point{{End: at(0), Value: 4}, {End: at(10), Value: math.NaN()}, {End: at(20), Value: math.Inf(1)}, {End: at(30), Value: 2}})
	if !a.HasValue || a.Min != 2 || a.Max != 4 || a.Last != 2 || a.Samples != 2 || a.Total != 6 ||
		!slices.Contains(a.Flags, FlagNaN) || !slices.Contains(a.Flags, FlagInf) {
		t.Fatalf("= %+v", a)
	}
	empty := Compute(g, win, nil, nil)
	if empty.HasValue || empty.Samples != 0 {
		t.Fatalf("empty window must have no value, got %+v", empty)
	}
	// window 밖 point는 쓰지 않는다 ([start,end))
	edge := Compute(g, win, nil, []Point{{End: win.End, Value: 1}, {End: win.Start.Add(-time.Nanosecond), Value: 1}})
	if edge.HasValue {
		t.Fatalf("points outside [start,end) used: %+v", edge)
	}
}

// non-monotonic cumulative는 원시 값이다 — 차분·rate를 적용하지 않는다.
func TestNonMonotonicCumulativeIsValue(t *testing.T) {
	s := Stream{Type: Sum, Temporality: Cumulative}
	a := Compute(s, win, nil, []Point{{End: at(0), Value: 10}, {End: at(10), Value: 7}})
	if !a.HasValue || a.HasIncrease || a.Last != 7 || a.Resets != 0 {
		t.Fatalf("= %+v", a)
	}
}

func hist(start time.Time, endSec int, buckets ...uint64) Point {
	var n uint64
	for _, b := range buckets {
		n += b
	}
	return Point{Start: start, End: at(endSec), Bounds: []float64{10, 100}, Buckets: buckets, Count: n, Sum: float64(n), HasSum: true}
}

func TestCumulativeHistogramDiffAndReset(t *testing.T) {
	s := Stream{Type: Histogram, Temporality: Cumulative}
	st := t0.Add(-time.Hour)
	base := hist(st, -15, 1, 1, 0)
	a := Compute(s, win, &base, []Point{hist(st, 0, 3, 2, 0), hist(st, 30, 4, 4, 1)})
	if !slices.Equal(a.Buckets, []uint64{3, 3, 1}) || a.Count != 7 || a.Resets != 0 {
		t.Fatalf("diff = %+v", a)
	}
	// bucket 감소 → reset, 새 값 전체를 더한다
	r := Compute(s, win, &base, []Point{hist(st, 0, 3, 2, 0), hist(st, 30, 1, 0, 0)})
	if r.Resets != 1 || !slices.Equal(r.Buckets, []uint64{3, 1, 0}) {
		t.Fatalf("reset = %+v", r)
	}
}

// histogram 경계 차이: 한 stream 안에서 바뀌면 새 기준점, 여러 stream 병합은 오류 (재분배하지 않는다).
func TestHistogramBoundsDifference(t *testing.T) {
	s := Stream{Type: Histogram, Temporality: Cumulative}
	st := t0.Add(-time.Hour)
	base := hist(st, -15, 1, 1, 0)
	changed := Point{Start: st, End: at(30), Bounds: []float64{5, 50}, Buckets: []uint64{9, 9, 9}, Count: 27}
	a := Compute(s, win, &base, []Point{hist(st, 0, 3, 2, 0), changed})
	if !slices.Contains(a.Flags, FlagBoundsChanged) || !a.Partial || a.Count != 3 {
		t.Fatalf("= %+v", a)
	}
	_, err := Merge(HistogramState{Bounds: []float64{10}, Buckets: []uint64{1, 1}}, HistogramState{Bounds: []float64{20}, Buckets: []uint64{1, 1}})
	if !errors.Is(err, ErrBoundsMismatch) {
		t.Fatalf("merge mismatched bounds: %v", err)
	}
}

// 단위 변경: 다른 단위의 stream은 합치지 않는다.
func TestUnitChange(t *testing.T) {
	_, err := Merge(HistogramState{Unit: "ms", Bounds: []float64{10}, Buckets: []uint64{1, 1}}, HistogramState{Unit: "s", Bounds: []float64{10}, Buckets: []uint64{1, 1}})
	if !errors.Is(err, ErrUnitMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestCountBucketMismatchExcluded(t *testing.T) {
	s := Stream{Type: Histogram, Temporality: Delta}
	bad := Point{End: at(0), Bounds: []float64{10}, Buckets: []uint64{1, 1}, Count: 5}
	good := Point{End: at(10), Bounds: []float64{10}, Buckets: []uint64{1, 1}, Count: 2}
	a := Compute(s, win, nil, []Point{bad, good})
	if a.Count != 2 || !slices.Contains(a.Flags, FlagCountBucketMismatch) || !math.IsNaN(a.HistSum) {
		t.Fatalf("= %+v (sum을 보내지 않았으면 NaN)", a)
	}
}

// 빈 bucket과 빈 histogram: 0을 돌려주지 않는다.
func TestQuantileEmptyBuckets(t *testing.T) {
	if _, ok := Quantile(0.95, []float64{10, 100}, []uint64{0, 0, 0}); ok {
		t.Error("no observations must not yield a quantile")
	}
	// 가운데 bucket만 비어 있어도 정상 보간
	v, ok := Quantile(0.5, []float64{10, 20, 30}, []uint64{5, 0, 5, 0})
	if !ok || v != 10 {
		t.Errorf("p50 = %v, %v", v, ok)
	}
	// +Inf bucket에 걸리면 가장 큰 유한 경계
	if v, ok := Quantile(0.99, []float64{10}, []uint64{1, 99}); !ok || v != 10 {
		t.Errorf("p99 in +Inf = %v", v)
	}
}

func uniformHistogram(bounds []float64, perBucket uint64) HistogramState {
	buckets := make([]uint64, len(bounds)+1)
	for i := range bounds {
		buckets[i] = perBucket
	}
	return HistogramState{Bounds: bounds, Buckets: buckets, Count: perBucket * uint64(len(bounds))}
}

// 알려진 분포: 0~100 균등 → p95 ≈ 95 (선형 보간 오차 1 이내).
func TestKnownDistributionP95(t *testing.T) {
	h := uniformHistogram([]float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}, 100)
	v, ok := Quantile(0.95, h.Bounds, h.Buckets)
	if !ok || math.Abs(v-95) > 1 {
		t.Fatalf("p95 = %v", v)
	}
}

// percentile을 평균하면 반드시 실패하는 fixture (D06 §04, CLAUDE.md 계약 5).
// instance A: 요청 1,000개가 0~10ms, instance B: 요청 10개가 900~1000ms.
// 실제 합산 p95는 10ms 근처인데, p95를 평균하면 ~500ms가 된다.
func TestPercentileAveragingFails(t *testing.T) {
	bounds := []float64{10, 100, 900, 1000}
	a := HistogramState{Bounds: bounds, Buckets: []uint64{1000, 0, 0, 0, 0}, Count: 1000}
	b := HistogramState{Bounds: bounds, Buckets: []uint64{0, 0, 0, 10, 0}, Count: 10}
	merged, err := Merge(a, b)
	if err != nil {
		t.Fatal(err)
	}
	truth, _ := Quantile(0.95, merged.Bounds, merged.Buckets)
	if truth > 10 {
		t.Fatalf("merged p95 = %v, want <= 10ms", truth)
	}
	pa, _ := Quantile(0.95, a.Bounds, a.Buckets)
	pb, _ := Quantile(0.95, b.Bounds, b.Buckets)
	averaged := (pa + pb) / 2
	if math.Abs(averaged-truth) < 100 {
		t.Fatalf("fixture too weak: averaged %v vs truth %v — averaging must be clearly wrong", averaged, truth)
	}
}

func TestUnsupportedTypes(t *testing.T) {
	for _, typ := range []Type{Summary, ExponentialHistogram} {
		a := Compute(Stream{Type: typ}, win, nil, []Point{{End: at(0)}})
		if !a.Partial || !slices.Contains(a.Flags, FlagUnsupported) {
			t.Errorf("%v = %+v", typ, a)
		}
	}
}
