package session

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

var (
	tenantA = mustTenant("11111111-1111-4111-8111-111111111111")
	tenantB = mustTenant("22222222-2222-4222-8222-222222222222")
	t0      = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
)

func mustTenant(s string) authz.TenantID {
	t, err := authz.ParseTenantID(s)
	if err != nil {
		panic(err)
	}
	return t
}

// FakeStore는 시험용 session 저장소다. membership은 (tenant, user) → role이다.
type FakeStore struct {
	Members  map[[2]string]authz.Role
	Sessions map[[32]byte]*controldb.SessionRecord
	Touches  int
	Err      error
}

func NewFakeStore() *FakeStore {
	return &FakeStore{Members: map[[2]string]authz.Role{}, Sessions: map[[32]byte]*controldb.SessionRecord{}}
}

func (f *FakeStore) UserMemberships(_ context.Context, userID string) ([]controldb.Membership, error) {
	var out []controldb.Membership
	for _, t := range []authz.TenantID{tenantA, tenantB} {
		if r, ok := f.Members[[2]string{t.String(), userID}]; ok {
			out = append(out, controldb.Membership{Tenant: t, Role: r})
		}
	}
	return out, f.Err
}

func (f *FakeStore) CreateSession(_ context.Context, n controldb.NewSession) error {
	if f.Err != nil {
		return f.Err
	}
	if _, ok := f.Members[[2]string{n.Tenant.String(), n.UserID}]; !ok {
		return controldb.ErrNotMember
	}
	var name *string
	if n.DisplayName != "" {
		name = &n.DisplayName
	}
	f.Sessions[n.IDHash] = &controldb.SessionRecord{Tenant: n.Tenant, UserID: n.UserID, DisplayName: name, LastSeenAt: n.Now,
		IdleExpiresAt: n.IdleExpiresAt, ExpiresAt: n.ExpiresAt}
	return nil
}

func (f *FakeStore) LookupSession(_ context.Context, h [32]byte) (controldb.SessionRecord, error) {
	if f.Err != nil {
		return controldb.SessionRecord{}, f.Err
	}
	s, ok := f.Sessions[h]
	if !ok {
		return controldb.SessionRecord{}, controldb.ErrSessionNotFound
	}
	rec := *s
	rec.Role = f.Members[[2]string{s.Tenant.String(), s.UserID}]
	return rec, nil
}

func (f *FakeStore) TouchSession(_ context.Context, _ authz.TenantID, h [32]byte, now, idle time.Time) error {
	f.Touches++
	s := f.Sessions[h]
	s.LastSeenAt = now
	if idle.After(s.ExpiresAt) {
		idle = s.ExpiresAt
	}
	s.IdleExpiresAt = idle
	return nil
}

func (f *FakeStore) RevokeSession(_ context.Context, _ authz.TenantID, h [32]byte, _, _, _ string, now time.Time) error {
	if s, ok := f.Sessions[h]; ok && s.RevokedAt == nil {
		s.RevokedAt = &now
	}
	return nil
}

