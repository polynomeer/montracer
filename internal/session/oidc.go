package session

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	// LoginCookieName은 로그인 중 state·nonce·PKCE verifier를 봉인해 두는 cookie다(10분).
	LoginCookieName = "__Host-mt_login"
	loginTTL        = 10 * time.Minute
	maxReturnTo     = 2048
)

// 로그인 실패 사유 (화면 /login?error=…). IdP 응답 원문은 싣지 않는다.
const (
	LoginErrState        = "state_mismatch"
	LoginErrIdP          = "idp_error"
	LoginErrToken        = "token_invalid"
	LoginErrNoMembership = "no_membership"
	LoginErrUnavailable  = "unavailable"
)

// Identity는 검증한 ID token의 사용자다.
type Identity struct {
	Issuer, Subject string
	// Name은 name claim이다(화면 표시, 없으면 ""). email은 받지 않는다 — 식별자로 쓰지 않고 PII를 늘리지 않는다.
	Name string
}

// Provider는 OIDC IdP다. AuthURL은 PKCE S256 challenge와 nonce를 붙인 authorization URL이고,
// Exchange는 code를 verifier와 함께 token으로 바꾸고 ID token(서명·issuer·audience·만료·nonce)을 검증한다.
type Provider interface {
	AuthURL(ctx context.Context, state, nonce, verifier string) (string, error)
	Exchange(ctx context.Context, code, verifier, nonce string) (Identity, error)
}

// OIDCConfig는 OIDC client 설정이다.
type OIDCConfig struct {
	Issuer       string // discovery 기준(/.well-known/openid-configuration)
	ClientID     string
	ClientSecret string // confidential client. public client면 "" (PKCE만)
	// RedirectURL은 고정 callback URL이다(D04 §02 "redirect URI는 고정 allowlist"). 요청 값으로 바꾸지 않는다.
	RedirectURL string
}

// OIDCProvider는 go-oidc 기반 Provider다. discovery는 처음 쓸 때 하고, 실패하면 다음 로그인에서 다시 한다
// (IdP 장애가 control-api 기동을 막지 않게).
type OIDCProvider struct {
	cfg      OIDCConfig
	client   *http.Client
	mu       sync.Mutex
	provider *oidc.Provider
}

// NewOIDCProvider는 Provider를 만든다. client가 nil이면 10초 timeout client를 쓴다.
func NewOIDCProvider(cfg OIDCConfig, client *http.Client) (*OIDCProvider, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		return nil, errors.New("session: oidc issuer, client id and redirect url are required")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &OIDCProvider{cfg: cfg, client: client}, nil
}

func (p *OIDCProvider) discover(ctx context.Context) (*oidc.Provider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.provider != nil {
		return p.provider, nil
	}
	prov, err := oidc.NewProvider(oidc.ClientContext(ctx, p.client), p.cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("session: oidc discovery: %w", err)
	}
	p.provider = prov
	return prov, nil
}

func (p *OIDCProvider) oauth(prov *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{ClientID: p.cfg.ClientID, ClientSecret: p.cfg.ClientSecret, Endpoint: prov.Endpoint(),
		RedirectURL: p.cfg.RedirectURL, Scopes: []string{oidc.ScopeOpenID, "profile"}}
}

// AuthURL은 authorization URL이다.
func (p *OIDCProvider) AuthURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	prov, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	return p.oauth(prov).AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), nil
}

// Exchange는 code를 바꾸고 ID token을 검증한다.
func (p *OIDCProvider) Exchange(ctx context.Context, code, verifier, nonce string) (Identity, error) {
	prov, err := p.discover(ctx)
	if err != nil {
		return Identity{}, err
	}
	ctx = oidc.ClientContext(ctx, p.client)
	tok, err := p.oauth(prov).Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		// IdP가 4xx(invalid_grant 등)로 거절했을 때만 검증 실패다. 연결 오류·timeout·5xx는 IdP 장애다(로그·unavailable).
		// 응답 본문은 싣지 않는다(code·token이 섞일 수 있다).
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.Response != nil && re.Response.StatusCode >= 400 && re.Response.StatusCode < 500 {
			return Identity{}, fmt.Errorf("session: token exchange rejected (%d): %w", re.Response.StatusCode, errInvalidToken)
		}
		if errors.As(err, &re) && re.Response != nil {
			return Identity{}, fmt.Errorf("session: token endpoint status %d", re.Response.StatusCode)
		}
		return Identity{}, errors.New("session: token endpoint unreachable")
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return Identity{}, fmt.Errorf("session: no id_token: %w", errInvalidToken)
	}
	idt, err := prov.Verifier(&oidc.Config{ClientID: p.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("session: id_token: %w", errInvalidToken)
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		return Identity{}, fmt.Errorf("session: nonce mismatch: %w", errInvalidToken)
	}
	var claims struct {
		Name string `json:"name"`
	}
	_ = idt.Claims(&claims)
	return Identity{Issuer: idt.Issuer, Subject: idt.Subject, Name: truncate(claims.Name, 256)}, nil
}

