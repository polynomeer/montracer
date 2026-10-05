// platform-probe는 플랫폼 synthetic probe다 (cmd/platform-probe/README.md, D04 §10, ADR 0031).
//
// 환경 변수
//
//	MONTRACER_PROBE_INGRESS_URL      ingress 기준 URL (예: https://ingest.example.com)
//	MONTRACER_PROBE_QUERY_URL        query-api 기준 URL
//	MONTRACER_PROBE_INGEST_KEY       probe tenant ingest key (secret manager에서 주입)
//	MONTRACER_PROBE_API_KEY          probe tenant API key, telemetry.read (secret manager에서 주입)
//	MONTRACER_PROBE_OTHER_API_KEY    (선택) 다른 probe tenant의 API key — 격리 검사
//	MONTRACER_PROBE_ENVIRONMENT      ingest key의 environment (기본 synthetic)
//	MONTRACER_PROBE_INTERVAL         주기 (기본 1m)
//	MONTRACER_METRICS_ADDR           운영 지표 listener (기본 :9464, /metrics)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/polynomeer/montracer/internal/opsmetrics"
	"github.com/polynomeer/montracer/internal/probe"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("platform-probe stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var interval time.Duration
	if v := os.Getenv("MONTRACER_PROBE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 10*time.Second || d > 10*time.Minute {
			return fmt.Errorf("MONTRACER_PROBE_INTERVAL must be a duration between 10s and 10m")
		}
		interval = d
	}
	reg := opsmetrics.NewRegistry()
	p, err := probe.New(probe.Config{
		IngressURL:  os.Getenv("MONTRACER_PROBE_INGRESS_URL"),
		QueryURL:    os.Getenv("MONTRACER_PROBE_QUERY_URL"),
		IngestKey:   os.Getenv("MONTRACER_PROBE_INGEST_KEY"),
		APIKey:      os.Getenv("MONTRACER_PROBE_API_KEY"),
		OtherAPIKey: os.Getenv("MONTRACER_PROBE_OTHER_API_KEY"),
		Environment: os.Getenv("MONTRACER_PROBE_ENVIRONMENT"),
		Interval:    interval,
		Logger:      logger,
		Observer:    opsmetrics.NewProbe(reg),
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
	logger.Info("platform-probe running", slog.String("metrics_addr", metricsSrv.Addr))
	p.Run(ctx)
	return nil
}
