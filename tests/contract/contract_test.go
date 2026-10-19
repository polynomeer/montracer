package contract

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polynomeer/montracer/api/openapi"
)

// 원천은 올바른 OpenAPI 3.1이고, operation마다 고유한 operationId와 담당 service가 있다.
func TestSpecIsValid(t *testing.T) {
	ops, err := Operations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) == 0 {
		t.Fatal("no operations")
	}
	ids := map[string]bool{}
	for _, o := range ops {
		if o.ID == "" || ids[o.ID] {
			t.Errorf("%s: operationId %q missing or duplicated", o.Pattern(), o.ID)
		}
		ids[o.ID] = true
		if o.Service != "query-api" && o.Service != "control-api" {
			t.Errorf("%s: %s = %q", o.Pattern(), ServiceExtension, o.Service)
		}
		if !strings.HasPrefix(o.Path, "/api/v1/") {
			t.Errorf("%s: path must be under /api/v1", o.Pattern())
		}
	}
}

func exchange(t *testing.T, method, target string, status int, body string) (string, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, target, nil)
	req.Header.Set("Authorization", "Bearer x")
	resp := &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: req}
	pattern, violation, err := check(req, nil, resp)
	if err != nil {
		t.Fatal(err)
	}
	return pattern, violation
}

const meta = `"meta":{"request_id":"r","schema_version":1,"partial":false,"failed_shards":[],"watermark":null,"sampled":null,` +
	`"coverage":null,"resolution_seconds":null,"scan_bytes":null,"warnings":[]}`

const service = `{"service_id":"aaaaaaaa-0000-4000-8000-000000000001","name":"checkout","namespace":"","environment":"prod",` +
	`"language":null,"status":"active","first_seen":"2026-10-07T00:00:00Z","last_seen":"2026-10-07T00:00:00Z","owner_team":null,` +
	`"repository_url":null,"runbook_url":null,"tier":null,"tags":[]}`

// 검사는 실제로 어긋남을 잡는다(헛돌지 않는다): 빠진 필수 field, 선언 안 된 field, 잘못된 enum·타입, 선언 안 된 status.
func TestCheckCatchesDrift(t *testing.T) {
	ok := `{"data":` + service + `,` + meta + `}`
	if p, v := exchange(t, http.MethodGet, "/api/v1/services/aaaaaaaa-0000-4000-8000-000000000001", 200, ok); p == "" || v != "" {
		t.Fatalf("valid exchange: pattern=%q violation=%s", p, v)
	}
	for name, body := range map[string]string{
		"missing required": strings.Replace(ok, `"status":"active",`, "", 1),
		"undeclared field": strings.Replace(ok, `"tags":[]`, `"tags":[],"secret":"x"`, 1),
		"bad enum":         strings.Replace(ok, `"status":"active"`, `"status":"gone"`, 1),
		"null for string":  strings.Replace(ok, `"name":"checkout"`, `"name":null`, 1),
		"zero for null":    strings.Replace(ok, `"coverage":null`, `"coverage":"0"`, 1),
	} {
		if _, v := exchange(t, http.MethodGet, "/api/v1/services/aaaaaaaa-0000-4000-8000-000000000001", 200, body); v == "" {
			t.Errorf("%s: not caught", name)
		}
	}
	if _, v := exchange(t, http.MethodGet, "/api/v1/services/aaaaaaaa-0000-4000-8000-000000000001", 418, `{}`); v == "" {
		t.Error("undeclared status not caught")
	}
	// 원천에 없는 경로가 성공하면 위반, 404면 정상(없는 경로)
	if _, v := exchange(t, http.MethodGet, "/api/v1/nope", 200, `{}`); v == "" {
		t.Error("unspecified path served 200 without a violation")
	}
	if _, v := exchange(t, http.MethodGet, "/api/v1/nope", 404, `{}`); v != "" {
		t.Errorf("404 on unknown path: %s", v)
	}
}

// 등록 route와 원천의 차이를 양쪽 모두 알려 준다.
func TestCheckRoutes(t *testing.T) {
	ops, err := Operations()
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	for _, o := range ops {
		if o.Service == "query-api" {
			routes = append(routes, o.Pattern())
		}
	}
	if err := CheckRoutes("query-api", routes); err != nil {
		t.Fatal(err)
	}
	err = CheckRoutes("query-api", append(routes[1:], "GET /api/v1/hidden"))
	if err == nil || !strings.Contains(err.Error(), "GET /api/v1/hidden") || !strings.Contains(err.Error(), routes[0]) {
		t.Errorf("diff not reported both ways: %v", err)
	}
}

// 호환 판정: 응답 field 삭제·operation 삭제·요청 필수 field 추가는 깨고, 응답 field 추가·설명 변경은 깨지 않는다.
func TestBreaking(t *testing.T) {
	base := openapi.Spec
	same, err := Breaking(base, base)
	if err != nil || len(same) != 0 {
		t.Fatalf("same spec: %v %v", same, err)
	}
	mutate := func(old, new string) []byte {
		t.Helper()
		if !bytes.Contains(base, []byte(old)) {
			t.Fatalf("fixture text not in spec: %q", old)
		}
		return bytes.Replace(base, []byte(old), []byte(new), 1)
	}
	breaking := map[string][]byte{
		"remove response property": mutate("        tier: {type: [string, 'null']}\n", ""),
		"remove operation":         mutate("  /api/v1/services/{service_id}:\n", "  /api/v1/services-renamed/{service_id}:\n"),
		"narrow type":              mutate("        span_count: {type: integer, minimum: 0}\n        complete:", "        span_count: {type: string}\n        complete:"),
	}
	for name, head := range breaking {
		if got, err := Breaking(base, head); err != nil || len(got) == 0 {
			t.Errorf("%s: not reported as breaking (%v)", name, err)
		}
	}
	compatible := map[string][]byte{
		"add response property": mutate("        tags:\n          type: array\n          items: {type: string}\n",
			"        tags:\n          type: array\n          items: {type: string}\n        slo_status: {type: [string, 'null']}\n"),
		"change description": mutate("summary: 서비스 하나 (ADR 0042)", "summary: 서비스 한 개 (ADR 0042)"),
	}
	for name, head := range compatible {
		if got, err := Breaking(base, head); err != nil || len(got) != 0 {
			t.Errorf("%s: reported as breaking: %v %v", name, got, err)
		}
	}
}
