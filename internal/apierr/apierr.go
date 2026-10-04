// Package apierr는 관리·조회 API의 공통 오류 envelope를 정의한다 (D02 §12, §19).
//
//	{"error":{"code":"...","message":"...","request_id":"...","retryable":false,"details":{...}}}
//
// 오류 응답에는 SQL, stack, 내부 오류 문자열, 다른 tenant의 존재를 넣지 않는다.
// 알 수 없는 오류는 내부 메시지를 버리고 일반 메시지로 바꾼다.
package apierr

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/polynomeer/montracer/internal/authz"
)

// Code는 기계가 읽는 오류 코드다.
type Code string

// D02 §12 표에 정의된 코드.
const (
	InvalidArgument  Code = "INVALID_ARGUMENT"
	Unauthenticated  Code = "UNAUTHENTICATED"
	Forbidden        Code = "FORBIDDEN"
	NotFound         Code = "NOT_FOUND"
	Expired          Code = "EXPIRED"
	Conflict         Code = "CONFLICT"
	RevisionMismatch Code = "REVISION_MISMATCH"
	RateLimited      Code = "RATE_LIMITED"
	Unavailable      Code = "UNAVAILABLE"
	QueryTimeout     Code = "QUERY_TIMEOUT"
	// Internal은 명세 표에 없는 500 응답용 코드다. 명세 확정 전까지의 임시 정의다.
	Internal Code = "INTERNAL"
)

var defaultStatus = map[Code]int{
	InvalidArgument:  http.StatusBadRequest,
	Unauthenticated:  http.StatusUnauthorized,
	Forbidden:        http.StatusForbidden,
	NotFound:         http.StatusNotFound,
	Expired:          http.StatusGone,
	Conflict:         http.StatusConflict,
	RevisionMismatch: http.StatusPreconditionFailed,
	RateLimited:      http.StatusTooManyRequests,
	Unavailable:      http.StatusServiceUnavailable,
	QueryTimeout:     http.StatusGatewayTimeout,
	Internal:         http.StatusInternalServerError,
}

// 503/504의 재시도 안전성은 요청의 멱등성에 달려 있으므로 기본값을 두지 않는다 (D02 §12).
// 호출자가 GET이나 Idempotency-Key 요청일 때 Retryable을 명시한다.
var defaultRetryable = map[Code]bool{
	RateLimited: true,
}

// Error는 클라이언트에 노출해도 안전한 오류다. Message는 사용자에게 보여줄 문장이다.
type Error struct {
	Code      Code
	Status    int // 0이면 Code의 기본 status. 예: INVALID_ARGUMENT를 422로 보낼 때 지정.
	Message   string
	Retryable bool
	Details   map[string]any
	// RetryAfterSeconds가 0보다 크면 Retry-After header를 보낸다 (429, 503).
	RetryAfterSeconds int
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// New는 code의 기본 status·retryable로 오류를 만든다.
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message, Retryable: defaultRetryable[code]}
}

// NewRateLimited는 429 오류를 만든다. 429는 Retry-After가 필수다 (D02 §12).
func NewRateLimited(message string, retryAfterSeconds int) *Error {
	if retryAfterSeconds < 1 {
		retryAfterSeconds = 1
	}
	e := New(RateLimited, message)
	e.RetryAfterSeconds = retryAfterSeconds
	return e
}

// HTTPStatus는 응답 status를 반환한다.
func (e *Error) HTTPStatus() int {
	if e.Status != 0 {
		return e.Status
	}
	if s, ok := defaultStatus[e.Code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// From은 임의의 오류를 노출 가능한 Error로 바꾼다.
// authz 오류는 대응 코드로, 그 외 알 수 없는 오류는 내부 내용을 버린 Internal로 바꾼다.
func From(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	switch {
	case errors.Is(err, authz.ErrBackendUnavailable):
		// 인증 확인 자체는 읽기 동작이라 재시도해도 안전하다.
		ae := New(Unavailable, "일시적으로 요청을 처리할 수 없습니다")
		ae.Retryable = true
		return ae
	case errors.Is(err, authz.ErrUnauthenticated):
		return New(Unauthenticated, "인증이 필요합니다")
	case errors.Is(err, authz.ErrStepUpRequired):
		ae := New(Forbidden, "추가 인증(MFA)이 필요합니다")
		ae.Details = map[string]any{"step_up_required": true}
		return ae
	case errors.Is(err, authz.ErrForbidden):
		return New(Forbidden, "이 작업을 수행할 권한이 없습니다")
	case errors.Is(err, authz.ErrNotFound):
		return New(NotFound, "리소스를 찾을 수 없습니다")
	default:
		return New(Internal, "요청을 처리하지 못했습니다")
	}
}

type body struct {
	Error envelope `json:"error"`
}

type envelope struct {
	Code      Code           `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
}

// Write는 오류를 JSON envelope로 쓴다. requestID는 middleware가 발급한 값이다.
func Write(w http.ResponseWriter, requestID string, err error) {
	e := From(err)
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	if e.RetryAfterSeconds > 0 {
		h.Set("Retry-After", strconv.Itoa(e.RetryAfterSeconds))
	}
	w.WriteHeader(e.HTTPStatus())
	_ = json.NewEncoder(w).Encode(body{Error: envelope{
		Code:      e.Code,
		Message:   e.Message,
		RequestID: requestID,
		Retryable: e.Retryable,
		Details:   e.Details,
	}})
}
