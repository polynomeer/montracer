// Package ingest는 OTLP/HTTP 수신 경계다 (D02 §04, §22, ADR 0002, 0020).
//
// 처리 순서 (D02 §04):
//
//	인증 → 압축 해제 한도·decode → 속성 검증 → tenant 주입·environment 범위 → PII 제거 → envelope → Kafka append → ACK
//
// ACK(200)는 요청의 모든 유효 record가 Kafka에 durable append된 뒤에만 반환한다. 일부 append가 실패하면
// 이미 기록된 record가 있어도 503(재시도 가능)으로 응답하고, 재전송 중복은 envelope event_id로 worker가 제거한다.
// 응답 형식은 OTLP 규격을 따른다(apierr envelope가 아니다, ADR 0014 §6).
// quota·weighted fair queue(D02 §05)는 아직 없다 (ADR 0020 §7).
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetry/envelope"
	"github.com/polynomeer/montracer/internal/telemetry/otlp"
	"github.com/polynomeer/montracer/internal/telemetry/redact"
)

// Producer는 envelope를 durable하게 append한다. 반환이 nil이면 모든 record가 acks=all로 기록된 것이다.
type Producer interface {
	ProduceSync(ctx context.Context, records []envelope.Record) error
}

// Authenticator는 ingest key token을 principal로 바꾼다 (authz.KeyHasher.Authenticate).
type Authenticator func(ctx context.Context, token string) (authz.Principal, error)

// Config는 Handler 설정이다.
type Config struct {
	Authenticate Authenticator
	Producer     Producer
	Redactor     *redact.Redactor
	Limits       otlp.Limits
	Rules        otlp.Rules
	RoutingEpoch int64
	// ProduceTimeout은 Kafka append 대기 상한이다. 넘으면 503(재시도 가능).
	ProduceTimeout time.Duration
	// RetryAfter는 429·503 응답의 Retry-After 초다.
	RetryAfter int
	Logger     *slog.Logger
	Now        func() time.Time
}

// reservedAttr는 payload가 tenant·플랫폼 메타데이터를 흉내 내는 속성이다. 의미를 갖지 않도록 Kafka에 쓰기 전에 지운다.
// tenant의 유일한 원천은 envelope header(인증 principal)다 (변경 불가 계약 1, ADR 0020 §3).
func reservedAttr(k string) bool {
	l := strings.ToLower(k)
	return l == "tenant_id" || l == "tenant.id" || l == "tenantid" || strings.HasPrefix(l, "mt.") || strings.HasPrefix(l, "montracer.")
}

func stripReserved(m pcommon.Map) {
	m.RemoveIf(func(k string, _ pcommon.Value) bool { return reservedAttr(k) })
}

// EnvironmentAttr는 environment 범위 검사에 쓰는 resource 속성이다 (D03 §02 필수 resource).
const EnvironmentAttr = "deployment.environment.name"

// ReasonEnvironmentNotAllowed는 key의 environment 범위 밖(또는 없음) resource의 record를 거절한 사유다 (D04 §02).
const ReasonEnvironmentNotAllowed = "environment_not_allowed"

// Handler는 /v1/traces, /v1/metrics, /v1/logs를 처리한다.
type Handler struct {
	cfg Config
	mux *http.ServeMux
}

