//go:build integration

package controldb

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

func keyWithEnvs(t *testing.T, f fixture, envs []string) authz.Principal {
	t.Helper()
	gen := f.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, envs)
	p, err := f.authenticate(gen.Token, authz.KindAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestServiceCatalog(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	store := NewServiceStore(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := func(n int) string { return "aaaaaaaa-0000-4000-8000-00000000000" + string(rune('0'+n)) }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(store.Observe(ctx, a.tenant, []ServiceObservation{
		{ServiceID: id(1), Environment: "prod", Namespace: "shop", Name: "Checkout", Language: "go", SeenAt: now.Add(-time.Hour)},
		{ServiceID: id(2), Environment: "staging", Namespace: "shop", Name: "checkout", SeenAt: now.Add(-time.Hour)},
		{ServiceID: id(3), Environment: "prod", Namespace: "", Name: "payment", SeenAt: now.Add(-48 * time.Hour)},     // inactive
		{ServiceID: id(4), Environment: "prod", Namespace: "", Name: "legacy", SeenAt: now.Add(-40 * 24 * time.Hour)}, // archived
	}))
	// 재관측: last_seen은 뒤로 가지 않고, 모르는 언어(빈 값)가 아는 언어를 지우지 않는다
	must(store.Observe(ctx, a.tenant, []ServiceObservation{
		{ServiceID: id(1), Environment: "prod", Namespace: "shop", Name: "Checkout", SeenAt: now.Add(-2 * time.Hour)},
		{ServiceID: id(2), Environment: "staging", Namespace: "shop", Name: "checkout", Language: "java", SeenAt: now},
	}))
	must(store.Observe(ctx, b.tenant, []ServiceObservation{{ServiceID: id(1), Environment: "prod", Name: "b-only", SeenAt: now}}))

	admin := keyWithEnvs(t, a, nil)
	list := func(p authz.Principal, q ServiceQuery) []Service {
		t.Helper()
		q.Now = now
		if q.Limit == 0 {
			q.Limit = 100
		}
		out, _, err := store.ListServices(ctx, p, q)
		must(err)
		return out
	}
	got := list(admin, ServiceQuery{})
	if len(got) != 3 || got[0].Name != "Checkout" || got[1].Name != "checkout" || got[2].Name != "payment" {
		t.Fatalf("services = %+v", got)
	}
	if !got[0].LastSeen.Equal(now.Add(-time.Hour)) || got[0].Language == nil || *got[0].Language != "go" || got[0].Status != "active" {
		t.Errorf("Checkout = %+v", got[0])
	}
	if got[1].Language == nil || *got[1].Language != "java" || got[2].Status != "inactive" {
		t.Errorf("checkout/payment = %+v %+v", got[1], got[2])
	}
	if all := list(admin, ServiceQuery{IncludeArchived: true}); len(all) != 4 || all[2].Name != "legacy" || all[2].Status != "archived" {
		t.Errorf("with archived = %+v", all)
	}
	// environment 제한 key: prod만
	for _, s := range list(keyWithEnvs(t, a, []string{"prod"}), ServiceQuery{}) {
		if s.Environment != "prod" {
			t.Errorf("env-scoped key saw %s/%s", s.Environment, s.Name)
		}
	}
	// keyset
	p1, more, err := store.ListServices(ctx, admin, ServiceQuery{Limit: 2, Now: now})
	must(err)
	if !more || len(p1) != 2 {
		t.Fatalf("page1 = %d more=%v", len(p1), more)
	}
	last := p1[1]
	p2 := list(admin, ServiceQuery{Limit: 2, After: &ServicePosition{NameNormalized: last.NameNormalized, Environment: last.Environment, ServiceID: last.ServiceID}})
	if len(p2) != 1 || p2[0].Name != "payment" {
		t.Errorf("page2 = %+v", p2)
	}
	// tenant 분리
	if bs := list(keyWithEnvs(t, b, nil), ServiceQuery{}); len(bs) != 1 || bs[0].Name != "b-only" {
		t.Errorf("tenant B = %+v", bs)
	}
}

// 앱 role은 사용자 관리 필드(owner_team 등)를 바꿀 수 없다 — agent 관측이 owner를 덮어쓰지 않는다(D02 §08).
func TestServiceOwnerFieldsNotWritableByApp(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	ctx := context.Background()
	if err := NewServiceStore(db).Observe(ctx, f.tenant, []ServiceObservation{
		{ServiceID: "aaaaaaaa-0000-4000-8000-000000000009", Environment: "prod", Name: "svc", SeenAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	err := db.WithTenant(ctx, f.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE services SET owner_team = 'attacker' WHERE tenant_id = app_tenant_id()`)
		return err
	})
	if err == nil || !isPgCode(err, "42501") { // insufficient_privilege
		t.Errorf("app role updated owner_team: %v", err)
	}
}
