package apierr

import (
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

func write(t *testing.T, err error) (*httptest.ResponseRecorder, wire) {
	t.Helper()
	rec := httptest.NewRecorder()
	Write(rec, "req_01", err)
	var w wire
	if e := json.Unmarshal(rec.Body.Bytes(), &w); e != nil {
		t.Fatalf("invalid JSON %q: %v", rec.Body.String(), e)
	}
	return rec, w
}

func TestWriteEnvelope(t *testing.T) {
	e := New(InvalidArgument, "조회 범위를 줄이세요")
	e.Details = map[string]any{"field": "range.to"}
	rec, w := write(t, e)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	if w.Error.Code != "INVALID_ARGUMENT" || w.Error.RequestID != "req_01" || w.Error.Retryable || w.Error.Details["field"] != "range.to" {
		t.Errorf("envelope = %+v", w.Error)
	}
}

func TestStatusMapping(t *testing.T) {
	cases := map[Code]int{
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

func TestRetryAfter(t *testing.T) {
	rec, w := write(t, NewRateLimited("잠시 후 다시 시도하세요", 7))
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "7" || !w.Error.Retryable {
		t.Errorf("status=%d retry-after=%q retryable=%v", rec.Code, rec.Header().Get("Retry-After"), w.Error.Retryable)
	}
	// 0 이하가 들어와도 429에는 Retry-After가 붙는다.
	rec, _ = write(t, NewRateLimited("x", 0))
	if rec.Header().Get("Retry-After") != "1" {
		t.Errorf("retry-after=%q, want 1", rec.Header().Get("Retry-After"))
	}
}

func TestUnavailableRetryableIsExplicit(t *testing.T) {
	// 503은 요청 멱등성에 따라 재시도 안전성이 다르므로 기본 false.
	if New(Unavailable, "x").Retryable || New(QueryTimeout, "x").Retryable {
		t.Error("503/504 must not default to retryable")
	}
}

func TestAuthzErrorsMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{authz.ErrUnauthenticated, 401, "UNAUTHENTICATED"},
		{fmt.Errorf("wrapped: %w", authz.ErrForbidden), 403, "FORBIDDEN"},
		{authz.ErrStepUpRequired, 403, "FORBIDDEN"},
		{authz.ErrNotFound, 404, "NOT_FOUND"},
		{fmt.Errorf("%w: key lookup: %w", authz.ErrBackendUnavailable, errors.New("dial tcp 10.0.0.5:5432")), 503, "UNAVAILABLE"},
	}
	for _, tc := range cases {
		rec, w := write(t, tc.err)
		if rec.Code != tc.status || w.Error.Code != tc.code {
			t.Errorf("%v → %d %s", tc.err, rec.Code, w.Error.Code)
		}
	}
	_, w := write(t, authz.ErrStepUpRequired)
	if w.Error.Details["step_up_required"] != true {
		t.Error("step-up detail missing")
	}
}

// 내부 오류 문자열(SQL, 호스트, tenant ID 등)은 응답에 절대 나오지 않아야 한다.
func TestUnknownErrorDoesNotLeak(t *testing.T) {
	internalDetail := "pq: relation \"tenant_22222222\" at 10.0.3.7:5432"
	rec, w := write(t, errors.New(internalDetail))
	if rec.Code != 500 || w.Error.Code != "INTERNAL" {
		t.Errorf("status=%d code=%s", rec.Code, w.Error.Code)
	}
	for _, frag := range []string{"pq:", "tenant_22222222", "10.0.3.7"} {
		if strings.Contains(rec.Body.String(), frag) {
			t.Errorf("response leaks %q: %s", frag, rec.Body.String())
		}
	}
}
