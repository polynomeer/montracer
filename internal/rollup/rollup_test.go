package rollup

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/metricagg"
)

const (
	tenantA = "11111111-1111-4111-8111-111111111111"
	tenantB = "22222222-2222-4222-8222-222222222222"
)

var (
	now    = time.Date(2026, 10, 5, 12, 5, 30, 0, time.UTC) // watermark = 12:03:00
	stream = [16]byte{1}
)

type fakeStore struct {
	progress Progress
	// maxObs가 있으면 MaxObserved가 그 값을 돌려준다(다른 stream이 계속 들어오는 정상 수집을 모사). nil이면 points에서 계산.
	maxObs  func() time.Time
	points  []RawPoint
	writes  [][]Row
	failing bool
	from    time.Time
	to      time.Time
}

func (f *fakeStore) MaxObserved(_ context.Context, since, until time.Time) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	for _, p := range f.points {
		e := p.Point.End
		if f.maxObs != nil {
			out[p.Tenant] = f.maxObs()
			continue
		}
		if !e.Before(since) && !e.After(until) && e.After(out[p.Tenant]) {
			out[p.Tenant] = e
		}
	}
	return out, nil
}

func (f *fakeStore) LoadProgress(context.Context, time.Time) (Progress, error) {
	return f.progress, nil
}

func (f *fakeStore) ReadPoints(_ context.Context, from, to time.Time) ([]RawPoint, error) {
	f.from, f.to = from, to
	var out []RawPoint
	for _, p := range f.points {
		if !p.Point.End.Before(from) && p.Point.End.Before(to) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeStore) ReadTenantPoints(ctx context.Context, tenant string, from, to time.Time) ([]RawPoint, error) {
	all, err := f.ReadPoints(ctx, from, to)
	var out []RawPoint
	for _, p := range all {
		if p.Tenant == tenant {
			out = append(out, p)
		}
	}
	return out, err
}

func (f *fakeStore) WriteWindows(_ context.Context, rows []Row) error {
	if f.failing {
		return errors.New("clickhouse down")
	}
	f.writes = append(f.writes, rows)
	return nil
}

func counterPoint(tenant string, end time.Time, v float64) RawPoint {
	return RawPoint{Tenant: tenant, StreamID: stream, MetricName: "http.requests", Unit: "1", Type: "sum",
		Temporality: "cumulative", Monotonic: true,
		Point: metricagg.Point{Start: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), End: end, Value: v}}
}

func hm(h, m, s int) time.Time { return time.Date(2026, 10, 5, h, m, s, 0, time.UTC) }

// job은 정상 수집(최대 관측 시각 ≈ 처리 시각)을 가정한 Job이다.
func job(s *fakeStore, clock *time.Time) *Job {
	if s.maxObs == nil {
		s.maxObs = func() time.Time { return *clock }
	}
	return New(Config{Store: s, Now: func() time.Time { return *clock }})
}

// lagJob은 MaxObserved를 points에서 계산한다(수집 지연·idle 시험용).
func lagJob(s *fakeStore, clock *time.Time) *Job {
	return New(Config{Store: s, Now: func() time.Time { return *clock }})
}

