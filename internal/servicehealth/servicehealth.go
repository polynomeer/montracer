// Package servicehealth는 monitor와 서비스의 연결과 서비스 상태 합치기다(ADR 0073). 저장소에 닿지 않는 순수 함수다.
//
//   - 연결: group의 유효 label = group label ∪ filter의 고정 eq 조건. 유효 label의 service.name이 서비스다(§1).
//   - 합치기: 걸린 기여의 가장 나쁜 상태. 멈춘 정상·평가 전은 정상보다 나쁘다(§2, ADR 0054 순서).
//   - 상태 시작 시각: 가장 나쁜 상태와 같은 기여 중 가장 이른 값, 하나라도 모르면 모름(§3).
package servicehealth

import (
	"sort"
	"time"

	"github.com/polynomeer/montracer/internal/monitor"
)

// label key (OTel semantic conventions).
const (
	KeyServiceName      = "service.name"
	KeyServiceNamespace = "service.namespace"
	KeyEnvironment      = "deployment.environment.name"
)

// 요약 상태 이름(controldb.MonitorStatus.State와 같다).
const (
	StateAlert           = "ALERT"
	StateEvaluationError = "EVALUATION_ERROR"
	StateNoData          = "NO_DATA"
	StateRecovering      = "RECOVERING"
	StatePending         = "PENDING"
	StateOK              = "OK"
	StateNotEvaluated    = "NOT_EVALUATED"
	StateDisabled        = "DISABLED"
)

// Rank는 나쁜 순서다(작을수록 나쁨). 멈춘 정상은 데이터 없음 다음이다(ADR 0054). 모르는 상태는 가장 나쁘게 본다 —
// 새 상태가 생겨도 정상처럼 보이지 않게 한다(계약 6).
func Rank(state string, stale bool) int {
	switch state {
	case StateAlert:
		return 0
	case StateEvaluationError:
		return 1
	case StateNoData:
		return 2
	case StateOK:
		if stale {
			return 3
		}
		return 7
	case StateRecovering:
		return 4
	case StatePending:
		return 5
	case StateNotEvaluated:
		return 6
	default:
		return -1
	}
}

// Key는 서비스 자연 키다. 빈 문자열은 "조건 없음"이다: Namespace가 비면 이름으로만 맞춘 것, Environment가 비면 전체 환경 monitor다.
type Key struct {
	Namespace   string
	Name        string
	Environment string
}

// Bucket은 monitor 하나의 지금 revision group들을 (group label의 서비스 값, 상태)로 묶은 것이다.
// label 값이 nil이면 group label에 그 key가 없다(group_by에 없음). 빈 문자열이면 속성이 없던 요청이 빈 값으로 묶인 것이다.
type Bucket struct {
	Namespace     *string
	Name          *string
	Environment   *string
	State         string
	Count         int
	EarliestSince *time.Time // 이 묶음의 state_since 중 가장 이른 값
	UnknownSince  bool       // 이 묶음에 state_since가 없는 group이 있다
}

// Monitor는 연결을 계산할 monitor 하나다.
type Monitor struct {
	ID      string
	Name    string
	Filter  *monitor.FilterNode
	GroupBy []string
	// Summary는 monitor 요약 상태(controldb.MonitorStatus.State)다. Stale은 평가가 멈췄다는 뜻이다.
	Summary string
	Stale   bool
	Buckets []Bucket
}

// Contribution은 한 서비스에 대한 monitor 하나의 기여다.
type Contribution struct {
	Key         Key
	MonitorID   string
	MonitorName string
	State       string
	Stale       bool
	Since       *time.Time
	SinceKnown  bool
}

// FixedLabels는 filter의 고정 eq 조건이다(and·eq만 있다, ADR 0049). 같은 field를 다른 값으로 두 번 고정했으면
// 어떤 요청과도 맞지 않으므로 ok=false다.
func FixedLabels(f *monitor.FilterNode) (map[string]string, bool) {
	out := map[string]string{}
	ok := true
	var walk func(n *monitor.FilterNode)
	walk = func(n *monitor.FilterNode) {
		if n == nil || !ok {
			return
		}
		switch n.Op {
		case "and":
			for i := range n.Args {
				walk(&n.Args[i])
			}
		case "eq":
			if n.Value == nil {
				return
			}
			if prev, seen := out[n.Field]; seen && prev != *n.Value {
				ok = false
				return
			}
			out[n.Field] = *n.Value
		}
	}
	walk(f)
	return out, ok
}

