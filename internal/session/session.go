// Package session은 사람 사용자의 browser session이다 (D04 §02 인증 흐름, D02 §12, ADR 0064).
//
//   - 로그인은 OIDC Authorization Code + PKCE다(oidc.go). state·nonce·PKCE verifier는 서버 key로 봉인한
//     짧은 cookie에 두고(10분), callback에서 state를 상수 시간으로 비교한다.
//   - session token은 무작위 32 byte다. cookie(__Host-, Secure, HttpOnly, SameSite=Lax)에만 있고 저장소에는 SHA-256만 둔다.
//   - idle 30분(활동마다 연장, 1분에 한 번만 기록)·최대 12시간. 폐기는 저장소 행으로 하므로 다음 요청부터 막힌다.
//   - tenant는 로그인 때 고른 membership이다. 역할은 요청마다 memberships에서 다시 읽는다(제거되면 미인증).
//   - 변경 요청(GET·HEAD·OPTIONS 밖)은 CSRF token(X-CSRF-Token)이 맞아야 한다. token은 session token의 HMAC이라
//     저장하지 않는다. Origin header가 있으면 허용 origin과 같아야 한다.
package session

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

const (
	// CookieName은 session cookie다. __Host- 접두어는 Secure·Path=/·Domain 없음을 브라우저가 강제한다(하위 도메인 주입 방지).
	CookieName = "__Host-mt_session"
	// CSRFHeader는 변경 요청이 보내는 CSRF token header다.
	CSRFHeader = "X-CSRF-Token"
	tokenBytes = 32
)

// ErrCSRF는 session으로 인증한 변경 요청의 CSRF token·Origin이 맞지 않는다는 뜻이다(403).
var ErrCSRF = fmt.Errorf("session: csrf token or origin mismatch: %w", authz.ErrForbidden)

// Store는 session 저장소다(controldb.SessionStore).
type Store interface {
	UserMemberships(ctx context.Context, userID string) ([]controldb.Membership, error)
	CreateSession(ctx context.Context, n controldb.NewSession) error
	LookupSession(ctx context.Context, idHash [32]byte) (controldb.SessionRecord, error)
	TouchSession(ctx context.Context, tenant authz.TenantID, idHash [32]byte, now, idleExpiresAt time.Time) error
	RevokeSession(ctx context.Context, tenant authz.TenantID, idHash [32]byte, userID, reason, requestID string, now time.Time) error
}

// Config는 Manager 설정이다. 0이면 기본값.
type Config struct {
	Store Store
	// Key는 32 byte 이상 무작위 master key다(MONTRACER_SESSION_KEY_HEX). CSRF HMAC·로그인 state 봉인 key를 여기서 나눈다.
	Key []byte
	// AllowedOrigin은 변경 요청의 Origin header가 같아야 하는 값이다(예: https://app.example.com). 비면 Origin을 보지 않는다.
	AllowedOrigin string
	IdleTimeout   time.Duration // 기본 30분 (D04 §02)
	MaxAge        time.Duration // 기본 12시간 (D04 §02)
	// TouchEvery보다 오래 활동이 없었을 때만 last_seen·idle 만료를 기록한다(요청마다 쓰지 않게, 기본 1분).
	TouchEvery time.Duration
	Now        func() time.Time
}

// Manager는 session 발급·인증·폐기다.
type Manager struct {
	cfg     Config
	csrfKey []byte
	sealKey []byte
}

// New는 Manager를 만든다.
func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, errors.New("session: Store is required")
	}
	if len(cfg.Key) < 32 {
		return nil, errors.New("session: key must be at least 32 bytes")
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = 12 * time.Hour
	}
	if cfg.TouchEvery <= 0 {
		cfg.TouchEvery = time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Manager{cfg: cfg, csrfKey: derive(cfg.Key, "csrf"), sealKey: derive(cfg.Key, "login-state")}, nil
}

// derive는 master key에서 용도별 key를 나눈다(HMAC-SHA256(key, label)). 한 key가 두 용도에 쓰이지 않게 한다.
func derive(key []byte, label string) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte("montracer/session/" + label))
	return m.Sum(nil)
}

// Current는 인증된 session이다.
type Current struct {
	Principal authz.Principal
	Record    controldb.SessionRecord
	// CSRFToken은 이 session의 변경 요청에 붙일 값이다(GET /auth/session이 돌려준다).
	CSRFToken string
	idHash    [32]byte
}

// IDHash는 session token의 SHA-256이다(폐기·교체에 쓴다).
func (c Current) IDHash() [32]byte { return c.idHash }

func hashToken(token string) [32]byte { return sha256.Sum256([]byte(token)) }

