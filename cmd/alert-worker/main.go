// alert-worker는 monitor 주기 평가 서비스다 (cmd/alert-worker/README.md, D02 §17, ADR 0050·0051).
//
// 환경 변수
//
//	MONTRACER_PG_APP_DSN       제어 DB 앱 계정 (monitor 정의·schedule·상태·outbox)
//	MONTRACER_CH_QUERY_DSN     ClickHouse query 계정 (읽기 전용, row policy) — tenant system principal로 조회
//	MONTRACER_ALERT_WORKER_ID  lease 소유자 이름 (기본: hostname-pid). 인스턴스마다 달라야 한다
//	MONTRACER_METRICS_ADDR     운영 지표 listener (기본 :9464, /metrics)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/polynomeer/montracer/internal/alertworker"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/opsmetrics"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("alert-worker stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	id := os.Getenv("MONTRACER_ALERT_WORKER_ID")
	if id == "" {
		host, err := os.Hostname()
		if err != nil {
			return errors.New("MONTRACER_ALERT_WORKER_ID is empty and hostname is unavailable")
		}
		id = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db, err := controldb.Open(startCtx, os.Getenv("MONTRACER_PG_APP_DSN"))
	if err != nil {
		return fmt.Errorf("control db: %w", err)
	}
	defer db.Close()
	store, err := telemetrystore.OpenQuery(startCtx, os.Getenv("MONTRACER_CH_QUERY_DSN"))
	if err != nil {
		return fmt.Errorf("clickhouse query: %w", err)
	}
	defer func() { _ = store.Close() }()

	reg := opsmetrics.NewRegistry()
	w, err := alertworker.New(alertworker.Config{
		Store:    controldb.NewAlertStore(db),
		Metrics:  store,
		Worker:   id,
		Logger:   logger,
		Observer: opsmetrics.NewAlert(reg),
	})
	if err != nil {
		return err
	}
	metricsSrv := opsmetrics.NewServer(os.Getenv("MONTRACER_METRICS_ADDR"), reg, nil)
	if err := opsmetrics.Start(ctx, metricsSrv, func(err error) {
		logger.Error("metrics listener stopped", slog.String("error", err.Error()))
	}); err != nil {
		return err
	}
	logger.Info("alert-worker running", slog.String("worker", id), slog.String("metrics_addr", metricsSrv.Addr))
	w.Run(ctx)
	shutdown, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	return metricsSrv.Shutdown(shutdown)
}