func newManager(t *testing.T, store Store, clock *time.Time) *Manager {
	t.Helper()
	m, err := New(Config{Store: store, Key: bytes.Repeat([]byte{7}, 32), AllowedOrigin: "https://app.example.com",
		Now: func() time.Time { return *clock }})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func request(method, token string, header map[string]string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), method, "/api/v1/dashboards", nil)
	if token != "" {
		r.AddCookie(&http.Cookie{Name: CookieName, Value: token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	for k, v := range header {
		r.Header.Set(k, v)
	}
	return r
}

func TestIssueAndAuthenticate(t *testing.T) {
	store := NewFakeStore()
	store.Members[[2]string{tenantA.String(), "oidc:aaa:alice"}] = authz.RoleDeveloper
	clock := t0
	m := newManager(t, store, &clock)
	s, err := m.Issue(context.Background(), tenantA, "oidc:aaa:alice", "Alice", "req-1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.Sessions) != 1 {
		t.Fatal("session not stored")
	}
	for h := range store.Sessions {
		if h == hashToken("") || bytes.Contains(h[:], []byte(s.Token)) {
			t.Fatal("token stored in clear")
		}
	}
	if !s.ExpiresAt.Equal(t0.Add(12 * time.Hour)) {
		t.Errorf("expires = %v", s.ExpiresAt)
	}
	c, err := m.Authenticate(request(http.MethodGet, s.Token, nil))
	if err != nil {
		t.Fatal(err)
	}
	p := c.Principal
	if p.Kind() != authz.KindUser || p.Tenant() != tenantA || p.Subject() != "oidc:aaa:alice" || authz.Authorize(p, authz.DashboardsWrite) != nil || authz.Authorize(p, authz.KeysManage) == nil {
		t.Errorf("principal = %v %v %v", p.Kind(), p.Tenant(), p.Subject())
	}
	if c.CSRFToken != s.CSRFToken || c.CSRFToken == "" || c.CSRFToken == s.Token {
		t.Error("csrf token")
	}
	// 다른 tenant의 member가 아니면 만들지 못한다
	if _, err := m.Issue(context.Background(), tenantB, "oidc:aaa:alice", "", "", time.Time{}); !errors.Is(err, controldb.ErrNotMember) {
		t.Errorf("issue for non-member tenant: %v", err)
	}
}

// 조직 전환처럼 이전 session에서 이어 만들 때 최대 만료가 늘지 않는다(12시간 상한, D04 §02).
func TestIssueNotAfter(t *testing.T) {
	store := NewFakeStore()
	store.Members[[2]string{tenantA.String(), "u1"}] = authz.RoleViewer
	clock := t0
	m := newManager(t, store, &clock)
	limit := t0.Add(3 * time.Hour)
	s, err := m.Issue(context.Background(), tenantA, "u1", "", "", limit)
	if err != nil || !s.ExpiresAt.Equal(limit) {
		t.Fatalf("expires = %v %v", s.ExpiresAt, err)
	}
	if _, err := m.Issue(context.Background(), tenantA, "u1", "", "", t0); !errors.Is(err, authz.ErrUnauthenticated) {
		t.Fatalf("issue past not-after: %v", err)
	}
}

func TestAuthenticateRejects(t *testing.T) {
	store := NewFakeStore()
	key := [2]string{tenantA.String(), "u1"}
	store.Members[key] = authz.RoleViewer
	clock := t0
	m := newManager(t, store, &clock)
	s, _ := m.Issue(context.Background(), tenantA, "u1", "", "", time.Time{})
	ok := func(r *http.Request) error { _, err := m.Authenticate(r); return err }

	// 형식이 틀린 cookie는 저장소를 보지 않고 거절
	if err := ok(request(http.MethodGet, "short", nil)); !errors.Is(err, authz.ErrUnauthenticated) {
		t.Errorf("malformed: %v", err)
	}
	if err := ok(request(http.MethodGet, strings.Repeat("A", len(s.Token)), nil)); !errors.Is(err, authz.ErrUnauthenticated) {
		t.Errorf("unknown token: %v", err)
	}
	// 변경 요청: CSRF 필수, Origin이 있으면 같아야 한다
	if err := ok(request(http.MethodPost, s.Token, nil)); !errors.Is(err, ErrCSRF) || !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("post without csrf: %v", err)
	}
	if err := ok(request(http.MethodPost, s.Token, map[string]string{CSRFHeader: "x" + s.CSRFToken})); !errors.Is(err, ErrCSRF) {
		t.Errorf("post with wrong csrf: %v", err)
	}
	if err := ok(request(http.MethodPost, s.Token, map[string]string{CSRFHeader: s.CSRFToken, "Origin": "https://evil.example"})); !errors.Is(err, ErrCSRF) {
		t.Errorf("post from other origin: %v", err)
	}
	if err := ok(request(http.MethodPost, s.Token, map[string]string{CSRFHeader: s.CSRFToken, "Origin": "https://app.example.com"})); err != nil {
		t.Errorf("post with csrf: %v", err)
	}
	// 역할 제거 → 다음 요청부터 미인증
	delete(store.Members, key)
	if err := ok(request(http.MethodGet, s.Token, nil)); !errors.Is(err, authz.ErrUnauthenticated) {
		t.Errorf("removed member: %v", err)
	}
	store.Members[key] = authz.RoleViewer
	// 폐기
	c, _ := m.Authenticate(request(http.MethodGet, s.Token, nil))
	_ = m.Revoke(context.Background(), c, "logout", "")
	if err := ok(request(http.MethodGet, s.Token, nil)); !errors.Is(err, authz.ErrUnauthenticated) {
		t.Errorf("revoked: %v", err)
	}
	// 저장소 장애는 그대로(503 fail closed)
	store.Err = errors.New("pg down")
	if err := ok(request(http.MethodGet, s.Token, nil)); err == nil || errors.Is(err, authz.ErrUnauthenticated) {
		t.Errorf("store error: %v", err)
	}
}

func TestIdleAndAbsoluteExpiry(t *testing.T) {
	store := NewFakeStore()
	store.Members[[2]string{tenantA.String(), "u1"}] = authz.RoleViewer
	clock := t0
	m := newManager(t, store, &clock)
	s, _ := m.Issue(context.Background(), tenantA, "u1", "", "", time.Time{})
	get := func() error { _, err := m.Authenticate(request(http.MethodGet, s.Token, nil)); return err }

	// 1분 안의 요청은 기록하지 않는다
	clock = t0.Add(30 * time.Second)
	if err := get(); err != nil || store.Touches != 0 {
		t.Fatalf("early: %v touches=%d", err, store.Touches)
	}
	// 29분마다 활동하면 idle로 끊기지 않는다
	for i := 1; i <= 24; i++ {
		clock = t0.Add(time.Duration(i) * 29 * time.Minute)
		if clock.Sub(t0) >= 12*time.Hour {
			break
		}
		if err := get(); err != nil {
			t.Fatalf("active at %v: %v", clock.Sub(t0), err)
		}
	}
	// 그래도 12시간이 되면 끝난다(연장은 최대 만료를 넘지 않는다)
	clock = t0.Add(12 * time.Hour)
	if err := get(); !errors.Is(err, authz.ErrUnauthenticated) {
		t.Fatalf("absolute expiry: %v", err)
	}

	// 30분 넘게 쉬면 끝난다
	s2, _ := m.Issue(context.Background(), tenantA, "u1", "", "", time.Time{})
	clock = clock.Add(30 * time.Minute)
	if _, err := m.Authenticate(request(http.MethodGet, s2.Token, nil)); !errors.Is(err, authz.ErrUnauthenticated) {
		t.Fatalf("idle expiry: %v", err)
	}
}

func TestLoginState(t *testing.T) {
	clock := t0
	m := newManager(t, NewFakeStore(), &clock)
	st, err := m.NewLoginState("/o/acme/services?range=1h")
	if err != nil {
		t.Fatal(err)
	}
	if st.State == st.Nonce || len(st.Verifier) < 43 || st.ReturnTo != "/o/acme/services?range=1h" {
		t.Fatalf("state = %+v", st)
	}
	w := httptest.NewRecorder()
	if err := m.SetLoginCookie(w, st); err != nil {
		t.Fatal(err)
	}
	cookie := w.Result().Cookies()[0]
	_ = w.Result().Body.Close()
	if cookie.Name != LoginCookieName || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode ||
		strings.Contains(cookie.Value, st.Verifier) || strings.Contains(cookie.Value, st.Nonce) {
		t.Fatalf("cookie = %+v", cookie)
	}
	take := func(value, state string) bool {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/auth/callback", nil)
		r.AddCookie(&http.Cookie{Name: LoginCookieName, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		rec := httptest.NewRecorder()
		_, ok := m.TakeLoginState(rec, r, state)
		res := rec.Result()
		_ = res.Body.Close()
		if c := res.Cookies(); len(c) != 1 || c[0].MaxAge >= 0 {
			t.Error("login cookie not cleared")
		}
		return ok
	}
	if !take(cookie.Value, st.State) {
		t.Fatal("valid state rejected")
	}
	if take(cookie.Value, "other") {
		t.Fatal("state mismatch accepted")
	}
	tampered := []byte(cookie.Value)
	tampered[10] ^= 1
	if take(string(tampered), st.State) {
		t.Fatal("tampered cookie accepted")
	}
	clock = t0.Add(11 * time.Minute)
	if take(cookie.Value, st.State) {
		t.Fatal("expired login state accepted")
	}
}

func TestSafeReturnTo(t *testing.T) {
	for in, want := range map[string]string{
		"/o/acme/map?tab=table":         "/o/acme/map?tab=table",
		"":                              "/",
		"https://evil.example/":         "/",
		"//evil.example/":               "/",
		"/\\evil.example":               "/",
		"/ok\r\nSet-Cookie:x":           "/",
		"javascript:alert(1)":           "/",
		"/" + strings.Repeat("a", 3000): "/",
	} {
		if got := SafeReturnTo(in); got != want {
			t.Errorf("SafeReturnTo(%q) = %q", in, got)
		}
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{Store: NewFakeStore(), Key: make([]byte, 16)}); err == nil {
		t.Error("short key accepted")
	}
	if _, err := New(Config{Key: make([]byte, 32)}); err == nil {
		t.Error("missing store accepted")
	}
	getenv := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if m, err := ManagerFromEnv(getenv(nil), NewFakeStore()); m != nil || err != nil {
		t.Error("session should be off without key")
	}
	if _, err := ManagerFromEnv(getenv(map[string]string{"MONTRACER_SESSION_KEY_HEX": "zz"}), NewFakeStore()); err == nil {
		t.Error("bad key accepted")
	}
	m, err := ManagerFromEnv(getenv(map[string]string{"MONTRACER_SESSION_KEY_HEX": strings.Repeat("ab", 32),
		"MONTRACER_PUBLIC_WEB_URL": "https://app.example.com/base"}), NewFakeStore())
	if err != nil || m.cfg.AllowedOrigin != "https://app.example.com" {
		t.Errorf("from env: %v %v", m, err)
	}
	if p, err := ProviderFromEnv(getenv(nil)); p != nil || err != nil {
		t.Error("oidc should be off without issuer")
	}
	if _, err := ProviderFromEnv(getenv(map[string]string{"MONTRACER_OIDC_ISSUER": "https://idp", "MONTRACER_OIDC_CLIENT_ID": "c",
		"MONTRACER_OIDC_REDIRECT_URL": "https://app/cb?x=1"})); err == nil {
		t.Error("redirect url with query accepted")
	}
}
