// devidp는 로컬 개발·smoke 전용 OIDC IdP다 (ADR 0065). production에서 쓰지 않는다.
//
//   - loopback 주소에만 bind한다(다른 주소면 기동 거부). 서명 key는 기동마다 새로 만든다(재시작하면 기존 ID token은 무효 —
//     session은 control-api가 따로 가지므로 영향 없음).
//   - Authorization Code + PKCE S256만 받는다. client_id·secret·redirect URI는 정확히 일치해야 한다. code는 60초·한 번만.
//   - 로그인 화면은 비밀번호 없이 정해진 개발 사용자(dev-admin, dev-viewer, dev-outsider) 중 하나를 고른다.
//
// 환경 변수
//
//	DEVIDP_ADDR           listen 주소 (기본 127.0.0.1:18090). issuer는 http://<주소>
//	DEVIDP_CLIENT_ID      기본 montracer-web
//	DEVIDP_CLIENT_SECRET  기본 local-dev-only
//	DEVIDP_REDIRECT_URIS  허용 redirect URI(쉼표), 기본 http://localhost:15173/api/v1/auth/callback
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// Users는 개발 사용자다. seed가 앞의 둘에 membership을 만든다(scripts/dev/demo). dev-outsider는 membership이 없다(거절 확인용).
var Users = []User{
	{Subject: "dev-admin", Name: "Dev Admin"},
	{Subject: "dev-viewer", Name: "Dev Viewer"},
	{Subject: "dev-outsider", Name: "Dev Outsider"},
}

// User는 개발 사용자 한 명이다.
type User struct{ Subject, Name string }

type grant struct {
	clientID, redirectURI, challenge, nonce, subject string
	expires                                          time.Time
}

// Server는 IdP다.
type Server struct {
	issuer, clientID, secret string
	redirects                []string
	key                      *rsa.PrivateKey
	keyID                    string
	now                      func() time.Time
	mu                       sync.Mutex
	codes                    map[string]grant
}

// New는 서버를 만든다. 서명 key를 새로 만든다.
func New(issuer, clientID, secret string, redirects []string) (*Server, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return &Server{issuer: issuer, clientID: clientID, secret: secret, redirects: redirects, key: key, keyID: randomString(8),
		now: time.Now, codes: map[string]grant{}}, nil
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Handler는 IdP 경로다.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", s.discovery)
	mux.HandleFunc("GET /jwks", s.jwks)
	mux.HandleFunc("GET /authorize", s.authorizePage)
	mux.HandleFunc("POST /authorize", s.approve)
	mux.HandleFunc("POST /token", s.token)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer": s.issuer, "authorization_endpoint": s.issuer + "/authorize", "token_endpoint": s.issuer + "/token",
		"jwks_uri": s.issuer + "/jwks", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"}, "code_challenge_methods_supported": []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"client_secret_basic", "client_secret_post"},
		"authorization_response_iss_parameter_supported": true,
	})
}

func (s *Server) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &s.key.PublicKey, KeyID: s.keyID, Algorithm: "RS256", Use: "sig"}}})
}

// authRequest는 authorization 요청이다. 잘못되면 redirect하지 않고 화면에 오류를 보인다(열린 redirect 방지, RFC 6749 §4.1.2.1).
type authRequest struct {
	ClientID, RedirectURI, State, Nonce, Challenge string
}

func (s *Server) parseAuth(q url.Values) (authRequest, error) {
	r := authRequest{ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), State: q.Get("state"), Nonce: q.Get("nonce"),
		Challenge: q.Get("code_challenge")}
	switch {
	case r.ClientID != s.clientID:
		return r, errors.New("unknown client_id")
	case !slices.Contains(s.redirects, r.RedirectURI):
		return r, errors.New("redirect_uri is not registered")
	case q.Get("response_type") != "code":
		return r, errors.New("response_type must be code")
	case q.Get("code_challenge_method") != "S256" || len(r.Challenge) < 43:
		return r, errors.New("PKCE S256 is required")
	case r.State == "" || r.Nonce == "":
		return r, errors.New("state and nonce are required")
	case !strings.Contains(" "+q.Get("scope")+" ", " openid "):
		return r, errors.New("scope must include openid")
	}
	return r, nil
}

