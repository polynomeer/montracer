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

	"github.com/polynomeer/montracer/internal/pipeline"
	"github.com/polynomeer/montracer/internal/rollup"
)

var errUsage = errors.New("usage: worker backfill --tenant UUID --from RFC3339 --to RFC3339 [--resolution 1m|1h|all] [--job-id ID]")

// backfillJob은 해상도 하나의 실행 계획이다.
type backfillJob struct {
	name string
	cfg  rollup.Config
	req  rollup.BackfillRequest
}

// backfillPlan은 검증을 마친 전체 계획이다.
type backfillPlan struct {
	jobs      []backfillJob
	jobID     string
	skipped1h bool // all인데 live 1h 재계산 구간 앞에 닫힌 시간이 없어 1h를 건너뜀
}

// planBackfill은 인자를 해석하고 모든 해상도의 범위를 **쓰기 전에** 검증한다.
// all이면 1h는 live 1h 재계산 구간 앞까지로 줄이고, 닫힌 시간이 없으면 건너뛴다. 1m은 줄이지 않는다(명시적 오류).
func planBackfill(args []string, now time.Time, opts rollup.BackfillOptions) (backfillPlan, error) {
	fs := flag.NewFlagSet("worker backfill", flag.ContinueOnError)
	fs.SetOutput(new(discard))
	var (
		tenant     = fs.String("tenant", "", "tenant UUID")
		fromS      = fs.String("from", "", "시작 (RFC3339, 포함)")
		toS        = fs.String("to", "", "끝 (RFC3339, 제외)")
		resolution = fs.String("resolution", "all", "1m | 1h | all")
		jobID      = fs.String("job-id", "", "로그 식별자 (기본: 무작위)")
	)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return backfillPlan{}, errUsage
	}
	from, err1 := time.Parse(time.RFC3339, *fromS)
	to, err2 := time.Parse(time.RFC3339, *toS)
	if *tenant == "" || err1 != nil || err2 != nil {
		return backfillPlan{}, errUsage
	}
	do1m, do1h := *resolution == "1m" || *resolution == "all", *resolution == "1h" || *resolution == "all"
	if !do1m && !do1h {
		return backfillPlan{}, errUsage
	}
	opts.Now = func() time.Time { return now }
	req := rollup.BackfillRequest{Tenant: *tenant, From: from, To: to}
	var plan backfillPlan
	if do1m {
		cfg := minuteConfig(nil, nil)
		if _, err := rollup.PlanBackfill(cfg, req, opts); err != nil {
			return backfillPlan{}, fmt.Errorf("1m: %w", err)
		}
		plan.jobs = append(plan.jobs, backfillJob{"1m", cfg, req})
	}
	if do1h {
		cfg := hourConfig(nil, nil)
		hreq := req
		if live := rollup.LiveRecomputeStart(cfg, now); *resolution == "all" && hreq.To.After(live) {
			hreq.To = live
		}
		if *resolution == "all" && !hreq.From.Truncate(time.Hour).Before(hreq.To) {
			plan.skipped1h = true // 닫힌 시간 window가 범위에 없다 — 1m만 한다
		} else {
			if _, err := rollup.PlanBackfill(cfg, hreq, opts); err != nil {
				return backfillPlan{}, fmt.Errorf("1h: %w", err)
			}
			plan.jobs = append(plan.jobs, backfillJob{"1h", cfg, hreq})
		}
	}
	plan.jobID = *jobID
	if plan.jobID == "" {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			return backfillPlan{}, fmt.Errorf("job id: %w", err)
		}
		plan.jobID = "bf-" + hex.EncodeToString(b[:])
	}
	return plan, nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// backfill은 tenant 하나의 과거 metric window를 원본에서 다시 계산한다 (ADR 0035).
// live rollup과 같은 설정(minuteConfig·hourConfig)을 쓴다.
func backfill(logger *slog.Logger, args []string) error {
	opts := rollup.BackfillOptions{RawRetention: pipeline.DefaultRetention.Metrics, Logger: logger}
	plan, err := planBackfill(args, time.Now().UTC(), opts)
	if err != nil {
		return err
	}
	opts.JobID = plan.jobID
	if plan.skipped1h {
		logger.Info("metric backfill 1h skipped: no closed hour before the live 1h recompute range", slog.String("job_id", plan.jobID))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := rollup.OpenClickHouse(ctx, os.Getenv("MONTRACER_CH_ROLLUP_DSN"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	for _, j := range plan.jobs {
		s := store
		if j.cfg.Window == time.Hour {
			if s, err = store.ForWindow(time.Hour); err != nil {
				return err
			}
		}
		j.cfg.Store, j.cfg.Logger = s, logger
		if _, err := rollup.Backfill(ctx, s, j.cfg, j.req, opts); err != nil {
			return fmt.Errorf("%s: %w", j.name, err)
		}
	}
	return nil
}
