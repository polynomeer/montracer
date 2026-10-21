//go:build integration

package controldb

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

// key 관리 API 저장소(ADR 0069, migration 00017): 이름·멱등 발급·상한(동시 포함)·조직 범위 폐기, 정지 조직의 key 인증 거절.
func TestKeyAPIStore(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	ctx := context.Background()
	create := func(f fixture, key, name string) (KeyMetadata, authz.GeneratedKey, bool, error) {
		t.Helper()
		iss, err := authz.ValidateKeyIssuance(f.admin, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
		if err != nil {
			t.Fatal(err)
		}
		gen, err := f.hasher.Generate(authz.KindAPIKey, nil)
		if err != nil {
			t.Fatal(err)
		}
		var m KeyMetadata
		_, replayed, err := f.store.CreateKeyIdempotent(ctx, f.admin, iss, gen, name, time.Now().Add(time.Hour),
			IdempotencyRequest{Key: key, MethodPath: "POST /api/v1/keys", RequestHash: sha256.Sum256([]byte(key))}, "req-"+key,
			func(km KeyMetadata) (StoredResponse, error) {
				m = km
				return StoredResponse{Status: 201, Body: []byte(`{}`)}, nil
			})
		return m, gen, replayed, err
	}

	m, gen, _, err := create(a, "k-1", "ci reader")
	if err != nil || m.Name == nil || *m.Name != "ci reader" || m.KeyID != gen.KeyID || m.Revision != 1 {
		t.Fatalf("create = %+v %v", m, err)
	}
	if _, _, replayed, err := create(a, "k-1", "ci reader"); err != nil || !replayed {
		t.Fatalf("replay = %v %v", replayed, err)
	}
	ks, err := a.store.ListKeys(ctx, a.admin)
	if err != nil || len(ks) != 1 || ks[0].Name == nil || *ks[0].Name != "ci reader" {
		t.Fatalf("list = %+v %v", ks, err)
	}
	if p, err := a.authenticate(gen.Token, authz.KindAPIKey); err != nil || p.Tenant() != a.tenant {
		t.Fatalf("authenticate new key: %v", err)
	}

	// 다른 조직의 principal로는 발급 요청을 저장하지 못하고(발급은 principal에 묶임), 폐기도 못 한다(404)
	iss, _ := authz.ValidateKeyIssuance(a.admin, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	if _, _, err := b.store.CreateKeyIdempotent(ctx, b.admin, iss, gen, "", time.Now().Add(time.Hour), IdempotencyRequest{Key: "x", MethodPath: "p"}, "", nil); err == nil {
		t.Fatal("issuance of tenant a stored by tenant b principal")
	}
	if err := b.store.RevokeKey(ctx, b.admin, gen.KeyID, time.Now(), ""); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("revoke other tenant's key: %v", err)
	}
	if _, err := a.authenticate(gen.Token, authz.KindAPIKey); err != nil {
		t.Fatalf("key revoked through other tenant: %v", err)
	}

	// 정지·폐쇄된 조직의 key(ingest·api)는 인증하지 않는다(D04 §02). 되살리면 다시 된다
	ingest := a.issue(t, authz.KindIngestKey, []authz.Action{authz.IngestTraces}, []string{"prod"})
	for _, status := range []string{"suspended", "closing", "deleted"} {
		if _, err := adminConn(t).Exec(ctx, `UPDATE tenants SET status = $2 WHERE id = $1`, a.tenant.String(), status); err != nil {
			t.Fatal(err)
		}
		if _, err := a.authenticate(gen.Token, authz.KindAPIKey); !errors.Is(err, authz.ErrUnauthenticated) {
			t.Fatalf("key of %s tenant: %v", status, err)
		}
		if _, err := a.authenticate(ingest.Token, authz.KindIngestKey); !errors.Is(err, authz.ErrUnauthenticated) {
			t.Fatalf("ingest key of %s tenant: %v", status, err)
		}
	}
	if _, err := adminConn(t).Exec(ctx, `UPDATE tenants SET status = 'restricted' WHERE id = $1`, a.tenant.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.authenticate(gen.Token, authz.KindAPIKey); err != nil {
		t.Fatalf("key of restricted tenant: %v", err)
	}
	if _, err := a.authenticate(ingest.Token, authz.KindIngestKey); err != nil {
		t.Fatalf("ingest key of restricted tenant: %v", err)
	}
	if _, err := adminConn(t).Exec(ctx, `UPDATE tenants SET status = 'active' WHERE id = $1`, a.tenant.String()); err != nil {
		t.Fatal(err)
	}

	// 살아 있는 key 상한: 동시에 만들어도 넘지 않는다(조직마다)
	room := MaxActiveKeys - activeKeys(t, db, a)
	results := make([]error, room+5)
	var wg sync.WaitGroup
	wg.Add(len(results))
	for i := range results {
		go func() {
			defer wg.Done()
			_, _, _, results[i] = create(a, fmt.Sprintf("fill-%d", i), "")
		}()
	}
	wg.Wait()
	ok, over := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrTooManyKeys):
			over++
		default:
			t.Fatalf("parallel create: %v", err)
		}
	}
	if ok != room || over != 5 || activeKeys(t, db, a) != MaxActiveKeys {
		t.Fatalf("parallel create ok=%d over=%d room=%d", ok, over, room)
	}
	if _, _, _, err := create(b, "b-1", ""); err != nil {
		t.Fatalf("limit counted across tenants: %v", err)
	}
	// 폐기하면 자리가 난다
	if err := a.store.RevokeKey(ctx, a.admin, gen.KeyID, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := create(a, "after-revoke", ""); err != nil {
		t.Fatalf("create after revoke: %v", err)
	}

	// 앱 role은 이름을 바꾸지 못한다(UPDATE 권한은 revoked_at·revision만)
	err = db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE api_keys SET name = 'x' WHERE tenant_id = $1`, a.tenant.String())
		return err
	})
	if err == nil {
		t.Fatal("app role renamed a key")
	}
}

func activeKeys(t *testing.T, db *DB, f fixture) int {
	t.Helper()
	var n int
	if err := db.WithTenant(context.Background(), f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM api_keys WHERE expires_at > now() AND (revoked_at IS NULL OR revoked_at > now())`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}
