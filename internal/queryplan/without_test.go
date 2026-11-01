package queryplan

import (
	"strings"
	"testing"
)

func ownedBy(fields ...string) func(string) bool {
	return func(f string) bool {
		for _, x := range fields {
			if x == f {
				return true
			}
		}
		return false
	}
}

// facet의 소유 조건만 뺀다(ADR 0074 §3). 연산자는 가리지 않고, 다른 field와 섞인 subtree는 남긴다.
func TestWithout(t *testing.T) {
	service := ownedBy("service_id", "service.name")
	cases := []struct {
		name, in, want string
		removed        bool
	}{
		{"최상위 and의 소유 leaf", `{"op":"and","args":[
			{"field":"service.name","op":"in","value":["a","b"]},
			{"field":"severity_number","op":"gte","value":17}]}`,
			`{"field":"severity_number","op":"gte","value":17}`, true},
		{"같은 facet의 두 field·소유 or", `{"op":"and","args":[
			{"field":"service_id","op":"eq","value":"00000000-0000-0000-0000-000000000001"},
			{"op":"or","args":[{"field":"service.name","op":"eq","value":"a"},{"field":"service.name","op":"neq","value":"b"}]},
			{"field":"body","op":"contains","value":"x"},
			{"field":"trace_id","op":"exists"}]}`,
			`{"args":[{"field":"body","op":"contains","value":"x"},{"field":"trace_id","op":"exists","value":null}],"op":"and"}`, true},
		{"섞인 or는 남긴다", `{"op":"and","args":[
			{"op":"or","args":[{"field":"service.name","op":"eq","value":"a"},{"field":"severity_number","op":"gte","value":17}]}]}`,
			`{"args":[{"args":[{"field":"service.name","op":"eq","value":"a"},{"field":"severity_number","op":"gte","value":17}],"op":"or"}],"op":"and"}`, false},
		{"최상위 소유 leaf 하나", `{"field":"service.name","op":"eq","value":"a"}`, `null`, true},
		{"최상위 소유 or", `{"op":"or","args":[{"field":"service.name","op":"eq","value":"a"},{"field":"service_id","op":"eq","value":"00000000-0000-0000-0000-000000000001"}]}`, `null`, true},
		{"다 빼면 조건 없음", `{"op":"and","args":[{"field":"service.name","op":"eq","value":"a"},{"field":"service_id","op":"neq","value":"00000000-0000-0000-0000-000000000001"}]}`, `null`, true},
		{"소유 조건 없음", `{"field":"body","op":"contains","value":"x"}`, `{"field":"body","op":"contains","value":"x"}`, false},
	}
	for _, c := range cases {
		got, removed := Without(parse(t, c.in), service)
		if CanonicalOf(got) != c.want || removed != c.removed {
			t.Errorf("%s: got %s %v, want %s %v", c.name, CanonicalOf(got), removed, c.want, c.removed)
		}
	}
	if got, removed := Without(nil, service); got != nil || removed {
		t.Error("nil filter")
	}
	// 범위 조건도 소유 field면 뺀다(severity "error 또는 fatal" = gte 17)
	got, removed := Without(parse(t, `{"op":"and","args":[{"field":"severity_number","op":"gte","value":17},{"field":"service.name","op":"eq","value":"a"}]}`), ownedBy("severity_number"))
	if !removed || CanonicalOf(got) != `{"field":"service.name","op":"eq","value":"a"}` {
		t.Errorf("range: %s", CanonicalOf(got))
	}
	// attributes.<key>는 같은 key만 소유한다
	got, _ = Without(parse(t, `{"op":"and","args":[{"field":"attributes.a","op":"eq","value":"1"},{"field":"attributes.b","op":"eq","value":"2"}]}`), ownedBy("attributes.a"))
	if CanonicalOf(got) != `{"field":"attributes.b","op":"eq","value":"2"}` {
		t.Errorf("attribute key: %s", CanonicalOf(got))
	}
}

// root_service_id는 요약 단계 field다(ADR 0074 §3). span 단계 service.name과 따로 나뉜다.
func TestRootServiceIDSummaryField(t *testing.T) {
	span, sum, err := Split(parse(t, `{"op":"and","args":[
		{"field":"service.name","op":"eq","value":"checkout"},
		{"field":"root_service_id","op":"in","value":["00000000-0000-0000-0000-000000000001"]}]}`), IsTraceSummaryField)
	if err != nil || span == nil || span.Field != "service.name" || sum == nil || sum.Field != "root_service_id" {
		t.Fatalf("split: %v %v %v", span, sum, err)
	}
	c, err := CompileWith(sum, TraceSummaryCatalog, Options{ParamPrefix: "t"})
	if err != nil || !strings.Contains(c.SQL, "root_service_id") || !strings.Contains(c.SQL, "{t0:Array(String)}") {
		t.Fatalf("compile: %+v %v", c, err)
	}
	if _, err := Compile(parse(t, `{"field":"root_service_id","op":"neq","value":"00000000-0000-0000-0000-000000000001"}`), TraceSummaryCatalog); err == nil {
		t.Error("root_service_id takes eq and in only")
	}
}
