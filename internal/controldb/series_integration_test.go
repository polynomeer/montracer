//go:build integration

package controldb

import (
	"context"
	"testing"
	"time"
)

func TestSeriesStore(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	s := NewSeriesStore(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	id1, id2, old := [16]byte{1}, [16]byte{2}, [16]byte{3}

	if err := s.Touch(ctx, a.tenant, []SeriesEntry{{id1, "m"}, {id2, "m"}}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Touch(ctx, a.tenant, []SeriesEntry{{old, "m"}}, now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 같은 stream ID라도 tenant B의 행은 별개다(RLS)
	if err := s.Touch(ctx, b.tenant, []SeriesEntry{{id1, "m"}}, now); err != nil {
		t.Fatal(err)
	}
	since := now.Add(-time.Hour)
	if n, err := s.ActiveCount(ctx, a.tenant, since); err != nil || n != 2 {
		t.Errorf("A active = %d, %v (오래된 series 제외)", n, err)
	}
	if n, err := s.ActiveCount(ctx, b.tenant, since); err != nil || n != 1 {
		t.Errorf("B active = %d, %v", n, err)
	}
	known, err := s.Known(ctx, a.tenant, [][16]byte{id1, old, {9}}, since)
	if err != nil || !known[id1] || known[old] || len(known) != 1 {
		t.Errorf("known = %v, %v", known, err)
	}
	// 재등록은 last_seen만 앞으로 (되돌리지 않음)
	if err := s.Touch(ctx, a.tenant, []SeriesEntry{{old, "m"}}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Touch(ctx, a.tenant, []SeriesEntry{{old, "m"}}, now.Add(-5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.ActiveCount(ctx, a.tenant, since); n != 3 {
		t.Errorf("after refresh active = %d, want 3", n)
	}
	// 정리는 그 tenant의 오래된 행만
	if err := s.Touch(ctx, a.tenant, []SeriesEntry{{[16]byte{4}, "m"}}, now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Cleanup(ctx, a.tenant, now.Add(-2*time.Hour)); err != nil || n != 1 {
		t.Errorf("cleanup = %d, %v", n, err)
	}
	if n, _ := s.ActiveCount(ctx, b.tenant, since); n != 1 {
		t.Errorf("tenant B affected by A cleanup: %d", n)
	}
	if n := count(t, db, nil, "metric_series"); n != 0 {
		t.Errorf("rows visible without tenant context: %d", n)
	}
}

func TestLabelValueStore(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	s := NewSeriesStore(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	v1 := LabelValue{Metric: "m", Key: "route", Hash: [16]byte{1}}
	v2 := LabelValue{Metric: "m", Key: "route", Hash: [16]byte{2}}
	other := LabelValue{Metric: "m", Key: "status", Hash: [16]byte{1}}
	if err := s.TouchValues(ctx, a.tenant, []LabelValue{v1, v2, other}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchValues(ctx, b.tenant, []LabelValue{v1}, now); err != nil {
		t.Fatal(err)
	}
	since := now.Add(-time.Hour)
	counts, err := s.ValueCounts(ctx, a.tenant, []LabelKey{{"m", "route"}, {"m", "status"}, {"m", "none"}}, since)
	if err != nil || counts[LabelKey{"m", "route"}] != 2 || counts[LabelKey{"m", "status"}] != 1 || len(counts) != 2 {
		t.Errorf("A counts = %v, %v", counts, err)
	}
	counts, _ = s.ValueCounts(ctx, b.tenant, []LabelKey{{"m", "route"}}, since)
	if counts[LabelKey{"m", "route"}] != 1 {
		t.Errorf("B counts = %v (RLS)", counts)
	}
	known, err := s.KnownValues(ctx, a.tenant, []LabelValue{v1, {Metric: "m", Key: "route", Hash: [16]byte{9}}}, since)
	if err != nil || !known[v1] || len(known) != 1 {
		t.Errorf("known = %v, %v", known, err)
	}
	if n, err := s.CleanupValues(ctx, a.tenant, now.Add(time.Second)); err != nil || n != 3 {
		t.Errorf("cleanup = %d, %v", n, err)
	}
	if counts, _ := s.ValueCounts(ctx, b.tenant, []LabelKey{{"m", "route"}}, since); counts[LabelKey{"m", "route"}] != 1 {
		t.Error("tenant B affected by A cleanup")
	}
}
