package query

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/queryplan"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// TraceStore는 trace 검색 저장소다 (telemetrystore.QueryStore, ADR 0043).
type TraceStore interface {
	SearchTraces(ctx context.Context, p authz.Principal, q telemetrystore.TraceSearchQuery, now time.Time) ([]telemetrystore.TraceSummary, bool, error)
}

// traceFields는 trace 검색 결과 field(projection)다. D02 §13 item(trace_id, root_service, duration_ms, span_count,
// has_error, complete)에 D05 §06 행(시작 시각, resource = root span 이름)과 D02 §22 사유·last_updated_at을 더했다(ADR 0043 §3).
var traceFields = []string{"trace_id", "start_time", "duration_ms", "span_count", "has_error", "complete", "reasons",
	"root_service", "root_service_id", "root_name", "last_updated_at"}

type tracePosition struct {
	StartNs int64  `json:"t"`
	TraceID string `json:"id"`
	Served  int    `json:"n"`
}

// searchTraces는 POST /api/v1/query/traces(또는 /query signal=traces)다 (D02 §13·§19, ADR 0043).
// 요청 형식·cursor·누적 상한·tenant 실행 slot·service.name 풀기는 log 검색과 같다.
func (h *Handler) searchTraces(w http.ResponseWriter, r *http.Request, p authz.Principal, req searchRequest) error {
	now := h.cfg.Now().UTC()
	rangeKey := "default:15m"
	rng := telemetrystore.TimeRange{From: now.Add(-defaultSearchRange), To: now}
	if req.Range != nil {
		rng = telemetrystore.TimeRange{From: req.Range.From.UTC(), To: req.Range.To.UTC()}
		rangeKey = rng.From.Format(time.RFC3339Nano) + "/" + rng.To.Format(time.RFC3339Nano)
	}
	for i, o := range req.Order {
		if i > 0 || o.Field != "start_time" || (o.Direction != "" && o.Direction != "desc") {
			return unsupported("order", `traces are ordered by [{"field":"start_time","direction":"desc"}] only`)
		}
	}
	proj, err := projectionOf(req.Projection, traceFields)
	if err != nil {
		return err
	}
	limit := req.Limit
	switch {
	case limit == 0:
		limit = 100
	case limit < 0 || limit > telemetrystore.MaxTraceSearchLimit:
		return invalid("limit", "must be within [1, 1000]")
	}
	// 나누기 전에 전체 filter를 한 번 검증한다: 깊이 4·조건 20·빈 and 같은 한도는 단계별이 아니라 요청 전체에 건다(D02 §15)
	if _, err := queryplan.Compile(req.Filter, queryplan.TraceCatalog); err != nil {
		return planError(err)
	}
	spanNode, sumNode, err := queryplan.Split(req.Filter, queryplan.IsTraceSummaryField)
	if err != nil {
		return planError(err)
	}
	spanCompiled, err := queryplan.CompileWith(spanNode, queryplan.TraceSpanCatalog, queryplan.Options{ParamPrefix: "s"})
	if err != nil {
		return planError(err)
	}
	sumCompiled, err := queryplan.CompileWith(sumNode, queryplan.TraceSummaryCatalog, queryplan.Options{ParamPrefix: "t"})
	if err != nil {
		return planError(err)
	}

	fp := append([]string{p.Kind().String(), p.Subject(), string(authz.TelemetryRead)}, p.Environments()...)
	binding := apicursor.Binding{
		Tenant:      p.Tenant().String(),
		Fingerprint: apicursor.Fingerprint(fp...),
		QueryHash:   apicursor.Fingerprint("traces", rangeKey, queryplan.CanonicalOf(req.Filter), strings.Join(proj, ",")),
	}
	q := telemetrystore.TraceSearchQuery{Range: rng, Limit: limit, ReceivedBefore: now, TraceFilter: sumCompiled}
	served := 0
	if req.Cursor != "" {
		claims, err := h.cfg.Cursor.Decode(req.Cursor, binding)
		var pos tracePosition
		if err != nil || json.Unmarshal(claims.Position, &pos) != nil || pos.TraceID == "" || pos.Served < 0 {
			return invalid("cursor", "invalid, expired, or not for this query")
		}
		q.After = &telemetrystore.TracePosition{Start: time.Unix(0, pos.StartNs).UTC(), TraceID: pos.TraceID}
		q.ReceivedBefore = claims.Snapshot
		served = pos.Served
		if req.Range == nil {
			q.Range = telemetrystore.TimeRange{From: claims.Snapshot.Add(-defaultSearchRange), To: claims.Snapshot}
		}
	}
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
	release, err := h.gate.acquire(ctx, p.Tenant().String())
	if err != nil {
		return err
	}
	defer release()
	recompile := func(ids map[string][]string) (queryplan.Compiled, error) {
		return queryplan.CompileWith(spanNode, queryplan.TraceSpanCatalog, queryplan.Options{ParamPrefix: "s", ServiceIDs: ids})
	}
	var unresolved bool
	if q.SpanFilter, q.EnvironmentServices, unresolved, err = h.resolveServices(ctx, p, spanCompiled, recompile); err != nil {
		return err
	}
	if unresolved {
		resp.Meta.Warnings = append(resp.Meta.Warnings, WarningServiceNameUnresolved)
	}
	traces, more, err := h.cfg.Traces.SearchTraces(ctx, p, q, now)
	if err != nil {
		return err
	}
	names, err := h.rootServiceNames(ctx, p, traces)
	if err != nil {
		return err
	}
	for _, t := range traces {
		resp.Data = append(resp.Data, traceRow(t, names, proj))
	}
	served += len(traces)
	if more && served >= MaxInteractiveRows {
		resp.Meta.Warnings = append(resp.Meta.Warnings, WarningInteractiveRowLimit)
		more = false
	}
	if more && len(traces) > 0 {
		last := traces[len(traces)-1]
		tok, err := h.cfg.Cursor.Encode(binding, q.ReceivedBefore, tracePosition{StartNs: last.Start.UnixNano(), TraceID: last.TraceID, Served: served})
		if err != nil {
			return err
		}
		resp.NextCursor = &tok
	}
	return writeSearch(w, resp)
}

