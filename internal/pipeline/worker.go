package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/polynomeer/montracer/internal/telemetry/envelope"
)

// GroupID는 수집 worker의 consumer group이다.
const GroupID = "montracer-worker-raw-v1"

// Config는 Worker 설정이다.
type Config struct {
	Brokers []string
	Group   string // 비우면 GroupID
	Sink    Sink
	Builder Builder
	Logger  *slog.Logger

	// MaxPollRecords는 poll 한 번의 최대 record 수다 (기본 5,000).
	MaxPollRecords int
	// RetryBudget은 sink 실패 시 같은 batch를 다시 시도하는 총 시간이다 (기본 60초).
	// 넘으면 offset을 commit하지 않고 Run이 오류로 끝난다. 재시작하면 마지막 commit 이후부터 다시 읽는다.
	RetryBudget time.Duration

	// Observer가 nil이면 지표를 내보내지 않는다.
	Observer Observer

	// afterWrite는 시험용 hook이다. insert 성공 직후·offset commit 전에 불리고, 오류면 commit 없이 멈춘다(crash 모사).
	afterWrite func() error
}

// BatchResult는 저장을 마친 batch 하나의 결과다. 값 내용·tenant를 담지 않는다 (D04 §10).
type BatchResult struct {
	Signal     string // traces | logs | metrics (알 수 없는 topic이면 unknown)
	Records    int    // 소비한 Kafka record 수 (= stored + duplicates + quarantined)
	Stored     int
	Duplicates int
	Conflicts  int
	// Quarantined는 사유별 quarantine 수다. 사유는 고정 enum이다.
	Quarantined map[string]int
	// InsertDuration은 재시도를 포함한 저장 시간이다.
	InsertDuration time.Duration
	// OldestAge는 batch에서 가장 오래 기다린 record의 ingress 수신 후 경과 시간이다. 알 수 없으면 0.
	OldestAge time.Duration
}

// Observer는 worker 결과를 운영 지표로 내보낸다. 구현은 internal/opsmetrics에 있다.
type Observer interface {
	ObserveBatch(BatchResult)
	// ObserveSinkError는 저장 실패 한 번이다. kind는 transient, row_rejected, unrecoverable.
	ObserveSinkError(kind string)
	// ObserveCommit은 offset commit 결과다. 성공한 commit의 batch만 ObserveBatch로 들어온다.
	ObserveCommit(ok bool)
}

// Worker는 Kafka 원본 topic을 읽어 sink에 쓰고, durable insert 뒤에만 offset을 commit한다 (D02 §05, §22).
type Worker struct {
	cfg Config
	cl  *kgo.Client
}

