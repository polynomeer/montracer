package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// fakeDryRunMetrics는 매 분 checkout 1,000건 중 오류 errs건을 돌려준다.
type fakeDryRunMetrics struct {
	errs      uint64
	err       error
	queries   int
	principal authz.Principal
}

func (f *fakeDryRunMetrics) RollupWatermark(_ context.Context, p authz.Principal, _ time.Duration, _, now time.Time) (time.Time, error) {
	f.principal = p
	return now.Add(-2 * time.Minute), f.err
}

func (f *fakeDryRunMetrics) MetricBuckets(_ context.Context, p authz.Principal, q telemetrystore.MetricQuery, _ time.Time) ([]telemetrystore.MetricBucket, error) {
	f.queries++
	if f.err != nil {
		return nil, f.err
	}
	var out []telemetrystore.MetricBucket
	b := func(status string, at time.Time, n uint64) telemetrystore.MetricBucket {
		return telemetrystore.MetricBucket{Group: []string{status}, StepStart: at, Types: []string{"histogram"}, Units: []string{"s"},
			Windows: 1, Streams: 1, HistogramWindows: 1, Count: n, BoundsVariants: 1}
	}
	for at := q.Range.From; at.Before(q.Range.To); at = at.Add(time.Minute) {
		out = append(out, b("200", at, 1000-f.errs), b("500", at, f.errs))
	}
	return out, nil
}

type budgetErr struct{}

func (budgetErr) Error() string                  { return "budget" }
func (budgetErr) BudgetExceeded() map[string]any { return nil }

func dryRunHandler(t *testing.T, m DryRunMetrics, ps principals) *Handler {
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
		Audit: &fakeAudit{}, Monitors: newFakeMonitors(), Cursor: signer, Now: func() time.Time { return now },
	}
	if m != nil {
		cfg.Metrics = m
	}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type validateEnvelope struct {
	Data struct {
		Warnings []string        `json:"warnings"`
		DryRun   json.RawMessage `json:"dry_run"`
	} `json:"data"`
}

func validate(t *testing.T, h http.Handler, token string) validateEnvelope {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/api/v1/monitors/validate", token, monitorBody, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate: %d %s", rec.Code, rec.Body)
	}
	var env validateEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	return env
}

// 요청자 권한으로 지난 24시간을 다시 평가해 후보 발화 구간을 돌려준다(오류율 5% → 사건 하나, 끝까지 열림)
func TestValidateDryRun(t *testing.T) {
	m := &fakeDryRunMetrics{errs: 50}
	dev := user(t, authz.RoleDeveloper)
	env := validate(t, dryRunHandler(t, m, principals{"dev": dev}), "dev")
	var dr struct {
		Evaluations int    `json:"evaluations"`
		StepSeconds int    `json:"step_seconds"`
		Reason      string `json:"reason"`
		Coverage    struct {
			Evaluated int `json:"evaluated"`
		} `json:"coverage"`
		Groups []struct {
			Intervals []struct {
				Start  time.Time  `json:"start"`
				End    *time.Time `json:"end"`
				Reason string     `json:"reason"`
			} `json:"firing_intervals"`
			FiringSeconds int64  `json:"firing_seconds"`
			FinalState    string `json:"final_state"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(env.Data.DryRun, &dr); err != nil {
		t.Fatalf("dry_run %s: %v", env.Data.DryRun, err)
	}
	if dr.Evaluations != 1440 || dr.StepSeconds != 60 || dr.Coverage.Evaluated != 1440 || dr.Reason != "" || len(dr.Groups) != 1 ||
		len(dr.Groups[0].Intervals) != 1 || dr.Groups[0].Intervals[0].End != nil || dr.Groups[0].FinalState != "ALERT" {
		t.Errorf("dry_run = %s", env.Data.DryRun)
	}
	for _, w := range env.Data.Warnings {
		if bytes.HasPrefix([]byte(w), []byte("dry_run")) {
			t.Errorf("dry-run warning on success: %v", env.Data.Warnings)
		}
	}
	if m.principal.Tenant() != dev.Tenant() || m.principal.Subject() != dev.Subject() || m.queries != 1 {
		t.Errorf("queried as %+v (%d queries)", m.principal, m.queries)
	}
}

// dry-run을 못 하면 dry_run은 null이고 이유가 경고로 온다 — "발화 없음"으로 보이지 않는다. validate 자체는 200이다.
func TestValidateDryRunUnavailable(t *testing.T) {
	writeOnly := func() authz.Principal {
		h, _ := authz.NewKeyHasher(bytes.Repeat([]byte{9}, 32))
		g, _ := h.Generate(authz.KindAPIKey, nil)
		rec := authz.KeyRecord{KeyID: g.KeyID, Tenant: tenantA, Kind: authz.KindAPIKey, Hash: g.Hash,
			Scopes: []authz.Action{authz.MonitorsWrite}, IssuerRole: authz.RoleTenantAdmin, ExpiresAt: now.Add(time.Hour)}
		p, err := h.Authenticate(context.Background(), g.Token, authz.KindAPIKey, func(context.Context, string) (authz.KeyRecord, error) { return rec, nil }, now)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}()
	for _, c := range []struct {
		name    string
		metrics DryRunMetrics
		token   string
		want    string
	}{
		{"no analytics store", nil, "dev", "dry_run_unavailable"},
		{"query failure", &fakeDryRunMetrics{err: errors.New("clickhouse down")}, "dev", "dry_run_failed"},
		{"budget", &fakeDryRunMetrics{err: budgetErr{}}, "dev", "dry_run_too_large"},
		{"no telemetry.read", &fakeDryRunMetrics{}, "write-only", "dry_run_forbidden"},
	} {
		h := dryRunHandler(t, c.metrics, principals{"dev": user(t, authz.RoleDeveloper), "write-only": writeOnly})
		env := validate(t, h, c.token)
		if string(env.Data.DryRun) != "null" || !slices.Contains(env.Data.Warnings, c.want) {
			t.Errorf("%s: dry_run=%s warnings=%v", c.name, env.Data.DryRun, env.Data.Warnings)
		}
	}
}

// 동시 dry-run 상한을 넘으면 기다리지 않고 dry_run_busy로 돌려준다(CPU 보호). 결과는 outcome별로 센다.
func TestValidateDryRunBusyAndObserved(t *testing.T) {
	outcomes := map[string]int{}
	h := dryRunHandler(t, &fakeDryRunMetrics{errs: 50}, principals{"dev": user(t, authz.RoleDeveloper)})
	h.cfg.ObserveDryRun = func(outcome string, d time.Duration) {
		outcomes[outcome]++
		if outcome == "ok" && d <= 0 {
			t.Error("ok dry-run without duration")
		}
	}
	if env := validate(t, h, "dev"); string(env.Data.DryRun) == "null" {
		t.Fatalf("dry-run failed: %v", env.Data.Warnings)
	}
	for i := 0; i < cap(h.dryRunSlots); i++ {
		h.dryRunSlots <- struct{}{}
	}
	env := validate(t, h, "dev")
	if string(env.Data.DryRun) != "null" || !slices.Contains(env.Data.Warnings, "dry_run_busy") {
		t.Errorf("busy: dry_run=%s warnings=%v", env.Data.DryRun, env.Data.Warnings)
	}
	if outcomes["ok"] != 1 || outcomes["busy"] != 1 {
		t.Errorf("outcomes = %v", outcomes)
	}
}
