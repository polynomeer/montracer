package controlapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/httpapi"
	"github.com/polynomeer/montracer/internal/session"
)

// 사람 로그인과 browser session (D04 §02, D02 §12, ADR 0064).
//
//	GET  /api/v1/auth/login?return_to=   → IdP로 302 (state·nonce·PKCE를 봉인 cookie에)
//	GET  /api/v1/auth/callback           → session cookie를 쓰고 return_to로 303 (실패는 /login?error=…)
//	GET  /api/v1/auth/session            → 지금 사용자·조직·역할·CSRF token
//	POST /api/v1/auth/logout             → session 폐기, cookie 삭제 (CSRF)
//	POST /api/v1/auth/session/tenant     → 다른 membership 조직으로 바꾼다. 새 session을 만들고 이전 것은 폐기한다 (CSRF)
//
// Sessions나 OIDC가 없으면(설정 off) 이 경로는 404다.

// loginPagePath는 로그인 실패를 보이는 화면 경로다(web route).
const loginPagePath = "/login"

type sessionMembership struct {
	TenantID   string  `json:"tenant_id"`
	TenantName *string `json:"tenant_name"`
	Role       string  `json:"role"`
}

// sessionInfo는 GET /auth/session의 data다. token은 싣지 않는다 — CSRF token만 싣는다(같은 origin만 읽는다).
type sessionInfo struct {
	UserID        string              `json:"user_id"`
	DisplayName   *string             `json:"display_name"`
	TenantID      string              `json:"tenant_id"`
	TenantName    *string             `json:"tenant_name"`
	Role          string              `json:"role"`
	Memberships   []sessionMembership `json:"memberships"`
	CSRFToken     string              `json:"csrf_token"`
	IdleExpiresAt string              `json:"idle_expires_at"`
	ExpiresAt     string              `json:"expires_at"`
	// StepUpExpiresAt은 MFA step-up이 끝나는 시각이다. 없거나 지났으면 null(민감 작업 전 /auth/step-up, ADR 0066).
	StepUpExpiresAt *string `json:"step_up_expires_at"`
}

func (h *Handler) registerAuth(b httpapi.Boundary) {
	h.handle("GET /api/v1/auth/login", http.HandlerFunc(h.login))
	h.handle("GET /api/v1/auth/callback", http.HandlerFunc(h.callback))
	h.handle("GET /api/v1/auth/session", b.Handle(h.withSession(h.getSession)))
	h.handle("POST /api/v1/auth/step-up", b.Handle(h.withSession(h.stepUp)))
	h.handle("POST /api/v1/auth/logout", b.Handle(h.withSession(h.logout)))
	h.handle("POST /api/v1/auth/session/tenant", b.Handle(h.withSession(h.switchTenant)))
}

func (h *Handler) authEnabled() bool { return h.cfg.Sessions != nil && h.cfg.OIDC != nil }

// redirect는 캐시되지 않는 redirect다(로그인 응답이 공유 cache에 남지 않게).
func redirect(w http.ResponseWriter, r *http.Request, to string, code int) {
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, to, code)
}

func (h *Handler) loginError(w http.ResponseWriter, r *http.Request, code string) {
	h.observeLogin(code)
	redirect(w, r, loginPagePath+"?error="+url.QueryEscape(code), http.StatusSeeOther)
}