// NewHandler는 Handler를 만든다.
func NewHandler(cfg Config) (*Handler, error) {
	if cfg.Authenticate == nil || cfg.Producer == nil || cfg.Redactor == nil {
		return nil, errors.New("ingest: authenticator, producer, redactor are required")
	}
	if cfg.Limits.MaxDecodedBytes == 0 {
		cfg.Limits = otlp.DefaultLimits
	}
	if cfg.Rules.MaxAttributes == 0 {
		cfg.Rules = otlp.DefaultRules
	}
	if cfg.ProduceTimeout == 0 {
		cfg.ProduceTimeout = 10 * time.Second
	}
	if cfg.RetryAfter == 0 {
		cfg.RetryAfter = 5
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/traces", h.serve(otlp.SignalTraces, authz.IngestTraces))
	h.mux.HandleFunc("POST /v1/metrics", h.serve(otlp.SignalMetrics, authz.IngestMetrics))
	h.mux.HandleFunc("POST /v1/logs", h.serve(otlp.SignalLogs, authz.IngestLogs))
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// bearer는 Authorization: Bearer <token>에서 token을 꺼낸다.
func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

// outcome은 로그·metric용 요청 결과다(값 내용 없음).
type outcome struct {
	status             int
	tenant             string
	accepted, rejected int
	reasons            map[string]int
}

func (o *outcome) reject(reason string, n int) {
	if n <= 0 {
		return
	}
	if o.reasons == nil {
		o.reasons = map[string]int{}
	}
	o.reasons[reason] += n
	o.rejected += n
}

func (h *Handler) serve(sig otlp.Signal, action authz.Action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := h.cfg.Now()
		received := start.UTC()
		enc, encErr := otlp.ParseEncoding(r.Header.Get("Content-Type"))
		var o outcome
		defer func() { h.log(r, sig, &o, start) }()

		// 1. 인증: ingest key만 허용, 원인은 구분하지 않는다 (D04 §02)
		p, err := h.cfg.Authenticate(r.Context(), bearer(r))
		if err != nil {
			if errors.Is(err, authz.ErrBackendUnavailable) {
				o.status = h.writeStatus(w, enc, http.StatusServiceUnavailable, "authentication temporarily unavailable", true)
				return
			}
			o.status = h.writeStatus(w, enc, http.StatusUnauthorized, "unauthenticated", false)
			return
		}
		if err := authz.Authorize(p, action); err != nil {
			o.status = h.writeStatus(w, enc, http.StatusForbidden, "key is not allowed for this signal", false)
			return
		}
		o.tenant = p.Tenant().String()

		// 2. decode
		if encErr != nil {
			o.status = h.writeStatus(w, otlp.EncodingProtobuf, http.StatusUnsupportedMediaType, "unsupported content type", false)
			return
		}
		if r.ContentLength > h.cfg.Limits.MaxWireBytes {
			o.status = h.writeStatus(w, enc, http.StatusRequestEntityTooLarge, "payload too large or too complex; split the batch", false)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, h.cfg.Limits.MaxWireBytes)
		payload, err := otlp.Decode(r.Body, r.Header.Get("Content-Type"), r.Header.Get("Content-Encoding"), sig, h.cfg.Limits)
		switch {
		case errors.Is(err, otlp.ErrBodyTooLarge), errors.Is(err, otlp.ErrTooComplex):
			o.status = h.writeStatus(w, enc, http.StatusRequestEntityTooLarge, "payload too large or too complex; split the batch", false)
			return
		case errors.Is(err, otlp.ErrUnsupportedMediaType):
			o.status = h.writeStatus(w, enc, http.StatusUnsupportedMediaType, "unsupported content encoding", false)
			return
		case errors.Is(err, otlp.ErrBodyRead):
			// 연결 끊김·읽기 timeout: 데이터 문제가 아니므로 재시도 가능 (OTLP retryable 503)
			o.status = h.writeStatus(w, enc, http.StatusServiceUnavailable, "request body could not be read; retry", true)
			return
		case errors.Is(err, otlp.ErrMalformed):
			o.status = h.writeStatus(w, enc, http.StatusBadRequest, "malformed payload", false)
			return
		case err != nil:
			o.status = h.writeStatus(w, enc, http.StatusInternalServerError, "internal error", false)
			h.cfg.Logger.ErrorContext(r.Context(), "ingest decode", slog.String("error", err.Error()))
			return
		}

		// 3~6. 검증 → environment 범위 → redaction → envelope
		meta := envelope.Meta{Tenant: p.Tenant(), ReceivedAt: received, PolicyVersion: h.cfg.Redactor.PolicyVersion(), RoutingEpoch: h.cfg.RoutingEpoch}
		res, err := h.prepare(payload, p, meta, received, &o)
		if err != nil {
			o.status = h.writeStatus(w, enc, http.StatusInternalServerError, "internal error", false)
			h.cfg.Logger.ErrorContext(r.Context(), "ingest prepare", slog.String("error", err.Error()))
			return
		}

		// 7. Kafka append (acks=all) — 모두 확인된 뒤에만 ACK
		if len(res.Records) > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), h.cfg.ProduceTimeout)
			err := h.cfg.Producer.ProduceSync(ctx, res.Records)
			cancel()
			if err != nil {
				o.status = h.writeStatus(w, enc, http.StatusServiceUnavailable, "temporarily unable to persist; retry", true)
				h.cfg.Logger.WarnContext(r.Context(), "ingest produce failed", slog.String("error", err.Error()))
				return
			}
		}
		o.accepted = len(res.Records)
		o.status = http.StatusOK
		h.writeSuccess(w, sig, enc, o)
	}
}

