package controlapi

import (
	"bytes"
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
	"github.com/polynomeer/montracer/internal/session"
	"github.com/polynomeer/montracer/tests/contract"
)

var tenantB = mustTenant("22222222-2222-4222-8222-222222222222")

// sessionStore는 시험용 session 저장소다.
type sessionStore struct {
	members  map[[2]string]authz.Role
	sessions map[[32]byte]*controldb.SessionRecord
	audits   []string
}

func newSessionStore() *sessionStore {
	return &sessionStore{members: map[[2]string]authz.Role{}, sessions: map[[32]byte]*controldb.SessionRecord{}}
}

func (f *sessionStore) UserMemberships(_ context.Context, userID string) ([]controldb.Membership, error) {
	var out []controldb.Membership
	for _, t := range []authz.TenantID{tenantA, tenantB} {
		if r, ok := f.members[[2]string{t.String(), userID}]; ok {
			out = append(out, controldb.Membership{Tenant: t, Role: r})
		}
	}
	return out, nil
}

func (f *sessionStore) CreateSession(_ context.Context, n controldb.NewSession) error {
	if _, ok := f.members[[2]string{n.Tenant.String(), n.UserID}]; !ok {
		return controldb.ErrNotMember
	}
	var name *string
	if n.DisplayName != "" {
		name = &n.DisplayName
	}
	f.sessions[n.IDHash] = &controldb.SessionRecord{Tenant: n.Tenant, UserID: n.UserID, DisplayName: name, LastSeenAt: n.Now,
		IdleExpiresAt: n.IdleExpiresAt, ExpiresAt: n.ExpiresAt}
	f.audits = append(f.audits, "session.created "+n.Tenant.String())
	return nil
}

func (f *sessionStore) LookupSession(_ context.Context, h [32]byte) (controldb.SessionRecord, error) {
	s, ok := f.sessions[h]
	if !ok {
		return controldb.SessionRecord{}, controldb.ErrSessionNotFound
	}
	rec := *s
	rec.Role = f.members[[2]string{s.Tenant.String(), s.UserID}]
	return rec, nil
}

func (f *sessionStore) TouchSession(context.Context, authz.TenantID, [32]byte, time.Time, time.Time) error {
	return nil
}

func (f *sessionStore) RevokeSession(_ context.Context, _ authz.TenantID, h [32]byte, _, reason, _ string, now time.Time) error {
	if s, ok := f.sessions[h]; ok && s.RevokedAt == nil {
		s.RevokedAt = &now
		f.audits = append(f.audits, "session.revoked "+reason)
	}
	return nil
}

// fakeIdP는 Provider 대역이다. code → identity.
type fakeIdP struct {
	codes    map[string]session.Identity
	verifier string
	nonce    string
	down     bool
}

func (f *fakeIdP) AuthURL(_ context.Context, state, nonce, verifier string) (string, error) {
	if f.down {
		return "", errors.New("discovery failed")
	}
	f.verifier, f.nonce = verifier, nonce
	return "https://idp.example.com/auth?state=" + url.QueryEscape(state), nil
}

func (f *fakeIdP) Exchange(_ context.Context, code, verifier, nonce string) (session.Identity, error) {
	id, ok := f.codes[code]
	if !ok || verifier != f.verifier || nonce != f.nonce {
		return session.Identity{}, errors.New("invalid") // 대역은 장애로 본다(검증 실패는 OIDCProvider 시험)
	}
	return id, nil
}

const issuer = "https://idp.example.com"

