package controlapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/httpapi"
	"github.com/polynomeer/montracer/internal/session"
)

// 멤버 관리와 초대 API (ADR 0067).
//   - 조회 members.read(Tenant Admin·Security Auditor), 변경 members.manage(Tenant Admin + 15분 안의 MFA step-up, ADR 0066).
//     API key는 step-up이 없어 변경할 수 없다.
//   - 초대 링크 token은 생성 응답에서 한 번만 준다. 저장은 SHA-256, 7일 만료, 한 번만 쓴다. tenant_admin으로는 초대하지 않는다.
//   - 수락은 POST /api/v1/auth/invitation(token은 본문 — URL·로그에 남지 않게) → IdP 로그인 → callback이 그 사용자로 수락하고
//     초대한 조직의 session을 만든다. 가입(JIT)은 여전히 없다 — 초대가 있어야 member가 된다(D04 §12).

// MemberStore는 멤버·초대 저장소다 (controldb.MemberStore).
type MemberStore interface {
	ListMembers(ctx context.Context, p authz.Principal, after string, limit int) ([]controldb.Member, bool, error)
	UpdateMemberRole(ctx context.Context, p authz.Principal, userID string, role authz.Role, ifMatch int64, requestID string) (controldb.Member, error)
	RemoveMember(ctx context.Context, p authz.Principal, userID string, ifMatch *int64, requestID string, now time.Time) error
	CreateInvitation(ctx context.Context, p authz.Principal, n controldb.NewInvitation, idem controldb.IdempotencyRequest, requestID string,
		respond func(controldb.Invitation) (controldb.StoredResponse, error)) (controldb.StoredResponse, bool, error)
	ListInvitations(ctx context.Context, p authz.Principal) ([]controldb.Invitation, error)
	RevokeInvitation(ctx context.Context, p authz.Principal, id, requestID string) error
	CheckInvitation(ctx context.Context, tokenHash [32]byte) (controldb.InvitationPreview, error)
	AcceptInvitation(ctx context.Context, tokenHash [32]byte, userID, displayName, requestID string) (controldb.AcceptedInvitation, error)
}

// InvitationTTL은 초대 링크의 유효 기간이다(ADR 0067).
const InvitationTTL = 7 * 24 * time.Hour

// WarnInvitationURLNotReplayed는 Idempotency-Key 재생 응답에 초대 링크가 없다는 경고다(token 원문은 저장하지 않는다).
const WarnInvitationURLNotReplayed = "invitation_url_not_replayed"

const (
	invitationTokenPrefix = "mti_"
	maxMemberBody         = 4 << 10
	maxLabelLen           = 64
)

// invitationTokenPattern은 초대 token 모양이다(접두어 + 32 byte base64url).
var invitationTokenPattern = regexp.MustCompile(`^mti_[A-Za-z0-9_-]{43}$`)

// MemberItem은 응답의 멤버다.
type MemberItem struct {
	UserID      string  `json:"user_id"`
	Role        string  `json:"role"`
	DisplayName *string `json:"display_name"`
	CreatedAt   string  `json:"created_at"`
	LastLoginAt *string `json:"last_login_at"`
	// IsSelf는 요청한 사람 자신인지다(화면이 "나"를 표시하고 자기 제거를 경고한다).
	IsSelf bool `json:"is_self"`
	// Revision은 역할 변경의 If-Match 값이다(D02 §20).
	Revision int64 `json:"revision"`
}

// InvitationItem은 응답의 대기 중 초대다(token 없음).
type InvitationItem struct {
	ID        string  `json:"id"`
	Role      string  `json:"role"`
	Label     *string `json:"label"`
	CreatedBy string  `json:"created_by"`
	CreatedAt string  `json:"created_at"`
	ExpiresAt string  `json:"expires_at"`
}

type membersResponse struct {
	Data       []MemberItem `json:"data"`
	NextCursor *string      `json:"next_cursor"`
	Meta       Meta         `json:"meta"`
}

type memberResponse struct {
	Data MemberItem   `json:"data"`
	Meta MutationMeta `json:"meta"`
}

type invitationsResponse struct {
	Data       []InvitationItem `json:"data"`
	NextCursor *string          `json:"next_cursor"`
	Meta       Meta             `json:"meta"`
}

