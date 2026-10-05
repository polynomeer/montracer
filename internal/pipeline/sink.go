package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Sink는 batch를 영속 저장한다. 반환이 nil이면 batch 전체가 durable하게 저장된 것이다.
type Sink interface {
	Write(ctx context.Context, b *Batch) error
}

// ClickHouseSink는 ingest 계정(INSERT만 가능)으로 원본 테이블에 동기 insert한다 (ADR 0018, 0021).
type ClickHouseSink struct {
	conn driver.Conn
	// InsertQuorum이 0보다 크면 그만큼의 replica 확인 뒤 insert가 성공한다 (D02 §05 "필요한 replica 확인").
	// 단일 node 로컬은 0이다.
	insertQuorum int
}

// ErrIngestCanRead는 ingest DSN이 원본 테이블을 읽을 수 있는 계정일 때 반환한다.
// worker가 전 tenant 데이터를 읽을 수 있으면 최소 권한이 깨진다 (ADR 0018 §2).
var ErrIngestCanRead = errors.New("pipeline: ingest account must not be able to read telemetry tables")

// OpenClickHouseSink는 ingest 계정으로 연결하고 권한을 확인한다.
func OpenClickHouseSink(ctx context.Context, dsn string, insertQuorum int) (*ClickHouseSink, error) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		// DSN에는 비밀번호가 있으므로 원인 문자열을 싣지 않는다.
		return nil, errors.New("pipeline: invalid clickhouse dsn")
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("pipeline: clickhouse open: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("pipeline: clickhouse ping: %w", err)
	}
	// 읽기 시도가 성공하면 권한이 과하다. 실패 원인은 권한 오류(497)여야 한다.
	var n uint64
	err = conn.QueryRow(ctx, `SELECT count() FROM spans_local WHERE 0`).Scan(&n)
	if err == nil {
		_ = conn.Close()
		return nil, ErrIngestCanRead
	}
	var ex *clickhouse.Exception
	if !errors.As(err, &ex) || ex.Code != 497 {
		_ = conn.Close()
		return nil, fmt.Errorf("pipeline: clickhouse grant check: %w", err)
	}
	return &ClickHouseSink{conn: conn, insertQuorum: insertQuorum}, nil
}

// rowErr는 행 하나의 append 실패다. 드라이버 변환 오류 문구에는 행 값이 실릴 수 있어 원인을 감싸지 않는다.
// append는 전송 전 클라이언트 단계라 같은 행이면 다시 해도 실패한다(결정적).
func rowErr(table string, idx int) error { return &RowError{Table: table, Index: idx} }

// Close는 연결을 닫는다.
func (s *ClickHouseSink) Close() error { return s.conn.Close() }

// Write는 batch의 행을 테이블별로 insert한다. 같은 batch를 다시 쓰면 token으로 중복 insert가 무시된다.
// 테이블 하나라도 실패하면 오류다. 이미 성공한 테이블은 재시도 때 token으로 걸러진다.
func (s *ClickHouseSink) Write(ctx context.Context, b *Batch) error {
	if len(b.Spans) > 0 {
		if err := s.insert(ctx, b.Token("spans_local"), `INSERT INTO spans_local (tenant_id, service_id, trace_id, span_id,
			parent_span_id, name, event_time, duration_ns, received_at, status, span_kind, attributes, payload,
			payload_hash, version, expires_at)`, func(batch driver.Batch) error {
			for i, r := range b.Spans {
				if err := batch.Append(r.Tenant.String(), r.ServiceID, string(r.TraceID[:]), string(r.SpanID[:]),
					string(r.ParentSpanID[:]), r.Name, r.EventTime, r.DurationNs, r.ReceivedAt, r.Status, r.Kind,
					r.Attributes, string(r.Payload), string(r.PayloadHash[:]), r.Version, r.ExpiresAt); err != nil {
					return rowErr("spans_local", i)
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("pipeline: insert spans: %w", err)
		}
	}
	if len(b.Logs) > 0 {
		if err := s.insert(ctx, b.Token("logs_local"), `INSERT INTO logs_local (tenant_id, service_id, event_id,
			event_time, severity, trace_id, span_id, body, attributes, version, expires_at)`, func(batch driver.Batch) error {
			for i, r := range b.Logs {
				if err := batch.Append(r.Tenant.String(), r.ServiceID, r.EventID, r.EventTime, r.Severity,
					string(r.TraceID[:]), string(r.SpanID[:]), r.Body, r.Attributes, r.Version, r.ExpiresAt); err != nil {
					return rowErr("logs_local", i)
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("pipeline: insert logs: %w", err)
		}
	}
	if len(b.Metrics) > 0 {
		if err := s.insert(ctx, b.Token("metric_points"), `INSERT INTO metric_points (tenant_id, stream_id, metric_name,
			unit, type, temporality, is_monotonic, start_time, end_time, point_hash, value, count, sum, bounds, buckets,
			payload, resource_json, attributes_json, version, expires_at)`, func(batch driver.Batch) error {
			for i, r := range b.Metrics {
				bounds, buckets := r.Bounds, r.Buckets
				if bounds == nil {
					bounds = []float64{}
				}
				if buckets == nil {
					buckets = []uint64{}
				}
				if err := batch.Append(r.Tenant.String(), string(r.StreamID[:]), r.Name, r.Unit, r.Type, r.Temporality,
					r.IsMonotonic, r.StartTime, r.EndTime, string(r.PointHash[:]), r.Value, r.Count, r.Sum, bounds, buckets,
					r.Payload, r.ResourceJSON, r.AttributesJSON, r.Version, r.ExpiresAt); err != nil {
					return rowErr("metric_points", i)
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("pipeline: insert metrics: %w", err)
		}
	}
	if len(b.Quarantine) > 0 {
		if err := s.insert(ctx, b.Token("ingest_quarantine"), `INSERT INTO ingest_quarantine (tenant_id, signal, reason,
			topic, kafka_partition, kafka_offset, event_id, schema_version, payload_bytes, payload_sha256, quarantined_at,
			expires_at)`, func(batch driver.Batch) error {
			for i, r := range b.Quarantine {
				if err := batch.Append(r.Tenant.String(), r.Signal, r.Reason, r.Topic, r.Partition, r.Offset, r.EventID,
					r.SchemaVersion, r.PayloadBytes, string(r.PayloadSHA256[:]), r.QuarantinedAt, r.ExpiresAt); err != nil {
					return rowErr("ingest_quarantine", i)
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("pipeline: insert quarantine: %w", err)
		}
	}
	return nil
}

func (s *ClickHouseSink) insert(ctx context.Context, token, query string, fill func(driver.Batch) error) error {
	settings := clickhouse.Settings{
		"insert_deduplicate":         1,
		"insert_deduplication_token": token,
		"async_insert":               0, // 동기 insert: 반환 시 part가 기록되어 있다
	}
	if s.insertQuorum > 0 {
		settings["insert_quorum"] = s.insertQuorum
	}
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(settings))
	batch, err := s.conn.PrepareBatch(ctx, query)
	if err != nil {
		return err
	}
	if err := fill(batch); err != nil {
		_ = batch.Abort()
		return err
	}
	return batch.Send()
}
