package controldb

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

// 멤버 관리와 초대 (ADR 0067, migration 00016).
//   - 조회(members.read)는 Tenant Admin·Security Auditor, 변경(members.manage)은 step-up한 Tenant Admin이다(ADR 0015 §2, 0066).
//   - 조직에는 Tenant Admin이 한 명 이상 남아야 한다. 마지막 관리자를 내리거나 빼면 ErrLastAdmin이다.
//   - 초대는 한 번만 쓰는 token(원문은 응답에만, 저장은 SHA-256)이고 7일 뒤 만료된다. tenant_admin으로는 초대하지 않는다.

// ErrLastAdmin은 조직의 마지막 Tenant Admin을 내리거나 빼려 했다는 뜻이다.
var ErrLastAdmin = errors.New("controldb: the tenant must keep at least one tenant admin")

// ErrTooManyInvitations는 대기 중 초대가 조직 상한(MaxPendingInvitations)에 닿았다는 뜻이다.
var ErrTooManyInvitations = errors.New("controldb: too many pending invitations")

// ErrInvitationInvalid는 초대 token이 없거나, 이미 쓰였거나, 폐기·만료됐거나, 조직이 가입을 받지 않는다는 뜻이다(구분하지 않는다).
var ErrInvitationInvalid = errors.New("controldb: invitation is invalid")

// MaxMemberPage는 멤버 목록 한 쪽의 최대 행 수다.
const MaxMemberPage = 1000

// MaxPendingInvitations는 조직의 대기 중(수락·폐기되지 않고 만료 전) 초대 상한이다.
const MaxPendingInvitations = 50

// InvitableRoles는 초대로 줄 수 있는 역할이다(tenant_admin 없음).
var InvitableRoles = []authz.Role{authz.RoleViewer, authz.RoleDeveloper, authz.RoleOperator, authz.RoleSecurityAuditor}

// Member는 조직의 멤버 한 명이다.
type Member struct {
	UserID      string
	Role        authz.Role
	DisplayName *string // 로그인 때 IdP name claim. 로그인한 적 없으면 nil
	CreatedAt   time.Time
	LastLoginAt *time.Time
	// Revision은 역할 변경마다 오른다(If-Match, D02 §20).
	Revision int64
}

// Invitation은 대기 중 초대다(token은 없다).
type Invitation struct {
	ID        string
	Role      authz.Role
	Label     *string
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// NewInvitation은 만들 초대다. TokenHash는 링크 token의 SHA-256이다.
type NewInvitation struct {
	TokenHash [32]byte
	Role      authz.Role
	Label     string // "" = 없음
	ExpiresAt time.Time
}

// InvitationPreview는 수락 전에 보여 줄 초대 내용이다(어느 조직에 어떤 역할로 들어가는지 — 사용자가 확인한다).
type InvitationPreview struct {
	TenantName *string
	Role       authz.Role
}

// AcceptedInvitation은 수락 결과다. AlreadyMember면 역할은 바꾸지 않았다(Role은 기존 역할).
type AcceptedInvitation struct {
	Tenant        authz.TenantID
	Role          authz.Role
	AlreadyMember bool
}

// MemberStore는 멤버·초대 저장소다.
type MemberStore struct{ db *DB }

// NewMemberStore는 MemberStore를 만든다.
func NewMemberStore(db *DB) *MemberStore { return &MemberStore{db: db} }

const memberColumns = `user_id, role, display_name, created_at, last_login_at, revision`

func scanMember(row pgx.Row) (Member, error) {
	var m Member
	var role string
	if err := row.Scan(&m.UserID, &role, &m.DisplayName, &m.CreatedAt, &m.LastLoginAt, &m.Revision); err != nil {
		return Member{}, err
	}
	m.Role = authz.Role(role)
	m.CreatedAt = m.CreatedAt.UTC()
	if m.LastLoginAt != nil {
		t := m.LastLoginAt.UTC()
		m.LastLoginAt = &t
	}
	return m, nil
}

// ListMembers는 user_id 순 멤버 목록이다. after가 있으면 그 user_id 다음부터다. more는 다음 쪽이 있는지다.
func (s *MemberStore) ListMembers(ctx context.Context, p authz.Principal, after string, limit int) ([]Member, bool, error) {
	if err := authz.AuthorizeTenantWide(p, authz.MembersRead); err != nil {
		return nil, false, err
	}
	if limit < 1 || limit > MaxMemberPage {
		return nil, false, errors.New("controldb: invalid member page size")
	}
	var out []Member
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+memberColumns+` FROM memberships
			WHERE tenant_id = app_tenant_id() AND user_id > $1 ORDER BY user_id LIMIT $2`, after, limit+1)
		if err != nil {
			return classify("select members", err)
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMember(rows)
			if err != nil {
				return classify("scan member", err)
			}
			out = append(out, m)
		}
		return classify("select members", rows.Err())
	})
	if err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// lockAdmins는 조직의 Tenant Admin 행을 잠그고 그 수를 센다. 역할 변경·제거가 동시에 와도 마지막 관리자 검사가 줄을 선다.
