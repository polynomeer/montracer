package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/polynomeer/montracer/internal/telemetry/envelope"
)

// kafkaUp은 수집 topic을 멱등하게 만들고, 이미 있는 topic의 내구성 설정을 검증한다.
// production 기본: RF 3, min.insync.replicas 2, retention 24h (ADR 0002, D02 §04).
// RF 3 미만은 MONTRACER_KAFKA_ALLOW_LOW_REPLICATION=1(로컬·CI 단일 broker)일 때만 허용한다.
func kafkaUp() error {
	brokers := strings.Split(os.Getenv("MONTRACER_KAFKA_BROKERS"), ",")
	if brokers[0] == "" {
		return errors.New(usage)
	}
	partitions, err := envInt("MONTRACER_KAFKA_PARTITIONS", 6)
	if err != nil {
		return err
	}
	rf, err := envInt("MONTRACER_KAFKA_REPLICATION", 3)
	if err != nil {
		return err
	}
	if rf < 3 && os.Getenv("MONTRACER_KAFKA_ALLOW_LOW_REPLICATION") != "1" {
		return fmt.Errorf("replication factor %d < 3: set MONTRACER_KAFKA_ALLOW_LOW_REPLICATION=1 only for local/CI single broker (ADR 0002)", rf)
	}
	want := topicSpec{
		rf:        rf,
		minISR:    min(2, rf),
		retention: (24 * time.Hour).Milliseconds(),
		maxMsg:    envelope.MaxMessageBytes + 64<<10,
	}

	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		return fmt.Errorf("kafka client: %w", err)
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	retention := strconv.FormatInt(want.retention, 10)
	maxMsg := strconv.Itoa(want.maxMsg)
	isr := strconv.Itoa(want.minISR)
	configs := map[string]*string{"retention.ms": &retention, "min.insync.replicas": &isr, "max.message.bytes": &maxMsg}
	topics := []string{envelope.TopicTraces, envelope.TopicLogs, envelope.TopicMetrics}
	resp, err := adm.CreateTopics(ctx, int32(partitions), int16(rf), configs, topics...) //nolint:gosec // envInt가 1..1000으로 검증
	if err != nil {
		return fmt.Errorf("create topics: %w", err)
	}
	for _, t := range topics {
		switch r := resp[t]; {
		case r.Err == nil:
			fmt.Printf("kafka      created %s (partitions=%d rf=%d min.insync=%d)\n", t, partitions, rf, want.minISR)
		case errors.Is(r.Err, kerr.TopicAlreadyExists):
			fmt.Printf("kafka      exists  %s — 설정 검증\n", t)
		default:
			return fmt.Errorf("create topic %s: %w", t, r.Err)
		}
	}
	return verifyTopics(ctx, adm, topics, want, partitions)
}

type topicSpec struct {
	rf, minISR, maxMsg int
	retention          int64
}

// verifyTopics는 실제 topic 설정이 내구성 계약과 맞는지 확인한다.
// 미리 RF 1·min.insync 1로 만들어진 topic이 조용히 통과하면 acks=all의 의미가 깨진다 (ADR 0002).
func verifyTopics(ctx context.Context, adm *kadm.Client, topics []string, want topicSpec, partitions int) error {
	details, err := adm.ListTopics(ctx, topics...)
	if err != nil {
		return fmt.Errorf("describe topics: %w", err)
	}
	cfgs, err := adm.DescribeTopicConfigs(ctx, topics...)
	if err != nil {
		return fmt.Errorf("describe topic configs: %w", err)
	}
	var problems []string
	for _, t := range topics {
		d, ok := details[t]
		if !ok || d.Err != nil {
			problems = append(problems, t+": not found")
			continue
		}
		for _, p := range d.Partitions {
			if len(p.Replicas) != want.rf {
				problems = append(problems, fmt.Sprintf("%s: partition %d replication %d, want %d", t, p.Partition, len(p.Replicas), want.rf))
				break
			}
		}
		if len(d.Partitions) < partitions {
			fmt.Printf("kafka      warn    %s has %d partitions (< %d); 늘리려면 운영 절차로 증설\n", t, len(d.Partitions), partitions)
		}
		rc, err := cfgs.On(t, nil)
		if err != nil {
			problems = append(problems, t+": configs unavailable")
			continue
		}
		got := map[string]string{}
		for _, c := range rc.Configs {
			if c.Value != nil {
				got[c.Key] = *c.Value
			}
		}
		check := func(key string, ok bool) {
			if !ok {
				problems = append(problems, fmt.Sprintf("%s: %s=%s", t, key, got[key]))
			}
		}
		isr, _ := strconv.Atoi(got["min.insync.replicas"])
		check("min.insync.replicas", isr == want.minISR)
		ret, _ := strconv.ParseInt(got["retention.ms"], 10, 64)
		check("retention.ms", ret == want.retention)
		mm, _ := strconv.Atoi(got["max.message.bytes"])
		check("max.message.bytes", mm >= want.maxMsg)
	}
	if len(problems) > 0 {
		return fmt.Errorf("kafka topic settings do not match the durability contract (ADR 0002, 0020):\n  %s", strings.Join(problems, "\n  "))
	}
	fmt.Println("kafka      verified rf·min.insync·retention·max.message.bytes")
	return nil
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 1000 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}
