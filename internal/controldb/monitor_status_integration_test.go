//go:build integration

package controldb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
)

// 상태 요약: 평가 전 NOT_EVALUATED, 꺼짐 DISABLED, group 중 가장 나쁜 상태(ALERT > EVALUATION_ERROR > NO_DATA …),
// 마지막 평가가 오래되면 stale. group 목록은 나쁜 상태 먼저, timeline은 평가·전이·알림을 시간순으로.
func TestMonitorStatusAndTimeline(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	other := newTenant(t, db)
	ctx := context.Background()
	monitors := NewMonitorStore(db)
	dev := developer(t, f)
	sealer := testSealer(t)
	np, _ := createPolicy(t, db, f, sealer, "s1")
	w := monitorWrite("0.02")
	w.NotificationPolicyID = &np.ID
	if _, _, err := monitors.CreateMonitor(ctx, dev, w, idemReq("a", "a"), "r", respondWith); err != nil {
		t.Fatal(err)
	}
	off := monitorWrite("0.02")
	off.Enabled = false
	if _, _, err := monitors.CreateMonitor(ctx, dev, off, idemReq("b", "b"), "r", respondWith); err != nil {
		t.Fatal(err)
	}
	ms, _, _, _ := monitors.ListMonitors(ctx, dev, nil, 10)
	var on, disabled string
	for _, m := range ms {
		if m.Enabled {
			on = m.ID
		} else {
			disabled = m.ID
		}
	}
	now := time.Now().UTC()
	st, err := monitors.MonitorStatuses(ctx, dev, []string{on, disabled}, now)
	if err != nil || st[on].State != SummaryNotEvaluated || st[disabled].State != SummaryDisabled {
		t.Fatalf("before evaluation: %+v %v", st, err)
	}
	// 평가: group 둘(ALERT, NO_DATA) → 요약 ALERT
	s := NewAlertStore(db)
	slot := now.Truncate(time.Minute).Add(-2 * time.Minute)
	c, ok, err := s.ClaimEvaluation(ctx, f.tenant, on, slot.Add(10*time.Second), time.Minute)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	nodata := alertGroup("NO_DATA", nil)
	nodata.Instance.GroupKey, nodata.Instance.Labels = "service.name=cart", map[string]string{"service.name": "cart"}
	reason := "below_minimum_requests"
	nodata.Instance.LastReason = &reason
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", PolicyID: &np.ID,
		Groups: []GroupWrite{openedGroup("0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f31"), nodata}}); err != nil {
		t.Fatal(err)
	}
	st, err = monitors.MonitorStatuses(ctx, dev, []string{on}, now)
	if err != nil || st[on].State != "ALERT" || st[on].GroupsByState["ALERT"] != 1 || st[on].GroupsByState["NO_DATA"] != 1 || st[on].Stale ||
		st[on].LastEvaluation == nil || !st[on].LastEvaluation.Slot.Equal(slot) {
		t.Fatalf("after evaluation: %+v %v", st[on], err)
	}
	// 마지막 평가가 주기의 3배(5분 하한)보다 오래되면 stale
	if st, _ := monitors.MonitorStatuses(ctx, dev, []string{on}, now.Add(10*time.Minute)); !st[on].Stale {
		t.Error("not stale after 10 minutes")
	}
	groups, more, err := monitors.MonitorGroups(ctx, dev, on, 10)
	if err != nil || more || len(groups) != 2 || groups[0].State != "ALERT" || groups[1].State != "NO_DATA" || groups[1].LastValue != nil ||
		groups[1].LastReason == nil || groups[1].Labels["service.name"] != "cart" {
		t.Fatalf("groups: %+v %v", groups, err)
	}
	if g, more, _ := monitors.MonitorGroups(ctx, dev, on, 1); len(g) != 1 || !more {
		t.Errorf("group limit: %d %v", len(g), more)
	}
	tl, err := monitors.MonitorTimelineFor(ctx, dev, on, now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil || len(tl.Evaluations) != 1 || len(tl.Transitions) != 1 || !tl.Transitions[0].Opened || len(tl.Notifications) != 1 ||
		tl.Notifications[0].EventType != "alert.opened" || tl.Notifications[0].Status != "pending" {
		t.Fatalf("timeline: %+v %v", tl, err)
	}
	if _, err := monitors.MonitorTimelineFor(ctx, dev, on, now.Add(-8*24*time.Hour), now); err == nil {
		t.Error("8-day timeline accepted")
	}
	// 정의가 바뀌면 이전 revision의 group은 요약에서 빠진다(새 revision은 평가 전)
	w.NotificationPolicyID = nil
	if _, err := monitors.UpdateMonitor(ctx, dev, on, 1, w, "r"); err != nil {
		t.Fatal(err)
	}
	// 이전 revision의 마지막 평가로 요약하지도 않는다: 새 revision은 평가 전이다
	if st, _ := monitors.MonitorStatuses(ctx, dev, []string{on}, now); st[on].GroupsByState["ALERT"] != 0 || st[on].State != SummaryNotEvaluated || st[on].LastEvaluation != nil {
		t.Errorf("old revision counted: %+v", st[on])
	}
	// 다른 tenant: 상태·group·timeline 모두 없다
	odev := developer(t, other)
	if st, err := monitors.MonitorStatuses(ctx, odev, []string{on}, now); err != nil || len(st) != 0 {
		t.Errorf("other tenant statuses: %+v %v", st, err)
	}
	if _, _, err := monitors.MonitorGroups(ctx, odev, on, 10); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("other tenant groups: %v", err)
	}
	if _, err := monitors.MonitorTimelineFor(ctx, odev, on, now.Add(-time.Hour), now); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("other tenant timeline: %v", err)
	}
}
