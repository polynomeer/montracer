package query

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/httpapi"
	"github.com/polynomeer/montracer/internal/queryplan"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// LogStore는 log 검색 저장소다 (telemetrystore.QueryStore).
type LogStore interface {
	SearchLogs(ctx context.Context, p authz.Principal, q telemetrystore.LogQuery, now time.Time) ([]telemetrystore.LogRecord, bool, error)
}

// 기본 검색 범위 (D02 §19: 생략하면 최근 15분).
const defaultSearchRange = 15 * time.Minute

// searchRequest는 POST /api/v1/query 본문이다 (D02 §19 QuerySpec).
type searchRequest struct {
	Signal       string          `json:"signal"`
	Range        *requestRange   `json:"range"`
	Filter       *queryplan.Node `json:"filter"`
	Projection   []string        `json:"projection"`
	Order        []searchOrder   `json:"order"`
	Limit        int             `json:"limit"`
	Cursor       string          `json:"cursor"`
	AllowPartial bool            `json:"allow_partial"`
	_            struct{}        `json:"-"`
}

type searchOrder struct {
	Field     string `json:"field"`
	Direction string `json:"direction"`
}

// logFields는 log 결과 field(projection)다.
var logFields = []string{"time", "event_id", "service_id", "severity_number", "trace_id", "span_id", "body", "attributes"}

type logPosition struct {
	TimeNs  int64  `json:"t"`
	EventID string `json:"id"`
	// Served는 이 cursor 전까지 돌려준 행 수다(interactive 누적 상한, D02 §13).
	Served int `json:"n"`
}

// MaxInteractiveRows는 cursor로 이어 읽을 수 있는 누적 행 상한이다 (D02 §13: 10,000행 초과는 export job).
const MaxInteractiveRows = 10000

// WarningInteractiveRowLimit은 누적 상한에 닿아 더 읽지 않았다는 meta.warnings 값이다.
const WarningInteractiveRowLimit = "interactive_row_limit_reached"

// search는 POST /api/v1/query(signal 필수)와 POST /api/v1/query/{signal}(경로가 signal을 정한다)이다.
func (h *Handler) search(fixedSignal string) principalHandler {
	return func(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
		if h.cfg.Logs == nil || h.cfg.Cursor == nil {
			return apierr.New(apierr.NotFound, "검색을 사용할 수 없습니다")
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			e := apierr.New(apierr.InvalidArgument, "요청 본문이 너무 큽니다")
			e.Status = http.StatusRequestEntityTooLarge
			return e
		}
		var req searchRequest
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			return invalid("body", "must be a JSON query (D02 §19)")
		}
		signal := req.Signal
		if fixedSignal != "" {
			if signal != "" && signal != fixedSignal {
				return invalid("signal", "must match the path ("+fixedSignal+") or be omitted")
			}
			signal = fixedSignal
		}
		switch signal {
		case "logs":
		case "metrics":
			return unsupported("signal", "metrics use POST /api/v1/query/metrics (expression, step_seconds)")
		case "traces", "errors":
			return unsupported("signal", signal+" search is not available yet; use GET /api/v1/traces/{trace_id}")
		case "":
			return invalid("signal", "required: logs")
		default:
			return invalid("signal", "one of logs, traces, metrics, errors")
		}
		return h.searchLogs(w, r, p, req)
	}
}

