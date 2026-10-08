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
	"slices"
	"strings"
	"time"
)

// smoke는 seed 결과를 공개 API로 확인한다 (D06 §11 "make smoke가 correlation·monitor·tenant 격리를 확인").
//
//  1. 알려진 장애 trace가 3 span·complete·오류 상태로 조회된다
//
//  2. 다른 tenant(globex) key로는 그 trace가 없는 trace와 같은 404다
//
//  3. 감사: 각 tenant는 자기 key 발급 기록만 본다
//
//  4. 비샘플링 metric oracle: acme 요청 1,000·오류 20 (2%) — rollup이 따라올 때까지 기다린다
//
//  5. metric 사전(GET /api/v1/metrics, /metrics/labels)에 oracle metric의 유형·허용 연산·label key가 있다
//
//     1a. 장애 trace의 log를 trace_id로 검색한다(log↔trace 연결, POST /api/v1/query/logs)
//     1d. 서비스 catalog로 service.name 검색(ADR 0039)
//
// 아직 없는 것: monitor(경보 평가 미구현).
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

	// 1a. log↔trace 연결: 장애 trace의 log를 trace_id로 찾는다(root info 1건 + payment error 1건)
	logReq := map[string]any{
		"range":  map[string]string{"from": window.Get("from"), "to": window.Get("to")},
		"filter": map[string]any{"field": "trace_id", "op": "eq", "value": failedTrace},
	}
	var logs struct {
		Data []struct {
			Severity int    `json:"severity_number"`
			Body     string `json:"body"`
		} `json:"data"`
	}
	if err := poll(ctx, 90*time.Second, "logs of trace "+failedTrace, func() (bool, error) {
		code, body, err := postJSON(ctx, cfg.queryURL+"/api/v1/query/logs", acme.APIToken, logReq)
		if err != nil {
			return false, err
		}
		if code != http.StatusOK {
			return false, fmt.Errorf("log search status %d: %s", code, body)
		}
		if err := json.Unmarshal(body, &logs); err != nil {
			return false, err
		}
		return len(logs.Data) == 2, nil
	}); err != nil {
		return err
	}
	errLogs := 0
	for _, l := range logs.Data {
		if l.Severity >= 17 {
			errLogs++
		}
	}
	if errLogs != 1 {
		return fmt.Errorf("logs of failed trace: %d error logs, want 1 (%+v)", errLogs, logs.Data)
	}
	ok1("log 연결: 장애 trace의 log 2건(error 1건)을 trace_id로 검색")

	// 1a'. trace 검색(ADR 0043): root 서비스의 오류 trace를 찾으면 오류 요청 수(20)와 같고, 모두 3 span·완전·root 서비스 이름이 있다.
	// 시나리오 범위 [anchor, anchor+6분)에 요청 1,000개가 모두 들어 있다.
	acmeService := demoTenants[0].Service
	traceReq := map[string]any{
		"range": map[string]string{"from": anchor.Format(time.RFC3339), "to": anchor.Add(6 * time.Minute).Format(time.RFC3339)},
		"filter": map[string]any{"op": "and", "args": []any{
			map[string]any{"field": "service.name", "op": "eq", "value": acmeService},
			map[string]any{"field": "has_error", "op": "eq", "value": true},
		}},
		"limit": 100,
	}
	var found struct {
		Data []struct {
			TraceID     string  `json:"trace_id"`
			SpanCount   int     `json:"span_count"`
			Complete    bool    `json:"complete"`
			HasError    bool    `json:"has_error"`
			RootService *string `json:"root_service"`
			DurationMs  float64 `json:"duration_ms"`
		} `json:"data"`
	}
	if err := poll(ctx, 90*time.Second, "trace search", func() (bool, error) {
		code, body, err := postJSON(ctx, cfg.queryURL+"/api/v1/query/traces", acme.APIToken, traceReq)
		if err != nil {
			return false, err
		}
		if code != http.StatusOK {
			return false, fmt.Errorf("trace search status %d: %s", code, body)
		}
		if err := json.Unmarshal(body, &found); err != nil {
			return false, err
		}
		return len(found.Data) == 20, nil
	}); err != nil {
		return fmt.Errorf("%w (found %d error traces, want 20)", err, len(found.Data))
	}
	sawFailed := false
	for _, f := range found.Data {
		if f.SpanCount != 3 || !f.Complete || !f.HasError || f.RootService == nil || *f.RootService != acmeService || f.DurationMs < 2000 {
			return fmt.Errorf("trace search row %+v: want 3 complete spans, error, root %s, slow (2.5s)", f, acmeService)
		}
		sawFailed = sawFailed || f.TraceID == failedTrace
	}
	if !sawFailed {
		return fmt.Errorf("trace search did not return the failed trace %s", failedTrace)
	}
	ok1("trace 검색: " + acmeService + " 오류 trace 20개(오류 요청 수와 같음), 모두 3 span·완전")

	// 1b. OTLP/gRPC로 받은 globex trace도 같은 경로로 조회된다
	grpcTrace := traceHex(globex.ID, anchor, 0)
	if err := poll(ctx, 90*time.Second, "grpc trace "+grpcTrace, func() (bool, error) {
		code, body, err := get(ctx, cfg.queryURL+"/api/v1/traces/"+grpcTrace, window, globex.APIToken)
		if err != nil || code == http.StatusNotFound {
			return false, err
		}
		if code != http.StatusOK {
			return false, fmt.Errorf("query status %d", code)
		}
		var t struct {
			Data struct {
				SpanCount int  `json:"span_count"`
				Complete  bool `json:"complete"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &t); err != nil {
			return false, err
		}
		return t.Data.SpanCount == 3 && t.Data.Complete, nil
	}); err != nil {
		return err
	}
	ok1("trace " + grpcTrace + ": OTLP/gRPC로 받은 globex trace 3 span 조회")

	// 1c. 서비스 catalog: ingress가 ACK한 요청의 서비스가 등록된다(비동기, 수 초)
	wantServices := map[string]bool{"checkout": false, "payment": false, "database": false}
	if err := poll(ctx, 60*time.Second, "service catalog", func() (bool, error) {
		code, body, err := get(ctx, cfg.queryURL+"/api/v1/services", url.Values{"environment": {"prod"}}, acme.APIToken)
		if err != nil {
			return false, err
		}
		if code != http.StatusOK {
			return false, fmt.Errorf("services status %d", code)
		}
		var resp struct {
			Data []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return false, err
		}
		for _, sv := range resp.Data {
			if _, ok := wantServices[sv.Name]; ok && sv.Status == "active" {
				wantServices[sv.Name] = true
			}
		}
		for _, seen := range wantServices {
			if !seen {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("%w (seen %v)", err, wantServices)
	}
	ok1("서비스 catalog: checkout·payment·database 등록(active)")

	// 1d. service.name 검색(ADR 0039): 장애 trace의 log 중 Payment(대소문자 무시) 서비스 것만 — error 1건
	byService := map[string]any{
		"range": map[string]string{"from": window.Get("from"), "to": window.Get("to")},
		"filter": map[string]any{"op": "and", "args": []any{
			map[string]any{"field": "trace_id", "op": "eq", "value": failedTrace},
			map[string]any{"field": "service.name", "op": "eq", "value": "Payment"},
		}},
	}
	code, body, err := postJSON(ctx, cfg.queryURL+"/api/v1/query/logs", acme.APIToken, byService)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, &logs); code != http.StatusOK || err != nil || len(logs.Data) != 1 || logs.Data[0].Severity < 17 {
		return fmt.Errorf("log search by service.name: status %d, %s", code, body)
	}
	ok1("service.name 검색: 장애 trace의 payment log 1건(error)")

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

	// 4. metric oracle (rollup watermark 때문에 수 분 걸릴 수 있다). counter 합과 duration histogram의 count가
	// 같은 요청 수·오류 수여야 한다(서비스 상세 RED는 histogram을 쓴다, ADR 0042).
	// 마지막은 범위 전체 한 점(step = 범위 길이, ADR 0042) — step 원점이 범위 시작인지 확인한다.
	for _, o := range []struct {
		metric, agg, label string
		step               int
	}{
		{metricName, "sum", "counter", 60},
		{durationMetric, "count", "histogram", 60},
		{durationMetric, "count", "histogram, 범위 전체 한 점", 300},
	} {
		want := map[string]float64{"200": 980, "500": 20}
		got := map[string]float64{}
		last := ""
		// point는 끝 시각(관측 시각)으로 window에 속한다(metricagg). 분 m의 delta point(끝 anchor+(m+1)분)는
		// window anchor+(m+1)분에 있으므로 [anchor+1분, anchor+6분)을 1분 step으로 읽는다.
		from, to := anchor.Add(time.Minute), anchor.Add(6*time.Minute)
		if err := poll(ctx, 4*time.Minute, "metric rollup "+o.metric, func() (bool, error) {
			req := map[string]any{
				"range":        map[string]string{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)},
				"step_seconds": o.step,
				"expression":   map[string]any{"metric": o.metric, "aggregation": o.agg, "group_by": []string{statusAttr}},
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
				if want := 300 / o.step; len(s.Points) != want {
					return false, fmt.Errorf("series %v: %d points, want %d (step 원점이 범위 시작이 아님)", s.Labels, len(s.Points), want)
				}
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
			return fmt.Errorf("%s: %w (last: %s, sums %v, want %v)", o.metric, err, last, got, want)
		}
		ok1(fmt.Sprintf("metric oracle(%s): 요청 %.0f·오류 %.0f (오류율 %.2f%%)", o.label, got["200"]+got["500"], got["500"], 100*got["500"]/(got["200"]+got["500"])))
	}
	// 5. metric 사전(ADR 0046): oracle 두 metric이 유형·연산과 함께 보이고, histogram의 label key에 status가 있다
	if err := smokeMetricCatalog(ctx, cfg, acme.APIToken, anchor); err != nil {
		return err
	}
	ok1("metric 사전: counter·histogram 유형과 허용 연산, label key")
	fmt.Println("smoke 통과. 아직 확인하지 않는 것: monitor(경보 평가 없음)")
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

func smokeMetricCatalog(ctx context.Context, cfg config, token string, anchor time.Time) error {
	rng := url.Values{"from": {anchor.Format(time.RFC3339)}, "to": {anchor.Add(10 * time.Minute).Format(time.RFC3339)}}
	q := url.Values{"q": {"."}}
	for k, v := range rng {
		q[k] = v
	}
	code, body, err := get(ctx, cfg.queryURL+"/api/v1/metrics", q, token)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("metric catalog status %d: %s", code, body)
	}
	var cat struct {
		Data []struct {
			Name     string `json:"name"`
			Variants []struct {
				Type         string   `json:"type"`
				Monotonic    bool     `json:"monotonic"`
				Aggregations []string `json:"aggregations"`
			} `json:"variants"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &cat); err != nil {
		return err
	}
	type expect struct {
		typ       string
		monotonic bool
		agg       string
	}
	want := map[string]expect{metricName: {"sum", true, "rate"}, durationMetric: {"histogram", false, "p95"}}
	found := 0
	for _, m := range cat.Data {
		w, ok := want[m.Name]
		if !ok {
			continue
		}
		if len(m.Variants) != 1 {
			return fmt.Errorf("metric catalog %s: %d variants, want 1", m.Name, len(m.Variants))
		}
		v := m.Variants[0]
		if v.Type != w.typ || v.Monotonic != w.monotonic || !slices.Contains(v.Aggregations, w.agg) {
			return fmt.Errorf("metric catalog %s = %+v, want %+v", m.Name, v, w)
		}
		found++
	}
	if found != len(want) {
		return fmt.Errorf("metric catalog: found %d of %v", found, want)
	}
	lq := url.Values{"metric": {durationMetric}}
	for k, v := range rng {
		lq[k] = v
	}
	code, body, err = get(ctx, cfg.queryURL+"/api/v1/metrics/labels", lq, token)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("metric labels status %d: %s", code, body)
	}
	var labels struct {
		Data struct {
			Keys []struct {
				Key string `json:"key"`
			} `json:"keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &labels); err != nil {
		return err
	}
	for _, k := range labels.Data.Keys {
		if k.Key == statusAttr {
			return nil
		}
	}
	return fmt.Errorf("metric labels of %s: no %s in %+v", durationMetric, statusAttr, labels.Data.Keys)
}
