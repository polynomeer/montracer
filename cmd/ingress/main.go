// ingress는 OTLP/HTTP 수신 서비스다 (cmd/ingress/README.md, D02 §04, ADR 0020).
//
// 환경 변수
//
//	MONTRACER_INGRESS_ADDR      listen 주소 (기본 :4318)
//	MONTRACER_PG_APP_DSN        제어 DB 앱 계정 (ingest key 조회)
//	MONTRACER_KEY_PEPPER_HEX    ingest key hash pepper (hex, 32 byte 이상, secret manager에서 주입)
//	MONTRACER_KAFKA_BROKERS     host:port[,host:port]
//	MONTRACER_ROUTING_EPOCH     tenant routing epoch (Cell registry 구현 전 고정값, 기본 1)
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
	h, err := ingest.NewHandler(ingest.Config{
		Authenticate: func(ctx context.Context, token string) (authz.Principal, error) {
			return hasher.Authenticate(ctx, token, authz.KindIngestKey, keys.LookupKey, time.Now())
		},
		Producer:     producer,
		Redactor:     redact.New(redact.DefaultPolicy),
		RoutingEpoch: epoch,
		Logger:       logger,
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
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.Info("ingress listening", slog.String("addr", addr))
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	return srv.Shutdown(shutdown)
}
