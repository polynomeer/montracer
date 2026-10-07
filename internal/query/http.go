package query

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/httpapi"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// SchemaVersion은 조회 응답 schema 버전이다 (D02 §19 meta.schema_version).
const SchemaVersion = 1

// Store는 조회 저장소다 (telemetrystore.QueryStore).
type Store interface {
	TraceSpanRecords(ctx context.Context, p authz.Principal, q telemetrystore.TraceQuery, now time.Time) ([]telemetrystore.SpanRecord, error)
}

// Config는 Handler 설정이다.
type Config struct {
	// Authenticate는 bearer token을 principal로 바꾼다. query API는 API key만 받는다(ingest key 거절, D02 §12).
	Authenticate func(ctx context.Context, token string) (authz.Principal, error)
	Store        Store
	// Metrics가 nil이면 metric 조회 경로는 404다.
	Metrics MetricStore
	Logger  *slog.Logger
	// QueryTimeout은 요청 하나의 저장소 조회 상한이다 (기본 10초, query 계정 max_execution_time 5초보다 길게).
	QueryTimeout time.Duration
	// Logs가 nil이거나 Cursor가 nil이면 검색 경로(/query, /query/logs)는 404다.
	Logs   LogStore
	Cursor *apicursor.Signer
	// Services가 nil이면 GET /api/v1/services는 404다 (서비스 catalog, ADR 0038).
	Services ServiceStore
	// MaxConcurrent·MaxWaiting은 tenant별 조회 동시 실행·대기 상한이다(기본 5·20, D02 §15).
	MaxConcurrent, MaxWaiting int
	// Observe가 있으면 route별 요청 결과를 운영 지표로 내보낸다.
	Observe httpapi.Observe
	// ObserveCatalog는 log 검색의 서비스 catalog 조회(제어 DB) 결과·시간이다(ADR 0039). nil이면 세지 않는다.
	ObserveCatalog func(outcome string, d time.Duration)
	Now            func() time.Time
}

// Handler는 조회 API다.
type Handler struct {
	cfg  Config
	mux  *http.ServeMux
	gate *tenantGate
}

// NewHandler는 route를 등록한다.
func NewHandler(cfg Config) (*Handler, error) {
	if cfg.Authenticate == nil || cfg.Store == nil {
		return nil, errors.New("query: Authenticate and Store are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = 10 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 5
	}
	if cfg.MaxWaiting <= 0 {
		cfg.MaxWaiting = 20
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), gate: newTenantGate(cfg.MaxConcurrent, cfg.MaxWaiting)}
	b := httpapi.Boundary{Logger: cfg.Logger}
	h.mux.Handle("GET /api/v1/traces/{trace_id}", b.Handle(h.authenticated(h.gated(h.getTrace))))
	// 본문이 있는 경로는 검증을 마친 뒤 handler 안에서 slot을 잡는다(느린 본문이 slot을 붙잡지 않게).
	h.mux.Handle("POST /api/v1/query/metrics", b.Handle(h.authenticated(h.queryMetrics)))
	h.mux.Handle("POST /api/v1/query", b.Handle(h.authenticated(h.search(""))))
	h.mux.Handle("POST /api/v1/query/logs", b.Handle(h.authenticated(h.search("logs"))))
	h.mux.Handle("GET /api/v1/services", b.Handle(h.authenticated(h.gated(h.listServices))))
	h.mux.Handle("GET /api/v1/services/{service_id}", b.Handle(h.authenticated(h.gated(h.getService))))
	return h, nil
}

// ServeHTTP는 request ID를 붙여 route로 넘긴다.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	httpapi.RequestID(httpapi.Instrument(h.mux, h.cfg.Observe)).ServeHTTP(w, r)
}

type principalHandler func(w http.ResponseWriter, r *http.Request, p authz.Principal) error

