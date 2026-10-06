package queryplan

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func parse(t *testing.T, s string) *Node {
	t.Helper()
	var n Node
	if err := json.Unmarshal([]byte(s), &n); err != nil {
		t.Fatal(err)
	}
	return &n
}

func TestCompileLogFilter(t *testing.T) {
	c, err := Compile(parse(t, `{"op":"and","args":[
		{"field":"severity_number","op":"gte","value":17},
		{"op":"or","args":[
			{"field":"body","op":"contains","value":"card declined"},
			{"field":"attributes.http.route","op":"eq","value":"/checkout"}]},
		{"field":"trace_id","op":"in","value":["4bf92f3577b34da6a3ce929d0e0e4736"]},
		{"field":"attributes.user.tier","op":"exists"}]}`), LogCatalog)
	if err != nil {
		t.Fatal(err)
	}
	want := "(severity >= {f0:Int64}) AND ((positionCaseInsensitiveUTF8(body, {f1:String}) > 0) OR " +
		"(mapContains(attributes, {f2:String}) AND attributes[{f2:String}] = {f3:String})) AND " +
		"(trace_id != unhex('00000000000000000000000000000000') AND has(arrayMap(x -> unhex(x), {f4:Array(String)}), trace_id)) AND " +
		"(mapContains(attributes, {f5:String}))"
	if c.SQL != want {
		t.Fatalf("sql =\n%s\nwant\n%s", c.SQL, want)
	}
	if c.Params["f1"] != "card declined" || c.Params["f2"] != "http.route" || c.Params["f0"] != int64(17) {
		t.Errorf("params = %v", c.Params)
	}
}

// 사용자 값은 SQL 문자열에 나타나지 않는다(계약 4). 인젝션 시도도 parameter 값일 뿐이다.
func TestUserValuesNeverInSQL(t *testing.T) {
	evil := `x') OR 1=1 --`
	c, err := Compile(parse(t, `{"op":"or","args":[
		{"field":"body","op":"contains","value":"`+strings.ReplaceAll(evil, `"`, `\"`)+`"},
		{"field":"attributes.k')OR(1=1)--","op":"eq","value":"v"}]}`), LogCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.SQL, "1=1") || strings.Contains(c.SQL, "'") {
		t.Fatalf("user input leaked into SQL: %s", c.SQL)
	}
}

func TestCompileRejects(t *testing.T) {
	deep := `{"field":"severity_number","op":"eq","value":1}`
	for i := 0; i < 5; i++ {
		deep = `{"op":"and","args":[` + deep + `]}`
	}
	many := make([]string, 21)
	for i := range many {
		many[i] = `{"field":"severity_number","op":"eq","value":1}`
	}
	bigIn := make([]string, 101)
	for i := range bigIn {
		bigIn[i] = `"x"`
	}
	for name, c := range map[string]struct{ filter, path string }{
		"unknown field":       {`{"field":"tenant_id","op":"eq","value":"x"}`, "filter.field"},
		"op not allowed":      {`{"field":"body","op":"eq","value":"x"}`, "filter.op"},
		"exists on column":    {`{"field":"trace_id","op":"exists"}`, "filter.op"},
		"too deep":            {deep, "filter.args[0].args[0].args[0].args[0]"},
		"too many leaves":     {`{"op":"and","args":[` + strings.Join(many, ",") + `]}`, "filter.args[20]"},
		"in too big":          {`{"field":"attributes.k","op":"in","value":[` + strings.Join(bigIn, ",") + `]}`, "filter.value"},
		"empty in":            {`{"field":"attributes.k","op":"in","value":[]}`, "filter.value"},
		"severity range":      {`{"field":"severity_number","op":"gt","value":25}`, "filter.value"},
		"severity not int":    {`{"field":"severity_number","op":"gt","value":"high"}`, "filter.value"},
		"bad trace id":        {`{"field":"trace_id","op":"eq","value":"XYZ"}`, "filter.value"},
		"zero trace id":       {`{"field":"trace_id","op":"eq","value":"00000000000000000000000000000000"}`, "filter.value"},
		"bad uuid":            {`{"field":"service_id","op":"eq","value":"not-a-uuid"}`, "filter.value"},
		"empty contains":      {`{"field":"body","op":"contains","value":""}`, "filter.value"},
		"long string":         {`{"field":"body","op":"contains","value":"` + strings.Repeat("a", 1025) + `"}`, "filter.value"},
		"empty attribute key": {`{"field":"attributes.","op":"eq","value":"v"}`, "filter.field"},
		"missing op":          {`{"field":"body","value":"x"}`, "filter.op"},
		"and without args":    {`{"op":"and"}`, "filter"},
		"leaf with args":      {`{"field":"body","op":"contains","value":"x","args":[{"op":"and"}]}`, "filter"},
	} {
		_, err := Compile(parse(t, c.filter), LogCatalog)
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Field != c.path {
			t.Errorf("%s: %v, want field %s", name, err, c.path)
		}
	}
}

