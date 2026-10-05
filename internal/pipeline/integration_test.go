//go:build integration

package pipeline

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/ingest"
	"github.com/polynomeer/montracer/internal/telemetry/envelope"
)

// 통합 테스트: 실제 Kafka(topic은 cmd/migrate kafka up)와 ClickHouse(migrate clickhouse up).
//
//	MONTRACER_TEST_KAFKA_BROKERS, MONTRACER_TEST_CH_INGEST_DSN, MONTRACER_TEST_CH_ADMIN_DSN

func env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("%s 필요 (make test-integration)", k)
	}
	return v
}

func randomTenant(t *testing.T) authz.TenantID {
	t.Helper()
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6], b[8] = (b[6]&0x0f)|0x40, (b[8]&0x3f)|0x80
	id, err := authz.ParseTenantID(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func adminConn(t *testing.T) driver.Conn {
	t.Helper()
	opts, err := clickhouse.ParseDSN(env(t, "MONTRACER_TEST_CH_ADMIN_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func count(t *testing.T, conn driver.Conn, q string, args ...any) uint64 {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

var topics = []string{envelope.TopicTraces, envelope.TopicLogs, envelope.TopicMetrics}

// startAtEnd는 새 group의 offset을 현재 끝으로 commit한다. 다른 테스트가 남긴 record를 읽지 않기 위해서다.
func startAtEnd(t *testing.T, adm *kadm.Client, group string) {
	t.Helper()
	ends, err := adm.ListEndOffsets(context.Background(), topics...)
	if err != nil {
		t.Fatal(err)
	}
	if err := adm.CommitAllOffsets(context.Background(), group, ends.Offsets()); err != nil {
		t.Fatal(err)
	}
}

// waitCommitted는 group의 commit offset이 모든 partition에서 끝 offset에 닿을 때까지 기다린다.
func waitCommitted(t *testing.T, adm *kadm.Client, group string, timeout time.Duration) {
	t.Helper()
	ctx := context.Background()
	ends, err := adm.ListEndOffsets(ctx, topics...)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(timeout)
	for {
		committed, err := adm.FetchOffsets(ctx, group)
		if err == nil {
			done := true
			ends.Each(func(o kadm.ListedOffset) {
				c, ok := committed.Lookup(o.Topic, o.Partition)
				if !ok || c.At < o.Offset {
					done = false
				}
			})
			if done {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("offset이 %s 안에 끝까지 commit되지 않았다", timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func committedSnapshot(t *testing.T, adm *kadm.Client, group string) map[string]int64 {
	t.Helper()
	res, err := adm.FetchOffsets(context.Background(), group)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	res.Each(func(o kadm.OffsetResponse) { out[fmt.Sprintf("%s/%d", o.Topic, o.Partition)] = o.At })
	return out
}

var errCrash = errors.New("simulated crash after insert, before commit")

// insert 성공 → commit 전 crash → 재시작. offset은 crash 전 그대로이고, 재처리 결과 논리 중복이 없다 (D02 §05, D06 Q2).
func TestWorkerCrashBetweenInsertAndCommit(t *testing.T) {
	ctx := context.Background()
	brokers := strings.Split(env(t, "MONTRACER_TEST_KAFKA_BROKERS"), ",")
	sink, err := OpenClickHouseSink(ctx, env(t, "MONTRACER_TEST_CH_INGEST_DSN"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	ch := adminConn(t)
	kcl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kcl.Close)
	adm := kadm.NewClient(kcl)
	group := fmt.Sprintf("it-worker-%d", time.Now().UnixNano())
	startAtEnd(t, adm, group)
	before := committedSnapshot(t, adm, group)

	// 입력: tenant A span 20개(그중 5개는 재전송), tenant B가 A와 같은 trace·span ID를 쓴 span 1개,
	// metric 5종, log 1개, header가 위조된 record 1개.
	tA, tB := randomTenant(t), randomTenant(t)
	received := time.Now().UTC().Truncate(time.Millisecond)
	producer, err := ingest.NewKafkaProducer(brokers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(producer.Close)
	var recs []envelope.Record
	var traceID pcommon.TraceID
	_, _ = rand.Read(traceID[:])
	for i := range 20 {
		td := span(traceID, pcommon.SpanID{byte(i + 1)}, fmt.Sprintf("op-%d", i))
		r, err := envelope.Traces(td, envelope.Meta{Tenant: tA, ReceivedAt: received, PolicyVersion: 1, RoutingEpoch: 1})
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r.Records...)
	}
	for i := range 5 { // client 재전송: 나중 수신, 같은 event_id
		td := span(traceID, pcommon.SpanID{byte(i + 1)}, "resent")
		r, _ := envelope.Traces(td, envelope.Meta{Tenant: tA, ReceivedAt: received.Add(time.Second), PolicyVersion: 1, RoutingEpoch: 1})
		recs = append(recs, r.Records...)
	}
	rb, _ := envelope.Traces(span(traceID, pcommon.SpanID{1}, "tenant-b"), envelope.Meta{Tenant: tB, ReceivedAt: received, PolicyVersion: 1, RoutingEpoch: 1})
	recs = append(recs, rb.Records...)
	rm, err := envelope.Metrics(allMetricTypes(), envelope.Meta{Tenant: tA, ReceivedAt: received, PolicyVersion: 1, RoutingEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	recs = append(recs, rm.Records...)
	rl, _ := envelope.Logs(logWithUID("uid-it-1"), envelope.Meta{Tenant: tA, ReceivedAt: received, PolicyVersion: 1, RoutingEpoch: 1}, nil)
	recs = append(recs, rl.Records...)
	if err := producer.ProduceSync(ctx, recs); err != nil {
		t.Fatal(err)
	}
	// 위조: key는 tenant A, header는 tenant B → 저장하지 않고 quarantine
	forged := rb.Records[0]
	forged.Key = append([]byte(nil), recs[0].Key...)
	if err := producer.ProduceSync(ctx, []envelope.Record{forged}); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// 1회차: insert 성공 직후 crash
	obs1, obs2 := &recordingObserver{}, &recordingObserver{}
	w1, err := NewWorker(Config{Brokers: brokers, Group: group, Sink: sink, Logger: logger, Observer: obs1, afterWrite: func() error { return errCrash }})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	err = w1.Run(runCtx)
	cancel()
	w1.Close()
	if !errors.Is(err, errCrash) {
		t.Fatalf("1회차 Run = %v", err)
	}
	if after := committedSnapshot(t, adm, group); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("crash 전 offset이 commit됐다: %v → %v", before, after)
	}
	if n := count(t, ch, `SELECT (SELECT count() FROM spans_local WHERE tenant_id IN ($1, $2))
		+ (SELECT count() FROM metric_points WHERE tenant_id = $1) + (SELECT count() FROM logs_local WHERE tenant_id = $1)
`, tA.String(), tB.String()); n == 0 {
		t.Fatal("1회차 insert가 없다 — 시험 전제(insert 후 crash)가 성립하지 않는다")
	}

	// 2회차: 정상 처리
	// commit하지 못한 batch는 지표에 세지 않는다(재처리 이중 계수 방지, ADR 0023)
	if len(obs1.batches) != 0 {
		t.Errorf("crashed run reported %d batches before commit", len(obs1.batches))
	}
	w2, err := NewWorker(Config{Brokers: brokers, Group: group, Sink: sink, Logger: logger, Observer: obs2})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel = context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w2.Run(runCtx) }()
	waitCommitted(t, adm, group, 60*time.Second)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("2회차 Run = %v", err)
	}
	w2.Close()
	// stage 회계: commit된 소비 record는 이번 테스트가 쓴 record를 모두 포함한다(ingress accepted와 같은 단위).
	// 다른 package의 통합 테스트가 같은 topic에 동시에 쓰므로 하한만 본다. 정확한 등식은 단위 테스트가 고정한다.
	consumed := 0
	for _, b := range obs2.batches {
		consumed += b.Records
	}
	if consumed < len(recs)+1 {
		t.Errorf("observed records = %d, want at least %d (produced incl. forged)", consumed, len(recs)+1)
	}

	// 논리 결과: span key별 한 행(FINAL은 검증용으로만 쓴다), 남은 값은 최초 수신 값
	if n := count(t, ch, `SELECT count() FROM spans_local FINAL WHERE tenant_id = $1`, tA.String()); n != 20 {
		t.Errorf("tenant A spans = %d, want 20", n)
	}
	if n := count(t, ch, `SELECT count() FROM spans_local FINAL WHERE tenant_id = $1 AND name = 'resent'`, tA.String()); n != 0 {
		t.Errorf("재전송 값이 최초 값을 덮었다 (%d행)", n)
	}
	// 계약: 같은 trace·span ID라도 tenant B 행은 따로 남는다
	if n := count(t, ch, `SELECT count() FROM spans_local FINAL WHERE tenant_id = $1 AND name = 'tenant-b'`, tB.String()); n != 1 {
		t.Errorf("tenant B span = %d, want 1", n)
	}
	if n := count(t, ch, `SELECT count() FROM metric_points FINAL WHERE tenant_id = $1`, tA.String()); n != 5 {
		t.Errorf("metric points = %d, want 5", n)
	}
	if n := count(t, ch, `SELECT count() FROM metric_points FINAL WHERE tenant_id = $1 AND type = 'histogram' AND isNaN(sum)`, tA.String()); n != 1 {
		t.Errorf("sum 없는 histogram은 NaN이어야 한다 (%d)", n)
	}
	if n := count(t, ch, `SELECT count() FROM logs_local FINAL WHERE tenant_id = $1 AND event_id = 'uid:uid-it-1'`, tA.String()); n != 1 {
		t.Errorf("logs = %d, want 1", n)
	}
	// lookup MV가 worker insert로도 채워진다
	if n := count(t, ch, `SELECT count() FROM trace_lookup WHERE tenant_id = $1 AND trace_id = unhex($2)`, tA.String(), hex.EncodeToString(traceID[:])); n == 0 {
		t.Error("trace_lookup이 비었다")
	}
	// 위조 record는 어느 tenant에도 저장되지 않고 quarantine에 위치만 남는다
	// header와 key의 tenant가 어긋나면 어느 쪽도 믿지 않으므로 tenant는 zero UUID다. payload 해시로 찾는다.
	forgedHash := sha256.Sum256(forged.Value)
	if n := count(t, ch, `SELECT count() FROM ingest_quarantine FINAL WHERE payload_sha256 = unhex($1) AND reason = $2
		AND tenant_id = toUUID('00000000-0000-0000-0000-000000000000')`, hex.EncodeToString(forgedHash[:]), ReasonTenantKeyMismatch); n != 1 {
		t.Errorf("quarantine = %d, want 1", n)
	}
	if n := count(t, ch, `SELECT count() FROM spans_local WHERE tenant_id = $1`, tB.String()); n == 0 || n > 2 {
		t.Errorf("tenant B 원본 행 = %d (정상 1행, crash 재처리로 최대 2행)", n)
	}
}

// 같은 batch를 다시 쓰면 insert token으로 무시된다 (merge 전 raw 행 수로 확인).
func TestSinkTokenDedup(t *testing.T) {
	ctx := context.Background()
	sink, err := OpenClickHouseSink(ctx, env(t, "MONTRACER_TEST_CH_INGEST_DSN"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	ch := adminConn(t)
	tenant := randomTenant(t)
	r, _ := envelope.Traces(span(pcommon.TraceID{0xdd, 1}, pcommon.SpanID{1}, "x"), envelope.Meta{Tenant: tenant, ReceivedAt: time.Now(), PolicyVersion: 1, RoutingEpoch: 1})
	msgs := toMsgs(0, time.Now().UnixNano(), r.Records...) // 다른 실행과 겹치지 않는 offset
	b := builder.Build(msgs)[0]
	for range 3 {
		if err := sink.Write(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, ch, `SELECT count() FROM spans_local WHERE tenant_id = $1`, tenant.String()); n != 1 {
		t.Errorf("raw rows = %d, want 1", n)
	}

	// topic 재생성으로 offset이 재사용돼도 다른 내용은 버려지지 않는다 (token에 내용 해시, ADR 0021 §3)
	other := randomTenant(t)
	r2, _ := envelope.Traces(span(pcommon.TraceID{0xdd, 2}, pcommon.SpanID{1}, "after-reset"), envelope.Meta{Tenant: other, ReceivedAt: time.Now(), PolicyVersion: 1, RoutingEpoch: 1})
	reused := builder.Build(toMsgs(0, msgs[0].Offset, r2.Records...))[0]
	if err := sink.Write(ctx, reused); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ch, `SELECT count() FROM spans_local WHERE tenant_id = $1`, other.String()); n != 1 {
		t.Errorf("record at reused offset was dropped as duplicate: rows = %d, want 1", n)
	}
}

// worker 계정이 원본을 읽을 수 있으면 기동하지 않는다 (ADR 0018 최소 권한).
func TestSinkRejectsReadableAccount(t *testing.T) {
	_, err := OpenClickHouseSink(context.Background(), env(t, "MONTRACER_TEST_CH_ADMIN_DSN"), 0)
	if !errors.Is(err, ErrIngestCanRead) {
		t.Fatalf("err = %v, want ErrIngestCanRead", err)
	}
}
