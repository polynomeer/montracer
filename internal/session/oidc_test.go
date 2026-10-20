package session

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// fakeIdP는 시험용 OIDC IdP다: discovery, JWKS, token endpoint(PKCE S256 검사). 실제 서명한 ID token을 준다.
type fakeIdP struct {
	srv        *httptest.Server
	key        *rsa.PrivateKey
	mu         sync.Mutex
	challenges map[string]string // code → code_challenge
	nonces     map[string]string // code → ID token에 넣을 nonce
	audience   string
	subject    string
	authTime   time.Time // zero면 auth_time claim을 넣지 않는다
	acr        string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{key: key, challenges: map[string]string{}, nonces: map[string]string{}, audience: "montracer-web", subject: "alice-123"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/auth", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/jwks", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		code, verifier := r.PostForm.Get("code"), r.PostForm.Get("code_verifier")
		if code == "c-500" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		f.mu.Lock()
		challenge, nonce := f.challenges[code], f.nonces[code]
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(verifier))
		if challenge == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 300,
			"id_token": f.sign(t, nonce)})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) sign(t *testing.T, nonce string) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	m := map[string]any{"iss": f.srv.URL, "sub": f.subject, "aud": f.audience, "nonce": nonce,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "name": "Alice", "email": "alice@example.com"}
	if !f.authTime.IsZero() {
		m["auth_time"] = f.authTime.Unix()
	}
	if f.acr != "" {
		m["acr"] = f.acr
	}
	claims, _ := json.Marshal(m)
	obj, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// authorize는 브라우저가 IdP에서 로그인한 것처럼 code를 발급한다. challenge는 authorization URL에서 가져온다.
func (f *fakeIdP) authorize(t *testing.T, authURL, code, nonceOverride string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	nonce := q.Get("nonce")
	if nonceOverride != "" {
		nonce = nonceOverride
	}
	f.mu.Lock()
	f.challenges[code], f.nonces[code] = q.Get("code_challenge"), nonce
	f.mu.Unlock()
}

func provider(t *testing.T, issuer string) *OIDCProvider {
	t.Helper()
	p, err := NewOIDCProvider(OIDCConfig{Issuer: issuer, ClientID: "montracer-web", ClientSecret: "s3cret",
		RedirectURL: "https://app.example.com/api/v1/auth/callback"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOIDCAuthorizationCodeWithPKCE(t *testing.T) {
	idp := newFakeIdP(t)
	p := provider(t, idp.srv.URL)
	ctx := context.Background()
	authURL, err := p.AuthURL(ctx, "st-1", "nonce-1", "verifier-0123456789012345678901234567890123456789", AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, authURL)
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") != "st-1" || q.Get("nonce") != "nonce-1" ||
		q.Get("redirect_uri") != "https://app.example.com/api/v1/auth/callback" || q.Get("response_type") != "code" || q.Get("scope") != "openid profile" {
		t.Fatalf("auth url = %s", authURL)
	}
	idp.authorize(t, authURL, "code-1", "")
	id, err := p.Exchange(ctx, "code-1", "verifier-0123456789012345678901234567890123456789", "nonce-1")
	if err != nil {
		t.Fatal(err)
	}
	if id.Issuer != idp.srv.URL || id.Subject != "alice-123" || id.Name != "Alice" || !id.AuthTime.IsZero() || id.ACR != "" {
		t.Fatalf("identity = %+v", id)
	}
	if q.Has("prompt") || q.Has("max_age") || q.Has("acr_values") {
		t.Fatalf("plain login asks for re-authentication: %s", authURL)
	}
}

// 재인증(step-up)은 prompt=login·max_age=0·acr_values를 보내고, ID token의 auth_time·acr을 돌려준다(ADR 0066).
func TestOIDCReauthentication(t *testing.T) {
	idp := newFakeIdP(t)
	idp.authTime, idp.acr = time.Unix(1_790_000_000, 0).UTC(), "mfa"
	p := provider(t, idp.srv.URL)
	ctx := context.Background()
	const verifier = "verifier-0123456789012345678901234567890123456789"
	authURL, err := p.AuthURL(ctx, "st-2", "nonce-2", verifier, AuthOptions{Reauthenticate: true, ACR: "mfa"})
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, authURL)
	if q.Get("prompt") != "login" || q.Get("max_age") != "0" || q.Get("acr_values") != "mfa" {
		t.Fatalf("auth url = %s", authURL)
	}
	idp.authorize(t, authURL, "code-2", "")
	id, err := p.Exchange(ctx, "code-2", verifier, "nonce-2")
	if err != nil {
		t.Fatal(err)
	}
	if !id.AuthTime.Equal(idp.authTime) || id.ACR != "mfa" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestOIDCRejects(t *testing.T) {
	idp := newFakeIdP(t)
	p := provider(t, idp.srv.URL)
	ctx := context.Background()
	verifier := "verifier-0123456789012345678901234567890123456789"
	authURL, _ := p.AuthURL(ctx, "st", "nonce-1", verifier, AuthOptions{})

	// PKCE verifier가 다르면(가로챈 code) token을 받지 못한다
	idp.authorize(t, authURL, "c-pkce", "")
	if _, err := p.Exchange(ctx, "c-pkce", "another-verifier-0123456789012345678901234567890123", "nonce-1"); !IsInvalidToken(err) {
		t.Errorf("wrong verifier: %v", err)
	}
	// ID token의 nonce가 이 로그인 것이 아니면(재전송) 거절
	idp.authorize(t, authURL, "c-nonce", "someone-elses-nonce")
	if _, err := p.Exchange(ctx, "c-nonce", verifier, "nonce-1"); !IsInvalidToken(err) {
		t.Errorf("nonce mismatch: %v", err)
	}
	// 다른 client에게 발급된 ID token(audience)은 거절
	idp.audience = "other-client"
	idp.authorize(t, authURL, "c-aud", "")
	if _, err := p.Exchange(ctx, "c-aud", verifier, "nonce-1"); !IsInvalidToken(err) {
		t.Errorf("wrong audience: %v", err)
	}
	// token endpoint 5xx는 검증 실패가 아니라 IdP 장애다(사용자에게 token_invalid로 보이지 않게)
	idp.authorize(t, authURL, "c-500", "")
	if _, err := p.Exchange(ctx, "c-500", verifier, "nonce-1"); err == nil || IsInvalidToken(err) {
		t.Errorf("idp 5xx: %v", err)
	}
	// IdP에 닿지 않으면 검증 실패가 아니라 장애다(unavailable)
	down := provider(t, "http://127.0.0.1:1")
	if _, err := down.AuthURL(ctx, "s", "n", verifier, AuthOptions{}); err == nil || IsInvalidToken(err) {
		t.Errorf("unreachable idp: %v", err)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}