func authHandler(t *testing.T, store *sessionStore, idp session.Provider, clock *time.Time) *Handler {
	t.Helper()
	signer, _ := apicursor.NewSigner([]byte("0123456789abcdef0123456789abcdef"), time.Hour, func() time.Time { return *clock })
	m, err := session.New(session.Config{Store: store, Key: bytes.Repeat([]byte{3}, 32), AllowedOrigin: "https://app.example.com",
		Now: func() time.Time { return *clock }})
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(Config{
		Authenticate: func(context.Context, string) (authz.Principal, error) {
			return authz.Principal{}, authz.ErrUnauthenticated
		},
		Audit: &fakeAudit{}, Cursor: signer, Sessions: m, OIDC: idp, Dashboards: newFakeDashboards(),
		Now: func() time.Time { return *clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type browser struct {
	t       *testing.T
	h       http.Handler
	cookies map[string]string
}

func (b *browser) do(method, target string, body string, header map[string]string) *http.Response {
	b.t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(context.Background(), method, target, nil)
	} else {
		r = httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range b.cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	for k, v := range header {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	contract.Serve(b.h, rec, r)
	res := rec.Result()
	b.t.Cleanup(func() { _ = res.Body.Close() })
	for _, c := range res.Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c.Value
		}
	}
	return res
}

// login은 로그인 시작 → IdP code → callback을 거친다. callback의 Location을 돌려준다.
func (b *browser) login(idp *fakeIdP, sub, returnTo string) string {
	b.t.Helper()
	res := b.do(http.MethodGet, "/api/v1/auth/login?return_to="+url.QueryEscape(returnTo), "", nil)
	if res.StatusCode != http.StatusFound {
		b.t.Fatalf("login status %d", res.StatusCode)
	}
	state := mustURL(b.t, res.Header.Get("Location")).Query().Get("state")
	idp.codes["code-"+sub] = session.Identity{Issuer: issuer, Subject: sub, Name: "Alice"}
	cb := b.do(http.MethodGet, "/api/v1/auth/callback?code=code-"+sub+"&state="+url.QueryEscape(state)+"&session_state=x", "", nil)
	if cb.StatusCode != http.StatusSeeOther {
		b.t.Fatalf("callback status %d", cb.StatusCode)
	}
	return cb.Header.Get("Location")
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

type sessionBody struct {
	Data struct {
		UserID      string  `json:"user_id"`
		DisplayName *string `json:"display_name"`
		TenantID    string  `json:"tenant_id"`
		Role        string  `json:"role"`
		Memberships []struct {
			TenantID string `json:"tenant_id"`
			Role     string `json:"role"`
		} `json:"memberships"`
		CSRFToken string `json:"csrf_token"`
		ExpiresAt string `json:"expires_at"`
	} `json:"data"`
}

func decodeSession(t *testing.T, res *http.Response) sessionBody {
	t.Helper()
	var b sessionBody
	if err := json.NewDecoder(res.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLoginFlow(t *testing.T) {
	store := newSessionStore()
	alice, _ := authz.OIDCUserID(issuer, "alice")
	store.members[[2]string{tenantA.String(), alice}] = authz.RoleDeveloper
	store.members[[2]string{tenantB.String(), alice}] = authz.RoleViewer
	clock := now
	idp := &fakeIdP{codes: map[string]session.Identity{}}
	b := &browser{t: t, h: authHandler(t, store, idp, &clock), cookies: map[string]string{}}

	// 로그인 시작: 봉인 cookie와 IdP redirect, callback 뒤 session cookie와 원래 경로
	if to := b.login(idp, "alice", "/o/acme/map?range=1h"); to != "/o/acme/map?range=1h" {
		t.Fatalf("return to %q", to)
	}
	if b.cookies[session.CookieName] == "" || b.cookies[session.LoginCookieName] != "" {
		t.Fatalf("cookies after login = %v", b.cookies)
	}
	res := b.do(http.MethodGet, "/api/v1/auth/session", "", nil)
	if res.StatusCode != http.StatusOK || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("session status %d", res.StatusCode)
	}
	s := decodeSession(t, res)
	if s.Data.UserID != alice || s.Data.TenantID != tenantA.String() || s.Data.Role != "developer" || len(s.Data.Memberships) != 2 ||
		s.Data.CSRFToken == "" || s.Data.DisplayName == nil || *s.Data.DisplayName != "Alice" {
		t.Fatalf("session = %+v", s.Data)
	}

	// session으로 관리 API를 부른다(GET은 CSRF 불필요, 변경은 필요)
	if res := b.do(http.MethodGet, "/api/v1/dashboards", "", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("dashboards with session: %d", res.StatusCode)
	}
	create := dashboardBody
	if res := b.do(http.MethodPost, "/api/v1/dashboards", create, map[string]string{"Idempotency-Key": "k-1"}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("mutation without csrf: %d", res.StatusCode)
	}
	if res := b.do(http.MethodPost, "/api/v1/dashboards", create, map[string]string{"Idempotency-Key": "k-2", session.CSRFHeader: s.Data.CSRFToken,
		"Origin": "https://evil.example"}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("mutation from other origin: %d", res.StatusCode)
	}
	if res := b.do(http.MethodPost, "/api/v1/dashboards", create, map[string]string{"Idempotency-Key": "k-3", session.CSRFHeader: s.Data.CSRFToken,
		"Origin": "https://app.example.com"}); res.StatusCode != http.StatusCreated {
		t.Fatalf("mutation with csrf: %d", res.StatusCode)
	}

	// 다른 조직으로 바꾸기: 새 token, 이전 session 폐기. 최대 만료는 늘지 않는다(20분 뒤 바꿔도 처음 로그인 기준)
	clock = clock.Add(20 * time.Minute)
	old := b.cookies[session.CookieName]
	res = b.do(http.MethodPost, "/api/v1/auth/session/tenant", `{"tenant_id":"`+tenantB.String()+`"}`, map[string]string{session.CSRFHeader: s.Data.CSRFToken})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("switch status %d", res.StatusCode)
	}
	s2 := decodeSession(t, res)
	if s2.Data.TenantID != tenantB.String() || s2.Data.Role != "viewer" || b.cookies[session.CookieName] == old || s2.Data.CSRFToken == s.Data.CSRFToken ||
		s2.Data.ExpiresAt != s.Data.ExpiresAt {
		t.Fatalf("switched = %+v", s2.Data)
	}
	stale := &browser{t: t, h: b.h, cookies: map[string]string{session.CookieName: old}}
	if res := stale.do(http.MethodGet, "/api/v1/auth/session", "", nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session after switch: %d", res.StatusCode)
	}
	// member가 아닌 조직으로는 못 바꾼다
	other := "33333333-3333-4333-8333-333333333333"
	if res := b.do(http.MethodPost, "/api/v1/auth/session/tenant", `{"tenant_id":"`+other+`"}`, map[string]string{session.CSRFHeader: s2.Data.CSRFToken}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("switch to non-member tenant: %d", res.StatusCode)
	}

	// 로그아웃: CSRF 필수, cookie 삭제, 이후 미인증
	if res := b.do(http.MethodPost, "/api/v1/auth/logout", "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("logout without csrf: %d", res.StatusCode)
	}
	if res := b.do(http.MethodPost, "/api/v1/auth/logout", "", map[string]string{session.CSRFHeader: s2.Data.CSRFToken}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", res.StatusCode)
	}
	if b.cookies[session.CookieName] != "" {
		t.Fatal("session cookie not cleared")
	}
	if strings.Join(store.audits, ",") != "session.created "+tenantA.String()+",session.created "+tenantB.String()+",session.revoked tenant_switch,session.revoked logout" {
		t.Errorf("audits = %v", store.audits)
	}
}

func TestLoginFailures(t *testing.T) {
	store := newSessionStore()
	clock := now
	idp := &fakeIdP{codes: map[string]session.Identity{}}
	h := authHandler(t, store, idp, &clock)
	loginErr := func(res *http.Response) string {
		t.Helper()
		if res.StatusCode != http.StatusSeeOther {
			t.Fatalf("status %d", res.StatusCode)
		}
		u := mustURL(t, res.Header.Get("Location"))
		if u.Path != "/login" {
			t.Fatalf("location %s", u)
		}
		return u.Query().Get("error")
	}

	// member가 아니면 session을 만들지 않는다(JIT 가입 없음)
	b := &browser{t: t, h: h, cookies: map[string]string{}}
	res := b.do(http.MethodGet, "/api/v1/auth/login?return_to=https://evil.example/", "", nil)
	state := mustURL(t, res.Header.Get("Location")).Query().Get("state")
	idp.codes["c"] = session.Identity{Issuer: issuer, Subject: "mallory"}
	if e := loginErr(b.do(http.MethodGet, "/api/v1/auth/callback?code=c&state="+url.QueryEscape(state), "", nil)); e != session.LoginErrNoMembership {
		t.Fatalf("no membership: %s", e)
	}
	if len(store.sessions) != 0 {
		t.Fatal("session created without membership")
	}

	// state가 다르거나 로그인 cookie가 없으면(CSRF 로그인) 거절
	b2 := &browser{t: t, h: h, cookies: map[string]string{}}
	b2.do(http.MethodGet, "/api/v1/auth/login", "", nil)
	if e := loginErr(b2.do(http.MethodGet, "/api/v1/auth/callback?code=c&state=forged", "", nil)); e != session.LoginErrState {
		t.Fatalf("forged state: %s", e)
	}
	b3 := &browser{t: t, h: h, cookies: map[string]string{}}
	if e := loginErr(b3.do(http.MethodGet, "/api/v1/auth/callback?code=c&state=x", "", nil)); e != session.LoginErrState {
		t.Fatalf("no login cookie: %s", e)
	}

	// IdP가 오류를 돌려주면 문구를 옮기지 않는다
	b4 := &browser{t: t, h: h, cookies: map[string]string{}}
	res = b4.do(http.MethodGet, "/api/v1/auth/login", "", nil)
	state = mustURL(t, res.Header.Get("Location")).Query().Get("state")
	if e := loginErr(b4.do(http.MethodGet, "/api/v1/auth/callback?error=access_denied&error_description=%3Cscript%3E&state="+url.QueryEscape(state), "", nil)); e != session.LoginErrIdP {
		t.Fatalf("idp error: %s", e)
	}

	// IdP에 닿지 않으면 unavailable
	idp.down = true
	b5 := &browser{t: t, h: h, cookies: map[string]string{}}
	if e := loginErr(b5.do(http.MethodGet, "/api/v1/auth/login", "", nil)); e != session.LoginErrUnavailable {
		t.Fatalf("idp down: %s", e)
	}

	// session 없이 /auth/session은 401, bearer key로도 session API는 쓰지 않는다
	if res := b5.do(http.MethodGet, "/api/v1/auth/session", "", map[string]string{"Authorization": "Bearer mt_api_x"}); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session without cookie: %d", res.StatusCode)
	}
}

// 로그인 설정이 없으면(rollback) 경로는 404이고 bearer key 인증은 그대로다.
func TestAuthDisabled(t *testing.T) {
	clock := now
	h := newHandler(t, &fakeAudit{}, principals{}, &clock)
	for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/callback", "/api/v1/auth/session"} {
		b := &browser{t: t, h: h, cookies: map[string]string{}}
		if res := b.do(http.MethodGet, path, "", nil); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	// session cookie가 있어도 session이 꺼져 있으면 미인증
	b := &browser{t: t, h: h, cookies: map[string]string{session.CookieName: strings.Repeat("A", 43)}}
	if res := b.do(http.MethodGet, "/api/v1/audit-events?"+rangeParams().Encode(), "", nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("cookie with sessions off: %d", res.StatusCode)
	}
}
