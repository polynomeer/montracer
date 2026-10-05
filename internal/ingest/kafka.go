package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/polynomeer/montracer/internal/telemetry/envelope"
)

// KafkaProducer는 acks=all·idempotent producer다 (ADR 0002).
// broker 쪽 replication factor 3·min.insync.replicas=2는 topic 설정이 보장한다(cmd/migrate kafka).
type KafkaProducer struct {
	cl *kgo.Client
}

// NewKafkaProducer는 producer를 만든다. topic 자동 생성은 쓰지 않는다.
func NewKafkaProducer(brokers []string, opts ...kgo.Opt) (*KafkaProducer, error) {
	if len(brokers) == 0 {
		return nil, errors.New("ingest: kafka brokers are required")
	}
	base := []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.RequiredAcks(kgo.AllISRAcks()), // acks=all; idempotence는 기본 활성
		kgo.ProducerBatchMaxBytes(envelope.MaxMessageBytes + 64<<10),
		kgo.ProducerBatchCompression(kgo.ZstdCompression(), kgo.Lz4Compression(), kgo.NoCompression()),
	}
	cl, err := kgo.NewClient(append(base, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("ingest: kafka client: %w", err)
	}
	return &KafkaProducer{cl: cl}, nil
}

// ProduceSync는 모든 record의 append 확인을 기다린다. 하나라도 실패하면 오류다.
func (p *KafkaProducer) ProduceSync(ctx context.Context, records []envelope.Record) error {
	recs := make([]*kgo.Record, len(records))
	for i, r := range records {
		hs := make([]kgo.RecordHeader, len(r.Headers))
		for j, h := range r.Headers {
			hs[j] = kgo.RecordHeader{Key: h.Key, Value: h.Value}
		}
		recs[i] = &kgo.Record{Topic: r.Topic, Key: r.Key, Value: r.Value, Headers: hs}
	}
	if err := p.cl.ProduceSync(ctx, recs...).FirstErr(); err != nil {
		return fmt.Errorf("ingest: kafka produce: %w", err)
	}
	return nil
}

// Ping은 broker 연결과 수집 topic 존재를 확인한다 (readiness).
// topic이 없으면 모든 요청이 timeout 뒤 503이 되므로 ready로 보고하지 않는다.
func (p *KafkaProducer) Ping(ctx context.Context) error {
	if err := p.cl.Ping(ctx); err != nil {
		return err
	}
	topics := []string{envelope.TopicTraces, envelope.TopicLogs, envelope.TopicMetrics}
	td, err := kadm.NewClient(p.cl).ListTopics(ctx, topics...)
	if err != nil {
		return fmt.Errorf("ingest: list topics: %w", err)
	}
	for _, t := range topics {
		if d, ok := td[t]; !ok || d.Err != nil {
			return fmt.Errorf("ingest: topic %s is not available", t)
		}
	}
	return nil
}

// Close는 남은 produce를 정리하고 연결을 닫는다.
func (p *KafkaProducer) Close() { p.cl.Close() }
