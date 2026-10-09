package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/polynomeer/montracer/internal/alerting"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/monitor"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// DryRunMetrics는 24시간 dry-run의 metric 조회다 (telemetrystore.QueryStore, query 계정·row policy).
type DryRunMetrics interface {
	MetricBuckets(ctx context.Context, p authz.Principal, q telemetrystore.MetricQuery, now time.Time) ([]telemetrystore.MetricBucket, error)
	RollupWatermark(ctx context.Context, p authz.Principal, window time.Duration, since, now time.Time) (time.Time, error)
}

// dry-run을 하지 못한 이유 (warnings). dry_run은 null이다 — 결과가 "발화 없음"처럼 보이지 않게 한다(계약 6).
const (
	warnDryRunUnavailable = monitor.WarnDryRunUnavailable // 이 배포에 분석 저장소 조회가 없다
	warnDryRunForbidden   = "dry_run_forbidden"           // 요청자에게 telemetry.read가 없다
	warnDryRunFailed      = "dry_run_failed"              // 조회 실패·시간 초과
	warnDryRunTooLarge    = "dry_run_too_large"           // series·window가 커서 예산을 넘는다
	warnDryRunBusy        = "dry_run_busy"                // 동시 dry-run 수 상한(CPU 보호). 잠시 뒤 다시 validate
)

// dryRun은 요청자 권한으로 지난 24시간을 다시 평가한다 (D05 §09, ADR 0052). 실패해도 validate는 200이다(spec은 유효하다).
func (h *Handler) dryRun(ctx context.Context, p authz.Principal, s monitor.Spec) (json.RawMessage, string) {
	out, warn, d := h.runDryRun(ctx, p, s)
	if h.cfg.ObserveDryRun != nil {
		outcome := "ok"
		if warn != "" {
			outcome = warn[len("dry_run_"):]
		}
		h.cfg.ObserveDryRun(outcome, d)
	}
	return out, warn
}

func (h *Handler) runDryRun(ctx context.Context, p authz.Principal, s monitor.Spec) (json.RawMessage, string, time.Duration) {
	if h.cfg.Metrics == nil {
		return nil, warnDryRunUnavailable, 0
	}
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, warnDryRunForbidden, 0
	}
	select {
	case h.dryRunSlots <- struct{}{}:
		defer func() { <-h.dryRunSlots }()
	default:
		return nil, warnDryRunBusy, 0
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, h.cfg.DryRunTimeout)
	defer cancel()
	now := h.cfg.Now()
	fail := func(stage string, err error) (json.RawMessage, string, time.Duration) {
		var budget interface{ BudgetExceeded() map[string]any }
		if errors.As(err, &budget) {
			return nil, warnDryRunTooLarge, time.Since(started)
		}
		h.cfg.Logger.Warn("monitor dry-run failed", slog.String("stage", stage), slog.String("error", err.Error()))
		return nil, warnDryRunFailed, time.Since(started)
	}
	wm, err := h.cfg.Metrics.RollupWatermark(ctx, p, time.Minute, now.Add(-alerting.DryRunRange-alerting.DryRunWarmup(s)), now)
	if err != nil {
		return fail("watermark", err)
	}
	// 조회마다 interactive 기본 예산(결과 10,000행·5초)보다 큰 예산을 싣는다: 1분 step 24시간은 series 하나가 약 1,440행이다
	plan := alerting.PlanDryRun(s, now, wm, telemetrystore.Budget{MaxResultRows: alerting.DryRunMaxBuckets, MaxExecutionTime: h.cfg.DryRunTimeout})
	var buckets []telemetrystore.MetricBucket
	for _, q := range plan.Queries {
		bs, err := h.cfg.Metrics.MetricBuckets(ctx, p, q, now)
		if err != nil {
			return fail("query", err)
		}
		buckets = append(buckets, bs...)
		if len(buckets) > alerting.DryRunMaxBuckets {
			return nil, warnDryRunTooLarge, time.Since(started)
		}
	}
	if alerting.DryRunMerges(s, plan, buckets) > alerting.DryRunMaxMerges {
		return nil, warnDryRunTooLarge, time.Since(started)
	}
	r, err := alerting.DryRun(ctx, s, plan, buckets)
	if err != nil {
		return fail("evaluate", err)
	}
	out, err := json.Marshal(r)
	if err != nil {
		return fail("encode", err)
	}
	return out, "", time.Since(started)
}
