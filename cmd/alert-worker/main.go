// alert-worker는 monitor 주기 평가 서비스다 (cmd/alert-worker/README.md, D02 §17, ADR 0050·0051).
//
// 환경 변수
//
//	MONTRACER_PG_APP_DSN       제어 DB 앱 계정 (monitor 정의·schedule·상태·outbox)
//	MONTRACER_CH_QUERY_DSN     ClickHouse query 계정 (읽기 전용, row policy) — tenant system principal로 조회
//	MONTRACER_ALERT_WORKER_ID  lease 소유자 이름 (기본: hostname-pid). 인스턴스마다 달라야 한다
//	MONTRACER_METRICS_ADDR     운영 지표 listener (기본 :9464, /metrics)
//	MONTRACER_NOTIFY_KEY_HEX   알림 서명 secret을 푸는 master key(hex, 32 byte 이상, secret manager). 비우면 알림을 보내지 않는다(쌓이기만 한다)
//	MONTRACER_NOTIFY_KEY_VERSION master key 버전 이름(기본 v1, control-api와 같아야 한다)
//	MONTRACER_NOTIFY_PREVIOUS_KEYS 회전 중인 이전 key "버전:hex,…"(풀기 전용)
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
	"github.com/polynomeer/montracer/internal/notify"
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

	// 알림 본문의 화면 deep link(D02 §20, ADR 0055). 비우면 links는 null이다. 형식 오류는 설정 오류라 기동을 거부한다
	links, err := notify.ParseWebLinks(os.Getenv("MONTRACER_PUBLIC_WEB_URL"))
	if err != nil {
		return fmt.Errorf("MONTRACER_PUBLIC_WEB_URL: %w", err)
	}
	// 재알림 끄기(ADR 0055 Rollback): false면 열린 사건을 다시 알리지 않는다. 열림·닫힘 알림은 그대로다
	renotify := true
	switch v := os.Getenv("MONTRACER_NOTIFY_RENOTIFY_ENABLED"); v {
	case "", "true":
	case "false":
		renotify = false
		logger.Info("renotification disabled by MONTRACER_NOTIFY_RENOTIFY_ENABLED=false")
	default:
		return errors.New("MONTRACER_NOTIFY_RENOTIFY_ENABLED must be true or false")
	}
	reg := opsmetrics.NewRegistry()
	w, err := alertworker.New(alertworker.Config{
		Store:    controldb.NewAlertStore(db).WithLinks(links).WithRenotify(renotify),
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
	// 알림 전송(ADR 0053): master key가 없으면 끈다. 알림은 쌓이고 MontracerNotificationBacklog가 울린다
	sealer, err := notify.SealerFromEnv(os.Getenv("MONTRACER_NOTIFY_KEY_HEX"), os.Getenv("MONTRACER_NOTIFY_KEY_VERSION"), os.Getenv("MONTRACER_NOTIFY_PREVIOUS_KEYS"))
	if err != nil {
		return err
	}
	// key가 없어도 루프는 돈다: 보내지 않고 backlog 지표만 내서 쌓이는 알림이 경보(MontracerNotificationBacklog)로 드러난다
	ncfg := alertworker.NotifierConfig{Store: controldb.NewDeliveryStore(db), Logger: logger, Observer: opsmetrics.NewNotify(reg)}
	if sealer != nil {
		ncfg.Sealer, ncfg.Sender = sealer, notify.NewClient(notify.ClientOptions{})
	} else {
		logger.Warn("notification delivery disabled: MONTRACER_NOTIFY_KEY_HEX is empty (notifications accumulate)")
	}
	n, err := alertworker.NewNotifier(ncfg)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	go func() { defer close(done); n.Run(ctx) }()
	logger.Info("alert-worker running", slog.String("worker", id), slog.String("metrics_addr", metricsSrv.Addr))
	w.Run(ctx)
	<-done
	shutdown, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	return metricsSrv.Shutdown(shutdown)
}
