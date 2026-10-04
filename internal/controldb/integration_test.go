//go:build integration

package controldb

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/polynomeer/montracer/internal/authz"
)

// 통합 테스트는 migration이 적용된 PostgreSQL이 필요하다 (make up && make migrate).
//
//	MONTRACER_TEST_PG_APP_DSN   앱 role(montracer_rw 멤버) — 테스트 대상
//
// 매 테스트는 새 무작위 tenant를 만들어 기존 데이터와 섞이지 않게 한다.

func appDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MONTRACER_TEST_PG_APP_DSN")
	if dsn == "" {
		t.Fatal("MONTRACER_TEST_PG_APP_DSN 필요 (make test-integration)")
	}
	return dsn
}

func openDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), appDSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

var testPepper = []byte("integration-test-pepper-32-bytes!!")

type fixture struct {
	db     *DB
	store  *KeyStore
	hasher authz.KeyHasher
	tenant authz.TenantID
	admin  authz.Principal
}

// newTenant는 tenant와 tenant_admin(admin), developer(dev) membership을 만든다.
func newTenant(t *testing.T, db *DB) fixture {
	t.Helper()
	ctx := context.Background()
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := authz.ParseTenantID(id)
	if err != nil {
		t.Fatal(err)
	}
	err = db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, region, cell, status) VALUES ($1, 'local', 'cell-0', 'active')`, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, 'admin', 'tenant_admin'), ($1, 'dev', 'developer')`, id)
		return err
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	h, err := authz.NewKeyHasher(testPepper)
	if err != nil {
		t.Fatal(err)
	}
	n := time.Now()
	admin, err := authz.NewUserPrincipal(tenant, "admin", authz.RoleTenantAdmin, n, n)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{db: db, store: NewKeyStore(db), hasher: h, tenant: tenant, admin: admin}
}

