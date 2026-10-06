package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

var (
	tenantA = mustTenant("11111111-1111-4111-8111-111111111111")
	now     = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
)

func mustTenant(s string) authz.TenantID {
	t, err := authz.ParseTenantID(s)
	if err != nil {
		panic(err)
	}
	return t
}

// fakeAudit는 시간 내림차순 이벤트 목록에서 keyset으로 page를 자른다.
type fakeAudit struct {
	events  []controldb.AuditEvent
	queries []controldb.AuditQuery
	err     error
}

func (f *fakeAudit) ListAuditEvents(_ context.Context, p authz.Principal, q controldb.AuditQuery) ([]controldb.AuditEvent, bool, error) {
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, false, f.err
	}
	allowed, err := authz.AuditCategories(p)
	if err != nil {
		return nil, false, err
	}
	ok := map[string]bool{}
	for _, c := range allowed {
		ok[c] = true
	}
	for _, c := range q.Categories {
		if !ok[c] {
			return nil, false, authz.ErrForbidden
		}
	}
	cats := q.Categories
	if len(cats) == 0 {
		cats = allowed
	}
	want := map[string]bool{}
	for _, c := range cats {
		want[c] = true
	}
	var out []controldb.AuditEvent
	for _, e := range f.events {
		if !want[e.Category] || e.OccurredAt.Before(q.From) || !e.OccurredAt.Before(q.To) || (q.Action != "" && e.Action != q.Action) {
			continue
		}
		if q.After != nil && !(e.OccurredAt.Before(q.After.OccurredAt) || (e.OccurredAt.Equal(q.After.OccurredAt) && e.ID < q.After.ID)) {
			continue
		}
		out = append(out, e)
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return out, more, nil
}

func event(i int, cat, action string) controldb.AuditEvent {
	return controldb.AuditEvent{
		ID: "00000000-0000-4000-8000-0000000000" + string(rune('a'+i/10)) + string(rune('0'+i%10)), OccurredAt: now.Add(-time.Duration(i) * time.Minute),
		Category: cat, Action: action, ActorKind: "user", ActorID: "u1", ResourceType: "api_key", ResourceID: "k",
		Details: json.RawMessage(`{"reason":"x"}`),
	}
}

type principals map[string]authz.Principal

