package controlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

// fakeDashboards는 저장소 계약(권한·idempotency·revision·tombstone)을 메모리로 흉내 낸다. 실제 SQL·RLS는 controldb 통합 시험이 본다.
type fakeDashboards struct {
	byID    map[string]*controldb.Dashboard
	idem    map[string]controldb.StoredResponse
	creates int
}

func newFakeDashboards() *fakeDashboards {
	return &fakeDashboards{byID: map[string]*controldb.Dashboard{}, idem: map[string]controldb.StoredResponse{}}
}

func (f *fakeDashboards) CreateDashboard(_ context.Context, p authz.Principal, w controldb.DashboardWrite, idem controldb.IdempotencyRequest, _ string,
	respond func(controldb.Dashboard) (controldb.StoredResponse, error)) (controldb.StoredResponse, bool, error) {
	if err := authz.Authorize(p, authz.DashboardsWrite); err != nil {
		return controldb.StoredResponse{}, false, err
	}
	if r, ok := f.idem[idem.Key]; ok {
		if string(r.Body) == "conflict" {
			return controldb.StoredResponse{}, false, controldb.ErrIdempotencyConflict
		}
		return r, true, nil
	}
	f.creates++
	d := &controldb.Dashboard{ID: fmt.Sprintf("0c6c2b4e-6f4f-4a52-9a0f-6f6f2b1f%04d", f.creates), Title: w.Title, Spec: w.Spec, WidgetCount: w.WidgetCount,
		Revision: 1, CreatedBy: p.Subject(), UpdatedBy: p.Subject(), CreatedAt: now, UpdatedAt: now}
	f.byID[d.ID] = d
	r, err := respond(*d)
	f.idem[idem.Key] = r
	return r, false, err
}

func (f *fakeDashboards) GetDashboard(_ context.Context, p authz.Principal, id string) (controldb.Dashboard, error) {
	if err := authz.Authorize(p, authz.DashboardsRead); err != nil {
		return controldb.Dashboard{}, err
	}
	d, ok := f.byID[id]
	if !ok {
		return controldb.Dashboard{}, authz.ErrNotFound
	}
	return *d, nil
}

func (f *fakeDashboards) ListDashboards(_ context.Context, p authz.Principal, after *controldb.DashboardPosition, limit int) ([]controldb.Dashboard, []controldb.DashboardPosition, bool, error) {
	if err := authz.Authorize(p, authz.DashboardsRead); err != nil {
		return nil, nil, false, err
	}
	var out []controldb.Dashboard
	var pos []controldb.DashboardPosition
	for i := 1; i <= f.creates; i++ {
		d, ok := f.byID[fmt.Sprintf("0c6c2b4e-6f4f-4a52-9a0f-6f6f2b1f%04d", i)]
		if !ok || (after != nil && d.ID <= after.ID) {
			continue
		}
		out = append(out, *d)
		pos = append(pos, controldb.DashboardPosition{TitleNormalized: strings.ToLower(d.Title), ID: d.ID})
	}
	more := len(out) > limit
	if more {
		out, pos = out[:limit], pos[:limit]
	}
	return out, pos, more, nil
}

func (f *fakeDashboards) UpdateDashboard(_ context.Context, p authz.Principal, id string, ifMatch int64, w controldb.DashboardWrite, _ string) (controldb.Dashboard, error) {
	if err := authz.Authorize(p, authz.DashboardsWrite); err != nil {
		return controldb.Dashboard{}, err
	}
	d, ok := f.byID[id]
	if !ok {
		return controldb.Dashboard{}, authz.ErrNotFound
	}
	if d.Revision != ifMatch {
		return controldb.Dashboard{}, controldb.ErrRevisionMismatch
	}
	d.Title, d.Spec, d.WidgetCount, d.Revision = w.Title, w.Spec, w.WidgetCount, d.Revision+1
	return *d, nil
}

func (f *fakeDashboards) DeleteDashboard(_ context.Context, p authz.Principal, id string, ifMatch *int64, _ string) (int64, error) {
	if err := authz.Authorize(p, authz.DashboardsWrite); err != nil {
		return 0, err
	}
	d, ok := f.byID[id]
	if !ok {
		return 0, authz.ErrNotFound
	}
	if ifMatch != nil && *ifMatch != d.Revision {
		return 0, controldb.ErrRevisionMismatch
	}
	delete(f.byID, id)
	return d.Revision + 1, nil
}

func dashboardHandler(t *testing.T, store DashboardStore, ps principals) *Handler {
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
		cfg.Dashboards = store
	}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

const dashboardBody = `{"title":"Checkout","variables":[{"name":"svc","kind":"service","default":"checkout"}],
	"widgets":[{"id":"rate","kind":"time_series","query":{"metric":"http.server.request.duration","aggregation":"rate",
	  "filter":{"op":"eq","field":"service.name","value":"$svc"}}},{"id":"note","kind":"text","text":"메모"}],
	"layout":[{"widget_id":"rate","x":0,"y":0,"w":12,"h":6},{"widget_id":"note","x":12,"y":0,"w":12,"h":6}]}`