type invitationResponse struct {
	Data InvitationItem `json:"data"`
	// InvitationURL은 생성 응답에만 있다. 다시 볼 수 없다.
	InvitationURL *string      `json:"invitation_url,omitempty"`
	Meta          MutationMeta `json:"meta"`
}

type memberPosition struct {
	UserID string `json:"u"`
}

func (h *Handler) registerMembers(b httpapi.Boundary) {
	h.handle("GET /api/v1/members", b.Handle(h.authenticated(h.listMembers)))
	h.handle("PATCH /api/v1/members/{user_id}", b.Handle(h.authenticated(h.updateMember)))
	h.handle("DELETE /api/v1/members/{user_id}", b.Handle(h.authenticated(h.removeMember)))
	h.handle("POST /api/v1/invitations", b.Handle(h.authenticated(h.createInvitation)))
	h.handle("GET /api/v1/invitations", b.Handle(h.authenticated(h.listInvitations)))
	h.handle("DELETE /api/v1/invitations/{id}", b.Handle(h.authenticated(h.revokeInvitation)))
	h.handle("POST /api/v1/auth/invitation", b.Handle(h.startInvitation))
}

func membersUnavailable() error {
	return apierr.New(apierr.NotFound, "멤버 관리 API를 사용할 수 없습니다")
}

func memberItem(m controldb.Member, p authz.Principal) MemberItem {
	item := MemberItem{UserID: m.UserID, Role: string(m.Role), DisplayName: m.DisplayName, CreatedAt: m.CreatedAt.Format(time.RFC3339Nano),
		IsSelf: p.Kind() == authz.KindUser && p.Subject() == m.UserID, Revision: m.Revision}
	if m.LastLoginAt != nil {
		t := m.LastLoginAt.Format(time.RFC3339Nano)
		item.LastLoginAt = &t
	}
	return item
}

func invitationItem(inv controldb.Invitation) InvitationItem {
	return InvitationItem{ID: inv.ID, Role: string(inv.Role), Label: inv.Label, CreatedBy: inv.CreatedBy,
		CreatedAt: inv.CreatedAt.Format(time.RFC3339Nano), ExpiresAt: inv.ExpiresAt.Format(time.RFC3339Nano)}
}

// memberUserID는 경로의 user_id다. 1..256자, 제어 문자 없음.
func memberUserID(r *http.Request) (string, error) {
	id := r.PathValue("user_id")
	if id == "" || len(id) > 256 || !utf8.ValidString(id) || strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return "", apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "user_id", Reason: "must be 1..256 characters without control characters"})
	}
	return id, nil
}

func memberErr(err error) error {
	switch {
	case errors.Is(err, controldb.ErrRevisionMismatch):
		return apierr.New(apierr.RevisionMismatch, "다른 변경이 먼저 저장되었습니다. 다시 불러와 확인한 뒤 바꾸세요")
	case errors.Is(err, authz.ErrNotFound):
		return apierr.New(apierr.NotFound, "멤버를 찾을 수 없습니다")
	case errors.Is(err, controldb.ErrLastAdmin):
		return apierr.New(apierr.Conflict, "조직에는 Tenant Admin이 한 명 이상 있어야 합니다")
	}
	return err
}

// readJSONBody는 크기 제한 안의 본문을 모르는 field 없이 읽는다.
func readJSONBody(w http.ResponseWriter, r *http.Request, v any, shape string) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMemberBody))
	if err != nil {
		e := apierr.New(apierr.InvalidArgument, "요청 본문이 너무 큽니다")
		e.Status = http.StatusRequestEntityTooLarge
		return nil, e
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "body", Reason: "must be " + shape})
	}
	return body, nil
}

// listMembers는 GET /api/v1/members?limit&cursor다(user_id 순).
func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Members == nil {
		return membersUnavailable()
	}
	q := r.URL.Query()
	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > controldb.MaxMemberPage {
			return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "limit", Reason: "must be within [1, 1000]"})
		}
		limit = n
	}
	binding := apicursor.Binding{
		Tenant:      p.Tenant().String(),
		Fingerprint: apicursor.Fingerprint(p.Kind().String(), p.Subject(), string(authz.MembersRead)),
		QueryHash:   apicursor.Fingerprint("members"),
	}
	after := ""
	if c := q.Get("cursor"); c != "" {
		claims, err := h.cfg.Cursor.Decode(c, binding)
		var pos memberPosition
		if err != nil || json.Unmarshal(claims.Position, &pos) != nil || pos.UserID == "" {
			return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "cursor", Reason: "invalid, expired, or not for this request"})
		}
		after = pos.UserID
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	members, more, err := h.cfg.Members.ListMembers(ctx, p, after, limit)
	if err != nil {
		return err
	}
	resp := membersResponse{Data: make([]MemberItem, 0, len(members)), Meta: newMeta(r)}
	for _, m := range members {
		resp.Data = append(resp.Data, memberItem(m, p))
	}
	if more && len(members) > 0 {
		tok, err := h.cfg.Cursor.Encode(binding, h.cfg.Now(), memberPosition{UserID: members[len(members)-1].UserID})
		if err != nil {
			return err
		}
		resp.NextCursor = &tok
	}
	return writeJSON(w, http.StatusOK, resp)
}

