package alerting

import (
	"fmt"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/metricvalue"
	"github.com/polynomeer/montracer/internal/monitor"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

var t0 = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func spec(t *testing.T, body string) monitor.Spec {
	t.Helper()
	s, _, err := monitor.Normalize([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const errorRatioSpec = `{"name":"오류율","query":{"kind":"error_ratio","filter":{"op":"and","args":[{"op":"eq","field":"deployment.environment.name","value":"prod"}]},
	"group_by":["service.name"]},"window_seconds":300,"evaluation_seconds":60,"condition":{"operator":"gt","threshold":0.02},
	"for_seconds":120,"no_data":{"action":"no_data"}}`

// count bucket: 5분 window를 빠짐없이 덮는다(stream 1개 × 5 window)
func count(svc, status string, n uint64) telemetrystore.MetricBucket {
	return telemetrystore.MetricBucket{Group: []string{svc, status}, StepStart: t0, Types: []string{"histogram"}, Units: []string{"s"},
		Windows: 5, Streams: 1, HistogramWindows: 5, Count: n, BoundsVariants: 1}
}

func minute(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }

func TestWindowAndQuery(t *testing.T) {
	s := spec(t, errorRatioSpec)
	if _, st := EvaluationWindow(s, t0, time.Time{}); st != WindowNone {
		t.Error("no watermark must not produce a window")
	}
	// 끝은 minute(now, watermark)의 분 경계: 확정 window만
	w, st := EvaluationWindow(s, t0, t0.Add(-90*time.Second))
	if st != WindowOK || !w.End.Equal(t0.Add(-2*time.Minute)) || !w.Start.Equal(t0.Add(-7*time.Minute)) {
		t.Errorf("window = %+v %v", w, st)
	}
	w, _ = EvaluationWindow(s, t0.Add(30*time.Second), t0.Add(time.Hour))
	if !w.End.Equal(t0) {
		t.Errorf("window end must not pass now: %+v", w)
	}
	// watermark가 10분 넘게 멈추면 stale(마지막 window를 계속 평가하지 않는다)
	if _, st := EvaluationWindow(s, t0, t0.Add(-11*time.Minute)); st != WindowStale {
		t.Errorf("stale watermark status = %v", st)
	}
	if _, st := EvaluationWindow(s, t0, t0.Add(-10*time.Minute)); st != WindowOK {
		t.Errorf("10 minutes behind is still fresh: %v", st)
	}
	q := Query(s, w)
	if fmt.Sprint(q.GroupBy) != "[service.name http.response.status_code]" || q.StepSeconds != 300 || q.Window != time.Minute ||
		q.Metric != monitor.DefaultErrorMetric || fmt.Sprint(q.Filters) != "[{deployment.environment.name prod}]" {
		t.Errorf("query = %+v", q)
	}
}

func TestComputeErrorRatio(t *testing.T) {
	s := spec(t, errorRatioSpec)
	partial := count("cart", "200", 900)
	partial.HistogramWindows = 3 // 5개 window 중 3개: 값이 작을 수 있다
	res := Compute(s, []telemetrystore.MetricBucket{
		count("checkout", "200", 980), count("checkout", "500", 20), // 정확히 2%
		count("payment", "200", 96), count("payment", "503", 3), // 99건(100 미만): 최소 요청 미달
		partial, count("cart", "404", 100), // 4xx는 오류가 아니다
	})
	by := map[string]GroupResult{}
	for _, g := range res.Groups {
		by[g.Labels["service.name"]] = g
	}
	if g := by["checkout"]; g.Value == nil || *g.Value != 0.02 || *g.Total != 1000 {
		t.Errorf("checkout = %+v", g)
	}
	// 2% 초과(gt) 조건: 정확히 2%는 위반이 아니다(D06 §05 경계 시험)
	if Judge(s, by["checkout"]) != OutcomeOK {
		t.Error("exactly the threshold must not violate a gt condition")
	}
	if g := by["payment"]; g.Value != nil || g.Reason != ReasonBelowMinimum || Judge(s, g) != OutcomeNoData {
		t.Errorf("payment below minimum = %+v (must be NO_DATA, not OK)", g)
	}
	if g := by["cart"]; g.Value == nil || *g.Value != 0 || !g.Partial {
		t.Errorf("cart = %+v", g)
	}
	if by["cart"].Key != `"service.name"="cart"` {
		t.Errorf("group key = %q", by["cart"].Key)
	}
}

func TestComputeErrorRatioDroppedAndUnknown(t *testing.T) {
	s := spec(t, errorRatioSpec)
	// 5xx bucket이 단위 충돌로 빠지면 남은 2xx만으로 "오류율 0"을 만들지 않는다(결측)
	conflict := count("checkout", "500", 50)
	conflict.Units = []string{"s", "ms"}
	res := Compute(s, []telemetrystore.MetricBucket{count("checkout", "200", 950), conflict})
	if g := res.Groups[0]; g.Value != nil || g.Reason != metricvalue.ReasonUnitConflict || Judge(s, g) != OutcomeNoData {
		t.Errorf("dropped 5xx bucket = %+v (must not judge OK)", g)
	}
	// status를 모르는 요청은 분모에 넣되 표시한다(오류율이 낮게 나올 수 있다)
	res = Compute(s, []telemetrystore.MetricBucket{count("checkout", "200", 900), count("checkout", "", 100)})
	if g := res.Groups[0]; g.Value == nil || *g.Value != 0 || !g.Partial || g.UnknownStatus == nil || *g.UnknownStatus != 100 {
		t.Errorf("unknown status = %+v", g)
	}
}

func TestComputeMetricAndLimits(t *testing.T) {
	s := spec(t, `{"name":"p95","query":{"kind":"metric","metric":"http.server.request.duration","aggregation":"p95"},
		"window_seconds":300,"evaluation_seconds":60,"condition":{"operator":"gt","threshold":0.5},"for_seconds":0,"no_data":{"action":"no_data"}}`)
	mixed := telemetrystore.MetricBucket{Group: []string{}, Types: []string{"histogram"}, Units: []string{"s"}, HistogramWindows: 5, Streams: 1, Count: 10, BoundsVariants: 2}
	res := Compute(s, []telemetrystore.MetricBucket{mixed})
	if len(res.Groups) != 1 || res.Groups[0].Value != nil || res.Groups[0].Reason != metricvalue.ReasonBoundsMismatch {
		t.Errorf("bounds mismatch = %+v", res.Groups)
	}
	// series가 하나도 없으면 group 하나가 데이터 없음(0이 아니다)
	if res := Compute(s, nil); len(res.Groups) != 1 || res.Groups[0].Reason != metricvalue.ReasonNoData {
		t.Errorf("no series = %+v", res.Groups)
	}
	// group 상한 초과면 일부만 평가하지 않는다
	g := spec(t, errorRatioSpec)
	var many []telemetrystore.MetricBucket
	for i := 0; i <= MaxGroups; i++ {
		many = append(many, count(fmt.Sprintf("svc-%04d", i), "200", 100))
	}
	if res := Compute(g, many); !res.GroupLimitExceeded || len(res.Groups) != 0 {
		t.Errorf("group limit: exceeded=%v groups=%d", res.GroupLimitExceeded, len(res.Groups))
	}
}

func TestViolatesBoundaries(t *testing.T) {
	for _, c := range []struct {
		op   string
		v    float64
		want bool
	}{
		{"gt", 1, false}, {"gt", 1.0001, true}, {"gte", 1, true}, {"gte", 0.9999, false},
		{"lt", 1, false}, {"lt", 0.9999, true}, {"lte", 1, true}, {"lte", 1.0001, false},
	} {
		if got := Violates(monitor.Condition{Operator: c.op, Threshold: 1}, c.v); got != c.want {
			t.Errorf("%s %v = %v", c.op, c.v, got)
		}
	}
}

// 데이터 판정: at = window 끝, now = 벽시계(테스트에서는 같은 분)
func data(s monitor.Spec, in Instance, o Outcome, m int) (Instance, Transition) {
	return Next(s, in, o, minute(m), minute(m))
}

func TestStateMachine(t *testing.T) {
	s := spec(t, errorRatioSpec) // for 120s, 평가 60s, 복구 2회
	var in Instance
	var tr Transition
	// 위반이 for_seconds(2분) 이어져야 ALERT: 0분 PENDING, 1분 PENDING, 2분 ALERT(사건 열림)
	in, tr = data(s, in, OutcomeViolating, 0)
	if in.State != StatePending || tr.From != StateOK || tr.Opened {
		t.Fatalf("t0: %+v %+v", in, tr)
	}
	in, _ = data(s, in, OutcomeViolating, 1)
	in, tr = data(s, in, OutcomeViolating, 2)
	if in.State != StateAlert || !tr.Opened || !in.EpisodeOpen || in.EpisodeReason != "violation" {
		t.Fatalf("t2: %+v %+v", in, tr)
	}
	// 정상 1번은 RECOVERING, 그사이 위반이면 같은 사건으로 ALERT(새로 열지 않음)
	in, tr = data(s, in, OutcomeOK, 3)
	if in.State != StateRecovering || tr.Closed {
		t.Fatalf("t3: %+v", in)
	}
	in, tr = data(s, in, OutcomeViolating, 4)
	if in.State != StateAlert || tr.Opened {
		t.Fatalf("t4: %+v %+v", in, tr)
	}
	// 결측은 사건을 닫지 않는다: NO_DATA지만 사건은 열려 있고, 이어진 정상은 복구 1회부터
	in, _ = data(s, in, OutcomeNoData, 5)
	if in.State != StateNoData || !in.EpisodeOpen {
		t.Fatalf("t5: %+v", in)
	}
	in, _ = data(s, in, OutcomeOK, 6)
	if in.State != StateRecovering || in.OKStreak != 1 {
		t.Fatalf("t6: %+v", in)
	}
	// RECOVERING 중 결측은 복구 연속을 끊는다
	in, _ = data(s, in, OutcomeNoData, 7)
	if in.OKStreak != 0 || !in.EpisodeOpen {
		t.Fatalf("t7: %+v", in)
	}
	// 조회 실패(window 없는 판정)도 복구 연속을 끊는다 — 실패로 사건이 닫히지 않는다
	in, _ = data(s, in, OutcomeOK, 8)
	in, _ = Next(s, in, OutcomeError, time.Time{}, minute(9))
	if in.State != StateEvaluationError || !in.EpisodeOpen || in.OKStreak != 0 {
		t.Fatalf("t9: %+v", in)
	}
	in, _ = data(s, in, OutcomeOK, 10)
	in, tr = data(s, in, OutcomeOK, 11)
	if in.State != StateOK || !tr.Closed || in.EpisodeOpen {
		t.Fatalf("t11: %+v %+v", in, tr)
	}
}

// 30초 주기에 1분 rollup이면 같은 window를 두 번 평가한다: 두 번째는 아무것도 바꾸지 않는다(정상 window 하나로 사건이 닫히지 않는다)
func TestSameWindowIsNotCountedTwice(t *testing.T) {
	s := spec(t, errorRatioSpec)
	in := Instance{State: StateAlert, EpisodeOpen: true, EpisodeReason: "violation"}
	in, _ = Next(s, in, OutcomeOK, minute(0), minute(0))
	in, tr := Next(s, in, OutcomeOK, minute(0), minute(0).Add(30*time.Second))
	if !tr.Repeated || in.State != StateRecovering || in.OKStreak != 1 {
		t.Fatalf("repeated window: %+v %+v", in, tr)
	}
	in, tr = Next(s, in, OutcomeOK, minute(1), minute(1))
	if in.State != StateOK || !tr.Closed {
		t.Errorf("next window closes: %+v %+v", in, tr)
	}
	// for_seconds도 window 끝으로 잰다: 같은 window를 여러 번 봐도 시간이 흐르지 않는다
	in = Instance{}
	for i := 0; i < 5; i++ {
		in, _ = Next(s, in, OutcomeViolating, minute(0), minute(0).Add(time.Duration(i)*30*time.Second))
	}
	if in.State != StatePending {
		t.Errorf("for_seconds must not elapse on a repeated window: %+v", in)
	}
}

func TestStateMachineEdges(t *testing.T) {
	s := spec(t, errorRatioSpec)
	// 조회 실패는 위반 시작 시각을 지우지 않는다: 실패가 끼어도 경보가 늦춰지지 않는다
	var in Instance
	in, _ = data(s, in, OutcomeViolating, 0)
	in, _ = Next(s, in, OutcomeError, time.Time{}, minute(1))
	in, tr := data(s, in, OutcomeViolating, 2)
	if in.State != StateAlert || !tr.Opened {
		t.Errorf("error between violations: %+v", in)
	}
	// 결측은 위반 연속을 끊는다
	in = Instance{}
	in, _ = data(s, in, OutcomeViolating, 0)
	in, _ = data(s, in, OutcomeNoData, 1)
	in, _ = data(s, in, OutcomeViolating, 2)
	if in.State != StatePending {
		t.Errorf("no data must reset the violation run: %+v", in)
	}
	// 위반 전 정상은 PENDING을 취소한다
	in, _ = data(s, Instance{}, OutcomeViolating, 0)
	in, _ = data(s, in, OutcomeOK, 1)
	if in.State != StateOK || in.ViolationSince != nil {
		t.Errorf("ok cancels pending: %+v", in)
	}
	// for_seconds 0: 첫 위반에 ALERT
	zero := s
	zero.ForSeconds = 0
	if in, tr := data(zero, Instance{}, OutcomeViolating, 0); in.State != StateAlert || !tr.Opened {
		t.Errorf("for 0: %+v", in)
	}
}

func TestNoDataAlertPolicy(t *testing.T) {
	nd := spec(t, `{"name":"x","query":{"kind":"error_ratio"},"window_seconds":300,"evaluation_seconds":60,
		"condition":{"operator":"gt","threshold":0.02},"for_seconds":120,"no_data":{"action":"alert","after_seconds":600}}`)
	// watermark가 멈춰 window가 없어도(window 없는 결측) 벽시계로 시간이 흘러 600초 뒤 ALERT
	var in Instance
	for m := 0; m < 10; m++ {
		in, _ = Next(nd, in, OutcomeNoData, time.Time{}, minute(m))
		if in.State != StateNoData {
			t.Fatalf("minute %d: %+v", m, in)
		}
	}
	in, tr := Next(nd, in, OutcomeNoData, time.Time{}, minute(10))
	if in.State != StateAlert || !tr.Opened || in.EpisodeReason != "no_data" {
		t.Fatalf("no data alert after 600s: %+v %+v", in, tr)
	}
	// 결측이 계속되면 ALERT 유지
	if in, _ = Next(nd, in, OutcomeNoData, time.Time{}, minute(11)); in.State != StateAlert {
		t.Errorf("still no data: %+v", in)
	}
	// no_data 사건 중 위반이 오면 같은 사건(새로 열지 않음)이고 사유가 violation으로 바뀐다
	v, tr := Next(nd, in, OutcomeViolating, minute(12), minute(12))
	if v.State != StateAlert || tr.Opened || v.EpisodeReason != "violation" {
		t.Errorf("violation during no_data episode: %+v %+v", v, tr)
	}
	// 데이터가 돌아와 정상 2회면 no_data 사건이 닫힌다
	in, _ = Next(nd, in, OutcomeOK, minute(12), minute(12))
	in, tr = Next(nd, in, OutcomeOK, minute(13), minute(13))
	if in.State != StateOK || !tr.Closed {
		t.Errorf("no_data episode closes after recovery: %+v %+v", in, tr)
	}
	// 조회 실패 중에는 결측 시작 시각을 유지한다(실패는 데이터가 돌아왔다는 근거가 아니다)
	in = Instance{}
	in, _ = Next(nd, in, OutcomeNoData, time.Time{}, minute(0))
	in, _ = Next(nd, in, OutcomeError, time.Time{}, minute(5))
	in, _ = Next(nd, in, OutcomeNoData, time.Time{}, minute(10))
	if in.State != StateAlert {
		t.Errorf("error must not reset no-data duration: %+v", in)
	}
}

func TestEvaluate(t *testing.T) {
	s := spec(t, errorRatioSpec)
	gone := `"service.name"="gone"`
	prev := map[string]Instance{gone: {State: StateAlert, EpisodeOpen: true}}
	labels := map[string]map[string]string{gone: {"service.name": "gone"}}
	w := Window{Start: minute(-5), End: minute(0)}
	res := Compute(s, []telemetrystore.MetricBucket{count("checkout", "200", 1000)})
	ev := Evaluate(s, prev, labels, Input{Window: w, Result: res}, minute(0))
	if ev.Status != MonitorEvaluated || len(ev.Groups) != 2 || ev.Groups[0].Labels["service.name"] != "checkout" || ev.Groups[0].Outcome != OutcomeOK {
		t.Fatalf("eval = %+v", ev)
	}
	// 사라진 group은 결측(정상이 아니다), 열린 사건 유지
	if g := ev.Groups[1]; g.Outcome != OutcomeNoData || g.Reason != ReasonGroupMissing || g.Next.State != StateNoData || !g.Next.EpisodeOpen {
		t.Errorf("gone = %+v", g)
	}
	// 조회 실패: 모든 group이 EVALUATION_ERROR, 이전 값으로 대체하지 않는다
	errs := Evaluate(s, prev, labels, Input{Window: w, QueryErr: ErrorQueryFailed}, minute(0))
	if errs.Status != MonitorError || len(errs.Groups) != 1 || errs.Groups[0].Next.State != StateEvaluationError || errs.Groups[0].Value != nil {
		t.Errorf("query failed = %+v", errs)
	}
	// group_by가 있고 이전 상태가 없어도 첫 실패가 monitor 수준으로 남는다
	first := Evaluate(s, nil, nil, Input{Window: w, QueryErr: ErrorQueryFailed}, minute(0))
	if first.Status != MonitorError || first.Reason != ErrorQueryFailed || len(first.Groups) != 0 {
		t.Errorf("first failure = %+v", first)
	}
	// group 상한 초과도 오류다
	if lim := Evaluate(s, nil, nil, Input{Window: w, Result: Result{GroupLimitExceeded: true}}, minute(0)); lim.Status != MonitorError || lim.Reason != ErrorGroupLimit {
		t.Errorf("group limit = %+v", lim)
	}
	// watermark 없음·멈춤: 결측(window 없는 판정)
	for st, reason := range map[WindowStatus]string{WindowNone: ReasonNoWatermark, WindowStale: ReasonStaleWatermark} {
		e := Evaluate(s, prev, labels, Input{WindowStatus: st}, minute(0))
		if e.Status != MonitorNoData || e.Reason != reason || e.Groups[0].Next.State != StateNoData || !e.Groups[0].Next.EpisodeOpen {
			t.Errorf("%s = %+v", reason, e)
		}
	}
	// group_by가 없고 이전 상태도 없으면 group 하나("")가 오류
	single := spec(t, `{"name":"x","query":{"kind":"error_ratio"},"window_seconds":300,"evaluation_seconds":60,
		"condition":{"operator":"gt","threshold":0.02},"for_seconds":0,"no_data":{"action":"no_data"}}`)
	if e := Evaluate(single, nil, nil, Input{Window: w, QueryErr: ErrorQueryFailed}, minute(0)); len(e.Groups) != 1 || e.Groups[0].Key != "" || e.Groups[0].Next.State != StateEvaluationError {
		t.Errorf("single group error = %+v", e)
	}
}