// errInvalidToken은 IdP 응답이 검증을 통과하지 못했다는 뜻이다(LoginErrToken). 원인 문구에 token을 싣지 않는다.
var errInvalidToken = errors.New("invalid token")

// IsInvalidToken은 Exchange 실패가 검증 실패(IdP 장애가 아님)인지다.
func IsInvalidToken(err error) bool { return errors.Is(err, errInvalidToken) }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// LoginState는 로그인 중 브라우저에 봉인해 두는 값이다.
type LoginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	ReturnTo string `json:"r"`
	Expires  int64  `json:"e"`
}

// NewLoginState는 무작위 state·nonce·PKCE verifier로 로그인 state를 만든다. returnTo는 SafeReturnTo로 거른다.
func (m *Manager) NewLoginState(returnTo string) (LoginState, error) {
	rnd := func() (string, error) {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("session: random: %w", err)
		}
		return base64.RawURLEncoding.EncodeToString(b[:]), nil
	}
	state, err := rnd()
	if err != nil {
		return LoginState{}, err
	}
	nonce, err := rnd()
	if err != nil {
		return LoginState{}, err
	}
	return LoginState{State: state, Nonce: nonce, Verifier: oauth2.GenerateVerifier(), ReturnTo: SafeReturnTo(returnTo),
		Expires: m.cfg.Now().Add(loginTTL).Unix()}, nil
}

// SafeReturnTo는 로그인 뒤 돌아갈 같은 사이트 경로다. 절대 URL·scheme 상대(//)·역슬래시·제어 문자는 "/"로 바꾼다(open redirect 방지).
func SafeReturnTo(v string) string {
	if v == "" || len(v) > maxReturnTo || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.ContainsAny(v, "\\\r\n\t") {
		return "/"
	}
	for _, c := range v {
		if c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	return v
}

// SetLoginCookie는 봉인한 로그인 state cookie를 쓴다.
func (m *Manager) SetLoginCookie(w http.ResponseWriter, s LoginState) error {
	v, err := m.seal(s)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: LoginCookieName, Value: v, Path: "/", MaxAge: int(loginTTL.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	return nil
}

// TakeLoginState는 로그인 cookie를 열어 돌려주고 지운다(한 번만 쓴다). state가 다르거나, 만료·변조됐으면 ok=false다.
func (m *Manager) TakeLoginState(w http.ResponseWriter, r *http.Request, state string) (LoginState, bool) {
	http.SetCookie(w, &http.Cookie{Name: LoginCookieName, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	c, err := r.Cookie(LoginCookieName)
	if err != nil {
		return LoginState{}, false
	}
	s, err := m.unseal(c.Value)
	if err != nil || m.cfg.Now().Unix() > s.Expires || state == "" ||
		subtle.ConstantTimeCompare([]byte(s.State), []byte(state)) != 1 {
		return LoginState{}, false
	}
	return s, true
}

// seal은 AES-256-GCM으로 봉인한다(무작위 nonce 앞에 붙임). 브라우저는 내용을 읽거나 바꾸지 못한다.
func (m *Manager) seal(s LoginState) (string, error) {
	plain, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	aead, err := m.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("session: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, []byte(LoginCookieName))), nil
}

func (m *Manager) unseal(v string) (LoginState, error) {
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return LoginState{}, err
	}
	aead, err := m.aead()
	if err != nil {
		return LoginState{}, err
	}
	if len(b) < aead.NonceSize() {
		return LoginState{}, errors.New("session: short login state")
	}
	plain, err := aead.Open(nil, b[:aead.NonceSize()], b[aead.NonceSize():], []byte(LoginCookieName))
	if err != nil {
		return LoginState{}, err
	}
	var s LoginState
	if err := json.Unmarshal(plain, &s); err != nil {
		return LoginState{}, err
	}
	return s, nil
}

func (m *Manager) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(m.sealKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
