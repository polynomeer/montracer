//go:build integration

package controldb

import (
	"context"
	"testing"
	"time"
)

func specWrite(name, query string, enabled bool) MonitorWrite {
	return MonitorWrite{Name: name, Spec: []byte(`{"name":"` + name + `","query":` + query + `,"condition":{"operator":"gt","threshold":0.02}}`),
		Enabled: enabled, EvaluationSeconds: 60}
}

// 서비스 상태(ADR 0073): filter로 걸린 monitor는 group 상태와 시작 시각으로, group_by만 있고 group이 없는 monitor는
// unattributed로, 꺼진·지운 monitor와 다른 tenant의 monitor는 빠진다. environment를 주면 그 환경 행만이다.
func TestServiceHealth(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	other := newTenant(t, db)
	ctx := context.Background()
	monitors := NewMonitorStore(db)
	dev := developer(t, f)
	create := func(_ any, w MonitorWrite, key string) string {
		t.Helper()
		if _, _, err := monitors.CreateMonitor(ctx, dev, w, idemReq(key, key), "r", respondWith); err != nil {
			t.Fatal(err)
		}
		ms, _, _, err := monitors.ListMonitors(ctx, dev, nil, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			if m.Name == w.Name {
				return m.ID
			}
		}
		t.Fatalf("created monitor %q not listed", w.Name)
		return ""
	}
	fixed := create(nil, specWrite("checkout prod 오류율", `{"kind":"error_ratio","filter":{"op":"and","args":[{"op":"eq","field":"service.name","value":"checkout"},{"op":"eq","field":"deployment.environment.name","value":"prod"}]},"group_by":[]}`, true), "a")
	create(nil, specWrite("서비스별 오류율", `{"kind":"error_ratio","filter":null,"group_by":["service.name"]}`, true), "b")
	create(nil, specWrite("꺼진 cart", `{"kind":"error_ratio","filter":{"op":"eq","field":"service.name","value":"cart"},"group_by":[]}`, false), "c")
	gone := create(nil, specWrite("지운 search", `{"kind":"error_ratio","filter":{"op":"eq","field":"service.name","value":"search"},"group_by":[]}`, true), "d")
	if _, err := monitors.DeleteMonitor(ctx, dev, gone, nil, "r"); err != nil {
		t.Fatal(err)
	}
	odev := developer(t, other)
	if _, _, err := monitors.CreateMonitor(ctx, odev, specWrite("다른 tenant", `{"kind":"error_ratio","filter":{"op":"eq","field":"service.name","value":"payments"},"group_by":[]}`, true),
		idemReq("o", "o"), "r", respondWith); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	// 평가 전: filter로 걸린 monitor는 NOT_EVALUATED로 걸리고, group_by만 있는 monitor는 unattributed
	sh, err := monitors.ServiceHealth(ctx, dev, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(sh.Services) != 1 || sh.Services[0].Key.Name != "checkout" || sh.Services[0].Key.Environment != "prod" || sh.Services[0].State != "NOT_EVALUATED" ||
		sh.Services[0].StateSince != nil {
		t.Fatalf("before evaluation: %+v", sh.Services)
	}
	if len(sh.Unattributed) != 1 || sh.Unattributed[0].Name != "서비스별 오류율" || sh.Unattributed[0].State != "NOT_EVALUATED" {
		t.Fatalf("unattributed: %+v", sh.Unattributed)
	}

	// 평가: ALERT group(시작 시각 있음) → checkout prod는 ALERT와 그 시각
	s := NewAlertStore(db)
	slot := now.Truncate(time.Minute).Add(-2 * time.Minute)
	c, ok, err := s.ClaimEvaluation(ctx, f.tenant, fixed, slot.Add(10*time.Second), time.Minute)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	g := openedGroup("0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f41")
	since := slot.Add(-time.Minute)
	g.Instance.GroupKey, g.Instance.Labels, g.Instance.StateSince = "", map[string]string{}, &since
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", Groups: []GroupWrite{g}}); err != nil {
		t.Fatal(err)
	}
	prod := "prod"
	sh, err = monitors.ServiceHealth(ctx, dev, &prod, now)
	if err != nil || len(sh.Services) != 1 || sh.Services[0].State != "ALERT" || sh.Services[0].StateSince == nil || !sh.Services[0].StateSince.Equal(since) {
		t.Fatalf("after evaluation: %+v %v", sh.Services, err)
	}
	groups, _, err := monitors.MonitorGroups(ctx, dev, fixed, 10)
	if err != nil || len(groups) != 1 || groups[0].StateSince == nil || !groups[0].StateSince.Equal(since) {
		t.Fatalf("group state_since: %+v %v", groups, err)
	}
	if st, _ := monitors.MonitorStatuses(ctx, dev, []string{fixed}, now); st[fixed].StateSince == nil || !st[fixed].StateSince.Equal(since) {
		t.Errorf("summary state_since: %+v", st[fixed])
	}
	// 다른 환경을 고르면 prod 전용 monitor는 빠진다
	stage := "stage"
	if sh, _ := monitors.ServiceHealth(ctx, dev, &stage, now); len(sh.Services) != 0 {
		t.Errorf("stage: %+v", sh.Services)
	}
	// 다른 tenant는 자기 monitor만 본다
	if sh, err := monitors.ServiceHealth(ctx, odev, nil, now); err != nil || len(sh.Services) != 1 || sh.Services[0].Key.Name != "payments" || len(sh.Unattributed) != 0 {
		t.Errorf("other tenant: %+v %v", sh, err)
	}
	// 한 번도 평가되지 않은 켜진 monitor는 정의가 바뀐 뒤 충분히 지나면 멈춘 것이다
	if sh, _ := monitors.ServiceHealth(ctx, dev, nil, now.Add(time.Hour)); len(sh.Unattributed) != 1 || !sh.Unattributed[0].Stale {
		t.Errorf("never evaluated must become stale: %+v", sh.Unattributed)
	}
}
