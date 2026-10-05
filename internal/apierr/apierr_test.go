package apierr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polynomeer/montracer/internal/authz"
)

type wire struct {
	Error struct {
		Code      string         `json:"code"`
		Message   string         `json:"message"`
		RequestID string         `json:"request_id"`
		Retryable bool           `json:"retryable"`
		Details   map[string]any `json:"details"`
	} `json:"error"`
}

func write(t *testing.T, err error, safe bool) (*httptest.ResponseRecorder, wire) {
	t.Helper()
	rec := httptest.NewRecorder()
	Write(rec, err, WriteOptions{RequestID: "req_01", SafeToRetry: safe})
	var w wire
	if e := json.Unmarshal(rec.Body.Bytes(), &w); e != nil {
		t.Fatalf("invalid JSON %q: %v", rec.Body.String(), e)
	}
	return rec, w
}

func TestWriteEnvelope(t *testing.T) {
	e := NewInvalidArgument("조회 범위를 줄이세요", FieldViolation{Field: "range.to", Reason: "must be after range.from"})
	rec, w := write(t, e, false)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rec.Code)
	}
	for k, want := range map[string]string{
		"Content-Type":           "application/json; charset=utf-8",
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if w.Error.Code != "INVALID_ARGUMENT" || w.Error.RequestID != "req_01" || w.Error.Retryable {
		t.Errorf("envelope = %+v", w.Error)
	}
	fv, _ := w.Error.Details["field_violations"].([]any)
	if len(fv) != 1 || fv[0].(map[string]any)["field"] != "range.to" {
		t.Errorf("field_violations = %v", w.Error.Details)
	}
}

func TestNoDetailsWithoutViolations(t *testing.T) {
	rec, _ := write(t, NewInvalidArgument("x"), false)
	if strings.Contains(rec.Body.String(), "details") {
		t.Errorf("empty details must be omitted: %s", rec.Body.String())
	}
}

