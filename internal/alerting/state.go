package alerting

import (
	"sort"
	"time"

	"github.com/polynomeer/montracer/internal/monitor"
)

// State는 group 하나의 경보 상태다 (D02 §17, D05 §09).
type State string

const (
	StateOK              State = "OK"
	StatePending         State = "PENDING"
	StateAlert           State = "ALERT"
	StateRecovering      State = "RECOVERING"
	StateNoData          State = "NO_DATA"
	StateEvaluationError State = "EVALUATION_ERROR"
)

// Outcome은 이번 평가에서 group의 판정이다.
type Outcome string

const (
	OutcomeViolating Outcome = "violating"
	OutcomeOK        Outcome = "ok"
	OutcomeNoData    Outcome = "no_data" // 값 없음(데이터 없음·최소 요청 미달·병합 불가·group 사라짐)
	OutcomeError     Outcome = "error"   // 조회 실패·group 상한 초과
)

// Instance는 group 하나의 저장된 상태다(alert_instances 행).
//   - EpisodeOpen: ALERT로 연 사건(episode)이 아직 닫히지 않았다. NO_DATA·EVALUATION_ERROR 동안에도 열려 있다 —
//     데이터가 없다고 경보가 조용히 풀리지 않는다(계약 6). 닫히는 길은 복구 2회 연속뿐이다(D02 §17).
//   - ViolationSince: 연속 위반이 시작된 window 끝(PENDING → ALERT 판정, for_seconds).
//   - NoDataSince: 연속 결측이 시작된 벽시계 시각(no_data alert 정책의 after_seconds). watermark가 멈춰도 시간이 흐른다.
//   - OKStreak: ALERT 뒤 서로 다른 window의 연속 정상 수(RECOVERING → OK).
//   - LastWindowEnd: 마지막으로 반영한 window 끝. 같은 window를 다시 평가해도(30초 주기·1분 rollup) 상태를 바꾸지 않는다.
type Instance struct {
	State          State
	EpisodeOpen    bool
	EpisodeReason  string // violation | no_data. 열린 사건 중 위반이 오면 violation으로 바뀐다(같은 사건)
	ViolationSince *time.Time
	NoDataSince    *time.Time
	OKStreak       int
	LastWindowEnd  *time.Time
}

// Transition은 상태 하나의 변화다. Opened·Closed는 episode 경계다(알림 단계 C가 쓴다).
// Repeated면 이미 반영한 window라 아무것도 바뀌지 않았다.
type Transition struct {
	From, To State
	Opened   bool
	Closed   bool
	Repeated bool
}

