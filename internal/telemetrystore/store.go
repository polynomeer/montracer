// Package telemetrystore는 분석 저장소(ClickHouse) 조회 계층이다 (D02 §09, §15, ADR 0003, 0018).
//
// 두 겹으로 tenant를 격리한다.
//  1. 모든 쿼리는 tenant·시간 범위·expires_at을 mandatory predicate로 넣는다 (D02 §15).
//  2. query 계정에는 row policy가 걸려 있어 요청 설정 SQL_montracer_tenant의 행만 보인다.
//     설정이 없으면 0행이다(fail closed). 1이 빠져도 2가 막는다.
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

// tenantContext는 row policy용 tenant 설정을 실은 context를 만든다.
func tenantContext(ctx context.Context, tenant authz.TenantID) context.Context {
	// 사용자 정의 설정은 CustomSetting으로 보내야 서버가 SQL_ 접두어 설정으로 받는다.
	return clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		TenantSetting: clickhouse.CustomSetting{Value: tenant.String()},
	}))
}

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

// TraceSpans는 trace 하나의 span을 읽는다.
// lookup 테이블로 필요한 날짜·서비스만 찾은 뒤 원본을 조회한다 (D02 §09, §15).
// 같은 span key는 version이 가장 큰 행만 남긴다(ReplacingMergeTree merge 전 중복 제거, FINAL 미사용).
// now는 expires_at 판정 시각이다. 삭제 tombstone predicate는 삭제 원장(F09) 구현 시 추가한다 (ADR 0018 §7).
func (s *QueryStore) TraceSpans(ctx context.Context, p authz.Principal, traceIDHex string, r TimeRange, now time.Time) ([]Span, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, err
	}
	if now.IsZero() {
		// zero면 expires_at > 1970이 되어 만료 데이터가 노출된다.
		return nil, errors.New("telemetrystore: now is required")
	}
	if err := r.validate(MaxTraceLookupRange); err != nil {
		return nil, err
	}
	traceID, err := hex.DecodeString(traceIDHex)
	if err != nil || len(traceID) != 16 || traceIDHex != hex.EncodeToString(traceID) {
		return nil, invalidArg("trace_id", "must be 32 lowercase hex characters")
	}
	if string(traceID) == string(make([]byte, 16)) {
		return nil, invalidArg("trace_id", "must not be all zeros") // 0 byte는 "없는 값" (D02 §09)
	}
	tenant := p.Tenant()
	ctx = tenantContext(ctx, tenant)
	rows, err := s.conn.Query(ctx, `
		WITH locations AS (
			SELECT event_date, service_id FROM trace_lookup
			WHERE tenant_id = $1 AND trace_id = unhex($2)
			  AND event_date >= toDate($3) AND event_date <= toDate($4)
			  AND expires_at > $5
			GROUP BY event_date, service_id
		)
		SELECT hex(trace_id), hex(span_id), hex(parent_span_id), toString(service_id), name,
		       event_time, duration_ns, status, span_kind
		FROM (
			-- span key별 최대 version 한 행 (D02 §05, §10)
			SELECT trace_id, span_id, parent_span_id, service_id, name, event_time, duration_ns, status, span_kind
			FROM spans_local
			WHERE tenant_id = $1
			  AND (toDate(event_time), service_id) IN (SELECT event_date, service_id FROM locations)
			  AND trace_id = unhex($2)
			  AND event_time >= $3 AND event_time < $4
			  AND expires_at > $5
			ORDER BY span_id, version DESC
			LIMIT 1 BY span_id
		)
		ORDER BY event_time, span_id`,
		tenant.String(), traceIDHex, r.From, r.To, now)
	if err != nil {
		return nil, classify("trace spans", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Span
	for rows.Next() {
		var sp Span
		if err := rows.Scan(&sp.TraceID, &sp.SpanID, &sp.ParentSpanID, &sp.ServiceID, &sp.Name,
			&sp.StartTime, &sp.DurationNs, &sp.Status, &sp.Kind); err != nil {
			return nil, fmt.Errorf("telemetrystore: scan span: %w", err)
		}
		sp.TraceID, sp.SpanID, sp.ParentSpanID = lower(sp.TraceID), lower(sp.SpanID), lower(sp.ParentSpanID)
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, classify("trace spans", err)
	}
	return out, nil
}
