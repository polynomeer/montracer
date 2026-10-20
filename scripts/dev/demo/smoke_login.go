package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// 사람 로그인 smoke (ADR 0064·0065): 개발 IdP(scripts/dev/devidp)로 실제 OIDC 흐름을 지나 control-api가 만든
// browser session으로 두 API를 부른다. 브라우저처럼 cookie를 들고 다닌다(__Host- cookie는 Secure라
// net/http cookiejar가 http에서 보내지 않으므로 직접 다룬다).
type browserClient struct {
	cookies map[string]string
	client  *http.Client
}

func newBrowserClient() *browserClient {
	return &browserClient{cookies: map[string]string{}, client: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// reply는 다 읽은 응답이다(body는 닫았다).
type reply struct {
	StatusCode int
	Header     http.Header
}

func (b *browserClient) do(ctx context.Context, method, target, body string, header map[string]string) (reply, []byte, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return reply{}, nil, err
	}
	if strings.HasPrefix(body, "{") {
		req.Header.Set("Content-Type", "application/json")
	} else if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range b.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := b.client.Do(req)
	if err != nil {
		return reply{}, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return reply{}, nil, err
	}
	for _, c := range res.Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c.Value
		}
	}
	return reply{StatusCode: res.StatusCode, Header: res.Header}, raw, nil
}

// login은 로그인 시작 → IdP 사용자 선택 → callback을 지나 callback의 Location을 돌려준다.
func (b *browserClient) login(ctx context.Context, cfg config, user, returnTo string) (string, error) {
	res, _, err := b.do(ctx, http.MethodGet, cfg.controlURL+"/api/v1/auth/login?return_to="+url.QueryEscape(returnTo), "", nil)
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusFound {
		return "", fmt.Errorf("login start status %d", res.StatusCode)
	}
	authURL, err := url.Parse(res.Header.Get("Location"))
	if err != nil || !strings.HasPrefix(authURL.String(), cfg.oidcIssuer+"/authorize") {
		return "", fmt.Errorf("login start location %q", res.Header.Get("Location"))
	}
	form := authURL.Query()
	form.Set("user", user)
	res, _, err = b.do(ctx, http.MethodPost, cfg.oidcIssuer+"/authorize", form.Encode(), nil)
	if err != nil {
		return "", err
	}
	cb, err := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != http.StatusFound || err != nil {
		return "", fmt.Errorf("idp approve status %d", res.StatusCode)
	}
	// callback은 web dev server를 거치게 등록돼 있다. smoke는 같은 query로 control-api를 바로 부른다(redirect URI는 token 교환 때 맞는다)
	res, _, err = b.do(ctx, http.MethodGet, cfg.controlURL+"/api/v1/auth/callback?"+cb.RawQuery, "", nil)
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusSeeOther {
		return "", fmt.Errorf("callback status %d", res.StatusCode)
	}
	return res.Header.Get("Location"), nil
}

type smokeSession struct {
	Data struct {
		TenantID    string  `json:"tenant_id"`
		TenantName  *string `json:"tenant_name"`
		Role        string  `json:"role"`
		Memberships []struct {
			TenantID   string  `json:"tenant_id"`
			TenantName *string `json:"tenant_name"`
		} `json:"memberships"`
		CSRFToken string `json:"csrf_token"`
	} `json:"data"`
}

func (b *browserClient) session(ctx context.Context, cfg config) (smokeSession, int, error) {
	var s smokeSession
	res, raw, err := b.do(ctx, http.MethodGet, cfg.controlURL+"/api/v1/auth/session", "", nil)
	if err != nil {
		return s, 0, err
	}
	if res.StatusCode == http.StatusOK {
		err = json.Unmarshal(raw, &s)
	}
	return s, res.StatusCode, err
}

func name(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// smokeLogin은 개발 IdP 로그인 흐름 전체를 확인한다. DEMO_OIDC_ISSUER가 없으면 건너뛴다.
func smokeLogin(ctx context.Context, cfg config, anchor string) error {
	if cfg.oidcIssuer == "" {
		fmt.Println("  skip 로그인: DEMO_OIDC_ISSUER 없음")
		return nil
	}
	b := newBrowserClient()
	to, err := b.login(ctx, cfg, "dev-admin", "/o/acme/overview?range=1h")
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if to != "/o/acme/overview?range=1h" {
		return fmt.Errorf("login returned to %q", to)
	}
	s, code, err := b.session(ctx, cfg)
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("session (status %d): %w", code, err)
	}
	if name(s.Data.TenantName) != "acme" || s.Data.Role != "tenant_admin" || len(s.Data.Memberships) != 2 || s.Data.CSRFToken == "" {
		return fmt.Errorf("session = %+v", s.Data)
	}
	// session으로 조회 API: GET은 cookie만, POST 조회는 CSRF
	res, raw, err := b.do(ctx, http.MethodGet, cfg.queryURL+"/api/v1/services", "", nil)
	if err != nil || res.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"checkout"`) {
		return fmt.Errorf("services with session (%s): %w", raw, err)
	}
	body := `{"range":{"from":"` + anchor + `","to":"` + anchor + `"}}`
	if res, _, err := b.do(ctx, http.MethodPost, cfg.queryURL+"/api/v1/query/logs", body, nil); err != nil || res.StatusCode != http.StatusForbidden {
		return fmt.Errorf("POST without csrf should be 403 (got %d): %w", res.StatusCode, err)
	}
	// 조직 전환: globex에서는 조회자다
	var globex string
	for _, m := range s.Data.Memberships {
		if name(m.TenantName) == "globex" {
			globex = m.TenantID
		}
	}
	res, raw, err = b.do(ctx, http.MethodPost, cfg.controlURL+"/api/v1/auth/session/tenant", `{"tenant_id":"`+globex+`"}`,
		map[string]string{"X-CSRF-Token": s.Data.CSRFToken})
	if err != nil || res.StatusCode != http.StatusOK {
		return fmt.Errorf("switch to globex (%s): %w", raw, err)
	}
	var sw smokeSession
	if err := json.Unmarshal(raw, &sw); err != nil || name(sw.Data.TenantName) != "globex" || sw.Data.Role != "viewer" {
		return fmt.Errorf("switched = %+v: %w", sw.Data, err)
	}
	// 로그아웃 뒤에는 미인증
	if res, _, err := b.do(ctx, http.MethodPost, cfg.controlURL+"/api/v1/auth/logout", "", map[string]string{"X-CSRF-Token": sw.Data.CSRFToken}); err != nil || res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("logout (got %d): %w", res.StatusCode, err)
	}
	if _, code, _ := b.session(ctx, cfg); code != http.StatusUnauthorized {
		return fmt.Errorf("session after logout: %d", code)
	}
	// membership이 없는 사용자는 session을 받지 못한다(JIT 가입 없음)
	outsider := newBrowserClient()
	to, err = outsider.login(ctx, cfg, "dev-outsider", "/")
	if err != nil {
		return fmt.Errorf("outsider login: %w", err)
	}
	if to != "/login?error=no_membership" {
		return errors.New("outsider got " + to)
	}
	if _, code, _ := outsider.session(ctx, cfg); code != http.StatusUnauthorized {
		return fmt.Errorf("outsider session: %d", code)
	}
	ok1("로그인: 개발 IdP → session(acme 관리자) → 조회 API(GET·CSRF 없는 POST 403) → globex 전환(조회자) → 로그아웃, membership 없는 사용자 거절")
	return nil
}
