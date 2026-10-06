//go:build integration

package controldb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
)

// insertAudit은 owner 계정으로 지정 시각의 감사 행을 넣는다(앱 role은 occurred_at을 고를 수 없다).
func insertAudit(t *testing.T, tenant authz.TenantID, category, action string, at time.Time) string {
	t.Helper()
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = adminConn(t).Exec(context.Background(), `
		INSERT INTO audit_events (tenant_id, id, occurred_at, category, action, actor_kind, actor_id, resource_type, resource_id)
		VALUES ($1, $2, $3, $4, $5, 'user', 'u1', 'monitor', 'm1')`, tenant.String(), id, at, category, action)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustUserP(t *testing.T, tenant authz.TenantID, role authz.Role) authz.Principal {
	t.Helper()
	p, err := authz.NewUserPrincipal(tenant, "u-"+string(role), role, time.Time{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestListAuditEvents(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	store := NewAuditStore(db)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	var ops []string
	for i := 0; i < 3; i++ {
		ops = append(ops, insertAudit(t, a.tenant, "operations", "monitor.updated", base.Add(time.Duration(i)*time.Minute)))
	}
	sec := insertAudit(t, a.tenant, "security", "member.removed", base.Add(10*time.Minute))
	insertAudit(t, b.tenant, "operations", "monitor.updated", base.Add(time.Minute))
	q := AuditQuery{From: base, To: base.Add(30 * time.Minute), Limit: 1000}

	// admin(audit.read): 두 범주, 시간 내림차순. 다른 tenant 행은 없다.
	got, more, err := store.ListAuditEvents(ctx, mustUserP(t, a.tenant, authz.RoleTenantAdmin), q)
	if err != nil || more || len(got) != 4 || got[0].ID != sec || got[3].ID != ops[0] {
		t.Fatalf("admin = %d rows more=%v err=%v", len(got), more, err)
	}
	// operator: operations만
	got, _, err = store.ListAuditEvents(ctx, mustUserP(t, a.tenant, authz.RoleOperator), q)
	if err != nil || len(got) != 3 {
		t.Fatalf("operator = %d rows, err=%v", len(got), err)
	}
	for _, e := range got {
		if e.Category != "operations" {
			t.Errorf("operator saw %s", e.Category)
		}
	}
	// operator가 security 요청 → forbidden
	q2 := q
	q2.Categories = []string{"security"}
	if _, _, err := store.ListAuditEvents(ctx, mustUserP(t, a.tenant, authz.RoleOperator), q2); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("operator security = %v", err)
	}
	// keyset: limit 2 → 다음 page는 마지막 위치 다음부터
	q3 := q
	q3.Limit = 2
	page1, more, err := store.ListAuditEvents(ctx, mustUserP(t, a.tenant, authz.RoleTenantAdmin), q3)
	if err != nil || !more || len(page1) != 2 {
		t.Fatalf("page1 = %d more=%v err=%v", len(page1), more, err)
	}
	q3.After = &AuditPosition{OccurredAt: page1[1].OccurredAt, ID: page1[1].ID}
	page2, more, err := store.ListAuditEvents(ctx, mustUserP(t, a.tenant, authz.RoleTenantAdmin), q3)
	if err != nil || more || len(page2) != 2 || page2[1].ID != ops[0] {
		t.Fatalf("page2 = %+v more=%v err=%v", page2, more, err)
	}
	// action 필터와 [from, to) 경계
	q4 := AuditQuery{From: base.Add(time.Minute), To: base.Add(2 * time.Minute), Action: "monitor.updated", Limit: 10}
	got, _, err = store.ListAuditEvents(ctx, mustUserP(t, a.tenant, authz.RoleTenantAdmin), q4)
	if err != nil || len(got) != 1 || got[0].ID != ops[1] {
		t.Fatalf("bounded = %+v err=%v", got, err)
	}
	// 권한 없는 role은 DB에 닿기 전에 거절
	if _, _, err := store.ListAuditEvents(ctx, mustUserP(t, a.tenant, authz.RoleDeveloper), q); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("developer = %v", err)
	}
}

// 같은 시각의 행이 여럿이어도 limit 1로 끝까지 순회하면 누락·중복이 없다(정렬과 keyset 비교가 같은 uuid 순서).
func TestListAuditEventsSameTimestamp(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	store := NewAuditStore(db)
	ctx := context.Background()
	at := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	want := map[string]bool{}
	for i := 0; i < 6; i++ {
		want[insertAudit(t, f.tenant, "operations", "monitor.updated", at)] = true
	}
	admin := mustUserP(t, f.tenant, authz.RoleTenantAdmin)
	q := AuditQuery{From: at, To: at.Add(time.Second), Limit: 1}
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		page, more, err := store.ListAuditEvents(ctx, admin, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page {
			if seen[e.ID] {
				t.Fatalf("duplicate %s", e.ID)
			}
			seen[e.ID] = true
		}
		if !more {
			break
		}
		q.After = &AuditPosition{OccurredAt: page[0].OccurredAt, ID: page[0].ID}
	}
	if len(seen) != len(want) {
		t.Fatalf("saw %d of %d rows", len(seen), len(want))
	}
}