func (h *Handler) observeLogin(outcome string) {
	if h.cfg.ObserveLogin != nil {
		h.cfg.ObserveLogin(outcome)
	}
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if !h.authEnabled() {
		httpapi.Boundary{Logger: h.cfg.Logger}.Handle(func(http.ResponseWriter, *http.Request) error {
			return apierr.New(apierr.NotFound, "로그인을 사용할 수 없습니다")
		}).ServeHTTP(w, r)
		return
	}
	st, err := h.cfg.Sessions.NewLoginState(r.URL.Query().Get("return_to"))
	if err != nil {
		h.loginError(w, r, session.LoginErrUnavailable)
		return
	}
	to, err := h.cfg.OIDC.AuthURL(r.Context(), st.State, st.Nonce, st.Verifier, session.AuthOptions{})
	if err != nil {
		h.cfg.Logger.Warn("oidc discovery failed", "error", err.Error())
		h.loginError(w, r, session.LoginErrUnavailable)
		return
	}
	if err := h.cfg.Sessions.SetLoginCookie(w, st); err != nil {
		h.loginError(w, r, session.LoginErrUnavailable)
		return
	}
	redirect(w, r, to, http.StatusFound)
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	if !h.authEnabled() {
		h.login(w, r) // 같은 404
		return
	}
	q := r.URL.Query()
	st, ok := h.cfg.Sessions.TakeLoginState(w, r, q.Get("state"))
	if !ok {
		h.loginError(w, r, session.LoginErrState)
		return
	}
	fail := func(code string) {
		if st.StepUp != "" { // 재인증 실패는 원래 화면으로 돌아갈 수 있게 return_to를 둔다(session은 그대로)
			h.stepUpError(w, r, code, st.ReturnTo)
			return
		}
		h.loginError(w, r, code)
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		fail(session.LoginErrIdP) // IdP 오류 문구는 옮기지 않는다
		return
	}
	id, err := h.cfg.OIDC.Exchange(r.Context(), q.Get("code"), st.Verifier, st.Nonce)
	if err != nil {
		if session.IsInvalidToken(err) {
			fail(session.LoginErrToken)
			return
		}
		h.cfg.Logger.Warn("oidc exchange failed", "error", err.Error())
		fail(session.LoginErrUnavailable)
		return
	}
	if st.StepUp != "" {
		h.completeStepUp(w, r, st, id)
		return
	}
	if st.Invitation != "" && h.cfg.Members != nil {
		h.completeInvitation(w, r, st, id)
		return
	}
	userID, err := authz.OIDCUserID(id.Issuer, id.Subject)
	if err != nil {
		h.loginError(w, r, session.LoginErrToken)
		return
	}
	members, err := h.cfg.Sessions.Memberships(r.Context(), userID)
	if err != nil {
		h.cfg.Logger.Warn("login membership lookup failed", "error", err.Error())
		h.loginError(w, r, session.LoginErrUnavailable)
		return
	}
	// 가입(JIT)으로 조직·역할을 만들지 않는다 — 미리 정한 membership이 있어야 한다(D04 §12 "JIT 가입만으로 관리자 role을 부여하지 않는다")
	if len(members) == 0 {
		h.loginError(w, r, session.LoginErrNoMembership)
		return
	}
	issued, err := h.cfg.Sessions.Issue(r.Context(), members[0].Tenant, userID, id.Name, httpapi.RequestIDFrom(r.Context()), time.Time{})
	switch {
	case errors.Is(err, controldb.ErrNotMember):
		h.loginError(w, r, session.LoginErrNoMembership)
		return
	case err != nil:
		h.loginError(w, r, session.LoginErrUnavailable)
		return
	}
	h.cfg.Sessions.SetCookie(w, issued)
	h.observeLogin("ok")
	redirect(w, r, st.ReturnTo, http.StatusSeeOther)
}

type sessionHandler func(w http.ResponseWriter, r *http.Request, c session.Current) error

// withSession은 session cookie로만 인증한다(bearer key는 session이 없다). 변경 요청이면 CSRF를 본다.
func (h *Handler) withSession(next sessionHandler) httpapi.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if !h.authEnabled() {
			return apierr.New(apierr.NotFound, "로그인을 사용할 수 없습니다")
		}
		c, err := h.cfg.Sessions.Authenticate(r)
		if err != nil {
			return err
		}
		return next(w, r.WithContext(authz.WithPrincipal(r.Context(), c.Principal)), c)
	}
}

