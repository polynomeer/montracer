package pipeline

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetry/envelope"
)

// QuarantineRow는 ingest_quarantine 한 행이다. payload 원문은 담지 않는다.
type QuarantineRow struct {
	Tenant        authz.TenantID // 해석 실패 시 zero
	Signal        string
	Reason        string
	Topic         string
	Partition     int32
	Offset        int64
	EventID       string
	SchemaVersion string
	PayloadBytes  uint32
	PayloadSHA256 [32]byte
	QuarantinedAt time.Time
	ExpiresAt     time.Time
}

// QuarantineRetention은 quarantine 보존 기간이다 (D02 §05, D04 §04).
const QuarantineRetention = 24 * time.Hour

// Batch는 한 Kafka partition의 연속 offset 범위에서 만든 insert 단위다.
// 내용은 (topic, partition, offset 범위)만으로 결정되므로 crash 후 같은 범위를 다시 읽으면 같은 행이 나온다.
// 그래서 범위를 insert_deduplication_token으로 쓸 수 있다 (D02 §05 "batch token과 record key를 모두 사용").
type Batch struct {
	Topic       string
	Partition   int32
	FirstOffset int64
	LastOffset  int64

	Spans      []SpanRow
	Logs       []LogRow
	Metrics    []MetricRow
	Quarantine []QuarantineRow

	Duplicates int // 같은 (tenant, event_id)가 batch 안에서 다시 나온 수
	// Conflicts는 같은 identity에 다른 값이 온 수다 (D02 §05, §21).
	//   span  : 같은 (tenant, trace_id, span_id)인데 payload가 다름 → 최초 수신 값 유지
	//   metric: 같은 (tenant, stream, start, end)인데 point hash가 다름 → 최초 수신 값을 남기고 나머지는
	//           conflicting_point_value로 quarantine (redaction이 series를 합친 경우 포함, ADR 0019 §5)
	Conflicts int
	Rejected  int // sink가 거부해 quarantine으로 돌린 행 수
}

// Token은 insert dedup token이다. 테이블마다 따로 비교되므로 접미어로 대상을 구분한다.
func (b *Batch) Token(table string) string {
	return fmt.Sprintf("%s/%d/%d-%d/%s", b.Topic, b.Partition, b.FirstOffset, b.LastOffset, table)
}

// Empty는 insert할 행이 없는지 알려준다.
func (b *Batch) Empty() bool {
	return len(b.Spans)+len(b.Logs)+len(b.Metrics)+len(b.Quarantine) == 0
}

// dedupKey는 (tenant, event_id)다. event_id만으로는 dedup하지 않는다 — client가 정하는 ID라
// tenant 없이 비교하면 다른 tenant의 record가 서로를 지운다 (ADR 0020 §4).
type dedupKey struct {
	tenant  authz.TenantID
	eventID string
}

// pointKey는 metric point의 시간 identity다. point hash(값)는 포함하지 않는다.
type pointKey struct {
	tenant     authz.TenantID
	stream     [16]byte
	start, end time.Time
}

// partState는 partition 하나의 batch 안 dedup 상태다.
type partState struct {
	seen   map[dedupKey]seenEntry
	points map[pointKey]pointEntry
}

// pointEntry는 시간 identity별로 남긴 metric 행이다.
type pointEntry struct {
	idx     int
	version uint64
	hash    [16]byte
}

// seenEntry는 batch 안에서 이미 본 key의 행 위치와 version·payload hash다.
type seenEntry struct {
	idx     int
	version uint64
	hash    [32]byte
}

// Builder는 Message를 partition별 Batch로 바꾼다.
type Builder struct {
	Retention Retention
	Now       func() time.Time
}

