//go:build integration

package controldb

import (
	"context"
	"fmt"
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
	id := func(n int) string { return fmt.Sprintf("aaaaaaaa-0000-4000-8000-%012d", n) }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	observe := func(tenant authz.TenantID, obs []ServiceObservation) {
		t.Helper()
		rejected, err := store.Observe(ctx, tenant, obs)
		if err != nil || rejected != 0 {
			t.Fatalf("observe: rejected=%d err=%v", rejected, err)
		}
	}
	observe(a.tenant, []ServiceObservation{
		{ServiceID: id(1), Environment: "prod", Namespace: "shop", Name: "Checkout", Language: "go", SeenAt: now.Add(-time.Hour)},
		{ServiceID: id(2), Environment: "staging", Namespace: "shop", Name: "checkout", SeenAt: now.Add(-time.Hour)},
		{ServiceID: id(3), Environment: "prod", Namespace: "", Name: "payment", SeenAt: now.Add(-48 * time.Hour)},     // inactive
		{ServiceID: id(4), Environment: "prod", Namespace: "", Name: "legacy", SeenAt: now.Add(-40 * 24 * time.Hour)}, // archived
		// 정확히 30일: archived (include_archived=false 목록에 나오지 않는다)
		{ServiceID: id(5), Environment: "prod", Namespace: "", Name: "zz-boundary", SeenAt: now.Add(-ServiceArchivedAfter)},
	})
	// 재관측: last_seen은 뒤로 가지 않고, 모르는 언어(빈 값)가 아는 언어를 지우지 않는다
	observe(a.tenant, []ServiceObservation{
		{ServiceID: id(1), Environment: "prod", Namespace: "shop", Name: "Checkout", SeenAt: now.Add(-2 * time.Hour)},
		{ServiceID: id(2), Environment: "staging", Namespace: "shop", Name: "checkout", Language: "java", SeenAt: now},
	})
	observe(b.tenant, []ServiceObservation{{ServiceID: id(1), Environment: "prod", Name: "b-only", SeenAt: now}})

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
	all := list(admin, ServiceQuery{IncludeArchived: true})
	if len(all) != 5 || all[2].Name != "legacy" || all[2].Status != "archived" || all[4].Name != "zz-boundary" || all[4].Status != "archived" {
		t.Errorf("with archived = %+v", all)
	}
	// environment 제한 key: prod의 보관되지 않은 서비스만, 정확히
	if prod := list(keyWithEnvs(t, a, []string{"prod"}), ServiceQuery{}); len(prod) != 2 || prod[0].Name != "Checkout" || prod[1].Name != "payment" {
		t.Errorf("env-scoped key = %+v", prod)
	}
	// keyset: limit=1로 이름 정규형이 같은 Checkout/prod → checkout/staging 경계를 넘는다
	var names []string
	var after *ServicePosition
	for page := 0; page < 10; page++ {
		got, more, err := store.ListServices(ctx, admin, ServiceQuery{Limit: 1, After: after, Now: now})
		must(err)
		if len(got) != 1 {
			t.Fatalf("page %d = %+v", page, got)
		}
		names = append(names, got[0].Name+"/"+got[0].Environment)
		after = &ServicePosition{NameNormalized: got[0].NameNormalized, Environment: got[0].Environment, ServiceID: got[0].ServiceID}
		if !more {
			break
		}
	}
	if fmt.Sprint(names) != "[Checkout/prod checkout/staging payment/prod]" {
		t.Errorf("keyset pages = %v", names)
	}
	// tenant 분리
	if bs := list(keyWithEnvs(t, b, nil), ServiceQuery{}); len(bs) != 1 || bs[0].Name != "b-only" {
		t.Errorf("tenant B = %+v", bs)
	}
	// 이름 풀기(ADR 0039): 대소문자 무시, archived 포함, environment 제한, 입력 이름이 key, 다른 tenant 이름은 안 풀림
	res, err := store.ResolveServiceNames(ctx, admin, []string{"CHECKOUT", "legacy", "b-only", "nope"})
	must(err)
	if fmt.Sprint(res) != fmt.Sprintf("map[CHECKOUT:[%s %s] legacy:[%s]]", id(1), id(2), id(4)) {
		t.Errorf("resolve = %v", res)
	}
	prodKey := keyWithEnvs(t, a, []string{"prod"})
	if res, err := store.ResolveServiceNames(ctx, prodKey, []string{"checkout"}); err != nil || fmt.Sprint(res) != fmt.Sprintf("map[checkout:[%s]]", id(1)) {
		t.Errorf("resolve (prod key) = %v %v", res, err)
	}
	// environment 범위 service_id: prod의 모든 서비스(archived 포함), staging 제외
	if got, err := store.EnvironmentServiceIDs(ctx, prodKey); err != nil || fmt.Sprint(got) != fmt.Sprint([]string{id(1), id(3), id(4), id(5)}) {
		t.Errorf("env services = %v %v", got, err)
	}
	if got, err := store.EnvironmentServiceIDs(ctx, keyWithEnvs(t, a, []string{"dev"})); err != nil || got == nil || len(got) != 0 {
		t.Errorf("env with no services = %#v %v", got, err)
	}
	if _, err := store.EnvironmentServiceIDs(ctx, admin); err == nil {
		t.Error("unrestricted principal got an environment scope")
	}
	// 단건 조회: archived 포함, 다른 tenant·허용 밖 environment·없는 id는 모두 같은 not found
	get := func(p authz.Principal, sid string) (Service, bool) {
		t.Helper()
		sv, ok, err := store.GetService(ctx, p, sid, now)
		must(err)
		return sv, ok
	}
	if sv, ok := get(admin, id(1)); !ok || sv.Name != "Checkout" || sv.Namespace != "shop" || sv.Status != "active" || sv.Tags == nil {
		t.Errorf("get Checkout = %+v %v", sv, ok)
	}
	if sv, ok := get(admin, id(4)); !ok || sv.Status != "archived" {
		t.Errorf("get archived = %+v %v", sv, ok)
	}
	if _, ok := get(prodKey, id(2)); ok {
		t.Error("prod key saw a staging service")
	}
	if sv, ok := get(keyWithEnvs(t, b, nil), id(2)); ok {
		t.Errorf("tenant B saw tenant A service: %+v", sv)
	}
	if sv, ok := get(keyWithEnvs(t, b, nil), id(1)); !ok || sv.Name != "b-only" {
		t.Errorf("tenant B own service = %+v %v", sv, ok)
	}
	if _, ok := get(admin, id(99)); ok {
		t.Error("unknown id found")
	}
	// id → 이름(ADR 0043): 다른 tenant·허용 밖 environment·없는 id는 결과에 없다
	svcNames, err := store.ServiceNames(ctx, admin, []string{id(1), id(2), id(99)})
	if err != nil || fmt.Sprint(svcNames) != fmt.Sprintf("map[%s:Checkout %s:checkout]", id(1), id(2)) {
		t.Errorf("names = %v %v", svcNames, err)
	}
	if names, err := store.ServiceNames(ctx, prodKey, []string{id(1), id(2)}); err != nil || len(names) != 1 || names[id(1)] != "Checkout" {
		t.Errorf("names (prod key) = %v %v", names, err)
	}
	if names, err := store.ServiceNames(ctx, keyWithEnvs(t, b, nil), []string{id(2)}); err != nil || len(names) != 0 {
		t.Errorf("tenant B names = %v %v", names, err)
	}
}