// Next는 이번 판정으로 다음 상태를 계산한다.
//   - at: 이번 window의 끝. 데이터로 판정한 결과(위반·정상·데이터 결측)에만 있다. 이미 반영한 window(at ≤ LastWindowEnd)면
//     아무것도 바꾸지 않는다 — 같은 데이터로 복구 횟수가 두 번 오르지 않는다.
//   - at이 zero면 window가 없는 판정이다(watermark 없음·멈춤, 조회 실패). 연속 횟수를 올리지 않는다.
//   - now: 벽시계. 결측 지속 시간(no_data after_seconds)을 잰다.
func Next(s monitor.Spec, prev Instance, outcome Outcome, at, now time.Time) (Instance, Transition) {
	next := prev
	if next.State == "" {
		next.State = StateOK
	}
	from := next.State
	if !at.IsZero() && prev.LastWindowEnd != nil && !at.After(*prev.LastWindowEnd) {
		return prev, Transition{From: from, To: from, Repeated: true}
	}
	if !at.IsZero() {
		t := at
		next.LastWindowEnd = &t
	}
	forDur := time.Duration(s.ForSeconds) * time.Second
	var opened, closed bool
	openEpisode := func(reason string) {
		next.State = StateAlert
		next.OKStreak = 0
		if !next.EpisodeOpen {
			next.EpisodeOpen, next.EpisodeReason, opened = true, reason, true
		}
	}

	switch outcome {
	case OutcomeViolating:
		next.NoDataSince = nil
		if next.ViolationSince == nil {
			t := at
			next.ViolationSince = &t
		}
		switch {
		case next.EpisodeOpen:
			// 이미 열린 사건(복구 중이었거나 결측·오류였어도): for_seconds를 다시 기다리지 않고 ALERT.
			// no_data로 연 사건이면 사유가 violation으로 바뀐다 — 같은 사건(episode)이다(알림 중복 억제 key 유지).
			openEpisode("violation")
			next.EpisodeReason = "violation"
		case at.Sub(*next.ViolationSince) >= forDur:
			openEpisode("violation")
		default:
			next.State = StatePending
		}
	case OutcomeOK:
		next.ViolationSince, next.NoDataSince = nil, nil
		if next.EpisodeOpen {
			next.OKStreak++
			if next.OKStreak >= s.RecoveryEvaluations {
				next.State, next.EpisodeOpen, next.EpisodeReason, next.OKStreak = StateOK, false, "", 0
				closed = true
			} else {
				next.State = StateRecovering
			}
		} else {
			next.State, next.OKStreak = StateOK, 0
		}
	case OutcomeNoData:
		// 결측은 위반 연속도, 복구 연속도 끊는다(어느 쪽도 확인되지 않았다)
		next.ViolationSince = nil
		next.OKStreak = 0
		if next.NoDataSince == nil {
			t := now
			next.NoDataSince = &t
		}
		if s.NoData.Action == monitor.NoDataAlert && s.NoData.AfterSeconds != nil &&
			now.Sub(*next.NoDataSince) >= time.Duration(*s.NoData.AfterSeconds)*time.Second {
			openEpisode("no_data")
		} else {
			next.State = StateNoData
		}
	case OutcomeError:
		// 조회 실패는 이전 값으로 대체하지 않는다(D02 §21). 상태는 EVALUATION_ERROR이고 열린 사건은 유지한다.
		// 보수적으로 비대칭이다: 위반 시작 시각은 지우지 않는다(실패는 회복의 근거가 아니다 — 실패가 끼어도 경보가 늦춰지지 않는다).
		// 복구 연속은 끊는다(실패가 끼면 "2회 연속 정상"이 확인되지 않았다 — 사건이 실패 덕에 닫히지 않는다).
		// 결측 시작 시각은 유지한다(실패는 데이터가 돌아왔다는 근거도 아니다).
		next.OKStreak = 0
		next.State = StateEvaluationError
	}
	return next, Transition{From: from, To: next.State, Opened: opened, Closed: closed}
}

// Judge는 group 값으로 판정을 정한다.
func Judge(s monitor.Spec, r GroupResult) Outcome {
	if r.Value == nil {
		return OutcomeNoData
	}
	if Violates(s.Condition, *r.Value) {
		return OutcomeViolating
	}
	return OutcomeOK
}

// GroupEval은 group 하나의 이번 평가다.
type GroupEval struct {
	Key        string
	Labels     map[string]string
	Value      *float64
	Total      *float64
	Reason     string // Value가 nil인 이유, 또는 오류·결측 사유
	Partial    bool
	Outcome    Outcome
	Prev, Next Instance
	Transition Transition
}

// monitor 수준 결과.
const (
	MonitorEvaluated = "evaluated"
	MonitorNoData    = "no_data" // 평가할 확정 window가 없다(watermark 없음·멈춤)
	MonitorError     = "error"   // 조회 실패·group 상한 초과
)

// monitor 전체가 평가되지 못한 이유.
const (
	ErrorQueryFailed = "query_failed"
	ErrorGroupLimit  = "group_limit_exceeded"
)

