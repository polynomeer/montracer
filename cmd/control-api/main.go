// control-api는 관리 API 서비스다 (cmd/control-api/README.md, D02 §14, §20).
//
// 환경 변수
//
//	MONTRACER_CONTROL_ADDR      listen 주소 (기본 :8081)
//	MONTRACER_PG_APP_DSN        제어 DB 앱 계정 (RLS 적용)
//	MONTRACER_KEY_PEPPER_HEX    key hash pepper (hex, 32 byte 이상, secret manager에서 주입)
//	MONTRACER_CURSOR_KEY_HEX    page cursor 서명 key (hex, 32 byte 이상, secret manager에서 주입, ADR 0034)
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
	"github.com/polynomeer/montracer/internal/controlapi"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/opsmetrics"
)

// cursorTTL은 page cursor 유효 기간이다 (D02 §19 "만료 15분 HMAC cursor", ADR 0034).
const cursorTTL = 15 * time.Minute

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("control-api stopped", slog.String("error", err.Error()))
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
	cursorKey, err := hex.DecodeString(os.Getenv("MONTRACER_CURSOR_KEY_HEX"))
	if err != nil {
		return errors.New("MONTRACER_CURSOR_KEY_HEX must be hex")
	}
	signer, err := apicursor.NewSigner(cursorKey, cursorTTL, nil)
	if err != nil {
		return fmt.Errorf("MONTRACER_CURSOR_KEY_HEX: %w", err)
	}
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db, err := controldb.Open(startCtx, os.Getenv("MONTRACER_PG_APP_DSN"))
	if err != nil {
		return fmt.Errorf("control db: %w", err)
	}
	defer db.Close()
	keys := controldb.NewKeyStore(db)
	// monitor API flag(ADR 0049 Rollback): MONTRACER_MONITOR_API_ENABLED=false면 경로가 404다. 기본은 켜짐.
	var monitors controlapi.MonitorStore
	switch v := os.Getenv("MONTRACER_MONITOR_API_ENABLED"); v {
	case "", "true":
		monitors = controldb.NewMonitorStore(db)
	case "false":
		logger.Info("monitor API disabled by MONTRACER_MONITOR_API_ENABLED=false")
	default:
		return errors.New("MONTRACER_MONITOR_API_ENABLED must be true or false")
	}

	reg := opsmetrics.NewRegistry()
	h, err := controlapi.NewHandler(controlapi.Config{
		Authenticate: func(ctx context.Context, token string) (authz.Principal, error) {
			return hasher.Authenticate(ctx, token, authz.KindAPIKey, keys.LookupKey, time.Now())
		},
		Audit:    controldb.NewAuditStore(db),
		Monitors: monitors,
		Cursor:   signer,
		Logger:   logger,
		Observe:  opsmetrics.NewControl(reg).Observe,
	})
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", h)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(r.Context()); err != nil {
			http.Error(w, "control db unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	addr := os.Getenv("MONTRACER_CONTROL_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
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
	logger.Info("control-api listening", slog.String("addr", addr), slog.String("metrics_addr", metricsSrv.Addr))
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
