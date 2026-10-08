// query-api는 조회 API 서비스다 (cmd/query-api/README.md, D02 §12~13, §19).
//
// 환경 변수
//
//	MONTRACER_QUERY_ADDR        listen 주소 (기본 :8080)
//	MONTRACER_PG_APP_DSN        제어 DB 앱 계정 (API key 조회)
//	MONTRACER_KEY_PEPPER_HEX    key hash pepper (hex, 32 byte 이상, secret manager에서 주입)
//	MONTRACER_CH_QUERY_DSN      ClickHouse query 계정 (읽기 전용, row policy)
//	MONTRACER_CURSOR_KEY_HEX    검색 page cursor 서명 key (hex, 32 byte 이상, secret manager에서 주입, ADR 0034·0037).
//	                            비우면 검색 경로(/query, /query/logs)만 끈다(404) — trace·metric 조회는 그대로(rollback)
//	MONTRACER_METRICS_ADDR      운영 지표 listener (기본 :9464, /metrics — 고객 경로와 분리)
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
	"syscall"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/opsmetrics"
	"github.com/polynomeer/montracer/internal/query"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("query-api stopped", slog.String("error", err.Error()))
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

	// 검색 cursor 만료 15분 (D02 §19). key가 없으면 검색만 끈다(설정 rollback, ADR 0037). 잘못된 key는 기동 오류다.
	var signer *apicursor.Signer
	if v := os.Getenv("MONTRACER_CURSOR_KEY_HEX"); v != "" {
		cursorKey, err := hex.DecodeString(v)
		if err != nil {
			return errors.New("MONTRACER_CURSOR_KEY_HEX must be hex")
		}
		if signer, err = apicursor.NewSigner(cursorKey, 15*time.Minute, nil); err != nil {
			return fmt.Errorf("MONTRACER_CURSOR_KEY_HEX: %w", err)
		}
	} else {
		logger.Warn("log search disabled: MONTRACER_CURSOR_KEY_HEX is not set")
	}
	store, err := telemetrystore.OpenQuery(startCtx, os.Getenv("MONTRACER_CH_QUERY_DSN"))
	if err != nil {
		return fmt.Errorf("telemetry store: %w", err)
	}
	defer func() { _ = store.Close() }()

	reg := opsmetrics.NewRegistry()
	queryMetrics := opsmetrics.NewQuery(reg)
	h, err := query.NewHandler(query.Config{
		Authenticate: func(ctx context.Context, token string) (authz.Principal, error) {
			return hasher.Authenticate(ctx, token, authz.KindAPIKey, keys.LookupKey, time.Now())
		},
		Store:          store,
		Metrics:        store,
		MetricCatalog:  store,
		Logs:           store,
		Traces:         store,
		Cursor:         signer,
		Services:       controldb.NewServiceStore(db),
		Logger:         logger,
		Observe:        queryMetrics.Observe,
		ObserveCatalog: queryMetrics.ObserveCatalog,
	})
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", h)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ping(r.Context()); err != nil {
			http.Error(w, "telemetry store unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	addr := os.Getenv("MONTRACER_QUERY_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// 저장소 조회 상한(query.Config.QueryTimeout 10초) + 응답 직렬화 여유 < WriteTimeout.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
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
	logger.Info("query-api listening", slog.String("addr", addr), slog.String("metrics_addr", metricsSrv.Addr))
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	err = srv.Shutdown(shutdown)
	_ = metricsSrv.Shutdown(shutdown)
	return err
}
