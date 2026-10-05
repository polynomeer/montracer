package pipeline

import (
	"crypto/sha256"
	"fmt"
	"sort"
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
	Conflicts  int // 같은 span key인데 payload가 다른 수 (최초 수신 값 유지)
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
	seenByPart := map[part]map[dedupKey]seenEntry{}
	for _, m := range msgs {
		p := part{m.Topic, m.Partition}
		b, ok := byPart[p]
		if !ok {
			b = &Batch{Topic: m.Topic, Partition: m.Partition, FirstOffset: m.Offset}
			byPart[p], seenByPart[p] = b, map[dedupKey]seenEntry{}
			order = append(order, p)
		}
		b.LastOffset = m.Offset

		md, err := parseMeta(m)
		if err == nil {
			err = bl.add(b, md, m.Value, seenByPart[p])
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
func (bl Builder) add(b *Batch, md meta, value []byte, seen map[dedupKey]seenEntry) error {
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
		seen[key] = seenEntry{idx: len(b.Metrics), version: row.Version}
		b.Metrics = append(b.Metrics, row)
	}
	return nil
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
