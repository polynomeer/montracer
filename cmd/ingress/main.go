// ingress는 OTLP/HTTP 수신 서비스다 (cmd/ingress/README.md, D02 §04, ADR 0020).
//
// 환경 변수
//
//	MONTRACER_INGRESS_ADDR      listen 주소 (기본 :4318)
//	MONTRACER_PG_APP_DSN        제어 DB 앱 계정 (ingest key 조회)
//	MONTRACER_KEY_PEPPER_HEX    ingest key hash pepper (hex, 32 byte 이상, secret manager에서 주입)
//	MONTRACER_KAFKA_BROKERS     host:port[,host:port]
//	MONTRACER_ROUTING_EPOCH     tenant routing epoch (Cell registry 구현 전 고정값, 기본 1)
//	MONTRACER_METRICS_ADDR      운영 지표 listener (기본 :9464, /metrics — 고객 경로와 분리)
//	MONTRACER_INGRESS_MAX_INFLIGHT   instance 동시 처리 요청 상한 (기본 256, 넘으면 503)
//	MONTRACER_INGRESS_REPLICAS       ingress replica 수 — tenant 한도를 이 수로 나눠 적용 (기본 1, ADR 0024)
//	MONTRACER_QUOTA_RECORDS_PER_SEC, MONTRACER_QUOTA_RECORDS_BURST,
//	MONTRACER_QUOTA_BYTES_PER_SEC,   MONTRACER_QUOTA_BYTES_BURST   tenant·signal별 기본 한도 (cluster 전체)
//	MONTRACER_QUOTA_OVERRIDES_FILE   tenant별 한도 JSON (선택, 10초마다 다시 읽음)
//	MONTRACER_QUOTA_ACTIVE_SERIES    tenant별 metric 활성 series 기본 상한 (기본 100,000, ADR 0029)
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/ingest"
	"github.com/polynomeer/montracer/internal/opsmetrics"
	"github.com/polynomeer/montracer/internal/quota"
	"github.com/polynomeer/montracer/internal/telemetry/otlp"
	"github.com/polynomeer/montracer/internal/telemetry/redact"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("ingress stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pepper, err := hex.DecodeString(os.Getenv("MONTRACER_KEY_PEPPER_HEX"))
	if err != nil {
		return errors.New("MONTRACER_KEY_PEPPER_HEX must be hex")
	}
	hasher, err := authz.NewKeyHasher(pepper)
	if err != nil {
		return err
	}
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db, err := controldb.Open(startCtx, os.Getenv("MONTRACER_PG_APP_DSN"))
	if err != nil {
		return fmt.Errorf("control db: %w", err)
	}
	defer db.Close()
	keys := controldb.NewKeyStore(db)

	producer, err := ingest.NewKafkaProducer(strings.Split(os.Getenv("MONTRACER_KAFKA_BROKERS"), ","))
	if err != nil {
		return err
	}
	defer producer.Close()
	if err := producer.Ping(startCtx); err != nil {
		return fmt.Errorf("kafka: %w", err)
	}

	epoch := int64(1)
	if v := os.Getenv("MONTRACER_ROUTING_EPOCH"); v != "" {
		if epoch, err = strconv.ParseInt(v, 10, 64); err != nil || epoch <= 0 {
			return errors.New("MONTRACER_ROUTING_EPOCH must be a positive integer")
		}
	}
	reg := opsmetrics.NewRegistry()
	ingressMetrics := opsmetrics.NewIngress(reg)
	limiter, overrides, err := newLimiter(ctx, logger, ingressMetrics)
	if err != nil {
		return err
	}
	series, err := newSeriesLimiter(ctx, logger, db, overrides)
	if err != nil {
		return err
	}
	maxInflight, err := envInt("MONTRACER_INGRESS_MAX_INFLIGHT", 256)
	if err != nil {
		return err
	}
	h, err := ingest.NewHandler(ingest.Config{
		Authenticate: func(ctx context.Context, token string) (authz.Principal, error) {
			return hasher.Authenticate(ctx, token, authz.KindIngestKey, keys.LookupKey, time.Now())
		},
		Producer:     producer,
		Redactor:     redact.New(redact.DefaultPolicy),
		RoutingEpoch: epoch,
		Logger:       logger,
		Observer:     ingressMetrics,
		Quota:        limiter,
		Series:       series,
		MaxInflight:  maxInflight,
	})
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", h)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := producer.Ping(r.Context()); err != nil {
			http.Error(w, "kafka unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	addr := os.Getenv("MONTRACER_INGRESS_ADDR")
	if addr == "" {
		addr = ":4318"
	}
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// 본문 읽기(ReadTimeout) + Kafka append 대기(ingest ProduceTimeout 10s) + 처리 여유 < WriteTimeout.
		// append가 끝났는데 응답을 못 써서 불필요한 재전송이 생기지 않게 한다.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	metricsSrv := opsmetrics.NewServer(os.Getenv("MONTRACER_METRICS_ADDR"), reg, nil)
	if err := opsmetrics.Start(ctx, metricsSrv, func(err error) {
		logger.Error("metrics listener stopped", slog.String("error", err.Error()))
	}); err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.Info("ingress listening", slog.String("addr", addr), slog.String("metrics_addr", metricsSrv.Addr))
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	err = srv.Shutdown(shutdown)
	_ = metricsSrv.Shutdown(shutdown) // 고객 요청을 다 끝낸 뒤 지표 listener를 닫는다
	return err
}

func envInt(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return n, nil
}

func envFloat(name string, def float64) (float64, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("%s must be a positive number", name)
	}
	return f, nil
}