// Build는 poll 한 번의 message를 partition별 Batch로 나눈다.
// 같은 partition 안의 message는 offset 순서여야 한다(Kafka 소비 순서).
func (bl Builder) Build(msgs []Message) []*Batch {
	now := time.Now
	if bl.Now != nil {
		now = bl.Now
	}
	type part struct {
		topic     string
		partition int32
	}
	byPart := map[part]*Batch{}
	var order []part
	stateByPart := map[part]*partState{}
	for _, m := range msgs {
		p := part{m.Topic, m.Partition}
		b, ok := byPart[p]
		if !ok {
			b = &Batch{Topic: m.Topic, Partition: m.Partition, FirstOffset: m.Offset}
			byPart[p] = b
			stateByPart[p] = &partState{seen: map[dedupKey]seenEntry{}, points: map[pointKey]pointEntry{}}
			order = append(order, p)
		}
		b.LastOffset = m.Offset

		md, err := parseMeta(m)
		if err == nil {
			md.offset = m.Offset
			err = bl.add(b, md, m.Value, stateByPart[p], now())
		}
		if reason := ReasonOf(err); reason != "" {
			b.Quarantine = append(b.Quarantine, quarantineRow(m, md, reason, now()))
		}
	}
	out := make([]*Batch, 0, len(order))
	sort.Slice(order, func(i, j int) bool {
		if order[i].topic != order[j].topic {
			return order[i].topic < order[j].topic
		}
		return order[i].partition < order[j].partition
	})
	for _, p := range order {
		out = append(out, byPart[p])
	}
	return out
}

// add는 정규화한 행을 batch에 넣는다. 같은 key가 이미 있으면 먼저 수신한 쪽(큰 version)을 남긴다.
func (bl Builder) add(b *Batch, md meta, value []byte, st *partState, now time.Time) error {
	seen := st.seen
	key := dedupKey{md.tenant, md.eventID}
	prev, dup := seen[key]
	switch md.signal {
	case SignalTraces:
		row, err := normalizeSpan(md, value, bl.Retention)
		if err != nil {
			return err
		}
		if dup {
			b.Duplicates++
			if prev.hash != row.PayloadHash {
				b.Conflicts++
			}
			if row.Version > prev.version {
				b.Spans[prev.idx] = row
				seen[key] = seenEntry{prev.idx, row.Version, row.PayloadHash}
			}
			return nil
		}
		seen[key] = seenEntry{len(b.Spans), row.Version, row.PayloadHash}
		b.Spans = append(b.Spans, row)
	case SignalLogs:
		row, err := normalizeLog(md, value, bl.Retention)
		if err != nil {
			return err
		}
		if dup {
			b.Duplicates++
			if row.Version > prev.version {
				b.Logs[prev.idx] = row
				prev.version = row.Version
				seen[key] = prev
			}
			return nil
		}
		seen[key] = seenEntry{idx: len(b.Logs), version: row.Version}
		b.Logs = append(b.Logs, row)
	case SignalMetrics:
		row, err := normalizeMetric(md, value, bl.Retention)
		if err != nil {
			return err
		}
		if dup {
			b.Duplicates++ // event_id에 point hash가 들어 있어 같은 key면 같은 값이다
			if row.Version > prev.version {
				b.Metrics[prev.idx] = row
				prev.version = row.Version
				seen[key] = prev
			}
			return nil
		}
		// 같은 stream·같은 시각의 상충 값 (D02 §05 "격리·경고"): 먼저 수신한 값만 남긴다.
		pk := pointKey{md.tenant, row.StreamID, row.StartTime, row.EndTime}
		if pe, ok := st.points[pk]; ok && pe.hash != row.PointHash {
			b.Conflicts++
			if row.Version <= pe.version {
				b.Quarantine = append(b.Quarantine, b.quarantineOf(md.tenant, SignalMetrics, ReasonConflictingPoint, row.Src, now))
				return nil
			}
			old := b.Metrics[pe.idx]
			b.Quarantine = append(b.Quarantine, b.quarantineOf(old.Tenant, SignalMetrics, ReasonConflictingPoint, old.Src, now))
			delete(seen, dedupKey{old.Tenant, old.Src.EventID})
			b.Metrics[pe.idx] = row
			st.points[pk] = pointEntry{pe.idx, row.Version, row.PointHash}
			seen[key] = seenEntry{idx: pe.idx, version: row.Version}
			return nil
		}
		st.points[pk] = pointEntry{len(b.Metrics), row.Version, row.PointHash}
		seen[key] = seenEntry{idx: len(b.Metrics), version: row.Version}
		b.Metrics = append(b.Metrics, row)
	}
	return nil
}