var page = template.Must(template.New("authorize").Parse(`<!doctype html>
<html lang="ko"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Montracer 개발 IdP</title>
<style>body{font-family:system-ui,sans-serif;max-width:28rem;margin:4rem auto;padding:0 1rem}button{display:block;width:100%;margin:.5rem 0;padding:.75rem;font-size:1rem}p{color:#555}</style>
</head><body>
<h1>Montracer 개발 IdP</h1>
<p>로컬 개발 전용입니다. 비밀번호 없이 개발 사용자를 고릅니다.</p>
<form method="post" action="/authorize">
{{range $k, $v := .Params}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
{{range .Users}}<button type="submit" name="user" value="{{.Subject}}">{{.Name}} ({{.Subject}})</button>{{end}}
</form></body></html>`))

func (s *Server) authorizePage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if _, err := s.parseAuth(q); err != nil {
		http.Error(w, "invalid authorization request: "+err.Error(), http.StatusBadRequest)
		return
	}
	params := map[string]string{}
	for k := range q {
		params[k] = q.Get(k)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	_ = page.Execute(w, map[string]any{"Params": params, "Users": Users})
}

// approve는 사용자를 고른 결과다. code를 만들어 redirect_uri로 돌려보낸다(iss 포함, RFC 9207).
func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	req, err := s.parseAuth(r.PostForm)
	if err != nil {
		http.Error(w, "invalid authorization request: "+err.Error(), http.StatusBadRequest)
		return
	}
	user := r.PostForm.Get("user")
	if !slices.ContainsFunc(Users, func(u User) bool { return u.Subject == user }) {
		http.Error(w, "unknown user", http.StatusBadRequest)
		return
	}
	code := randomString(32)
	s.mu.Lock()
	s.codes[code] = grant{clientID: req.ClientID, redirectURI: req.RedirectURI, challenge: req.Challenge, nonce: req.Nonce,
		subject: user, expires: s.now().Add(60 * time.Second)}
	s.mu.Unlock()
	to, _ := url.Parse(req.RedirectURI)
	q := to.Query()
	q.Set("code", code)
	q.Set("state", req.State)
	q.Set("iss", s.issuer)
	to.RawQuery = q.Encode()
	http.Redirect(w, r, to.String(), http.StatusFound)
}

func tokenError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != s.clientID || subtle.ConstantTimeCompare([]byte(secret), []byte(s.secret)) != 1 {
		tokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		tokenError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	code := r.PostForm.Get("code")
	s.mu.Lock()
	g, found := s.codes[code]
	delete(s.codes, code) // 한 번만
	s.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !found || s.now().After(g.expires) || g.clientID != id || g.redirectURI != r.PostForm.Get("redirect_uri") ||
		subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(g.challenge)) != 1 {
		tokenError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	name := ""
	for _, u := range Users {
		if u.Subject == g.subject {
			name = u.Name
		}
	}
	idToken, err := s.sign(map[string]any{"iss": s.issuer, "sub": g.subject, "aud": s.clientID, "nonce": g.nonce, "name": name,
		"iat": s.now().Unix(), "exp": s.now().Add(5 * time.Minute).Unix(), "auth_time": s.now().Unix()})
	if err != nil {
		tokenError(w, http.StatusInternalServerError, "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": randomString(32), "token_type": "Bearer", "expires_in": 300, "id_token": idToken})
}

func (s *Server) sign(claims map[string]any) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: s.key}, (&jose.SignerOptions{}).WithHeader("kid", s.keyID))
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return obj.CompactSerialize()
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	addr := getenv("DEVIDP_ADDR", "127.0.0.1:18090")
	host, _, err := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		logger.Error("devidp binds only to a loopback address", "addr", addr)
		os.Exit(2)
	}
	s, err := New("http://"+addr, getenv("DEVIDP_CLIENT_ID", "montracer-web"), getenv("DEVIDP_CLIENT_SECRET", "local-dev-only"),
		strings.Split(getenv("DEVIDP_REDIRECT_URIS", "http://localhost:15173/api/v1/auth/callback"), ","))
	if err != nil {
		logger.Error("devidp", "error", err)
		os.Exit(1)
	}
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	fmt.Fprintf(os.Stderr, "devidp (local development only): issuer %s\n", s.issuer)
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("devidp stopped", "error", err)
		os.Exit(1)
	}
}