func (h *Handler) searchLogs(w http.ResponseWriter, r *http.Request, p authz.Principal, req searchRequest) error {
	now := h.cfg.Now().UTC()
	// range를 생략하면 "최근 15분"이다. 다음 page도 같은 15분이어야 하므로 hash에는 구체 시각 대신 "default"를 넣고,
	// cursor page에서는 첫 page의 snapshot 기준으로 다시 만든다(리뷰에서 발견: 요청마다 now로 다시 계산하면 cursor가 깨진다).
	rangeKey := "default:15m"
	rng := telemetrystore.TimeRange{From: now.Add(-defaultSearchRange), To: now}
	if req.Range != nil {
		rng = telemetrystore.TimeRange{From: req.Range.From.UTC(), To: req.Range.To.UTC()}
		rangeKey = rng.From.Format(time.RFC3339Nano) + "/" + rng.To.Format(time.RFC3339Nano)
	}
	for i, o := range req.Order {
		if i > 0 || o.Field != "time" || (o.Direction != "" && o.Direction != "desc") {
			return unsupported("order", `logs are ordered by [{"field":"time","direction":"desc"}] only`)
		}
	}
	proj, err := projection(req.Projection)
	if err != nil {
		return err
	}
	limit := req.Limit
	switch {
	case limit == 0:
		limit = 100
	case limit < 0 || limit > telemetrystore.MaxLogLimit:
		return invalid("limit", "must be within [1, 1000]")
	}
	compiled, err := queryplan.Compile(req.Filter, queryplan.LogCatalog)
	if err != nil {
		return planError(err)
	}

	// 권한 fingerprint: principal(key 종류·주체)·action·environment 범위가 바뀌면 cursor를 쓸 수 없다.
	fp := append([]string{p.Kind().String(), p.Subject(), string(authz.TelemetryRead)}, p.Environments()...)
	binding := apicursor.Binding{
		Tenant:      p.Tenant().String(),
		Fingerprint: apicursor.Fingerprint(fp...),
		// page 크기는 넣지 않는다(page마다 바꿀 수 있다)
		QueryHash: apicursor.Fingerprint("logs", rangeKey, compiled.Canonical, strings.Join(proj, ",")),
	}
	q := telemetrystore.LogQuery{Range: rng, Limit: limit, ReceivedBefore: now}
	served := 0
	if req.Cursor != "" {
		claims, err := h.cfg.Cursor.Decode(req.Cursor, binding)
		var pos logPosition
		if err != nil || json.Unmarshal(claims.Position, &pos) != nil || pos.EventID == "" || pos.Served < 0 {
			return invalid("cursor", "invalid, expired, or not for this query")
		}
		q.After = &telemetrystore.LogPosition{EventTime: time.Unix(0, pos.TimeNs).UTC(), EventID: pos.EventID}
		q.ReceivedBefore = claims.Snapshot
		served = pos.Served
		if req.Range == nil {
			q.Range = telemetrystore.TimeRange{From: claims.Snapshot.Add(-defaultSearchRange), To: claims.Snapshot}
		}
	}
	// interactive 누적 상한(D02 §13): 남은 만큼만 읽는다
	if remaining := MaxInteractiveRows - served; q.Limit > remaining {
		q.Limit = remaining
	}
	resp := searchResponse{Data: []map[string]any{}, Meta: newMeta(r)}
	if q.Limit <= 0 {
		resp.Meta.Warnings = append(resp.Meta.Warnings, WarningInteractiveRowLimit)
		return writeSearch(w, resp)
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.QueryTimeout)
	defer cancel()
	// 입력 검증을 마친 뒤에만 tenant 실행 slot을 잡는다(느린 본문·잘못된 요청이 slot을 붙잡지 않게).
	// catalog 조회(제어 DB)도 slot 안에서 한다 — tenant 동시 실행 상한이 제어 DB 부하도 묶는다(리뷰 반영).
	release, err := h.gate.acquire(ctx, p.Tenant().String())
	if err != nil {
		return err
	}
	defer release()
	// 서비스 catalog로 service.name과 environment 범위를 푼다(ADR 0039). cursor page마다 다시 푼다 —
	// 그 사이 등록된 서비스는 다음 page부터 맞을 수 있다(수신 snapshot이 범위를 묶는다).
	// 제어 DB 조회는 같은 QueryTimeout 예산을 쓴다(느리면 ClickHouse 시간이 준다).
	var unresolved bool
	if q.Filter, q.EnvironmentServices, unresolved, err = h.resolveServices(ctx, p, req.Filter, compiled); err != nil {
		return err
	}
	if unresolved {
		resp.Meta.Warnings = append(resp.Meta.Warnings, WarningServiceNameUnresolved)
	}
	records, more, err := h.cfg.Logs.SearchLogs(ctx, p, q, now)
	if err != nil {
		return err
	}
	for _, rec := range records {
		resp.Data = append(resp.Data, logRow(rec, proj))
	}
	served += len(records)
	if more && served >= MaxInteractiveRows {
		resp.Meta.Warnings = append(resp.Meta.Warnings, WarningInteractiveRowLimit)
		more = false
	}
	if more && len(records) > 0 {
		last := records[len(records)-1]
		tok, err := h.cfg.Cursor.Encode(binding, q.ReceivedBefore, logPosition{TimeNs: last.EventTime.UnixNano(), EventID: last.EventID, Served: served})
		if err != nil {
			return err
		}
		resp.NextCursor = &tok
	}
	return writeSearch(w, resp)
}

// WarningServiceNameUnresolved는 filter의 service.name 중 catalog에 없는(볼 수 있는 서비스가 없는) 이름이 있다는
// meta.warnings 값이다. 그 이름의 eq·in은 맞는 행이 없다 — "log 없음"과 구분하게 알린다(계약 6, ADR 0039 §1).
const WarningServiceNameUnresolved = "service_name_unresolved"