func newHandler(t *testing.T, store AuditStore, ps principals, clock *time.Time) *Handler {
	t.Helper()
	signer, err := apicursor.NewSigner([]byte("0123456789abcdef0123456789abcdef"), time.Hour, func() time.Time { return *clock })
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
		Audit: store, Cursor: signer, Now: func() time.Time { return *clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func user(t *testing.T, role authz.Role) authz.Principal {
	t.Helper()
	p, err := authz.NewUserPrincipal(tenantA, "u-"+string(role), role, time.Time{}, now)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type response struct {
	Data       []AuditEvent `json:"data"`
	NextCursor *string      `json:"next_cursor"`
	Meta       Meta         `json:"meta"`
	Err        *struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func get(t *testing.T, h http.Handler, token string, params url.Values) (int, response) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit-events?"+params.Encode(), nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %d %s: %v", rec.Code, rec.Body.String(), err)
	}
	return rec.Code, resp
}

func rangeParams(extra ...string) url.Values {
	v := url.Values{"from": {now.Add(-24 * time.Hour).Format(time.RFC3339)}, "to": {now.Add(time.Hour).Format(time.RFC3339)}}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	return v
}

// audit.read는 두 범주, audit.operations.read(Operator)는 operations만 본다(ADR 0015 §4).
func TestAuditCategoriesByRole(t *testing.T) {
	store := &fakeAudit{events: []controldb.AuditEvent{event(1, "security", "key.revoked"), event(2, "operations", "monitor.updated")}}
	clock := now
	h := newHandler(t, store, principals{"admin": user(t, authz.RoleTenantAdmin), "op": user(t, authz.RoleOperator), "dev": user(t, authz.RoleDeveloper)}, &clock)

	if code, resp := get(t, h, "admin", rangeParams()); code != 200 || len(resp.Data) != 2 {
		t.Fatalf("admin: %d %+v", code, resp)
	}
	code, resp := get(t, h, "op", rangeParams())
	if code != 200 || len(resp.Data) != 1 || resp.Data[0].Category != "operations" {
		t.Fatalf("operator: %d %+v", code, resp)
	}
	// operator가 security를 명시 요청하면 조용히 빼지 않고 403
	if code, resp := get(t, h, "op", rangeParams("category", "security")); code != 403 || resp.Err.Code != "FORBIDDEN" {
		t.Errorf("operator security: %d %+v", code, resp.Err)
	}
	if code, _ := get(t, h, "dev", rangeParams()); code != 403 {
		t.Errorf("developer: %d, want 403", code)
	}
	if code, _ := get(t, h, "", rangeParams()); code != 401 {
		t.Errorf("no token: %d, want 401", code)
	}
}

func TestAuditPaginationWithCursor(t *testing.T) {
	var events []controldb.AuditEvent
	for i := 1; i <= 5; i++ {
		events = append(events, event(i, "security", "key.created"))
	}
	store := &fakeAudit{events: events}
	clock := now
	h := newHandler(t, store, principals{"admin": user(t, authz.RoleTenantAdmin)}, &clock)

	var seen []string
	params := rangeParams("limit", "2")
	for page := 0; page < 5; page++ {
		code, resp := get(t, h, "admin", params)
		if code != 200 {
			t.Fatalf("page %d: %d %+v", page, code, resp.Err)
		}
		for _, e := range resp.Data {
			seen = append(seen, e.ID)
		}
		if resp.NextCursor == nil {
			break
		}
		params.Set("cursor", *resp.NextCursor)
		clock = clock.Add(time.Minute) // 다음 page 사이에 시간이 지나도 snapshot은 첫 page 시각
	}
	if len(seen) != 5 || seen[0] != events[0].ID || seen[4] != events[4].ID {
		t.Fatalf("seen = %v", seen)
	}
	// 이후 page의 상한은 첫 page snapshot(now)이다
	if q := store.queries[len(store.queries)-1]; !q.To.Equal(now) || q.After == nil {
		t.Errorf("last query = %+v, want To=snapshot and keyset position", q)
	}
}

// cursor는 다른 query·권한에 쓸 수 없다.
func TestAuditCursorBinding(t *testing.T) {
	var events []controldb.AuditEvent
	for i := 1; i <= 3; i++ {
		events = append(events, event(i, "operations", "monitor.updated"))
	}
	clock := now
	admin, op := user(t, authz.RoleTenantAdmin), user(t, authz.RoleOperator)
	h := newHandler(t, &fakeAudit{events: events}, principals{"admin": admin, "op": op}, &clock)
	_, resp := get(t, h, "admin", rangeParams("limit", "1"))
	if resp.NextCursor == nil {
		t.Fatal("no cursor")
	}
	cur := *resp.NextCursor
	for name, c := range map[string]struct {
		token  string
		params url.Values
	}{
		"other principal permissions": {"op", rangeParams("limit", "1", "cursor", cur)},
		"other query":                 {"admin", rangeParams("limit", "1", "cursor", cur, "action", "monitor.updated")},
		"tampered":                    {"admin", rangeParams("limit", "1", "cursor", cur+"x")},
	} {
		code, resp := get(t, h, c.token, c.params)
		if code != 400 || resp.Err == nil || resp.Err.Code != "INVALID_ARGUMENT" {
			t.Errorf("%s: %d %+v", name, code, resp.Err)
		}
	}
	// page 크기는 query hash에 들어가지 않는다
	if code, _ := get(t, h, "admin", rangeParams("limit", "2", "cursor", cur)); code != 200 {
		t.Errorf("different limit: %d", code)
	}
}

func TestAuditValidation(t *testing.T) {
	clock := now
	store := &fakeAudit{}
	h := newHandler(t, store, principals{"admin": user(t, authz.RoleTenantAdmin)}, &clock)
	for name, params := range map[string]url.Values{
		"no from":       {"to": {now.Format(time.RFC3339)}},
		"bad time":      {"from": {"yesterday"}, "to": {now.Format(time.RFC3339)}},
		"reversed":      {"from": {now.Format(time.RFC3339)}, "to": {now.Add(-time.Hour).Format(time.RFC3339)}},
		"range too big": {"from": {now.Add(-367 * 24 * time.Hour).Format(time.RFC3339)}, "to": {now.Format(time.RFC3339)}},
		"bad category":  rangeParams("category", "billing"),
		"limit zero":    rangeParams("limit", "0"),
		"limit big":     rangeParams("limit", "1001"),
		"action spaces": rangeParams("action", "key revoked"),
		"action long":   rangeParams("action", strings.Repeat("a", 129)),
	} {
		if code, resp := get(t, h, "admin", params); code != 400 || resp.Err.Code != "INVALID_ARGUMENT" {
			t.Errorf("%s: %d %+v", name, code, resp.Err)
		}
	}
	if len(store.queries) != 0 {
		t.Errorf("invalid requests reached the store: %d", len(store.queries))
	}
}

// 응답 형식: details는 객체 그대로, request_id는 null 허용, next_cursor는 없으면 null, 빈 결과는 [].
func TestAuditResponseShape(t *testing.T) {
	clock := now
	e := event(1, "security", "key.revoked")
	h := newHandler(t, &fakeAudit{events: []controldb.AuditEvent{e}}, principals{"admin": user(t, authz.RoleTenantAdmin)}, &clock)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit-events?"+rangeParams().Encode(), nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{`"details":{"reason":"x"}`, `"request_id":null`, `"next_cursor":null`, `"schema_version":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("audit response cacheable")
	}
	_, resp := get(t, h, "admin", url.Values{"from": {now.Add(-48 * time.Hour).Format(time.RFC3339)}, "to": {now.Add(-47 * time.Hour).Format(time.RFC3339)}})
	if resp.Data == nil || len(resp.Data) != 0 {
		t.Errorf("empty data = %#v, want []", resp.Data)
	}
}