// updateMember는 PATCH /api/v1/members/{user_id}({"role": …})다. If-Match가 없으면 428, 다르면 412다(D02 §20).
// 마지막 Tenant Admin은 내릴 수 없다(409).
func (h *Handler) updateMember(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Members == nil {
		return membersUnavailable()
	}
	if err := authz.AuthorizeTenantWide(p, authz.MembersManage); err != nil {
		return err
	}
	userID, err := memberUserID(r)
	if err != nil {
		return err
	}
	rev, present, err := parseIfMatch(r)
	if err != nil {
		return err
	}
	if !present {
		return ifMatchRequired()
	}
	var req struct {
		Role string `json:"role"`
	}
	if _, err := readJSONBody(w, r, &req, `{"role": role}`); err != nil {
		return err
	}
	role := authz.Role(req.Role)
	if !role.Valid() {
		return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "role",
			Reason: "must be one of viewer, developer, operator, tenant_admin, security_auditor"})
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	m, err := h.cfg.Members.UpdateMemberRole(ctx, p, userID, role, rev, httpapi.RequestIDFrom(r.Context()))
	if err != nil {
		return memberErr(err)
	}
	w.Header().Set("ETag", etag(m.Revision))
	return writeJSON(w, http.StatusOK, memberResponse{Data: memberItem(m, p),
		Meta: MutationMeta{RequestID: httpapi.RequestIDFrom(r.Context()), SchemaVersion: SchemaVersion, Warnings: []string{}}})
}