func lockAdmins(ctx context.Context, tx pgx.Tx) (int, error) {
	rows, err := tx.Query(ctx, `SELECT user_id FROM memberships WHERE tenant_id = app_tenant_id() AND role = 'tenant_admin'
		ORDER BY user_id FOR UPDATE`)
	if err != nil {
		return 0, classify("lock admins", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n, classify("lock admins", rows.Err())
}

// lockMember는 멤버 한 명을 잠그고 읽는다. 없으면 authz.ErrNotFound다.
func lockMember(ctx context.Context, tx pgx.Tx, userID string) (Member, error) {
	m, err := scanMember(tx.QueryRow(ctx, `SELECT `+memberColumns+` FROM memberships
		WHERE tenant_id = app_tenant_id() AND user_id = $1 FOR UPDATE`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Member{}, authz.ErrNotFound
	}
	if err != nil {
		return Member{}, classify("select member", err)
	}
	return m, nil
}

// revokeInvitationsBy는 그 사용자가 만든 대기 중 초대를 폐기하고 수를 돌려준다. 관리자가 빠지거나 강등되면 그가 만든 링크로
// 다시 들어오지 못하게 한다(D04 §12 deprovision).
func revokeInvitationsBy(ctx context.Context, tx pgx.Tx, userID string) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE member_invitations SET revoked_at = now()
		WHERE tenant_id = app_tenant_id() AND created_by = $1 AND accepted_at IS NULL AND revoked_at IS NULL`, userID)
	if err != nil {
		return 0, classify("revoke invitations by member", err)
	}
	return tag.RowsAffected(), nil
}

// UpdateMemberRole은 멤버의 역할을 바꾼다. ifMatch가 지금 revision과 다르면 ErrRevisionMismatch다(D02 §20).
// 같은 역할이면 아무것도 하지 않는다. 다음 요청부터 그 사용자의 session·API key에 반영된다.
// Tenant Admin에서 내려가면 그가 만든 대기 중 초대를 함께 폐기한다.
func (s *MemberStore) UpdateMemberRole(ctx context.Context, p authz.Principal, userID string, role authz.Role, ifMatch int64, requestID string) (Member, error) {
	if err := authz.AuthorizeTenantWide(p, authz.MembersManage); err != nil {
		return Member{}, err
	}
	if !role.Valid() {
		return Member{}, errors.New("controldb: invalid role")
	}
	var out Member
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		admins, err := lockAdmins(ctx, tx)
		if err != nil {
			return err
		}
		m, err := lockMember(ctx, tx, userID)
		if err != nil {
			return err
		}
		if m.Revision != ifMatch {
			return ErrRevisionMismatch
		}
		if m.Role == role {
			out = m
			return nil
		}
		if m.Role == authz.RoleTenantAdmin && admins <= 1 {
			return ErrLastAdmin
		}
		if out, err = scanMember(tx.QueryRow(ctx, `UPDATE memberships SET role = $2, revision = revision + 1
			WHERE tenant_id = app_tenant_id() AND user_id = $1 RETURNING `+memberColumns, userID, string(role))); err != nil {
			return classify("update member role", err)
		}
		var revoked int64
		if m.Role == authz.RoleTenantAdmin {
			if revoked, err = revokeInvitationsBy(ctx, tx, userID); err != nil {
				return err
			}
		}
		details, _ := json.Marshal(map[string]any{"from": string(m.Role), "to": string(role), "invitations_revoked": revoked})
		return writeAuditAndOutbox(ctx, tx, p.Tenant(), change{action: "member.role_changed", actorKind: p.Kind().String(), actorID: p.Subject(),
			resourceType: "member", resourceID: userID, revision: out.Revision, requestID: requestID, details: details})
	})
	return out, err
}

// RemoveMember는 멤버를 조직에서 뺀다. ifMatch가 있으면 지금 revision이어야 한다.
// 그 사용자의 이 조직 session과 그가 만든 대기 중 초대를 함께 폐기한다(다음 요청부터 미인증).
// 그 사용자가 발급한 API key는 발급자 역할이 없어져 인증에서 무효가 된다(ADR 0015 §1).
func (s *MemberStore) RemoveMember(ctx context.Context, p authz.Principal, userID string, ifMatch *int64, requestID string, now time.Time) error {
	if err := authz.AuthorizeTenantWide(p, authz.MembersManage); err != nil {
		return err
	}
	return s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		admins, err := lockAdmins(ctx, tx)
		if err != nil {
			return err
		}
		m, err := lockMember(ctx, tx, userID)
		if err != nil {
			return err
		}
		if ifMatch != nil && m.Revision != *ifMatch {
			return ErrRevisionMismatch
		}
		if m.Role == authz.RoleTenantAdmin && admins <= 1 {
			return ErrLastAdmin
		}
		if _, err := tx.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = app_tenant_id() AND user_id = $1`, userID); err != nil {
			return classify("delete member", err)
		}
		tag, err := tx.Exec(ctx, `UPDATE user_sessions SET revoked_at = $2
			WHERE tenant_id = app_tenant_id() AND user_id = $1 AND revoked_at IS NULL`, userID, now)
		if err != nil {
			return classify("revoke member sessions", err)
		}
		revoked, err := revokeInvitationsBy(ctx, tx, userID)
		if err != nil {
			return err
		}
		details, _ := json.Marshal(map[string]any{"role": string(m.Role), "sessions_revoked": tag.RowsAffected(), "invitations_revoked": revoked})
		return writeAuditAndOutbox(ctx, tx, p.Tenant(), change{action: "member.removed", actorKind: p.Kind().String(), actorID: p.Subject(),
			resourceType: "member", resourceID: userID, revision: m.Revision + 1, requestID: requestID, details: details})
	})
}

