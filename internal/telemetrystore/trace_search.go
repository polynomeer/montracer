package telemetrystore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/queryplan"
)

// MaxTraceSearchRange는 interactive trace 검색 범위 상한이다 (D02 §19: trace/log 최대 24시간).
const MaxTraceSearchRange = 24 * time.Hour

// MaxTraceSearchLimit은 page 하나의 최대 trace 수다 (D02 §19).
const MaxTraceSearchLimit = 1000

// TraceSearchPadding은 범위 경계에 걸친 trace를 요약할 때 범위 앞뒤로 더 읽는 시간이다(ADR 0043 §2).
// 이보다 긴 trace는 범위 밖 span이 빠져 missing_parent·missing_root로 보일 수 있다(숨기지 않고 불완전으로 표시).
const TraceSearchPadding = time.Hour

// TraceSummarySpanLimit은 trace 하나에서 요약에 쓰는 최대 span 수다(단건 조회 MaxTraceSpans와 같다).
const TraceSummarySpanLimit = MaxTraceSpans

// TracePosition은 keyset 위치다. (start_time, trace_id) 내림차순에서 이보다 뒤(더 이른) trace부터 읽는다.
type TracePosition struct {
	Start   time.Time
	TraceID string // 소문자 hex 32
}

// TraceSearchQuery는 trace 검색 조건이다 (ADR 0043).
type TraceSearchQuery struct {
	Range TimeRange
	// SpanFilter는 span 단계 조건(queryplan.TraceSpanCatalog, parameter 접두어 "s")이다. 맞는 span이 하나라도 있는 trace를 고른다.
	SpanFilter queryplan.Compiled
	// TraceFilter는 trace 요약 조건(queryplan.TraceSummaryCatalog, 접두어 "t")이다.
	TraceFilter queryplan.Compiled
	Limit       int
	After       *TracePosition
	// ReceivedBefore가 있으면 그 시각까지 수신한 span만 본다(첫 page snapshot, D02 §15).
	ReceivedBefore time.Time
	// EnvironmentServices는 environment로 제한된 principal의 service_id 집합이다(ADR 0039). 제한되면 필수.
	EnvironmentServices []string
}

// TraceSummary는 검색 결과 trace 하나다(D02 §13 item + D05 §06 행).
type TraceSummary struct {
	TraceID       string
	Start         time.Time // 가장 이른 span 시작
	DurationMs    float64   // 가장 이른 시작 → 가장 늦은 끝
	SpanCount     uint64
	HasError      bool
	RootServiceID string // root span이 없으면 ""
	RootName      string // root span 이름(resource). 없으면 ""
	Roots         uint64
	MissingParent bool
	SpanLimitHit  bool
	LastReceived  time.Time // 이 trace에서 가장 늦게 수신한 span 시각 (D02 §22 last_updated_at)
}