// removeMember는 DELETE /api/v1/members/{user_id}다. If-Match가 있으면 지금 revision이어야 한다.
// 그 사용자의 이 조직 session과 그가 만든 대기 중 초대를 함께 끝낸다.
func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Members == nil {
		return membersUnavailable()
	}
	if err := authz.AuthorizeTenantWide(p, authz.MembersManage); err != nil {
		return err
	}
	userID, err := memberUserID(r)
	if err != nil {
		return err
	}
	rev, present, err := parseIfMatch(r)
	if err != nil {
		return err
	}
	var ifMatch *int64
	if present {
		ifMatch = &rev
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	if err := h.cfg.Members.RemoveMember(ctx, p, userID, ifMatch, httpapi.RequestIDFrom(r.Context()), h.cfg.Now().UTC()); err != nil {
		return memberErr(err)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// newInvitationToken은 초대 token과 그 SHA-256이다.
func newInvitationToken() (string, [32]byte, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", [32]byte{}, err
	}
	token := invitationTokenPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	return token, sha256.Sum256([]byte(token)), nil
}

// createInvitation은 POST /api/v1/invitations({"role", "label"})다. 201 응답에 초대 링크를 한 번만 싣는다.
func (h *Handler) createInvitation(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Members == nil {
		return membersUnavailable()
	}
	if err := authz.AuthorizeTenantWide(p, authz.MembersManage); err != nil {
		return err
	}
	key := r.Header.Get("Idempotency-Key")
	if !idempotencyKeyPattern.MatchString(key) {
		return apierr.NewInvalidArgument("Idempotency-Key가 필요합니다", apierr.FieldViolation{Field: "Idempotency-Key", Reason: "required header, 1..255 visible ASCII characters"})
	}
	var req struct {
		Role  string  `json:"role"`
		Label *string `json:"label"`
	}
	body, err := readJSONBody(w, r, &req, `{"role": role, "label": string?}`)
	if err != nil {
		return err
	}
	role := authz.Role(req.Role)
	invitable := false
	for _, r := range controldb.InvitableRoles {
		invitable = invitable || r == role
	}
	if !invitable {
		return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "role",
			Reason: "must be one of viewer, developer, operator, security_auditor (tenant_admin is granted after joining)"})
	}
	label := ""
	if req.Label != nil {
		label = strings.TrimSpace(*req.Label)
		if label == "" || utf8.RuneCountInString(label) > maxLabelLen || strings.IndexFunc(label, unicode.IsControl) >= 0 {
			return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "label", Reason: "must be 1..64 characters without control characters"})
		}
	}
	token, hash, err := newInvitationToken()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	requestID := httpapi.RequestIDFrom(r.Context())
	idem := controldb.IdempotencyRequest{Key: key, MethodPath: "POST /api/v1/invitations", RequestHash: sha256.Sum256(body)}
	n := controldb.NewInvitation{TokenHash: hash, Role: role, Label: label, ExpiresAt: h.cfg.Now().UTC().Add(InvitationTTL)}
	var created *controldb.Invitation
	stored, replayed, err := h.cfg.Members.CreateInvitation(ctx, p, n, idem, requestID, func(inv controldb.Invitation) (controldb.StoredResponse, error) {
		created = &inv
		// 저장하는 응답(재생용)에는 링크가 없다
		b, err := json.Marshal(invitationResponse{Data: invitationItem(inv),
			Meta: MutationMeta{RequestID: requestID, SchemaVersion: SchemaVersion, Warnings: []string{WarnInvitationURLNotReplayed}}})
		return controldb.StoredResponse{Status: http.StatusCreated, Body: b}, err
	})
	switch {
	case errors.Is(err, controldb.ErrIdempotencyConflict):
		return apierr.New(apierr.Conflict, "같은 Idempotency-Key를 다른 요청에 쓸 수 없습니다")
	case errors.Is(err, controldb.ErrTooManyInvitations):
		return apierr.New(apierr.Conflict, "대기 중인 초대가 너무 많습니다. 쓰지 않는 초대를 폐기하세요")
	case err != nil:
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	if replayed || created == nil {
		w.Header().Set("Idempotent-Replayed", "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stored.Status)
		_, err := w.Write(stored.Body)
		return err
	}
	// 링크는 화면의 초대 페이지다. token은 fragment에 둔다 — 서버 로그·Referer로 가지 않는다
	link := "/invite#" + token
	if h.cfg.Sessions != nil {
		link = h.cfg.Sessions.Origin() + link
	}
	return writeJSON(w, http.StatusCreated, invitationResponse{Data: invitationItem(*created), InvitationURL: &link,
		Meta: MutationMeta{RequestID: requestID, SchemaVersion: SchemaVersion, Warnings: []string{}}})
}

// listInvitations는 GET /api/v1/invitations다(대기 중, 만든 순, 최대 50).
func (h *Handler) listInvitations(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Members == nil {
		return membersUnavailable()
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	invs, err := h.cfg.Members.ListInvitations(ctx, p)
	if err != nil {
		return err
	}
	resp := invitationsResponse{Data: make([]InvitationItem, 0, len(invs)), Meta: newMeta(r)}
	for _, inv := range invs {
		resp.Data = append(resp.Data, invitationItem(inv))
	}
	return writeJSON(w, http.StatusOK, resp)
}

// revokeInvitation은 DELETE /api/v1/invitations/{id}다. 없거나 이미 쓰였으면 404다.
func (h *Handler) revokeInvitation(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.Members == nil {
		return membersUnavailable()
	}
	if err := authz.AuthorizeTenantWide(p, authz.MembersManage); err != nil {
		return err
	}
	id := r.PathValue("id")
	if !monitorIDPattern.MatchString(id) {
		return apierr.NewInvalidArgument("요청 값이 올바르지 않습니다", apierr.FieldViolation{Field: "id", Reason: "must be a lowercase UUID"})
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	if err := h.cfg.Members.RevokeInvitation(ctx, p, id, httpapi.RequestIDFrom(r.Context())); err != nil {
		if errors.Is(err, authz.ErrNotFound) {
			return apierr.New(apierr.NotFound, "대기 중인 초대를 찾을 수 없습니다")
		}
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// startInvitation은 POST /api/v1/auth/invitation({"token"})이다. 초대를 쓸 수 있으면 IdP 로그인 URL을 돌려준다(로그인 state에
// token hash를 봉인). 로그인 전이라 session·CSRF token이 없다 — JSON 본문(415 밖은 거절)과 Origin 검사로 다른 사이트의 강제 요청을 막는다.
func (h *Handler) startInvitation(w http.ResponseWriter, r *http.Request) error {
	if !h.authEnabled() || h.cfg.Members == nil {
		return apierr.New(apierr.NotFound, "로그인을 사용할 수 없습니다")
	}
	if err := h.cfg.Sessions.CheckOrigin(r); err != nil {
		return err
	}
	var req struct {
		Token string `json:"token"`
	}
	if _, err := readJSONBody(w, r, &req, `{"token": string}`); err != nil {
		return err
	}
	invalid := apierr.New(apierr.NotFound, "초대가 없거나 이미 쓰였거나 만료됐습니다")
	if !invitationTokenPattern.MatchString(req.Token) {
		h.observeLogin(session.LoginErrInvitation)
		return invalid
	}
	hash := sha256.Sum256([]byte(req.Token))
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	preview, err := h.cfg.Members.CheckInvitation(ctx, hash)
	if err != nil {
		if errors.Is(err, controldb.ErrInvitationInvalid) {
			h.observeLogin(session.LoginErrInvitation)
			return invalid
		}
		return err
	}
	st, err := h.cfg.Sessions.NewLoginState("/")
	if err != nil {
		return err
	}
	st.Invitation = hex.EncodeToString(hash[:])
	to, err := h.cfg.OIDC.AuthURL(r.Context(), st.State, st.Nonce, st.Verifier, session.AuthOptions{})
	if err != nil {
		h.cfg.Logger.Warn("oidc discovery failed", "error", err.Error())
		h.observeLogin(session.LoginErrUnavailable)
		return apierr.New(apierr.Unavailable, "지금은 로그인을 시작할 수 없습니다")
	}
	if err := h.cfg.Sessions.SetLoginCookie(w, st); err != nil {
		return err
	}
	// 들어갈 조직과 역할을 함께 준다 — 화면이 "어느 조직에 들어갑니다"를 보여 주고 확인을 받은 뒤 IdP로 이동한다
	// (남이 보낸 링크로 모르는 사이 다른 조직에 들어가 일하지 않게)
	return writeJSON(w, http.StatusOK, invitationStartResponse{Data: invitationStart{AuthorizationURL: to, TenantName: preview.TenantName,
		Role: string(preview.Role)}, Meta: newMeta(r)})
}

type invitationStartResponse struct {
	Data invitationStart `json:"data"`
	Meta Meta            `json:"meta"`
}

type invitationStart struct {
	AuthorizationURL string  `json:"authorization_url"`
	TenantName       *string `json:"tenant_name"`
	Role             string  `json:"role"`
}

// completeInvitation은 초대 수락 callback이다. IdP 사용자로 초대를 수락하고 그 조직의 session을 만든다.
func (h *Handler) completeInvitation(w http.ResponseWriter, r *http.Request, st session.LoginState, id session.Identity) {
	userID, err := authz.OIDCUserID(id.Issuer, id.Subject)
	if err != nil {
		h.loginError(w, r, session.LoginErrToken)
		return
	}
	raw, err := hex.DecodeString(st.Invitation)
	if err != nil || len(raw) != sha256.Size {
		h.loginError(w, r, session.LoginErrInvitation)
		return
	}
	var hash [32]byte
	copy(hash[:], raw)
	requestID := httpapi.RequestIDFrom(r.Context())
	acc, err := h.cfg.Members.AcceptInvitation(r.Context(), hash, userID, id.Name, requestID)
	switch {
	case errors.Is(err, controldb.ErrInvitationInvalid):
		h.loginError(w, r, session.LoginErrInvitation)
		return
	case err != nil:
		h.cfg.Logger.Warn("invitation accept failed", "error", err.Error())
		h.loginError(w, r, session.LoginErrUnavailable)
		return
	}
	issued, err := h.cfg.Sessions.Issue(r.Context(), acc.Tenant, userID, id.Name, requestID, time.Time{})
	if err != nil {
		h.cfg.Logger.Warn("invitation session failed", "error", err.Error())
		h.loginError(w, r, session.LoginErrUnavailable)
		return
	}
	h.cfg.Sessions.SetCookie(w, issued)
	h.observeLogin("invitation_ok")
	redirect(w, r, st.ReturnTo, http.StatusSeeOther)
}