func hasKey(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

// Contributions는 monitor 하나가 어느 서비스에 무엇을 기여하는지다. attributed=false면 서비스를 알 수 없지만
// 보여야 하는 monitor다(group_by service.name만 있고 group이 없음 — 처음부터 결측·실패, 평가 전, revision 직후).
func Contributions(m Monitor) (cs []Contribution, unattributed bool) {
	if m.Summary == StateDisabled {
		return nil, false
	}
	fixed, ok := FixedLabels(m.Filter)
	if !ok {
		return nil, false // 모순된 filter는 어느 서비스와도 맞지 않는다
	}
	fixedName, nameFixed := fixed[KeyServiceName]
	if nameFixed && fixedName == "" {
		nameFixed = false
	}
	if len(m.Buckets) == 0 {
		switch {
		case nameFixed:
			return []Contribution{{
				Key:       Key{Namespace: fixed[KeyServiceNamespace], Name: fixedName, Environment: fixed[KeyEnvironment]},
				MonitorID: m.ID, MonitorName: m.Name, State: m.Summary, Stale: m.Stale,
				// 요약이 group에서 오지 않았다(평가 전·group 없는 결측·실패) — 시작 시각은 모른다(§3)
				SinceKnown: false,
			}}, false
		case hasKey(m.GroupBy, KeyServiceName):
			return nil, true
		default:
			return nil, false // 조직 전체 monitor
		}
	}
	pick := func(group *string, fixedV string) string {
		if group != nil {
			return *group
		}
		return fixedV
	}
	for _, b := range m.Buckets {
		name := pick(b.Name, fixedName)
		if b.Name == nil && !nameFixed {
			continue // service.name이 유효 label에 없다
		}
		if name == "" {
			continue // 속성이 없던 요청이 빈 값으로 묶인 group
		}
		cs = append(cs, Contribution{
			Key:       Key{Namespace: pick(b.Namespace, fixed[KeyServiceNamespace]), Name: name, Environment: pick(b.Environment, fixed[KeyEnvironment])},
			MonitorID: m.ID, MonitorName: m.Name, State: b.State, Stale: m.Stale,
			Since: b.EarliestSince, SinceKnown: !b.UnknownSince && b.EarliestSince != nil,
		})
	}
	return cs, false
}

// MonitorRef는 서비스에 걸린 monitor 하나(그 서비스에 대한 가장 나쁜 기여)다.
type MonitorRef struct {
	ID              string
	Name            string
	State           string
	Stale           bool
	AllEnvironments bool
}

// Service는 서비스 하나의 합친 상태다.
type Service struct {
	Key                            Key
	State                          string
	Stale                          bool
	StateSince                     *time.Time
	MonitorCount                   int
	NameOnly                       bool
	IncludesAllEnvironmentMonitors bool
	Monitors                       []MonitorRef // 나쁜 순 최대 MaxMonitorRefs
}

// MaxMonitorRefs는 서비스마다 돌려줄 monitor 수다.
const MaxMonitorRefs = 5

type acc struct {
	svc      Service
	rank     int
	since    *time.Time
	sinceOK  bool
	stale    bool
	monitors map[string]*MonitorRef
	monRank  map[string]int
}

// Aggregate는 기여를 서비스마다 합친다. environment가 있으면 그 환경의 기여와 전체 환경 monitor의 기여를 그 환경 행 하나로
// 합친다. 없으면 환경별 행과 전체 환경 행(Environment "")을 따로 둔다. 결과는 키 순서로 정렬한다.
func Aggregate(cs []Contribution, environment *string) []Service {
	rows := map[Key]*acc{}
	for _, c := range cs {
		k := c.Key
		allEnv := c.Key.Environment == ""
		if environment != nil {
			if !allEnv && c.Key.Environment != *environment {
				continue
			}
			k.Environment = *environment
		}
		a := rows[k]
		if a == nil {
			a = &acc{svc: Service{Key: k}, rank: 1 << 30, monitors: map[string]*MonitorRef{}, monRank: map[string]int{}}
			rows[k] = a
		}
		r := Rank(c.State, c.Stale)
		switch {
		case r < a.rank:
			a.rank, a.svc.State = r, c.State
			a.since, a.sinceOK = c.Since, c.SinceKnown
		case r == a.rank:
			if !c.SinceKnown {
				a.sinceOK = false
			} else if a.since == nil || c.Since.Before(*a.since) {
				a.since = c.Since
			}
		}
		a.stale = a.stale || c.Stale
		if c.Key.Namespace == "" {
			a.svc.NameOnly = true
		}
		if allEnv && environment != nil {
			a.svc.IncludesAllEnvironmentMonitors = true
		}
		if mr, seen := a.monRank[c.MonitorID]; !seen || r < mr {
			a.monRank[c.MonitorID] = r
			a.monitors[c.MonitorID] = &MonitorRef{ID: c.MonitorID, Name: c.MonitorName, State: c.State, Stale: c.Stale, AllEnvironments: allEnv}
		}
	}
	out := make([]Service, 0, len(rows))
	for _, a := range rows {
		s := a.svc
		s.Stale = a.stale
		if a.sinceOK && a.since != nil {
			t := a.since.UTC()
			s.StateSince = &t
		}
		s.MonitorCount = len(a.monitors)
		refs := make([]MonitorRef, 0, len(a.monitors))
		for _, m := range a.monitors {
			refs = append(refs, *m)
		}
		sort.Slice(refs, func(i, j int) bool {
			ri, rj := a.monRank[refs[i].ID], a.monRank[refs[j].ID]
			if ri != rj {
				return ri < rj
			}
			if refs[i].Name != refs[j].Name {
				return refs[i].Name < refs[j].Name
			}
			return refs[i].ID < refs[j].ID
		})
		if len(refs) > MaxMonitorRefs {
			refs = refs[:MaxMonitorRefs]
		}
		s.Monitors = refs
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Key, out[j].Key
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Environment < b.Environment
	})
	return out
}