// CSRFToken은 session token의 CSRF token이다: base64url(HMAC-SHA256(csrfKey, token)).
func (m *Manager) CSRFToken(token string) string {
	h := hmac.New(sha256.New, m.csrfKey)
	_, _ = h.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// HasCookie는 요청에 session cookie가 있는지다(bearer가 없을 때만 session을 본다).
func HasCookie(r *http.Request) bool {
	c, err := r.Cookie(CookieName)
	return err == nil && c.Value != ""
}

// Authenticate는 session cookie로 사용자를 인증한다. 없거나, 폐기·만료됐거나, member가 아니게 됐으면
// authz.ErrUnauthenticated다(원인을 구분해 노출하지 않는다). 변경 요청이면 CSRF·Origin도 본다.
// 저장소 장애는 그대로 돌려준다(경계에서 503, fail closed).
func (m *Manager) Authenticate(r *http.Request) (Current, error) {
	c, err := r.Cookie(CookieName)
	if err != nil || len(c.Value) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return Current{}, authz.ErrUnauthenticated
	}
	token := c.Value
	idHash := hashToken(token)
	rec, err := m.cfg.Store.LookupSession(r.Context(), idHash)
	if errors.Is(err, controldb.ErrSessionNotFound) {
		return Current{}, authz.ErrUnauthenticated
	}
	if err != nil {
		return Current{}, err
	}
	now := m.cfg.Now().UTC()
	if rec.RevokedAt != nil || !now.Before(rec.IdleExpiresAt) || !now.Before(rec.ExpiresAt) || rec.Role == "" {
		return Current{}, authz.ErrUnauthenticated
	}
	csrf := m.CSRFToken(token)
	if !safeMethod(r.Method) {
		if !hmac.Equal([]byte(r.Header.Get(CSRFHeader)), []byte(csrf)) {
			return Current{}, ErrCSRF
		}
		if o := r.Header.Get("Origin"); o != "" && m.cfg.AllowedOrigin != "" && o != m.cfg.AllowedOrigin {
			return Current{}, ErrCSRF
		}
	}
	var stepUp time.Time
	if rec.StepUpAt != nil {
		stepUp = *rec.StepUpAt
	}
	p, err := authz.NewUserPrincipal(rec.Tenant, rec.UserID, rec.Role, stepUp, now)
	if err != nil {
		return Current{}, authz.ErrUnauthenticated
	}
	if now.Sub(rec.LastSeenAt) >= m.cfg.TouchEvery {
		// 연장 기록 실패는 이 요청을 막지 않는다(다음 요청이 다시 기록한다). idle 만료가 늦게 반영될 뿐이다.
		next := now.Add(m.cfg.IdleTimeout)
		if err := m.cfg.Store.TouchSession(r.Context(), rec.Tenant, idHash, now, next); err == nil {
			rec.LastSeenAt, rec.IdleExpiresAt = now, minTime(next, rec.ExpiresAt)
		}
	}
	return Current{Principal: p, Record: rec, CSRFToken: csrf, idHash: idHash}, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Issued는 새로 만든 session이다. Token은 cookie에만 넣는다(로그·응답 본문 금지).
type Issued struct {
	Token     string
	CSRFToken string
	ExpiresAt time.Time
}

// Issue는 사용자·tenant의 새 session을 만든다. member가 아니면 controldb.ErrNotMember다.
func (m *Manager) Issue(ctx context.Context, tenant authz.TenantID, userID, displayName, requestID string) (Issued, error) {
	var raw [tokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Issued{}, fmt.Errorf("session: token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	now := m.cfg.Now().UTC()
	expires := now.Add(m.cfg.MaxAge)
	if err := m.cfg.Store.CreateSession(ctx, controldb.NewSession{
		IDHash: hashToken(token), Tenant: tenant, UserID: userID, DisplayName: displayName,
		Now: now, IdleExpiresAt: minTime(now.Add(m.cfg.IdleTimeout), expires), ExpiresAt: expires, RequestID: requestID,
	}); err != nil {
		return Issued{}, err
	}
	return Issued{Token: token, CSRFToken: m.CSRFToken(token), ExpiresAt: expires}, nil
}

// Revoke는 session을 폐기한다.
func (m *Manager) Revoke(ctx context.Context, c Current, reason, requestID string) error {
	return m.cfg.Store.RevokeSession(ctx, c.Record.Tenant, c.idHash, c.Record.UserID, reason, requestID, m.cfg.Now().UTC())
}

// Memberships는 사용자의 조직 목록이다.
func (m *Manager) Memberships(ctx context.Context, userID string) ([]controldb.Membership, error) {
	return m.cfg.Store.UserMemberships(ctx, userID)
}

// SetCookie는 session cookie를 쓴다. 만료는 session 최대 만료와 같다(브라우저가 그 뒤 보내지 않게).
func (m *Manager) SetCookie(w http.ResponseWriter, s Issued) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: s.Token, Path: "/", Expires: s.ExpiresAt,
		MaxAge: int(s.ExpiresAt.Sub(m.cfg.Now()).Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// ClearCookie는 session cookie를 지운다.
func ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}
