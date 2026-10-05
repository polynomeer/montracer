// Package httpapi는 관리·조회 API의 HTTP 경계다 (ADR 0014).
//
// handler는 오류를 반환만 하고, 이 패키지가 오류를 한 번 변환해 응답하고 한 번 로그를 남긴다.
// panic 복구, client 연결 끊김, request ID 발급도 여기서 처리한다.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/polynomeer/montracer/internal/apierr"
)

// RequestIDHeader는 응답에 request ID를 싣는 header다.
const RequestIDHeader = "X-Request-ID"

type requestIDKey struct{}

// RequestIDFrom은 context의 request ID를 반환한다. 없으면 빈 문자열.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 실패는 복구 불가능한 환경 문제다. 상관 ID 없이도 응답은 계속한다.
		return "req_unavailable"
	}
	return "req_" + hex.EncodeToString(b[:])
}

// RequestID는 요청마다 서버가 request ID를 발급한다. client가 보낸 X-Request-ID는 신뢰하지 않는다.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// HandlerFunc는 오류를 반환하는 handler다. 응답을 쓴 뒤에는 nil을 반환해야 한다.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Boundary는 오류 변환·로그 정책을 가진 handler adapter다.
type Boundary struct {
	Logger *slog.Logger
}

// Handle은 HandlerFunc를 http.Handler로 감싼다. RequestID middleware 안쪽에 둔다.
func (b Boundary) Handle(h HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}
		defer b.recoverPanic(tw, r)

		err := h(tw, r)
		if err == nil {
			return
		}
		ctx := r.Context()
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			// client가 연결을 끊었다. 받을 사람이 없으므로 응답하지 않는다.
			b.logger().DebugContext(ctx, "request canceled by client", b.attrs(r, nil)...)
			return
		}
		if tw.wrote {
			b.logger().ErrorContext(ctx, "error after response started",
				append(b.attrs(r, nil), slog.String("error", err.Error()))...)
			return
		}
		e := apierr.From(err)
		apierr.Write(tw, e, b.options(r))
		b.logError(r, e)
	})
}

func (b Boundary) recoverPanic(tw *trackingWriter, r *http.Request) {
	p := recover()
	if p == nil {
		return
	}
	if p == http.ErrAbortHandler { //nolint:errorlint // sentinel 값 비교가 net/http 규약이다
		panic(p)
	}
	b.logger().ErrorContext(r.Context(), "panic recovered",
		append(b.attrs(r, nil), slog.Any("panic", p), slog.String("stack", string(debug.Stack())))...)
	if !tw.wrote {
		apierr.Write(tw, apierr.New(apierr.Internal, "요청을 처리하지 못했습니다"), b.options(r))
	}
}

func (b Boundary) options(r *http.Request) apierr.WriteOptions {
	return apierr.WriteOptions{RequestID: RequestIDFrom(r.Context()), SafeToRetry: safeToRetry(r)}
}

// safeToRetry: 부작용이 없거나 멱등성 키가 있는 요청 (ADR 0014 §3).
func safeToRetry(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return r.Header.Get("Idempotency-Key") != ""
}

// logError는 ADR 0014 §5의 수준 정책을 따른다. body·query·header는 기록하지 않는다.
func (b Boundary) logError(r *http.Request, e *apierr.Error) {
	ctx := r.Context()
	status := e.HTTPStatus()
	attrs := b.attrs(r, e)
	switch {
	case status >= 500:
		b.logger().ErrorContext(ctx, "request failed", append(attrs, slog.String("error", e.Error()))...)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		b.logger().InfoContext(ctx, "request denied", attrs...)
	default:
		b.logger().DebugContext(ctx, "request rejected", attrs...)
	}
}

func (b Boundary) attrs(r *http.Request, e *apierr.Error) []any {
	attrs := []any{
		slog.String("request_id", RequestIDFrom(r.Context())),
		slog.String("method", r.Method),
		// 원 URL이 아니라 route pattern만 남긴다 (path에 ID·검색어가 들어갈 수 있다).
		slog.String("route", r.Pattern),
	}
	if e != nil {
		attrs = append(attrs, slog.String("code", string(e.Code)), slog.Int("status", e.HTTPStatus()))
	}
	return attrs
}

func (b Boundary) logger() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}

// trackingWriter는 응답 시작 여부를 기록한다.
type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (t *trackingWriter) WriteHeader(code int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackingWriter) Write(p []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(p)
}

// Unwrap은 http.ResponseController가 원래 writer에 접근하게 한다.
func (t *trackingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// Observe는 처리를 마친 요청 하나의 route pattern·status·소요 시간을 받는다 (운영 지표, D04 §10).
// route는 등록된 pattern(예: "GET /api/v1/traces/{trace_id}")이며 원 URL·ID·query string은 넘기지 않는다.
type Observe func(route string, status int, d time.Duration)

// Instrument는 ServeMux 바로 바깥에 둔다. mux가 같은 *http.Request에 Pattern을 채우므로 처리 후에 읽는다.
// 등록되지 않은 경로는 route "unmatched"로 모아 label cardinality를 묶는다.
func Instrument(next http.Handler, observe Observe) http.Handler {
	if observe == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		observe(route, sw.status, time.Since(start))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(p)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
