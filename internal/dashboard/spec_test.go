package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const validSpec = `{
  "title": " Checkout ",
  "description": "결제 서비스",
  "variables": [{"name":"env","kind":"environment","default":"prod"},{"name":"svc","kind":"service"}],
  "widgets": [
    {"id":"rate","kind":"time_series","title":"요청량","unit":"req/s",
     "query":{"metric":"http.server.request.duration","aggregation":"rate",
       "filter":{"op":"and","args":[{"op":"eq","field":"deployment.environment.name","value":"$env"},{"op":"eq","field":"service.name","value":"$svc"}]},
       "group_by":["http.route"]}},
    {"id":"p95","kind":"stat","query":{"metric":"http.server.request.duration","aggregation":"p95"},"time_override":{"range":"4h"}},
    {"id":"top","kind":"top_list","query":{"metric":"http.server.request.duration","aggregation":"count","group_by":["service.name"]},"display":{"limit":5}},
    {"id":"note","kind":"text","text":"<b>장애 대응</b> 절차는 RB04"}
  ],
  "layout": [
    {"widget_id":"rate","x":0,"y":0,"w":16,"h":6},
    {"widget_id":"p95","x":16,"y":0,"w":8,"h":3},
    {"widget_id":"top","x":16,"y":3,"w":8,"h":3},
    {"widget_id":"note","x":0,"y":6,"w":24,"h":2}
  ]
}`

func violations(t *testing.T, body string) (*ValidationError, map[string]string) {
	t.Helper()
	_, err := Normalize([]byte(body))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
	m := map[string]string{}
	for _, v := range ve.Violations {
		m[v.Field] = v.Reason
	}
	return ve, m
}

// 정규형: 기본값을 채우고(version, 표시 기본값, 빈 배열), 앞뒤 공백을 뗀다. text는 글자 그대로 둔다(해석하지 않는다).
func TestNormalizeValid(t *testing.T) {
	s, err := Normalize([]byte(validSpec))
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != 1 || s.Title != "Checkout" || len(s.Widgets) != 4 || len(s.Layout) != 4 || len(s.Variables) != 2 {
		t.Fatalf("spec = %+v", s)
	}
	ts := s.Widgets[0]
	if ts.Display.Stack == nil || *ts.Display.Stack || ts.Display.Limit != nil || *ts.Unit != "req/s" || ts.Query.Filter == nil {
		t.Errorf("time_series = %+v", ts)
	}
	if st := s.Widgets[1]; *st.Display.Decimals != DefaultDecimals || st.TimeOverride.Range != "4h" || len(st.Query.GroupBy) != 0 {
		t.Errorf("stat = %+v", st)
	}
	if top := s.Widgets[2]; *top.Display.Limit != 5 || *top.Display.Sort != "desc" {
		t.Errorf("top = %+v", top)
	}
	if note := s.Widgets[3]; note.Query != nil || *note.Text != "<b>장애 대응</b> 절차는 RB04" {
		t.Errorf("text = %+v", note)
	}
	if s.Variables[1].Default != nil || *s.Variables[0].Default != "prod" {
		t.Errorf("variables = %+v", s.Variables)
	}
	// 정규형은 다시 넣어도 같다
	again, err := Normalize(s.Canonical())
	if err != nil || string(again.Canonical()) != string(s.Canonical()) {
		t.Errorf("canonical round trip: %v", err)
	}
	empty, err := Normalize([]byte(`{"title":"빈 dashboard"}`))
	if err != nil || string(empty.Canonical()) != `{"version":1,"title":"빈 dashboard","description":"","variables":[],"layout":[],"widgets":[]}` {
		t.Errorf("empty = %s %v", empty.Canonical(), err)
	}
}

