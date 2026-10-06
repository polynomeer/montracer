package telemetrystore

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/queryplan"
)

// MaxLogSearchRange는 interactive log 검색 범위 상한이다 (D02 §19: trace/log 최대 24시간).
const MaxLogSearchRange = 24 * time.Hour

// MaxLogLimit은 page 하나의 최대 행 수다 (D02 §19: 기본 100·최대 1,000).
const MaxLogLimit = 1000

// ErrEnvironmentScoped는 environment로 제한된 principal이 log를 검색하려 했다는 뜻이다.
// logs_local에는 environment가 없어(서비스 catalog 전, ADR 0037 §4) 범위 안임을 증명할 수 없다 — 보여주지 않는다.
var ErrEnvironmentScoped = fmt.Errorf("telemetrystore: environment-scoped keys cannot search logs yet: %w", authz.ErrForbidden)

// LogPosition은 keyset 위치다. (event_time, event_id) 내림차순에서 이보다 뒤(더 오래된) 행부터 읽는다.
type LogPosition struct {
	EventTime time.Time
	EventID   string
}

// LogQuery는 log 검색 조건이다. Filter는 queryplan.Compile 결과다(사용자 값은 parameter).
type LogQuery struct {
	Range  TimeRange
	Filter queryplan.Compiled
	Limit  int
	After  *LogPosition
	// ReceivedBefore가 있으면 그 시각까지 수신한 행만 본다(첫 page snapshot, D02 §15).
	ReceivedBefore time.Time
}

// LogRecord는 검색 결과 log 하나다.
type LogRecord struct {
	EventID    string
	ServiceID  string
	EventTime  time.Time
	Severity   uint8
	TraceID    string // 소문자 hex, 없으면 ""
	SpanID     string
	Body       string
	Attributes map[string]string
}

// minVersionReceivedBy는 receivedBefore까지 수신한 행의 최소 version이다.
// version = MaxUint64 − (수신 ms<<20 | offset) (pipeline, ADR 0021 §4)라 수신이 이를수록 크다.
func minVersionReceivedBy(t time.Time) uint64 {
	ms := t.UnixMilli()
	if ms < 0 {
		ms = 0
	}
	return math.MaxUint64 - (uint64(ms)<<20 | (1<<20 - 1)) //nolint:gosec // 위에서 음수 제외
}

func ch64(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05.000000000") }

// SearchLogs는 log를 (event_time, event_id) 내림차순으로 limit개 읽고, 다음 page가 있는지 함께 돌려준다 (D02 §15, §19).
//
// mandatory predicate(tenant·시간·expires_at·수신 snapshot)는 여기서 넣는다 — filter AST에는 없다(계약 4).
// tenant는 row policy로 한 번 더 막는다(ADR 0018). 같은 event_id는 최초 수신(version 최대) 한 행만 남긴다
// (FINAL 없이 시간 범위 안에서만 dedup — bounded). 삭제 tombstone predicate는 삭제 원장(F09)과 함께 추가한다(ADR 0018 §7).
func (s *QueryStore) SearchLogs(ctx context.Context, p authz.Principal, q LogQuery, now time.Time) ([]LogRecord, bool, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, false, err
	}
	if p.EnvironmentRestricted() {
		return nil, false, ErrEnvironmentScoped
	}
	if now.IsZero() {
		return nil, false, errors.New("telemetrystore: now is required")
	}
	if err := q.Range.validate(MaxLogSearchRange); err != nil {
		return nil, false, err
	}
	if q.Limit <= 0 || q.Limit > MaxLogLimit {
		return nil, false, invalidArg("limit", fmt.Sprintf("must be within [1, %d]", MaxLogLimit))
	}
	if q.Filter.SQL == "" {
		return nil, false, errors.New("telemetrystore: filter must be compiled (use queryplan.Compile)")
	}
	minVersion := uint64(0)
	if !q.ReceivedBefore.IsZero() {
		minVersion = minVersionReceivedBy(q.ReceivedBefore)
	}
	hasAfter, afterTime, afterID := uint8(0), time.Unix(0, 0), ""
	if q.After != nil {
		hasAfter, afterTime, afterID = 1, q.After.EventTime, q.After.EventID
	}
	args := []any{
		clickhouse.Named("tenant", p.Tenant().String()),
		clickhouse.Named("from", ch64(q.Range.From)), clickhouse.Named("to", ch64(q.Range.To)),
		clickhouse.Named("now", seconds(now)), clickhouse.Named("limit", q.Limit+1),
		clickhouse.Named("min_version", minVersion),
		clickhouse.Named("has_after", hasAfter), clickhouse.Named("after_time", ch64(afterTime)), clickhouse.Named("after_id", afterID),
	}
	for name, v := range q.Filter.Params {
		args = append(args, clickhouse.Named(name, v))
	}
	ctx = tenantContext(ctx, p.Tenant())
	// filter 조각은 queryplan이 만든 고정 column 표현과 {f<n>:Type} parameter뿐이다(사용자 문자열 없음).
	// 1) 안쪽: mandatory predicate만으로 고른 뒤 event_id마다 최초 수신(version 최대) 한 행 — event_time과 무관하다.
	// 2) 바깥: dedup된 행에 사용자 filter·keyset·정렬을 적용한다. 그래서 같은 event_id가 두 page에 나오지 않고,
	//    나중에 받은 재전송이 시각이 크다는 이유로 보이지 않는다(D02 §05·§21 최초 승인 값, 리뷰에서 발견).
	// filter 조각은 queryplan이 만든 고정 column 표현과 {f<n>:Type} parameter뿐이다(사용자 문자열 없음).
	rows, err := s.conn.Query(ctx, `
		SELECT event_id, toString(service_id), event_time, severity, hex(trace_id), hex(span_id), body, attributes
		FROM (
			SELECT event_id, service_id, event_time, severity, trace_id, span_id, body, attributes
			FROM logs_local
			WHERE tenant_id = {tenant:UUID}
			  AND event_time >= {from:DateTime64(9, 'UTC')} AND event_time < {to:DateTime64(9, 'UTC')}
			  AND expires_at > {now:DateTime('UTC')}
			  AND version >= {min_version:UInt64}
			ORDER BY event_id, version DESC
			LIMIT 1 BY event_id
		)
		WHERE ({has_after:UInt8} = 0 OR (event_time, event_id) < ({after_time:DateTime64(9, 'UTC')}, {after_id:String}))
		  AND (`+q.Filter.SQL+`)
		ORDER BY event_time DESC, event_id DESC
		LIMIT {limit:UInt32}`, args...)
	if err != nil {
		return nil, false, classify("log search", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LogRecord
	for rows.Next() {
		var (
			r               LogRecord
			traceID, spanID string
		)
		if err := rows.Scan(&r.EventID, &r.ServiceID, &r.EventTime, &r.Severity, &traceID, &spanID, &r.Body, &r.Attributes); err != nil {
			return nil, false, fmt.Errorf("telemetrystore: scan log: %w", err)
		}
		r.EventTime = r.EventTime.UTC()
		r.TraceID, r.SpanID = nonZeroHex(traceID), nonZeroHex(spanID)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, classify("log search", err)
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return out, more, nil
}

// nonZeroHex는 모두 0인 ID(없는 값, D02 §09)를 ""로 바꾼다.
func nonZeroHex(s string) string {
	b, err := hex.DecodeString(s)
	if err != nil {
		return ""
	}
	for _, x := range b {
		if x != 0 {
			return lower(s)
		}
	}
	return ""
}
