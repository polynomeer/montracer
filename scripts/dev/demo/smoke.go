package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// smoke는 seed 결과를 공개 API로 확인한다 (D06 §11 "make smoke가 correlation·monitor·tenant 격리를 확인").
//  1. 알려진 장애 trace가 3 span·complete·오류 상태로 조회된다
//  2. 다른 tenant(globex) key로는 그 trace가 없는 trace와 같은 404다
//  3. 감사: 각 tenant는 자기 key 발급 기록만 본다
//  4. 비샘플링 metric oracle: acme 요청 1,000·오류 20 (2%) — rollup이 따라올 때까지 기다린다
//
// 아직 없는 것: log 조회 API(correlation은 trace_id를 가진 log를 수집까지만), monitor(경보 평가 미구현).
func smoke(ctx context.Context, cfg config) error {
	st, ok, err := loadState(cfg.stateFile)
	if err != nil {
		return err
	}
	if !ok || len(st.Tenants) != len(demoTenants) {
		return errors.New("seed 상태가 없다 — make seed SCENARIO=checkout 먼저")
	}
	acme, globex := st.Tenants[0], st.Tenants[1]
	anchor := st.Anchor
	// anchor는 지금보다 10분 이상 전이라 to = anchor+10분은 미래가 아니다.
	window := url.Values{"from": {anchor.Add(-10 * time.Minute).Format(time.RFC3339)}, "to": {anchor.Add(10 * time.Minute).Format(time.RFC3339)}}
	failedTrace := traceHex(acme.ID, anchor, 0)

	// 1. trace 조회 (worker가 저장할 때까지 최대 90초)
	var trace struct {
		Data struct {
			SpanCount int  `json:"span_count"`
			Complete  bool `json:"complete"`
			Spans     []struct {
				ServiceName string `json:"service_name"`
				StatusCode  string `json:"status_code"`
			} `json:"spans"`
		} `json:"data"`
	}
	if err := poll(ctx, 90*time.Second, "trace "+failedTrace, func() (bool, error) {
		code, body, err := get(ctx, cfg.queryURL+"/api/v1/traces/"+failedTrace, window, acme.APIToken)
		if err != nil || code == http.StatusNotFound {
			return false, err
		}
		if code != http.StatusOK {
			return false, fmt.Errorf("query status %d", code)
		}
		if err := json.Unmarshal(body, &trace); err != nil {
			return false, err
		}
		return trace.Data.SpanCount == 3 && trace.Data.Complete, nil
	}); err != nil {
		return err
	}
	services, errored := map[string]bool{}, false
	for _, s := range trace.Data.Spans {
		services[s.ServiceName] = true
		if strings.Contains(strings.ToLower(s.StatusCode), "error") {
			errored = true
		}
	}
	if !errored || len(services) != 3 {
		return fmt.Errorf("failed trace: services=%v errored=%v, want 3 services and error status", services, errored)
	}
	ok1("trace " + failedTrace + ": 3 span, 3 service, 오류 상태")

	// 2. tenant 격리: globex가 acme trace를 보면 안 된다(없는 trace와 같은 응답)
	other, otherBody, err := get(ctx, cfg.queryURL+"/api/v1/traces/"+failedTrace, window, globex.APIToken)
	if err != nil {
		return err
	}
	missing, missingBody, err := get(ctx, cfg.queryURL+"/api/v1/traces/"+strings.Repeat("0", 31)+"1", window, globex.APIToken)
	if err != nil {
		return err
	}
	if other != http.StatusNotFound || other != missing || errorCode(otherBody) != errorCode(missingBody) {
		return fmt.Errorf("isolation: globex reading acme trace got %d %s (unknown trace: %d)", other, errorCode(otherBody), missing)
	}
	ok1("격리: globex key로 acme trace는 없는 trace와 같은 404")

	// 3. 감사: 자기 key 발급만 보인다
	n := time.Now().UTC()
	auditWindow := url.Values{"from": {n.Add(-30 * 24 * time.Hour).Format(time.RFC3339)}, "to": {n.Add(time.Minute).Format(time.RFC3339)}, "limit": {"1000"}}
	seen := map[string]map[string]bool{}
	for _, t := range st.Tenants {
		code, body, err := get(ctx, cfg.controlURL+"/api/v1/audit-events", auditWindow, t.APIToken)
		if err != nil {
			return err
		}
		if code != http.StatusOK {
			return fmt.Errorf("audit %s: status %d", t.Name, code)
		}
		var resp struct {
			Data []struct {
				Action     string `json:"action"`
				ResourceID string `json:"resource_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return err
		}
		seen[t.Name] = map[string]bool{}
		for _, e := range resp.Data {
			seen[t.Name][e.ResourceID] = true
		}
	}
	if !seen["acme"][acme.APIKeyID] || !seen["globex"][globex.APIKeyID] || seen["globex"][acme.APIKeyID] || seen["acme"][globex.APIKeyID] {
		return errors.New("audit: each tenant must see its own key.created and not the other's")
	}
	ok1("감사: 각 tenant는 자기 key 발급 기록만 본다")

	// 4. metric oracle (rollup watermark 때문에 수 분 걸릴 수 있다)
	want := map[string]float64{"200": 980, "500": 20}
	got := map[string]float64{}
	last := ""
	// point는 끝 시각(관측 시각)으로 window에 속한다(metricagg). 분 m의 delta point(끝 anchor+(m+1)분)는
	// window anchor+(m+1)분에 있으므로 [anchor+1분, anchor+6분)을 1분 step으로 읽는다.
	from, to := anchor.Add(time.Minute), anchor.Add(6*time.Minute)
	if err := poll(ctx, 4*time.Minute, "metric rollup", func() (bool, error) {
		req := map[string]any{
			"range":        map[string]string{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)},
			"step_seconds": 60,
			"expression":   map[string]any{"metric": metricName, "aggregation": "sum", "group_by": []string{statusAttr}},
		}
		code, body, err := postJSON(ctx, cfg.queryURL+"/api/v1/query/metrics", acme.APIToken, req)
		if err != nil {
			return false, err
		}
		if code != http.StatusOK {
			return false, fmt.Errorf("metric query status %d: %s", code, body)
		}
		var resp struct {
			Data struct {
				Series []struct {
					Labels map[string]string `json:"labels"`
					Points []struct {
						V       *float64 `json:"v"`
						Reason  string   `json:"reason"`
						Partial bool     `json:"partial"`
					} `json:"points"`
				} `json:"series"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return false, err
		}
		got = map[string]float64{}
		last = fmt.Sprintf("%d series", len(resp.Data.Series))
		for _, s := range resp.Data.Series {
			for _, p := range s.Points {
				if p.V == nil || p.Partial {
					last = fmt.Sprintf("series %v: v=%v reason=%q partial=%v", s.Labels, p.V != nil, p.Reason, p.Partial)
					return false, nil // 아직 계산 중(pending·partial) — 0으로 보지 않는다(계약 6)
				}
				got[s.Labels[statusAttr]] += *p.V
			}
		}
		return got["200"] == want["200"] && got["500"] == want["500"], nil
	}); err != nil {
		return fmt.Errorf("%w (last: %s, sums %v, want %v)", err, last, got, want)
	}
	ok1(fmt.Sprintf("metric oracle: 요청 %.0f·오류 %.0f (오류율 %.2f%%)", got["200"]+got["500"], got["500"], 100*got["500"]/(got["200"]+got["500"])))
	fmt.Println("smoke 통과. 아직 확인하지 않는 것: log 조회(API 없음), monitor(경보 평가 없음)")
	return nil
}

func ok1(msg string) { fmt.Println("  ok  " + msg) }

// poll은 check가 true가 될 때까지 2초마다 다시 본다. 마지막 오류를 함께 돌려준다.
func poll(ctx context.Context, limit time.Duration, what string, check func() (bool, error)) error {
	deadline := time.Now().Add(limit)
	var last error
	for {
		done, err := check()
		if done {
			return nil
		}
		last = err
		if time.Now().After(deadline) {
			if last != nil {
				return fmt.Errorf("%s not ready within %s: %w", what, limit, last)
			}
			return fmt.Errorf("%s not ready within %s", what, limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func get(ctx context.Context, u string, params url.Values, token string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?"+params.Encode(), nil)
	if err != nil {
		return 0, nil, err
	}
	return do(req, token)
}

func postJSON(ctx context.Context, u, token string, body any) (int, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(req, token)
}

func do(req *http.Request, token string) (int, []byte, error) {
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s unreachable (make dev 실행 중인가?): %w", req.URL.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b, err
}

func errorCode(body []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return e.Error.Code
}