func TestStatusMapping(t *testing.T) {
	cases := map[Code]int{
		InvalidArgument:  400,
		Unauthenticated:  401,
		Forbidden:        403,
		NotFound:         404,
		Expired:          410,
		Conflict:         409,
		RevisionMismatch: 412,
		RateLimited:      429,
		Unavailable:      503,
		QueryTimeout:     504,
		Internal:         500,
	}
	for code, want := range cases {
		if got := New(code, "x").HTTPStatus(); got != want {
			t.Errorf("%s: status %d, want %d", code, got, want)
		}
	}
	unsupported := &Error{Code: InvalidArgument, Status: http.StatusUnprocessableEntity, Message: "x"}
	if unsupported.HTTPStatus() != 422 {
		t.Error("explicit 422 override ignored")
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
		safe bool
		want bool
	}{
		{"429 always", NewRateLimited("x", 3), false, true},
		{"503 on safe request", New(Unavailable, "x"), true, true},
		{"503 on unsafe mutation", New(Unavailable, "x"), false, false},
		{"504 on safe request", New(QueryTimeout, "x"), true, true},
		{"504 on unsafe mutation", New(QueryTimeout, "x"), false, false},
		{"400 never", New(InvalidArgument, "x"), true, false},
		{"500 never", New(Internal, "x"), true, false},
		{"explicit yes", &Error{Code: Unavailable, Retry: RetryYes}, false, true},
		{"explicit no", &Error{Code: RateLimited, Retry: RetryNo}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Retryable(tc.safe); got != tc.want {
				t.Fatalf("Retryable(%v) = %v, want %v", tc.safe, got, tc.want)
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	rec, w := write(t, NewRateLimited("잠시 후 다시 시도하세요", 7), false)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "7" || !w.Error.Retryable {
		t.Errorf("status=%d retry-after=%q retryable=%v", rec.Code, rec.Header().Get("Retry-After"), w.Error.Retryable)
	}
	// 0 이하가 들어와도 429에는 Retry-After가 붙는다.
	rec, _ = write(t, NewRateLimited("x", 0), false)
	if rec.Header().Get("Retry-After") != "1" {
		t.Errorf("retry-after=%q, want 1", rec.Header().Get("Retry-After"))
	}
}

func TestFromMapping(t *testing.T) {
	backendDown := fmt.Errorf("%w: key lookup: %w", authz.ErrBackendUnavailable, errors.New("dial tcp 10.0.0.5:5432"))
	cases := []struct {
		name      string
		err       error
		status    int
		code      string
		retryable bool // unsafe(POST) 요청 기준
	}{
		{"unauthenticated", authz.ErrUnauthenticated, 401, "UNAUTHENTICATED", false},
		{"forbidden wrapped", fmt.Errorf("wrapped: %w", authz.ErrForbidden), 403, "FORBIDDEN", false},
		{"step-up", authz.ErrStepUpRequired, 403, "FORBIDDEN", false},
		{"cross tenant", authz.ErrNotFound, 404, "NOT_FOUND", false},
		// 인증 확인은 부작용 전이므로 POST여도 재시도 안전.
		{"auth backend down", backendDown, 503, "UNAVAILABLE", true},
		{"deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), 503, "UNAVAILABLE", false},
		{"dependency unavailable marker", fmt.Errorf("store: %w", unavailable{}), 503, "UNAVAILABLE", false},
		{"apierr passthrough", fmt.Errorf("handler: %w", New(Conflict, "이미 존재합니다")), 409, "CONFLICT", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, w := write(t, tc.err, false)
			if rec.Code != tc.status || w.Error.Code != tc.code || w.Error.Retryable != tc.retryable {
				t.Fatalf("got %d %s retryable=%v", rec.Code, w.Error.Code, w.Error.Retryable)
			}
			if strings.Contains(rec.Body.String(), "10.0.0.5") {
				t.Fatalf("cause leaked: %s", rec.Body.String())
			}
		})
	}
	_, w := write(t, authz.ErrStepUpRequired, false)
	if w.Error.Details["step_up_required"] != true {
		t.Error("step-up detail missing")
	}
}

type unavailable struct{}

func (unavailable) Error() string     { return "dial tcp 10.0.0.5:5432: connection refused" }
func (unavailable) Unavailable() bool { return true }

// cause는 로그용으로 보존되지만 응답에는 나오지 않는다.
func TestCauseKeptForLogsNotResponse(t *testing.T) {
	internalDetail := "pq: relation \"tenant_22222222\" at 10.0.3.7:5432"
	cause := errors.New(internalDetail)
	rec := httptest.NewRecorder()
	e := From(cause)
	Write(rec, e, WriteOptions{RequestID: "req_01"})
	if rec.Code != 500 || e.Code != Internal {
		t.Errorf("status=%d code=%s", rec.Code, e.Code)
	}
	if !errors.Is(e, cause) || !strings.Contains(e.Error(), internalDetail) {
		t.Errorf("cause not preserved for logging: %v", e)
	}
	for _, frag := range []string{"pq:", "tenant_22222222", "10.0.3.7"} {
		if strings.Contains(rec.Body.String(), frag) {
			t.Errorf("response leaks %q: %s", frag, rec.Body.String())
		}
	}
}

type invalidArg struct{}

func (invalidArg) Error() string                     { return "bad trace id 4bf9" }
func (invalidArg) InvalidArgument() (string, string) { return "trace_id", "must be 32 lowercase hex" }

type budget struct{}

func (budget) Error() string                  { return "range over" }
func (budget) BudgetExceeded() map[string]any { return map[string]any{"max_range_seconds": 604800} }

type timeout struct{}

func (timeout) Error() string      { return "code 159" }
func (timeout) QueryTimeout() bool { return true }

func TestDomainErrorInterfaces(t *testing.T) {
	rec, w := write(t, fmt.Errorf("store: %w", invalidArg{}), true)
	fv, _ := w.Error.Details["field_violations"].([]any)
	if rec.Code != 400 || w.Error.Code != "INVALID_ARGUMENT" || len(fv) != 1 || fv[0].(map[string]any)["field"] != "trace_id" {
		t.Errorf("invalid argument: %d %+v", rec.Code, w.Error)
	}
	if strings.Contains(rec.Body.String(), "4bf9") {
		t.Errorf("error text leaked: %s", rec.Body.String())
	}
	rec, w = write(t, budget{}, true)
	if rec.Code != 422 || w.Error.Code != "QUERY_BUDGET_EXCEEDED" || w.Error.Retryable || w.Error.Details["max_range_seconds"] != float64(604800) {
		t.Errorf("budget: %d %+v", rec.Code, w.Error)
	}
	rec, w = write(t, timeout{}, true)
	if rec.Code != 504 || w.Error.Code != "QUERY_TIMEOUT" || !w.Error.Retryable {
		t.Errorf("timeout on safe request: %d %+v", rec.Code, w.Error)
	}
}
