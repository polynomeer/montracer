package controlapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

// fakeDeployments는 저장소 계약(권한·재생·같은 값 한 행·한도)을 메모리로 흉내 낸다. 실제 SQL·RLS는 controldb 통합 시험이 본다.
type fakeDeployments struct {
	rows      []controldb.Deployment
	idem      map[string]controldb.StoredResponse
	hashes    map[string][32]byte
	limited   bool
	expired   int64
	lastQuery controldb.DeploymentQuery
	more      bool
}

func newFakeDeployments() *fakeDeployments {
	return &fakeDeployments{idem: map[string]controldb.StoredResponse{}, hashes: map[string][32]byte{}}
}

func (f *fakeDeployments) RecordDeployment(_ context.Context, p authz.Principal, w controldb.DeploymentWrite, idem controldb.IdempotencyRequest, _ string, n time.Time,
	respond func(controldb.Deployment, bool) (controldb.StoredResponse, error)) (controldb.DeploymentResult, error) {
	if err := authz.Authorize(p, authz.DeploymentsWrite); err != nil {
		return controldb.DeploymentResult{}, err
	}
	if !p.AllowsEnvironment(w.Environment) {
		return controldb.DeploymentResult{}, authz.ErrForbidden
	}
	if r, ok := f.idem[idem.Key]; ok {
		if f.hashes[idem.Key] != idem.RequestHash {
			return controldb.DeploymentResult{}, controldb.ErrIdempotencyConflict
		}
		return controldb.DeploymentResult{Stored: r, Replayed: true}, nil
	}
	if f.limited {
		return controldb.DeploymentResult{}, controldb.ErrDeploymentRateLimited
	}
	for _, d := range f.rows {
		if strings.EqualFold(d.ServiceName, w.ServiceName) && d.ServiceNamespace == w.ServiceNamespace && d.Environment == w.Environment &&
			d.Version == w.Version && d.Commit == w.Commit && d.OccurredAt.Equal(w.OccurredAt) {
			r, err := respond(d, false)
			f.idem[idem.Key], f.hashes[idem.Key] = r, idem.RequestHash
			return controldb.DeploymentResult{Stored: r}, err
		}
	}
	d := controldb.Deployment{ID: fmt.Sprintf("0c6c2b4e-6f4f-4a52-9a0f-6f6f2b1f%04d", len(f.rows)+1), ServiceNamespace: w.ServiceNamespace, ServiceName: w.ServiceName,
		Environment: w.Environment, Version: w.Version, Commit: w.Commit, OccurredAt: w.OccurredAt, RecordedAt: n, Source: "api"}
	f.rows = append(f.rows, d)
	r, err := respond(d, true)
	f.idem[idem.Key], f.hashes[idem.Key] = r, idem.RequestHash
	return controldb.DeploymentResult{Stored: r, Created: true, Expired: f.expired}, err
}

func (f *fakeDeployments) ListDeployments(_ context.Context, p authz.Principal, q controldb.DeploymentQuery, _ time.Time) ([]controldb.Deployment, bool, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, false, err
	}
	if q.Environment != nil && !p.AllowsEnvironment(*q.Environment) {
		return nil, false, authz.ErrForbidden
	}
	f.lastQuery = q
	return f.rows, f.more, nil
}

type deploymentObs struct {
	outcomes []string
	expired  int64
}

func deploymentHandler(t *testing.T, store DeploymentStore, ps principals, obs *deploymentObs) *Handler {
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
		Audit: &fakeAudit{}, Cursor: signer, Now: func() time.Time { return now },
	}
	if store != nil {
		cfg.Deployments = store
	}
	if obs != nil {
		cfg.ObserveDeployment = func(o string) { obs.outcomes = append(obs.outcomes, o) }
		cfg.ObserveDeploymentsExpired = func(n int64) { obs.expired += n }
	}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

const deploymentBody = `{"service":{"name":"checkout","namespace":"shop"},"environment":"prod","version":"1.4.2","commit":"3F9C2AB1D4E5","timestamp":"2026-10-04T05:30:00Z"}`

func idemKey(k string) map[string]string { return map[string]string{"Idempotency-Key": k} }

