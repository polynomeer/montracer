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
	want := "(severity >= {f0:Int64}) AND ((positionCaseInsensitiveUTF8(body, {f1:String}) > 0) OR (attributes[{f2:String}] = {f3:String})) AND " +
		"(has(arrayMap(x -> unhex(x), {f4:Array(String)}), trace_id)) AND (mapContains(attributes, {f5:String}))"
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