func (h *Handler) writeSession(w http.ResponseWriter, r *http.Request, c session.Current, csrf string) error {
	members, err := h.cfg.Sessions.Memberships(r.Context(), c.Record.UserID)
	if err != nil {
		return err
	}
	info := sessionInfo{UserID: c.Record.UserID, DisplayName: c.Record.DisplayName, TenantID: c.Record.Tenant.String(),
		Role: string(c.Record.Role), Memberships: make([]sessionMembership, 0, len(members)), CSRFToken: csrf,
		IdleExpiresAt: c.Record.IdleExpiresAt.Format(time.RFC3339), ExpiresAt: c.Record.ExpiresAt.Format(time.RFC3339)}
	if t := h.cfg.Sessions.StepUpExpiresAt(c.Record); t != nil {
		v := t.Format(time.RFC3339)
		info.StepUpExpiresAt = &v
	}
	for _, m := range members {
		info.Memberships = append(info.Memberships, sessionMembership{TenantID: m.Tenant.String(), TenantName: m.TenantName, Role: string(m.Role)})
		if m.Tenant == c.Record.Tenant {
			info.TenantName = m.TenantName
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	return json.NewEncoder(w).Encode(struct {
		Data sessionInfo `json:"data"`
		Meta Meta        `json:"meta"`
	}{info, newMeta(r)})
}

func (h *Handler) getSession(w http.ResponseWriter, r *http.Request, c session.Current) error {
	return h.writeSession(w, r, c, c.CSRFToken)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request, c session.Current) error {
	if err := h.cfg.Sessions.Revoke(r.Context(), c, "logout", httpapi.RequestIDFrom(r.Context())); err != nil {
		return err
	}
	session.ClearCookie(w)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// switchTenant는 같은 사용자의 다른 membership 조직으로 session을 바꾼다. token을 새로 만든다(session fixation 방지).
// member가 아닌 조직은 403이다 — 자기 membership 목록 밖이라 존재를 숨길 이유가 없다(GET /auth/session이 목록을 준다).
func (h *Handler) switchTenant(w http.ResponseWriter, r *http.Request, c session.Current) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		e := apierr.New(apierr.InvalidArgument, "요청 본문이 너무 큽니다")
		e.Status = http.StatusRequestEntityTooLarge
		return e
	}
	var req struct {
		TenantID string `json:"tenant_id"`
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "body", Reason: "must be {\"tenant_id\": uuid}"})
	}
	tenant, err := authz.ParseTenantID(req.TenantID)
	if err != nil {
		return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "tenant_id", Reason: "must be a lowercase UUID"})
	}
	members, err := h.cfg.Sessions.Memberships(r.Context(), c.Record.UserID)
	if err != nil {
		return err
	}
	member := false
	for _, m := range members {
		member = member || m.Tenant == tenant
	}
	if !member {
		return authz.ErrForbidden
	}
	name := ""
	if c.Record.DisplayName != nil {
		name = *c.Record.DisplayName
	}
	reqID := httpapi.RequestIDFrom(r.Context())
	// 새 session의 최대 만료는 이전 session을 넘지 않는다(IdP 재인증 없이 12시간을 늘리지 못하게)
	issued, err := h.cfg.Sessions.Issue(r.Context(), tenant, c.Record.UserID, name, reqID, c.Record.ExpiresAt)
	if errors.Is(err, controldb.ErrNotMember) {
		return authz.ErrForbidden
	}
	if err != nil {
		return err
	}
	if err := h.cfg.Sessions.Revoke(r.Context(), c, "tenant_switch", reqID); err != nil {
		return err
	}
	h.cfg.Sessions.SetCookie(w, issued)
	// 새 session으로 다시 읽어 돌려준다(역할·만료는 새 조직 기준)
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodGet
	r2.Header.Del("Cookie")
	r2.AddCookie(&http.Cookie{Name: session.CookieName, Value: issued.Token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	next, err := h.cfg.Sessions.Authenticate(r2)
	if err != nil {
		return err
	}
	return h.writeSession(w, r, next, issued.CSRFToken)
}

// stepUp은 POST /api/v1/auth/step-up 이다(ADR 0066). 지금 session의 사용자에게 IdP 재인증(prompt=login, max_age=0,
// 설정 시 acr_values)을 요구하는 authorization URL을 JSON으로 돌려준다. 화면이 그 URL로 이동한다.
// POST라 CSRF token·Origin을 요구한다 — 다른 사이트가 피해자에게 재인증을 시켜 탈취된 session에 step-up을 얹지 못하게.
// 로그인 state는 시작한 session에 묶는다.
func (h *Handler) stepUp(w http.ResponseWriter, r *http.Request, c session.Current) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		e := apierr.New(apierr.InvalidArgument, "요청 본문이 너무 큽니다")
		e.Status = http.StatusRequestEntityTooLarge
		return e
	}
	var req struct {
		ReturnTo string `json:"return_to"`
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || len(req.ReturnTo) > 2048 {
		return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "body", Reason: "must be {\"return_to\": path}"})
	}
	st, err := h.cfg.Sessions.NewLoginState(req.ReturnTo)
	if err != nil {
		return err
	}
	st.StepUp = c.IDHashHex()
	to, err := h.cfg.OIDC.AuthURL(r.Context(), st.State, st.Nonce, st.Verifier, session.AuthOptions{Reauthenticate: true, ACR: h.cfg.StepUpACR})
	if err != nil {
		h.cfg.Logger.Warn("oidc discovery failed", "error", err.Error())
		h.observeLogin(session.LoginErrUnavailable)
		return apierr.New(apierr.Unavailable, "지금은 재인증을 시작할 수 없습니다")
	}
	if err := h.cfg.Sessions.SetLoginCookie(w, st); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, authorizationResponse{Data: authorizationURL{AuthorizationURL: to}, Meta: newMeta(r)})
}