// quarantineOf는 정규화까지 마친 행을 quarantine 행으로 바꾼다. 원문은 담지 않는다.
func (b *Batch) quarantineOf(tenant authz.TenantID, sig Signal, reason string, src Source, now time.Time) QuarantineRow {
	return QuarantineRow{
		Tenant:        tenant,
		Signal:        string(sig),
		Reason:        reason,
		Topic:         b.Topic,
		Partition:     b.Partition,
		Offset:        src.Offset,
		EventID:       src.EventID,
		SchemaVersion: strconv.Itoa(envelope.SchemaVersion),
		PayloadBytes:  src.PayloadBytes,
		PayloadSHA256: src.PayloadSHA256,
		QuarantinedAt: now.UTC(),
		ExpiresAt:     now.UTC().Add(QuarantineRetention).Truncate(time.Second),
	}
}

// RowError는 sink가 특정 행을 결정적으로 거부했다는 뜻이다(드라이버 변환 오류 등).
// 같은 행으로 다시 시도해도 실패하므로 worker는 그 행만 quarantine으로 돌리고 나머지를 저장한다.
type RowError struct {
	Table string
	Index int
}

func (e *RowError) Error() string {
	return fmt.Sprintf("pipeline: %s row %d rejected by sink", e.Table, e.Index)
}

// Reject는 table의 idx번째 행을 빼고 sink_rejected로 quarantine한다.
// quarantine 행 자체가 거부되면 돌릴 곳이 없으므로 false다(그때는 partition을 멈춘다).
func (b *Batch) Reject(table string, idx int, now time.Time) bool {
	switch {
	case table == "spans_local" && idx >= 0 && idx < len(b.Spans):
		r := b.Spans[idx]
		b.Spans = append(b.Spans[:idx], b.Spans[idx+1:]...)
		b.Quarantine = append(b.Quarantine, b.quarantineOf(r.Tenant, SignalTraces, ReasonSinkRejected, r.Src, now))
	case table == "logs_local" && idx >= 0 && idx < len(b.Logs):
		r := b.Logs[idx]
		b.Logs = append(b.Logs[:idx], b.Logs[idx+1:]...)
		b.Quarantine = append(b.Quarantine, b.quarantineOf(r.Tenant, SignalLogs, ReasonSinkRejected, r.Src, now))
	case table == "metric_points" && idx >= 0 && idx < len(b.Metrics):
		r := b.Metrics[idx]
		b.Metrics = append(b.Metrics[:idx], b.Metrics[idx+1:]...)
		b.Quarantine = append(b.Quarantine, b.quarantineOf(r.Tenant, SignalMetrics, ReasonSinkRejected, r.Src, now))
	default:
		return false
	}
	b.Rejected++
	return true
}

func quarantineRow(m Message, md meta, reason string, now time.Time) QuarantineRow {
	sig := string(topicSignal[m.Topic])
	schema, _ := header(m.Headers, envelope.HeaderSchema)
	if len(schema) > 16 {
		schema = schema[:16]
	}
	eventID := md.eventID // header 검증을 통과했을 때만 채워진다
	return QuarantineRow{
		Tenant:        md.tenant,
		Signal:        sig,
		Reason:        reason,
		Topic:         m.Topic,
		Partition:     m.Partition,
		Offset:        m.Offset,
		EventID:       eventID,
		SchemaVersion: string(schema),
		PayloadBytes:  uint32(min(len(m.Value), 1<<31)), //nolint:gosec // 상한으로 자름
		PayloadSHA256: sha256.Sum256(m.Value),
		QuarantinedAt: now.UTC(),
		ExpiresAt:     now.UTC().Add(QuarantineRetention).Truncate(time.Second),
	}
}
