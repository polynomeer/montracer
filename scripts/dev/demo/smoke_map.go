package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// smokeServiceMap은 seed trace로 만든 service map을 확인한다 (ADR 0062).
//
// seed 체인은 checkout(server) → payment(server, 부모가 checkout server span) → database 서비스의 postgres client span이다.
//   - checkout → payment: client span이 없으므로 server span fallback으로 센다. 요청 1,000·오류 20(payment 오류).
//   - database → postgresql: 짝이 없는 client span(db.system.name). 요청 1,000·오류 0.
//
// seed를 두 번 보냈으므로 같은 span의 중복 행을 한 번만 세는지도 함께 본다(1,000이지 2,000이 아니다).
func smokeServiceMap(ctx context.Context, cfg config, token string, anchor time.Time) error {
	type node struct {
		NodeID string `json:"node_id"`
		Kind   string `json:"kind"`
		Name   string `json:"name"`
	}
	type edge struct {
		Source      string  `json:"source"`
		Destination string  `json:"destination"`
		Connection  string  `json:"connection"`
		Confidence  string  `json:"confidence"`
		Requests    *uint64 `json:"requests"`
		Errors      *uint64 `json:"errors"`
		Latency     *struct {
			P95Ms *float64 `json:"p95_ms"`
		} `json:"latency"`
	}
	req := map[string]any{
		"range":       map[string]string{"from": anchor.Format(time.RFC3339), "to": anchor.Add(6 * time.Minute).Format(time.RFC3339)},
		"environment": "prod",
	}
	last := ""
	return poll(ctx, 4*time.Minute, "service map", func() (bool, error) {
		code, body, err := postJSON(ctx, cfg.queryURL+"/api/v1/service-map/query", token, req)
		if err != nil {
			return false, err
		}
		if code != http.StatusOK {
			return false, fmt.Errorf("service map status %d: %s", code, body)
		}
		var resp struct {
			Data struct {
				Nodes []node `json:"nodes"`
				Edges []edge `json:"edges"`
			} `json:"data"`
			Meta struct {
				Sampled *bool `json:"sampled"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return false, err
		}
		names := map[string]node{}
		for _, n := range resp.Data.Nodes {
			names[n.NodeID] = n
		}
		find := func(src, dst string) *edge {
			for i, e := range resp.Data.Edges {
				if names[e.Source].Name == src && names[e.Destination].Name == dst {
					return &resp.Data.Edges[i]
				}
			}
			return nil
		}
		call, db := find("checkout", "payment"), find("database", "postgresql")
		last = fmt.Sprintf("%d nodes, %d edges", len(resp.Data.Nodes), len(resp.Data.Edges))
		if call == nil || db == nil || call.Requests == nil || db.Requests == nil {
			return false, nil
		}
		last = fmt.Sprintf("checkout→payment %d/%d, database→postgresql %d", *call.Requests, *call.Errors, *db.Requests)
		if *call.Requests < 1000 || *db.Requests < 1000 {
			return false, nil // 아직 계산 중인 window가 있다
		}
		switch {
		case *call.Requests != 1000 || *call.Errors != 20 || *db.Requests != 1000 || *db.Errors != 0:
			return false, fmt.Errorf("edge counts: %s (want 1000/20, 1000/0 — 중복 행을 두 번 셌다)", last)
		case call.Confidence != "observed" || call.Connection != "rpc" || names[db.Destination].Kind != "database":
			return false, fmt.Errorf("edge shape: %+v %+v", *call, names[db.Destination])
		case call.Latency == nil || call.Latency.P95Ms == nil:
			return false, fmt.Errorf("checkout→payment latency missing")
		case resp.Meta.Sampled == nil || !*resp.Meta.Sampled:
			return false, fmt.Errorf("meta.sampled must be true (span 표본 기반)")
		}
		fmt.Println("  ok  service map: checkout→payment 요청 1,000·오류 20(server fallback), database→postgresql 1,000(외부 DB), 중복 행 한 번")
		return true, nil
	})
}
