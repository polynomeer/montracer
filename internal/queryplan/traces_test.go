package queryplan

import (
	"errors"
	"testing"
)

// trace 검색 filter는 span 단계와 trace 요약 단계로 나뉜다(ADR 0043).
func TestSplitTraceFilter(t *testing.T) {
	span, sum, err := Split(parse(t, `{"op":"and","args":[
		{"field":"service.name","op":"eq","value":"checkout"},
		{"field":"duration_ms","op":"gte","value":500},
		{"op":"or","args":[{"field":"status","op":"eq","value":2},{"field":"name","op":"contains","value":"charge"}]},
		{"field":"has_error","op":"eq","value":true}]}`), IsTraceSummaryField)
	if err != nil {
		t.Fatal(err)
	}
	if CanonicalOf(span) != `{"args":[{"field":"service.name","op":"eq","value":"checkout"},{"args":[{"field":"status","op":"eq","value":2},{"field":"name","op":"contains","value":"charge"}],"op":"or"}],"op":"and"}` {
		t.Errorf("span = %s", CanonicalOf(span))
	}
	if CanonicalOf(sum) != `{"args":[{"field":"duration_ms","op":"gte","value":500},{"field":"has_error","op":"eq","value":true}],"op":"and"}` {
		t.Errorf("summary = %s", CanonicalOf(sum))
	}
	// 한쪽뿐이면 다른 쪽은 nil, 잎 하나도 그대로
	if s, m, err := Split(parse(t, `{"field":"has_error","op":"eq","value":true}`), IsTraceSummaryField); err != nil || s != nil || m == nil || m.Field != "has_error" {
		t.Errorf("leaf = %v %v %v", s, m, err)
	}
	if s, m, err := Split(nil, IsTraceSummaryField); s != nil || m != nil || err != nil {
		t.Error("nil filter")
	}
}

// 두 단계 field를 or 안에서 섞으면 거절한다(span 하나인지 trace 전체인지 모호).
func TestSplitRejectsMixedLevels(t *testing.T) {
	for _, f := range []string{
		`{"op":"or","args":[{"field":"status","op":"eq","value":2},{"field":"has_error","op":"eq","value":true}]}`,
		`{"op":"and","args":[{"field":"name","op":"eq","value":"x"},{"op":"or","args":[{"field":"name","op":"eq","value":"y"},{"field":"duration_ms","op":"gt","value":1}]}]}`,
	} {
		_, _, err := Split(parse(t, f), IsTraceSummaryField)
		var fe *FieldError
		if !errors.As(err, &fe) {
			t.Errorf("%s: err = %v", f, err)
		}
	}
}

func TestCompileTraceCatalogs(t *testing.T) {
	c, err := CompileWith(parse(t, `{"op":"and","args":[{"field":"duration_ms","op":"gte","value":500},{"field":"has_error","op":"eq","value":true}]}`),
		TraceSummaryCatalog, Options{ParamPrefix: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if c.SQL != "(duration_ms >= {t0:Int64}) AND (has_error = {t1:Bool})" || c.Params["t1"] != true || c.Params["t0"] != int64(500) {
		t.Errorf("compiled = %+v", c)
	}
	s, err := CompileWith(parse(t, `{"op":"and","args":[{"field":"status","op":"eq","value":2},{"field":"name","op":"contains","value":"charge"}]}`),
		TraceSpanCatalog, Options{ParamPrefix: "s"})
	if err != nil || s.SQL != "(status = {s0:Int64}) AND (positionCaseInsensitiveUTF8(name, {s1:String}) > 0)" {
		t.Errorf("span compiled = %+v %v", s, err)
	}
	for _, bad := range []string{
		`{"field":"has_error","op":"eq","value":"true"}`, // 문자열은 bool이 아니다
		`{"field":"has_error","op":"in","value":[true]}`, // bool에 in 없음
		`{"field":"duration_ms","op":"gte","value":-1}`,
		`{"field":"duration_ms","op":"eq","value":5}`, // 실수 시간에 eq는 의미가 약하다
		`{"field":"status","op":"eq","value":3}`,
	} {
		cat := TraceSummaryCatalog
		if !IsTraceSummaryField(parse(t, bad).Field) {
			cat = TraceSpanCatalog
		}
		if _, err := Compile(parse(t, bad), cat); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if _, err := CompileWith(nil, TraceSpanCatalog, Options{ParamPrefix: "Bad;"}); err != nil {
		t.Error("nil filter needs no prefix check")
	}
	if _, err := CompileWith(parse(t, `{"field":"status","op":"eq","value":1}`), TraceSpanCatalog, Options{ParamPrefix: "Bad;"}); err == nil {
		t.Error("invalid prefix accepted")
	}
}