// 앱 role은 사용자 관리 필드(owner_team 등)를 바꿀 수 없다 — agent 관측이 owner를 덮어쓰지 않는다(D02 §08).
func TestServiceOwnerFieldsNotWritableByApp(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	ctx := context.Background()
	if _, err := NewServiceStore(db).Observe(ctx, f.tenant, []ServiceObservation{
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
	// 처음 등록할 때도 사용자 관리 필드를 넣을 수 없다(column 단위 INSERT 권한)
	err = db.WithTenant(ctx, f.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO services (tenant_id, service_id, environment, namespace, name, first_seen, last_seen, owner_team)
			VALUES (app_tenant_id(), 'aaaaaaaa-0000-4000-8000-000000000008', 'prod', '', 'svc2', now(), now(), 'attacker')`)
		return err
	})
	if err == nil || !isPgCode(err, "42501") {
		t.Errorf("app role inserted owner_team: %v", err)
	}
}

// tenant 상한: 넘는 새 서비스는 등록하지 않고 세며, 이미 있는 서비스의 갱신은 계속된다.
// 한 호출 안의 같은 service_id는 합친다(ON CONFLICT 두 번 갱신 오류가 나지 않는다).
func TestServiceCatalogTenantLimit(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	store := NewServiceStore(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	sid := func(i int) string { return fmt.Sprintf("bbbbbbbb-0000-4000-8000-%012d", i) }
	obs := make([]ServiceObservation, 0, MaxServicesPerTenant+3)
	for i := 0; i < MaxServicesPerTenant+2; i++ {
		obs = append(obs, ServiceObservation{ServiceID: sid(i), Environment: "prod", Name: fmt.Sprintf("svc-%05d", i), SeenAt: now.Add(-time.Hour)})
	}
	obs = append(obs, obs[0]) // 중복
	rejected, err := store.Observe(ctx, f.tenant, obs)
	if err != nil || rejected != 2 {
		t.Fatalf("rejected=%d err=%v", rejected, err)
	}
	var count int
	if err := db.WithTenant(ctx, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM services`).Scan(&count)
	}); err != nil || count != MaxServicesPerTenant {
		t.Fatalf("count=%d err=%v", count, err)
	}
	// 상한에 닿아도 기존 서비스는 갱신되고, 새 서비스는 다시 거절된다
	rejected, err = store.Observe(ctx, f.tenant, []ServiceObservation{
		{ServiceID: sid(0), Environment: "prod", Name: "svc-00000", Language: "go", SeenAt: now},
		{ServiceID: sid(MaxServicesPerTenant + 5), Environment: "prod", Name: "new", SeenAt: now},
	})
	if err != nil || rejected != 1 {
		t.Fatalf("rejected=%d err=%v", rejected, err)
	}
	got, _, err := store.ListServices(ctx, keyWithEnvs(t, f, nil), ServiceQuery{Limit: 1, Now: now})
	if err != nil || len(got) != 1 || got[0].ServiceID != sid(0) || !got[0].LastSeen.Equal(now) || got[0].Language == nil || *got[0].Language != "go" {
		t.Errorf("updated = %+v %v", got, err)
	}
}
