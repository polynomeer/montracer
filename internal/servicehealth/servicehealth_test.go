package servicehealth

import (
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/monitor"
)

func str(s string) *string { return &s }

func eq(field, value string) monitor.FilterNode {
	return monitor.FilterNode{Op: "eq", Field: field, Value: str(value)}
}

func and(args ...monitor.FilterNode) *monitor.FilterNode {
	return &monitor.FilterNode{Op: "and", Args: args}
}

var t0 = time.Date(2026, 10, 27, 5, 0, 0, 0, time.UTC)

func at(m int) *time.Time {
	t := t0.Add(time.Duration(m) * time.Minute)
	return &t
}

func TestFixedLabels(t *testing.T) {
	got, ok := FixedLabels(and(eq(KeyServiceName, "checkout"), *and(eq(KeyEnvironment, "prod"))))
	if !ok || got[KeyServiceName] != "checkout" || got[KeyEnvironment] != "prod" {
		t.Fatalf("got %v %v", got, ok)
	}
	if _, ok := FixedLabels(and(eq(KeyServiceName, "a"), eq(KeyServiceName, "b"))); ok {
		t.Fatal("contradictory filter must not link")
	}
	if got, ok := FixedLabels(nil); !ok || len(got) != 0 {
		t.Fatalf("nil filter: %v %v", got, ok)
	}
}

func TestContributionsLinkRules(t *testing.T) {
	// 고정 filter: 그 서비스(환경 포함)
	cs, un := Contributions(Monitor{ID: "m1", Name: "checkout 오류율", Filter: and(eq(KeyServiceName, "checkout"), eq(KeyEnvironment, "prod")),
		Summary: StateAlert, Buckets: []Bucket{{State: StateAlert, Count: 1, EarliestSince: at(3)}}})
	if un || len(cs) != 1 || cs[0].Key != (Key{Name: "checkout", Environment: "prod", EnvironmentSet: true}) || cs[0].State != StateAlert || !cs[0].SinceKnown {
		t.Fatalf("fixed filter: %+v %v", cs, un)
	}
	// group_by service.name: group마다, 빈 값 group은 걸지 않는다
	cs, _ = Contributions(Monitor{ID: "m2", GroupBy: []string{KeyServiceName}, Summary: StateOK, Buckets: []Bucket{
		{Name: str("cart"), State: StateOK, Count: 2, EarliestSince: at(1)},
		{Name: str(""), State: StateAlert, Count: 1, EarliestSince: at(1)},
	}})
	if len(cs) != 1 || cs[0].Key.Name != "cart" || cs[0].Key.EnvironmentSet {
		t.Fatalf("group_by: %+v", cs)
	}
	// 조직 전체 monitor(service.name 없음)는 어디에도 걸리지 않는다
	cs, un = Contributions(Monitor{ID: "m3", Summary: StateAlert, Buckets: []Bucket{{State: StateAlert, Count: 1}}})
	if len(cs) != 0 || un {
		t.Fatalf("org-wide: %+v %v", cs, un)
	}
	// 꺼진 monitor는 기여하지 않는다
	cs, _ = Contributions(Monitor{ID: "m4", Filter: and(eq(KeyServiceName, "checkout")), Summary: StateDisabled})
	if len(cs) != 0 {
		t.Fatalf("disabled: %+v", cs)
	}
	// 모순된 filter
	cs, _ = Contributions(Monitor{ID: "m5", Filter: and(eq(KeyServiceName, "a"), eq(KeyServiceName, "b")), Summary: StateAlert,
		Buckets: []Bucket{{State: StateAlert, Count: 1}}})
	if len(cs) != 0 {
		t.Fatalf("contradictory: %+v", cs)
	}
}

func TestContributionsWithoutGroups(t *testing.T) {
	// filter로 걸린 monitor가 group 없이 실패: 요약 상태로 걸리고 시작 시각은 모름
	cs, un := Contributions(Monitor{ID: "m1", Filter: and(eq(KeyServiceName, "checkout")), Summary: StateEvaluationError})
	if un || len(cs) != 1 || cs[0].State != StateEvaluationError || cs[0].SinceKnown {
		t.Fatalf("filter, no groups: %+v %v", cs, un)
	}
	// group_by service.name만 있는 monitor가 group 없이 실패(revision 직후 포함): 서비스를 알 수 없음 — 따로 보인다
	cs, un = Contributions(Monitor{ID: "m2", GroupBy: []string{KeyServiceName}, Summary: StateNoData})
	if !un || len(cs) != 0 {
		t.Fatalf("group_by only, no groups: %+v %v", cs, un)
	}
	// 평가 전
	cs, _ = Contributions(Monitor{ID: "m3", Filter: and(eq(KeyServiceName, "checkout")), Summary: StateNotEvaluated})
	if len(cs) != 1 || cs[0].State != StateNotEvaluated {
		t.Fatalf("not evaluated: %+v", cs)
	}
}