func TestNilFilterAndCanonical(t *testing.T) {
	c, err := Compile(nil, LogCatalog)
	if err != nil || c.SQL != "1" {
		t.Fatalf("nil filter: %+v %v", c, err)
	}
	a, _ := Compile(parse(t, `{ "value": 3, "op":"gte",  "field":"severity_number" }`), LogCatalog)
	b, _ := Compile(parse(t, `{"field":"severity_number","op":"gte","value":3}`), LogCatalog)
	if a.Canonical != b.Canonical {
		t.Errorf("canonical differs: %s vs %s", a.Canonical, b.Canonical)
	}
}

// 없는 값은 비교 대상이 아니다(계약 6): 없는 map key는 eq ""·neq에 맞지 않고, 0 byte ID는 neq에 맞지 않는다.
func TestAbsentValuesNeverMatch(t *testing.T) {
	for filter, want := range map[string]string{
		`{"field":"attributes.k","op":"eq","value":""}`:                              "mapContains(attributes, {f0:String}) AND attributes[{f0:String}] = {f1:String}",
		`{"field":"attributes.k","op":"neq","value":"v"}`:                            "mapContains(attributes, {f0:String}) AND attributes[{f0:String}] != {f1:String}",
		`{"field":"trace_id","op":"neq","value":"4bf92f3577b34da6a3ce929d0e0e4736"}`: "trace_id != unhex('00000000000000000000000000000000') AND trace_id != unhex({f0:String})",
		`{"field":"span_id","op":"neq","value":"00f067aa0ba902b7"}`:                  "span_id != unhex('0000000000000000') AND span_id != unhex({f0:String})",
	} {
		c, err := Compile(parse(t, filter), LogCatalog)
		if err != nil || c.SQL != want {
			t.Errorf("%s:\n got %s (%v)\nwant %s", filter, c.SQL, err, want)
		}
	}
}

// service.name: 첫 컴파일은 이름만 모으고(Unresolved), 풀린 ID로 다시 컴파일하면 service_id 집합 비교다.
// catalog에 없는 이름은 빈 집합이라 eq·in은 맞는 행이 없고 neq는 모두 맞는다.
func TestCompileServiceName(t *testing.T) {
	f := parse(t, `{"op":"or","args":[
		{"field":"service.name","op":"in","value":["Checkout","payment","Checkout"]},
		{"field":"service.name","op":"neq","value":"ghost"}]}`)
	first, err := Compile(f, LogCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Unresolved || strings.Join(first.ServiceNames, ",") != "Checkout,payment,ghost" {
		t.Fatalf("first = %+v", first)
	}
	ids := map[string][]string{"Checkout": {"aaaaaaaa-0000-4000-8000-000000000001", "aaaaaaaa-0000-4000-8000-000000000002"}}
	c, err := CompileWith(f, LogCatalog, Options{ServiceIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	want := "(service_id IN {f0:Array(UUID)}) OR (service_id NOT IN {f1:Array(UUID)})"
	if c.SQL != want || c.Unresolved {
		t.Fatalf("sql = %s (unresolved=%v)", c.SQL, c.Unresolved)
	}
	// 같은 이름은 한 번만, 없는 이름(payment, ghost)은 아무 ID도 더하지 않는다.
	if got := c.Params["f0"].([]string); len(got) != 2 {
		t.Errorf("f0 = %v", got)
	}
	if got := c.Params["f1"].([]string); len(got) != 0 {
		t.Errorf("f1 = %v", got)
	}
	if c.Canonical != first.Canonical || strings.Contains(c.Canonical, "aaaaaaaa") {
		t.Errorf("canonical must hold names, not resolved IDs: %s", c.Canonical)
	}
	for _, bad := range []string{
		`{"field":"service.name","op":"eq","value":""}`,
		`{"field":"service.name","op":"contains","value":"check"}`,
		`{"field":"service.name","op":"eq","value":"` + strings.Repeat("x", 256) + `"}`,
		`{"field":"service.name","op":"in","value":[]}`,
	} {
		if _, err := Compile(parse(t, bad), LogCatalog); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