func TestClosedWindowsOnlyAndBaselineFromLookback(t *testing.T) {
	s := &fakeStore{points: []RawPoint{
		counterPoint(tenantA, hm(11, 50, 0), 100), // 재계산 범위 이전: 기준점
		counterPoint(tenantA, hm(12, 0, 15), 110),
		counterPoint(tenantA, hm(12, 0, 45), 130),
		counterPoint(tenantA, hm(12, 2, 30), 150),
		counterPoint(tenantA, hm(12, 3, 10), 999), // watermark(12:03) 이후: 아직 닫히지 않음
	}}
	clock := now
	if err := job(s, &clock).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !s.from.Equal(hm(11, 43, 0)) || !s.to.Equal(hm(12, 3, 0)) {
		t.Errorf("read range = [%v, %v)", s.from, s.to)
	}
	rows := s.writes[0]
	if len(rows) != 2 { // 12:00, 12:02 window만. 12:01은 point가 없어 행이 없다(0이 아님)
		t.Fatalf("rows = %d", len(rows))
	}
	r0, r1 := rows[0], rows[1]
	if !r0.WindowStart.Equal(hm(12, 0, 0)) || r0.Agg.Increase != 30 || r0.Agg.Partial {
		t.Errorf("12:00 = %+v (기준점 100은 lookback에서)", r0.Agg)
	}
	if !r1.WindowStart.Equal(hm(12, 2, 0)) || r1.Agg.Increase != 20 {
		t.Errorf("12:02 = %+v (기준점은 직전 window의 130)", r1.Agg)
	}
	if !r0.ExpiresAt.Equal(r0.WindowStart.Add(90 * 24 * time.Hour)) {
		t.Errorf("expires = %v", r0.ExpiresAt)
	}
}

// 10분 안의 늦은 point는 그 window만 새 revision으로 다시 쓴다. 바뀌지 않은 window는 다시 쓰지 않는다.
func TestLatePointRewritesOnlyChangedWindow(t *testing.T) {
	s := &fakeStore{points: []RawPoint{
		counterPoint(tenantA, hm(11, 50, 0), 100), // lookback 기준점
		counterPoint(tenantA, hm(12, 0, 30), 110),
		counterPoint(tenantA, hm(12, 2, 30), 150),
	}}
	clock := now
	j := job(s, &clock)
	if err := j.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := s.writes[0]
	clock = now.Add(30 * time.Second)                                      // 같은 watermark
	s.points = append(s.points, counterPoint(tenantA, hm(12, 0, 50), 120)) // 12:00 window에 늦게 도착
	if err := j.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 12:00 window가 바뀌고, 12:02 window의 기준점(110 → 120)도 바뀌어 둘 다 다시 쓴다
	if len(s.writes) != 2 || len(s.writes[1]) != 2 {
		t.Fatalf("writes = %v", s.writes)
	}
	re0, re1 := s.writes[1][0], s.writes[1][1]
	if !re0.WindowStart.Equal(hm(12, 0, 0)) || re0.Agg.Increase != 20 || re0.Revision <= first[0].Revision {
		t.Errorf("12:00 rewrite = %+v rev %d (was %d)", re0.Agg, re0.Revision, first[0].Revision)
	}
	if !re1.WindowStart.Equal(hm(12, 2, 0)) || re1.Agg.Increase != 30 {
		t.Errorf("12:02 rewrite = %+v", re1.Agg)
	}
	// 변화가 없으면 아무것도 쓰지 않는다
	clock = now.Add(45 * time.Second)
	if err := j.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.writes) != 2 {
		t.Errorf("unchanged windows rewritten: %d writes", len(s.writes))
	}
}

