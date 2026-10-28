package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/servicehealth"
)

// fakeServiceHealth는 저장소처럼 상태 조회 권한을 먼저 본다(controldb.MonitorStore.ServiceHealth와 같은 순서).
type fakeServiceHealth struct {
	out    controldb.ServiceHealth
	gotEnv *string
}

func (f *fakeServiceHealth) ServiceHealth(_ context.Context, p authz.Principal, env *string, _ time.Time) (controldb.ServiceHealth, error) {
	if err := controldb.AuthorizeMonitorState(p); err != nil {
		return controldb.ServiceHealth{}, err
	}
	f.gotEnv = env
	return f.out, nil
}

func serviceHealthHandler(t *testing.T, store ServiceHealthStore, ps principals, truncated *[]string) *Handler {
	t.Helper()
	signer, err := apicursor.NewSigner([]byte("0123456789abcdef0123456789abcdef"), time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Authenticate: func(_ context.Context, token string) (authz.Principal, error) {
			if p, ok := ps[token]; ok {
				return p, nil
			}
			return authz.Principal{}, authz.ErrUnauthenticated
		},
		Audit: &fakeAudit{}, Cursor: signer, Now: func() time.Time { return now }, ServiceHealth: store,
	}
	if truncated != nil {
		cfg.ObserveServiceHealthTruncated = func(r string) { *truncated = append(*truncated, r) }
	}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// 서비스 상태: 합친 상태·시작 시각·전체 환경 표시, 서비스를 알 수 없는 monitor, 잘림 경고와 지표 (ADR 0073)
func TestServiceHealth(t *testing.T) {
	since := now.Add(-30 * time.Minute)
	store := &fakeServiceHealth{out: controldb.ServiceHealth{
		Services: []servicehealth.Service{{
			Key: servicehealth.Key{Name: "checkout", Environment: "prod", EnvironmentSet: true}, State: "ALERT", StateSince: &since, MonitorCount: 2, NameOnly: true,
			IncludesAllEnvironmentMonitors: true, Partial: true,
			Monitors: []servicehealth.MonitorRef{{ID: "m1", Name: "checkout 오류율", State: "ALERT"}, {ID: "m2", Name: "전체 p95", State: "OK", AllEnvironments: true}},
		}},
		Unattributed: []servicehealth.MonitorRef{{ID: "m3", Name: "서비스별 오류율", State: "EVALUATION_ERROR"}},
		Warnings:     []string{controldb.WarnMonitorsTruncated},
	}}
	var truncated []string
	h := serviceHealthHandler(t, store, principals{"dev": user(t, authz.RoleDeveloper)}, &truncated)
	rec := do(t, h, http.MethodGet, "/api/v1/service-health?environment=prod", "dev", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		Data struct {
			Services     []ServiceHealthItem  `json:"services"`
			Unattributed []ServiceMonitorItem `json:"unattributed"`
		} `json:"data"`
		Meta Meta `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	s := resp.Data.Services[0]
	if s.ServiceNamespace != nil || s.ServiceName != "checkout" || *s.Environment != "prod" || s.State != "ALERT" || *s.StateSince != ts(since) ||
		!s.IncludesAllEnvironmentMonitors || !s.NameOnly || !s.Partial || len(s.Monitors) != 2 || !s.Monitors[1].AllEnvironments {
		t.Errorf("service = %+v", s)
	}
	if len(resp.Data.Unattributed) != 1 || resp.Data.Unattributed[0].State != "EVALUATION_ERROR" {
		t.Errorf("unattributed = %+v", resp.Data.Unattributed)
	}
	if strings.Join(resp.Meta.Warnings, ",") != controldb.WarnMonitorsTruncated || strings.Join(truncated, ",") != controldb.WarnMonitorsTruncated {
		t.Errorf("warnings = %v, metric = %v", resp.Meta.Warnings, truncated)
	}
	if store.gotEnv == nil || *store.gotEnv != "prod" {
		t.Errorf("environment not passed: %v", store.gotEnv)
	}
	// environment 없음은 nil(모든 환경), 빈 결과는 빈 배열(null 아님)
	store.out = controldb.ServiceHealth{}
	rec = do(t, h, http.MethodGet, "/api/v1/service-health", "dev", "", nil)
	if rec.Code != http.StatusOK || store.gotEnv != nil || !strings.Contains(rec.Body.String(), `"services":[]`) || !strings.Contains(rec.Body.String(), `"unattributed":[]`) {
		t.Errorf("empty: %d %s", rec.Code, rec.Body)
	}
}

// 잘못된 environment는 400, environment 제한 key는 403(빈 결과가 아님), flag가 꺼지면 404
func TestServiceHealthRejects(t *testing.T) {
	store := &fakeServiceHealth{}
	h := serviceHealthHandler(t, store, principals{"dev": user(t, authz.RoleDeveloper), "envkey": monitorKey(t, []string{"prod"}, authz.TelemetryRead)}, nil)
	for _, q := range []string{"?environment=", "?environment=" + strings.Repeat("a", 65), "?environment=a%0Ab"} {
		if rec := do(t, h, http.MethodGet, "/api/v1/service-health"+q, "dev", "", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/service-health", "envkey", "", nil); rec.Code != http.StatusForbidden {
		t.Errorf("environment-restricted key: %d %s", rec.Code, rec.Body)
	}
	off := serviceHealthHandler(t, nil, principals{"dev": user(t, authz.RoleDeveloper)}, nil)
	if rec := do(t, off, http.MethodGet, "/api/v1/service-health", "dev", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("flag off: %d", rec.Code)
	}
}
