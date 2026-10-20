package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
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
	return b.viaIdP(ctx, cfg, "/api/v1/auth/login?return_to="+url.QueryEscape(returnTo), user)
}

// stepUp은 POST로 재인증을 시작(CSRF)하고 IdP 사용자 선택 → callback을 지나 callback의 Location을 돌려준다(ADR 0066).
func (b *browserClient) stepUp(ctx context.Context, cfg config, csrf, user, returnTo string) (string, error) {
	body, err := json.Marshal(map[string]string{"return_to": returnTo})
	if err != nil {
		return "", err
	}
	res, raw, err := b.do(ctx, http.MethodPost, cfg.controlURL+"/api/v1/auth/step-up", string(body), map[string]string{"X-CSRF-Token": csrf})
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("step-up start status %d", res.StatusCode)
	}
	var start struct {
		Data struct {
			AuthorizationURL string `json:"authorization_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &start); err != nil {
		return "", err
	}
	return b.approve(ctx, cfg, start.Data.AuthorizationURL, user)
}

// viaIdP는 start(로그인 시작 경로)의 IdP redirect를 따라 user로 승인하고 callback까지 지난다.
func (b *browserClient) viaIdP(ctx context.Context, cfg config, start, user string) (string, error) {
	res, _, err := b.do(ctx, http.MethodGet, cfg.controlURL+start, "", nil)
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusFound {
		return "", fmt.Errorf("start status %d", res.StatusCode)
	}
	return b.approve(ctx, cfg, res.Header.Get("Location"), user)
}

// approve는 IdP authorization URL에서 user로 승인하고 callback까지 지나 callback의 Location을 돌려준다.
func (b *browserClient) approve(ctx context.Context, cfg config, rawAuthURL, user string) (string, error) {
	authURL, err := url.Parse(rawAuthURL)
	if err != nil || !strings.HasPrefix(authURL.String(), cfg.oidcIssuer+"/authorize") {
		return "", fmt.Errorf("authorization url %q", rawAuthURL)
	}
	form := authURL.Query()
	form.Set("user", user)
	res, _, err := b.do(ctx, http.MethodPost, cfg.oidcIssuer+"/authorize", form.Encode(), nil)
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
		CSRFToken       string  `json:"csrf_token"`
		StepUpExpiresAt *string `json:"step_up_expires_at"`
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

// expectServices는 session으로 서비스 목록을 읽어 want가 있고 absent가 없는지 본다(조직 격리).
func (b *browserClient) expectServices(ctx context.Context, cfg config, want, absent string) error {
	res, raw, err := b.do(ctx, http.MethodGet, cfg.queryURL+"/api/v1/services", "", nil)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("services status %d", res.StatusCode)
	}
	if !strings.Contains(string(raw), `"name":"`+want+`"`) || strings.Contains(string(raw), `"name":"`+absent+`"`) {
		return fmt.Errorf("services want %q without %q: %s", want, absent, raw)
	}
	return nil
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
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("session status %d", code)
	}
	if name(s.Data.TenantName) != "acme" || s.Data.Role != "tenant_admin" || len(s.Data.Memberships) != 2 || s.Data.CSRFToken == "" ||
		s.Data.StepUpExpiresAt != nil {
		return fmt.Errorf("session = %+v", s.Data)
	}
	// MFA step-up: 같은 사용자로 재인증하면 15분 동안 민감 작업을 할 수 있다. 다른 사용자로 재인증하면 거절(ADR 0066)
	to, err = b.stepUp(ctx, cfg, s.Data.CSRFToken, "dev-viewer", "/o/acme/settings")
	if err != nil {
		return fmt.Errorf("step-up as another user: %w", err)
	}
	if !strings.HasPrefix(to, "/login?error=step_up_rejected") {
		return fmt.Errorf("step-up as another user returned to %q", to)
	}
	to, err = b.stepUp(ctx, cfg, s.Data.CSRFToken, "dev-admin", "/o/acme/settings")
	if err != nil {
		return fmt.Errorf("step-up: %w", err)
	}
	if to != "/o/acme/settings" {
		return fmt.Errorf("step-up returned to %q", to)
	}
	if s, _, err = b.session(ctx, cfg); err != nil {
		return fmt.Errorf("session after step-up: %w", err)
	}
	if s.Data.StepUpExpiresAt == nil {
		return errors.New("session after step-up has no step_up_expires_at")
	}
	if err := smokeMembers(ctx, cfg, b, s.Data.CSRFToken); err != nil {
		return fmt.Errorf("members: %w", err)
	}
	if err := smokeKeys(ctx, cfg, b, s.Data.CSRFToken); err != nil {
		return fmt.Errorf("keys: %w", err)
	}
	// session으로 조회 API: GET은 cookie만, POST 조회는 CSRF
	if err := b.expectServices(ctx, cfg, "checkout", "inventory"); err != nil {
		return fmt.Errorf("acme session: %w", err)
	}
	body := `{"range":{"from":"` + anchor + `","to":"` + anchor + `"}}`
	res, _, err := b.do(ctx, http.MethodPost, cfg.queryURL+"/api/v1/query/logs", body, nil)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusForbidden {
		return fmt.Errorf("POST without csrf: status %d, want 403", res.StatusCode)
	}
	// 조직 전환: globex에서는 조회자다
	var globex string
	for _, m := range s.Data.Memberships {
		if name(m.TenantName) == "globex" {
			globex = m.TenantID
		}
	}
	res, raw, err := b.do(ctx, http.MethodPost, cfg.controlURL+"/api/v1/auth/session/tenant", `{"tenant_id":"`+globex+`"}`,
		map[string]string{"X-CSRF-Token": s.Data.CSRFToken})
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("switch to globex: status %d %s", res.StatusCode, raw)
	}
	var sw smokeSession
	if err := json.Unmarshal(raw, &sw); err != nil {
		return err
	}
	if name(sw.Data.TenantName) != "globex" || sw.Data.Role != "viewer" {
		return fmt.Errorf("switched = %+v", sw.Data)
	}
	// 바꾼 뒤에는 globex 데이터만 보인다(acme의 checkout이 사라진다)
	if err := b.expectServices(ctx, cfg, "inventory", "checkout"); err != nil {
		return fmt.Errorf("globex session: %w", err)
	}
	// 로그아웃 뒤에는 미인증
	res, _, err = b.do(ctx, http.MethodPost, cfg.controlURL+"/api/v1/auth/logout", "", map[string]string{"X-CSRF-Token": sw.Data.CSRFToken})
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("logout status %d", res.StatusCode)
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
	ok1("로그인: 개발 IdP → session(acme 관리자) → step-up(다른 사용자 거절) → 멤버 초대·수락·제거 → API key 발급·사용·폐기 → 조회 API(GET·CSRF 없는 POST 403) → globex 전환(조회자) → 로그아웃, membership 없는 사용자 거절")
	return nil
}

// smokeMembers는 멤버 관리(ADR 0067)를 확인한다: step-up한 acme 관리자(admin)가 dev-outsider를 조회자로 초대하고, outsider가
// 초대 링크로 로그인하면 acme 조회자 session이 생긴다. 관리자가 빼면 그 session은 끝난다(seed 상태로 되돌린다).
func smokeMembers(ctx context.Context, cfg config, admin *browserClient, csrf string) error {
	outsider, err := authz.OIDCUserID(cfg.oidcIssuer, "dev-outsider")
	if err != nil {
		return err
	}
	write := map[string]string{"X-CSRF-Token": csrf}
	memberURL := cfg.controlURL + "/api/v1/members/" + url.PathEscape(outsider)
	// 앞선 smoke가 중간에 멈췄으면 남아 있을 수 있다
	if res, _, err := admin.do(ctx, http.MethodDelete, memberURL, "", write); err != nil || (res.StatusCode != http.StatusNoContent && res.StatusCode != http.StatusNotFound) {
		return fmt.Errorf("cleanup outsider: %d %w", res.StatusCode, err)
	}
	res, raw, err := admin.do(ctx, http.MethodGet, cfg.controlURL+"/api/v1/members", "", nil)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"is_self":true`) || strings.Contains(string(raw), outsider) {
		return fmt.Errorf("members list: %d %s", res.StatusCode, raw)
	}
	write["Idempotency-Key"] = "smoke-invite-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	res, raw, err = admin.do(ctx, http.MethodPost, cfg.controlURL+"/api/v1/invitations", `{"role":"viewer","label":"smoke"}`, write)
	if err != nil {
		return err
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
		InvitationURL string `json:"invitation_url"`
	}
	if res.StatusCode != http.StatusCreated || json.Unmarshal(raw, &created) != nil {
		return fmt.Errorf("create invitation: %d %s", res.StatusCode, raw)
	}
	_, token, ok := strings.Cut(created.InvitationURL, "#")
	if !ok {
		return fmt.Errorf("invitation url %q", created.InvitationURL)
	}
	guest := newBrowserClient()
	body, _ := json.Marshal(map[string]string{"token": token})
	res, raw, err = guest.do(ctx, http.MethodPost, cfg.controlURL+"/api/v1/auth/invitation", string(body), nil)
	if err != nil {
		return err
	}
	var start struct {
		Data struct {
			AuthorizationURL string `json:"authorization_url"`
		} `json:"data"`
	}
	if res.StatusCode != http.StatusOK || json.Unmarshal(raw, &start) != nil {
		return fmt.Errorf("start invitation: %d %s", res.StatusCode, raw)
	}
	to, err := guest.approve(ctx, cfg, start.Data.AuthorizationURL, "dev-outsider")
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	if to != "/" {
		return fmt.Errorf("accept returned to %q", to)
	}
	gs, code, err := guest.session(ctx, cfg)
	if err != nil || code != http.StatusOK || name(gs.Data.TenantName) != "acme" || gs.Data.Role != "viewer" {
		return fmt.Errorf("invited session = %d %+v: %w", code, gs.Data, err)
	}
	// 같은 링크는 다시 못 쓴다
	if res, _, err := newBrowserClient().do(ctx, http.MethodPost, cfg.controlURL+"/api/v1/auth/invitation", string(body), nil); err != nil || res.StatusCode != http.StatusNotFound {
		return fmt.Errorf("reuse invitation: %d %w", res.StatusCode, err)
	}
	delete(write, "Idempotency-Key")
	if res, _, err := admin.do(ctx, http.MethodDelete, memberURL, "", write); err != nil || res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("remove outsider: %d %w", res.StatusCode, err)
	}
	if _, code, _ := guest.session(ctx, cfg); code != http.StatusUnauthorized {
		return fmt.Errorf("removed member's session: %d", code)
	}
	return nil
}