// 쓰기 실패는 기록하지 않으므로 다음 주기에 같은 window를 다시 쓴다.
func TestWriteFailureRetried(t *testing.T) {
	s := &fakeStore{points: []RawPoint{counterPoint(tenantA, hm(11, 50, 0), 1), counterPoint(tenantA, hm(12, 0, 30), 2)}, failing: true}
	clock := now
	j := job(s, &clock)
	if err := j.Cycle(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	s.failing = false
	if err := j.Cycle(context.Background()); err != nil || len(s.writes) != 1 || len(s.writes[0]) != 1 {
		t.Fatalf("retry: err=%v writes=%v", err, s.writes)
	}
}

// 같은 stream ID라도 tenant가 다르면 별도 행이다.
func TestTenantsSeparate(t *testing.T) {
	s := &fakeStore{points: []RawPoint{
		counterPoint(tenantA, hm(11, 50, 0), 1), counterPoint(tenantA, hm(12, 0, 30), 2),
		counterPoint(tenantB, hm(11, 50, 0), 10), counterPoint(tenantB, hm(12, 0, 30), 50),
	}}
	clock := now
	if err := job(s, &clock).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := s.writes[0]
	if len(rows) != 2 || rows[0].Tenant == rows[1].Tenant {
		t.Fatalf("rows = %+v", rows)
	}
	got := []float64{rows[0].Agg.Increase, rows[1].Agg.Increase}
	slices.Sort(got)
	if !slices.Equal(got, []float64{1, 40}) {
		t.Errorf("increases = %v", got)
	}
}

// 재계산 범위를 지난 window는 다시 쓰지 않는다(10분 이후는 backfill 전용, D02 §07).
func TestOutsideRecomputeRangeNotWritten(t *testing.T) {
	s := &fakeStore{points: []RawPoint{counterPoint(tenantA, hm(11, 40, 0), 1), counterPoint(tenantA, hm(11, 50, 30), 2)}}
	clock := now
	if err := job(s, &clock).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.writes) != 0 {
		t.Errorf("window 11:50 (before 11:53) written: %+v", s.writes)
	}
}

func TestContentTokenStable(t *testing.T) {
	r := Row{Tenant: tenantA, StreamID: stream, WindowStart: hm(12, 0, 0), Revision: 5, Agg: metricagg.Aggregate{HasIncrease: true, Increase: 3}}
	same := r // 같은 내용의 다른 값
	if ContentToken([]Row{r}) != ContentToken([]Row{same}) {
		t.Error("token not stable")
	}
	r2 := r
	r2.Agg.Increase = 4
	if ContentToken([]Row{r}) == ContentToken([]Row{r2}) {
		t.Error("different content same token")
	}
}

// watermark = 최대 관측 시각 − 2분 (D02 §07): 수집이 밀리면 window를 늦게 닫는다.
func TestWatermarkFollowsObservedTimeWhenIngestionLags(t *testing.T) {
	// 처리 시계는 12:05:30이지만 관측된 최신 point는 12:01:40 → watermark 11:59
	s := &fakeStore{points: []RawPoint{
		counterPoint(tenantA, hm(11, 50, 0), 1),
		counterPoint(tenantA, hm(11, 58, 30), 2),
		counterPoint(tenantA, hm(12, 0, 30), 3),
		counterPoint(tenantA, hm(12, 1, 40), 4),
	}}
	clock := now
	if err := lagJob(s, &clock).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.writes) == 0 {
		t.Fatal("no windows written")
	}
	for _, r := range s.writes[0] {
		if !r.WindowStart.Before(hm(11, 59, 0)) {
			t.Errorf("window %v closed before watermark 11:59 (수집 지연 중)", r.WindowStart)
		}
	}
}

// idle (D02 §21): 최대 관측 시각이 60초 넘게 늘지 않으면 처리 시계로 닫는다.
func TestIdleInputClosesWindowsByProcessingClock(t *testing.T) {
	s := &fakeStore{points: []RawPoint{counterPoint(tenantA, hm(11, 50, 0), 1), counterPoint(tenantA, hm(12, 0, 30), 2)}}
	clock := now
	j := lagJob(s, &clock)
	_ = j.Cycle(context.Background()) // 첫 관측: 12:00:30 → event watermark 11:58 (12:00 window 미닫힘)
	for _, rows := range s.writes {
		for _, r := range rows {
			if !r.WindowStart.Before(hm(11, 58, 0)) {
				t.Fatalf("window %v closed before event watermark 11:58", r.WindowStart)
			}
		}
	}
	before := len(s.writes)
	clock = now.Add(90 * time.Second) // 입력 없이 90초 → idle → 처리 시계(12:05)로 닫는다
	if err := j.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.writes) != before+1 || !s.writes[before][0].WindowStart.Equal(hm(12, 0, 0)) {
		t.Fatalf("idle window not closed: %+v", s.writes)
	}
}