const invitationColumns = `invitation_id::text, role, label, created_by, created_at, expires_at`

func scanInvitation(row pgx.Row) (Invitation, error) {
	var inv Invitation
	var role string
	if err := row.Scan(&inv.ID, &role, &inv.Label, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt); err != nil {
		return Invitation{}, err
	}
	inv.Role = authz.Role(role)
	inv.CreatedAt, inv.ExpiresAt = inv.CreatedAt.UTC(), inv.ExpiresAt.UTC()
	return inv, nil
}

// CreateInvitation은 초대를 만든다(Idempotency-Key 아래, ADR 0060). 대기 중 초대가 상한이면 ErrTooManyInvitations다.
// respond가 만든 응답을 재생용으로 저장한다 — 응답에 token을 넣지 않는다(원문은 첫 응답에만).
func (s *MemberStore) CreateInvitation(ctx context.Context, p authz.Principal, n NewInvitation, idem IdempotencyRequest, requestID string,
	respond func(Invitation) (StoredResponse, error)) (StoredResponse, bool, error) {
	if err := authz.AuthorizeTenantWide(p, authz.MembersManage); err != nil {
		return StoredResponse{}, false, err
	}
	invitable := false
	for _, r := range InvitableRoles {
		invitable = invitable || r == n.Role
	}
	if !invitable || n.ExpiresAt.IsZero() {
		return StoredResponse{}, false, errors.New("controldb: invalid invitation")
	}
	var label *string
	if n.Label != "" {
		label = &n.Label
	}
	return runIdempotent(ctx, s.db, p, idem, "create invitation", func(tx pgx.Tx) (StoredResponse, error) {
		// 같은 조직의 초대 만들기를 줄 세운다(상한 검사와 삽입 사이에 다른 요청이 끼지 않게)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('member_invitations' || app_tenant_id()::text, 0))`); err != nil {
			return StoredResponse{}, classify("lock invitations", err)
		}
		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM member_invitations WHERE tenant_id = app_tenant_id()
			AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()`).Scan(&pending); err != nil {
			return StoredResponse{}, classify("count invitations", err)
		}
		if pending >= MaxPendingInvitations {
			return StoredResponse{}, ErrTooManyInvitations
		}
		id, err := newUUID()
		if err != nil {
			return StoredResponse{}, err
		}
		inv, err := scanInvitation(tx.QueryRow(ctx, `
			INSERT INTO member_invitations (tenant_id, invitation_id, token_hash, role, label, created_by, expires_at)
			VALUES (app_tenant_id(), $1, $2, $3, $4, $5, $6) RETURNING `+invitationColumns,
			id, n.TokenHash[:], string(n.Role), label, p.Subject(), n.ExpiresAt))
		if err != nil {
			return StoredResponse{}, classify("insert invitation", err)
		}
		details, _ := json.Marshal(map[string]string{"role": string(n.Role), "expires_at": inv.ExpiresAt.Format(time.RFC3339)})
		if err := writeAuditAndOutbox(ctx, tx, p.Tenant(), change{action: "member.invitation_created", actorKind: p.Kind().String(), actorID: p.Subject(),
			resourceType: "invitation", resourceID: inv.ID, revision: 1, requestID: requestID, details: details}); err != nil {
			return StoredResponse{}, err
		}
		return respond(inv)
	})
}

// ListInvitations는 대기 중(수락·폐기되지 않고 만료 전) 초대 목록이다(만든 순, 최대 MaxPendingInvitations).
func (s *MemberStore) ListInvitations(ctx context.Context, p authz.Principal) ([]Invitation, error) {
	if err := authz.AuthorizeTenantWide(p, authz.MembersRead); err != nil {
		return nil, err
	}
	var out []Invitation
	err := s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+invitationColumns+` FROM member_invitations
			WHERE tenant_id = app_tenant_id() AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
			ORDER BY created_at, invitation_id LIMIT $1`, MaxPendingInvitations)
		if err != nil {
			return classify("select invitations", err)
		}
		defer rows.Close()
		for rows.Next() {
			inv, err := scanInvitation(rows)
			if err != nil {
				return classify("scan invitation", err)
			}
			out = append(out, inv)
		}
		return classify("select invitations", rows.Err())
	})
	return out, err
}

// RevokeInvitation은 대기 중 초대를 폐기한다. 없거나 이미 수락·폐기·만료됐으면 authz.ErrNotFound다.
func (s *MemberStore) RevokeInvitation(ctx context.Context, p authz.Principal, id, requestID string) error {
	if err := authz.AuthorizeTenantWide(p, authz.MembersManage); err != nil {
		return err
	}
	return s.db.WithTenant(ctx, p.Tenant(), func(tx pgx.Tx) error {
		var role string
		err := tx.QueryRow(ctx, `UPDATE member_invitations SET revoked_at = now()
			WHERE tenant_id = app_tenant_id() AND invitation_id = $1::uuid AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
			RETURNING role`, id).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return authz.ErrNotFound
		}
		if err != nil {
			return classify("revoke invitation", err)
		}
		details, _ := json.Marshal(map[string]string{"role": role})
		return writeAuditAndOutbox(ctx, tx, p.Tenant(), change{action: "member.invitation_revoked", actorKind: p.Kind().String(), actorID: p.Subject(),
			resourceType: "invitation", resourceID: id, revision: 2, requestID: requestID, details: details})
	})
}

// lookupInvitation은 token hash로 초대의 tenant와 역할을 찾는다(tenant를 모르는 상태, invitation_lookup 정책).
// 쓸 수 없는 초대는 ErrInvitationInvalid다.
func (s *MemberStore) lookupInvitation(ctx context.Context, tokenHash [32]byte) (authz.TenantID, authz.Role, error) {
	var (
		tenant authz.TenantID
		role   authz.Role
	)
	err := s.db.inTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.invitation_lookup', $1, true)`, hex.EncodeToString(tokenHash[:])); err != nil {
			return classify("set invitation lookup", err)
		}
		var id, r string
		err := tx.QueryRow(ctx, `SELECT tenant_id::text, role FROM member_invitations
			WHERE token_hash = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()`, tokenHash[:]).Scan(&id, &r)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvitationInvalid
		}
		if err != nil {
			return classify("select invitation", err)
		}
		role = authz.Role(r)
		tenant, err = authz.ParseTenantID(id)
		return err
	})
	return tenant, role, err
}