// smokeKeys는 API key 관리(ADR 0069)를 확인한다: step-up한 acme 관리자가 조회 key를 발급하면 그 key로 query-api를 부를 수 있고,
// 폐기하면 바로 401이다.
func smokeKeys(ctx context.Context, cfg config, admin *browserClient, csrf string) error {
	write := map[string]string{"X-CSRF-Token": csrf, "Idempotency-Key": "smoke-key-" + strconv.FormatInt(time.Now().UnixNano(), 36)}
	res, raw, err := admin.do(ctx, http.MethodPost, cfg.controlURL+"/api/v1/keys",
		`{"kind":"api","name":"smoke","scopes":["telemetry.read"],"expires_in_days":1}`, write)
	if err != nil {
		return err
	}
	var created struct {
		Data struct {
			KeyID string `json:"key_id"`
		} `json:"data"`
		Token string `json:"token"`
	}
	if res.StatusCode != http.StatusCreated || json.Unmarshal(raw, &created) != nil || created.Token == "" {
		return fmt.Errorf("create key: %d %s", res.StatusCode, raw)
	}
	bearer := map[string]string{"Authorization": "Bearer " + created.Token}
	keyClient := newBrowserClient()
	if res, _, err := keyClient.do(ctx, http.MethodGet, cfg.queryURL+"/api/v1/services", "", bearer); err != nil || res.StatusCode != http.StatusOK {
		return fmt.Errorf("services with new key: %d %w", res.StatusCode, err)
	}
	delete(write, "Idempotency-Key")
	if res, _, err := admin.do(ctx, http.MethodDelete, cfg.controlURL+"/api/v1/keys/"+created.Data.KeyID, "", write); err != nil || res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("revoke key: %d %w", res.StatusCode, err)
	}
	if res, _, err := keyClient.do(ctx, http.MethodGet, cfg.queryURL+"/api/v1/services", "", bearer); err != nil || res.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("services with revoked key: %d %w", res.StatusCode, err)
	}
	return nil
}
