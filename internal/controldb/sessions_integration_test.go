//go:build integration

package controldb

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

// session 저장소 (migration 00013, ADR 0064): 조회 정책은 정확한 hash·한 사용자만, 폐기·연장은 tenant 범위,
// 역할은 요청마다 membership에서 다시 읽는다.
func TestSessionStore(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	store := NewSessionStore(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	addMember := func(f fixture, user, role string) {
		t.Helper()
		if err := db.WithTenant(ctx, f.tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, $3)`, f.tenant.String(), user, role)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	user := "oidc:000000000000:alice-" + randomSuffix(t)
	addMember(a, user, "developer")
	addMember(b, user, "viewer")

	// 로그인: 그 사용자의 membership만 보인다(다른 사용자의 membership은 member_lookup에 걸리지 않는다)
	// 조직 표시 이름(migration 00014)은 provisioning이 정하고 로그인 목록에 함께 나온다. 없으면 nil
	slug := "org-" + randomSuffix(t)
	if _, err := adminConn(t).Exec(ctx, `UPDATE tenants SET display_name = $2 WHERE id = $1`, a.tenant.String(), slug); err != nil {
		t.Fatal(err)
	}
	ms, err := store.UserMemberships(ctx, user)
	if err != nil || len(ms) != 2 {
		t.Fatalf("memberships = %v %v", ms, err)
	}
	for _, m := range ms {
		switch {
		case m.Tenant == a.tenant && (m.TenantName == nil || *m.TenantName != slug):
			t.Errorf("tenant a name = %v", m.TenantName)
		case m.Tenant == b.tenant && m.TenantName != nil:
			t.Errorf("tenant b name = %v", *m.TenantName)
		}
	}
	for _, bad := range []string{"Bad Name", a.tenant.String()} {
		if _, err := adminConn(t).Exec(ctx, `UPDATE tenants SET display_name = $2 WHERE id = $1`, b.tenant.String(), bad); err == nil {
			t.Errorf("display_name accepted %q (non-slug or tenant-id shaped)", bad)
		}
	}
	if other, err := store.UserMemberships(ctx, "admin"); err != nil || len(other) == 0 {
		t.Fatalf("admin memberships = %v %v", other, err)
	} else {
		for _, m := range other {
			if m.Role != authz.RoleTenantAdmin {
				t.Errorf("membership of another user leaked: %+v", m)
			}
		}
	}

	h := sha256.Sum256([]byte("token-" + randomSuffix(t)))
	n := NewSession{IDHash: h, Tenant: a.tenant, UserID: user, DisplayName: "Alice", Now: now,
		IdleExpiresAt: now.Add(30 * time.Minute), ExpiresAt: now.Add(12 * time.Hour), RequestID: "req-s"}
	if err := store.CreateSession(ctx, n); err != nil {
		t.Fatal(err)
	}
	// member가 아닌 tenant로는 만들지 못한다
	if err := store.CreateSession(ctx, NewSession{IDHash: sha256.Sum256([]byte("x" + randomSuffix(t))), Tenant: a.tenant, UserID: "nobody",
		Now: now, IdleExpiresAt: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour)}); !errors.Is(err, ErrNotMember) {
		t.Fatalf("non-member session: %v", err)
	}

	rec, err := store.LookupSession(ctx, h)
	if err != nil || rec.Tenant != a.tenant || rec.UserID != user || rec.Role != authz.RoleDeveloper || rec.DisplayName == nil || *rec.DisplayName != "Alice" {
		t.Fatalf("lookup = %+v %v", rec, err)
	}
	if _, err := store.LookupSession(ctx, sha256.Sum256([]byte("unknown"))); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("unknown hash: %v", err)
	}

	// 연장은 최대 만료를 넘지 않는다. 다른 tenant 범위로는 바꾸지 못한다(행이 안 보인다)
	if err := store.TouchSession(ctx, a.tenant, h, now.Add(time.Minute), now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rec, _ = store.LookupSession(ctx, h)
	if !rec.IdleExpiresAt.Equal(rec.ExpiresAt) || !rec.LastSeenAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("touch = %+v", rec)
	}
	if err := store.RevokeSession(ctx, b.tenant, h, user, "logout", "", now); err != nil {
		t.Fatal(err)
	}
	if rec, _ := store.LookupSession(ctx, h); rec.RevokedAt != nil {
		t.Fatal("revoked through another tenant's scope")
	}

	// 역할 변경·제거는 다음 조회부터
	if err := db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE memberships SET role = 'viewer' WHERE tenant_id = $1 AND user_id = $2`, a.tenant.String(), user)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := store.LookupSession(ctx, h); rec.Role != authz.RoleViewer {
		t.Fatalf("role after change = %q", rec.Role)
	}
	if err := db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, a.tenant.String(), user)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := store.LookupSession(ctx, h); rec.Role != "" {
		t.Fatalf("role after removal = %q", rec.Role)
	}

	// 정지된 조직: 로그인 목록에서 빠지고, 살아 있는 session도 미인증이 된다(D04 §02 tenant 폐쇄)
	addMember(a, user, "developer")
	if rec, _ := store.LookupSession(ctx, h); rec.Role != authz.RoleDeveloper {
		t.Fatalf("role after re-add = %q", rec.Role)
	}
	if _, err := adminConn(t).Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, a.tenant.String()); err != nil {
		t.Fatal(err)
	}
	if rec, _ := store.LookupSession(ctx, h); rec.Role != "" {
		t.Fatalf("suspended tenant session role = %q", rec.Role)
	}
	if ms, _ := store.UserMemberships(ctx, user); len(ms) != 1 || ms[0].Tenant != b.tenant {
		t.Fatalf("memberships with suspended tenant = %v", ms)
	}
	if err := store.CreateSession(ctx, NewSession{IDHash: sha256.Sum256([]byte("s" + randomSuffix(t))), Tenant: a.tenant, UserID: user,
		Now: now, IdleExpiresAt: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour)}); !errors.Is(err, ErrNotMember) {
		t.Fatalf("session in suspended tenant: %v", err)
	}
	if _, err := adminConn(t).Exec(ctx, `UPDATE tenants SET status = 'active' WHERE id = $1`, a.tenant.String()); err != nil {
		t.Fatal(err)
	}

	// 폐기와 감사(토큰·hash는 감사에 없다)
	if err := store.RevokeSession(ctx, a.tenant, h, user, "logout", "req-r", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if rec, _ := store.LookupSession(ctx, h); rec.RevokedAt == nil {
		t.Fatal("not revoked")
	}
	var actions []string
	if err := db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT action FROM audit_events WHERE actor_id = $1 ORDER BY occurred_at, action`, user)
		if err != nil {
			return err
		}
		actions, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[0] != "session.created" || actions[1] != "session.revoked" {
		t.Fatalf("audit = %v", actions)
	}

	// 앱 role은 session의 tenant·user·최대 만료를 바꾸지 못한다(열 단위 GRANT)
	err = db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE user_sessions SET expires_at = expires_at + interval '1 day' WHERE tenant_id = $1`, a.tenant.String())
		return err
	})
	if err == nil {
		t.Fatal("app role extended expires_at")
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	return id[:8]
}
