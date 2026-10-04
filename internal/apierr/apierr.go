// Package apierr는 관리·조회 API의 공통 오류 envelope를 정의한다 (D02 §12, §19, ADR 0014).
//
//	{"error":{"code":"...","message":"...","request_id":"...","retryable":false,"details":{...}}}
//
// 오류 응답에는 SQL, stack, 내부 오류 문자열, 다른 tenant의 존재를 넣지 않는다.
// 알 수 없는 오류는 내부 메시지를 버리고 INTERNAL로 바꾼다.
// 이 패키지는 로그를 남기지 않는다. 변환·로그는 API 경계(internal/httpapi)에서 한 번만 한다.
package apierr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/polynomeer/montracer/internal/authz"
)

// Code는 기계가 읽는 오류 코드다. API 계약의 일부이며 추가 시 OpenAPI enum을 함께 갱신한다.
type Code string

// D02 §12 코드 + INTERNAL (ADR 0014).
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
	Internal         Code = "INTERNAL"
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

// Retry는 오류의 재시도 정책이다 (ADR 0014 §3).
type Retry uint8

const (
	// RetryAuto는 code와 요청의 재시도 안전성으로 결정한다.
	RetryAuto Retry = iota
	// RetryYes는 부작용 전 단계 실패처럼 항상 재시도해도 안전할 때 쓴다.
	RetryYes
	// RetryNo는 재시도하면 안 될 때 쓴다.
	RetryNo
)

// FieldViolation은 INVALID_ARGUMENT의 field 단위 오류다 (D02 §12 "field path 반환").
type FieldViolation struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// Error는 클라이언트에 노출해도 안전한 오류다. Message는 사용자에게 보여줄 문장이다.
type Error struct {
	Code    Code
	Status  int // 0이면 Code의 기본 status. 예: 의미상 미지원 INVALID_ARGUMENT는 422.
	Message string
	Retry   Retry
	Details map[string]any
	// RetryAfterSeconds가 0보다 크면 Retry-After header를 보낸다 (429, 503).
	RetryAfterSeconds int
	// cause는 경계 로그용 원인이다. 응답에는 절대 쓰지 않는다.
	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return string(e.Code) + ": " + e.Message + ": " + e.cause.Error()
	}
	return string(e.Code) + ": " + e.Message
}

// Unwrap은 원인 오류를 돌려준다 (errors.Is/As용).
func (e *Error) Unwrap() error { return e.cause }

// New는 code의 기본 status로 오류를 만든다.
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Wrap은 원인을 보존한 오류를 만든다. 원인은 로그에만 쓰인다.
func Wrap(cause error, code Code, message string) *Error {
	return &Error{Code: code, Message: message, cause: cause}
}

// NewInvalidArgument는 field path를 담은 400 오류를 만든다.
func NewInvalidArgument(message string, violations ...FieldViolation) *Error {
	e := New(InvalidArgument, message)
	if len(violations) > 0 {
		e.Details = map[string]any{"field_violations": violations}
	}
	return e
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

// Retryable은 요청의 재시도 안전성(safeRequest)을 고려해 retryable을 확정한다.
func (e *Error) Retryable(safeRequest bool) bool {
	switch e.Retry {
	case RetryYes:
		return true
	case RetryNo:
		return false
	}
	switch e.Code {
	case RateLimited:
		return true
	case Unavailable, QueryTimeout:
		return safeRequest
	default:
		return false
	}
}

// From은 임의의 오류를 노출 가능한 Error로 바꾼다. 원래 오류는 cause로 보존한다.
func From(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	switch {
	case errors.Is(err, authz.ErrBackendUnavailable):
		// 인증 확인은 부작용 전 단계이므로 항상 재시도해도 안전하다.
		ae := Wrap(err, Unavailable, "일시적으로 요청을 처리할 수 없습니다")
		ae.Retry = RetryYes
		return ae
	case errors.Is(err, authz.ErrUnauthenticated):
		return Wrap(err, Unauthenticated, "인증이 필요합니다")
	case errors.Is(err, authz.ErrStepUpRequired):
		ae := Wrap(err, Forbidden, "추가 인증(MFA)이 필요합니다")
		ae.Details = map[string]any{"step_up_required": true}
		return ae
	case errors.Is(err, authz.ErrForbidden):
		return Wrap(err, Forbidden, "이 작업을 수행할 권한이 없습니다")
	case errors.Is(err, authz.ErrNotFound):
		return Wrap(err, NotFound, "리소스를 찾을 수 없습니다")
	case errors.Is(err, context.DeadlineExceeded):
		return Wrap(err, Unavailable, "요청 처리 시간이 초과되었습니다")
	default:
		return Wrap(err, Internal, "요청을 처리하지 못했습니다")
	}
}

// WriteOptions는 응답 작성에 필요한 요청 정보다.
type WriteOptions struct {
	RequestID string
	// SafeToRetry는 GET·HEAD·OPTIONS 또는 Idempotency-Key가 있는 요청인지다.
	SafeToRetry bool
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

// Write는 오류를 JSON envelope로 쓴다. 로그가 필요한 호출자는 먼저 From으로 변환해 같은 값을 쓴다.
func Write(w http.ResponseWriter, err error, opt WriteOptions) {
	e := From(err)
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if e.RetryAfterSeconds > 0 {
		h.Set("Retry-After", strconv.Itoa(e.RetryAfterSeconds))
	}
	w.WriteHeader(e.HTTPStatus())
	_ = json.NewEncoder(w).Encode(body{Error: envelope{
		Code:      e.Code,
		Message:   e.Message,
		RequestID: opt.RequestID,
		Retryable: e.Retryable(opt.SafeToRetry),
		Details:   e.Details,
	}})
}