// SearchTraces는 조건에 맞는 trace를 시작 시각 내림차순(동률은 trace_id)으로 limit개 요약하고, 다음 page가 있는지 돌려준다.
//
// 단계 (ADR 0043):
//  1. matched: 범위 안에서 span 조건에 맞는 span이 있는 trace_id
//  2. spans: 그 trace의 span을 범위 ± TraceSearchPadding에서 읽고 (trace_id, span_id)마다 최초 수신 한 행만 남긴다
//  3. 요약: 시작·끝·span 수·오류·root·부모 누락. 시작이 범위 안인 trace만, 요약 조건·keyset을 적용한다
//
// mandatory predicate(tenant·시간·expires_at·수신 snapshot·environment 범위 서비스)는 두 읽기 모두에 넣는다(계약 4).
// environment 범위 밖 span은 요약에도 들어가지 않는다 — 그 부모를 가진 span은 missing_parent로 보인다(ADR 0022 §3과 같다).
func (s *QueryStore) SearchTraces(ctx context.Context, p authz.Principal, q TraceSearchQuery, now time.Time) ([]TraceSummary, bool, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, false, err
	}
	scoped := uint8(0)
	scope := []string{}
	if p.EnvironmentRestricted() {
		if q.EnvironmentServices == nil {
			return nil, false, ErrEnvironmentScoped
		}
		if len(q.EnvironmentServices) > MaxEnvironmentServices {
			return nil, false, errors.New("telemetrystore: environment service scope too large")
		}
		scoped, scope = 1, q.EnvironmentServices
	}
	if now.IsZero() {
		return nil, false, errors.New("telemetrystore: now is required")
	}
	if err := q.Range.validate(MaxTraceSearchRange); err != nil {
		return nil, false, err
	}
	if q.Limit <= 0 || q.Limit > MaxTraceSearchLimit {
		return nil, false, invalidArg("limit", fmt.Sprintf("must be within [1, %d]", MaxTraceSearchLimit))
	}
	for _, f := range []queryplan.Compiled{q.SpanFilter, q.TraceFilter} {
		if f.SQL == "" || f.Unresolved {
			return nil, false, errors.New("telemetrystore: filters must be compiled with resolved service names (queryplan.CompileWith)")
		}
	}
	minVersion := uint64(0)
	if !q.ReceivedBefore.IsZero() {
		minVersion = minVersionReceivedBy(q.ReceivedBefore)
	}
	hasAfter, afterTime, afterID := uint8(0), time.Unix(0, 0), ""
	if q.After != nil {
		if !isLowerHex(q.After.TraceID, 32) {
			return nil, false, invalidArg("cursor", "invalid trace position")
		}
		hasAfter, afterTime, afterID = 1, q.After.Start, q.After.TraceID
	}
	args := []any{
		clickhouse.Named("tenant", p.Tenant().String()),
		clickhouse.Named("from", ch64(q.Range.From)), clickhouse.Named("to", ch64(q.Range.To)),
		clickhouse.Named("pad_from", ch64(q.Range.From.Add(-TraceSearchPadding))), clickhouse.Named("pad_to", ch64(q.Range.To.Add(TraceSearchPadding))),
		clickhouse.Named("now", seconds(now)), clickhouse.Named("limit", q.Limit+1),
		clickhouse.Named("min_version", minVersion),
		clickhouse.Named("env_scoped", scoped), clickhouse.Named("env_services", scope),
		clickhouse.Named("has_after", hasAfter), clickhouse.Named("after_time", ch64(afterTime)), clickhouse.Named("after_id", afterID),
		clickhouse.Named("span_limit", TraceSummarySpanLimit),
	}
	for _, f := range []queryplan.Compiled{q.SpanFilter, q.TraceFilter} {
		for name, v := range f.Params {
			args = append(args, clickhouse.Named(name, v))
		}
	}
	ctx = tenantContext(ctx, p.Tenant())
	// filter 조각은 queryplan이 만든 고정 column 표현과 {s<n>|t<n>:Type} parameter뿐이다(사용자 문자열 없음, ADR 0032).
	// mandatory predicate는 matched·spans 두 읽기에 모두 있다.
	rows, err := s.conn.Query(ctx, `
		WITH
		  matched AS (
			SELECT DISTINCT trace_id
			FROM spans_local
			WHERE tenant_id = {tenant:UUID}
			  AND event_time >= {from:DateTime64(9, 'UTC')} AND event_time < {to:DateTime64(9, 'UTC')}
			  AND expires_at > {now:DateTime('UTC')}
			  AND version >= {min_version:UInt64}
			  AND ({env_scoped:UInt8} = 0 OR service_id IN {env_services:Array(UUID)})
			  AND trace_id != unhex('00000000000000000000000000000000')
			  AND (`+q.SpanFilter.SQL+`)
		  ),
		  spans AS (
			SELECT trace_id, span_id, parent_span_id, service_id, name, event_time, duration_ns, status, received_at
			FROM spans_local
			WHERE tenant_id = {tenant:UUID}
			  AND event_time >= {pad_from:DateTime64(9, 'UTC')} AND event_time < {pad_to:DateTime64(9, 'UTC')}
			  AND expires_at > {now:DateTime('UTC')}
			  AND version >= {min_version:UInt64}
			  AND ({env_scoped:UInt8} = 0 OR service_id IN {env_services:Array(UUID)})
			  AND trace_id IN (SELECT trace_id FROM matched)
			ORDER BY trace_id, span_id, version DESC
			LIMIT 1 BY trace_id, span_id
		  )
		SELECT hex(trace_id), start_time, duration_ms, span_count, has_error, roots,
		       if(roots > 0, toString(root_service_id), ''), root_name, toBool(missing_parents > 0), toBool(span_count > {span_limit:UInt32}),
		       last_received
		FROM (
			SELECT trace_id,
			       min(event_time) AS start_time,
			       (max(toUnixTimestamp64Nano(event_time) + toInt64(duration_ns)) - toUnixTimestamp64Nano(min(event_time))) / 1e6 AS duration_ms,
			       count() AS span_count,
			       toBool(max(status = 2)) AS has_error,
			       countIf(parent_span_id = unhex('0000000000000000')) AS roots,
			       argMinIf(service_id, event_time, parent_span_id = unhex('0000000000000000')) AS root_service_id,
			       argMinIf(name, event_time, parent_span_id = unhex('0000000000000000')) AS root_name,
			       max(received_at) AS last_received,
			       groupArray({span_limit:UInt32})(span_id) AS ids,
			       arrayDistinct(arrayFilter(x -> x != unhex('0000000000000000'), groupArray({span_limit:UInt32})(parent_span_id))) AS parents,
			       -- 부모 누락: 부모 id 집합 중 span id 집합에 없는 것(hash 교집합, 선형). span 한도를 넘으면 잘린 목록으로는
			       -- 판정할 수 없어 세지 않는다(span_limit_reached 사유로 알린다, D02 §22)
			       if(count() > {span_limit:UInt32}, 0, length(parents) - length(arrayIntersect(parents, ids))) AS missing_parents
			FROM spans
			GROUP BY trace_id
		)
		WHERE start_time >= {from:DateTime64(9, 'UTC')} AND start_time < {to:DateTime64(9, 'UTC')}
		  AND ({has_after:UInt8} = 0 OR (start_time, trace_id) < ({after_time:DateTime64(9, 'UTC')}, toFixedString(unhex({after_id:String}), 16)))
		  AND (`+q.TraceFilter.SQL+`)
		ORDER BY start_time DESC, trace_id DESC
		LIMIT {limit:UInt32}`, args...)
	if err != nil {
		return nil, false, classify("trace search", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TraceSummary
	for rows.Next() {
		var t TraceSummary
		if err := rows.Scan(&t.TraceID, &t.Start, &t.DurationMs, &t.SpanCount, &t.HasError, &t.Roots,
			&t.RootServiceID, &t.RootName, &t.MissingParent, &t.SpanLimitHit, &t.LastReceived); err != nil {
			return nil, false, fmt.Errorf("telemetrystore: scan trace summary: %w", err)
		}
		t.TraceID, t.Start, t.LastReceived = lower(t.TraceID), t.Start.UTC(), t.LastReceived.UTC()
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, false, classify("trace search", err)
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return out, more, nil
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
