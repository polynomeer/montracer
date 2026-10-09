package alerting

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/polynomeer/montracer/internal/metricvalue"
	"github.com/polynomeer/montracer/internal/monitor"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// 24시간 dry-run (D05 §09, ADR 0052): 지난 24시간을 평가 주기마다 다시 평가해 후보 발화 구간과 데이터 coverage를 보여준다.
// 주기 평가와 같은 함수(Compute·Evaluate)를 쓴다. 조회는 1분 bucket을 한 번에 받고 평가 시점마다 window만큼 합친다
// (평가 시점마다 조회하면 1,440번이다). 미래 알림 횟수의 예측이 아니다.

const (
	// DryRunRange는 다시 평가할 기간이다(평가 시점 = window 끝이 이 안에 있다).
	DryRunRange = 24 * time.Hour
	// DryRunMaxGroups는 응답에 담는 group 수다. 발화 시간이 긴 순이고 나머지는 수만 센다.
	DryRunMaxGroups = 100
	// DryRunMaxBuckets는 받는 1분 bucket 수의 상한이다(조회 비용·응답 시간). 넘으면 dry-run을 하지 않는다.
	DryRunMaxBuckets = 200_000
	// DryRunMaxMerges는 평가 시점 × window의 bucket 합치기 횟수 상한이다(긴 window × 많은 series).
	DryRunMaxMerges = 10_000_000
)

// DryRunPlan은 dry-run에 필요한 조회와 평가 시점이다.
type DryRunPlan struct {
	Queries []telemetrystore.MetricQuery // 1분 step. 한 조회의 점 수 상한(MaxPointsPerSeries)을 넘지 않게 나눈다
	Ends    []time.Time                  // 평가 시점(window 끝), 오름차순. warm-up 시점을 포함한다
	// ReportFrom은 결과에 담는 첫 시점이다. 그 전 시점(warm-up)은 상태만 만든다 —
	// 처음 상태를 OK로 두면 for_seconds·no_data after_seconds 동안 발화할 수 없어 "발화 없음"처럼 보이기 때문이다.
	ReportFrom time.Time
	Step       time.Duration // 평가 간격(30초 주기는 1분 rollup을 다시 읽으므로 1분)
	// Reason은 평가할 window가 없는 이유다(no_watermark). 있으면 Queries·Ends가 비어 있다.
	Reason string
	// Stale은 watermark가 StaleAfter보다 오래 멈췄다는 뜻이다. 주기 평가라면 지금은 결측(stale_watermark)이다.
	Stale     bool
	DataUntil time.Time
}

// DryRunWarmup은 결과 앞에 상태만 만드는 기간이다: for_seconds와 no_data after_seconds 중 큰 값.
func DryRunWarmup(s monitor.Spec) time.Duration {
	w := s.ForSeconds
	if s.NoData.Action == monitor.NoDataAlert && s.NoData.AfterSeconds != nil {
		w = max(w, *s.NoData.AfterSeconds)
	}
	return time.Duration(w) * time.Second
}

// PlanDryRun은 지금·watermark에서 평가 시점과 조회를 정한다. 마지막 시점은 주기 평가의 window 끝과 같다(min(지금, watermark) 분 경계).
// budget은 조회마다 보내는 실행 예산이다(query 계정 interactive 기본값 10,000행으로는 series 7개를 넘지 못한다).
func PlanDryRun(s monitor.Spec, now, watermark time.Time, budget telemetrystore.Budget) DryRunPlan {
	step := time.Duration(max(s.EvaluationSeconds, 60)) * time.Second
	p := DryRunPlan{Step: step, DataUntil: watermark}
	if watermark.IsZero() {
		p.Reason = ReasonNoWatermark
		return p
	}
	p.Stale = now.Sub(watermark) > StaleAfter
	last := now
	if watermark.Before(last) {
		last = watermark
	}
	last = last.UTC().Truncate(time.Minute)
	first := last.Add(-DryRunRange - DryRunWarmup(s))
	for t := last; t.After(first); t = t.Add(-step) {
		p.Ends = append(p.Ends, t)
		if t.After(last.Add(-DryRunRange)) {
			p.ReportFrom = t
		}
	}
	slices.SortFunc(p.Ends, func(a, b time.Time) int { return a.Compare(b) })
	window := time.Duration(s.WindowSeconds) * time.Second
	base := Query(s, Window{Start: p.Ends[0].Add(-window), End: last})
	base.StepSeconds, base.Window, base.Budget = 60, time.Minute, budget
	chunk := time.Duration(telemetrystore.MaxPointsPerSeries) * time.Minute
	for from := base.Range.From; from.Before(last); from = from.Add(chunk) {
		q := base
		q.GroupBy = slices.Clone(base.GroupBy)
		to := from.Add(chunk)
		if to.After(last) {
			to = last
		}
		q.Range = telemetrystore.TimeRange{From: from, To: to}
		p.Queries = append(p.Queries, q)
	}
	return p
}

