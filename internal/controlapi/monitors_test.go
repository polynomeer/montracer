package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

// fakeMonitors는 저장소 계약(권한·revision·tombstone·idempotency)을 메모리로 흉내 낸다.
// 실제 SQL·RLS·동시성은 internal/controldb 통합 시험이 본다.
type fakeMonitors struct {
	byID  map[string]*controldb.Monitor
	order []string
	idem  map[string]struct {
		hash [32]byte
		resp controldb.StoredResponse
	}
	creates   int
	lastWrite controldb.MonitorWrite
	lastMatch *int64
}

func newFakeMonitors() *fakeMonitors {
	return &fakeMonitors{byID: map[string]*controldb.Monitor{}, idem: map[string]struct {
		hash [32]byte
		resp controldb.StoredResponse
	}{}}
}

func (f *fakeMonitors) CreateMonitor(_ context.Context, p authz.Principal, w controldb.MonitorWrite, idem controldb.IdempotencyRequest, _ string,
	respond func(controldb.Monitor) (controldb.StoredResponse, error)) (controldb.StoredResponse, bool, error) {
	if err := authz.Authorize(p, authz.MonitorsWrite); err != nil {
		return controldb.StoredResponse{}, false, err
	}
	k := p.Subject() + "|" + idem.MethodPath + "|" + idem.Key
	if prev, ok := f.idem[k]; ok {
		if prev.hash != idem.RequestHash {
			return controldb.StoredResponse{}, false, controldb.ErrIdempotencyConflict
		}
		return prev.resp, true, nil
	}
	f.creates++
	f.lastWrite = w
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", len(f.order)+1)
	m := &controldb.Monitor{ID: id, Name: w.Name, Spec: w.Spec, Enabled: w.Enabled, Revision: 1, CreatedBy: p.Subject(), UpdatedBy: p.Subject(), CreatedAt: now, UpdatedAt: now}
	f.byID[id] = m
	f.order = append(f.order, id)
	resp, err := respond(*m)
	if err != nil {
		return controldb.StoredResponse{}, false, err
	}
	f.idem[k] = struct {
		hash [32]byte
		resp controldb.StoredResponse
	}{idem.RequestHash, resp}
	return resp, false, nil
}

func (f *fakeMonitors) GetMonitor(_ context.Context, p authz.Principal, id string) (controldb.Monitor, error) {
	if err := authz.Authorize(p, authz.MonitorsRead); err != nil {
		return controldb.Monitor{}, err
	}
	m, ok := f.byID[id]
	if !ok {
		return controldb.Monitor{}, authz.ErrNotFound
	}
	return *m, nil
}

func (f *fakeMonitors) ListMonitors(_ context.Context, p authz.Principal, after *controldb.MonitorPosition, limit int) ([]controldb.Monitor, []controldb.MonitorPosition, bool, error) {
	if err := authz.Authorize(p, authz.MonitorsRead); err != nil {
		return nil, nil, false, err
	}
	var out []controldb.Monitor
	var pos []controldb.MonitorPosition
	for _, id := range f.order {
		m, ok := f.byID[id]
		if !ok || (after != nil && id <= after.ID) {
			continue
		}
		out = append(out, *m)
		pos = append(pos, controldb.MonitorPosition{NameNormalized: strings.ToLower(m.Name), ID: id})
	}
	more := len(out) > limit
	if more {
		out, pos = out[:limit], pos[:limit]
	}
	return out, pos, more, nil
}

func (f *fakeMonitors) UpdateMonitor(_ context.Context, p authz.Principal, id string, ifMatch int64, w controldb.MonitorWrite, _ string) (controldb.Monitor, error) {
	if err := authz.Authorize(p, authz.MonitorsWrite); err != nil {
		return controldb.Monitor{}, err
	}
	m, ok := f.byID[id]
	if !ok {
		return controldb.Monitor{}, authz.ErrNotFound
	}
	if m.Revision != ifMatch {
		return controldb.Monitor{}, controldb.ErrRevisionMismatch
	}
	f.lastWrite = w
	m.Name, m.Spec, m.Enabled, m.Revision = w.Name, w.Spec, w.Enabled, m.Revision+1
	return *m, nil
}

