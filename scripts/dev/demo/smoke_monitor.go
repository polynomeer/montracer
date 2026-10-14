package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// 6. monitor oracle (D06 §05 "2% 경계 시험", ADR 0052·0054): seed는 분마다 요청 200·오류 4라 어느 5분 window도 정확히 2%다.
//   - 실제 수집 경로(ingress → Kafka → worker → rollup)를 거친 데이터로 control-api validate의 24시간 dry-run을 돌린다.
//     오류율 > 2%는 발화하지 않고(정확히 2%는 위반이 아니다), > 1.99%는 발화한다. 최대값은 2%다.
//   - seed 뒤 새 데이터가 없으므로 rollup watermark는 anchor+5분에 멈춘다. 10분(StaleAfter)이 지나면 dry-run 이유는 stale_watermark가
//     되지만 group 값은 계산된다 — 끝난 seed 데이터의 정상 상태다. 그래서 이유는 ""와 stale_watermark를 받고, 다른 이유는 실패다.
//   - 실제 monitor를 만들어 alert-worker가 평가하는지 본다. 평가 window 끝은 min(평가 시각, watermark)다.
//     watermark가 멈춘 지 10분 안이면 window [anchor, anchor+5분)이 seed 전부(1,000·20 = 2% > 1.99%)라 ALERT,
//     10분이 지났으면 결측(stale_watermark)이라 NO_DATA다. 평가 시각(slot)으로 기대값을 정하고, 경계 ±2분은 둘 다 받는다.
//     정상(OK)·평가 오류·다른 이유의 결측은 실패다(계약 6). 끝나면 지운다(못 지우면 경고).
//
// seed가 24시간보다 오래됐으면 dry-run 범위 밖이라 건너뛴다(그렇다고 적는다).
func smokeMonitors(ctx context.Context, cfg config, acme tenantState, anchor time.Time) error {
	if time.Since(anchor) > 23*time.Hour {
		fmt.Println("  skip monitor oracle: seed 시각이 24시간보다 오래돼 dry-run 범위 밖이다(seed 상태 파일을 지우고 다시 seed)")
		return nil
	}
	spec := smokeMonitorSpec
	type group struct {
		FiringIntervals        []json.RawMessage `json:"firing_intervals"`
		FiringIntervalsOmitted int               `json:"firing_intervals_omitted"`
		MaxValue               *float64          `json:"max_value"`
		Evaluated              int               `json:"evaluated"`
	}
	dryRun := func(threshold float64) (group, error) {
		code, body, err := postJSON(ctx, cfg.controlURL+"/api/v1/monitors/validate", acme.APIToken, spec("smoke dry-run", threshold))
		if err != nil {
			return group{}, err
		}
		if code != http.StatusOK {
			return group{}, fmt.Errorf("validate status %d: %s", code, body)
		}
		var resp struct {
			Data struct {
				Warnings []string `json:"warnings"`
				DryRun   *struct {
					Reason string  `json:"reason"`
					Groups []group `json:"groups"`
				} `json:"dry_run"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return group{}, err
		}
		dr := resp.Data.DryRun
		if dr == nil {
			return group{}, fmt.Errorf("dry-run 없음(warnings %v) — control-api에 MONTRACER_CH_QUERY_DSN이 있는가", resp.Data.Warnings)
		}
		if dr.Reason != "" && dr.Reason != "stale_watermark" {
			return group{}, fmt.Errorf("dry-run reason %q (no_watermark·no_data면 rollup이 seed를 따라오지 않았다)", dr.Reason)
		}
		if len(dr.Groups) != 1 {
			return group{}, fmt.Errorf("dry-run groups = %d, want 1 (checkout)", len(dr.Groups))
		}
		return dr.Groups[0], nil
	}
	at, err := dryRun(0.02)
	if err != nil {
		return fmt.Errorf("monitor dry-run(> 2%%): %w", err)
	}
	if n := len(at.FiringIntervals) + at.FiringIntervalsOmitted; n != 0 || at.MaxValue == nil || *at.MaxValue != 0.02 || at.Evaluated == 0 {
		return fmt.Errorf("monitor dry-run(> 2%%): firing %d, max %v, evaluated %d — want no firing at exactly 2%%", n, at.MaxValue, at.Evaluated)
	}
	below, err := dryRun(0.0199)
	if err != nil {
		return fmt.Errorf("monitor dry-run(> 1.99%%): %w", err)
	}
	if len(below.FiringIntervals)+below.FiringIntervalsOmitted == 0 {
		return errors.New("monitor dry-run(> 1.99%): no firing — 2% should violate a 1.99% threshold")
	}
	ok1(fmt.Sprintf("monitor dry-run 경계: 정확히 2%%는 > 2%%에서 발화 없음(평가 %d회, 최대 2%%), > 1.99%%에서 발화", at.Evaluated))

	// 실제 monitor: alert-worker가 평가하고, 결과를 정상으로 보이지 않는다
	code, body, err := postJSONWith(ctx, http.MethodPost, cfg.controlURL+"/api/v1/monitors", acme.APIToken, spec("smoke checkout 오류율", 0.0199),
		map[string]string{"Idempotency-Key": "smoke-" + strconv.FormatInt(time.Now().UnixNano(), 10)})
	if err != nil {
		return err
	}
	if code != http.StatusCreated {
		return fmt.Errorf("create monitor status %d: %s", code, body)
	}
	var created struct {
		Data struct {
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return fmt.Errorf("create monitor response: %w", err)
	}
	if created.Data.ID == "" {
		return fmt.Errorf("create monitor: no id in %s", body)
	}
	monitorURL := cfg.controlURL + "/api/v1/monitors/" + created.Data.ID
	defer func() {
		// 지우기는 smoke 결과를 바꾸지 않지만, 못 지우면 남은 monitor가 계속 평가되므로 알린다(id만, token은 출력하지 않는다)
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		code, _, err := postJSONWith(dctx, http.MethodDelete, monitorURL, acme.APIToken, nil, map[string]string{"If-Match": `"` + strconv.FormatInt(created.Data.Revision, 10) + `"`})
		if err != nil || code != http.StatusNoContent {
			fmt.Printf("  warn smoke monitor %s를 지우지 못했다(status %d, %v) — 직접 지우세요\n", created.Data.ID, code, err)
		}
	}()
	var state, evalStatus, evalReason string
	var evalSlot time.Time
	if err := poll(ctx, 3*time.Minute, "monitor evaluation", func() (bool, error) {
		code, body, err := get(ctx, monitorURL, nil, acme.APIToken)
		if err != nil {
			return false, err
		}
		if code != http.StatusOK {
			return false, fmt.Errorf("get monitor status %d: %s", code, body)
		}
		var m struct {
			Data struct {
				Status *struct {
					State          string `json:"state"`
					LastEvaluation *struct {
						Slot   time.Time `json:"slot"`
						Status string    `json:"status"`
						Reason *string   `json:"reason"`
					} `json:"last_evaluation"`
				} `json:"status"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			return false, err
		}
		if m.Data.Status == nil {
			return false, errors.New("status null — key에 telemetry.read가 있고 environment 제한이 없어야 한다")
		}
		if m.Data.Status.LastEvaluation == nil {
			return false, nil // 첫 평가 전
		}
		e := m.Data.Status.LastEvaluation
		state, evalStatus, evalSlot = m.Data.Status.State, e.Status, e.Slot
		if e.Reason != nil {
			evalReason = *e.Reason
		}
		return true, nil
	}); err != nil {
		return err
	}
	// watermark(= anchor+5분)가 멈춘 지 얼마나 됐는지로 기대값을 정한다(alert-worker StaleAfter 10분, 경계 ±2분은 둘 다)
	stalled := evalSlot.Sub(anchor.Add(5 * time.Minute))
	alert := state == "ALERT" && evalStatus == "evaluated"
	stale := state == "NO_DATA" && evalReason == "stale_watermark"
	switch {
	case stalled < 8*time.Minute && !alert:
		return fmt.Errorf("monitor evaluated as %s (%s %q) %s after the seed watermark — want ALERT (seed window 2%% > 1.99%%)", state, evalStatus, evalReason, stalled)
	case stalled > 12*time.Minute && !stale:
		return fmt.Errorf("monitor evaluated as %s (%s %q) %s after the seed watermark — want NO_DATA (stale_watermark), never OK", state, evalStatus, evalReason, stalled)
	case !alert && !stale:
		return fmt.Errorf("monitor evaluated as %s (%s %q) — want ALERT or NO_DATA (stale_watermark), never OK", state, evalStatus, evalReason)
	}
	ok1(fmt.Sprintf("monitor 평가: alert-worker 상태 %s(평가 %s %s, watermark가 멈춘 지 %s) — 정상으로 보이지 않는다", state, evalStatus, evalReason, stalled.Round(time.Second)))
	return nil
}

// smokeMonitorSpec은 checkout 5분 오류율 > threshold monitor다(최소 100 요청, 유지 없이 바로, 결측은 데이터 없음).
func smokeMonitorSpec(name string, threshold float64) map[string]any {
	return map[string]any{
		"name":           name,
		"query":          map[string]any{"kind": "error_ratio", "filter": map[string]any{"op": "and", "args": []any{map[string]any{"op": "eq", "field": "service.name", "value": "checkout"}}}},
		"window_seconds": 300, "evaluation_seconds": 60,
		"condition":        map[string]any{"operator": "gt", "threshold": threshold},
		"minimum_requests": 100, "for_seconds": 0, "no_data": map[string]any{"action": "no_data"},
	}
}

// postJSONWith는 method·추가 header로 JSON을 보낸다(body nil이면 본문 없음).
func postJSONWith(ctx context.Context, method, u, token string, body any, headers map[string]string) (int, []byte, error) {
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if rd != nil {
		req, err = http.NewRequestWithContext(ctx, method, u, rd)
	}
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return do(req, token)
}
