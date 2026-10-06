package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/polynomeer/montracer/internal/rollup"
)

var errUsage = errors.New("usage: worker backfill --tenant UUID --from RFC3339 --to RFC3339 [--resolution 1m|1h|all] [--job-id ID]")

// backfill은 tenant 하나의 과거 metric window를 원본에서 다시 계산한다 (ADR 0035).
// live rollup과 같은 설정(minuteConfig·hourConfig)을 쓴다. 1h는 닫힌 시간 window까지만 계산한다.
func backfill(logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("worker backfill", flag.ContinueOnError)
	var (
		tenant     = fs.String("tenant", "", "tenant UUID")
		fromS      = fs.String("from", "", "시작 (RFC3339, 포함)")
		toS        = fs.String("to", "", "끝 (RFC3339, 제외)")
		resolution = fs.String("resolution", "all", "1m | 1h | all")
		jobID      = fs.String("job-id", "", "로그 식별자 (기본: 무작위)")
	)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errUsage
	}
	from, err1 := time.Parse(time.RFC3339, *fromS)
	to, err2 := time.Parse(time.RFC3339, *toS)
	if *tenant == "" || err1 != nil || err2 != nil {
		return errUsage
	}
	do1m, do1h := *resolution == "1m" || *resolution == "all", *resolution == "1h" || *resolution == "all"
	if !do1m && !do1h {
		return errUsage
	}
	if *jobID == "" {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			return fmt.Errorf("job id: %w", err)
		}
		*jobID = "bf-" + hex.EncodeToString(b[:])
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := rollup.OpenClickHouse(ctx, os.Getenv("MONTRACER_CH_ROLLUP_DSN"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	hourly, err := store.ForWindow(time.Hour)
	if err != nil {
		return err
	}
	opts := rollup.BackfillOptions{JobID: *jobID, Logger: logger}
	req := rollup.BackfillRequest{Tenant: *tenant, From: from, To: to}
	if do1m {
		if _, err := rollup.Backfill(ctx, store, minuteConfig(store, logger), req, opts); err != nil {
			return fmt.Errorf("1m: %w", err)
		}
	}
	if do1h {
		// 아직 닫히지 않은 시간 window는 live job 몫이다. all이면 1h 범위를 현재 시간의 시작까지로 줄인다.
		hreq := req
		if closed := time.Now().UTC().Truncate(time.Hour); *resolution == "all" && hreq.To.After(closed) {
			hreq.To = closed
		}
		if !hreq.From.Truncate(time.Hour).Before(hreq.To) {
			logger.Info("metric backfill 1h skipped: no closed hour in range", slog.String("job_id", *jobID))
			return nil
		}
		if _, err := rollup.Backfill(ctx, hourly, hourConfig(hourly, logger), hreq, opts); err != nil {
			return fmt.Errorf("1h: %w", err)
		}
	}
	return nil
}