func TestAggregateWorstStaleAndSince(t *testing.T) {
	k := Key{Name: "checkout", Environment: "prod", EnvironmentSet: true}
	got := Aggregate([]Contribution{
		{Key: k, MonitorID: "a", MonitorName: "a", State: StateOK, Since: at(0), SinceKnown: true},
		{Key: k, MonitorID: "b", MonitorName: "b", State: StateAlert, Since: at(5), SinceKnown: true},
		{Key: k, MonitorID: "c", MonitorName: "c", State: StateAlert, Since: at(3), SinceKnown: true},
	}, nil)
	if len(got) != 1 || got[0].State != StateAlert || !got[0].StateSince.Equal(*at(3)) || got[0].MonitorCount != 3 || got[0].Monitors[0].State != StateAlert || got[0].Monitors[2].ID != "a" {
		t.Fatalf("worst: %+v", got)
	}
	// 같은 최악 상태 중 하나라도 시작 시각을 모르면 모름
	got = Aggregate([]Contribution{
		{Key: k, MonitorID: "a", State: StateAlert, Since: at(1), SinceKnown: true},
		{Key: k, MonitorID: "b", State: StateAlert, SinceKnown: false},
	}, nil)
	if got[0].StateSince != nil {
		t.Fatalf("unknown since must stay unknown: %v", got[0].StateSince)
	}
	// 평가 전 + 정상 = 평가 전(정상보다 나쁘다), 멈춘 정상은 진행 중 상태보다 나쁘다
	got = Aggregate([]Contribution{{Key: k, MonitorID: "a", State: StateOK, SinceKnown: true, Since: at(0)}, {Key: k, MonitorID: "b", State: StateNotEvaluated}}, nil)
	if got[0].State != StateNotEvaluated {
		t.Fatalf("not evaluated vs ok: %+v", got[0])
	}
	got = Aggregate([]Contribution{{Key: k, MonitorID: "a", State: StateOK, Stale: true}, {Key: k, MonitorID: "b", State: StatePending}}, nil)
	if got[0].State != StateOK || !got[0].Stale {
		t.Fatalf("stale ok must outrank pending and keep stale: %+v", got[0])
	}
	// 모르는 상태는 가장 나쁘게
	got = Aggregate([]Contribution{{Key: k, MonitorID: "a", State: StateAlert}, {Key: k, MonitorID: "b", State: "MUTED_SOMETHING"}}, nil)
	if got[0].State != "MUTED_SOMETHING" {
		t.Fatalf("unknown state must not hide: %+v", got[0])
	}
}

func TestAggregateEnvironments(t *testing.T) {
	prod := Key{Name: "checkout", Environment: "prod", EnvironmentSet: true}
	all := Key{Name: "checkout"}
	stage := Key{Name: "checkout", Environment: "stage", EnvironmentSet: true}
	cs := []Contribution{
		{Key: prod, MonitorID: "p", MonitorName: "p", State: StateOK, Since: at(0), SinceKnown: true},
		{Key: all, MonitorID: "x", MonitorName: "x", State: StateAlert, Since: at(2), SinceKnown: true},
		{Key: stage, MonitorID: "s", MonitorName: "s", State: StatePending, Since: at(1), SinceKnown: true},
	}
	// environment 없음: 환경별 행과 전체 환경 행을 따로
	got := Aggregate(cs, nil)
	if len(got) != 3 || got[0].Key != all || got[1].Key != prod || got[2].Key != stage {
		t.Fatalf("separate rows: %+v", got)
	}
	// environment=prod: prod 기여 + 전체 환경 기여를 prod 행 하나로, stage는 빠진다
	env := "prod"
	got = Aggregate(cs, &env)
	if len(got) != 1 || got[0].Key != prod || got[0].State != StateAlert || !got[0].IncludesAllEnvironmentMonitors || got[0].MonitorCount != 2 {
		t.Fatalf("merged prod: %+v", got)
	}
	if !got[0].Monitors[0].AllEnvironments || got[0].Monitors[1].AllEnvironments {
		t.Fatalf("all-environment marker: %+v", got[0].Monitors)
	}
	// 이름으로만 걸린 기여 표시
	if !got[0].NameOnly {
		t.Fatal("namespace-less contribution must be marked name_only")
	}
}

func TestAggregateMonitorRefsCapped(t *testing.T) {
	k := Key{Name: "checkout"}
	var cs []Contribution
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		cs = append(cs, Contribution{Key: k, MonitorID: id, MonitorName: id, State: StateOK})
	}
	got := Aggregate(cs, nil)
	if got[0].MonitorCount != 7 || len(got[0].Monitors) != MaxMonitorRefs {
		t.Fatalf("refs: %+v", got[0])
	}
}

// 빈 environment·namespace group 값(속성이 없던 요청)은 "조건 없음"(전체 환경·이름만)과 다르다
func TestEmptyGroupValuesAreNotWildcards(t *testing.T) {
	cs, _ := Contributions(Monitor{ID: "m", GroupBy: []string{KeyServiceName, KeyEnvironment, KeyServiceNamespace}, Summary: StateAlert, Buckets: []Bucket{
		{Name: str("checkout"), Environment: str(""), Namespace: str(""), State: StateAlert, Count: 1},
	}})
	if len(cs) != 1 || !cs[0].Key.EnvironmentSet || !cs[0].Key.NamespaceSet || cs[0].Key.Environment != "" {
		t.Fatalf("empty group values: %+v", cs)
	}
	prod := "prod"
	if got := Aggregate(cs, &prod); len(got) != 0 {
		t.Fatalf("empty-environment group must not merge into prod: %+v", got)
	}
	got := Aggregate(cs, nil)
	if len(got) != 1 || got[0].NameOnly {
		t.Fatalf("empty namespace is an exact value, not name-only: %+v", got)
	}
}