// rootServiceNames는 root 서비스 id → 이름을 catalog로 푼다. catalog가 없거나 아직 등록 전이면 이름은 null이다.
func (h *Handler) rootServiceNames(ctx context.Context, p authz.Principal, traces []telemetrystore.TraceSummary) (map[string]string, error) {
	if h.cfg.Services == nil {
		return map[string]string{}, nil
	}
	seen := map[string]bool{}
	var ids []string
	for _, t := range traces {
		if t.RootServiceID != "" && !seen[t.RootServiceID] {
			seen[t.RootServiceID] = true
			ids = append(ids, t.RootServiceID)
		}
	}
	return h.cfg.Services.ServiceNames(ctx, p, ids)
}

// traceRow는 projection field만 담는다. 모르는 값(root가 없는 trace의 root 서비스·이름)은 null이다.
// complete=false는 구조 불완전(D02 §22)이고 meta.partial(실행 일부 실패)과 다르다.
func traceRow(t telemetrystore.TraceSummary, names map[string]string, proj []string) map[string]any {
	reasons := []string{}
	if t.Roots == 0 {
		reasons = append(reasons, ReasonMissingRoot)
	}
	if t.MissingParent {
		reasons = append(reasons, ReasonMissingParent)
	}
	if t.SpanLimitHit {
		reasons = append(reasons, ReasonSpanLimit)
	}
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	row := make(map[string]any, len(proj))
	for _, f := range proj {
		switch f {
		case "trace_id":
			row[f] = t.TraceID
		case "start_time":
			row[f] = t.Start.Format(time.RFC3339Nano)
		case "duration_ms":
			row[f] = t.DurationMs
		case "span_count":
			row[f] = t.SpanCount
		case "has_error":
			row[f] = t.HasError
		case "complete":
			row[f] = len(reasons) == 0
		case "reasons":
			row[f] = reasons
		case "root_service":
			if n, ok := names[t.RootServiceID]; ok {
				row[f] = n
			} else {
				row[f] = nil
			}
		case "root_service_id":
			row[f] = nullable(t.RootServiceID)
		case "last_updated_at":
			row[f] = t.LastReceived.Format(time.RFC3339Nano)
		case "root_name":
			if t.Roots == 0 {
				row[f] = nil
			} else {
				row[f] = t.RootName
			}
		}
	}
	return row
}
