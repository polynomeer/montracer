// Package queryplan은 조회 filter JSON AST를 검증하고 SQL 조건 조각으로 컴파일한다 (D02 §15, §19, ADR 0037).
//
//   - 허용 연산자: and, or (깊이 ≤ 4), eq, neq, in, exists, gt, gte, lt, lte, contains
//   - 한도: 조건(leaf) ≤ 20, in 값 ≤ 100, 문자열 값 ≤ 1,024자
//   - field는 signal별 catalog에 있는 것만 받는다. column·타입·허용 연산자는 catalog가 정한다.
//   - 사용자 값은 모두 서버 측 query parameter(`{name:Type}`)로만 들어간다. SQL에 이어 붙이는 것은 catalog의 고정 column
//     표현뿐이다(계약 4).
//   - tenant·시간·권한 scope·expires_at은 **여기서 다루지 않는다.** AST 밖의 mandatory predicate로 저장소가 넣는다.
//     이 패키지가 만든 조각만으로는 query가 되지 않는다(telemetrystore가 감싼다).
package queryplan

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// 한도 (D02 §15).
const (
	MaxDepth      = 4
	MaxLeaves     = 20
	MaxInValues   = 100
	MaxStringLen  = 1024
	maxMapKeyLen  = 255
	mapFieldDelim = "."
)