func (h *Handler) prepare(p otlp.Payload, principal authz.Principal, meta envelope.Meta, received time.Time, o *outcome) (envelope.Result, error) {
	rules := h.cfg.Rules
	allowEnv := func(attrs pcommon.Map) bool {
		v, ok := attrs.Get(EnvironmentAttr)
		return ok && v.Type() == pcommon.ValueTypeStr && principal.AllowsEnvironment(v.Str())
	}
	switch p.Signal {
	case otlp.SignalTraces:
		vr := otlp.ValidateTraces(p.Traces, received, rules)
		addReasons(o, vr)
		p.Traces.ResourceSpans().RemoveIf(func(rs ptrace.ResourceSpans) bool {
			if allowEnv(rs.Resource().Attributes()) {
				return false
			}
			o.reject(ReasonEnvironmentNotAllowed, countSpans(rs))
			return true
		})
		for i := 0; i < p.Traces.ResourceSpans().Len(); i++ {
			stripReserved(p.Traces.ResourceSpans().At(i).Resource().Attributes())
		}
		rr := h.cfg.Redactor.Traces(p.Traces)
		o.reject(redact.ReasonRedactionFailed, rr.Failed)
		res, err := envelope.Traces(p.Traces, meta)
		o.reject(envelope.ReasonEnvelopeTooLarge, res.TooLarge)
		return res, err
	case otlp.SignalLogs:
		vr := otlp.ValidateLogs(p.Logs, received, rules)
		addReasons(o, vr)
		p.Logs.ResourceLogs().RemoveIf(func(rl plog.ResourceLogs) bool {
			if allowEnv(rl.Resource().Attributes()) {
				return false
			}
			o.reject(ReasonEnvironmentNotAllowed, countLogs(rl))
			return true
		})
		for i := 0; i < p.Logs.ResourceLogs().Len(); i++ {
			stripReserved(p.Logs.ResourceLogs().At(i).Resource().Attributes())
		}
		rr := h.cfg.Redactor.Logs(p.Logs)
		o.reject(redact.ReasonRedactionFailed, rr.Failed)
		res, err := envelope.Logs(p.Logs, meta, nil)
		o.reject(envelope.ReasonEnvelopeTooLarge, res.TooLarge)
		return res, err
	case otlp.SignalMetrics:
		vr := otlp.ValidateMetrics(p.Metrics, received, rules)
		addReasons(o, vr)
		p.Metrics.ResourceMetrics().RemoveIf(func(rm pmetric.ResourceMetrics) bool {
			if allowEnv(rm.Resource().Attributes()) {
				return false
			}
			o.reject(ReasonEnvironmentNotAllowed, countPoints(rm))
			return true
		})
		for i := 0; i < p.Metrics.ResourceMetrics().Len(); i++ {
			stripReserved(p.Metrics.ResourceMetrics().At(i).Resource().Attributes())
		}
		rr := h.cfg.Redactor.Metrics(p.Metrics)
		o.reject(redact.ReasonRedactionFailed, rr.Failed)
		res, err := envelope.Metrics(p.Metrics, meta)
		o.reject(envelope.ReasonEnvelopeTooLarge, res.TooLarge)
		return res, err
	default:
		return envelope.Result{}, fmt.Errorf("ingest: unknown signal %d", p.Signal)
	}
}

func addReasons(o *outcome, r otlp.Result) {
	for reason, n := range r.Reasons {
		o.reject(string(reason), n)
	}
}

func countSpans(rs ptrace.ResourceSpans) int {
	n := 0
	for i := 0; i < rs.ScopeSpans().Len(); i++ {
		n += rs.ScopeSpans().At(i).Spans().Len()
	}
	return n
}

func countLogs(rl plog.ResourceLogs) int {
	n := 0
	for i := 0; i < rl.ScopeLogs().Len(); i++ {
		n += rl.ScopeLogs().At(i).LogRecords().Len()
	}
	return n
}

func countPoints(rm pmetric.ResourceMetrics) int {
	md := pmetric.NewMetrics()
	rm.CopyTo(md.ResourceMetrics().AppendEmpty())
	return md.DataPointCount()
}

func (h *Handler) log(r *http.Request, sig otlp.Signal, o *outcome, start time.Time) {
	// payload·header·key를 기록하지 않는다 (D04 §10). tenant UUID는 운영 식별자라 남긴다.
	attrs := []any{
		slog.String("signal", sig.String()),
		slog.Int("status", o.status),
		slog.String("tenant_id", o.tenant),
		slog.Int("accepted", o.accepted),
		slog.Int("rejected", o.rejected),
		slog.Duration("duration", h.cfg.Now().Sub(start)),
	}
	for reason, n := range o.reasons {
		attrs = append(attrs, slog.Int("rejected."+reason, n))
	}
	level := slog.LevelInfo
	if o.status >= 500 {
		level = slog.LevelWarn
	}
	h.cfg.Logger.Log(r.Context(), level, "otlp request", attrs...)
}

// writeSuccess는 OTLP Export*ServiceResponse를 쓴다. 거절이 있으면 partial_success를 채운다.
func (h *Handler) writeSuccess(w http.ResponseWriter, sig otlp.Signal, enc otlp.Encoding, o outcome) {
	msg := rejectMessage(o.reasons)
	body, err := encodeResponse(sig, enc, int64(o.rejected), msg)
	if err != nil {
		h.writeStatus(w, enc, http.StatusInternalServerError, "internal error", false)
		return
	}
	w.Header().Set("Content-Type", contentType(enc))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// writeStatus는 OTLP/HTTP 오류 응답(google.rpc.Status)을 쓴다. 메시지는 고정 문구다(입력 내용 없음).
func (h *Handler) writeStatus(w http.ResponseWriter, enc otlp.Encoding, status int, message string, retry bool) int {
	if enc == 0 {
		enc = otlp.EncodingProtobuf
	}
	if retry {
		w.Header().Set("Retry-After", strconv.Itoa(h.cfg.RetryAfter))
	}
	w.Header().Set("Content-Type", contentType(enc))
	w.WriteHeader(status)
	_, _ = w.Write(encodeStatus(enc, grpcCode(status), message))
	return status
}

func contentType(enc otlp.Encoding) string {
	if enc == otlp.EncodingJSON {
		return "application/json"
	}
	return "application/x-protobuf"
}
