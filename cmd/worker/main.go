// worker는 Kafka 원본 topic을 ClickHouse 원본 테이블로 옮기는 수집 worker다 (cmd/worker/README.md, D02 §05, ADR 0021).
//
// 환경 변수
//
//	MONTRACER_KAFKA_BROKERS       host:port[,host:port]
//	MONTRACER_CH_INGEST_DSN       ClickHouse ingest 계정 (INSERT만)
//	MONTRACER_CH_INSERT_QUORUM    insert 성공에 필요한 replica 수 (production 2, 로컬 0=끔)
//	MONTRACER_WORKER_GROUP        consumer group (기본 montracer-worker-raw-v1)
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
	"syscall"
	"time"

	"github.com/polynomeer/montracer/internal/opsmetrics"
	"github.com/polynomeer/montracer/internal/pipeline"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("worker stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	quorum := 0
	if v := os.Getenv("MONTRACER_CH_INSERT_QUORUM"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return errors.New("MONTRACER_CH_INSERT_QUORUM must be a non-negative integer")
		}
		quorum = n
	}
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sink, err := pipeline.OpenClickHouseSink(startCtx, os.Getenv("MONTRACER_CH_INGEST_DSN"), quorum)
	if err != nil {
		return err
	}
	defer func() { _ = sink.Close() }()

	reg := opsmetrics.NewRegistry()
	w, err := pipeline.NewWorker(pipeline.Config{
		Brokers:  strings.Split(os.Getenv("MONTRACER_KAFKA_BROKERS"), ","),
		Group:    os.Getenv("MONTRACER_WORKER_GROUP"),
		Sink:     sink,
		Logger:   logger,
		Observer: opsmetrics.NewWorker(reg),
	})
	if err != nil {
		return err
	}
	defer w.Close()
	if err := w.Ping(startCtx); err != nil {
		return fmt.Errorf("kafka: %w", err)
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
	logger.Info("worker started", slog.String("metrics_addr", metricsSrv.Addr))
	if err := w.Run(ctx); err != nil {
		return err
	}
	logger.Info("worker stopped by signal")
	return nil
}
