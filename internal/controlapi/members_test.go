package controlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/session"
)

// fakeMembers는 시험용 멤버·초대 저장소다. membership은 sessionStore와 같은 map을 쓴다(수락 뒤 바로 로그인된다).
type fakeMembers struct {
	sessions    *sessionStore
	invitations map[[32]byte]*fakeInvitation
	idem        map[string]controldb.StoredResponse
	seq         int
	now         func() time.Time
	revs        map[[2]string]int64 // membership revision(없으면 1)
}

type fakeInvitation struct {
	tenant           authz.TenantID
	inv              controldb.Invitation
	accepted, revoke bool
}

func newFakeMembers(s *sessionStore, now func() time.Time) *fakeMembers {
	return &fakeMembers{sessions: s, invitations: map[[32]byte]*fakeInvitation{}, idem: map[string]controldb.StoredResponse{}, now: now,
		revs: map[[2]string]int64{}}
}

func (f *fakeMembers) list(tenant authz.TenantID) []controldb.Member {
	var out []controldb.Member
	for k, r := range f.sessions.members {
		if k[0] == tenant.String() {
			out = append(out, controldb.Member{UserID: k[1], Role: r, CreatedAt: now, Revision: f.rev(k)})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].UserID < out[b].UserID })
	return out
}

func (f *fakeMembers) rev(k [2]string) int64 {
	if r, ok := f.revs[k]; ok {
		return r
	}
	return 1
}

// revokeBy는 그 사용자가 만든 대기 중 초대를 폐기한다.
func (f *fakeMembers) revokeBy(tenant authz.TenantID, userID string) {
	for _, i := range f.invitations {
		if i.tenant == tenant && i.inv.CreatedBy == userID && !i.accepted {
			i.revoke = true
		}
	}
}

func (f *fakeMembers) admins(tenant authz.TenantID) int {
	n := 0
	for _, m := range f.list(tenant) {
		if m.Role == authz.RoleTenantAdmin {
			n++
		}
	}
	return n
}

