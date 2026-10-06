package main

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// anchorReuseLimit: 이전 anchor가 이만큼 지나면 새 anchor로 새 dataset을 만든다.
// 시나리오는 anchor부터 5분이고, 수집 허용 범위는 과거 24시간이다(D02 §07).
const anchorReuseLimit = 20 * time.Hour

// newAnchor는 지금보다 10~15분 전, 5분 경계다. 시나리오(5분)가 모두 과거이고(미래 시각 없음),
// rollup의 첫 계산 범위(watermark − 10분) 안에 들어온다. 5분 경계라 metric 조회 step 300초와 맞는다.
func newAnchor(now time.Time) time.Time { return now.Truncate(5 * time.Minute).Add(-10 * time.Minute) }

func seed(ctx context.Context, cfg config) error {
	prev, ok, err := loadState(cfg.stateFile)
	if err != nil {
		return err
	}
	byName := map[string]tenantState{}
	for _, t := range prev.Tenants {
		byName[t.Name] = t
	}
	tenants, err := provision(ctx, cfg, byName)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	anchor := newAnchor(now)
	if ok && !prev.Anchor.IsZero() && now.Sub(prev.Anchor) < anchorReuseLimit {
		anchor = prev.Anchor // 같은 기준 → 같은 ID → logical 중복 없음
	}
	st := state{Anchor: anchor, Tenants: tenants}
	// key를 먼저 저장한다. 전송이 중간에 실패해도 다음 실행이 같은 key·anchor를 쓴다.
	if err := saveState(cfg.stateFile, st); err != nil {
		return err
	}

	tm, lm, mm := &ptrace.JSONMarshaler{}, &plog.JSONMarshaler{}, &pmetric.JSONMarshaler{}
	for k, dt := range demoTenants {
		ts := tenants[k]
		for from := 0; from < dt.Requests; from += batchSize {
			to := min(from+batchSize, dt.Requests)
			tb, err := tm.MarshalTraces(buildTraces(dt, anchor, from, to))
			if err != nil {
				return err
			}
			if dt.Transport == "grpc" {
				err = exportGRPC(ctx, cfg.ingressGRPC, ts.IngestToken, buildTraces(dt, anchor, from, to))
			} else {
				err = post(ctx, cfg.ingressURL, "/v1/traces", ts.IngestToken, tb)
			}
			if err != nil {
				return err
			}
			lb, err := lm.MarshalLogs(buildLogs(dt, anchor, from, to))
			if err != nil {
				return err
			}
			if err := post(ctx, cfg.ingressURL, "/v1/logs", ts.IngestToken, lb); err != nil {
				return err
			}
		}
		mb, err := mm.MarshalMetrics(buildMetrics(dt, anchor))
		if err != nil {
			return err
		}
		if err := post(ctx, cfg.ingressURL, "/v1/metrics", ts.IngestToken, mb); err != nil {
			return err
		}
		errs := 0
		for i := 0; i < dt.Requests; i++ {
			if failed(i) {
				errs++
			}
		}
		fmt.Printf("seeded %-7s tenant %s: %d requests (%d errors), %d spans via %s, metric %s\n",
			dt.Name, dt.ID, dt.Requests, errs, dt.Requests*3, transportOf(dt), metricName)
	}
	fmt.Printf("anchor %s (재실행하면 같은 데이터를 다시 보내고 dedup된다)\n", anchor.Format(time.RFC3339))
	fmt.Printf("알려진 장애 trace (acme, 느림+오류): %s\n", traceHex(demoTenants[0].ID, anchor, 0))
	fmt.Printf("key·상태: %s (로컬 전용, git 밖)\n", cfg.stateFile)
	fmt.Println("확인: make smoke")
	return nil
}
