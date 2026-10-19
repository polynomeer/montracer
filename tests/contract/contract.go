// Package contract는 HTTP 교환을 OpenAPI 원천(api/openapi)으로 검사한다 (D02 §12, §20, D06 §11, ADR 0059).
//
// handler 시험과 격리 시험이 주고받은 요청·응답을 기록하고, 기록할 때마다 검사한다.
//   - 응답은 항상 검사한다: 상태 코드가 그 operation에 선언돼 있고 본문이 schema에 맞아야 한다(선언 안 된 property도 거절).
//   - 요청은 서버가 받아들인 경우(2xx·3xx)만 검사한다. 거절된 요청은 negative 시험이 일부러 만든 잘못된 입력이다.
//     서버가 받아들인 요청이 schema에 어긋나면 schema가 너무 좁거나 서버가 너무 느슨하다.
//   - Run은 package 시험이 끝난 뒤 위반을 보고하고, 그 service의 모든 operation이 검사된 성공 응답을 한 번 이상 가졌는지 본다.
package contract

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
	"github.com/pb33f/libopenapi-validator/config"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"

	"github.com/polynomeer/montracer/api/openapi"
)

// ServiceExtension은 operation이 어느 service(cmd/<service>)의 것인지 적는 확장 필드다.
const ServiceExtension = "x-montracer-service"

// Operation은 OpenAPI operation 하나다.
type Operation struct {
	Method, Path, Service, ID string
}

// Pattern은 net/http ServeMux pattern("METHOD /path")이다. OpenAPI path template과 같은 문법이다.
func (o Operation) Pattern() string { return o.Method + " " + o.Path }

var (
	loadOnce   sync.Once
	loadErr    error
	specModel  *v3.Document
	operations []Operation
	opMux      *http.ServeMux

	mu         sync.Mutex
	val        validator.Validator
	violations []string
	covered    = map[string]bool{} // 검사를 통과한 성공 응답이 있는 operation pattern
)

func load() error {
	loadOnce.Do(func() {
		doc, err := libopenapi.NewDocument(openapi.Spec)
		if err != nil {
			loadErr = fmt.Errorf("contract: parse spec: %w", err)
			return
		}
		m, err := doc.BuildV3Model()
		if err != nil {
			loadErr = fmt.Errorf("contract: build model: %w", err)
			return
		}
		specModel = &m.Model
		v, errs := validator.NewValidator(doc, config.WithStrictMode(), config.WithFormatAssertions(),
			// 모든 응답의 공통 header는 operation마다 적지 않는다(info.description에 적었다).
			config.WithStrictIgnoredHeadersExtra("X-Request-ID", "X-Content-Type-Options"))
		if len(errs) > 0 {
			loadErr = fmt.Errorf("contract: validator: %v", errs)
			return
		}
		if ok, verrs := v.ValidateDocument(); !ok {
			msgs := make([]string, 0, len(verrs))
			for _, e := range verrs {
				msgs = append(msgs, e.Error())
			}
			loadErr = fmt.Errorf("contract: spec is not valid OpenAPI: %s", strings.Join(msgs, "; "))
			return
		}
		val = v
		opMux = http.NewServeMux()
		for path, item := range specModel.Paths.PathItems.FromOldest() {
			for method, op := range item.GetOperations().FromOldest() {
				o := Operation{Method: strings.ToUpper(method), Path: path, ID: op.OperationId}
				if ext, ok := op.Extensions.Get(ServiceExtension); ok && ext != nil {
					o.Service = ext.Value
				}
				operations = append(operations, o)
				opMux.HandleFunc(o.Pattern(), func(http.ResponseWriter, *http.Request) {})
			}
		}
	})
	return loadErr
}

// Operations는 원천의 operation 목록이다.
func Operations() ([]Operation, error) {
	if err := load(); err != nil {
		return nil, err
	}
	return append([]Operation(nil), operations...), nil
}