type dashboardEnvelope struct {
	Data DashboardItem `json:"data"`
}

func decodeDashboard(t *testing.T, body []byte) DashboardItem {
	t.Helper()
	var env dashboardEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return env.Data
}

// 생성: Idempotency-Key 필수, 정규형 spec과 위젯 수·ETag·Location. 같은 key 재시도는 같은 응답(새로 만들지 않음).
// 목록은 요약(spec 없음), 단건은 spec.
func TestDashboardCreateAndRead(t *testing.T) {
	store := newFakeDashboards()
	h := dashboardHandler(t, store, principals{"dev": user(t, authz.RoleDeveloper), "viewer": user(t, authz.RoleViewer)})
	if r := do(t, h, http.MethodPost, "/api/v1/dashboards", "dev", dashboardBody, nil); r.Code != http.StatusBadRequest {
		t.Errorf("no Idempotency-Key: %d", r.Code)
	}
	rec := do(t, h, http.MethodPost, "/api/v1/dashboards", "dev", dashboardBody, map[string]string{"Idempotency-Key": "k1"})
	d := decodeDashboard(t, rec.Body.Bytes())
	if rec.Code != http.StatusCreated || d.Revision != 1 || d.WidgetCount != 2 || rec.Header().Get("ETag") != `"1"` ||
		rec.Header().Get("Location") != "/api/v1/dashboards/"+d.ID || !strings.Contains(string(d.Spec), `"version":1`) || !strings.Contains(string(d.Spec), `"$svc"`) {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	again := do(t, h, http.MethodPost, "/api/v1/dashboards", "dev", dashboardBody, map[string]string{"Idempotency-Key": "k1"})
	if again.Header().Get("Idempotent-Replayed") != "true" || store.creates != 1 {
		t.Errorf("replay: %v creates=%d", again.Header(), store.creates)
	}
	list := do(t, h, http.MethodGet, "/api/v1/dashboards", "viewer", "", nil)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), `"spec"`) || !strings.Contains(list.Body.String(), `"widget_count":2`) {
		t.Errorf("list: %d %s", list.Code, list.Body)
	}
	got := do(t, h, http.MethodGet, "/api/v1/dashboards/"+d.ID, "viewer", "", nil)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"spec"`) || got.Header().Get("ETag") != `"1"` {
		t.Errorf("get: %d %s", got.Code, got.Body)
	}
	if r := do(t, h, http.MethodGet, "/api/v1/dashboards/0c6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9999", "viewer", "", nil); r.Code != http.StatusNotFound {
		t.Errorf("missing: %d", r.Code)
	}
	if r := do(t, h, http.MethodGet, "/api/v1/dashboards/NOT-A-UUID", "viewer", "", nil); r.Code != http.StatusBadRequest {
		t.Errorf("bad id: %d", r.Code)
	}
}

// 권한: Viewer는 읽기만(쓰기 403), Security Auditor는 dashboard를 볼 수 없다(D04 §01).
func TestDashboardPermissions(t *testing.T) {
	store := newFakeDashboards()
	h := dashboardHandler(t, store, principals{"viewer": user(t, authz.RoleViewer), "auditor": user(t, authz.RoleSecurityAuditor), "dev": user(t, authz.RoleDeveloper)})
	if r := do(t, h, http.MethodPost, "/api/v1/dashboards", "viewer", dashboardBody, map[string]string{"Idempotency-Key": "k"}); r.Code != http.StatusForbidden {
		t.Errorf("viewer create: %d", r.Code)
	}
	if r := do(t, h, http.MethodGet, "/api/v1/dashboards", "auditor", "", nil); r.Code != http.StatusForbidden {
		t.Errorf("auditor list: %d", r.Code)
	}
	d := decodeDashboard(t, do(t, h, http.MethodPost, "/api/v1/dashboards", "dev", dashboardBody, map[string]string{"Idempotency-Key": "k"}).Body.Bytes())
	for _, c := range []struct{ method, body string }{{http.MethodPut, dashboardBody}, {http.MethodDelete, ""}} {
		if r := do(t, h, c.method, "/api/v1/dashboards/"+d.ID, "viewer", c.body, map[string]string{"If-Match": `"1"`}); r.Code != http.StatusForbidden {
			t.Errorf("viewer %s: %d", c.method, r.Code)
		}
	}
}

// 수정: If-Match 필수(428)·다르면 412(마지막 쓰기가 덮어쓰지 않는다). 삭제는 tombstone, 이후 404.
func TestDashboardUpdateDelete(t *testing.T) {
	store := newFakeDashboards()
	h := dashboardHandler(t, store, principals{"dev": user(t, authz.RoleDeveloper)})
	d := decodeDashboard(t, do(t, h, http.MethodPost, "/api/v1/dashboards", "dev", dashboardBody, map[string]string{"Idempotency-Key": "k"}).Body.Bytes())
	path := "/api/v1/dashboards/" + d.ID
	renamed := strings.Replace(dashboardBody, `"Checkout"`, `"Checkout v2"`, 1)
	if r := do(t, h, http.MethodPut, path, "dev", renamed, nil); r.Code != http.StatusPreconditionRequired {
		t.Errorf("no If-Match: %d", r.Code)
	}
	rec := do(t, h, http.MethodPut, path, "dev", renamed, map[string]string{"If-Match": `"1"`})
	if got := decodeDashboard(t, rec.Body.Bytes()); rec.Code != http.StatusOK || got.Revision != 2 || got.Title != "Checkout v2" || rec.Header().Get("ETag") != `"2"` {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	if r := do(t, h, http.MethodPut, path, "dev", renamed, map[string]string{"If-Match": `"1"`}); r.Code != http.StatusPreconditionFailed {
		t.Errorf("stale If-Match: %d", r.Code)
	}
	if r := do(t, h, http.MethodDelete, path, "dev", "", map[string]string{"If-Match": `"1"`}); r.Code != http.StatusPreconditionFailed {
		t.Errorf("stale delete: %d", r.Code)
	}
	if r := do(t, h, http.MethodDelete, path, "dev", "", map[string]string{"If-Match": `"2"`}); r.Code != http.StatusNoContent {
		t.Errorf("delete: %d", r.Code)
	}
	if r := do(t, h, http.MethodGet, path, "dev", "", nil); r.Code != http.StatusNotFound {
		t.Errorf("after delete: %d", r.Code)
	}
}

// 검증: 형식 오류 400(필드 위치), 아직 지원하지 않는 의미뿐이면 422, 너무 크면 413. API가 꺼지면 404.
func TestDashboardValidationAndFlag(t *testing.T) {
	h := dashboardHandler(t, newFakeDashboards(), principals{"dev": user(t, authz.RoleDeveloper)})
	idem := map[string]string{"Idempotency-Key": "k"}
	rec := do(t, h, http.MethodPost, "/api/v1/dashboards", "dev", `{"title":"x","widgets":[{"id":"a","kind":"text","text":"t"}],"layout":[{"widget_id":"a","x":20,"y":0,"w":6,"h":1}]}`, idem)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"layout[0]"`) {
		t.Errorf("layout beyond grid: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPost, "/api/v1/dashboards", "dev", `{"title":"x","widgets":[{"id":"a","kind":"histogram"}],"layout":[{"widget_id":"a","x":0,"y":0,"w":6,"h":1}]}`, idem)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("histogram: %d %s", rec.Code, rec.Body)
	}
	big := `{"title":"` + strings.Repeat("a", 300<<10) + `"}`
	if r := do(t, h, http.MethodPost, "/api/v1/dashboards", "dev", big, idem); r.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize: %d", r.Code)
	}
	off := dashboardHandler(t, nil, principals{"dev": user(t, authz.RoleDeveloper)})
	if r := do(t, off, http.MethodGet, "/api/v1/dashboards", "dev", "", nil); r.Code != http.StatusNotFound {
		t.Errorf("flag off: %d", r.Code)
	}
}

