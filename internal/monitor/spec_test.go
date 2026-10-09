package monitor

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const errorRatio = `{
  "name": " Checkout 오류율 ",
  "query": {"kind": "error_ratio", "filter": {"op": "and", "args": [{"op": "eq", "field": "service.name", "value": "checkout"}]}, "group_by": ["http.route"]},
  "window_seconds": 300, "evaluation_seconds": 30,
  "condition": {"operator": "gt", "threshold": 0.02},
  "for_seconds": 120,
  "no_data": "alert_after_600s"
}`

func TestNormalizeErrorRatio(t *testing.T) {
	s, warnings, err := Normalize([]byte(errorRatio))
	if err != nil {
		t.Fatal(err)
	}
	// 기본값을 채운 명시적 정규형: 원천 histogram, 최소 요청 100, 복구 2회, 문자열 no_data → 객체
	want := `{"name":"Checkout 오류율","enabled":true,"query":{"kind":"error_ratio","metric":"http.server.request.duration","aggregation":null,` +
		`"filter":{"op":"and","args":[{"op":"eq","field":"service.name","value":"checkout"}]},"group_by":["http.route"]},` +
		`"window_seconds":300,"evaluation_seconds":30,"condition":{"operator":"gt","threshold":0.02},"minimum_requests":100,` +
		`"for_seconds":120,"recovery_evaluations":2,"no_data":{"action":"alert","after_seconds":600},"notification_policy_id":null}`
	if got := string(s.Canonical()); got != want {
		t.Errorf("canonical =\n%s\nwant\n%s", got, want)
	}
	if !slices.Contains(warnings, WarnSubMinuteEvaluation) || !slices.Contains(warnings, WarnDryRunUnavailable) {
		t.Errorf("warnings = %v", warnings)
	}
	// 정규형을 다시 넣으면 같은 정규형(멱등)
	again, _, err := Normalize(s.Canonical())
	if err != nil || string(again.Canonical()) != want {
		t.Errorf("normalize(canonical) = %s, %v", again.Canonical(), err)
	}
}