func (f *fakeMembers) ListMembers(_ context.Context, p authz.Principal, after string, limit int) ([]controldb.Member, bool, error) {
	if err := authz.AuthorizeTenantWide(p, authz.MembersRead); err != nil {
		return nil, false, err
	}
	var out []controldb.Member
	for _, m := range f.list(p.Tenant()) {
		if m.UserID > after {
			out = append(out, m)
		}
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

func (f *fakeMembers) UpdateMemberRole(_ context.Context, p authz.Principal, userID string, role authz.Role, ifMatch int64, _ string) (controldb.Member, error) {
	if err := authz.AuthorizeTenantWide(p, authz.MembersManage); err != nil {
		return controldb.Member{}, err
	}
	k := [2]string{p.Tenant().String(), userID}
	cur, ok := f.sessions.members[k]
	if !ok {
		return controldb.Member{}, authz.ErrNotFound
	}
	if f.rev(k) != ifMatch {
		return controldb.Member{}, controldb.ErrRevisionMismatch
	}
	if role == cur {
		return controldb.Member{UserID: userID, Role: role, CreatedAt: now, Revision: f.rev(k)}, nil
	}
	if cur == authz.RoleTenantAdmin && f.admins(p.Tenant()) <= 1 {
		return controldb.Member{}, controldb.ErrLastAdmin
	}
	if cur == authz.RoleTenantAdmin {
		f.revokeBy(p.Tenant(), userID)
	}
	f.sessions.members[k] = role
	f.revs[k] = f.rev(k) + 1
	return controldb.Member{UserID: userID, Role: role, CreatedAt: now, Revision: f.revs[k]}, nil
}

func (f *fakeMembers) RemoveMember(_ context.Context, p authz.Principal, userID string, ifMatch *int64, _ string, at time.Time) error {
	if err := authz.AuthorizeTenantWide(p, authz.MembersManage); err != nil {
		return err
	}
	k := [2]string{p.Tenant().String(), userID}
	cur, ok := f.sessions.members[k]
	if !ok {
		return authz.ErrNotFound
	}
	if ifMatch != nil && *ifMatch != f.rev(k) {
		return controldb.ErrRevisionMismatch
	}
	if cur == authz.RoleTenantAdmin && f.admins(p.Tenant()) <= 1 {
		return controldb.ErrLastAdmin
	}
	f.revokeBy(p.Tenant(), userID)
	delete(f.sessions.members, k)
	delete(f.revs, k)
	for _, s := range f.sessions.sessions {
		if s.Tenant == p.Tenant() && s.UserID == userID && s.RevokedAt == nil {
			s.RevokedAt = &at
		}
	}
	return nil
}

func (f *fakeMembers) pending(tenant authz.TenantID) []controldb.Invitation {
	var out []controldb.Invitation
	for _, i := range f.invitations {
		if i.tenant == tenant && !i.accepted && !i.revoke && i.inv.ExpiresAt.After(f.now()) {
			out = append(out, i.inv)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

func (f *fakeMembers) CreateInvitation(_ context.Context, p authz.Principal, n controldb.NewInvitation, idem controldb.IdempotencyRequest, _ string,
	respond func(controldb.Invitation) (controldb.StoredResponse, error)) (controldb.StoredResponse, bool, error) {
	if err := authz.AuthorizeTenantWide(p, authz.MembersManage); err != nil {
		return controldb.StoredResponse{}, false, err
	}
	key := p.Subject() + "\n" + idem.Key
	if r, ok := f.idem[key]; ok {
		return r, true, nil
	}
	if len(f.pending(p.Tenant())) >= controldb.MaxPendingInvitations {
		return controldb.StoredResponse{}, false, controldb.ErrTooManyInvitations
	}
	f.seq++
	inv := controldb.Invitation{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq), Role: n.Role, CreatedBy: p.Subject(),
		CreatedAt: f.now().UTC(), ExpiresAt: n.ExpiresAt}
	if n.Label != "" {
		inv.Label = &n.Label
	}
	f.invitations[n.TokenHash] = &fakeInvitation{tenant: p.Tenant(), inv: inv}
	r, err := respond(inv)
	f.idem[key] = r
	return r, false, err
}

func (f *fakeMembers) ListInvitations(_ context.Context, p authz.Principal) ([]controldb.Invitation, error) {
	if err := authz.AuthorizeTenantWide(p, authz.MembersRead); err != nil {
		return nil, err
	}
	return f.pending(p.Tenant()), nil
}

func (f *fakeMembers) RevokeInvitation(_ context.Context, p authz.Principal, id, _ string) error {
	if err := authz.AuthorizeTenantWide(p, authz.MembersManage); err != nil {
		return err
	}
	for _, i := range f.invitations {
		if i.tenant == p.Tenant() && i.inv.ID == id && !i.accepted && !i.revoke {
			i.revoke = true
			return nil
		}
	}
	return authz.ErrNotFound
}

func (f *fakeMembers) usable(h [32]byte) (*fakeInvitation, error) {
	i, ok := f.invitations[h]
	if !ok || i.accepted || i.revoke || !i.inv.ExpiresAt.After(f.now()) {
		return nil, controldb.ErrInvitationInvalid
	}
	return i, nil
}

func (f *fakeMembers) CheckInvitation(_ context.Context, h [32]byte) (controldb.InvitationPreview, error) {
	i, err := f.usable(h)
	if err != nil {
		return controldb.InvitationPreview{}, err
	}
	name := "acme"
	return controldb.InvitationPreview{TenantName: &name, Role: i.inv.Role}, nil
}

func (f *fakeMembers) AcceptInvitation(_ context.Context, h [32]byte, userID, _, _ string) (controldb.AcceptedInvitation, error) {
	i, err := f.usable(h)
	if err != nil {
		return controldb.AcceptedInvitation{}, err
	}
	i.accepted = true
	k := [2]string{i.tenant.String(), userID}
	if r, ok := f.sessions.members[k]; ok {
		return controldb.AcceptedInvitation{Tenant: i.tenant, Role: r, AlreadyMember: true}, nil
	}
	f.sessions.members[k] = i.inv.Role
	return controldb.AcceptedInvitation{Tenant: i.tenant, Role: i.inv.Role}, nil
}

type memberPage struct {
	Data []struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
		IsSelf bool   `json:"is_self"`
	} `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

type errorBody struct {
	Error struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func decodeErr(t *testing.T, res result) errorBody {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal(res.Body, &e); err != nil {
		t.Fatalf("error body %s: %v", res.Body, err)
	}
	return e
}

func TestMembersAPI(t *testing.T) {
	store := newSessionStore()
	uid := func(sub string) string { id, _ := authz.OIDCUserID(issuer, sub); return id }
	alice, bob, dave := uid("alice"), uid("bob"), uid("dave")
	store.members[[2]string{tenantA.String(), alice}] = authz.RoleTenantAdmin
	store.members[[2]string{tenantA.String(), bob}] = authz.RoleDeveloper
	store.members[[2]string{tenantA.String(), dave}] = authz.RoleViewer
	store.members[[2]string{tenantB.String(), uid("eve")}] = authz.RoleTenantAdmin
	clock := now
	idp := &fakeIdP{codes: map[string]session.Identity{}}
	h := authHandlerACR(t, store, idp, &clock, "")
	newBrowser := func(sub string) (*browser, string) {
		b := &browser{t: t, h: h, cookies: map[string]string{}}
		b.login(idp, sub, "/")
		return b, decodeSession(t, b.do(http.MethodGet, "/api/v1/auth/session", "", nil)).Data.CSRFToken
	}
	a, csrf := newBrowser("alice")
	write := func(b *browser, csrf string, extra map[string]string) map[string]string {
		hdr := map[string]string{session.CSRFHeader: csrf, "Origin": "https://app.example.com"}
		for k, v := range extra {
			hdr[k] = v
		}
		return hdr
	}
	memberPath := func(id string) string { return "/api/v1/members/" + url.PathEscape(id) }

	// 목록(step-up 불필요): user_id 순, 자기 표시, 다른 조직 멤버는 없다. cursor로 다음 쪽
	res := a.do(http.MethodGet, "/api/v1/members?limit=2", "", nil)
	var page memberPage
	if res.StatusCode != http.StatusOK || json.Unmarshal(res.Body, &page) != nil || len(page.Data) != 2 || page.NextCursor == nil {
		t.Fatalf("members page 1: %d %s", res.StatusCode, res.Body)
	}
	res = a.do(http.MethodGet, "/api/v1/members?limit=2&cursor="+url.QueryEscape(*page.NextCursor), "", nil)
	var page2 memberPage
	if res.StatusCode != http.StatusOK || json.Unmarshal(res.Body, &page2) != nil || len(page2.Data) != 1 || page2.NextCursor != nil {
		t.Fatalf("members page 2: %d %s", res.StatusCode, res.Body)
	}
	all := append(page.Data, page2.Data...)
	self := 0
	for _, m := range all {
		if m.UserID == uid("eve") {
			t.Fatal("other tenant's member listed")
		}
		if m.IsSelf {
			self++
			if m.UserID != alice {
				t.Fatalf("is_self on %s", m.UserID)
			}
		}
	}
	if self != 1 {
		t.Fatalf("is_self count %d", self)
	}
	// 조회자는 멤버 목록을 볼 수 없다
	d, _ := newBrowser("dave")
	if res := d.do(http.MethodGet, "/api/v1/members", "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer lists members: %d", res.StatusCode)
	}

	// 변경은 step-up이 필요하다(403 step_up_required)
	res = a.do(http.MethodPatch, memberPath(bob), `{"role":"operator"}`, write(a, csrf, map[string]string{"If-Match": `"1"`}))
	if res.StatusCode != http.StatusForbidden || decodeErr(t, res).Error.Details["step_up_required"] != true {
		t.Fatalf("patch without step-up: %d %s", res.StatusCode, res.Body)
	}
	if res := a.stepUp(idp, csrf, session.Identity{Issuer: issuer, Subject: "alice", AuthTime: clock, AMR: []string{"mfa"}}, "/"); res.StatusCode != http.StatusSeeOther ||
		res.Header.Get("Location") != "/" {
		t.Fatalf("step-up: %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	// 역할 바꾸기·검증·마지막 관리자 보호
	res = a.do(http.MethodPatch, memberPath(bob), `{"role":"operator"}`, write(a, csrf, map[string]string{"If-Match": `"1"`}))
	if res.StatusCode != http.StatusOK || !strings.Contains(string(res.Body), `"role":"operator"`) || store.members[[2]string{tenantA.String(), bob}] != authz.RoleOperator ||
		res.Header.Get("ETag") != `"2"` {
		t.Fatalf("patch: %d %s", res.StatusCode, res.Body)
	}
	// If-Match: 없으면 428, 지난 revision이면 412(다른 관리자의 변경을 덮어쓰지 않는다)
	if res := a.do(http.MethodPatch, memberPath(bob), `{"role":"viewer"}`, write(a, csrf, nil)); res.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("patch without If-Match: %d", res.StatusCode)
	}
	if res := a.do(http.MethodPatch, memberPath(bob), `{"role":"viewer"}`, write(a, csrf, map[string]string{"If-Match": `"1"`})); res.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("patch with stale revision: %d", res.StatusCode)
	}
	if res := a.do(http.MethodDelete, memberPath(bob), "", write(a, csrf, map[string]string{"If-Match": `"1"`})); res.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("delete with stale revision: %d", res.StatusCode)
	}
	if res := a.do(http.MethodPatch, memberPath(bob), `{"role":"owner"}`, write(a, csrf, map[string]string{"If-Match": `"1"`})); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid role: %d", res.StatusCode)
	}
	if res := a.do(http.MethodPatch, memberPath(uid("nobody")), `{"role":"viewer"}`, write(a, csrf, map[string]string{"If-Match": `"1"`})); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown member: %d", res.StatusCode)
	}
	if res := a.do(http.MethodPatch, memberPath(uid("eve")), `{"role":"viewer"}`, write(a, csrf, map[string]string{"If-Match": `"1"`})); res.StatusCode != http.StatusNotFound {
		t.Fatalf("other tenant's member: %d", res.StatusCode)
	}
	if res := a.do(http.MethodPatch, memberPath(alice), `{"role":"viewer"}`, write(a, csrf, map[string]string{"If-Match": `"1"`})); res.StatusCode != http.StatusConflict {
		t.Fatalf("demote last admin: %d", res.StatusCode)
	}
	if res := a.do(http.MethodDelete, memberPath(alice), "", write(a, csrf, map[string]string{"If-Match": `"1"`})); res.StatusCode != http.StatusConflict {
		t.Fatalf("remove last admin: %d", res.StatusCode)
	}
	if res := a.do(http.MethodPatch, memberPath(bob), `{"role":"operator"}`, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("patch without csrf: %d", res.StatusCode)
	}

	// 초대: Idempotency-Key 필수, tenant_admin 불가, 링크는 한 번만(재생에는 없음)
	if res := a.do(http.MethodPost, "/api/v1/invitations", `{"role":"developer"}`, write(a, csrf, nil)); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("invite without key: %d", res.StatusCode)
	}
	if res := a.do(http.MethodPost, "/api/v1/invitations", `{"role":"tenant_admin"}`, write(a, csrf, map[string]string{"Idempotency-Key": "i-0"})); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("invite as admin: %d", res.StatusCode)
	}
	if res := a.do(http.MethodPost, "/api/v1/invitations", `{"role":"viewer","label":"  "}`, write(a, csrf, map[string]string{"Idempotency-Key": "i-0"})); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("blank label: %d", res.StatusCode)
	}
	res = a.do(http.MethodPost, "/api/v1/invitations", `{"role":"developer","label":"결제팀"}`, write(a, csrf, map[string]string{"Idempotency-Key": "i-1"}))
	var created struct {
		Data          InvitationItem `json:"data"`
		InvitationURL *string        `json:"invitation_url"`
		Meta          MutationMeta   `json:"meta"`
	}
	if res.StatusCode != http.StatusCreated || json.Unmarshal(res.Body, &created) != nil || created.InvitationURL == nil ||
		!strings.HasPrefix(*created.InvitationURL, "https://app.example.com/invite#mti_") || created.Data.Role != "developer" ||
		created.Data.Label == nil || *created.Data.Label != "결제팀" || created.Data.ExpiresAt != now.Add(InvitationTTL).Format(time.RFC3339Nano) {
		t.Fatalf("invite: %d %s", res.StatusCode, res.Body)
	}
	token := strings.TrimPrefix(*created.InvitationURL, "https://app.example.com/invite#")
	replay := a.do(http.MethodPost, "/api/v1/invitations", `{"role":"developer","label":"결제팀"}`, write(a, csrf, map[string]string{"Idempotency-Key": "i-1"}))
	if replay.StatusCode != http.StatusCreated || replay.Header.Get("Idempotent-Replayed") != "true" || strings.Contains(string(replay.Body), "mti_") ||
		!strings.Contains(string(replay.Body), WarnInvitationURLNotReplayed) {
		t.Fatalf("replay: %d %s", replay.StatusCode, replay.Body)
	}
	res = a.do(http.MethodGet, "/api/v1/invitations", "", nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(res.Body), created.Data.ID) || strings.Contains(string(res.Body), "mti_") {
		t.Fatalf("list invitations: %d %s", res.StatusCode, res.Body)
	}

	// 수락: token은 본문으로, Origin 검사, 잘못된 token은 404. IdP 로그인 뒤 초대한 조직의 session(역할 developer)
	carol := &browser{t: t, h: h, cookies: map[string]string{}}
	start := func(b *browser, body, origin string) result {
		return b.do(http.MethodPost, "/api/v1/auth/invitation", body, map[string]string{"Origin": origin})
	}
	if res := start(carol, `{"token":"`+token+`"}`, "https://evil.example"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("start from other origin: %d", res.StatusCode)
	}
	if res := start(carol, `{"token":"mti_`+strings.Repeat("A", 43)+`"}`, "https://app.example.com"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown token: %d", res.StatusCode)
	}
	if res := start(carol, `{"token":"x"}`, "https://app.example.com"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("malformed token: %d", res.StatusCode)
	}
	res = start(carol, `{"token":"`+token+`"}`, "https://app.example.com")
	var auth struct {
		Data struct {
			AuthorizationURL string  `json:"authorization_url"`
			TenantName       *string `json:"tenant_name"`
			Role             string  `json:"role"`
		} `json:"data"`
	}
	// 화면이 확인을 받을 수 있게 들어갈 조직·역할을 준다
	if res.StatusCode != http.StatusOK || json.Unmarshal(res.Body, &auth) != nil || idp.lastOpts.Reauthenticate ||
		auth.Data.TenantName == nil || *auth.Data.TenantName != "acme" || auth.Data.Role != "developer" {
		t.Fatalf("start: %d %s", res.StatusCode, res.Body)
	}
	state := mustURL(t, auth.Data.AuthorizationURL).Query().Get("state")
	idp.codes["inv"] = session.Identity{Issuer: issuer, Subject: "carol", Name: "Carol"}
	cb := carol.do(http.MethodGet, "/api/v1/auth/callback?code=inv&state="+url.QueryEscape(state), "", nil)
	if cb.StatusCode != http.StatusSeeOther || cb.Header.Get("Location") != "/" {
		t.Fatalf("accept callback: %d %s", cb.StatusCode, cb.Header.Get("Location"))
	}
	cs := decodeSession(t, carol.do(http.MethodGet, "/api/v1/auth/session", "", nil))
	if cs.Data.UserID != uid("carol") || cs.Data.TenantID != tenantA.String() || cs.Data.Role != "developer" {
		t.Fatalf("carol session = %+v", cs.Data)
	}
	// 한 번만 쓴다
	if res := start(&browser{t: t, h: h, cookies: map[string]string{}}, `{"token":"`+token+`"}`, "https://app.example.com"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("reuse: %d", res.StatusCode)
	}
	// 시작 뒤 폐기된 초대로 돌아오면 invitation_invalid
	res = a.do(http.MethodPost, "/api/v1/invitations", `{"role":"viewer"}`, write(a, csrf, map[string]string{"Idempotency-Key": "i-2"}))
	if err := json.Unmarshal(res.Body, &created); err != nil || res.StatusCode != http.StatusCreated {
		t.Fatalf("invite 2: %d", res.StatusCode)
	}
	token2 := strings.TrimPrefix(*created.InvitationURL, "https://app.example.com/invite#")
	frank := &browser{t: t, h: h, cookies: map[string]string{}}
	res = start(frank, `{"token":"`+token2+`"}`, "https://app.example.com")
	_ = json.Unmarshal(res.Body, &auth)
	state = mustURL(t, auth.Data.AuthorizationURL).Query().Get("state")
	if res := a.do(http.MethodDelete, "/api/v1/invitations/"+created.Data.ID, "", write(a, csrf, nil)); res.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: %d", res.StatusCode)
	}
	if res := a.do(http.MethodDelete, "/api/v1/invitations/"+created.Data.ID, "", write(a, csrf, nil)); res.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke twice: %d", res.StatusCode)
	}
	idp.codes["inv2"] = session.Identity{Issuer: issuer, Subject: "frank"}
	cb = frank.do(http.MethodGet, "/api/v1/auth/callback?code=inv2&state="+url.QueryEscape(state), "", nil)
	if cb.StatusCode != http.StatusSeeOther || cb.Header.Get("Location") != "/login?error="+session.LoginErrInvitation {
		t.Fatalf("revoked invitation callback: %d %s", cb.StatusCode, cb.Header.Get("Location"))
	}
	if _, ok := store.members[[2]string{tenantA.String(), uid("frank")}]; ok {
		t.Fatal("revoked invitation created a member")
	}

	// 빼기: 그 사용자의 session은 다음 요청부터 미인증
	bb, _ := newBrowser("bob")
	if res := a.do(http.MethodDelete, memberPath(bob), "", write(a, csrf, map[string]string{"If-Match": `"2"`})); res.StatusCode != http.StatusNoContent {
		t.Fatalf("remove: %d", res.StatusCode)
	}
	if res := bb.do(http.MethodGet, "/api/v1/auth/session", "", nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("removed member's session: %d", res.StatusCode)
	}
	if res := a.do(http.MethodDelete, memberPath(bob), "", write(a, csrf, nil)); res.StatusCode != http.StatusNotFound {
		t.Fatalf("remove twice: %d", res.StatusCode)
	}

	// step-up 15분이 지나면 다시 필요하다
	clock = clock.Add(authz.StepUpWindow + time.Second)
	if res := a.do(http.MethodPatch, memberPath(dave), `{"role":"developer"}`, write(a, csrf, map[string]string{"If-Match": `"1"`})); res.StatusCode != http.StatusForbidden {
		t.Fatalf("patch after step-up window: %d", res.StatusCode)
	}
}

// 대기 중 초대가 상한이면 409, 멤버 API를 끄면 404다.
func TestInvitationLimitAndDisabled(t *testing.T) {
	store := newSessionStore()
	alice, _ := authz.OIDCUserID(issuer, "alice")
	store.members[[2]string{tenantA.String(), alice}] = authz.RoleTenantAdmin
	clock := now
	idp := &fakeIdP{codes: map[string]session.Identity{}}
	h := authHandlerACR(t, store, idp, &clock, "")
	fm := h.cfg.Members.(*fakeMembers)
	for i := 0; i < controldb.MaxPendingInvitations; i++ {
		fm.invitations[[32]byte{byte(i), 1}] = &fakeInvitation{tenant: tenantA, inv: controldb.Invitation{ExpiresAt: now.Add(time.Hour)}}
	}
	a := &browser{t: t, h: h, cookies: map[string]string{}}
	a.login(idp, "alice", "/")
	csrf := decodeSession(t, a.do(http.MethodGet, "/api/v1/auth/session", "", nil)).Data.CSRFToken
	a.stepUp(idp, csrf, session.Identity{Issuer: issuer, Subject: "alice", AuthTime: clock, AMR: []string{"mfa"}}, "/")
	res := a.do(http.MethodPost, "/api/v1/invitations", `{"role":"viewer"}`, map[string]string{session.CSRFHeader: csrf, "Idempotency-Key": "k"})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("over limit: %d %s", res.StatusCode, res.Body)
	}

	h.cfg.Members = nil
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/members", ""}, {http.MethodGet, "/api/v1/invitations", ""},
		{http.MethodPost, "/api/v1/auth/invitation", `{"token":"x"}`},
	} {
		if res := a.do(c.method, c.path, c.body, map[string]string{session.CSRFHeader: csrf}); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s with member API off: %d", c.method, c.path, res.StatusCode)
		}
	}
}