// DryRunInterval은 후보 발화 구간(사건 하나)이다. End가 nil이면 dry-run 끝까지 열려 있다.
// Start가 결과 범위(from)보다 이르면 warm-up 중에 열린 사건이다.
type DryRunInterval struct {
	Start  time.Time  `json:"start"`
	End    *time.Time `json:"end"`
	Reason string     `json:"reason"` // violation | no_data
}

// DryRunGroup은 group 하나의 dry-run 결과다.
type DryRunGroup struct {
	Key              string            `json:"group_key"`
	Labels           map[string]string `json:"labels"`
	Intervals        []DryRunInterval  `json:"firing_intervals"` // 최대 DryRunMaxIntervals개(앞에서부터)
	IntervalsOmitted int               `json:"firing_intervals_omitted"`
	FiringSeconds    int64             `json:"firing_seconds"` // 결과 범위 안의 발화 시간(생략한 구간 포함)
	FinalState       State             `json:"final_state"`
	Evaluated        int               `json:"evaluated"`      // 값으로 판정한 시점 수
	NoData           int               `json:"no_data"`        // 값이 없던 시점 수(최소 요청 미달 포함)
	Partial          int               `json:"partial"`        // 일부 window만으로 계산한 시점 수
	FirstValueAt     *time.Time        `json:"first_value_at"` // 처음 값이 있던 window 끝
	MaxValue         *float64          `json:"max_value"`      // 기간 중 가장 큰 값(임계값과 비교해 보기)
	LastValue        *float64          `json:"last_value"`     // 마지막 시점의 값
	LastReason       string            `json:"last_reason"`    // 마지막 시점에 값이 없던 이유
	Transitions      int               `json:"transitions"`    // 상태 전이 수(사건 열림·닫힘 포함)
}

// DryRunMaxIntervals는 group 하나에 담는 구간 수다(flapping이면 수백 개가 된다).
const DryRunMaxIntervals = 50

// DryRunResult는 dry-run 결과다. Evaluations가 0이면 Reason이 있다.
type DryRunResult struct {
	From          *time.Time     `json:"from"` // 첫 평가 시점(window 끝)
	To            *time.Time     `json:"to"`   // 마지막 평가 시점
	WarmupSeconds int            `json:"warmup_seconds"`
	DataUntil     *time.Time     `json:"data_until"`
	StepSeconds   int            `json:"step_seconds"`
	Evaluations   int            `json:"evaluations"`
	Coverage      DryRunCoverage `json:"coverage"`
	Groups        []DryRunGroup  `json:"groups"`
	GroupsTotal   int            `json:"groups_total"`
	GroupsOmitted int            `json:"groups_omitted"`
	Reason        string         `json:"reason"` // no_watermark | stale_watermark | no_data(series 없음) | ""
}

// DryRunCoverage는 monitor 수준 평가 결과의 분포다. 결측·오류를 발화 없음과 섞지 않는다(계약 6).
type DryRunCoverage struct {
	Evaluated int `json:"evaluated"`
	NoData    int `json:"no_data"`
	Error     int `json:"error"` // group 상한 초과
}

