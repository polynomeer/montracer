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
}

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
	rng := telemetrystore.TimeRange{From: now.Add(-defaultSearchRange), To: now}
	if req.Range != nil {
		rng = telemetrystore.TimeRange{From: req.Range.From.UTC(), To: req.Range.To.UTC()}
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

	binding := apicursor.Binding{
		Tenant: p.Tenant().String(),
		// 권한 fingerprint: principal이 바뀌면(다른 key·사용자) cursor를 쓸 수 없다
		Fingerprint: apicursor.Fingerprint(p.Kind().String(), p.Subject(), string(authz.TelemetryRead)),
		// page 크기는 넣지 않는다(page마다 바꿀 수 있다)
		QueryHash: apicursor.Fingerprint("logs", rng.From.Format(time.RFC3339Nano), rng.To.Format(time.RFC3339Nano),
			compiled.Canonical, strings.Join(proj, ",")),
	}
	q := telemetrystore.LogQuery{Range: rng, Filter: compiled, Limit: limit, ReceivedBefore: now}
	if req.Cursor != "" {
		claims, err := h.cfg.Cursor.Decode(req.Cursor, binding)
		var pos logPosition
		if err != nil || json.Unmarshal(claims.Position, &pos) != nil || pos.EventID == "" {
			return invalid("cursor", "invalid, expired, or not for this query")
		}
		q.After = &telemetrystore.LogPosition{EventTime: time.Unix(0, pos.TimeNs).UTC(), EventID: pos.EventID}
		q.ReceivedBefore = claims.Snapshot
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.QueryTimeout)
	defer cancel()
	records, more, err := h.cfg.Logs.SearchLogs(ctx, p, q, now)
	if err != nil {
		return err
	}
	resp := searchResponse{Data: make([]map[string]any, 0, len(records)), Meta: newMeta(r)}
	for _, rec := range records {
		resp.Data = append(resp.Data, logRow(rec, proj))
	}
	if more && len(records) > 0 {
		last := records[len(records)-1]
		tok, err := h.cfg.Cursor.Encode(binding, q.ReceivedBefore, logPosition{TimeNs: last.EventTime.UnixNano(), EventID: last.EventID})
		if err != nil {
			return err
		}
		resp.NextCursor = &tok
	}
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