// resolveServices는 filter의 service.name을 service_id로 풀어 다시 컴파일하고, environment로 제한된 principal이면
// 허용 environment의 service_id 집합(mandatory predicate)을 가져온다. catalog가 없으면 둘 다 쓸 수 없다.
// unresolved는 풀리지 않은 이름이 있었다는 뜻이다.
func (h *Handler) resolveServices(ctx context.Context, p authz.Principal, filter *queryplan.Node, compiled queryplan.Compiled) (queryplan.Compiled, []string, bool, error) {
	if h.cfg.Services == nil {
		if len(compiled.ServiceNames) > 0 {
			return compiled, nil, false, unsupported("filter", "service.name needs the service catalog")
		}
		return compiled, nil, false, nil // 제한된 principal은 저장소가 403(ErrEnvironmentScoped)으로 막는다
	}
	if len(compiled.ServiceNames) == 0 && !p.EnvironmentRestricted() {
		return compiled, nil, false, nil
	}
	start := time.Now()
	compiled, scope, unresolved, err := h.lookupServices(ctx, p, filter, compiled)
	if h.cfg.ObserveCatalog != nil {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		h.cfg.ObserveCatalog(outcome, time.Since(start))
	}
	return compiled, scope, unresolved, err
}

func (h *Handler) lookupServices(ctx context.Context, p authz.Principal, filter *queryplan.Node, compiled queryplan.Compiled) (queryplan.Compiled, []string, bool, error) {
	unresolved := false
	if len(compiled.ServiceNames) > 0 {
		ids, err := h.cfg.Services.ResolveServiceNames(ctx, p, compiled.ServiceNames)
		if err != nil {
			return compiled, nil, false, err
		}
		for _, n := range compiled.ServiceNames {
			if len(ids[n]) == 0 {
				unresolved = true
			}
		}
		if compiled, err = queryplan.CompileWith(filter, queryplan.LogCatalog, queryplan.Options{ServiceIDs: ids}); err != nil {
			return compiled, nil, false, planError(err)
		}
	}
	if !p.EnvironmentRestricted() {
		return compiled, nil, unresolved, nil
	}
	scope, err := h.cfg.Services.EnvironmentServiceIDs(ctx, p)
	if err != nil {
		return compiled, nil, false, err
	}
	if len(scope) > telemetrystore.MaxEnvironmentServices {
		// catalog 상한은 근사라 넘을 수 있다. 내부 오류(500)가 아니라 예산 초과로 알린다.
		e := apierr.New(apierr.QueryBudgetExceeded, "이 key의 environment에 서비스가 너무 많습니다")
		e.Details = map[string]any{"max_environment_services": telemetrystore.MaxEnvironmentServices}
		return compiled, nil, false, e
	}
	return compiled, scope, unresolved, nil
}

func writeSearch(w http.ResponseWriter, resp searchResponse) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	return json.NewEncoder(w).Encode(resp)
}

// searchResponse는 D02 §19 공통 성공 응답이다.
type searchResponse struct {
	Data       []map[string]any `json:"data"`
	NextCursor *string          `json:"next_cursor"`
	Meta       Meta             `json:"meta"`
}

func newMeta(r *http.Request) Meta {
	return Meta{RequestID: httpapi.RequestIDFrom(r.Context()), SchemaVersion: SchemaVersion, FailedShards: []string{}, Warnings: []string{}}
}

func projection(req []string) ([]string, error) {
	if len(req) == 0 {
		return logFields, nil
	}
	known := map[string]bool{}
	for _, f := range logFields {
		known[f] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range req {
		if !known[f] {
			return nil, invalid("projection", "unknown field "+f+" (one of "+strings.Join(logFields, ", ")+")")
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out, nil
}

// logRow는 projection field만 담는다. 없는 trace·span ID는 null이다(0으로 채우지 않는다).
func logRow(r telemetrystore.LogRecord, proj []string) map[string]any {
	row := make(map[string]any, len(proj))
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	for _, f := range proj {
		switch f {
		case "time":
			row[f] = r.EventTime.Format(time.RFC3339Nano)
		case "event_id":
			row[f] = r.EventID
		case "service_id":
			row[f] = r.ServiceID
		case "severity_number":
			row[f] = r.Severity
		case "trace_id":
			row[f] = nullable(r.TraceID)
		case "span_id":
			row[f] = nullable(r.SpanID)
		case "body":
			row[f] = r.Body
		case "attributes":
			if r.Attributes == nil {
				row[f] = map[string]string{}
			} else {
				row[f] = r.Attributes
			}
		}
	}
	return row
}

// planError는 queryplan 입력 오류를 field 경로가 있는 400으로 바꾼다.
func planError(err error) error {
	var fe *queryplan.FieldError
	if errors.As(err, &fe) {
		return invalid(fe.Field, fe.Reason)
	}
	return err
}
