package alerting

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/metricvalue"
	"github.com/polynomeer/montracer/internal/monitor"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// 1분 count bucket (stream 1개, window 1개)
func minuteCount(svc, status string, at time.Time, n uint64) telemetrystore.MetricBucket {
	return telemetrystore.MetricBucket{Group: []string{svc, status}, StepStart: at, Types: []string{"histogram"}, Units: []string{"s"},
		Windows: 1, Streams: 1, HistogramWindows: 1, Count: n, BoundsVariants: 1, Bounds: []float64{0.5}, Buckets: []uint64{n, 0}}
}

func dryRun(t *testing.T, s monitor.Spec, p DryRunPlan, bs []telemetrystore.MetricBucket) DryRunResult {
	t.Helper()
	r, err := DryRun(context.Background(), s, p, bs)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPlanDryRun(t *testing.T) {
	s := spec(t, errorRatioSpec) // for 120초 → warm-up 2분
	now := t0.Add(30 * time.Second)
	budget := telemetrystore.Budget{MaxResultRows: 200_000, MaxExecutionTime: 10 * time.Second}
	p := PlanDryRun(s, now, t0.Add(-3*time.Minute), budget)
	last := t0.Add(-3 * time.Minute)
	// 마지막 시점은 min(지금, watermark) 분 경계, 결과는 24시간(1,440시점), 그 앞 warm-up 2시점
	if len(p.Ends) != 1442 || !p.Ends[1441].Equal(last) || !p.ReportFrom.Equal(last.Add(-1439*time.Minute)) ||
		!p.Ends[0].Equal(last.Add(-1441*time.Minute)) || p.Stale {
		t.Fatalf("ends %d [%v..%v] report from %v", len(p.Ends), p.Ends[0], p.Ends[len(p.Ends)-1], p.ReportFrom)
	}
	// 첫 시점의 window 시작부터 마지막 시점까지 1분 step, error_ratio는 status를 group에 더한다, 예산을 싣는다
	q := p.Queries[0]
	if len(p.Queries) != 1 || q.StepSeconds != 60 || q.Window != time.Minute || !q.Range.From.Equal(p.Ends[0].Add(-5*time.Minute)) ||
		!q.Range.To.Equal(last) || len(q.GroupBy) != 2 || q.GroupBy[1] != StatusKey || len(q.Filters) != 1 || q.Budget != budget {
		t.Errorf("query = %+v", q)
	}
	// 5분 주기는 288시점(+warm-up), 30초 주기는 1분 간격(1분 rollup을 다시 읽는 반복 평가는 뺀다)
	s.ForSeconds = 0
	s.EvaluationSeconds = 300
	if p := PlanDryRun(s, now, now, budget); len(p.Ends) != 288 || p.Step != 5*time.Minute || !p.ReportFrom.Equal(p.Ends[0]) {
		t.Errorf("300s: %d %v", len(p.Ends), p.Step)
	}
	s.EvaluationSeconds = 30
	if p := PlanDryRun(s, now, now, budget); len(p.Ends) != 1440 || p.Step != time.Minute {
		t.Errorf("30s: %d %v", len(p.Ends), p.Step)
	}
	// warm-up은 for_seconds와 no_data alert after_seconds 중 큰 값
	after := 3600
	s.ForSeconds, s.NoData.Action, s.NoData.AfterSeconds = 600, monitor.NoDataAlert, &after
	if w := DryRunWarmup(s); w != time.Hour {
		t.Errorf("warmup = %v", w)
	}
	// 긴 window·warm-up은 점 수 상한으로 조회를 나눈다(빈틈·겹침 없이)
	s.WindowSeconds, s.ForSeconds, s.NoData.Action, s.NoData.AfterSeconds = 86400, 86400, monitor.NoDataState, nil
	p = PlanDryRun(s, now, now, budget)
	for i, q := range p.Queries {
		if q.Range.To.Sub(q.Range.From) > time.Duration(telemetrystore.MaxPointsPerSeries)*time.Minute {
			t.Errorf("chunk too long: %v", q.Range)
		}
		if i > 0 && !p.Queries[i-1].Range.To.Equal(q.Range.From) {
			t.Errorf("gap between chunks %d", i)
		}
	}
	if len(p.Queries) != 3 || !p.Queries[2].Range.To.Equal(p.Ends[len(p.Ends)-1]) || len(p.Ends) != 2880 {
		t.Errorf("chunks %d ends %d", len(p.Queries), len(p.Ends))
	}
	// watermark 없음 → 평가 안 함, 멈춤 → 표시
	if p := PlanDryRun(s, now, time.Time{}, budget); p.Reason != ReasonNoWatermark || len(p.Ends) != 0 || len(p.Queries) != 0 {
		t.Errorf("no watermark: %+v", p)
	}
	if p := PlanDryRun(s, now, now.Add(-time.Hour), budget); !p.Stale {
		t.Error("stale watermark not flagged")
	}
}

// 1분 bucket을 합친 값은 window bucket과 같다: 합·min·max·histogram 원소, 경계가 다르면 병합 불가, 빠진 분은 partial
func TestMergeBuckets(t *testing.T) {
	a := telemetrystore.MetricBucket{Group: []string{"x"}, StepStart: t0, Types: []string{"histogram"}, Units: []string{"s"}, Windows: 2, Streams: 2,
		ValueWindows: 2, Total: 10, Samples4Avg: 4, Min: 1, Max: 5, Last: 3, HistogramWindows: 2, Count: 4, HistSum: 2,
		BoundsVariants: 1, Bounds: []float64{0.1, 1}, Buckets: []uint64{1, 2, 1}, Flags: []string{"reset"}}
	b := a
	b.StepStart, b.Min, b.Max, b.Last, b.Buckets, b.Flags = t0.Add(time.Minute), 0.5, 4, 7, []uint64{0, 3, 1}, nil
	b.Windows, b.HistogramWindows = 1, 1 // stream 하나가 이 분에 빠졌다
	m := MergeBuckets([]telemetrystore.MetricBucket{a, b})
	if m.Total != 20 || m.Samples4Avg != 8 || m.Min != 0.5 || m.Max != 5 || m.Last != 7 || m.Count != 8 || m.HistSum != 4 ||
		m.BoundsVariants != 1 || m.Buckets[0] != 1 || m.Buckets[1] != 5 || m.Buckets[2] != 2 || m.Streams != 2 || len(m.Flags) != 1 {
		t.Errorf("merged = %+v", m)
	}
	if v, r := metricvalue.Value("avg", m, 2*time.Minute); r != "" || v != 2.5 {
		t.Errorf("avg = %v %s", v, r)
	}
	if !metricvalue.MissingWindows("count", m, 2) {
		t.Error("missing minute not partial")
	}
	// 입력 bucket을 바꾸지 않는다
	if a.Buckets[1] != 2 {
		t.Error("merge mutated input")
	}
	c := b
	c.Bounds = []float64{0.2, 1}
	if m := MergeBuckets([]telemetrystore.MetricBucket{a, c}); m.BoundsVariants == 1 {
		t.Error("different bounds merged")
	}
	if _, r := metricvalue.Value("p95", MergeBuckets([]telemetrystore.MetricBucket{a, c}), time.Minute); r != metricvalue.ReasonBoundsMismatch {
		t.Errorf("p95 reason = %s", r)
	}
}

// 24시간 중 한 시간만 5%: 사건 하나(시작·끝·발화 시간), 정확히 2%인 다른 group은 발화하지 않는다.
// 최소 요청 미달 시점은 결측으로 센다(발화 없음과 섞지 않는다).
func TestDryRunErrorRatio(t *testing.T) {
	s := spec(t, errorRatioSpec) // window 5분, for 120초, 복구 2회
	now := t0
	p := PlanDryRun(s, now, now, telemetrystore.Budget{})
	var bs []telemetrystore.MetricBucket
	spikeFrom, spikeTo := t0.Add(-10*time.Hour), t0.Add(-9*time.Hour)
	for at := p.Queries[0].Range.From; at.Before(p.Queries[0].Range.To); at = at.Add(time.Minute) {
		errs := uint64(0)
		if !at.Before(spikeFrom) && at.Before(spikeTo) {
			errs = 50
		}
		bs = append(bs, minuteCount("checkout", "200", at, 1000-errs), minuteCount("checkout", "500", at, errs))
		bs = append(bs, minuteCount("cart", "200", at, 980), minuteCount("cart", "503", at, 20))
		if at.Before(t0.Add(-20 * time.Hour)) { // 저트래픽 서비스: 처음 4시간만, 최소 요청(100) 미달
			bs = append(bs, minuteCount("search", "200", at, 5))
		}
	}
	r := dryRun(t, s, p, bs)
	if r.Evaluations != 1440 || r.Coverage.Evaluated != 1440 || r.Coverage.NoData != 0 || r.Reason != "" || r.GroupsTotal != 3 {
		t.Fatalf("result = %+v", r)
	}
	checkout := r.Groups[0]
	if checkout.Labels["service.name"] != "checkout" || len(checkout.Intervals) != 1 {
		t.Fatalf("groups = %+v", r.Groups)
	}
	iv := checkout.Intervals[0]
	// 5분 window의 오류율이 2%를 넘는 첫 window 끝(spike 시작 + 2분: 50×2/5000 = 2%는 아직 아님, 3분째 3%) + for 120초
	wantStart := spikeFrom.Add(3*time.Minute + 2*time.Minute)
	// spike가 끝나고 window 오류율이 2% 이하가 된 뒤 2회 연속 정상
	if !iv.Start.Equal(wantStart) || iv.End == nil || iv.Reason != "violation" || checkout.FiringSeconds != int64(iv.End.Sub(iv.Start)/time.Second) {
		t.Errorf("interval = %+v end=%v firing=%d", iv, iv.End, checkout.FiringSeconds)
	}
	if iv.End.Before(spikeTo) || iv.End.After(spikeTo.Add(10*time.Minute)) {
		t.Errorf("interval end %v not shortly after spike end %v", iv.End, spikeTo)
	}
	if checkout.MaxValue == nil || *checkout.MaxValue < 0.0499 || checkout.FinalState != StateOK {
		t.Errorf("checkout = %+v", checkout)
	}
	var cart, search DryRunGroup
	for _, g := range r.Groups {
		switch g.Labels["service.name"] {
		case "cart":
			cart = g
		case "search":
			search = g
		}
	}
	if len(cart.Intervals) != 0 || cart.FiringSeconds != 0 || cart.Evaluated != 1440 || *cart.MaxValue != 0.02 {
		t.Errorf("cart (exactly 2%%) = %+v", cart)
	}
	// search: 값이 한 번도 없다(최소 요청 미달 → 결측), 데이터가 끊긴 뒤도 결측(group_missing) — 발화 없음이지만 평가된 적도 없다
	if search.Evaluated != 0 || search.NoData != 1440 || search.FirstValueAt != nil || search.FinalState != StateNoData || search.LastReason != ReasonGroupMissing {
		t.Errorf("search = %+v", search)
	}
}

func TestDryRunWithoutWatermark(t *testing.T) {
	s := spec(t, errorRatioSpec)
	r := dryRun(t, s, PlanDryRun(s, t0, time.Time{}, telemetrystore.Budget{}), nil)
	if r.Evaluations != 0 || r.Reason != ReasonNoWatermark || r.From != nil || r.DataUntil != nil || r.Groups == nil {
		t.Errorf("result = %+v", r)
	}
	// 데이터가 없는 24시간: 평가는 하지만 group_by가 있으면 group이 없다 — reason no_data로 '발화 없음'과 구분한다
	r = dryRun(t, s, PlanDryRun(s, t0, t0, telemetrystore.Budget{}), nil)
	if r.Evaluations != 1440 || r.GroupsTotal != 0 || r.Reason != metricvalue.ReasonNoData {
		t.Errorf("empty = %+v", r)
	}
}

func TestDryRunGroupCap(t *testing.T) {
	s := spec(t, errorRatioSpec)
	s.WindowSeconds = 60
	p := PlanDryRun(s, t0, t0, telemetrystore.Budget{})
	var bs []telemetrystore.MetricBucket
	at := p.Ends[len(p.Ends)-1].Add(-time.Minute)
	for i := 0; i < DryRunMaxGroups+5; i++ {
		bs = append(bs, minuteCount(string(rune('a'+i%26))+string(rune('a'+i/26)), "200", at, 1000))
	}
	r := dryRun(t, s, p, bs)
	if r.GroupsTotal != DryRunMaxGroups+5 || len(r.Groups) != DryRunMaxGroups || r.GroupsOmitted != 5 {
		t.Errorf("total=%d returned=%d omitted=%d", r.GroupsTotal, len(r.Groups), r.GroupsOmitted)
	}
}

func everyMinute(p DryRunPlan, f func(at time.Time) []telemetrystore.MetricBucket) []telemetrystore.MetricBucket {
	var bs []telemetrystore.MetricBucket
	for _, q := range p.Queries {
		for at := q.Range.From; at.Before(q.Range.To); at = at.Add(time.Minute) {
			bs = append(bs, f(at)...)
		}
	}
	return bs
}

// 계속 위반 중인 monitor: warm-up 덕에 결과 범위 첫 시점부터 발화 중이다(처음 상태 OK 때문에 for_seconds만큼 늦게 보이지 않는다).
// (warm-up 없이는 첫 시점부터 for 3600초가 지나야 열린다.) 발화 시간은 범위 안만 센다.
func TestDryRunWarmup(t *testing.T) {
	s := spec(t, errorRatioSpec)
	s.ForSeconds = 3600
	p := PlanDryRun(s, t0, t0, telemetrystore.Budget{})
	bs := everyMinute(p, func(at time.Time) []telemetrystore.MetricBucket {
		return []telemetrystore.MetricBucket{minuteCount("checkout", "200", at, 900), minuteCount("checkout", "500", at, 100)}
	})
	r := dryRun(t, s, p, bs)
	g := r.Groups[0]
	if r.Evaluations != 1440 || r.WarmupSeconds != 3600 || len(g.Intervals) != 1 || g.Intervals[0].Start.After(*r.From) ||
		g.Intervals[0].End != nil || g.FiringSeconds != int64(r.To.Sub(*r.From)/time.Second) {
		t.Errorf("result = %+v group = %+v", r, g)
	}
	// warm-up 중에 이미 열린 사건: 구간 시작은 실제로 열린 시각(범위 앞)이고 발화 시간은 범위 안만 센다
	after := 3600
	s.ForSeconds, s.NoData.Action, s.NoData.AfterSeconds = 60, monitor.NoDataAlert, &after
	p = PlanDryRun(s, t0, t0, telemetrystore.Budget{})
	r = dryRun(t, s, p, everyMinute(p, func(at time.Time) []telemetrystore.MetricBucket {
		return []telemetrystore.MetricBucket{minuteCount("checkout", "200", at, 900), minuteCount("checkout", "500", at, 100)}
	}))
	g = r.Groups[0]
	if len(g.Intervals) != 1 || !g.Intervals[0].Start.Before(*r.From) || g.FiringSeconds != int64(r.To.Sub(*r.From)/time.Second) {
		t.Errorf("episode from warm-up = %+v", g)
	}
}

// flapping: 구간은 50개까지만 담고 나머지는 센다. 발화 시간은 생략한 구간도 포함한다.
func TestDryRunIntervalCap(t *testing.T) {
	s := spec(t, errorRatioSpec)
	s.WindowSeconds, s.ForSeconds = 60, 0
	p := PlanDryRun(s, t0, t0, telemetrystore.Budget{})
	bs := everyMinute(p, func(at time.Time) []telemetrystore.MetricBucket {
		errs := uint64(0)
		if at.Minute()%10 < 3 { // 10분마다 3분 위반
			errs = 100
		}
		return []telemetrystore.MetricBucket{minuteCount("checkout", "200", at, 1000-errs), minuteCount("checkout", "500", at, errs)}
	})
	g := dryRun(t, s, p, bs).Groups[0]
	if len(g.Intervals) != DryRunMaxIntervals || g.IntervalsOmitted != 144-DryRunMaxIntervals || g.FiringSeconds < 144*3*60 {
		t.Errorf("intervals %d omitted %d firing %d", len(g.Intervals), g.IntervalsOmitted, g.FiringSeconds)
	}
}

func TestDryRunCanceled(t *testing.T) {
	s := spec(t, errorRatioSpec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DryRun(ctx, s, PlanDryRun(s, t0, t0, telemetrystore.Budget{}), nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

// 분마다 stream 집합이 다르면(pod 교체) 어느 분엔 빠진 stream이 있다 → partial. 같으면 아니다.
// group 값에 구분 문자가 있어도 다른 group이 섞이지 않는다.
func TestMergeStreamSetAndSeriesKey(t *testing.T) {
	a := minuteCount("x", "200", t0, 10)
	b := minuteCount("x", "200", t0.Add(time.Minute), 10)
	a.StreamSet, b.StreamSet = 1, 1
	if m := MergeBuckets([]telemetrystore.MetricBucket{a, b}); metricvalue.MissingWindows("count", m, 2) {
		t.Error("same streams marked partial")
	}
	b.StreamSet = 2
	m := MergeBuckets([]telemetrystore.MetricBucket{a, b})
	if !metricvalue.MissingWindows("count", m, 2) {
		t.Error("stream rotation not partial for histogram count")
	}
	// gauge 값 통계는 저장소 조회처럼 stream 교체로 partial이 되지 않는다
	if m.Partial || metricvalue.MissingWindows("avg", m, 2) {
		t.Error("stream rotation marked gauge partial")
	}
	if seriesKey([]string{"a\x00b", "c"}) == seriesKey([]string{"a", "b\x00c"}) || seriesKey([]string{"ab", ""}) == seriesKey([]string{"a", "b"}) {
		t.Error("series keys collide")
	}
}