func TestNormalizeRejects(t *testing.T) {
	widget := func(extra string) string {
		return `{"title":"x","widgets":[{"id":"a","kind":"time_series","query":{"metric":"m","aggregation":"sum"}` + extra + `}],"layout":[{"widget_id":"a","x":0,"y":0,"w":6,"h":4}]}`
	}
	for name, c := range map[string]struct{ body, field string }{
		"unknown field":        {`{"title":"x","owner":"me"}`, "body"},
		"no title":             {`{}`, "title"},
		"control char title":   {`{"title":"a\u0007b"}`, "title"},
		"bad version":          {`{"title":"x","version":2}`, "version"},
		"unknown kind":         {`{"title":"x","widgets":[{"id":"a","kind":"pie"}],"layout":[{"widget_id":"a","x":0,"y":0,"w":1,"h":1}]}`, "widgets[0].kind"},
		"no query":             {`{"title":"x","widgets":[{"id":"a","kind":"stat"}],"layout":[{"widget_id":"a","x":0,"y":0,"w":1,"h":1}]}`, "widgets[0].query"},
		"bad aggregation":      {strings.Replace(widget(""), `"sum"`, `"median"`, 1), "widgets[0].query.aggregation"},
		"display not for kind": {widget(`,"display":{"limit":3}`), "widgets[0].display.limit"},
		"time override":        {widget(`,"time_override":{"range":"30d"}`), "widgets[0].time_override.range"},
		"text with query":      {`{"title":"x","widgets":[{"id":"a","kind":"text","text":"t","query":{"metric":"m","aggregation":"sum"}}],"layout":[{"widget_id":"a","x":0,"y":0,"w":1,"h":1}]}`, "widgets[0].query"},
		"stat group_by":        {`{"title":"x","widgets":[{"id":"a","kind":"stat","query":{"metric":"m","aggregation":"sum","group_by":["k"]}}],"layout":[{"widget_id":"a","x":0,"y":0,"w":1,"h":1}]}`, "widgets[0].query.group_by"},
		"top list no group":    {`{"title":"x","widgets":[{"id":"a","kind":"top_list","query":{"metric":"m","aggregation":"sum"}}],"layout":[{"widget_id":"a","x":0,"y":0,"w":1,"h":1}]}`, "widgets[0].query.group_by"},
		"unknown variable":     {`{"title":"x","widgets":[{"id":"a","kind":"stat","query":{"metric":"m","aggregation":"sum","filter":{"op":"eq","field":"service.name","value":"$svc"}}}],"layout":[{"widget_id":"a","x":0,"y":0,"w":1,"h":1}]}`, "widgets[0].query.filter.value"},
		"variable wrong field": {`{"title":"x","variables":[{"name":"env","kind":"environment"}],"widgets":[{"id":"a","kind":"stat","query":{"metric":"m","aggregation":"sum","filter":{"op":"eq","field":"service.name","value":"$env"}}}],"layout":[{"widget_id":"a","x":0,"y":0,"w":1,"h":1}]}`, "widgets[0].query.filter.value"},
		"duplicate variable":   {`{"title":"x","variables":[{"name":"env","kind":"environment"},{"name":"env","kind":"service"}]}`, "variables[1].name"},
		"bad variable name":    {`{"title":"x","variables":[{"name":"Env","kind":"environment"}]}`, "variables[0].name"},
		"missing placement":    {`{"title":"x","widgets":[{"id":"a","kind":"text","text":"t"}]}`, "layout"},
		"placement of nothing": {`{"title":"x","layout":[{"widget_id":"ghost","x":0,"y":0,"w":1,"h":1}]}`, "layout[0].widget_id"},
		"beyond 24 columns":    {`{"title":"x","widgets":[{"id":"a","kind":"text","text":"t"}],"layout":[{"widget_id":"a","x":20,"y":0,"w":6,"h":1}]}`, "layout[0]"},
		"zero width":           {`{"title":"x","widgets":[{"id":"a","kind":"text","text":"t"}],"layout":[{"widget_id":"a","x":0,"y":0,"w":0,"h":1}]}`, "layout[0].w"},
		"duplicate widget id":  {`{"title":"x","widgets":[{"id":"a","kind":"text","text":"t"},{"id":"a","kind":"text","text":"u"}],"layout":[{"widget_id":"a","x":0,"y":0,"w":1,"h":1}]}`, "widgets[1].id"},
		"overlap":              {`{"title":"x","widgets":[{"id":"a","kind":"text","text":"t"},{"id":"b","kind":"text","text":"u"}],"layout":[{"widget_id":"a","x":0,"y":0,"w":6,"h":4},{"widget_id":"b","x":5,"y":3,"w":6,"h":4}]}`, "layout[1]"},
		"filter too deep":      {`{"title":"x","widgets":[{"id":"a","kind":"stat","query":{"metric":"m","aggregation":"sum","filter":{"op":"and","args":[{"op":"and","args":[{"op":"and","args":[{"op":"and","args":[{"op":"eq","field":"f","value":"v"}]}]}]}]}}}],"layout":[{"widget_id":"a","x":0,"y":0,"w":1,"h":1}]}`, "widgets[0].query.filter.args[0].args[0].args[0].args[0]"},
	} {
		_, m := violations(t, c.body)
		if _, ok := m[c.field]; !ok {
			t.Errorf("%s: violations %v, want field %s", name, m, c.field)
		}
	}
}

// 50 위젯 상한(D02 §14). 정확히 50은 된다.
func TestWidgetLimit(t *testing.T) {
	build := func(n int) string {
		var ws, ls []string
		for i := range n {
			ws = append(ws, fmt.Sprintf(`{"id":"w%d","kind":"text","text":"t"}`, i))
			ls = append(ls, fmt.Sprintf(`{"widget_id":"w%d","x":0,"y":%d,"w":1,"h":1}`, i, i))
		}
		return `{"title":"x","widgets":[` + strings.Join(ws, ",") + `],"layout":[` + strings.Join(ls, ",") + `]}`
	}
	if _, err := Normalize([]byte(build(MaxWidgets))); err != nil {
		t.Errorf("50 widgets: %v", err)
	}
	if _, m := violations(t, build(MaxWidgets+1)); m["widgets"] == "" {
		t.Errorf("51 widgets: %v", m)
	}
}