// Input은 한 번의 평가 입력이다.
//   - WindowStatus가 WindowOK가 아니면 Result·QueryErr는 쓰지 않는다(조회하지 않았다).
//   - QueryErr가 있으면 조회가 실패했다.
type Input struct {
	Window       Window
	WindowStatus WindowStatus
	Result       Result
	QueryErr     string
}

// MonitorEval은 monitor 하나의 평가 결과다. Status·Reason은 group이 없어도 남는다 —
// group_by가 있는 monitor가 처음부터 실패해도 "평가 안 됨"이 기록된다(D05 §09: query failure가 green으로 보이지 않음).
type MonitorEval struct {
	Status string
	Reason string
	Groups []GroupEval
}

// Evaluate는 이전 group 상태와 이번 입력으로 monitor·group별 다음 상태를 계산한다.
//   - window가 없거나 멈췄으면(no_watermark·stale_watermark) 모든 group이 결측이다(window 없는 판정, 벽시계로 지속 시간).
//   - 조회가 실패하거나 group이 MaxGroups를 넘으면 모든 group이 EVALUATION_ERROR다(일부 group만 평가해 나머지를 숨기지 않는다).
//   - 전에 있던 group이 이번 결과에 없으면 결측(group_missing)이다 — 사라졌다고 정상이 되지 않는다.
//   - group_by가 없으면 group은 늘 하나("")다.
//
// group은 key 순서다.
func Evaluate(s monitor.Spec, prev map[string]Instance, prevLabels map[string]map[string]string, in Input, now time.Time) MonitorEval {
	whole := func(status, reason string, o Outcome) MonitorEval {
		keys := sortedKeys(prev)
		if len(keys) == 0 && len(s.Query.GroupBy) == 0 {
			keys = []string{""}
		}
		ev := MonitorEval{Status: status, Reason: reason}
		for _, k := range keys {
			p := prev[k]
			n, t := Next(s, p, o, time.Time{}, now)
			ev.Groups = append(ev.Groups, GroupEval{Key: k, Labels: labelsOr(prevLabels[k]), Reason: reason, Outcome: o, Prev: p, Next: n, Transition: t})
		}
		return ev
	}
	switch in.WindowStatus {
	case WindowNone:
		return whole(MonitorNoData, ReasonNoWatermark, OutcomeNoData)
	case WindowStale:
		return whole(MonitorNoData, ReasonStaleWatermark, OutcomeNoData)
	}
	if in.QueryErr != "" {
		return whole(MonitorError, in.QueryErr, OutcomeError)
	}
	if in.Result.GroupLimitExceeded {
		return whole(MonitorError, ErrorGroupLimit, OutcomeError)
	}
	ev := MonitorEval{Status: MonitorEvaluated}
	at := in.Window.End
	seen := map[string]bool{}
	for _, r := range in.Result.Groups {
		seen[r.Key] = true
		p := prev[r.Key]
		o := Judge(s, r)
		n, t := Next(s, p, o, at, now)
		ev.Groups = append(ev.Groups, GroupEval{Key: r.Key, Labels: r.Labels, Value: r.Value, Total: r.Total, Reason: r.Reason, Partial: r.Partial,
			Outcome: o, Prev: p, Next: n, Transition: t})
	}
	for _, k := range sortedKeys(prev) {
		if seen[k] {
			continue
		}
		p := prev[k]
		n, t := Next(s, p, OutcomeNoData, at, now)
		ev.Groups = append(ev.Groups, GroupEval{Key: k, Labels: labelsOr(prevLabels[k]), Reason: ReasonGroupMissing, Outcome: OutcomeNoData, Prev: p, Next: n, Transition: t})
	}
	sortEvals(ev.Groups)
	return ev
}

func labelsOr(l map[string]string) map[string]string {
	if l == nil {
		return map[string]string{}
	}
	return l
}

func sortedKeys(m map[string]Instance) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortEvals(es []GroupEval) {
	sort.Slice(es, func(i, j int) bool { return es[i].Key < es[j].Key })
}