// newLimiter는 tenant quota를 만든다 (ADR 0024). overrides 파일이 잘못됐으면 기동하지 않는다.
// 두 번째 반환값은 현재 overrides(없으면 nil)이며 series 상한도 같은 파일을 쓴다.
func newLimiter(ctx context.Context, logger *slog.Logger, m *opsmetrics.Ingress) (*quota.Limiter, func() quota.Overrides, error) {
	d := quota.DefaultLimits
	var err error
	if d.RecordsPerSecond, err = envFloat("MONTRACER_QUOTA_RECORDS_PER_SEC", d.RecordsPerSecond); err != nil {
		return nil, nil, err
	}
	if d.RecordsBurst, err = envInt("MONTRACER_QUOTA_RECORDS_BURST", d.RecordsBurst); err != nil {
		return nil, nil, err
	}
	if d.BytesPerSecond, err = envFloat("MONTRACER_QUOTA_BYTES_PER_SEC", d.BytesPerSecond); err != nil {
		return nil, nil, err
	}
	if d.BytesBurst, err = envInt("MONTRACER_QUOTA_BYTES_BURST", d.BytesBurst); err != nil {
		return nil, nil, err
	}
	replicas, err := envInt("MONTRACER_INGRESS_REPLICAS", 1)
	if err != nil {
		return nil, nil, err
	}
	if d.BytesBurst < int(otlp.DefaultLimits.MaxDecodedBytes) {
		return nil, nil, fmt.Errorf("MONTRACER_QUOTA_BYTES_BURST must be >= %d (max decoded request) or every max-size batch gets 413", otlp.DefaultLimits.MaxDecodedBytes)
	}
	cfg := quota.Config{Default: d, Replicas: replicas, MinBytesBurst: int(otlp.DefaultLimits.MaxDecodedBytes)}
	if path := os.Getenv("MONTRACER_QUOTA_OVERRIDES_FILE"); path != "" {
		f, err := quota.LoadFileOverrides(path)
		if err != nil {
			return nil, nil, err
		}
		cfg.Overrides = f.Get
		go f.Watch(ctx, 10*time.Second,
			func() { m.ObserveOverridesReload(true); logger.Info("quota overrides reloaded") },
			func(err error) {
				m.ObserveOverridesReload(false)
				logger.Error("quota overrides reload failed; keeping previous", slog.String("error", err.Error()))
			})
	}
	return quota.New(cfg), cfg.Overrides, nil
}

// seriesStore는 controldb.SeriesStore를 quota.SeriesStore에 맞춘다.
type seriesStore struct{ *controldb.SeriesStore }

func (s seriesStore) Touch(ctx context.Context, tenant authz.TenantID, ids [][16]byte, metrics []string, now time.Time) error {
	entries := make([]controldb.SeriesEntry, len(ids))
	for i := range ids {
		entries[i] = controldb.SeriesEntry{StreamID: ids[i], Metric: metrics[i]}
	}
	return s.SeriesStore.Touch(ctx, tenant, entries, now)
}

// newSeriesLimiter는 metric 활성 series 상한이다 (D02 §10, ADR 0029). 등록부는 제어 DB(RLS)다.
// 10분마다 다룬 tenant의 2시간 넘게 안 보인 series를 지운다(판정에는 영향 없음, 공간 정리).
func newSeriesLimiter(ctx context.Context, logger *slog.Logger, db *controldb.DB, overrides func() quota.Overrides) (*quota.SeriesLimiter, error) {
	store := controldb.NewSeriesStore(db)
	def, err := envInt("MONTRACER_QUOTA_ACTIVE_SERIES", quota.DefaultActiveSeries)
	if err != nil {
		return nil, err
	}
	l := quota.NewSeriesLimiter(quota.SeriesConfig{Store: seriesStore{store}, Default: def,
		Limit: func(tenant string) int {
			if overrides == nil {
				return 0
			}
			return overrides()[tenant]["metrics"].ActiveSeries
		}})
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				now := time.Now()
				for _, tenant := range l.Tenants(now) {
					id, err := authz.ParseTenantID(tenant)
					if err != nil {
						continue
					}
					if _, err := store.Cleanup(ctx, id, now.Add(-2*time.Hour)); err != nil {
						logger.Warn("metric series cleanup failed", slog.String("tenant_id", tenant), slog.String("error", err.Error()))
					}
				}
			}
		}
	}()
	return l, nil
}
