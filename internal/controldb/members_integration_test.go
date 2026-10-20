//go:build integration

package controldb

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

// 멤버 관리와 초대 (migration 00016, ADR 0067): 권한·step-up, 마지막 관리자 보호(동시 요청 포함), 제거 시 session 폐기,
// 초대 한 번만·폐기·만료·정지 조직·상한, tenant 범위, 열 단위 권한, 감사.
func TestMemberStore(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	ms := NewMemberStore(db)
	ctx := context.Background()
	now := time.Now().UTC()
	user := func(f fixture, id string, role authz.Role) authz.Principal {
		t.Helper()
		p, err := authz.NewUserPrincipal(f.tenant, id, role, now, now)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	addMember := func(f fixture, id, role string) {
		t.Helper()
		if err := db.WithTenant(ctx, f.tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, $3)`, f.tenant.String(), id, role)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	actions := func(f fixture, resource string) []string {
		t.Helper()
		var out []string
		if err := db.WithTenant(ctx, f.tenant, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT action FROM audit_events WHERE resource_id = $1 ORDER BY occurred_at, action`, resource)
			if err != nil {
				return err
			}
			out, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// 목록: 조직 범위, user_id 순, 쪽 나누기. 개발자는 볼 수 없다
	onlyA := "only-a-" + randomSuffix(t)
	addMember(a, onlyA, "viewer")
	got, more, err := ms.ListMembers(ctx, a.admin, "", 2)
	if err != nil || len(got) != 2 || !more || got[0].UserID != "admin" || got[1].UserID != "dev" {
		t.Fatalf("list page 1 = %+v %v %v", got, more, err)
	}
	if got, more, err = ms.ListMembers(ctx, a.admin, "dev", 2); err != nil || len(got) != 1 || more || got[0].UserID != onlyA {
		t.Fatalf("list page 2 = %+v %v %v", got, more, err)
	}
	if bs, _, _ := ms.ListMembers(ctx, b.admin, "", 10); len(bs) != 2 {
		t.Fatalf("tenant b members = %+v", bs)
	}
	if _, _, err := ms.ListMembers(ctx, user(a, "dev", authz.RoleDeveloper), "", 10); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("developer lists members: %v", err)
	}
	if _, _, err := ms.ListMembers(ctx, user(a, "aud", authz.RoleSecurityAuditor), "", 10); err != nil {
		t.Fatalf("auditor lists members: %v", err)
	}

	// 변경: step-up 필요, 다른 조직 멤버는 없는 것, 마지막 관리자 보호
	noStepUp, _ := authz.NewUserPrincipal(a.tenant, "admin", authz.RoleTenantAdmin, time.Time{}, now)
	if _, err := ms.UpdateMemberRole(ctx, noStepUp, "dev", authz.RoleOperator, ""); !errors.Is(err, authz.ErrStepUpRequired) {
		t.Fatalf("update without step-up: %v", err)
	}
	m, err := ms.UpdateMemberRole(ctx, a.admin, "dev", authz.RoleOperator, "req-1")
	if err != nil || m.Role != authz.RoleOperator {
		t.Fatalf("update = %+v %v", m, err)
	}
	if _, err := ms.UpdateMemberRole(ctx, b.admin, onlyA, authz.RoleOperator, ""); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("update other tenant's member: %v", err)
	}
	if _, err := ms.UpdateMemberRole(ctx, a.admin, "admin", authz.RoleViewer, ""); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demote last admin: %v", err)
	}
	if err := ms.RemoveMember(ctx, a.admin, "admin", "", now); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("remove last admin: %v", err)
	}
	if acts := actions(a, "dev"); len(acts) != 1 || acts[0] != "member.role_changed" {
		t.Fatalf("audit = %v", acts)
	}

	// 관리자 둘이 서로를 동시에 내려도 한 명은 남는다(관리자 행 잠금)
	addMember(a, "admin2", "tenant_admin")
	admin2 := user(a, "admin2", authz.RoleTenantAdmin)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); _, errs[0] = ms.UpdateMemberRole(ctx, admin2, "admin", authz.RoleViewer, "") }()
	go func() {
		defer wg.Done()
		_, errs[1] = ms.UpdateMemberRole(ctx, a.admin, "admin2", authz.RoleViewer, "")
	}()
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) || (!errors.Is(errs[0], ErrLastAdmin) && !errors.Is(errs[1], ErrLastAdmin)) {
		t.Fatalf("concurrent demotion = %v, %v", errs[0], errs[1])
	}
	survivor := a.admin
	if errs[1] != nil { // admin이 내려갔다
		survivor = admin2
	}

	// 빼기: membership과 그 사용자의 이 조직 session이 함께 끝난다
	sessions := NewSessionStore(db)
	h := sha256.Sum256([]byte("tok-" + randomSuffix(t)))
	if err := sessions.CreateSession(ctx, NewSession{IDHash: h, Tenant: a.tenant, UserID: onlyA, DisplayName: "Only A", Now: now,
		IdleExpiresAt: now.Add(30 * time.Minute), ExpiresAt: now.Add(12 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := ms.ListMembers(ctx, survivor, "only-", 10); len(got) != 1 || got[0].DisplayName == nil || *got[0].DisplayName != "Only A" || got[0].LastLoginAt == nil {
		t.Fatalf("login did not record name/last login: %+v", got)
	}
	if err := ms.RemoveMember(ctx, survivor, onlyA, "req-rm", now); err != nil {
		t.Fatal(err)
	}
	if rec, err := sessions.LookupSession(ctx, h); err != nil || rec.RevokedAt == nil || rec.Role != "" {
		t.Fatalf("removed member's session = %+v %v", rec, err)
	}
	if err := ms.RemoveMember(ctx, survivor, onlyA, "", now); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("remove twice: %v", err)
	}
	if acts := actions(a, onlyA); strings.Join(acts, ",") != "session.created,member.removed" {
		t.Fatalf("audit = %v", acts)
	}

	// 초대: 멱등 생성, 목록(token 없음), 다른 조직에서는 보이지 않음
	invite := func(f fixture, p authz.Principal, key string, role authz.Role, expires time.Time) ([32]byte, Invitation, bool) {
		t.Helper()
		hash := sha256.Sum256([]byte("mti_" + randomSuffix(t)))
		var inv Invitation
		_, replayed, err := ms.CreateInvitation(ctx, p, NewInvitation{TokenHash: hash, Role: role, Label: "team", ExpiresAt: expires},
			IdempotencyRequest{Key: key, MethodPath: "POST /api/v1/invitations", RequestHash: sha256.Sum256([]byte(key))}, "",
			func(i Invitation) (StoredResponse, error) {
				inv = i
				body, _ := json.Marshal(map[string]string{"id": i.ID})
				return StoredResponse{Status: 201, Body: body}, nil
			})
		if err != nil {
			t.Fatal(err)
		}
		return hash, inv, replayed
	}
	if _, _, err := ms.CreateInvitation(ctx, survivor, NewInvitation{TokenHash: sha256.Sum256([]byte("x")), Role: authz.RoleTenantAdmin, ExpiresAt: now.Add(time.Hour)},
		IdempotencyRequest{Key: "k", MethodPath: "p"}, "", nil); err == nil {
		t.Fatal("invitation as tenant_admin accepted")
	}
	hash1, inv1, _ := invite(a, survivor, "inv-1", authz.RoleDeveloper, now.Add(7*24*time.Hour))
	if _, _, replayed := invite(a, survivor, "inv-1", authz.RoleDeveloper, now.Add(7*24*time.Hour)); !replayed {
		t.Fatal("same Idempotency-Key created twice")
	}
	if invs, err := ms.ListInvitations(ctx, survivor); err != nil || len(invs) != 1 || invs[0].ID != inv1.ID || invs[0].Label == nil {
		t.Fatalf("invitations = %+v %v", invs, err)
	}
	if invs, _ := ms.ListInvitations(ctx, b.admin); len(invs) != 0 {
		t.Fatalf("tenant b sees tenant a's invitation: %+v", invs)
	}
	if err := ms.RevokeInvitation(ctx, b.admin, inv1.ID, ""); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("revoke from other tenant: %v", err)
	}

	// 수락: tenant는 초대에서, 한 번만
	if err := ms.CheckInvitation(ctx, hash1); err != nil {
		t.Fatal(err)
	}
	newUser := "oidc:000000000000:new-" + randomSuffix(t)
	acc, err := ms.AcceptInvitation(ctx, hash1, newUser, "New User", "req-acc")
	if err != nil || acc.Tenant != a.tenant || acc.Role != authz.RoleDeveloper || acc.AlreadyMember {
		t.Fatalf("accept = %+v %v", acc, err)
	}
	if _, err := ms.AcceptInvitation(ctx, hash1, "someone-else", "", ""); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("accept twice: %v", err)
	}
	if err := ms.CheckInvitation(ctx, hash1); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("check used invitation: %v", err)
	}
	if got, _, _ := ms.ListMembers(ctx, survivor, "oidc:", 10); len(got) != 1 || got[0].UserID != newUser || got[0].Role != authz.RoleDeveloper ||
		got[0].DisplayName == nil || *got[0].DisplayName != "New User" {
		t.Fatalf("accepted member = %+v", got)
	}
	if acts := actions(a, newUser); len(acts) != 1 || acts[0] != "member.invitation_accepted" {
		t.Fatalf("audit = %v", acts)
	}

	// 이미 멤버면 역할을 바꾸지 않는다
	hash2, _, _ := invite(a, survivor, "inv-2", authz.RoleViewer, now.Add(time.Hour))
	if acc, err := ms.AcceptInvitation(ctx, hash2, "dev", "", ""); err != nil || !acc.AlreadyMember || acc.Role != authz.RoleOperator {
		t.Fatalf("accept as member = %+v %v", acc, err)
	}

	// 폐기·만료·정지 조직
	hash3, inv3, _ := invite(a, survivor, "inv-3", authz.RoleViewer, now.Add(time.Hour))
	if err := ms.RevokeInvitation(ctx, survivor, inv3.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := ms.RevokeInvitation(ctx, survivor, inv3.ID, ""); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("revoke twice: %v", err)
	}
	if _, err := ms.AcceptInvitation(ctx, hash3, "x-"+randomSuffix(t), "", ""); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("accept revoked: %v", err)
	}
	hash4, _, _ := invite(a, survivor, "inv-4", authz.RoleViewer, now.Add(-time.Second))
	if _, err := ms.AcceptInvitation(ctx, hash4, "x-"+randomSuffix(t), "", ""); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("accept expired: %v", err)
	}
	hash5, _, _ := invite(a, survivor, "inv-5", authz.RoleViewer, now.Add(time.Hour))
	if _, err := adminConn(t).Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, a.tenant.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.AcceptInvitation(ctx, hash5, "x-"+randomSuffix(t), "", ""); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("accept in suspended tenant: %v", err)
	}
	if _, err := adminConn(t).Exec(ctx, `UPDATE tenants SET status = 'active' WHERE id = $1`, a.tenant.String()); err != nil {
		t.Fatal(err)
	}

	// 대기 중 초대 상한(조직마다)
	for i := len(mustList(t, ms, survivor)); i < MaxPendingInvitations; i++ {
		invite(a, survivor, fmt.Sprintf("fill-%d", i), authz.RoleViewer, now.Add(time.Hour))
	}
	_, _, err = ms.CreateInvitation(ctx, survivor, NewInvitation{TokenHash: sha256.Sum256([]byte("over" + randomSuffix(t))), Role: authz.RoleViewer, ExpiresAt: now.Add(time.Hour)},
		IdempotencyRequest{Key: "over", MethodPath: "POST /api/v1/invitations"}, "", func(Invitation) (StoredResponse, error) { return StoredResponse{Status: 201, Body: []byte(`{}`)}, nil })
	if !errors.Is(err, ErrTooManyInvitations) {
		t.Fatalf("over limit: %v", err)
	}
	if invs, _ := ms.ListInvitations(ctx, b.admin); len(invs) != 0 {
		t.Fatal("limit counted across tenants")
	}

	// 앱 role은 초대의 역할·token hash를 바꾸지 못한다(열 단위 GRANT)
	err = db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE member_invitations SET role = 'operator' WHERE tenant_id = $1`, a.tenant.String())
		return err
	})
	if err == nil {
		t.Fatal("app role changed invitation role")
	}
}

func mustList(t *testing.T, ms *MemberStore, p authz.Principal) []Invitation {
	t.Helper()
	invs, err := ms.ListInvitations(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return invs
}
