// worker는 수집 worker다 (cmd/worker/README.md, D02 §05, §10, ADR 0021, 0026).
//
// 역할 (MONTRACER_WORKER_ROLES, 쉼표 구분, 기본 ingest)
//
//	ingest : Kafka 원본 topic → ClickHouse 원본 테이블
//	rollup : metric_points → metric_1m·metric_1h window 재계산. cluster에서 한 process만 켠다(ADR 0026 §4, 0028)
//	servicemap : span 쌍 → service_edges_1m edge window 재계산. cluster에서 한 process만 켠다(ADR 0062)
//
// 환경 변수
//
//	MONTRACER_KAFKA_BROKERS       host:port[,host:port]                (ingest)
//	MONTRACER_CH_INGEST_DSN       ClickHouse ingest 계정 (INSERT만)      (ingest)
//	MONTRACER_CH_INSERT_QUORUM    insert 성공에 필요한 replica 수 (production 2, 로컬 0=끔)
//	MONTRACER_WORKER_GROUP        consumer group (기본 montracer-worker-raw-v1)
//	MONTRACER_CH_ROLLUP_DSN       ClickHouse rollup 계정 (metric 읽기 + metric_1m 쓰기) (rollup)
//	MONTRACER_CH_SERVICEMAP_DSN   ClickHouse service map 계정 (span 짝 컬럼 읽기 + service_edges_1m 쓰기) (servicemap)
//	MONTRACER_METRICS_ADDR        운영 지표 listener (기본 :9464, /metrics·/healthz)
//
// 일회성 metric backfill (D02 §07 "10분 이후는 backfill job만", ADR 0035) — MONTRACER_CH_ROLLUP_DSN 필요
//
//	worker backfill --tenant UUID --from RFC3339 --to RFC3339 [--resolution 1m|1h|all] [--job-id ID]
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
	"github.com/polynomeer/montracer/internal/servicemap"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) > 1 && os.Args[1] == "backfill" {
		if err := backfill(logger, os.Args[2:]); err != nil {
			logger.Error("metric backfill failed", slog.String("error", err.Error()))
			if errors.Is(err, errUsage) {
				os.Exit(2)
			}
			os.Exit(1)
		}
		return
	}
	if err := run(logger); err != nil {
		logger.Error("worker stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// minuteConfig는 1분 rollup 설정이다 (ADR 0026). live job과 backfill이 같은 값을 쓴다.
func minuteConfig(store rollup.Store, logger *slog.Logger) rollup.Config {
	return rollup.Config{Store: store, Logger: logger}
}

// hourConfig는 1시간 rollup 설정이다: 원본에서 직접 계산, 늦은 point(≤10분)는 아직 닫히지 않은
// 마지막 1시간 window에 반영된다 (ADR 0028). 드문 stream도 직전 시간의 point를 기준점으로 쓴다.
func hourConfig(store rollup.Store, logger *slog.Logger) rollup.Config {
	return rollup.Config{Store: store, Logger: logger, Window: time.Hour,
		Recompute: time.Hour, MaxCatchUp: 24 * time.Hour, Interval: 2 * time.Minute, Retention: 395 * 24 * time.Hour,
		BaselineLookback: time.Hour}
}

// workerRoles는 MONTRACER_WORKER_ROLES다.
type workerRoles struct{ ingest, rollup, serviceMap bool }

func roles() (workerRoles, error) {
	var rs workerRoles
	v := os.Getenv("MONTRACER_WORKER_ROLES")
	if v == "" {
		v = "ingest"
	}
	for _, r := range strings.Split(v, ",") {
		switch strings.TrimSpace(r) {
		case "ingest":
			rs.ingest = true
		case "rollup":
			rs.rollup = true
		case "servicemap":
			rs.serviceMap = true
		default:
			return workerRoles{}, fmt.Errorf("MONTRACER_WORKER_ROLES: unknown role %q (ingest, rollup, servicemap)", r)
		}
	}
	return rs, nil
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rs, err := roles()
	if err != nil {
		return err
	}

	reg := opsmetrics.NewRegistry()
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var ingestWorker *pipeline.Worker
	if rs.ingest {
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

	var jobs []*rollup.Job
	if rs.rollup {
		store, err := rollup.OpenClickHouse(startCtx, os.Getenv("MONTRACER_CH_ROLLUP_DSN"))
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		hourly, err := store.ForWindow(time.Hour)
		if err != nil {
			return err
		}
		rm := opsmetrics.NewRollup(reg)
		minute, hour := minuteConfig(store, logger), hourConfig(hourly, logger)
		minute.Observer, hour.Observer = rm.For("1m"), rm.For("1h")
		jobs = append(jobs, rollup.New(minute), rollup.New(hour))
	}

	var edgeJob *servicemap.Job
	if rs.serviceMap {
		store, err := servicemap.OpenClickHouse(startCtx, os.Getenv("MONTRACER_CH_SERVICEMAP_DSN"))
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		edgeJob = servicemap.New(servicemap.Config{Store: store, Logger: logger, Observer: opsmetrics.NewServiceMap(reg)})
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
	logger.Info("worker started", slog.Bool("ingest", rs.ingest), slog.Bool("rollup", rs.rollup), slog.Bool("servicemap", rs.serviceMap),
		slog.String("metrics_addr", metricsSrv.Addr))

	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func() { defer wg.Done(); job.Run(ctx) }()
	}
	if edgeJob != nil {
		wg.Add(1)
		go func() { defer wg.Done(); edgeJob.Run(ctx) }()
	}
	var runErr error
	if ingestWorker != nil {
		runErr = ingestWorker.Run(ctx)
		if runErr != nil {
			stop() // ingest가 오류로 멈추면 rollup·servicemap도 함께 내린다(재시작은 orchestrator가)
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
