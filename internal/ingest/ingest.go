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
	"github.com/polynomeer/montracer/internal/quota"
	"github.com/polynomeer/montracer/internal/telemetry/envelope"
	"github.com/polynomeer/montracer/internal/telemetry/otlp"
	"github.com/polynomeer/montracer/internal/telemetry/redact"
)

// Producer는 envelope를 durable하게 append한다. 반환이 nil이면 모든 record가 acks=all로 기록된 것이다.
type Producer interface {
	ProduceSync(ctx context.Context, records []envelope.Record) error
}

// RequestResult는 요청 하나의 결과다. 운영 지표용이며 값 내용·tenant·key를 담지 않는다 (D04 §10).
type RequestResult struct {
	Signal   string
	Status   int
	Accepted int
	// Rejected는 거절 사유별 record 수다. 사유는 고정 enum이라 label cardinality가 bounded다.
	Rejected map[string]int
	Duration time.Duration
	// ProduceAttempted는 Kafka append를 시도했는지다. ProduceDuration은 그 대기 시간이다.
	ProduceAttempted bool
	ProduceDuration  time.Duration
	ProduceFailed    bool
	// ProduceCanceled는 client가 append 대기 중 연결을 끊은 경우다. broker 장애가 아니라 따로 센다.
	ProduceCanceled bool
}

// Observer는 요청 결과를 운영 지표로 내보낸다. 구현은 internal/opsmetrics에 있다.
type Observer interface {
	ObserveRequest(RequestResult)
}

// SeriesAdmitter는 metric 활성 series 상한이다 (quota.SeriesLimiter, D02 §10).
type SeriesAdmitter interface {
	Admit(ctx context.Context, tenant authz.TenantID, refs []envelope.StreamRef, now time.Time) ([]bool, error)
}

var errSeriesUnavailable = errors.New("ingest: metric series registry unavailable")

// Quota는 tenant·signal별 rate limit이다 (quota.Limiter).
type Quota interface {
	Allow(tenant, signal string, records, bytes int, now time.Time) quota.Decision
}