// CheckInvitation은 초대를 지금 쓸 수 있는지 보고, 들어갈 조직 이름과 역할을 돌려준다(수락 전 확인 화면).
// 쓸 수 없거나 조직이 가입을 받지 않으면 ErrInvitationInvalid다.
func (s *MemberStore) CheckInvitation(ctx context.Context, tokenHash [32]byte) (InvitationPreview, error) {
	tenant, role, err := s.lookupInvitation(ctx, tokenHash)
	if err != nil {
		return InvitationPreview{}, err
	}
	out := InvitationPreview{Role: role}
	err = s.db.inTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant.String()); err != nil {
			return classify("set tenant", err)
		}
		// tenant는 저장된 초대 행에서 온 값이다(요청 입력이 아님)
		err := tx.QueryRow(ctx, `SELECT display_name FROM tenants WHERE id = app_tenant_id() AND status IN (`+sessionStatuses+`)`).Scan(&out.TenantName)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvitationInvalid
		}
		return classify("select tenant", err)
	})
	if err != nil {
		return InvitationPreview{}, err
	}
	return out, nil
}

// AcceptInvitation은 IdP 로그인을 마친 사용자(userID)로 초대를 수락한다. 한 트랜잭션에서 초대를 쓰고 membership을 만든다.
// 이미 그 조직의 멤버면 역할을 바꾸지 않고 초대만 쓴다(AlreadyMember). 조직이 가입을 받지 않는 상태면 ErrInvitationInvalid다.
// tenant는 저장된 초대 행에서 온다(요청 입력이 아님).
func (s *MemberStore) AcceptInvitation(ctx context.Context, tokenHash [32]byte, userID, displayName, requestID string) (AcceptedInvitation, error) {
	if userID == "" {
		return AcceptedInvitation{}, errors.New("controldb: user id is required")
	}
	tenant, _, err := s.lookupInvitation(ctx, tokenHash)
	if err != nil {
		return AcceptedInvitation{}, err
	}
	var display *string
	if displayName != "" {
		display = &displayName
	}
	out := AcceptedInvitation{Tenant: tenant}
	err = s.db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		var open bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = app_tenant_id() AND status IN (`+sessionStatuses+`))`).Scan(&open); err != nil {
			return classify("select tenant", err)
		}
		if !open {
			return ErrInvitationInvalid
		}
		var id, role string
		err := tx.QueryRow(ctx, `UPDATE member_invitations SET accepted_at = now(), accepted_by = $2
			WHERE tenant_id = app_tenant_id() AND token_hash = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
			RETURNING invitation_id::text, role`, tokenHash[:], userID).Scan(&id, &role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvitationInvalid // 그 사이 다른 사람이 썼거나 폐기·만료됐다
		}
		if err != nil {
			return classify("accept invitation", err)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role, display_name) VALUES (app_tenant_id(), $1, $2, $3)
			ON CONFLICT (tenant_id, user_id) DO NOTHING`, userID, role, display)
		if err != nil {
			return classify("insert member", err)
		}
		out.AlreadyMember = tag.RowsAffected() == 0
		var existing string
		var revision int64
		if err := tx.QueryRow(ctx, `SELECT role, revision FROM memberships WHERE tenant_id = app_tenant_id() AND user_id = $1`, userID).Scan(&existing, &revision); err != nil {
			return classify("select member", err)
		}
		out.Role = authz.Role(existing)
		details, _ := json.Marshal(map[string]any{"invitation_id": id, "role": string(out.Role), "already_member": out.AlreadyMember})
		return writeAuditAndOutbox(ctx, tx, tenant, change{action: "member.invitation_accepted", actorKind: "user", actorID: userID,
			resourceType: "member", resourceID: userID, revision: revision, requestID: requestID, details: details})
	})
	if err != nil {
		return AcceptedInvitation{}, err
	}
	return out, nil
}