// 목록 cursor: 다음 page가 있으면 서명된 cursor, 다른 principal이 쓰면 400.
func TestDashboardListCursor(t *testing.T) {
	store := newFakeDashboards()
	h := dashboardHandler(t, store, principals{"dev": user(t, authz.RoleDeveloper), "viewer": user(t, authz.RoleViewer)})
	for i := range 3 {
		do(t, h, http.MethodPost, "/api/v1/dashboards", "dev", dashboardBody, map[string]string{"Idempotency-Key": fmt.Sprint("k", i)})
	}
	var page struct {
		Data       []DashboardItem `json:"data"`
		NextCursor *string         `json:"next_cursor"`
	}
	rec := do(t, h, http.MethodGet, "/api/v1/dashboards?limit=2", "dev", "", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Data) != 2 || page.NextCursor == nil {
		t.Fatalf("page 1: %s", rec.Body)
	}
	if r := do(t, h, http.MethodGet, "/api/v1/dashboards?cursor="+*page.NextCursor, "viewer", "", nil); r.Code != http.StatusBadRequest {
		t.Errorf("cursor reused by another principal: %d", r.Code)
	}
	rec = do(t, h, http.MethodGet, "/api/v1/dashboards?limit=2&cursor="+*page.NextCursor, "dev", "", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Data) != 1 || page.NextCursor != nil {
		t.Errorf("page 2: %s", rec.Body)
	}
}
