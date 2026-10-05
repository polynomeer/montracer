// Package telemetrystore는 분석 저장소(ClickHouse) 조회 계층이다 (D02 §09, §15, ADR 0003, 0018).
//
// 두 겹으로 tenant를 격리한다.
//  1. 모든 쿼리는 tenant·시간 범위·expires_at을 mandatory predicate로 넣는다 (D02 §15).
//  2. query 계정에는 row policy가 걸려 있어 요청 설정 SQL_montracer_tenant의 행만 보인다.
//     설정이 없으면 0행이다(fail closed). 1이 빠져도 2가 막는다.
//
// 사용자 값(trace_id, metric 이름, label 값 등)은 서버 측 query parameter({name:Type})로만 보낸다.
// clickhouse-go의 위치 인자($1)는 값을 SQL 문자열에 끼워 보내므로 system.query_log.query에 남는다 (D04 §10, ADR 0032).
//
// raw SQL은 이 패키지(와 이후 query planner)에만 둔다 (D06 §10).
package telemetrystore

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/polynomeer/montracer/internal/authz"
)

// TenantSetting은 row policy가 읽는 요청 설정 이름이다 (migrations/clickhouse/00001).
const TenantSetting = "SQL_montracer_tenant"

// ErrNotReadOnly는 query store DSN이 쓰기 가능한 계정일 때 반환한다.
// query service는 읽기 전용 계정(readonly≥1)으로만 접속해야 한다 (ADR 0018).
var ErrNotReadOnly = errors.New("telemetrystore: query account must be read-only")

// QueryStore는 query 계정 연결이다.
type QueryStore struct {
	conn driver.Conn
}

// OpenQuery는 query 계정으로 연결하고 읽기 전용인지 확인한다.
func OpenQuery(ctx context.Context, dsn string) (*QueryStore, error) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		// DSN에는 비밀번호가 있으므로 원인 문자열을 싣지 않는다.
		return nil, errors.New("telemetrystore: invalid dsn")
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, unavailable("open", err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, unavailable("ping", err)
	}
	var readonly uint8
	if err := conn.QueryRow(ctx, `SELECT getSetting('readonly')`).Scan(&readonly); err != nil {
		_ = conn.Close()
		return nil, classify("check readonly", err)
	}
	if readonly == 0 {
		_ = conn.Close()
		return nil, ErrNotReadOnly
	}
	return &QueryStore{conn: conn}, nil
}

// Close는 연결을 닫는다.
func (s *QueryStore) Close() error { return s.conn.Close() }

// Ping은 연결을 확인한다 (readiness).
func (s *QueryStore) Ping(ctx context.Context) error {
	if err := s.conn.Ping(ctx); err != nil {
		return unavailable("ping", err)
	}
	return nil
}

// tenantContext는 row policy용 tenant 설정을 실은 context를 만든다.
func tenantContext(ctx context.Context, tenant authz.TenantID) context.Context {
	// 사용자 정의 설정은 CustomSetting으로 보내야 서버가 SQL_ 접두어 설정으로 받는다.
	return clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		TenantSetting: clickhouse.CustomSetting{Value: tenant.String()},
	}))
}

// seconds는 시각을 초 단위로 내린다. DateTime parameter는 소수 초를 받지 않으며,
// 이전 client 측 binding(toDateTime)도 초 단위였으므로 predicate 의미가 같다.
func seconds(t time.Time) time.Time { return t.Truncate(time.Second) }

// TimeRange는 [From, To) 구간이다 (D02 §12).
type TimeRange struct {
	From, To time.Time
}

// MaxTraceLookupRange는 trace 단건 조회의 최대 범위다 (D02 §13 "최대 7일의 명시적 경계").
const MaxTraceLookupRange = 7 * 24 * time.Hour

func (r TimeRange) validate(max time.Duration) error {
	if r.From.IsZero() || r.To.IsZero() || !r.From.Before(r.To) {
		return invalidArg("range", "must be a non-empty [from, to)")
	}
	if r.To.Sub(r.From) > max {
		return &budgetError{op: "range", details: map[string]any{"max_range_seconds": int64(max / time.Second)}}
	}
	return nil
}

// Span은 조회 결과 span이다. payload(원본 OTLP)는 상세 조회에서만 읽는다.
type Span struct {
	TraceID      string // 소문자 hex 32
	SpanID       string // 소문자 hex 16
	ParentSpanID string
	ServiceID    string
	Name         string
	StartTime    time.Time
	DurationNs   uint64
	Status       uint8
	Kind         uint8
}

// SpanRecord는 trace 상세 조회용 span이다. Payload는 단일 span OTLP protobuf다 (ADR 0021 §5).
type SpanRecord struct {
	Span
	ReceivedAt time.Time
	Payload    []byte
}

// MaxTraceSpans는 trace 단건 조회가 돌려주는 span 상한이다.
// query 계정의 기본 결과 행 예산(max_result_rows 10,000, ADR 0018)과 같다. 넘으면 호출자가 truncation을 표시한다.
const MaxTraceSpans = 10000

// TraceQuery는 trace 단건 조회 조건이다.
type TraceQuery struct {
	TraceID   string    // 소문자 hex 32
	Range     TimeRange // 최대 7일
	ServiceID string    // 선택. 비우면 모든 서비스
	Limit     int       // 0이면 MaxTraceSpans. 상한도 MaxTraceSpans
}