// DryRun은 1분 bucket으로 평가 시점마다 window를 합쳐 다시 평가한다. ReportFrom 전(warm-up)은 상태만 만든다.
// ctx가 끝나면(시간 상한) 멈추고 ctx 오류를 돌려준다.
func DryRun(ctx context.Context, s monitor.Spec, p DryRunPlan, minute []telemetrystore.MetricBucket) (DryRunResult, error) {
	res := DryRunResult{StepSeconds: int(p.Step / time.Second), Reason: p.Reason, Groups: []DryRunGroup{},
		WarmupSeconds: int(DryRunWarmup(s) / time.Second)}
	if !p.DataUntil.IsZero() {
		d := p.DataUntil.UTC()
		res.DataUntil = &d
	}
	if p.Stale {
		res.Reason = ReasonStaleWatermark
	}
	if len(p.Ends) == 0 {
		return res, nil
	}
	from, to := p.ReportFrom, p.Ends[len(p.Ends)-1]
	res.From, res.To = &from, &to

	if len(minute) == 0 && res.Reason == "" {
		// 기간 동안 series가 하나도 없다: "발화 없음"이 아니라 데이터 없음이다(계약 6)
		res.Reason = metricvalue.ReasonNoData
	}
	window := time.Duration(s.WindowSeconds) * time.Second
	series := splitSeries(minute)
	prev := map[string]Instance{}
	labels := map[string]map[string]string{}
	groups := map[string]*DryRunGroup{}
	type openEpisode struct {
		start  time.Time
		reason string
		index  int // Intervals 안 위치, 생략했으면 -1
	}
	open := map[string]openEpisode{}
	group := func(g GroupEval) *DryRunGroup {
		r := groups[g.Key]
		if r == nil {
			r = &DryRunGroup{Key: g.Key, Labels: g.Labels, Intervals: []DryRunInterval{}}
			groups[g.Key] = r
		}
		return r
	}
	for i, end := range p.Ends {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return DryRunResult{}, err
			}
		}
		report := !end.Before(from)
		start := end.Add(-window)
		var merged []telemetrystore.MetricBucket
		for _, sr := range series {
			if b, ok := sr.window(start, end); ok {
				merged = append(merged, b)
			}
		}
		ev := Evaluate(s, prev, labels, Input{Window: Window{Start: start, End: end}, WindowStatus: WindowOK, Result: Compute(s, merged)}, end)
		if report {
			res.Evaluations++
			switch ev.Status {
			case MonitorEvaluated:
				res.Coverage.Evaluated++
			case MonitorNoData:
				res.Coverage.NoData++
			default:
				res.Coverage.Error++
			}
		}
		for _, g := range ev.Groups {
			prev[g.Key], labels[g.Key] = g.Next, g.Labels
			t := g.Transition
			if t.Closed {
				if o, ok := open[g.Key]; ok {
					delete(open, g.Key)
					if report {
						r := group(g)
						r.FiringSeconds += int64(end.Sub(maxTime(o.start, from)) / time.Second)
						if o.index >= 0 {
							e := end
							r.Intervals[o.index].End = &e
						}
					}
				}
			}
			if t.Opened {
				open[g.Key] = openEpisode{start: end, reason: g.Next.EpisodeReason, index: -1}
			}
			if !report {
				continue
			}
			r := group(g)
			// warm-up 중에 열린 사건이 범위 첫 시점에 이어지면 그때 구간으로 담는다(시작은 실제로 열린 시각)
			if o, ok := open[g.Key]; ok && o.index < 0 && (t.Opened || end.Equal(from)) {
				if len(r.Intervals) < DryRunMaxIntervals {
					r.Intervals = append(r.Intervals, DryRunInterval{Start: o.start, Reason: o.reason})
					o.index = len(r.Intervals) - 1
				} else {
					r.IntervalsOmitted++
					o.index = -2 // 생략(다시 담지 않는다)
				}
				open[g.Key] = o
			}
			r.FinalState = g.Next.State
			if g.Value != nil {
				v := *g.Value
				r.Evaluated++
				r.LastValue, r.LastReason = &v, ""
				if r.FirstValueAt == nil {
					e := end
					r.FirstValueAt = &e
				}
				if r.MaxValue == nil || v > *r.MaxValue {
					r.MaxValue = &v
				}
			} else {
				r.LastValue, r.LastReason = nil, g.Reason
				if g.Outcome == OutcomeNoData {
					r.NoData++
				}
			}
			if g.Partial {
				r.Partial++
			}
			if !t.Repeated && (t.From != t.To || t.Opened || t.Closed) {
				r.Transitions++
			}
		}
	}
	for k, o := range open {
		if r := groups[k]; r != nil {
			r.FiringSeconds += int64(to.Sub(maxTime(o.start, from)) / time.Second)
		}
	}
	all := make([]DryRunGroup, 0, len(groups))
	for _, g := range groups {
		all = append(all, *g)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].FiringSeconds != all[j].FiringSeconds {
			return all[i].FiringSeconds > all[j].FiringSeconds
		}
		return all[i].Key < all[j].Key
	})
	res.GroupsTotal = len(all)
	if len(all) > DryRunMaxGroups {
		res.GroupsOmitted = len(all) - DryRunMaxGroups
		all = all[:DryRunMaxGroups]
	}
	res.Groups = all
	return res, nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// DryRunMerges는 DryRun이 할 bucket 합치기 횟수의 상한 추정이다(평가 시점 × window 분 × series).
func DryRunMerges(s monitor.Spec, p DryRunPlan, minute []telemetrystore.MetricBucket) int {
	return len(p.Ends) * (s.WindowSeconds / 60) * len(splitSeries(minute))
}

// series는 같은 group 값(error_ratio는 status 포함)의 1분 bucket이다(시간순).
type series struct{ buckets []telemetrystore.MetricBucket }