// authenticated는 bearer token을 검증한다. 인증 저장소 장애는 503으로 fail closed한다 (D02 §02).
func (h *Handler) authenticated(next principalHandler) httpapi.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		token, ok := httpapi.Bearer(r)
		if !ok {
			return authz.ErrUnauthenticated
		}
		p, err := h.cfg.Authenticate(r.Context(), token)
		if err != nil {
			return err
		}
		return next(w, r.WithContext(authz.WithPrincipal(r.Context(), p)), p)
	}
}

// gated는 tenant별 동시 실행 상한 안에서만 조회를 실행한다 (D02 §15).
func (h *Handler) gated(next principalHandler) principalHandler {
	return func(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
		// 대기도 조회 시간 상한 안에서만 한다
		ctx, cancel := context.WithTimeout(r.Context(), h.cfg.QueryTimeout)
		defer cancel()
		release, err := h.gate.acquire(ctx, p.Tenant().String())
		if err != nil {
			return err
		}
		defer release()
		return next(w, r, p)
	}
}

// Meta는 조회 응답 공통 meta다 (D02 §13, §19).
// 아직 계산하지 않는 값은 null이다. 0·true·100%로 채우지 않는다.
type Meta struct {
	RequestID         string   `json:"request_id"`
	SchemaVersion     int      `json:"schema_version"`
	Partial           bool     `json:"partial"`
	FailedShards      []string `json:"failed_shards"`
	Watermark         *string  `json:"watermark"`          // 수집 watermark 미구현 → null
	Sampled           *bool    `json:"sampled"`            // sampling 정책 추적 미구현 → null
	Coverage          *float64 `json:"coverage"`           // 분모를 모르면 null (D02 §19)
	ResolutionSeconds *int     `json:"resolution_seconds"` // trace는 해당 없음
	ScanBytes         *int64   `json:"scan_bytes"`         // 실행 통계 미수집 → null
	Warnings          []string `json:"warnings"`
}

type traceResponse struct {
	Data Trace `json:"data"`
	Meta Meta  `json:"meta"`
}

func parseTime(r *http.Request, field string) (time.Time, error) {
	v := r.URL.Query().Get(field)
	if v == "" {
		return time.Time{}, apierr.NewInvalidArgument("요청 값이 올바르지 않습니다",
			apierr.FieldViolation{Field: field, Reason: "required (RFC3339 UTC)"})
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, apierr.NewInvalidArgument("요청 값이 올바르지 않습니다",
			apierr.FieldViolation{Field: field, Reason: "must be RFC3339"})
	}
	return t.UTC(), nil
}

// getTrace는 GET /api/v1/traces/{trace_id}?from&to[&service_id] 이다 (D02 §13).
// trace ID 단건 조회도 최대 7일의 명시적 시간 경계를 요구한다. 보이는 span이 없으면 404다 —
// 다른 tenant·권한 범위 밖 trace와 존재하지 않는 trace를 구별하지 않는다.
func (h *Handler) getTrace(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	from, err := parseTime(r, "from")
	if err != nil {
		return err
	}
	to, err := parseTime(r, "to")
	if err != nil {
		return err
	}
	traceID := r.PathValue("trace_id")
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.QueryTimeout)
	defer cancel()
	recs, err := h.cfg.Store.TraceSpanRecords(ctx, p, telemetrystore.TraceQuery{
		TraceID:   traceID,
		Range:     telemetrystore.TimeRange{From: from, To: to},
		ServiceID: r.URL.Query().Get("service_id"),
		Limit:     telemetrystore.MaxTraceSpans,
	}, h.cfg.Now())
	if err != nil {
		return err
	}
	trace, ok := buildTrace(p, traceID, recs, len(recs) >= telemetrystore.MaxTraceSpans)
	if !ok {
		return apierr.New(apierr.NotFound, "trace를 찾을 수 없습니다")
	}
	resp := traceResponse{Data: trace, Meta: Meta{
		RequestID:     httpapi.RequestIDFrom(r.Context()),
		SchemaVersion: SchemaVersion,
		FailedShards:  []string{},
		Warnings:      []string{},
	}}
	body, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store") // tenant 데이터
	_, _ = w.Write(body)
	return nil
}