// TraceSpans는 trace 하나의 span을 읽는다 (payload 없이).
func (s *QueryStore) TraceSpans(ctx context.Context, p authz.Principal, traceIDHex string, r TimeRange, now time.Time) ([]Span, error) {
	recs, err := s.traceRecords(ctx, p, TraceQuery{TraceID: traceIDHex, Range: r}, now, false)
	if err != nil {
		return nil, err
	}
	out := make([]Span, len(recs))
	for i, rec := range recs {
		out[i] = rec.Span
	}
	return out, nil
}

// TraceSpanRecords는 trace 하나의 span을 payload·수신 시각과 함께 읽는다 (trace 상세, D02 §13).
func (s *QueryStore) TraceSpanRecords(ctx context.Context, p authz.Principal, q TraceQuery, now time.Time) ([]SpanRecord, error) {
	return s.traceRecords(ctx, p, q, now, true)
}

// traceRecords는 lookup 테이블로 필요한 날짜·서비스만 찾은 뒤 원본을 조회한다 (D02 §09, §15).
// 같은 span key는 version이 가장 큰 행(최초 수신, ADR 0021 §4)만 남긴다(ReplacingMergeTree merge 전 중복 제거, FINAL 미사용).
// now는 expires_at 판정 시각이다. 삭제 tombstone predicate는 삭제 원장(F09) 구현 시 추가한다 (ADR 0018 §7).
func (s *QueryStore) traceRecords(ctx context.Context, p authz.Principal, q TraceQuery, now time.Time, withPayload bool) ([]SpanRecord, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, err
	}
	if now.IsZero() {
		// zero면 expires_at > 1970이 되어 만료 데이터가 노출된다.
		return nil, errors.New("telemetrystore: now is required")
	}
	r := q.Range
	if err := r.validate(MaxTraceLookupRange); err != nil {
		return nil, err
	}
	traceIDHex := q.TraceID
	traceID, err := hex.DecodeString(traceIDHex)
	if err != nil || len(traceID) != 16 || traceIDHex != hex.EncodeToString(traceID) {
		return nil, invalidArg("trace_id", "must be 32 lowercase hex characters")
	}
	if string(traceID) == string(make([]byte, 16)) {
		return nil, invalidArg("trace_id", "must not be all zeros") // 0 byte는 "없는 값" (D02 §09)
	}
	if q.ServiceID != "" {
		if _, err := authz.ParseTenantID(q.ServiceID); err != nil { // 같은 UUID 형식 검사
			return nil, invalidArg("service_id", "must be a UUID")
		}
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxTraceSpans {
		limit = MaxTraceSpans
	}
	payloadCol := "''"
	if withPayload {
		payloadCol = "payload"
	}
	tenant := p.Tenant()
	ctx = tenantContext(ctx, tenant)
	// service_id는 빈 문자열이면 조건을 끈다. 사용자 값은 서버 측 query parameter로만 넣는다 (D02 §15, ADR 0032).
	rows, err := s.conn.Query(ctx, `
		WITH locations AS (
			SELECT event_date, service_id FROM trace_lookup
			WHERE tenant_id = {tenant:UUID} AND trace_id = unhex({trace_id:String})
			  AND event_date >= toDate({from:DateTime('UTC')}) AND event_date <= toDate({to:DateTime('UTC')})
			  AND expires_at > {now:DateTime('UTC')}
			  AND ({service_id:String} = '' OR service_id = toUUIDOrZero({service_id:String}))
			GROUP BY event_date, service_id
		)
		SELECT hex(trace_id), hex(span_id), hex(parent_span_id), toString(service_id), name,
		       event_time, duration_ns, status, span_kind, received_at, `+payloadCol+`
		FROM (
			-- span key별 최대 version 한 행 (D02 §05, §10)
			SELECT trace_id, span_id, parent_span_id, service_id, name, event_time, duration_ns, status, span_kind,
			       received_at, payload
			FROM spans_local
			WHERE tenant_id = {tenant:UUID}
			  AND (toDate(event_time), service_id) IN (SELECT event_date, service_id FROM locations)
			  AND trace_id = unhex({trace_id:String})
			  AND event_time >= {from:DateTime('UTC')} AND event_time < {to:DateTime('UTC')}
			  AND expires_at > {now:DateTime('UTC')}
			ORDER BY span_id, version DESC
			LIMIT 1 BY span_id
		)
		ORDER BY event_time, span_id
		LIMIT {limit:UInt32}`,
		clickhouse.Named("tenant", tenant.String()), clickhouse.Named("trace_id", traceIDHex),
		clickhouse.Named("from", seconds(r.From)), clickhouse.Named("to", seconds(r.To)), clickhouse.Named("now", seconds(now)),
		clickhouse.Named("service_id", q.ServiceID), clickhouse.Named("limit", limit))
	if err != nil {
		return nil, classify("trace spans", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SpanRecord
	for rows.Next() {
		var (
			rec     SpanRecord
			payload string
		)
		sp := &rec.Span
		if err := rows.Scan(&sp.TraceID, &sp.SpanID, &sp.ParentSpanID, &sp.ServiceID, &sp.Name,
			&sp.StartTime, &sp.DurationNs, &sp.Status, &sp.Kind, &rec.ReceivedAt, &payload); err != nil {
			return nil, fmt.Errorf("telemetrystore: scan span: %w", err)
		}
		sp.TraceID, sp.SpanID, sp.ParentSpanID = lower(sp.TraceID), lower(sp.SpanID), lower(sp.ParentSpanID)
		if withPayload {
			rec.Payload = []byte(payload)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, classify("trace spans", err)
	}
	return out, nil
}