func TestAuditStoreUnavailable(t *testing.T) {
	clock := now
	h := newHandler(t, &fakeAudit{err: errors.Join(authz.ErrBackendUnavailable)}, principals{"admin": user(t, authz.RoleTenantAdmin)}, &clock)
	if code, resp := get(t, h, "admin", rangeParams()); code != 503 {
		t.Errorf("store down: %d %+v", code, resp.Err)
	}
}

// break-glass 행: 운영자·승인자 ID는 가리고, 사유·ticket은 보인다.
func TestAuditMasksOperatorIdentity(t *testing.T) {
	clock := now
	e := event(1, "security", "key.revoked")
	e.ActorKind, e.ActorID = "operator", "op-kim"
	e.Details = json.RawMessage(`{"break_glass":{"approver":"sec-lee","ticket":"INC-1042","reason":"leaked key"},"revoke_at":"x"}`)
	h := newHandler(t, &fakeAudit{events: []controldb.AuditEvent{e}}, principals{"admin": user(t, authz.RoleTenantAdmin)}, &clock)
	_, resp := get(t, h, "admin", rangeParams())
	if len(resp.Data) != 1 {
		t.Fatalf("data = %+v", resp.Data)
	}
	got := resp.Data[0]
	d := string(got.Details)
	if got.ActorID != operatorActor || got.ActorKind != "operator" || strings.Contains(d, "sec-lee") || strings.Contains(d, "op-kim") ||
		!strings.Contains(d, "INC-1042") || !strings.Contains(d, "leaked key") || !strings.Contains(d, "revoke_at") {
		t.Errorf("operator row = %+v details=%s", got, d)
	}
}