// authorizationResponse는 IdP로 이동할 URL이다(step-up·초대 수락 시작).
type authorizationResponse struct {
	Data authorizationURL `json:"data"`
	Meta Meta             `json:"meta"`
}

type authorizationURL struct {
	AuthorizationURL string `json:"authorization_url"`
}

// stepUpError는 재인증 실패를 로그인 화면으로 보낸다. return_to를 두어 다시 시도하거나 돌아갈 수 있게 한다.
func (h *Handler) stepUpError(w http.ResponseWriter, r *http.Request, code, returnTo string) {
	h.observeLogin(code)
	redirect(w, r, loginPagePath+"?error="+url.QueryEscape(code)+"&return_to="+url.QueryEscape(returnTo), http.StatusSeeOther)
}

// completeStepUp은 재인증 callback이다. 같은 session cookie이고, 같은 사용자가 재인증을 시작한 뒤 최근에 인증했으며, MFA 증거가
// 있을 때만 step_up_at을 기록한다(ADR 0066). 조건 불만족은 step_up_rejected, 제어 DB 장애는 unavailable이다(session은 그대로).
func (h *Handler) completeStepUp(w http.ResponseWriter, r *http.Request, st session.LoginState, id session.Identity) {
	c, err := h.cfg.Sessions.Authenticate(r)
	switch {
	case errors.Is(err, authz.ErrUnauthenticated) || (err == nil && c.IDHashHex() != st.StepUp):
		// 시작한 session이 없어졌거나 바뀌었다(다른 탭의 로그아웃·조직 전환). 어느 조직의 감사인지 정할 수 없어 지표만 남긴다
		h.stepUpError(w, r, session.LoginErrStepUp, st.ReturnTo)
		return
	case err != nil:
		h.cfg.Logger.Warn("step-up session lookup failed", "error", err.Error())
		h.stepUpError(w, r, session.LoginErrUnavailable, st.ReturnTo)
		return
	}
	var started time.Time // 시작 시각이 없는 state는 거절된다(CompleteStepUp)
	if st.Issued > 0 {
		started = time.Unix(st.Issued, 0)
	}
	err = h.cfg.Sessions.CompleteStepUp(r.Context(), c, id, h.cfg.StepUpACR, started, httpapi.RequestIDFrom(r.Context()))
	switch {
	case errors.Is(err, session.ErrStepUpRejected):
		h.stepUpError(w, r, session.LoginErrStepUp, st.ReturnTo)
		return
	case err != nil:
		h.cfg.Logger.Warn("step-up record failed", "error", err.Error())
		h.stepUpError(w, r, session.LoginErrUnavailable, st.ReturnTo)
		return
	}
	h.observeLogin("step_up_ok")
	redirect(w, r, st.ReturnTo, http.StatusSeeOther)
}