func TestNormalizeMetric(t *testing.T) {
	s, warnings, err := Normalize([]byte(`{"name":"p95","enabled":false,"query":{"kind":"metric","metric":"http.server.request.duration","aggregation":"p95"},
		"window_seconds":300,"evaluation_seconds":60,"condition":{"operator":"gte","threshold":500},"for_seconds":0,"no_data":{"action":"no_data"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Enabled || s.MinimumRequests != nil || s.Query.Filter != nil || len(s.Query.GroupBy) != 0 || s.Query.GroupBy == nil ||
		s.NoData.Action != NoDataState || s.NoData.AfterSeconds != nil || *s.Query.Aggregation != "p95" {
		t.Errorf("spec = %+v", s)
	}
	if !slices.Contains(warnings, WarnSingleEvaluation) || slices.Contains(warnings, WarnSubMinuteEvaluation) {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestNormalizeRejects(t *testing.T) {
	base := map[string]string{
		"name": `"x"`, "query": `{"kind":"error_ratio"}`, "window_seconds": "300", "evaluation_seconds": "60",
		"condition": `{"operator":"gt","threshold":0.02}`, "no_data": `{"action":"no_data"}`, "for_seconds": "120",
	}
	build := func(over map[string]string) string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range over {
			m[k] = v
		}
		var parts []string
		for k, v := range m {
			if v != "" {
				parts = append(parts, fmt.Sprintf("%q:%s", k, v))
			}
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	for _, c := range []struct {
		over        map[string]string
		field       string
		unsupported bool
	}{
		{map[string]string{"name": `"  "`}, "name", false},
		{map[string]string{"name": `"` + strings.Repeat("가", 201) + `"`}, "name", false},
		{map[string]string{"evaluation_seconds": "45"}, "evaluation_seconds", false},
		{map[string]string{"window_seconds": "30"}, "window_seconds", false},
		{map[string]string{"window_seconds": "90", "evaluation_seconds": "30"}, "window_seconds", false}, // 분 단위 아님
		{map[string]string{"window_seconds": "86460"}, "window_seconds", false},
		{map[string]string{"for_seconds": "90"}, "for_seconds", false}, // evaluation 60의 배수 아님
		{map[string]string{"condition": `{"operator":"ne","threshold":1}`}, "condition.operator", false},
		{map[string]string{"condition": `{"operator":"gt","threshold":2}`}, "condition.threshold", false}, // 비율은 0~1
		{map[string]string{"condition": `{"operator":"gt"}`}, "condition", false},
		{map[string]string{"minimum_requests": "-1"}, "minimum_requests", false},
		{map[string]string{"query": `{"kind":"metric","metric":"m","aggregation":"avg"}`, "minimum_requests": "10"}, "minimum_requests", false},
		{map[string]string{"query": `{"kind":"metric","metric":"m","aggregation":"median"}`}, "query.aggregation", false},
		{map[string]string{"query": `{"kind":"error_ratio","aggregation":"p95"}`}, "query.aggregation", false},
		{map[string]string{"query": `{"kind":"logs"}`}, "query.kind", false},
		{map[string]string{"query": `{"kind":"error_ratio","group_by":["a","a"]}`}, "query.group_by[1]", false},
		{map[string]string{"query": `{"kind":"error_ratio","group_by":["a","b","c","d","e","f"]}`}, "query.group_by", false},
		{map[string]string{"query": `{"kind":"error_ratio","filter":{"op":"eq","field":"a"}}`}, "query.filter.value", false},
		{map[string]string{"query": `{"kind":"error_ratio","filter":{"op":"eq","field":"a","value":"b","x":1}}`}, "query.filter", false},
		// 결측 정책은 필수이고 OK로 바꾸는 선택지는 없다(계약 6)
		{map[string]string{"no_data": ""}, "no_data", false},
		{map[string]string{"no_data": `{"action":"ok"}`}, "no_data.action", false},
		{map[string]string{"no_data": `"ok"`}, "no_data", false},
		{map[string]string{"no_data": `{"action":"alert"}`}, "no_data.after_seconds", false},
		{map[string]string{"no_data": `{"action":"alert","after_seconds":120}`}, "no_data.after_seconds", false}, // window보다 짧음
		{map[string]string{"no_data": `{"action":"no_data","after_seconds":600}`}, "no_data.after_seconds", false},
		{map[string]string{"extra": `1`}, "body", false},
		// 아직 지원하지 않는 의미는 422
		{map[string]string{"query": `{"kind":"error_ratio","filter":{"op":"or","args":[]}}`}, "query.filter.op", true},
		{map[string]string{"notification_policy_id": `"8d49d404-4115-42f8-9c59-f94ff40f7270"`}, "notification_policy_id", true},
		{map[string]string{"recovery_evaluations": "3"}, "recovery_evaluations", true},
		{map[string]string{"for_seconds": ""}, "for_seconds", false}, // 필수: 생략을 0으로 보지 않는다
		{map[string]string{"query": `{"kind":"error_ratio","metric":"jvm.memory.used"}`}, "query.metric", true},
	} {
		body := build(c.over)
		_, _, err := Normalize([]byte(body))
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: err = %v", body, err)
			continue
		}
		found := slices.ContainsFunc(ve.Violations, func(v Violation) bool { return v.Field == c.field })
		if !found || ve.Unsupported != c.unsupported {
			t.Errorf("%s: violations %+v unsupported=%v, want field %s unsupported=%v", body, ve.Violations, ve.Unsupported, c.field, c.unsupported)
		}
	}
	// 형식 오류와 미지원이 섞이면 400(미지원만일 때만 422)
	_, _, err := Normalize([]byte(build(map[string]string{"evaluation_seconds": "45", "notification_policy_id": `"x"`})))
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Unsupported {
		t.Errorf("mixed violations must be 400: %+v", err)
	}
	if _, _, err := Normalize([]byte(`{"name":"x"} {}`)); err == nil {
		t.Error("trailing JSON must be rejected")
	}
	if _, _, err := Normalize([]byte(strings.Repeat(" ", MaxSpecBytes+1))); err == nil {
		t.Error("oversized body must be rejected")
	}
}

func TestDepthLimit(t *testing.T) {
	deep := `{"op":"eq","field":"a","value":"b"}`
	for i := 0; i < 4; i++ {
		deep = `{"op":"and","args":[` + deep + `]}`
	}
	_, _, err := Normalize([]byte(`{"name":"x","query":{"kind":"error_ratio","filter":` + deep + `},"window_seconds":300,"evaluation_seconds":60,
		"condition":{"operator":"gt","threshold":0.1},"for_seconds":60,"no_data":{"action":"no_data"}}`))
	if err == nil || !strings.Contains(err.Error(), "depth") {
		t.Errorf("depth 5 err = %v", err)
	}
}
