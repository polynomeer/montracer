package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/session"
)

// 개발 IdP가 control-api의 실제 OIDC client(internal/session)와 맞물린다: PKCE·state·nonce·ID token.
func TestDevIdPWithSessionClient(t *testing.T) {
	var s *Server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.Handler().ServeHTTP(w, r) }))
	t.Cleanup(ts.Close)
	redirect := "http://localhost:15173/api/v1/auth/callback"
	var err error
	if s, err = New(ts.URL, "montracer-web", "secret", []string{redirect}); err != nil {
		t.Fatal(err)
	}
	p, err := session.NewOIDCProvider(session.OIDCConfig{Issuer: ts.URL, ClientID: "montracer-web", ClientSecret: "secret", RedirectURL: redirect}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	verifier := "verifier-0123456789012345678901234567890123456789"
	authURL, err := p.AuthURL(ctx, "state-1", "nonce-1", verifier, session.AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)

	// 화면: 개발 사용자 목록
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/authorize?"+u.RawQuery, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	body := string(raw)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "dev-admin") || !strings.Contains(res.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("authorize page: %d %s", res.StatusCode, body)
	}

	type reply struct {
		StatusCode int
		Header     http.Header
	}
	approve := func(form url.Values) reply {
		t.Helper()
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/authorize", strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return reply{StatusCode: res.StatusCode, Header: res.Header}
	}
	form := u.Query()
	form.Set("user", "dev-admin")
	ap := approve(form)
	loc, _ := url.Parse(ap.Header.Get("Location"))
	if ap.StatusCode != http.StatusFound || loc.Host != "localhost:15173" || loc.Query().Get("state") != "state-1" || loc.Query().Get("iss") != ts.URL {
		t.Fatalf("approve: %d %s", ap.StatusCode, loc)
	}
	code := loc.Query().Get("code")
	id, err := p.Exchange(ctx, code, verifier, "nonce-1")
	if err != nil || id.Subject != "dev-admin" || id.Issuer != ts.URL || id.Name != "Dev Admin" {
		t.Fatalf("exchange: %+v %v", id, err)
	}
	// code는 한 번만
	if _, err := p.Exchange(ctx, code, verifier, "nonce-1"); !session.IsInvalidToken(err) {
		t.Fatalf("code reuse: %v", err)
	}

	// 등록되지 않은 redirect URI·PKCE 없음은 redirect하지 않고 거절
	bad := u.Query()
	bad.Set("redirect_uri", "https://evil.example/cb")
	bad.Set("user", "dev-admin")
	if res := approve(bad); res.StatusCode != http.StatusBadRequest || res.Header.Get("Location") != "" {
		t.Fatalf("unregistered redirect: %d", res.StatusCode)
	}
	plain := u.Query()
	plain.Del("code_challenge_method")
	plain.Set("user", "dev-admin")
	if res := approve(plain); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("no pkce: %d", res.StatusCode)
	}

	// 재인증(step-up): 요청한 acr_values의 첫 값을 acr로, 지금을 auth_time으로, amr은 mfa로 돌려준다(ADR 0066)
	reauthURL, err := p.AuthURL(ctx, "state-2", "nonce-2", verifier, session.AuthOptions{Reauthenticate: true, ACR: "mfa"})
	if err != nil {
		t.Fatal(err)
	}
	ru, _ := url.Parse(reauthURL)
	reform := ru.Query()
	reform.Set("user", "dev-admin")
	ap = approve(reform)
	loc, _ = url.Parse(ap.Header.Get("Location"))
	id, err = p.Exchange(ctx, loc.Query().Get("code"), verifier, "nonce-2")
	if err != nil || id.ACR != "mfa" || id.AuthTime.IsZero() || time.Since(id.AuthTime) > time.Minute || len(id.AMR) != 1 || id.AMR[0] != "mfa" {
		t.Fatalf("reauth exchange: %+v %v", id, err)
	}
	if id.Subject != "dev-admin" {
		t.Fatalf("reauth subject = %q", id.Subject)
	}
}