func (f *fakeMonitors) DeleteMonitor(_ context.Context, p authz.Principal, id string, ifMatch *int64, _ string) (int64, error) {
	if err := authz.Authorize(p, authz.MonitorsWrite); err != nil {
		return 0, err
	}
	f.lastMatch = ifMatch
	m, ok := f.byID[id]
	if !ok {
		return 0, authz.ErrNotFound
	}
	if ifMatch != nil && *ifMatch != m.Revision {
		return 0, controldb.ErrRevisionMismatch
	}
	delete(f.byID, id)
	return m.Revision + 1, nil
}

func monitorHandler(t *testing.T, store MonitorStore, ps principals) *Handler {
	t.Helper()
	clock := now
	signer, err := apicursor.NewSigner([]byte("0123456789abcdef0123456789abcdef"), time.Hour, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(Config{
		Authenticate: func(_ context.Context, token string) (authz.Principal, error) {
			if p, ok := ps[token]; ok {
				return p, nil
			}
			return authz.Principal{}, authz.ErrUnauthenticated
		},
		Audit: &fakeAudit{}, Monitors: store, Cursor: signer, Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func do(t *testing.T, h http.Handler, method, path, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const monitorBody = `{"name":"Checkout 오류율","query":{"kind":"error_ratio","filter":{"op":"eq","field":"service.name","value":"checkout"}},
	"window_seconds":300,"evaluation_seconds":60,"condition":{"operator":"gt","threshold":0.02},"for_seconds":120,"no_data":"alert_after_600s"}`

type monitorEnvelope struct {
	Data  MonitorItem  `json:"data"`
	Meta  MutationMeta `json:"meta"`
	Error *struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func decodeMonitor(t *testing.T, rec *httptest.ResponseRecorder) monitorEnvelope {
	t.Helper()
	var env monitorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %d %s: %v", rec.Code, rec.Body, err)
	}
	return env
}

func TestMonitorCreateIdempotency(t *testing.T) {
	store := newFakeMonitors()
	h := monitorHandler(t, store, principals{"dev": user(t, authz.RoleDeveloper)})
	idem := map[string]string{"Idempotency-Key": "create-1"}

	rec := do(t, h, http.MethodPost, "/api/v1/monitors", "dev", monitorBody, idem)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	env := decodeMonitor(t, rec)
	if env.Data.Revision != 1 || env.Data.Name != "Checkout 오류율" || rec.Header().Get("ETag") != `"1"` ||
		rec.Header().Get("Location") != "/api/v1/monitors/"+env.Data.ID || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("create response: %+v headers %v", env.Data, rec.Header())
	}
	// 저장되는 것은 정규형(기본값 채움)이다
	if !bytes.Contains(store.lastWrite.Spec, []byte(`"minimum_requests":100`)) || !bytes.Contains(env.Data.Spec, []byte(`"recovery_evaluations":2`)) {
		t.Errorf("normalized spec not stored: %s", store.lastWrite.Spec)
	}
	// 경고 없는 정의: 빈 목록(null 아님). 생성은 dry-run을 하지 않으므로 dry-run 경고도 없다
	if len(env.Meta.Warnings) != 0 || !strings.Contains(rec.Body.String(), `"warnings":[]`) {
		t.Errorf("warnings = %v", env.Meta.Warnings)
	}

	// 같은 key·같은 본문: 처음 응답 그대로, 새로 만들지 않음
	again := do(t, h, http.MethodPost, "/api/v1/monitors", "dev", monitorBody, idem)
	if again.Code != http.StatusCreated || again.Body.String() != rec.Body.String() || again.Header().Get("Idempotent-Replayed") != "true" || store.creates != 1 {
		t.Errorf("replay: %d replayed=%q creates=%d", again.Code, again.Header().Get("Idempotent-Replayed"), store.creates)
	}
	// 같은 key·다른 본문: 409
	other := do(t, h, http.MethodPost, "/api/v1/monitors", "dev", strings.Replace(monitorBody, "0.02", "0.05", 1), idem)
	if other.Code != http.StatusConflict || decodeMonitor(t, other).Error.Code != "CONFLICT" {
		t.Errorf("reuse with different body: %d %s", other.Code, other.Body)
	}
	// key 없음·형식 오류: 400, 저장소에 닿지 않음
	for _, key := range []string{"", "has space", strings.Repeat("k", 256)} {
		if r := do(t, h, http.MethodPost, "/api/v1/monitors", "dev", monitorBody, map[string]string{"Idempotency-Key": key}); r.Code != http.StatusBadRequest {
			t.Errorf("key %q: %d", key, r.Code)
		}
	}
	if store.creates != 1 {
		t.Errorf("creates = %d", store.creates)
	}
}

func TestMonitorValidationAndPermissions(t *testing.T) {
	store := newFakeMonitors()
	h := monitorHandler(t, store, principals{"dev": user(t, authz.RoleDeveloper), "viewer": user(t, authz.RoleViewer)})
	idem := map[string]string{"Idempotency-Key": "k"}

	// validate: 저장하지 않고 정규형·경고·dry_run(null)
	rec := do(t, h, http.MethodPost, "/api/v1/monitors/validate", "dev", monitorBody, nil)
	var v struct {
		Data struct {
			NormalizedSpec json.RawMessage `json:"normalized_spec"`
			Warnings       []string        `json:"warnings"`
			DryRun         json.RawMessage `json:"dry_run"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || rec.Code != http.StatusOK || string(v.Data.DryRun) != "null" ||
		!bytes.Contains(v.Data.NormalizedSpec, []byte(`"recovery_evaluations":2`)) || store.creates != 0 {
		t.Errorf("validate: %d %s", rec.Code, rec.Body)
	}
	// 형식 오류 400(field violation), 아직 지원하지 않는 의미 422
	bad := do(t, h, http.MethodPost, "/api/v1/monitors", "dev", strings.Replace(monitorBody, `"evaluation_seconds":60`, `"evaluation_seconds":45`, 1), idem)
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "evaluation_seconds") {
		t.Errorf("invalid spec: %d %s", bad.Code, bad.Body)
	}
	unsup := do(t, h, http.MethodPost, "/api/v1/monitors/validate", "dev", strings.Replace(monitorBody, `"name"`, `"notification_policy_id":"x","name"`, 1), nil)
	if unsup.Code != http.StatusUnprocessableEntity {
		t.Errorf("unsupported: %d %s", unsup.Code, unsup.Body)
	}
	// 권한: viewer는 쓰기·validate 403. 인증 없으면 401
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/monitors", monitorBody},
		{http.MethodPost, "/api/v1/monitors/validate", monitorBody},
	} {
		if r := do(t, h, c.method, c.path, "viewer", c.body, idem); r.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s: %d", c.method, c.path, r.Code)
		}
	}
	// D04 §01: Viewer 범위는 telemetry·dashboard다. monitor 조회는 Developer부터(monitors.read)
	if r := do(t, h, http.MethodGet, "/api/v1/monitors", "viewer", "", nil); r.Code != http.StatusForbidden {
		t.Errorf("viewer list: %d %s", r.Code, r.Body)
	}
	if r := do(t, h, http.MethodGet, "/api/v1/monitors", "", "", nil); r.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", r.Code)
	}
	// 권한이 header 검사보다 먼저다: viewer는 Idempotency-Key·If-Match 없이 보내도 403
	if r := do(t, h, http.MethodPost, "/api/v1/monitors", "viewer", monitorBody, nil); r.Code != http.StatusForbidden {
		t.Errorf("viewer without Idempotency-Key: %d", r.Code)
	}
	if r := do(t, h, http.MethodPut, "/api/v1/monitors/00000000-0000-4000-8000-000000000001", "viewer", monitorBody, nil); r.Code != http.StatusForbidden {
		t.Errorf("viewer PUT without If-Match: %d", r.Code)
	}
	// 본문 크기 초과는 413(D02 §19)
	if r := do(t, h, http.MethodPost, "/api/v1/monitors/validate", "dev", strings.Repeat(" ", 64<<10+1), nil); r.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized: %d", r.Code)
	}
	if store.creates != 0 {
		t.Errorf("rejected requests created monitors: %d", store.creates)
	}
}

func TestMonitorUpdateDeleteRevision(t *testing.T) {
	store := newFakeMonitors()
	h := monitorHandler(t, store, principals{"dev": user(t, authz.RoleDeveloper)})
	created := decodeMonitor(t, do(t, h, http.MethodPost, "/api/v1/monitors", "dev", monitorBody, map[string]string{"Idempotency-Key": "k"}))
	path := "/api/v1/monitors/" + created.Data.ID
	updated := strings.Replace(monitorBody, "0.02", "0.05", 1)

	// If-Match 없음 428, 형식 오류 400, 불일치 412
	if r := do(t, h, http.MethodPut, path, "dev", updated, nil); r.Code != http.StatusPreconditionRequired {
		t.Errorf("no If-Match: %d %s", r.Code, r.Body)
	}
	if r := do(t, h, http.MethodPut, path, "dev", updated, map[string]string{"If-Match": `W/"1"`}); r.Code != http.StatusBadRequest {
		t.Errorf("weak etag: %d", r.Code)
	}
	if r := do(t, h, http.MethodPut, path, "dev", updated, map[string]string{"If-Match": `"7"`}); r.Code != http.StatusPreconditionFailed {
		t.Errorf("stale If-Match: %d", r.Code)
	}
	// 맞으면 revision 2, ETag
	ok := do(t, h, http.MethodPut, path, "dev", updated, map[string]string{"If-Match": `"1"`})
	if ok.Code != http.StatusOK || decodeMonitor(t, ok).Data.Revision != 2 || ok.Header().Get("ETag") != `"2"` {
		t.Errorf("update: %d %s", ok.Code, ok.Body)
	}
	// GET은 ETag와 정규형. 정규형을 그대로 PUT해도 된다(조회 → 수정 왕복)
	got := do(t, h, http.MethodGet, path, "dev", "", nil)
	spec := decodeMonitor(t, got).Data.Spec
	if got.Header().Get("ETag") != `"2"` {
		t.Errorf("get etag: %v", got.Header())
	}
	if r := do(t, h, http.MethodPut, path, "dev", string(spec), map[string]string{"If-Match": "2"}); r.Code != http.StatusOK {
		t.Errorf("round-trip PUT: %d %s", r.Code, r.Body)
	}
	// 삭제: 낡은 If-Match 412, 맞으면 204, 그 뒤 GET 404. 형식이 틀린 id는 400
	if r := do(t, h, http.MethodDelete, path, "dev", "", map[string]string{"If-Match": `"1"`}); r.Code != http.StatusPreconditionFailed {
		t.Errorf("stale delete: %d", r.Code)
	}
	if r := do(t, h, http.MethodDelete, path, "dev", "", map[string]string{"If-Match": `"3"`}); r.Code != http.StatusNoContent {
		t.Errorf("delete: %d %s", r.Code, r.Body)
	}
	if r := do(t, h, http.MethodGet, path, "dev", "", nil); r.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d", r.Code)
	}
	if r := do(t, h, http.MethodGet, "/api/v1/monitors/NOT-A-UUID", "dev", "", nil); r.Code != http.StatusBadRequest {
		t.Errorf("bad id: %d", r.Code)
	}
}

func TestMonitorList(t *testing.T) {
	store := newFakeMonitors()
	h := monitorHandler(t, store, principals{"dev": user(t, authz.RoleDeveloper)})
	for i := 0; i < 3; i++ {
		do(t, h, http.MethodPost, "/api/v1/monitors", "dev", strings.Replace(monitorBody, "Checkout", fmt.Sprintf("M%d", i), 1), map[string]string{"Idempotency-Key": fmt.Sprintf("k%d", i)})
	}
	var names []string
	path := "/api/v1/monitors?limit=2"
	for page := 0; page < 3; page++ {
		rec := do(t, h, http.MethodGet, path, "dev", "", nil)
		var resp struct {
			Data       []MonitorItem `json:"data"`
			NextCursor *string       `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, rec.Code, rec.Body)
		}
		for _, m := range resp.Data {
			names = append(names, m.Name)
		}
		if resp.NextCursor == nil {
			break
		}
		path = "/api/v1/monitors?limit=2&cursor=" + *resp.NextCursor
	}
	if fmt.Sprint(names) != "[M0 오류율 M1 오류율 M2 오류율]" {
		t.Errorf("names = %v", names)
	}
	if r := do(t, h, http.MethodGet, "/api/v1/monitors?cursor=forged", "dev", "", nil); r.Code != http.StatusBadRequest {
		t.Errorf("forged cursor: %d", r.Code)
	}
	if r := do(t, h, http.MethodGet, "/api/v1/monitors?limit=201", "dev", "", nil); r.Code != http.StatusBadRequest {
		t.Errorf("limit 201: %d", r.Code)
	}
}

func TestMonitorAPIDisabled(t *testing.T) {
	h := monitorHandler(t, nil, principals{"dev": user(t, authz.RoleDeveloper)})
	if r := do(t, h, http.MethodGet, "/api/v1/monitors", "dev", "", nil); r.Code != http.StatusNotFound {
		t.Errorf("without store: %d", r.Code)
	}
}

func monitorKey(t *testing.T, envs []string) authz.Principal {
	t.Helper()
	h, _ := authz.NewKeyHasher(bytes.Repeat([]byte{7}, 32))
	g, _ := h.Generate(authz.KindAPIKey, nil)
	rec := authz.KeyRecord{KeyID: g.KeyID, Tenant: tenantA, Kind: authz.KindAPIKey, Hash: g.Hash,
		Scopes: []authz.Action{authz.MonitorsRead, authz.MonitorsWrite}, Environments: envs, IssuerRole: authz.RoleTenantAdmin, ExpiresAt: now.Add(time.Hour)}
	p, err := h.Authenticate(context.Background(), g.Token, authz.KindAPIKey, func(context.Context, string) (authz.KeyRecord, error) { return rec, nil }, now)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// environment 제한 key는 monitor를 만들거나 바꿀 수 없다: 평가는 tenant 전체 telemetry를 읽는다(ADR 0051). 제한 없는 key는 된다.
func TestMonitorWriteRejectsEnvironmentRestrictedKey(t *testing.T) {
	store := newFakeMonitors()
	h := monitorHandler(t, store, principals{"staging": monitorKey(t, []string{"staging"}), "all": monitorKey(t, nil)})
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/monitors/validate"}, {http.MethodPost, "/api/v1/monitors"},
		{http.MethodPut, "/api/v1/monitors/0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f01"}, {http.MethodDelete, "/api/v1/monitors/0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f01"},
	} {
		rec := do(t, h, c.method, c.path, "staging", monitorBody, map[string]string{"Idempotency-Key": "k", "If-Match": `"1"`})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s by restricted key: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if store.creates != 0 {
		t.Errorf("restricted key created %d monitors", store.creates)
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/monitors", "all", monitorBody, map[string]string{"Idempotency-Key": "k"}); rec.Code != http.StatusCreated {
		t.Errorf("unrestricted key create: %d %s", rec.Code, rec.Body)
	}
	// 정의 조회는 telemetry가 아니므로 제한 key도 된다
	if rec := do(t, h, http.MethodGet, "/api/v1/monitors", "staging", "", nil); rec.Code != http.StatusOK {
		t.Errorf("restricted key list: %d %s", rec.Code, rec.Body)
	}
}