func splitSeries(bs []telemetrystore.MetricBucket) []series {
	idx := map[string]int{}
	var out []series
	for _, b := range bs {
		k := seriesKey(b.Group)
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, series{})
		}
		out[i].buckets = append(out[i].buckets, b)
	}
	for i := range out {
		slices.SortFunc(out[i].buckets, func(a, b telemetrystore.MetricBucket) int { return a.StepStart.Compare(b.StepStart) })
	}
	return out
}

// seriesKey는 group 값 목록의 key다. 값에 구분 문자가 들어 있어도 다른 group이 섞이지 않게 길이를 앞에 붙인다.
func seriesKey(vs []string) string {
	var b strings.Builder
	for _, v := range vs {
		b.WriteString(strconv.Itoa(len(v)))
		b.WriteByte(':')
		b.WriteString(v)
	}
	return b.String()
}

func (s series) window(start, end time.Time) (telemetrystore.MetricBucket, bool) {
	lo := sort.Search(len(s.buckets), func(i int) bool { return !s.buckets[i].StepStart.Before(start) })
	hi := sort.Search(len(s.buckets), func(i int) bool { return !s.buckets[i].StepStart.Before(end) })
	if lo >= hi {
		return telemetrystore.MetricBucket{}, false
	}
	b := MergeBuckets(s.buckets[lo:hi])
	b.StepStart = start
	return b, true
}

// MergeBuckets는 같은 group의 1분 bucket들을 window 하나로 합친다. 저장소가 window step으로 합친 값과 같다.
//   - 합(증가량·count·hist_sum·total·samples)은 더하고, min·max는 값이 있는 bucket끼리, histogram bucket은 경계가 모두 같을 때만 원소별로 더한다
//     (다르면 BoundsVariants > 1 → bounds_mismatch, 계약 5).
//   - Streams는 분마다 센 수의 최대다. 분마다 stream 집합(StreamSet 지문)이 다르면 어느 분에 빠진 stream이 있다는 뜻이라
//     Streams를 하나 늘려 "stream 수 × 분 수보다 window가 적음"(metricvalue.MissingWindows)이 되게 한다. 저장소 조회의
//     partial과 같은 집계(증가량·histogram)에서만 partial이 되고 gauge 값 통계에는 영향이 없다(ADR 0052).
func MergeBuckets(bs []telemetrystore.MetricBucket) telemetrystore.MetricBucket {
	var out telemetrystore.MetricBucket
	if len(bs) == 0 {
		return out
	}
	out.Group, out.StepStart, out.StreamSet = bs[0].Group, bs[0].StepStart, bs[0].StreamSet
	types, units, flags := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var bounds []float64
	boundsSet, lastAt, streamsChanged := false, time.Time{}, false
	for _, b := range bs {
		for _, t := range b.Types {
			types[t] = true
		}
		for _, u := range b.Units {
			units[u] = true
		}
		for _, f := range b.Flags {
			flags[f] = true
		}
		out.Windows += b.Windows
		out.Streams = max(out.Streams, b.Streams)
		out.Samples += b.Samples
		if b.ValueWindows > 0 {
			if out.ValueWindows == 0 || b.Min < out.Min {
				out.Min = b.Min
			}
			if out.ValueWindows == 0 || b.Max > out.Max {
				out.Max = b.Max
			}
			if !b.StepStart.Before(lastAt) {
				out.Last, lastAt = b.Last, b.StepStart
			}
			out.ValueWindows += b.ValueWindows
			out.Total += b.Total
			out.Samples4Avg += b.Samples4Avg
		}
		out.IncreaseWindows += b.IncreaseWindows
		out.Increase += b.Increase
		if b.HistogramWindows > 0 {
			out.HistogramWindows += b.HistogramWindows
			out.Count += b.Count
			out.HistSum += b.HistSum
			switch {
			case b.BoundsVariants != 1:
				out.BoundsVariants = 2
			case !boundsSet:
				bounds, boundsSet = b.Bounds, true
				out.Buckets = slices.Clone(b.Buckets)
				out.BoundsVariants = max(out.BoundsVariants, 1)
			case !slices.Equal(bounds, b.Bounds) || len(out.Buckets) != len(b.Buckets):
				out.BoundsVariants = 2
			default:
				for i := range b.Buckets {
					out.Buckets[i] += b.Buckets[i]
				}
			}
		}
		out.Partial = out.Partial || b.Partial
		streamsChanged = streamsChanged || b.StreamSet != out.StreamSet
	}
	if streamsChanged {
		out.Streams++
	}
	out.Bounds = bounds
	out.Types, out.Units, out.Flags = sortedSet(types), sortedSet(units), sortedSet(flags)
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
