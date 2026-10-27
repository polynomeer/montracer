package controlapi

import (
	"context"
	"net/http"
	"time"
	"unicode"

	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/httpapi"
	"github.com/polynomeer/montracer/internal/servicehealth"
)

// 서비스 상태 API (ADR 0073). monitor 정의에서 서비스 연결을 도출해 서비스마다 가장 나쁜 상태를 준다.
//   - 걸린 monitor가 없는 서비스는 응답에 없다(화면은 "판정 없음"). 잘렸으면 meta.warnings로 알린다.
//   - 평가 전·멈춤·결측·실패를 정상과 구분한다(계약 6). 권한은 상태 조회 권한(tenant 전체)이다.

// ServiceHealthStore는 서비스 상태 조회다(controldb.MonitorStore).
type ServiceHealthStore interface {
	ServiceHealth(ctx context.Context, p authz.Principal, environment *string, now time.Time) (controldb.ServiceHealth, error)
}

// ServiceHealthItem은 서비스 하나의 합친 상태다.
type ServiceHealthItem struct {
	ServiceNamespace               *string              `json:"service_namespace"`
	ServiceName                    string               `json:"service_name"`
	Environment                    *string              `json:"environment"`
	State                          string               `json:"state"`
	Stale                          bool                 `json:"stale"`
	StateSince                     *string              `json:"state_since"`
	MonitorCount                   int                  `json:"monitor_count"`
	NameOnly                       bool                 `json:"name_only"`
	IncludesAllEnvironmentMonitors bool                 `json:"includes_all_environment_monitors"`
	Partial                        bool                 `json:"partial"`
	Monitors                       []ServiceMonitorItem `json:"monitors"`
}

// ServiceMonitorItem은 서비스에 걸린 monitor 하나다.
type ServiceMonitorItem struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	State           string `json:"state"`
	Stale           bool   `json:"stale"`
	AllEnvironments bool   `json:"all_environments"`
}

type serviceHealthResponse struct {
	Data struct {
		Services     []ServiceHealthItem  `json:"services"`
		Unattributed []ServiceMonitorItem `json:"unattributed"`
	} `json:"data"`
	Meta Meta `json:"meta"`
}

func (h *Handler) registerServiceHealth(b httpapi.Boundary) {
	h.handle("GET /api/v1/service-health", b.Handle(h.authenticated(h.serviceHealth)))
}

// validEnvironment: 1..64 byte, 제어 문자 없음(authz의 environment 규칙과 같다).
func validEnvironment(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// optional: 조건이 없으면 null, 있으면 값(빈 문자열 포함 — 속성이 없던 요청의 group).
func optional(v string, set bool) *string {
	if !set {
		return nil
	}
	return &v
}

func monitorRefItems(refs []servicehealth.MonitorRef) []ServiceMonitorItem {
	out := make([]ServiceMonitorItem, 0, len(refs))
	for _, m := range refs {
		out = append(out, ServiceMonitorItem{ID: m.ID, Name: m.Name, State: m.State, Stale: m.Stale, AllEnvironments: m.AllEnvironments})
	}
	return out
}

// serviceHealth는 GET /api/v1/service-health?environment다.
func (h *Handler) serviceHealth(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.ServiceHealth == nil {
		return apierr.New(apierr.NotFound, "서비스 상태 API를 사용할 수 없습니다")
	}
	var env *string
	if q := r.URL.Query(); q.Has("environment") {
		v := q.Get("environment")
		if !validEnvironment(v) {
			return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "environment", Reason: "must be 1..64 bytes without control characters"})
		}
		env = &v
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	sh, err := h.cfg.ServiceHealth.ServiceHealth(ctx, p, env, time.Now())
	if err != nil {
		return err
	}
	for _, w := range sh.Warnings {
		if h.cfg.ObserveServiceHealthTruncated != nil {
			h.cfg.ObserveServiceHealthTruncated(w)
		}
	}
	var resp serviceHealthResponse
	resp.Data.Services = make([]ServiceHealthItem, 0, len(sh.Services))
	for _, s := range sh.Services {
		resp.Data.Services = append(resp.Data.Services, ServiceHealthItem{
			ServiceNamespace: optional(s.Key.Namespace, s.Key.NamespaceSet), ServiceName: s.Key.Name, Environment: optional(s.Key.Environment, s.Key.EnvironmentSet),
			State: s.State, Stale: s.Stale, StateSince: tsPtr(s.StateSince), MonitorCount: s.MonitorCount,
			NameOnly: s.NameOnly, IncludesAllEnvironmentMonitors: s.IncludesAllEnvironmentMonitors, Partial: s.Partial, Monitors: monitorRefItems(s.Monitors),
		})
	}
	resp.Data.Unattributed = monitorRefItems(sh.Unattributed)
	resp.Meta = newMeta(r)
	resp.Meta.Warnings = append(resp.Meta.Warnings, sh.Warnings...)
	return writeJSON(w, http.StatusOK, resp)
}
