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

// group_by 묶음은 labels JSON 경로에서 서비스·환경을 읽고, 빈 환경 값(속성이 없던 요청)은 전체 환경 monitor가 아니다.
// 같은 상태 group 중 하나라도 시작 시각을 모르면 요약·서비스의 시작 시각은 모름이다.
func TestServiceHealthGroupByAndUnknownSince(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	ctx := context.Background()
	monitors := NewMonitorStore(db)
	dev := developer(t, f)
	if _, _, err := monitors.CreateMonitor(ctx, dev, specWrite("서비스·환경별 오류율", `{"kind":"error_ratio","filter":null,"group_by":["service.name","deployment.environment.name"]}`, true),
		idemReq("g", "g"), "r", respondWith); err != nil {
		t.Fatal(err)
	}
	ms, _, _, _ := monitors.ListMonitors(ctx, dev, nil, 10)
	id := ms[0].ID
	now := time.Now().UTC()
	s := NewAlertStore(db)
	slot := now.Truncate(time.Minute).Add(-2 * time.Minute)
	c, ok, err := s.ClaimEvaluation(ctx, f.tenant, id, slot.Add(10*time.Second), time.Minute)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	since := slot.Add(-time.Minute)
	group := func(key, svc, env, state string, at *time.Time) GroupWrite {
		return GroupWrite{Instance: StoredInstance{GroupKey: key, Labels: map[string]string{"service.name": svc, "deployment.environment.name": env}, State: state, StateSince: at}}
	}
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", Groups: []GroupWrite{
		group("k1", "checkout", "prod", "NO_DATA", &since),
		group("k2", "checkout", "prod", "NO_DATA", nil), // 이 열 전의 행처럼 시작 시각 모름
		group("k3", "checkout", "", "OK", &since),       // 환경 속성이 없던 요청
	}}); err != nil {
		t.Fatal(err)
	}
	sh, err := monitors.ServiceHealth(ctx, dev, nil, now)
	if err != nil || len(sh.Services) != 2 {
		t.Fatalf("services: %+v %v", sh.Services, err)
	}
	for _, svc := range sh.Services {
		switch {
		case svc.Key.Environment == "prod":
			if svc.State != "NO_DATA" || svc.StateSince != nil {
				t.Errorf("prod: %+v", svc)
			}
		case svc.Key.EnvironmentSet && svc.Key.Environment == "":
			if svc.State != "OK" {
				t.Errorf("empty env: %+v", svc)
			}
		default:
			t.Errorf("unexpected row (empty env treated as all environments?): %+v", svc)
		}
	}
	if st, _ := monitors.MonitorStatuses(ctx, dev, []string{id}, now); st[id].State != "NO_DATA" || st[id].StateSince != nil {
		t.Errorf("summary since must be unknown: %+v", st[id])
	}
}

// 상한에서 잘리면 경고를 내고 모든 행을 partial로, 경계의 monitor와 그 뒤는 기여 계산에서 뺀다(일부만 읽은 monitor가
// 덜 나쁘게 보이거나 읽지 못한 monitor가 "group 없음"으로 잘못 분류되지 않게).
func TestServiceHealthTruncation(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	ctx := context.Background()
	monitors := NewMonitorStore(db)
	dev := developer(t, f)
	for _, k := range []string{"x", "y", "z"} {
		if _, _, err := monitors.CreateMonitor(ctx, dev, specWrite("checkout "+k, `{"kind":"error_ratio","filter":{"op":"eq","field":"service.name","value":"checkout"},"group_by":[]}`, true),
			idemReq(k, k), "r", respondWith); err != nil {
			t.Fatal(err)
		}
	}
	defer func(m int) { serviceHealthMonitorLimit = m }(serviceHealthMonitorLimit)
	serviceHealthMonitorLimit = 2
	sh, err := monitors.ServiceHealth(ctx, dev, nil, time.Now().UTC())
	if err != nil || len(sh.Warnings) != 1 || sh.Warnings[0] != WarnMonitorsTruncated || len(sh.Services) != 1 || !sh.Services[0].Partial || sh.Services[0].MonitorCount != 2 {
		t.Fatalf("monitor cap: %+v %v", sh, err)
	}
	// 같은 요청을 다시 해도 같은 monitor를 고른다(결정적 순서)
	again, _ := monitors.ServiceHealth(ctx, dev, nil, time.Now().UTC())
	if again.Services[0].Monitors[0].ID != sh.Services[0].Monitors[0].ID || again.Services[0].Monitors[1].ID != sh.Services[0].Monitors[1].ID {
		t.Errorf("non-deterministic selection: %+v vs %+v", sh.Services[0].Monitors, again.Services[0].Monitors)
	}
	serviceHealthMonitorLimit = ServiceHealthMaxMonitors

	// group 묶음 상한: 모든 monitor를 평가해 group 하나씩 두고 상한을 1로 — 경계 monitor와 그 뒤는 빠지고 "group 없음"(평가 전)으로
	// 잘못 걸리지 않는다
	ms, _, _, _ := monitors.ListMonitors(ctx, dev, nil, 10)
	s := NewAlertStore(db)
	now := time.Now().UTC()
	slot := now.Truncate(time.Minute).Add(-2 * time.Minute)
	for _, m := range ms {
		c, ok, err := s.ClaimEvaluation(ctx, f.tenant, m.ID, slot.Add(10*time.Second), time.Minute)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		g := GroupWrite{Instance: StoredInstance{GroupKey: "", Labels: map[string]string{}, State: "ALERT"}}
		if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", Groups: []GroupWrite{g}}); err != nil {
			t.Fatal(err)
		}
	}
	defer func(b int) { serviceHealthBucketLimit = b }(serviceHealthBucketLimit)
	serviceHealthBucketLimit = 1
	sh, err = monitors.ServiceHealth(ctx, dev, nil, now)
	if err != nil || len(sh.Warnings) != 1 || sh.Warnings[0] != WarnGroupsTruncated {
		t.Fatalf("bucket cap: %+v %v", sh, err)
	}
	for _, svc := range sh.Services {
		if !svc.Partial || svc.State != "ALERT" {
			t.Errorf("rows after bucket cut: %+v", svc)
		}
	}
	if len(sh.Services) != 0 && sh.Services[0].MonitorCount > 1 {
		t.Errorf("monitors at or after the cut must be excluded: %+v", sh.Services[0])
	}
}
