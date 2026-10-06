package rollup

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// hourCfg는 cmd/worker hourConfig와 같은 1시간 설정이다.
var hourCfg = Config{Window: time.Hour, Recompute: time.Hour, MaxCatchUp: 24 * time.Hour, Retention: 395 * 24 * time.Hour, BaselineLookback: time.Hour}

// 3시간 전 1분 간격 cumulative counter(분당 +10). live job은 이 구간을 계산하지 않는다(watermark − 10분 밖).
func oldCounter(tenant string, from time.Time, n int) []RawPoint {
	var out []RawPoint
	for i := 0; i <= n; i++ {
		out = append(out, counterPoint(tenant, from.Add(time.Duration(i)*time.Minute).Add(30*time.Second), float64(100+10*i)))
	}
	return out
}

func TestBackfillFillsPastWindowsForOneTenant(t *testing.T) {
	now := hm(12, 0, 0)
	start := hm(9, 0, 0)
	s := &fakeStore{progress: Progress{MaxRevision: uint64(now.Add(time.Hour).UnixNano())}}
	s.points = append(oldCounter(tenantA, start, 30), oldCounter(tenantB, start, 30)...)

	res, err := Backfill(context.Background(), s, Config{}, BackfillRequest{Tenant: tenantA, From: start, To: start.Add(30 * time.Minute)},
		BackfillOptions{ChunkWindows: 10, Pause: -1, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chunks != 3 || res.Windows != 30 || len(s.writes) != 3 {
		t.Fatalf("result = %+v, writes = %d", res, len(s.writes))
	}
	if res.Revision <= s.progress.MaxRevision {
		t.Errorf("revision %d not above stored max %d", res.Revision, s.progress.MaxRevision)
	}
	for _, rows := range s.writes {
		for _, r := range rows {
			if r.Tenant != tenantA {
				t.Fatalf("wrote tenant %s", r.Tenant)
			}
			// 첫 window는 cumulative 첫 point가 기준점뿐이라 missing_baseline이다(ADR 0025 §3, 0으로 만들지 않음).
			// chunk 경계 window(09:10, 09:20)는 lookback 기준점으로 증가량을 안다.
			if r.WindowStart.Equal(start) {
				if r.Agg.HasIncrease {
					t.Errorf("first window has increase without baseline: %+v", r.Agg)
				}
				continue
			}
			if !r.Agg.HasIncrease || r.Agg.Increase != 10 || r.Revision != res.Revision {
				t.Errorf("window %s: %+v", r.WindowStart.Format("15:04"), r.Agg)
			}
			if !r.ExpiresAt.Equal(r.WindowStart.Add(90 * 24 * time.Hour)) {
				t.Errorf("expires %s", r.ExpiresAt)
			}
		}
	}
}

// 값이 없는 window는 쓰지 않는다(0으로 만들지 않는다).
func TestBackfillSkipsEmptyWindows(t *testing.T) {
	now := hm(12, 0, 0)
	s := &fakeStore{points: oldCounter(tenantA, hm(9, 0, 0), 5)}
	res, err := Backfill(context.Background(), s, Config{}, BackfillRequest{Tenant: tenantA, From: hm(8, 0, 0), To: hm(10, 0, 0)},
		BackfillOptions{Pause: -1, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Windows != 6 {
		t.Errorf("windows = %d, want only the 6 with data", res.Windows)
	}
}

// 1시간 resolution도 같은 계산이다(경계는 window 단위로 넓혀 맞춘다).
func TestBackfillHourlyAlignsToWindow(t *testing.T) {
	now := hm(13, 0, 0)
	s := &fakeStore{points: oldCounter(tenantA, hm(9, 0, 0), 90)}
	res, err := Backfill(context.Background(), s, hourCfg, BackfillRequest{Tenant: tenantA, From: hm(9, 20, 0), To: hm(10, 10, 0)},
		BackfillOptions{Pause: -1, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if !res.From.Equal(hm(9, 0, 0)) || !res.To.Equal(hm(11, 0, 0)) || res.Windows != 2 {
		t.Errorf("result = %+v", res)
	}
}

func TestBackfillRejectsRanges(t *testing.T) {
	now := hm(12, 0, 0)
	opts := BackfillOptions{Pause: -1, Now: func() time.Time { return now }}
	for name, req := range map[string]BackfillRequest{
		"bad tenant":           {Tenant: "acme", From: hm(9, 0, 0), To: hm(10, 0, 0)},
		"reversed":             {Tenant: tenantA, From: hm(10, 0, 0), To: hm(9, 0, 0)},
		"open window":          {Tenant: tenantA, From: hm(11, 0, 0), To: hm(12, 0, 30)},
		"future":               {Tenant: tenantA, From: hm(11, 0, 0), To: hm(13, 0, 0)},
		"live recompute range": {Tenant: tenantA, From: hm(11, 0, 0), To: hm(11, 50, 0)}, // live는 floor(11:58)−10분=11:48부터
		"retention margin":     {Tenant: tenantA, From: now.Add(-15*24*time.Hour + time.Hour), To: hm(10, 0, 0)},
		"beyond raw retention": {Tenant: tenantA, From: now.Add(-16 * 24 * time.Hour), To: hm(10, 0, 0)},
	} {
		s := &fakeStore{}
		if _, err := Backfill(context.Background(), s, Config{}, req, opts); !errors.Is(err, ErrBackfillRange) {
			t.Errorf("%s: %v, want ErrBackfillRange", name, err)
		}
		if len(s.writes) != 0 {
			t.Errorf("%s: wrote rows", name)
		}
	}
}

func TestBackfillWriteFailureStops(t *testing.T) {
	now := hm(12, 0, 0)
	s := &fakeStore{points: oldCounter(tenantA, hm(9, 0, 0), 30), failing: true}
	res, err := Backfill(context.Background(), s, Config{}, BackfillRequest{Tenant: tenantA, From: hm(9, 0, 0), To: hm(9, 30, 0)},
		BackfillOptions{ChunkWindows: 10, Pause: -1, Now: func() time.Time { return now }})
	if err == nil || res.Chunks != 0 {
		t.Fatalf("err=%v chunks=%d, want stop at first chunk", err, res.Chunks)
	}
}

// 같은 범위는 chunk 크기와 무관하게 같은 값이다(기준점은 window 앞 lookback 안에서만, 리뷰에서 발견).
// 15분마다 오는 cumulative stream: lookback(10분) 밖 기준점은 쓰지 않으므로 window 위치와 상관없이 missing_baseline이다.
func TestBackfillIndependentOfChunking(t *testing.T) {
	now := hm(12, 0, 0)
	var pts []RawPoint
	for i := 0; i < 8; i++ {
		pts = append(pts, counterPoint(tenantA, hm(9, 0, 30).Add(time.Duration(i)*15*time.Minute), float64(100+i*10)))
	}
	run := func(chunk int) map[time.Time]Row {
		s := &fakeStore{points: pts}
		if _, err := Backfill(context.Background(), s, Config{}, BackfillRequest{Tenant: tenantA, From: hm(9, 0, 0), To: hm(11, 0, 0)},
			BackfillOptions{ChunkWindows: chunk, Pause: -1, Now: func() time.Time { return now }}); err != nil {
			t.Fatal(err)
		}
		out := map[time.Time]Row{}
		for _, rows := range s.writes {
			for _, r := range rows {
				r.Revision, r.ComputedAt = 0, time.Time{}
				out[r.WindowStart] = r
			}
		}
		return out
	}
	a, b := run(7), run(60)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("chunk 7 and 60 differ:\n%v\n%v", a, b)
	}
	for ws, r := range a {
		if r.Agg.HasIncrease {
			t.Errorf("window %s used a baseline older than the lookback: %+v", ws.Format("15:04"), r.Agg)
		}
	}
}

// 대문자 UUID도 정규화해 실제로 쓴다(조용히 0 window로 성공하지 않는다).
func TestBackfillCanonicalTenant(t *testing.T) {
	now := hm(12, 0, 0)
	s := &fakeStore{points: oldCounter(tenantA, hm(9, 0, 0), 5)}
	res, err := Backfill(context.Background(), s, Config{}, BackfillRequest{Tenant: strings.ToUpper(tenantA), From: hm(9, 0, 0), To: hm(10, 0, 0)},
		BackfillOptions{Pause: -1, Now: func() time.Time { return now }})
	if err != nil || res.Windows != 6 || res.Tenant != tenantA {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestBackfillPointCap(t *testing.T) {
	now := hm(12, 0, 0)
	s := &fakeStore{points: oldCounter(tenantA, hm(9, 0, 0), 30)}
	_, err := Backfill(context.Background(), s, Config{}, BackfillRequest{Tenant: tenantA, From: hm(9, 0, 0), To: hm(10, 0, 0)},
		BackfillOptions{MaxPoints: 10, Pause: -1, Now: func() time.Time { return now }})
	if !errors.Is(err, ErrBackfillTooLarge) || len(s.writes) != 0 {
		t.Fatalf("err=%v writes=%d", err, len(s.writes))
	}
}

func TestLiveRecomputeStart(t *testing.T) {
	now := hm(12, 0, 30)
	if got := LiveRecomputeStart(Config{}, now); !got.Equal(hm(11, 48, 0)) {
		t.Errorf("1m live start = %s", got.Format("15:04"))
	}
	if got := LiveRecomputeStart(hourCfg, now); !got.Equal(hm(10, 0, 0)) {
		t.Errorf("1h live start = %s", got.Format("15:04"))
	}
}
