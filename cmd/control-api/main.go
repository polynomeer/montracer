// control-api는 관리 API 서비스다 (cmd/control-api/README.md, D02 §14, §20).
//
// 환경 변수
//
//	MONTRACER_CONTROL_ADDR      listen 주소 (기본 :8081)
//	MONTRACER_PG_APP_DSN        제어 DB 앱 계정 (RLS 적용)
//	MONTRACER_KEY_PEPPER_HEX    key hash pepper (hex, 32 byte 이상, secret manager에서 주입)
//	MONTRACER_CURSOR_KEY_HEX    page cursor 서명 key (hex, 32 byte 이상, secret manager에서 주입, ADR 0034)
//	MONTRACER_METRICS_ADDR      운영 지표 listener (기본 :9464, /metrics — 고객 경로와 분리)
//	MONTRACER_CH_QUERY_DSN      (선택) ClickHouse query 계정. 있으면 monitor validate가 24시간 dry-run을 한다(ADR 0052)
//	MONTRACER_NOTIFY_KEY_HEX    (선택) 알림 서명 secret 암호화 master key(hex, 32 byte 이상). 비우면 notification policy API는 404(ADR 0053)
//	MONTRACER_NOTIFY_KEY_VERSION master key 버전 이름(기본 v1)
//	MONTRACER_NOTIFY_PREVIOUS_KEYS 회전 중인 이전 key "버전:hex,…"(control-api는 암호화만 하므로 보통 비운다)
//	MONTRACER_SESSION_KEY_HEX   (선택) browser session key(hex, 32 byte 이상, query-api와 같은 값). 비우면 session 끔(ADR 0064)
//	MONTRACER_OIDC_STEPUP_ACR   (선택) MFA step-up에 인정할 acr 값 목록(공백 구분, IdP의 MFA 정책 이름). 비면 ID token amr의 MFA 증거를 본다(ADR 0066)
//	MONTRACER_OIDC_ISSUER       (선택) OIDC issuer. 비우면 로그인 경로 404. MONTRACER_OIDC_CLIENT_ID·_CLIENT_SECRET·_REDIRECT_URL과 함께
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
	"github.com/polynomeer/montracer/internal/notify"
	"github.com/polynomeer/montracer/internal/opsmetrics"
	"github.com/polynomeer/montracer/internal/session"
	"github.com/polynomeer/montracer/internal/telemetrystore"
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
	// 정의 변경으로 닫힌 사건의 알림에도 alert-worker와 같은 deep link를 싣는다(ADR 0055)
	links, err := notify.ParseWebLinks(os.Getenv("MONTRACER_PUBLIC_WEB_URL"))
	if err != nil {
		return fmt.Errorf("MONTRACER_PUBLIC_WEB_URL: %w", err)
	}
	switch v := os.Getenv("MONTRACER_MONITOR_API_ENABLED"); v {
	case "", "true":
		monitors = controldb.NewMonitorStore(db).WithLinks(links)
	case "false":
		logger.Info("monitor API disabled by MONTRACER_MONITOR_API_ENABLED=false")
	default:
		return errors.New("MONTRACER_MONITOR_API_ENABLED must be true or false")
	}
	// 서비스 상태 API flag(ADR 0073 Rollback): false거나 monitor API가 꺼졌으면 /api/v1/service-health가 404다. 기본은 켜짐.
	var serviceHealth controlapi.ServiceHealthStore
	switch v := os.Getenv("MONTRACER_SERVICE_HEALTH_ENABLED"); v {
	case "", "true":
		if store, ok := monitors.(*controldb.MonitorStore); ok && store != nil {
			serviceHealth = store
		}
	case "false":
		logger.Info("service health API disabled by MONTRACER_SERVICE_HEALTH_ENABLED=false")
	default:
		return errors.New("MONTRACER_SERVICE_HEALTH_ENABLED must be true or false")
	}
	// dashboard API flag(ADR 0057 Rollback): false면 /api/v1/dashboards 경로가 404다. 기본은 켜짐.
	var dashboards controlapi.DashboardStore
	switch v := os.Getenv("MONTRACER_DASHBOARD_API_ENABLED"); v {
	case "", "true":
		dashboards = controldb.NewDashboardStore(db)
	case "false":
		logger.Info("dashboard API disabled by MONTRACER_DASHBOARD_API_ENABLED=false")
	default:
		return errors.New("MONTRACER_DASHBOARD_API_ENABLED must be true or false")
	}
	// key 관리 API flag(ADR 0069 Rollback): false면 /api/v1/keys 경로가 404다. 기본은 켜짐.
	var (
		keyAPI      controlapi.KeyStore
		generateKey controlapi.KeyGenerator
	)
	switch v := os.Getenv("MONTRACER_KEY_API_ENABLED"); v {
	case "", "true":
		keyAPI = keys
		generateKey = func(kind authz.Kind) (authz.GeneratedKey, error) { return hasher.Generate(kind, nil) }
	case "false":
		logger.Info("key API disabled by MONTRACER_KEY_API_ENABLED=false")
	default:
		return errors.New("MONTRACER_KEY_API_ENABLED must be true or false")
	}
	// 멤버·초대 API flag(ADR 0067 Rollback): false면 /api/v1/members·/api/v1/invitations·/api/v1/auth/invitation이 404다. 기본은 켜짐.
	var members controlapi.MemberStore
	switch v := os.Getenv("MONTRACER_MEMBER_API_ENABLED"); v {
	case "", "true":
		members = controldb.NewMemberStore(db)
	case "false":
		logger.Info("member API disabled by MONTRACER_MEMBER_API_ENABLED=false")
	default:
		return errors.New("MONTRACER_MEMBER_API_ENABLED must be true or false")
	}

	// 24시간 dry-run(ADR 0052): 요청자 권한으로 query 계정·row policy 경로를 읽는다. DSN이 없으면 dry_run은 null(경고 dry_run_unavailable).
	// ClickHouse는 선택 의존이다: 기동 때 닿지 않으면 경고만 남기고 뜬다(dry-run 때 다시 연결, 그동안 dry_run_failed).
	// DSN 형식 오류·읽기 전용이 아닌 계정은 설정 오류라 기동을 거부한다.
	var metrics controlapi.DryRunMetrics
	if dsn := os.Getenv("MONTRACER_CH_QUERY_DSN"); dsn != "" {
		lazy := newLazyQuery(dsn, logger)
		if _, err := lazy.connect(startCtx); err != nil {
			if errors.Is(err, telemetrystore.ErrNotReadOnly) || errors.Is(err, telemetrystore.ErrInvalidDSN) {
				return fmt.Errorf("clickhouse query: %w", err)
			}
			logger.Warn("clickhouse query unavailable at start; monitor dry-run will retry", slog.String("error", err.Error()))
		}
		defer lazy.Close()
		metrics = lazy
	} else {
		logger.Info("monitor dry-run disabled: MONTRACER_CH_QUERY_DSN is empty")
	}

	sealer, err := notify.SealerFromEnv(os.Getenv("MONTRACER_NOTIFY_KEY_HEX"), os.Getenv("MONTRACER_NOTIFY_KEY_VERSION"), os.Getenv("MONTRACER_NOTIFY_PREVIOUS_KEYS"))
	if err != nil {
		return err
	}
	var policies controlapi.PolicyStore
	if sealer != nil && monitors != nil {
		policies = controldb.NewNotificationPolicyStore(db)
	} else {
		logger.Info("notification policy API disabled (MONTRACER_NOTIFY_KEY_HEX empty or monitor API off)")
	}

	reg := opsmetrics.NewRegistry()
	// browser session·OIDC 로그인(ADR 0064). key가 없으면 bearer key만 받는다(rollback).
	sessions, err := session.ManagerFromEnv(os.Getenv, controldb.NewSessionStore(db))
	if err != nil {
		return err
	}
	oidcProvider, err := session.ProviderFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	if oidcProvider != nil && sessions == nil {
		return errors.New("MONTRACER_OIDC_ISSUER needs MONTRACER_SESSION_KEY_HEX")
	}
	h, err := controlapi.NewHandler(controlapi.Config{
		Sessions:  sessions,
		OIDC:      oidcProvider,
		StepUpACR: os.Getenv("MONTRACER_OIDC_STEPUP_ACR"),
		Authenticate: func(ctx context.Context, token string) (authz.Principal, error) {
			return hasher.Authenticate(ctx, token, authz.KindAPIKey, keys.LookupKey, time.Now())
		},
		Audit:                         controldb.NewAuditStore(db),
		Monitors:                      monitors,
		Dashboards:                    dashboards,
		Members:                       members,
		Keys:                          keyAPI,
		GenerateKey:                   generateKey,
		Metrics:                       metrics,
		Policies:                      policies,
		Sealer:                        sealer,
		Cursor:                        signer,
		Logger:                        logger,
		Observe:                       opsmetrics.NewControl(reg).Observe,
		ObserveDryRun:                 opsmetrics.NewDryRun(reg).ObserveDryRun,
		ObserveLogin:                  opsmetrics.NewLogin(reg).ObserveLogin,
		ServiceHealth:                 serviceHealth,
		ObserveServiceHealthTruncated: opsmetrics.NewServiceHealth(reg).ObserveTruncated,
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