// Node는 filter AST 노드다. and·or는 Args, 나머지는 Field·Value를 쓴다.
type Node struct {
	Op    string          `json:"op"`
	Args  []Node          `json:"args,omitempty"`
	Field string          `json:"field,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// FieldError는 입력 오류다. Field는 AST 안의 경로다(예: filter.args[1].value).
type FieldError struct {
	Field, Reason string
}

func (e *FieldError) Error() string { return "queryplan: " + e.Field + ": " + e.Reason }

func fieldErr(path, reason string, a ...any) error {
	return &FieldError{Field: path, Reason: fmt.Sprintf(reason, a...)}
}

// Kind는 catalog field의 값 타입이다.
type Kind int

const (
	// String은 일반 문자열 column이다.
	String Kind = iota + 1
	// Int는 0 이상 정수 column이다(Min·Max로 범위 제한).
	Int
	// TraceID는 hex 32자(FixedString(16)) column이다. 모두 0인 값은 "없음"이라 받지 않는다.
	TraceID
	// SpanID는 hex 16자(FixedString(8)) column이다.
	SpanID
	// UUID는 UUID column이다.
	UUID
	// Text는 본문 검색 전용(contains만) column이다.
	Text
	// MapString은 Map(String,String) column이다. field는 "<prefix>.<key>"다.
	MapString
)

// Field는 catalog 항목이다.
type Field struct {
	Name   string
	Column string // 고정 SQL 표현(사용자 입력 아님)
	Kind   Kind
	Ops    []string
	// Min·Max는 Int 범위다.
	Min, Max int64
}

// Catalog는 signal 하나의 filter 가능 field다.
type Catalog struct {
	fields map[string]Field
	// maps는 "<prefix>." → MapString field다(예: "attributes." → attributes column).
	maps map[string]Field
}

// NewCatalog는 field 목록으로 catalog를 만든다.
func NewCatalog(fields ...Field) Catalog {
	c := Catalog{fields: map[string]Field{}, maps: map[string]Field{}}
	for _, f := range fields {
		if f.Kind == MapString {
			c.maps[f.Name+mapFieldDelim] = f
			continue
		}
		c.fields[f.Name] = f
	}
	return c
}

func (c Catalog) lookup(name string) (Field, string, bool) {
	if f, ok := c.fields[name]; ok {
		return f, "", true
	}
	for prefix, f := range c.maps {
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
			return f, name[len(prefix):], true
		}
	}
	return Field{}, "", false
}

// Compiled는 컴파일 결과다. SQL은 저장소가 mandatory predicate와 AND로 묶는다.
type Compiled struct {
	SQL    string         // 조건이 없으면 "1"
	Params map[string]any // 이름은 "f<n>" (저장소 parameter와 겹치지 않게)
	// Canonical은 정규화한 AST JSON이다(cursor query hash·cache key용).
	Canonical string
}

// Compile은 filter를 검증하고 SQL 조각으로 바꾼다. filter가 nil이면 조건 없음("1")이다.
func Compile(filter *Node, cat Catalog) (Compiled, error) {
	if filter == nil {
		return Compiled{SQL: "1", Params: map[string]any{}, Canonical: "null"}, nil
	}
	c := &compiler{cat: cat, params: map[string]any{}}
	sql, err := c.node(*filter, "filter", 1)
	if err != nil {
		return Compiled{}, err
	}
	canon, err := json.Marshal(canonical(*filter))
	if err != nil {
		return Compiled{}, fmt.Errorf("queryplan: canonical: %w", err)
	}
	return Compiled{SQL: sql, Params: c.params, Canonical: string(canon)}, nil
}

type compiler struct {
	cat    Catalog
	params map[string]any
	leaves int
}

func (c *compiler) param(v any) string {
	name := "f" + strconv.Itoa(len(c.params))
	c.params[name] = v
	return name
}

func (c *compiler) node(n Node, path string, depth int) (string, error) {
	switch n.Op {
	case "and", "or":
		if depth > MaxDepth {
			return "", fieldErr(path, "and/or nesting deeper than %d", MaxDepth)
		}
		if len(n.Args) == 0 || n.Field != "" || len(n.Value) > 0 {
			return "", fieldErr(path, "%s needs args and no field/value", n.Op)
		}
		parts := make([]string, 0, len(n.Args))
		for i, a := range n.Args {
			s, err := c.node(a, fmt.Sprintf("%s.args[%d]", path, i), depth+1)
			if err != nil {
				return "", err
			}
			parts = append(parts, "("+s+")")
		}
		return strings.Join(parts, " "+strings.ToUpper(n.Op)+" "), nil
	case "":
		return "", fieldErr(path+".op", "required")
	}
	c.leaves++
	if c.leaves > MaxLeaves {
		return "", fieldErr(path, "more than %d conditions", MaxLeaves)
	}
	if len(n.Args) > 0 {
		return "", fieldErr(path, "%s takes field and value, not args", n.Op)
	}
	f, mapKey, ok := c.cat.lookup(n.Field)
	if !ok {
		return "", fieldErr(path+".field", "unknown field %q", n.Field)
	}
	if !allowed(f.Ops, n.Op) {
		return "", fieldErr(path+".op", "%q is not allowed for %s (allowed: %s)", n.Op, n.Field, strings.Join(f.Ops, ", "))
	}
	col := f.Column
	if f.Kind == MapString {
		if err := checkMapKey(mapKey); err != nil {
			return "", fieldErr(path+".field", "%s", err)
		}
		if n.Op == "exists" {
			return fmt.Sprintf("mapContains(%s, {%s:String})", f.Column, c.param(mapKey)), nil
		}
		col = fmt.Sprintf("%s[{%s:String}]", f.Column, c.param(mapKey))
	}
	if n.Op == "exists" {
		return "", fieldErr(path+".op", "exists is only for map fields")
	}
	vpath := path + ".value"
	if n.Op == "in" {
		var raw []json.RawMessage
		if err := json.Unmarshal(n.Value, &raw); err != nil || len(raw) == 0 {
			return "", fieldErr(vpath, "in needs a non-empty array")
		}
		if len(raw) > MaxInValues {
			return "", fieldErr(vpath, "in takes at most %d values", MaxInValues)
		}
		return c.inExpr(f, col, raw, vpath)
	}
	v, typ, err := scalar(f, n.Value, vpath)
	if err != nil {
		return "", err
	}
	name := c.param(v)
	switch n.Op {
	case "eq", "neq", "gt", "gte", "lt", "lte":
		return fmt.Sprintf("%s %s %s", col, sqlOp[n.Op], placeholder(f, name, typ)), nil
	case "contains":
		if v.(string) == "" {
			return "", fieldErr(vpath, "contains needs a non-empty string")
		}
		// 대소문자 무시 부분 문자열 (D02 §15 MVP: substring·token, regex 없음)
		return fmt.Sprintf("positionCaseInsensitiveUTF8(%s, {%s:String}) > 0", col, name), nil
	}
	return "", fieldErr(path+".op", "unknown operator %q", n.Op)
}

var sqlOp = map[string]string{"eq": "=", "neq": "!=", "gt": ">", "gte": ">=", "lt": "<", "lte": "<="}

// placeholder는 값 parameter를 column 타입에 맞는 표현으로 만든다.
func placeholder(f Field, name, typ string) string {
	switch f.Kind {
	case TraceID, SpanID:
		return "unhex({" + name + ":String})"
	case UUID:
		return "toUUID({" + name + ":String})"
	default:
		return "{" + name + ":" + typ + "}"
	}
}

func (c *compiler) inExpr(f Field, col string, raw []json.RawMessage, path string) (string, error) {
	var (
		strs []string
		ints []int64
	)
	for i, r := range raw {
		v, _, err := scalar(f, r, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return "", err
		}
		switch x := v.(type) {
		case string:
			strs = append(strs, x)
		case int64:
			ints = append(ints, x)
		}
	}
	switch f.Kind {
	case Int:
		return fmt.Sprintf("has({%s:Array(Int64)}, toInt64(%s))", c.param(ints), col), nil
	case TraceID, SpanID:
		return fmt.Sprintf("has(arrayMap(x -> unhex(x), {%s:Array(String)}), %s)", c.param(strs), col), nil
	case UUID:
		return fmt.Sprintf("has(arrayMap(x -> toUUID(x), {%s:Array(String)}), %s)", c.param(strs), col), nil
	default:
		return fmt.Sprintf("has({%s:Array(String)}, %s)", c.param(strs), col), nil
	}
}

// scalar는 값 하나를 field 타입으로 검증해 parameter 값과 ClickHouse 타입 이름을 돌려준다.
func scalar(f Field, raw json.RawMessage, path string) (any, string, error) {
	if len(raw) == 0 {
		return nil, "", fieldErr(path, "required")
	}
	switch f.Kind {
	case Int:
		var n json.Number
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if err := dec.Decode(&n); err != nil {
			return nil, "", fieldErr(path, "must be an integer")
		}
		v, err := n.Int64()
		if err != nil || v < f.Min || v > f.Max {
			return nil, "", fieldErr(path, "must be an integer in [%d, %d]", f.Min, f.Max)
		}
		return v, "Int64", nil
	default:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, "", fieldErr(path, "must be a string")
		}
		if len([]rune(s)) > MaxStringLen {
			return nil, "", fieldErr(path, "at most %d characters", MaxStringLen)
		}
		switch f.Kind {
		case TraceID:
			if !isHex(s, 32) || s == strings.Repeat("0", 32) {
				return nil, "", fieldErr(path, "must be 32 lowercase hex characters, not all zeros")
			}
		case SpanID:
			if !isHex(s, 16) || s == strings.Repeat("0", 16) {
				return nil, "", fieldErr(path, "must be 16 lowercase hex characters, not all zeros")
			}
		case UUID:
			if !isUUID(s) {
				return nil, "", fieldErr(path, "must be a lowercase UUID")
			}
		}
		return s, "String", nil
	}
}

func isHex(s string, n int) bool {
	if len(s) != n || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func isUUID(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	return isHex(s[0:8]+s[9:13]+s[14:18]+s[19:23]+s[24:], 32)
}

func checkMapKey(k string) error {
	if k == "" || len(k) > maxMapKeyLen {
		return errors.New("attribute key must be 1..255 bytes")
	}
	for _, r := range k {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return errors.New("attribute key has non-printable or space characters")
		}
	}
	return nil
}

func allowed(ops []string, op string) bool {
	for _, o := range ops {
		if o == op {
			return true
		}
	}
	return false
}

// canonical은 cursor·cache key용 정규형이다(값은 원문 그대로, 공백·key 순서만 정리).
func canonical(n Node) any {
	if n.Op == "and" || n.Op == "or" {
		args := make([]any, len(n.Args))
		for i, a := range n.Args {
			args[i] = canonical(a)
		}
		return map[string]any{"op": n.Op, "args": args}
	}
	var v any
	_ = json.Unmarshal(n.Value, &v)
	return map[string]any{"op": n.Op, "field": n.Field, "value": v}
}