// CheckRoutes는 service가 등록한 route와 원천의 operation이 정확히 같은지 본다(빠진 쪽을 모두 알려 준다).
func CheckRoutes(service string, routes []string) error {
	ops, err := Operations()
	if err != nil {
		return err
	}
	spec := map[string]bool{}
	for _, o := range ops {
		if o.Service == service {
			spec[o.Pattern()] = true
		}
	}
	code := map[string]bool{}
	for _, r := range routes {
		code[r] = true
	}
	var missing, extra []string
	for r := range code {
		if !spec[r] {
			missing = append(missing, r)
		}
	}
	for r := range spec {
		if !code[r] {
			extra = append(extra, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing)+len(extra) > 0 {
		return fmt.Errorf("contract: %s routes and OpenAPI differ\n  registered but not in spec: %v\n  in spec (%s=%s) but not registered: %v",
			service, missing, ServiceExtension, service, extra)
	}
	return nil
}

// Serve는 h.ServeHTTP(rec, req)를 실행하고 교환을 기록·검사한다. handler 시험의 요청 helper가 쓴다.
func Serve(h http.Handler, rec *httptest.ResponseRecorder, req *http.Request) {
	body := readBody(req)
	h.ServeHTTP(rec, req)
	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	record(req, body, res)
}

// Wrap은 교환을 기록·검사하는 handler다(httptest.NewServer로 띄우는 통합·격리 시험용).
func Wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		for k, vs := range rec.Header() {
			w.Header()[k] = vs
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		res := rec.Result()
		defer func() { _ = res.Body.Close() }()
		record(r, body, res)
	})
}

func readBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	b, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b
}

func record(req *http.Request, body []byte, resp *http.Response) {
	pattern, violation, err := check(req, body, resp)
	mu.Lock()
	defer mu.Unlock()
	switch {
	case err != nil:
		violations = append(violations, err.Error())
	case violation != "":
		violations = append(violations, violation)
	case pattern != "" && resp.StatusCode < 400:
		covered[pattern] = true
	}
}

// check는 교환 하나를 검사한다. pattern은 맞은 operation("" = 원천에 없는 경로), violation은 위반 설명("" = 통과)이다.
func check(req *http.Request, body []byte, resp *http.Response) (pattern, violation string, err error) {
	if err := load(); err != nil {
		return "", "", err
	}
	probe := req.Clone(req.Context())
	probe.Body = io.NopCloser(bytes.NewReader(body))
	_, pattern = opMux.Handler(probe)
	where := req.Method + " " + req.URL.Path
	if pattern == "" {
		// 원천에 없는 경로: route parity가 등록된 route를 막으므로, 여기 오는 것은 없는 경로·method의 404·405뿐이어야 한다.
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			return "", fmt.Sprintf("%s → %d: path is not in the OpenAPI spec", where, resp.StatusCode), nil
		}
		return "", "", nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	c := req.Clone(req.Context())
	c.Body = io.NopCloser(bytes.NewReader(body))
	mu.Lock() // validator의 schema cache는 동시 사용을 가정하지 않는다
	defer mu.Unlock()
	var errs []string
	if ok, verrs := val.ValidateHttpResponse(c, resp); !ok {
		for _, e := range verrs {
			errs = append(errs, "response: "+e.Error())
		}
	}
	if resp.StatusCode < 400 {
		c = req.Clone(req.Context())
		c.Body = io.NopCloser(bytes.NewReader(body))
		if ok, verrs := val.ValidateHttpRequest(c); !ok {
			for _, e := range verrs {
				errs = append(errs, "request: "+e.Error())
			}
		}
	}
	if len(errs) > 0 {
		return pattern, fmt.Sprintf("%s → %d\n      %s\n      body: %s", where, resp.StatusCode, strings.Join(errs, "\n      "), clip(respBody)), nil
	}
	return pattern, "", nil
}

func clip(b []byte) string {
	const n = 400
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}

// Run은 TestMain에서 m.Run을 감싼다: 시험이 끝나면 기록된 위반을 보고하고,
// service의 operation마다 검사를 통과한 성공 응답이 있었는지 본다(service가 ""이면 coverage는 보지 않는다).
func Run(m *testing.M, service string) int {
	code := m.Run()
	if err := load(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	mu.Lock()
	defer mu.Unlock()
	if len(violations) > 0 {
		_, _ = fmt.Fprintf(os.Stderr, "\nOpenAPI contract violations (api/openapi/montracer.yaml, ADR 0059): %d\n", len(violations))
		for _, v := range violations {
			_, _ = fmt.Fprintln(os.Stderr, "  - "+v)
		}
		code = 1
	}
	// -run으로 일부 시험만 돌리면 coverage는 보지 않는다.
	if f := flag.Lookup("test.run"); service != "" && code == 0 && (f == nil || f.Value.String() == "") {
		var uncovered []string
		for _, o := range operations {
			if o.Service == service && !covered[o.Pattern()] {
				uncovered = append(uncovered, o.Pattern())
			}
		}
		sort.Strings(uncovered)
		if len(uncovered) > 0 {
			_, _ = fmt.Fprintf(os.Stderr, "\nOpenAPI operations of %s without a validated success response in this package's tests: %v\n", service, uncovered)
			code = 1
		}
	}
	return code
}