// 쓰기: Idempotency-Key 필수, 새 행 201, 같은 key 재생, 다른 본문 409, 다른 key의 같은 값 200(한 행). commit은 소문자로, 기록자는 응답에 없다.
func TestRecordDeployment(t *testing.T) {
	store := newFakeDeployments()
	store.expired = 2
	obs := &deploymentObs{}
	h := deploymentHandler(t, store, principals{"op": user(t, authz.RoleOperator)}, obs)
	if r := do(t, h, http.MethodPost, "/api/v1/deployments", "op", deploymentBody, nil); r.Code != http.StatusBadRequest {
		t.Errorf("no key: %d", r.Code)
	}
	rec := do(t, h, http.MethodPost, "/api/v1/deployments", "op", deploymentBody, idemKey("k1"))
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"commit":"3f9c2ab1d4e5"`) || !strings.Contains(rec.Body.String(), `"source":"api"`) {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "actor") || strings.Contains(rec.Body.String(), `"op"`) {
		t.Errorf("recorder must not be in the response: %s", rec.Body)
	}
	again := do(t, h, http.MethodPost, "/api/v1/deployments", "op", deploymentBody, idemKey("k1"))
	if again.Code != http.StatusCreated || again.Header().Get("Idempotent-Replayed") != "true" || len(store.rows) != 1 {
		t.Errorf("replay: %d %v rows=%d", again.Code, again.Header(), len(store.rows))
	}
	other := strings.Replace(deploymentBody, "1.4.2", "1.4.3", 1)
	if r := do(t, h, http.MethodPost, "/api/v1/deployments", "op", other, idemKey("k1")); r.Code != http.StatusConflict {
		t.Errorf("different body: %d", r.Code)
	}
	if r := do(t, h, http.MethodPost, "/api/v1/deployments", "op", deploymentBody, idemKey("k2")); r.Code != http.StatusOK || len(store.rows) != 1 || r.Header().Get("Idempotent-Replayed") != "" {
		t.Errorf("same value, other key: %d rows=%d", r.Code, len(store.rows))
	}
	// timestamp 생략: 수신 시각. 같은 key 재시도는 한 행
	noTime := `{"service":{"name":"cart"},"environment":"prod","version":"2.0.0"}`
	first := do(t, h, http.MethodPost, "/api/v1/deployments", "op", noTime, idemKey("k3"))
	if first.Code != http.StatusCreated || !strings.Contains(first.Body.String(), `"commit":null`) || !strings.Contains(first.Body.String(), `"service_namespace":""`) {
		t.Fatalf("no timestamp: %d %s", first.Code, first.Body)
	}
	if r := do(t, h, http.MethodPost, "/api/v1/deployments", "op", noTime, idemKey("k3")); r.Code != http.StatusCreated || len(store.rows) != 2 {
		t.Errorf("retry without timestamp: %d rows=%d", r.Code, len(store.rows))
	}
	want := "created,replayed,existing,created,replayed"
	// 첫 호출(key 없음)·다른 본문(409) 앞의 invalid·conflict는 결과 수에 invalid만 든다
	if got := strings.Join(obs.outcomes, ","); got != "invalid,"+want {
		t.Errorf("outcomes = %s", got)
	}
	if obs.expired != 4 {
		t.Errorf("expired = %d", obs.expired)
	}
}

func TestRecordDeploymentRejects(t *testing.T) {
	store := newFakeDeployments()
	h := deploymentHandler(t, store, principals{
		"op": user(t, authz.RoleOperator), "dev": user(t, authz.RoleDeveloper), "viewer": user(t, authz.RoleViewer),
		"prodkey": monitorKey(t, []string{"prod"}, authz.DeploymentsWrite), "noscope": monitorKey(t, nil, authz.TelemetryRead),
	}, nil)
	for name, body := range map[string]string{
		"not json":       `{`,
		"no service":     `{"environment":"prod","version":"1"}`,
		"empty name":     `{"service":{"name":""},"environment":"prod","version":"1"}`,
		"long name":      `{"service":{"name":"` + strings.Repeat("n", 256) + `"},"environment":"prod","version":"1"}`,
		"control char":   `{"service":{"name":"a\nb"},"environment":"prod","version":"1"}`,
		"no env":         `{"service":{"name":"a"},"version":"1"}`,
		"long env":       `{"service":{"name":"a"},"environment":"` + strings.Repeat("e", 65) + `","version":"1"}`,
		"no version":     `{"service":{"name":"a"},"environment":"prod"}`,
		"bad version":    `{"service":{"name":"a"},"environment":"prod","version":"1 2"}`,
		"bad commit":     `{"service":{"name":"a"},"environment":"prod","version":"1","commit":"xyz"}`,
		"short commit":   `{"service":{"name":"a"},"environment":"prod","version":"1","commit":"abc"}`,
		"bad time":       `{"service":{"name":"a"},"environment":"prod","version":"1","timestamp":"yesterday"}`,
		"too old":        `{"service":{"name":"a"},"environment":"prod","version":"1","timestamp":"2026-01-01T00:00:00Z"}`,
		"future":         `{"service":{"name":"a"},"environment":"prod","version":"1","timestamp":"2027-01-01T00:00:00Z"}`,
		"trailing value": `{"service":{"name":"a"},"environment":"prod","version":"1"}{"message":"x"}`,
		"bidi name":      `{"service":{"name":"a\u202eb"},"environment":"prod","version":"1"}`,
		"c1 control":     `{"service":{"name":"a\u0085b"},"environment":"prod","version":"1"}`,
		"empty commit":   `{"service":{"name":"a"},"environment":"prod","version":"1","commit":""}`,
		"null commit":    `{"service":{"name":"a"},"environment":"prod","version":"1","commit":null}`,
		"null time":      `{"service":{"name":"a"},"environment":"prod","version":"1","timestamp":null}`,
		"free text":      `{"service":{"name":"a"},"environment":"prod","version":"1","message":"fix bug for kim@example.com"}`,
		"tenant in body": `{"service":{"name":"a"},"environment":"prod","version":"1","tenant_id":"x"}`,
	} {
		if r := do(t, h, http.MethodPost, "/api/v1/deployments", "op", body, idemKey("r-"+name)); r.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, r.Code, r.Body)
		}
	}
	if len(store.rows) != 0 {
		t.Fatalf("rejected requests stored rows: %+v", store.rows)
	}
	// 권한: Operator 미만 403, scope 없는 key 403, environment 제한 key의 다른 환경 403
	for tok, body := range map[string]string{"dev": deploymentBody, "viewer": deploymentBody, "noscope": deploymentBody,
		"prodkey": strings.Replace(deploymentBody, `"prod"`, `"staging"`, 1)} {
		if r := do(t, h, http.MethodPost, "/api/v1/deployments", tok, body, idemKey("p")); r.Code != http.StatusForbidden {
			t.Errorf("%s: %d", tok, r.Code)
		}
	}
	if r := do(t, h, http.MethodPost, "/api/v1/deployments", "prodkey", deploymentBody, idemKey("ok")); r.Code != http.StatusCreated {
		t.Errorf("prod key on prod: %d %s", r.Code, r.Body)
	}
	// 한도: 429와 Retry-After
	store.limited = true
	if r := do(t, h, http.MethodPost, "/api/v1/deployments", "op", strings.Replace(deploymentBody, "1.4.2", "9", 1), idemKey("lim")); r.Code != http.StatusTooManyRequests || r.Header().Get("Retry-After") != "60" {
		t.Errorf("rate limit: %d %v", r.Code, r.Header())
	}
}

// 조회: 기본 범위 7일, namespace 없음·빈 값·지정 구별, cursor, 빈 결과 200, environment 제한.
func TestListDeployments(t *testing.T) {
	store := newFakeDeployments()
	h := deploymentHandler(t, store, principals{
		"viewer": user(t, authz.RoleViewer), "auditor": user(t, authz.RoleSecurityAuditor), "prodkey": monitorKey(t, []string{"prod"}, authz.TelemetryRead),
	}, nil)
	empty := do(t, h, http.MethodGet, "/api/v1/deployments?service_name=checkout", "viewer", "", nil)
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"data":[]`) || !strings.Contains(empty.Body.String(), `"next_cursor":null`) {
		t.Fatalf("empty: %d %s", empty.Code, empty.Body)
	}
	q := store.lastQuery
	if !q.To.Equal(now) || !q.From.Equal(now.Add(-7*24*time.Hour)) || q.ServiceNamespace != nil || q.Environment != nil || q.Limit != 100 {
		t.Errorf("defaults: %+v", q)
	}
	do(t, h, http.MethodGet, "/api/v1/deployments?service_name=checkout&service_namespace=", "viewer", "", nil)
	if store.lastQuery.ServiceNamespace == nil || *store.lastQuery.ServiceNamespace != "" {
		t.Errorf("empty namespace: %+v", store.lastQuery)
	}
	store.rows = []controldb.Deployment{{ID: "0c6c2b4e-6f4f-4a52-9a0f-6f6f2b1f0001", ServiceName: "checkout", Environment: "prod", Version: "1", OccurredAt: now.Add(-time.Hour), RecordedAt: now, Source: "api"}}
	store.more = true
	page := do(t, h, http.MethodGet, "/api/v1/deployments?service_name=checkout&service_namespace=shop&environment=prod&limit=1", "viewer", "", nil)
	if page.Code != http.StatusOK || strings.Contains(page.Body.String(), `"next_cursor":null`) {
		t.Fatalf("page: %d %s", page.Code, page.Body)
	}
	cur := decodeCursorField(t, page.Body.Bytes())
	next := do(t, h, http.MethodGet, "/api/v1/deployments?service_name=CHECKOUT&service_namespace=shop&environment=prod&limit=1&cursor="+cur, "viewer", "", nil)
	if next.Code != http.StatusOK || store.lastQuery.After == nil || store.lastQuery.After.ID != store.rows[0].ID {
		t.Errorf("next page: %d %+v", next.Code, store.lastQuery)
	}
	if r := do(t, h, http.MethodGet, "/api/v1/deployments?service_name=other&limit=1&cursor="+cur, "viewer", "", nil); r.Code != http.StatusBadRequest {
		t.Errorf("cursor for another query: %d", r.Code)
	}
	// 범위를 생략한 조회의 다음 page는 첫 page 시각 기준이다(시계가 흘러도 범위가 움직이지 않는다)
	h.cfg.Now = func() time.Time { return now.Add(10 * time.Minute) }
	if r := do(t, h, http.MethodGet, "/api/v1/deployments?service_name=checkout&service_namespace=shop&environment=prod&limit=1&cursor="+cur, "viewer", "", nil); r.Code != http.StatusOK ||
		!store.lastQuery.To.Equal(now) || !store.lastQuery.From.Equal(now.Add(-7*24*time.Hour)) || !store.lastQuery.RecordedBefore.Equal(now) {
		t.Errorf("range regenerated from the snapshot: %d %+v", r.Code, store.lastQuery)
	}
	h.cfg.Now = func() time.Time { return now }
	// to만 주면 from은 그 앞 7일이다
	if r := do(t, h, http.MethodGet, "/api/v1/deployments?service_name=a&to=2026-01-08T00:00:00Z", "viewer", "", nil); r.Code != http.StatusOK ||
		!store.lastQuery.From.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("to only: %d %+v", r.Code, store.lastQuery)
	}
	for name, path := range map[string]string{
		"no service": "/api/v1/deployments",
		"bad range":  "/api/v1/deployments?service_name=a&from=2026-10-05T00:00:00Z&to=2026-10-01T00:00:00Z",
		"too long":   "/api/v1/deployments?service_name=a&from=2024-01-01T00:00:00Z&to=2026-10-01T00:00:00Z",
		"bad limit":  "/api/v1/deployments?service_name=a&limit=501",
		"bad env":    "/api/v1/deployments?service_name=a&environment=",
	} {
		if r := do(t, h, http.MethodGet, path, "viewer", "", nil); r.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, r.Code)
		}
	}
	// 감사자는 telemetry를 못 본다. 제한 key는 허용 밖을 명시하면 403(빈 결과가 아니다)
	if r := do(t, h, http.MethodGet, "/api/v1/deployments?service_name=a", "auditor", "", nil); r.Code != http.StatusForbidden {
		t.Errorf("auditor: %d", r.Code)
	}
	if r := do(t, h, http.MethodGet, "/api/v1/deployments?service_name=a&environment=staging", "prodkey", "", nil); r.Code != http.StatusForbidden {
		t.Errorf("out of scope environment: %d", r.Code)
	}
}

// flag off(Deployments nil)는 두 경로 모두 404다(ADR 0076 Rollback).
func TestDeploymentsDisabled(t *testing.T) {
	h := deploymentHandler(t, nil, principals{"op": user(t, authz.RoleOperator)}, nil)
	if r := do(t, h, http.MethodPost, "/api/v1/deployments", "op", deploymentBody, idemKey("k")); r.Code != http.StatusNotFound {
		t.Errorf("post: %d", r.Code)
	}
	if r := do(t, h, http.MethodGet, "/api/v1/deployments?service_name=a", "op", "", nil); r.Code != http.StatusNotFound {
		t.Errorf("get: %d", r.Code)
	}
}

func decodeCursorField(t *testing.T, body []byte) string {
	t.Helper()
	s := string(body)
	const key = `"next_cursor":"`
	i := strings.Index(s, key)
	if i < 0 {
		t.Fatalf("no cursor: %s", s)
	}
	rest := s[i+len(key):]
	return rest[:strings.Index(rest, `"`)]
}
