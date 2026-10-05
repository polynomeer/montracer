// worker는 수집 worker다 (cmd/worker/README.md, D02 §05, §10, ADR 0021, 0026).
//
// 역할 (MONTRACER_WORKER_ROLES, 쉼표 구분, 기본 ingest)
//
//	ingest : Kafka 원본 topic → ClickHouse 원본 테이블
//	rollup : metric_points → metric_1m window 재계산. cluster에서 한 process만 켠다(ADR 0026 §4)
//
// 환경 변수
//
//	MONTRACER_KAFKA_BROKERS       host:port[,host:port]                (ingest)
//	MONTRACER_CH_INGEST_DSN       ClickHouse ingest 계정 (INSERT만)      (ingest)
//	MONTRACER_CH_INSERT_QUORUM    insert 성공에 필요한 replica 수 (production 2, 로컬 0=끔)
//	MONTRACER_WORKER_GROUP        consumer group (기본 montracer-worker-raw-v1)
//	MONTRACER_CH_ROLLUP_DSN       ClickHouse rollup 계정 (metric 읽기 + metric_1m 쓰기) (rollup)
//	MONTRACER_METRICS_ADDR        운영 지표 listener (기본 :9464, /metrics·/healthz)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/polynomeer/montracer/internal/opsmetrics"
	"github.com/polynomeer/montracer/internal/pipeline"
	"github.com/polynomeer/montracer/internal/rollup"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("worker stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func roles() (ingest, roll bool, err error) {
	v := os.Getenv("MONTRACER_WORKER_ROLES")
	if v == "" {
		v = "ingest"
	}
	for _, r := range strings.Split(v, ",") {
		switch strings.TrimSpace(r) {
		case "ingest":
			ingest = true
		case "rollup":
			roll = true
		default:
			return false, false, fmt.Errorf("MONTRACER_WORKER_ROLES: unknown role %q (ingest, rollup)", r)
		}
	}
	return ingest, roll, nil
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	doIngest, doRollup, err := roles()
	if err != nil {
		return err
	}

	reg := opsmetrics.NewRegistry()
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var ingestWorker *pipeline.Worker
	if doIngest {
		quorum := 0
		if v := os.Getenv("MONTRACER_CH_INSERT_QUORUM"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return errors.New("MONTRACER_CH_INSERT_QUORUM must be a non-negative integer")
			}
			quorum = n
		}
		sink, err := pipeline.OpenClickHouseSink(startCtx, os.Getenv("MONTRACER_CH_INGEST_DSN"), quorum)
		if err != nil {
			return err
		}
		defer func() { _ = sink.Close() }()
		ingestWorker, err = pipeline.NewWorker(pipeline.Config{
			Brokers:  strings.Split(os.Getenv("MONTRACER_KAFKA_BROKERS"), ","),
			Group:    os.Getenv("MONTRACER_WORKER_GROUP"),
			Sink:     sink,
			Logger:   logger,
			Observer: opsmetrics.NewWorker(reg),
		})
		if err != nil {
			return err
		}
		defer ingestWorker.Close()
		if err := ingestWorker.Ping(startCtx); err != nil {
			return fmt.Errorf("kafka: %w", err)
		}
	}

	var job *rollup.Job
	if doRollup {
		store, err := rollup.OpenClickHouse(startCtx, os.Getenv("MONTRACER_CH_ROLLUP_DSN"))
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		job = rollup.New(rollup.Config{Store: store, Logger: logger, Observer: opsmetrics.NewRollup(reg)})
	}

	metricsSrv := opsmetrics.NewServer(os.Getenv("MONTRACER_METRICS_ADDR"), reg, nil)
	if err := opsmetrics.Start(ctx, metricsSrv, func(err error) {
		logger.Error("metrics listener stopped", slog.String("error", err.Error()))
	}); err != nil {
		return err
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutdown)
	}()
	logger.Info("worker started", slog.Bool("ingest", doIngest), slog.Bool("rollup", doRollup),
		slog.String("metrics_addr", metricsSrv.Addr))

	var wg sync.WaitGroup
	if job != nil {
		wg.Add(1)
		go func() { defer wg.Done(); job.Run(ctx) }()
	}
	var runErr error
	if ingestWorker != nil {
		runErr = ingestWorker.Run(ctx)
		if runErr != nil {
			stop() // ingest가 오류로 멈추면 rollup도 함께 내린다(재시작은 orchestrator가)
		}
	} else {
		<-ctx.Done()
	}
	wg.Wait()
	if runErr != nil {
		return runErr
	}
	logger.Info("worker stopped by signal")
	return nil
}