// NewWorker는 consumer group client를 만든다.
func NewWorker(cfg Config, opts ...kgo.Opt) (*Worker, error) {
	if len(cfg.Brokers) == 0 || cfg.Brokers[0] == "" {
		return nil, errors.New("pipeline: kafka brokers are required")
	}
	if cfg.Sink == nil {
		return nil, errors.New("pipeline: sink is required")
	}
	if cfg.Group == "" {
		cfg.Group = GroupID
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxPollRecords <= 0 {
		cfg.MaxPollRecords = 5000
	}
	if cfg.RetryBudget <= 0 {
		cfg.RetryBudget = 60 * time.Second
	}
	if cfg.Builder.Retention == (Retention{}) {
		cfg.Builder.Retention = DefaultRetention
	}
	base := []kgo.Opt{
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ConsumerGroup(cfg.Group),
		kgo.ConsumeTopics(envelope.TopicTraces, envelope.TopicLogs, envelope.TopicMetrics),
		// 처음 시작하는 group은 가장 오래된 offset부터 읽는다. 최신부터 읽으면 기동 전 append된 record를 잃는다.
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.DisableAutoCommit(),
		// poll~commit 사이에는 rebalance를 막는다. 처리 중인 partition이 다른 worker로 넘어가 같은 범위를
		// 동시에 쓰는 일을 줄인다(그래도 중복은 at-least-once 범위 안이다).
		kgo.BlockRebalanceOnPoll(),
		// rebalance 대기 상한은 sink 재시도 예산보다 커야 한다.
		kgo.RebalanceTimeout(cfg.RetryBudget + 30*time.Second),
		kgo.FetchMaxBytes(32 << 20),
	}
	cl, err := kgo.NewClient(append(base, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("pipeline: kafka client: %w", err)
	}
	return &Worker{cfg: cfg, cl: cl}, nil
}

// Ping은 broker 연결을 확인한다.
func (w *Worker) Ping(ctx context.Context) error { return w.cl.Ping(ctx) }

// Close는 group에서 나가고 연결을 닫는다. commit하지 않은 범위는 다음 소유자가 다시 읽는다.
func (w *Worker) Close() { w.cl.Close() }

// Run은 ctx가 끝나거나 처리할 수 없는 오류가 날 때까지 소비한다. ctx 종료는 nil을 반환한다.
func (w *Worker) Run(ctx context.Context) error {
	log := w.cfg.Logger
	for {
		fetches := w.cl.PollRecords(ctx, w.cfg.MaxPollRecords)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			w.cl.AllowRebalance()
			return nil
		}
		fetches.EachError(func(topic string, partition int32, err error) {
			// fetch 오류는 client가 재시도한다. 원인만 남긴다.
			log.Warn("kafka fetch error", slog.String("topic", topic), slog.Int("partition", int(partition)), slog.String("error", err.Error()))
		})
		records := fetches.Records()
		if len(records) == 0 {
			w.cl.AllowRebalance()
			continue
		}
		stored, err := w.process(ctx, records)
		if err != nil {
			w.cl.AllowRebalance()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		// 모든 batch가 durable하게 저장된 뒤에만 commit한다. commit 실패면 다음 소유자가 같은 범위를 다시 쓰고
		// insert token·ReplacingMergeTree·query dedup이 중복을 흡수한다.
		err = w.cl.CommitRecords(ctx, records...)
		if w.cfg.Observer != nil && ctx.Err() == nil {
			w.cfg.Observer.ObserveCommit(err == nil)
		}
		if err == nil {
			// 지표는 commit이 확정된 batch만 센다. commit 전에 세면 crash·commit 실패 뒤 재처리한 범위가
			// 두 번 세져 처리량이 입력보다 크게 보이고, 처리 정체 경보(FallingBehind)가 가려진다.
			for _, s := range stored {
				w.observe(s.batch, s.duration)
			}
		}
		if err != nil {
			w.cl.AllowRebalance()
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("pipeline: commit offsets: %w", err)
		}
		w.cl.AllowRebalance()
	}
}

func toMessages(records []*kgo.Record) []Message {
	msgs := make([]Message, len(records))
	for i, r := range records {
		hs := make([]envelope.Header, len(r.Headers))
		for j, h := range r.Headers {
			hs[j] = envelope.Header{Key: h.Key, Value: h.Value}
		}
		msgs[i] = Message{Topic: r.Topic, Partition: r.Partition, Offset: r.Offset, Key: r.Key, Value: r.Value, Headers: hs}
	}
	return msgs
}

// storedBatch는 저장을 마쳤지만 아직 commit되지 않은 batch다.
type storedBatch struct {
	batch    *Batch
	duration time.Duration
}

func (w *Worker) process(ctx context.Context, records []*kgo.Record) ([]storedBatch, error) {
	batches := w.cfg.Builder.Build(toMessages(records))
	stored := make([]storedBatch, 0, len(batches))
	for _, b := range batches {
		start := time.Now()
		if err := w.write(ctx, b); err != nil {
			return nil, err
		}
		stored = append(stored, storedBatch{b, time.Since(start)})
		attrs := []any{
			slog.String("topic", b.Topic), slog.Int("partition", int(b.Partition)),
			slog.Int64("first_offset", b.FirstOffset), slog.Int64("last_offset", b.LastOffset),
			slog.Int("spans", len(b.Spans)), slog.Int("logs", len(b.Logs)), slog.Int("metrics", len(b.Metrics)),
			slog.Int("duplicates", b.Duplicates), slog.Int("conflicts", b.Conflicts), slog.Int("quarantined", len(b.Quarantine)),
			slog.Int("rejected", b.Rejected),
		}
		if len(b.Quarantine) > 0 || b.Conflicts > 0 {
			w.cfg.Logger.Warn("batch stored with quarantine or conflicts", attrs...)
		} else {
			w.cfg.Logger.Debug("batch stored", attrs...)
		}
	}
	if w.cfg.afterWrite != nil {
		if err := w.cfg.afterWrite(); err != nil {
			return nil, err
		}
	}
	return stored, nil
}

func (w *Worker) observe(b *Batch, insert time.Duration) {
	if w.cfg.Observer == nil {
		return
	}
	sig := string(topicSignal[b.Topic])
	if sig == "" {
		sig = "unknown"
	}
	q := map[string]int{}
	for _, r := range b.Quarantine {
		q[r.Reason]++
	}
	var age time.Duration
	if !b.OldestReceivedAt.IsZero() {
		age = max(time.Since(b.OldestReceivedAt), 0)
	}
	w.cfg.Observer.ObserveBatch(BatchResult{
		Signal: sig, Records: b.Records, Stored: len(b.Spans) + len(b.Logs) + len(b.Metrics),
		Duplicates: b.Duplicates, Conflicts: b.Conflicts, Quarantined: q, InsertDuration: insert, OldestAge: age,
	})
}

// errSummary는 로그용 오류 요약이다. ClickHouse 예외 문구에는 입력 값 일부가 실릴 수 있어 코드만 남긴다.
func errSummary(err error) string {
	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		return fmt.Sprintf("clickhouse exception code %d", ex.Code)
	}
	return err.Error()
}

// write는 batch를 재시도 예산 안에서 저장한다. 같은 token으로 다시 쓰므로 재시도가 중복을 만들지 않는다.
func (w *Worker) write(ctx context.Context, b *Batch) error {
	if b.Empty() {
		return nil
	}
	deadline := time.Now().Add(w.cfg.RetryBudget)
	backoff := 200 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := w.cfg.Sink.Write(ctx, b)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 행 하나가 결정적으로 거부되면 그 행만 quarantine(DLQ)으로 돌리고 즉시 다시 쓴다.
		// partition 전체를 멈추지 않는다 (Kafka Connect errors.tolerance + DLQ, Uber DLQ와 같은 방식).
		var rowErr *RowError
		isRowErr := errors.As(err, &rowErr)
		if isRowErr && b.Reject(rowErr.Table, rowErr.Index, time.Now()) {
			if w.cfg.Observer != nil {
				w.cfg.Observer.ObserveSinkError("row_rejected")
			}
			w.cfg.Logger.Warn("sink rejected row, quarantined", slog.String("topic", b.Topic),
				slog.Int("partition", int(b.Partition)), slog.String("table", rowErr.Table))
			continue
		}
		if w.cfg.Observer != nil {
			kind := "transient"
			if isRowErr {
				kind = "unrecoverable" // quarantine 행 자체가 거부됨: 돌릴 곳이 없어 partition이 멈춘다
			}
			w.cfg.Observer.ObserveSinkError(kind)
		}
		if time.Now().Add(backoff).After(deadline) {
			return fmt.Errorf("pipeline: sink failed for %s/%d after %d attempts: %s", b.Topic, b.Partition, attempt, errSummary(err))
		}
		w.cfg.Logger.Warn("sink write failed, retrying", slog.String("topic", b.Topic), slog.Int("partition", int(b.Partition)),
			slog.Int("attempt", attempt), slog.String("error", errSummary(err)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}