// 미래 시각 point가 window를 처리 시계보다 먼저 닫지 못한다.
func TestFuturePointDoesNotAdvanceBeyondProcessing(t *testing.T) {
	s := &fakeStore{points: []RawPoint{counterPoint(tenantA, hm(11, 50, 0), 1), counterPoint(tenantA, hm(12, 3, 30), 2), counterPoint(tenantA, now.Add(4*time.Minute), 9)}}
	clock := now
	if err := lagJob(s, &clock).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, rows := range s.writes {
		for _, r := range rows {
			if !r.WindowStart.Before(hm(12, 3, 0)) {
				t.Errorf("window %v beyond processing watermark", r.WindowStart)
			}
		}
	}
}

// watermark가 10분 넘게 뛰어도(따라잡기·idle 전환) 처리 완료 위치부터 계산해 window를 빠뜨리지 않는다.
func TestCatchUpFromProgressAfterWatermarkJump(t *testing.T) {
	s := &fakeStore{
		progress: Progress{Done: map[string]time.Time{tenantA: hm(11, 40, 0)}},
		points: []RawPoint{
			counterPoint(tenantA, hm(11, 39, 0), 1),
			counterPoint(tenantA, hm(11, 45, 30), 2), // watermark−10분(11:53) 이전이지만 처리 전 window
			counterPoint(tenantA, hm(12, 0, 30), 3),
		},
	}
	clock := now
	if err := job(s, &clock).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	var starts []time.Time
	for _, r := range s.writes[0] {
		starts = append(starts, r.WindowStart)
	}
	if len(starts) != 2 || !starts[0].Equal(hm(11, 45, 0)) || !starts[1].Equal(hm(12, 0, 0)) {
		t.Fatalf("windows = %v (11:45 must not be skipped)", starts)
	}
	if !s.from.Equal(hm(11, 30, 0)) {
		t.Errorf("read from = %v, want 11:40 − lookback", s.from)
	}
}

// 따라잡기 상한(1시간)을 넘는 공백은 gap으로 기록하고 상한부터 이어간다(나머지는 backfill).
func TestGapBeyondCatchUpLimit(t *testing.T) {
	obs := &gapObserver{}
	s := &fakeStore{
		progress: Progress{Done: map[string]time.Time{tenantA: hm(9, 0, 0)}},
		points:   []RawPoint{counterPoint(tenantA, hm(11, 50, 0), 1), counterPoint(tenantA, hm(12, 0, 30), 2)},
	}
	clock := now
	s.maxObs = func() time.Time { return clock }
	j := New(Config{Store: s, Now: func() time.Time { return clock }, Observer: obs})
	if err := j.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if obs.last.Gaps != 1 || !s.from.Equal(hm(11, 3, 0).Add(-10*time.Minute)) {
		t.Errorf("gaps=%d read from=%v", obs.last.Gaps, s.from)
	}
}

type gapObserver struct{ last CycleResult }

func (o *gapObserver) ObserveCycle(r CycleResult) { o.last = r }

// 재시작 뒤 새 revision은 저장된 최대 revision보다 크다 — 시계가 뒤로 가도 오래된 내용이 이기지 않는다.
func TestRevisionMonotonicAcrossRestart(t *testing.T) {
	future := uint64(hm(23, 0, 0).UnixNano())
	s := &fakeStore{
		progress: Progress{MaxRevision: future},
		points:   []RawPoint{counterPoint(tenantA, hm(11, 50, 0), 1), counterPoint(tenantA, hm(12, 0, 30), 2)},
	}
	clock := now
	if err := job(s, &clock).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.writes[0][0].Revision; got <= future {
		t.Errorf("revision %d not above stored max %d", got, future)
	}
}

