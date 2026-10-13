package controlapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

func createdMonitor(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/api/v1/monitors", "dev", monitorBody, map[string]string{"Idempotency-Key": "s1"})
	env := decodeMonitor(t, rec)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	return env.Data.ID
}

// 목록·단건은 상태 요약을 싣는다. 요약이 없는 monitor(일정 없음)는 OK가 아니라 NOT_EVALUATED다.
func TestMonitorStatusInList(t *testing.T) {
	store := newFakeMonitors()
	h := monitorHandler(t, store, principals{"dev": user(t, authz.RoleDeveloper)})
	id := createdMonitor(t, h)
	reason := "query_failed"
	store.statuses = map[string]controldb.MonitorStatus{id: {State: "EVALUATION_ERROR", GroupsByState: map[string]int{"EVALUATION_ERROR": 2}, Stale: true,
		LastEvaluation: &controldb.EvaluationSummary{Slot: now.Add(-time.Hour), Status: "error", Reason: &reason}}}
	var list struct {
		Data []MonitorItem `json:"data"`
	}
	rec := do(t, h, http.MethodGet, "/api/v1/monitors", "dev", "", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Data) != 1 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	st := list.Data[0].Status
	if st == nil || st.State != "EVALUATION_ERROR" || st.GroupsByState["EVALUATION_ERROR"] != 2 || !st.Stale || st.LastEvaluation == nil || *st.LastEvaluation.Reason != "query_failed" {
		t.Errorf("status = %+v", st)
	}
	store.statuses = nil
	env := decodeMonitor(t, do(t, h, http.MethodGet, "/api/v1/monitors/"+id, "dev", "", nil))
	if env.Data.Status == nil || env.Data.Status.State != controldb.SummaryNotEvaluated {
		t.Errorf("unevaluated status = %+v", env.Data.Status)
	}
}

// group 상태와 timeline: 값이 없으면 null과 이유, 기간 기본 24시간·최대 7일, 없는 monitor는 404
func TestMonitorGroupsAndTimeline(t *testing.T) {
	store := newFakeMonitors()
	h := monitorHandler(t, store, principals{"dev": user(t, authz.RoleDeveloper), "viewer": user(t, authz.RoleViewer)})
	id := createdMonitor(t, h)
	reason, ep := "below_minimum_requests", "0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f01"
	since := now.Add(-10 * time.Minute)
	store.groups = []controldb.GroupState{{GroupHash: "h1", Labels: map[string]string{"service.name": "checkout"}, State: "NO_DATA", NoDataSince: &since,
		LastReason: &reason, EpisodeID: &ep, UpdatedAt: now}}
	rec := do(t, h, http.MethodGet, "/api/v1/monitors/"+id+"/groups", "dev", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"last_value":null`) || !strings.Contains(rec.Body.String(), `"last_reason":"below_minimum_requests"`) ||
		!strings.Contains(rec.Body.String(), `"truncated":false`) {
		t.Errorf("groups: %d %s", rec.Code, rec.Body)
	}
	to := "add"
	store.timeline = controldb.MonitorTimeline{
		Evaluations: []controldb.EvaluationSummary{{Slot: now.Add(-time.Minute), Status: "evaluated", Groups: 1}},
		Transitions: []controldb.Transition{{At: now.Add(-time.Minute), GroupHash: "h1", Labels: map[string]string{}, From: "OK", To: &to, Opened: true}},
		Notifications: []controldb.NotificationAttempt{{ID: "n1", PolicyID: "p1", EventType: "alert.opened", Status: "failed", Attempts: 30,
			LastError: &reason, CreatedAt: now.Add(-time.Minute)}},
		NotificationsTruncated: true,
	}
	rec = do(t, h, http.MethodGet, "/api/v1/monitors/"+id+"/timeline", "dev", "", nil)
	var tl struct {
		Data TimelineItem `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tl); err != nil || rec.Code != http.StatusOK || len(tl.Data.Evaluations) != 1 || len(tl.Data.Transitions) != 1 ||
		len(tl.Data.Notifications) != 1 || !tl.Data.NotificationsTruncated || store.tlTo.Sub(store.tlFrom) != 24*time.Hour {
		t.Errorf("timeline: %d %s", rec.Code, rec.Body)
	}
	for _, q := range []string{"?from=2026-10-01T00:00:00Z&to=2026-10-09T00:00:00Z", "?from=2026-10-05T00:00:00Z&to=2026-10-04T00:00:00Z", "?from=yesterday"} {
		if r := do(t, h, http.MethodGet, "/api/v1/monitors/"+id+"/timeline"+q, "dev", "", nil); r.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, r.Code)
		}
	}
	missing := "/api/v1/monitors/0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9fff"
	for _, path := range []string{missing + "/groups", missing + "/timeline"} {
		if r := do(t, h, http.MethodGet, path, "dev", "", nil); r.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, r.Code)
		}
	}
	if r := do(t, h, http.MethodGet, "/api/v1/monitors/"+id+"/timeline", "viewer", "", nil); r.Code != http.StatusForbidden {
		t.Errorf("viewer: %d", r.Code)
	}
}

// 상태·group·timeline은 tenant 전체 telemetry의 평가 결과다. environment 제한 key와 telemetry.read 없는 key는 정의만 보고
// 상태는 null + status_forbidden 경고, groups·timeline은 403이다(ADR 0054 §1). 제한 없고 telemetry.read 있는 key는 본다.
func TestMonitorStatusForbiddenForRestrictedKeys(t *testing.T) {
	store := newFakeMonitors()
	h := monitorHandler(t, store, principals{
		"dev":     user(t, authz.RoleDeveloper),
		"staging": monitorKey(t, []string{"staging"}, authz.TelemetryRead),
		"no-tel":  monitorKey(t, nil),
		"full":    monitorKey(t, nil, authz.TelemetryRead),
	})
	id := createdMonitor(t, h)
	store.statuses = map[string]controldb.MonitorStatus{id: {State: "ALERT", GroupsByState: map[string]int{"ALERT": 1}}}
	type envelope struct {
		Data json.RawMessage `json:"data"`
		Meta struct {
			Warnings []string `json:"warnings"`
		} `json:"meta"`
	}
	for _, c := range []struct {
		token  string
		hidden bool
	}{{"staging", true}, {"no-tel", true}, {"full", false}, {"dev", false}} {
		for _, path := range []string{"/api/v1/monitors", "/api/v1/monitors/" + id} {
			rec := do(t, h, http.MethodGet, path, c.token, "", nil)
			var env envelope
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &env) != nil {
				t.Fatalf("%s %s: %d %s", c.token, path, rec.Code, rec.Body)
			}
			nullStatus := strings.Contains(string(env.Data), `"status":null`)
			warned := len(env.Meta.Warnings) == 1 && env.Meta.Warnings[0] == WarnStatusForbidden
			if nullStatus != c.hidden || warned != c.hidden || strings.Contains(string(env.Data), `"ALERT"`) == c.hidden {
				t.Errorf("%s %s: hidden=%v body=%s", c.token, path, c.hidden, rec.Body)
			}
		}
		for _, path := range []string{"/api/v1/monitors/" + id + "/groups", "/api/v1/monitors/" + id + "/timeline"} {
			rec := do(t, h, http.MethodGet, path, c.token, "", nil)
			if want := map[bool]int{true: http.StatusForbidden, false: http.StatusOK}[c.hidden]; rec.Code != want {
				t.Errorf("%s %s: %d, want %d", c.token, path, rec.Code, want)
			}
		}
	}
}