// 거절 사유 (record 단위 회계, D02 §21 valid_before_quota = quota_reject + durable_accepted).
const (
	// ReasonRateLimited: tenant rate 초과로 429. client가 재시도한다.
	ReasonRateLimited = "rate_limited"
	// ReasonOverBurst: 요청 하나가 tenant burst보다 커서 413. batch를 나눠야 한다.
	ReasonOverBurst = "quota_over_burst"
)

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
	// Observer가 nil이면 지표를 내보내지 않는다.
	Observer Observer
	// Quota가 nil이면 tenant rate limit을 적용하지 않는다(시험·로컬).
	Quota Quota
	// Series가 nil이면 활성 series 상한을 적용하지 않는다(dimension 규칙은 항상 적용).
	Series SeriesAdmitter
	// MaxInflight는 instance 하나의 동시 처리 요청 상한이다(기본 256). 넘으면 503 — 계약 초과가 아닌 과부하다.
	MaxInflight int
	Now         func() time.Time
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
	cfg      Config
	mux      *http.ServeMux
	inflight chan struct{}
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
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = 256
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), inflight: make(chan struct{}, cfg.MaxInflight)}
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
	produceAttempted   bool
	produceDuration    time.Duration
	produceFailed      bool
	produceCanceled    bool
	// quota는 거절 판정이다. 과부하(503)와 계약 초과(429·413)를 구분하는 결정 로그 (D01 §08)
	quota *quota.Decision
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

		// 0. instance 과부하 보호: 인증·decode 전에 거절해 CPU·제어 DB를 지킨다.
		select {
		case h.inflight <- struct{}{}:
			defer func() { <-h.inflight }()
		default:
			o.status = h.writeStatus(w, enc, http.StatusServiceUnavailable, "ingress overloaded; retry", true)
			return
		}

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
		// quota는 redaction 뒤·envelope 앞이다 (D02 §04). 통과하지 못하면 envelope도 만들지 않고 아무것도 append하지 않는다.
		gate := func(records int) quota.Decision {
			if h.cfg.Quota == nil || records == 0 {
				return quota.Decision{Outcome: quota.Allowed}
			}
			return h.cfg.Quota.Allow(o.tenant, sig.String(), records, payload.DecodedBytes, h.cfg.Now())
		}
		res, d, quotaRecords, err := h.prepare(r.Context(), payload, p, meta, received, &o, gate)
		if errors.Is(err, errSeriesUnavailable) {
			// series 등록부(제어 DB) 장애: 판정할 수 없으므로 받지 않는다(fail closed, 재시도 가능)
			o.status = h.writeStatus(w, enc, http.StatusServiceUnavailable, "temporarily unable to check metric series; retry", true)
			h.cfg.Logger.WarnContext(r.Context(), "metric series registry unavailable", slog.String("error", err.Error()))
			return
		}
		if err != nil {
			o.status = h.writeStatus(w, enc, http.StatusInternalServerError, "internal error", false)
			h.cfg.Logger.ErrorContext(r.Context(), "ingest prepare", slog.String("error", err.Error()))
			return
		}
		switch d.Outcome {
		case quota.RateLimited:
			o.reject(ReasonRateLimited, quotaRecords)
			o.quota = &d
			w.Header().Set("Retry-After", strconv.Itoa(quota.RetryAfterSeconds(d.RetryAfter)))
			o.status = h.writeStatus(w, enc, http.StatusTooManyRequests, "tenant ingest rate limit exceeded; retry later", false)
			return
		case quota.OverBurst:
			o.reject(ReasonOverBurst, quotaRecords)
			o.quota = &d
			o.status = h.writeStatus(w, enc, http.StatusRequestEntityTooLarge, "batch exceeds tenant burst limit; split the batch", false)
			return
		}

		// 7. Kafka append (acks=all) — 모두 확인된 뒤에만 ACK
		if len(res.Records) > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), h.cfg.ProduceTimeout)
			produceStart := h.cfg.Now()
			err := h.cfg.Producer.ProduceSync(ctx, res.Records)
			o.produceAttempted, o.produceDuration = true, h.cfg.Now().Sub(produceStart)
			cancel()
			if err != nil {
				if r.Context().Err() != nil {
					o.produceCanceled = true // client 연결 끊김. append timeout(ProduceTimeout)은 장애로 센다
				} else {
					o.produceFailed = true
				}
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

// prepare는 검증 → environment 범위 → 예약 속성 제거 → redaction → quota(gate) → envelope 순서로 처리한다.
// quota를 통과하지 못하면 판정과 대상 record 수를 돌려주고 envelope를 만들지 않는다.
func (h *Handler) prepare(ctx context.Context, p otlp.Payload, principal authz.Principal, meta envelope.Meta, received time.Time, o *outcome,
	gate func(records int) quota.Decision) (envelope.Result, quota.Decision, int, error) {
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
		if n := p.Traces.SpanCount(); n > 0 {
			if d := gate(n); d.Outcome != quota.Allowed {
				return envelope.Result{}, d, n, nil
			}
		}
		res, err := envelope.Traces(p.Traces, meta)
		o.reject(envelope.ReasonEnvelopeTooLarge, res.TooLarge)
		return res, quota.Decision{}, 0, err
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
		if n := p.Logs.LogRecordCount(); n > 0 {
			if d := gate(n); d.Outcome != quota.Allowed {
				return envelope.Result{}, d, n, nil
			}
		}
		res, err := envelope.Logs(p.Logs, meta, nil)
		o.reject(envelope.ReasonEnvelopeTooLarge, res.TooLarge)
		return res, quota.Decision{}, 0, err
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
		// cardinality 1단계 (D02 §10): 금지 dimension·label 과다는 redaction **전에** key 이름으로 판정한다.
		// redaction이 user_id 같은 key를 지운 뒤에 보면, 서로 다른 사용자별 series가 하나로 합쳐진 채 통과한다
		// ("숨은 자동 attribute 삭제로 series를 합치지 않는다"). 거절한 point는 저장하지 않으므로 PII 계약과 충돌하지 않는다.
		refs := envelope.MetricStreams(p.Metrics, principal.Tenant())
		dimReasons := make([]string, len(refs))
		for i, r := range refs {
			dimReasons[i] = quota.CheckDimensions(r)
		}
		envelope.RemovePoints(p.Metrics, func(i int) bool {
			if dimReasons[i] != "" {
				o.reject(dimReasons[i], 1)
				return true
			}
			return false
		})
		rr := h.cfg.Redactor.Metrics(p.Metrics)
		o.reject(redact.ReasonRedactionFailed, rr.Failed)
		// 2단계: rate quota → 활성 series 상한(redaction 뒤 identity = 저장될 identity). 모두 envelope 전이다.
		if n := p.Metrics.DataPointCount(); n > 0 {
			if d := gate(n); d.Outcome != quota.Allowed {
				return envelope.Result{}, d, n, nil
			}
		}
		if h.cfg.Series != nil && p.Metrics.DataPointCount() > 0 {
			refs = envelope.MetricStreams(p.Metrics, principal.Tenant())
			rejected, err := h.cfg.Series.Admit(ctx, principal.Tenant(), refs, received)
			if err != nil {
				return envelope.Result{}, quota.Decision{}, 0, fmt.Errorf("%w: %w", errSeriesUnavailable, err)
			}
			o.reject(quota.ReasonSeriesLimit, envelope.RemovePoints(p.Metrics, func(i int) bool { return rejected[i] }))
		}
		res, err := envelope.Metrics(p.Metrics, meta)
		o.reject(envelope.ReasonEnvelopeTooLarge, res.TooLarge)
		return res, quota.Decision{}, 0, err
	default:
		return envelope.Result{}, quota.Decision{}, 0, fmt.Errorf("ingest: unknown signal %d", p.Signal)
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
	duration := h.cfg.Now().Sub(start)
	if h.cfg.Observer != nil {
		h.cfg.Observer.ObserveRequest(RequestResult{
			Signal: sig.String(), Status: o.status, Accepted: o.accepted, Rejected: o.reasons,
			Duration: duration, ProduceAttempted: o.produceAttempted, ProduceDuration: o.produceDuration,
			ProduceFailed: o.produceFailed, ProduceCanceled: o.produceCanceled,
		})
	}
	// payload·header·key를 기록하지 않는다 (D04 §10). tenant UUID는 운영 식별자라 남긴다.
	attrs := []any{
		slog.String("signal", sig.String()),
		slog.Int("status", o.status),
		slog.String("tenant_id", o.tenant),
		slog.Int("accepted", o.accepted),
		slog.Int("rejected", o.rejected),
		slog.Duration("duration", duration),
	}
	if q := o.quota; q != nil {
		// 적용 한도를 함께 남겨 replica 수 설정 오류 같은 내부 원인의 429를 사후에 구분한다.
		attrs = append(attrs, slog.String("quota_limit", q.Limit), slog.Float64("quota_rate_per_replica", q.RatePerReplica),
			slog.Int("quota_burst", q.Burst), slog.Int("quota_replicas", q.Replicas), slog.Bool("quota_overridden", q.Overridden))
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