// tenant별 watermark: 빠른 tenant가 느린 tenant의 window를 닫지 않는다 (D02 §21).
func TestPerTenantWatermark(t *testing.T) {
	s := &fakeStore{points: []RawPoint{
		counterPoint(tenantA, hm(11, 50, 0), 1), counterPoint(tenantA, hm(12, 0, 30), 2), counterPoint(tenantA, hm(12, 5, 20), 3),
		counterPoint(tenantB, hm(11, 50, 0), 1), counterPoint(tenantB, hm(12, 0, 30), 2), // B는 12:00:30 이후 밀림
	}}
	clock := now
	if err := lagJob(s, &clock).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.writes[0] {
		if r.Tenant == tenantB && !r.WindowStart.Before(hm(11, 58, 0)) {
			t.Errorf("tenant B window %v closed by tenant A's watermark", r.WindowStart)
		}
	}
	foundA := false
	for _, r := range s.writes[0] {
		if r.Tenant == tenantA && r.WindowStart.Equal(hm(12, 0, 0)) {
			foundA = true
		}
	}
	if !foundA {
		t.Error("tenant A 12:00 window not closed")
	}
}

// batch를 넘어 같은 관측 시각에 다른 값이 오면 먼저 수신한 값(version 큰 것)을 쓰고 표시한다 (ADR 0021 §4).
func TestCrossBatchConflictUsesFirstReceived(t *testing.T) {
	g := func(v float64, version uint64) RawPoint {
		return RawPoint{Tenant: tenantA, StreamID: stream, MetricName: "queue.depth", Type: "gauge", Temporality: "unspecified",
			Version: version, Point: metricagg.Point{End: hm(12, 0, 30), Value: v}}
	}
	s := &fakeStore{points: []RawPoint{g(9, 100), g(1, 200)}} // 200이 먼저 수신(version 큼)
	clock := now
	if err := job(s, &clock).Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := s.writes[0][0]
	if r.Agg.Last != 1 || r.Agg.Samples != 1 || !slices.Contains(r.Agg.Flags, metricagg.FlagDuplicateTimestamp) {
		t.Errorf("= %+v", r.Agg)
	}
}

// 1시간 rollup: 같은 계산을 1시간 window로 한다. 닫힌 시간만 쓰고, 늦은 point는 다음 주기에 그 시간을 다시 쓴다.
func TestHourlyWindow(t *testing.T) {
	s := &fakeStore{points: []RawPoint{
		counterPoint(tenantA, hm(10, 55, 0), 100), // 10:00 시간의 기준점 → 11:00 시간 계산 시 lookback으로 찾는다
		counterPoint(tenantA, hm(11, 10, 0), 130),
		counterPoint(tenantA, hm(11, 50, 0), 170),
		counterPoint(tenantA, hm(12, 1, 0), 180), // 12:00 시간은 아직 닫히지 않음
	}}
	clock := hm(12, 5, 30)
	s.maxObs = func() time.Time { return clock }
	j := New(Config{Store: s, Now: func() time.Time { return clock }, Window: time.Hour, Recompute: time.Hour,
		MaxCatchUp: 24 * time.Hour, Retention: 395 * 24 * time.Hour})
	if err := j.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.writes) != 1 || len(s.writes[0]) != 1 {
		t.Fatalf("writes = %+v", s.writes)
	}
	r := s.writes[0][0]
	if !r.WindowStart.Equal(hm(11, 0, 0)) || r.Agg.Increase != 70 || r.Agg.Partial || !r.ExpiresAt.Equal(hm(11, 0, 0).Add(395*24*time.Hour)) {
		t.Errorf("11:00 hour = %+v expires %v", r.Agg, r.ExpiresAt)
	}
	// 12:00 시간 안의 늦은 point는 그 시간이 닫힌 뒤 반영된다(아직 열린 window)
	s.points = append(s.points, counterPoint(tenantA, hm(11, 58, 0), 175))
	clock = hm(12, 7, 0)
	if err := j.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.writes) != 2 || s.writes[1][0].Agg.Increase != 75 {
		t.Errorf("late point in 11:00 hour not reflected: %+v", s.writes)
	}
}