// 아직 지원하지 않는 의미(histogram 위젯, team 변수, or filter)만 있으면 422(Unsupported)다. 다른 오류와 섞이면 400이다.
func TestUnsupported(t *testing.T) {
	ve, _ := violations(t, `{"title":"x","variables":[{"name":"t","kind":"team"}],"widgets":[{"id":"a","kind":"histogram"}],"layout":[{"widget_id":"a","x":0,"y":0,"w":1,"h":1}]}`)
	if !ve.Unsupported || len(ve.Violations) != 2 {
		t.Errorf("unsupported only: %+v", ve)
	}
	ve, _ = violations(t, `{"title":"","widgets":[{"id":"a","kind":"histogram"}],"layout":[{"widget_id":"a","x":0,"y":0,"w":1,"h":1}]}`)
	if ve.Unsupported {
		t.Errorf("mixed with invalid title must be 400: %+v", ve)
	}
}

func TestSpecBytesLimit(t *testing.T) {
	big, _ := json.Marshal(map[string]string{"title": strings.Repeat("a", MaxSpecBytes)})
	if _, m := violations(t, string(big)); m["body"] == "" {
		t.Errorf("oversize: %v", m)
	}
}

// 개수 상한을 넘는 본문은 항목을 하나씩 보지 않고 바로 거절한다: 겹치는 배치 수천 개가 오류 수천만 개를 만들지 않는다(PS-0013).
func TestOversizedCollectionsRejectFast(t *testing.T) {
	var ls []string
	for range 5000 {
		ls = append(ls, `{"widget_id":"a","x":0,"y":0,"w":1,"h":1}`)
	}
	body := `{"title":"x","widgets":[{"id":"a","kind":"text","text":"t"}],"layout":[` + strings.Join(ls, ",") + `]}`
	ve, m := violations(t, body)
	if len(ve.Violations) != 1 || m["layout"] == "" {
		t.Errorf("5000 placements: %d violations %v", len(ve.Violations), m)
	}
	// 상한 안에서 서로 겹치는 50개는 오류가 유한하다(50×49/2 이하)
	var ws, ls2 []string
	for i := range MaxWidgets {
		ws = append(ws, fmt.Sprintf(`{"id":"w%d","kind":"text","text":"t"}`, i))
		ls2 = append(ls2, fmt.Sprintf(`{"widget_id":"w%d","x":0,"y":0,"w":1,"h":1}`, i))
	}
	ve, _ = violations(t, `{"title":"x","widgets":[`+strings.Join(ws, ",")+`],"layout":[`+strings.Join(ls2, ",")+`]}`)
	if n := len(ve.Violations); n > MaxWidgets*(MaxWidgets-1)/2 {
		t.Errorf("overlap violations = %d", n)
	}
	var vs []string
	for i := range MaxVariables + 1 {
		vs = append(vs, fmt.Sprintf(`{"name":"v%d","kind":"service"}`, i))
	}
	if ve, m := violations(t, `{"title":"x","variables":[`+strings.Join(vs, ",")+`]}`); len(ve.Violations) != 1 || m["variables"] == "" {
		t.Errorf("11 variables: %v", m)
	}
}

// label·metric 이름에 제어 문자·잘못된 UTF-8은 저장하지 않는다.
func TestLabelsRejectControlCharacters(t *testing.T) {
	for name, q := range map[string]string{
		"metric newline": `{"metric":"m\nx","aggregation":"sum"}`,
		"group_by NUL":   `{"metric":"m","aggregation":"sum","group_by":["a\u0000b"]}`,
		"field tab":      `{"metric":"m","aggregation":"sum","filter":{"op":"eq","field":"a\tb","value":"v"}}`,
		"value bell":     `{"metric":"m","aggregation":"sum","filter":{"op":"eq","field":"f","value":"v\u0007"}}`,
	} {
		body := `{"title":"x","widgets":[{"id":"a","kind":"time_series","query":` + q + `}],"layout":[{"widget_id":"a","x":0,"y":0,"w":1,"h":1}]}`
		if _, err := Normalize([]byte(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// team 변수를 선언하고 참조만 하면 "아직 지원하지 않음"(422)이다 — 참조가 형식 오류(400)로 바뀌지 않는다.
func TestTeamVariableReferenceIsUnsupported(t *testing.T) {
	ve, _ := violations(t, `{"title":"x","variables":[{"name":"team","kind":"team"}],"widgets":[{"id":"a","kind":"stat","query":{"metric":"m","aggregation":"sum","filter":{"op":"eq","field":"team","value":"$team"}}}],"layout":[{"widget_id":"a","x":0,"y":0,"w":1,"h":1}]}`)
	if !ve.Unsupported {
		t.Errorf("team variable reference: %+v", ve)
	}
}