func (f fixture) issue(t *testing.T, kind authz.Kind, scopes []authz.Action, envs []string) authz.GeneratedKey {
	t.Helper()
	iss, err := authz.ValidateKeyIssuance(f.admin, kind, scopes, envs)
	if err != nil {
		t.Fatalf("ValidateKeyIssuance: %v", err)
	}
	gen, err := f.hasher.Generate(kind, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CreateKey(context.Background(), iss, gen, time.Now().Add(time.Hour), "req_test"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	return gen
}

func (f fixture) authenticate(token string, kind authz.Kind) (authz.Principal, error) {
	return f.hasher.Authenticate(context.Background(), token, kind, f.store.LookupKey, time.Now())
}

func count(t *testing.T, db *DB, tenant *authz.TenantID, table string) int {
	t.Helper()
	ctx := context.Background()
	var n int
	q := `SELECT count(*) FROM ` + table // table은 테스트 상수
	if tenant == nil {
		conn, err := db.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if err := conn.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	if err := db.WithTenant(ctx, *tenant, func(tx pgx.Tx) error { return tx.QueryRow(ctx, q).Scan(&n) }); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestKeyLifecycleEndToEnd(t *testing.T) {
	f := newTenant(t, openDB(t))
	ingest := f.issue(t, authz.KindIngestKey, []authz.Action{authz.IngestTraces}, []string{"production"})

	p, err := f.authenticate(ingest.Token, authz.KindIngestKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if p.Tenant() != f.tenant || !p.AllowsEnvironment("production") || authz.Authorize(p, authz.IngestTraces) != nil {
		t.Fatalf("principal = %+v", p)
	}

	// 감사·outbox가 같은 트랜잭션으로 기록되었다.
	if a, o := count(t, f.db, &f.tenant, "audit_events"), count(t, f.db, &f.tenant, "outbox"); a != 1 || o != 1 {
		t.Fatalf("audit=%d outbox=%d, want 1/1", a, o)
	}

	keys, err := f.store.ListKeys(context.Background(), f.admin)
	if err != nil || len(keys) != 1 || keys[0].KeyID != ingest.KeyID || keys[0].Kind != authz.KindIngestKey {
		t.Fatalf("ListKeys = %+v, %v", keys, err)
	}

	if err := f.store.RevokeKey(context.Background(), f.admin, ingest.KeyID, time.Now(), "req_revoke"); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := f.authenticate(ingest.Token, authz.KindIngestKey); !errors.Is(err, authz.ErrUnauthenticated) {
		t.Fatalf("revoked key: %v", err)
	}
	// 재폐기는 멱등이며 감사를 추가하지 않는다.
	if err := f.store.RevokeKey(context.Background(), f.admin, ingest.KeyID, time.Now(), "req_revoke2"); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if a := count(t, f.db, &f.tenant, "audit_events"); a != 2 {
		t.Fatalf("audit=%d, want 2", a)
	}
}

func TestRotationOverlap(t *testing.T) {
	f := newTenant(t, openDB(t))
	old := f.issue(t, authz.KindIngestKey, []authz.Action{authz.IngestLogs}, []string{"production"})
	if err := f.store.RevokeKey(context.Background(), f.admin, old.KeyID, time.Now().Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.authenticate(old.Token, authz.KindIngestKey); err != nil {
		t.Fatalf("key must stay valid until scheduled revoke: %v", err)
	}
	if err := f.store.RevokeKey(context.Background(), f.admin, old.KeyID, time.Now().Add(25*time.Hour), ""); err == nil {
		t.Fatal("revoke beyond 24h overlap must fail")
	}
}

func TestTenantIsolation(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	keyA := a.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)

	// B는 A의 key를 볼 수도, 폐기할 수도 없다. 존재도 드러나지 않는다(ErrNotFound).
	if n := count(t, db, &b.tenant, "api_keys"); n != 0 {
		t.Fatalf("tenant B sees %d keys", n)
	}
	if err := b.store.RevokeKey(context.Background(), b.admin, keyA.KeyID, time.Now(), ""); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("cross-tenant revoke: %v, want ErrNotFound", err)
	}
	if _, err := a.authenticate(keyA.Token, authz.KindAPIKey); err != nil {
		t.Fatalf("A key must still work: %v", err)
	}
	// B tenant context로 A tenant_id를 넣는 쓰기는 WITH CHECK가 막는다.
	err := db.WithTenant(context.Background(), b.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, 'intruder', 'tenant_admin')`, a.tenant.String())
		return err
	})
	if !isPgCode(err, "42501") {
		t.Fatalf("cross-tenant insert: %v, want RLS violation 42501", err)
	}
}

func TestNoTenantContextSeesNothing(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	f.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	for _, table := range []string{"tenants", "memberships", "api_keys", "audit_events", "outbox"} {
		if n := count(t, db, nil, table); n != 0 {
			t.Errorf("%s without tenant context: %d rows, want 0", table, n)
		}
	}
}

func TestKeyLookupPolicyIsNarrow(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	k1 := f.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	f.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	ctx := context.Background()
	err := db.inTx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.key_lookup', $1, true)`, k1.KeyID); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM api_keys`).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("key_lookup exposes %d rows, want exactly 1", n)
		}
		// lookup 정책은 SELECT 전용: 같은 행을 수정할 수 없다.
		tag, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE key_id = $1`, k1.KeyID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("key_lookup allowed UPDATE of %d rows", tag.RowsAffected())
		}
		// tenant 관련 다른 테이블은 여전히 닫혀 있다.
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM memberships`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("key_lookup exposes %d memberships", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAppRoleCannotBypassRLS(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	var super, bypass bool
	if err := db.pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("app role must not be superuser/bypassrls (super=%v bypass=%v)", super, bypass)
	}
	var owner bool
	if err := db.pool.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) = current_user FROM pg_class WHERE relname = 'api_keys'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner {
		t.Fatal("app role must not own tables")
	}
	// row_security=off는 RLS 대상 쿼리를 오류로 만든다 (우회 불가).
	err := db.inTx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL row_security = off`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT count(*) FROM api_keys`)
		return err
	})
	if !isPgCode(err, "42501") {
		t.Fatalf("row_security=off: %v, want 42501", err)
	}
}

func TestAuditIsAppendOnly(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	f.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	for _, stmt := range []string{`UPDATE audit_events SET action = 'tampered'`, `DELETE FROM audit_events`, `DELETE FROM outbox`} {
		err := db.WithTenant(context.Background(), f.tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), stmt)
			return err
		})
		if !isPgCode(err, "42501") {
			t.Errorf("%s: %v, want permission denied", stmt, err)
		}
	}
}

func TestCreateKeyIsAtomic(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	iss, err := authz.ValidateKeyIssuance(f.admin, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gen, _ := f.hasher.Generate(authz.KindAPIKey, nil)
	// 감사 insert가 request_id 길이 제약으로 실패하면 key도 남지 않아야 한다.
	err = f.store.CreateKey(context.Background(), iss, gen, time.Now().Add(time.Hour), strings.Repeat("x", 200))
	if err == nil {
		t.Fatal("want audit failure")
	}
	if n := count(t, db, &f.tenant, "api_keys"); n != 0 {
		t.Fatalf("key persisted without audit: %d", n)
	}
	if _, err := f.authenticate(gen.Token, authz.KindAPIKey); !errors.Is(err, authz.ErrUnauthenticated) {
		t.Fatalf("partial key authenticates: %v", err)
	}
}

func TestIssuerRoleIsCurrent(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	key := f.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead, authz.DashboardsWrite}, nil)
	ingest := f.issue(t, authz.KindIngestKey, []authz.Action{authz.IngestTraces}, []string{"production"})
	ctx := context.Background()
	exec := func(q string) {
		t.Helper()
		if err := db.WithTenant(ctx, f.tenant, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err }); err != nil {
			t.Fatal(err)
		}
	}

	exec(`UPDATE memberships SET role = 'viewer' WHERE user_id = 'admin'`)
	p, err := f.authenticate(key.Token, authz.KindAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	if authz.Authorize(p, authz.TelemetryRead) != nil || !errors.Is(authz.Authorize(p, authz.DashboardsWrite), authz.ErrForbidden) {
		t.Fatal("demoted issuer: key must lose dashboards.write")
	}

	exec(`DELETE FROM memberships WHERE user_id = 'admin'`)
	if _, err := f.authenticate(key.Token, authz.KindAPIKey); !errors.Is(err, authz.ErrUnauthenticated) {
		t.Fatalf("removed issuer: %v, want ErrUnauthenticated", err)
	}
	if _, err := f.authenticate(ingest.Token, authz.KindIngestKey); err != nil {
		t.Fatalf("ingest key must survive issuer removal: %v", err)
	}
}

func TestUnknownKeyAndClosedPool(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	if _, err := f.store.LookupKey(context.Background(), "ffffffffffffffff"); !errors.Is(err, authz.ErrKeyNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
	gen := f.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	db.Close()
	_, err := f.authenticate(gen.Token, authz.KindAPIKey)
	if !errors.Is(err, authz.ErrBackendUnavailable) || errors.Is(err, authz.ErrUnauthenticated) {
		t.Fatalf("closed pool: %v, want ErrBackendUnavailable", err)
	}
}

func isPgCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
