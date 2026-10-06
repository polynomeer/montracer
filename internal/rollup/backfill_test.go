package rollup

import (
	"context"
	"errors"
	"testing"
	"time"
)

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
	now := hm(12, 0, 0)
	s := &fakeStore{points: oldCounter(tenantA, hm(9, 0, 0), 90)}
	res, err := Backfill(context.Background(), s, Config{Window: time.Hour, BaselineLookback: time.Hour}, BackfillRequest{Tenant: tenantA, From: hm(9, 20, 0), To: hm(10, 10, 0)},
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
