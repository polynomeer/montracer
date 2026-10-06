//go:build integration

package controldb

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

const bgReason = "INC drill: leaked key suspected, revoke now"

func grantFor(t *testing.T, tenant authz.TenantID, actions ...authz.BreakGlassAction) authz.BreakGlassGrant {
	t.Helper()
	g, err := authz.NewBreakGlassGrant(tenant, "op-kim", "sec-lee", "INC-1042", bgReason, actions, time.Now(), 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

type auditRow struct {
	action, actorKind, actorID, resourceID, details string
}

func auditRows(t *testing.T, db *DB, tenant authz.TenantID, actorKind string) []auditRow {
	t.Helper()
	ctx := context.Background()
	var out []auditRow
	err := db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT action, actor_kind, actor_id, resource_id, details::text
			FROM audit_events WHERE actor_kind = $1 ORDER BY occurred_at, id`, actorKind)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r auditRow
			if err := rows.Scan(&r.action, &r.actorKind, &r.actorID, &r.resourceID, &r.details); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBreakGlassRevokeKey(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	ctx := context.Background()
	key := f.issue(t, authz.KindIngestKey, []authz.Action{authz.IngestTraces}, []string{"prod"})
	outboxBefore := count(t, db, &f.tenant, "outbox")

	out, err := f.store.BreakGlassRevokeKey(ctx, grantFor(t, f.tenant, authz.BreakGlassKeysRevoke), key.KeyID, "req-bg-1")
	if err != nil || out != RevokeApplied {
		t.Fatalf("revoke = %v, %v", out, err)
	}
	if _, err := f.authenticate(key.Token, authz.KindIngestKey); !errors.Is(err, authz.ErrUnauthenticated) {
		t.Errorf("revoked key still authenticates: %v", err)
	}
	rows := auditRows(t, db, f.tenant, "operator")
	if len(rows) != 1 || rows[0].action != "key.revoked" || rows[0].actorID != "op-kim" || rows[0].resourceID != key.KeyID ||
		!strings.Contains(rows[0].details, "INC-1042") || !strings.Contains(rows[0].details, "sec-lee") || !strings.Contains(rows[0].details, bgReason) {
		t.Fatalf("audit = %+v", rows)
	}
	// outbox에는 revocation event가 나가되 사유·승인자는 없다
	if n := count(t, db, &f.tenant, "outbox"); n != outboxBefore+1 {
		t.Errorf("outbox rows = %d, want %d", n, outboxBefore+1)
	}
	var payload, typ string
	err = db.WithTenant(ctx, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT type, payload::text FROM outbox WHERE resource_id = $1 ORDER BY occurred_at DESC LIMIT 1`, key.KeyID).Scan(&typ, &payload)
	})
	if err != nil {
		t.Fatal(err)
	}
	if typ != "key.revoked" || !strings.Contains(payload, "revoke_at") || !strings.Contains(payload, `"actor_kind": "operator"`) ||
		strings.Contains(payload, "INC-1042") || strings.Contains(payload, bgReason) || strings.Contains(payload, "sec-lee") {
		t.Errorf("outbox = %s %s", typ, payload)
	}

	// 다시 폐기: 변경 없음, 시도는 감사, outbox 추가 없음
	out, err = f.store.BreakGlassRevokeKey(ctx, grantFor(t, f.tenant, authz.BreakGlassKeysRevoke), key.KeyID, "")
	if err != nil || out != RevokeAlreadyRevoked {
		t.Fatalf("second revoke = %v, %v", out, err)
	}
	if n := count(t, db, &f.tenant, "outbox"); n != outboxBefore+1 {
		t.Errorf("no-op revoke wrote outbox (%d rows)", n)
	}
	if rows := auditRows(t, db, f.tenant, "operator"); len(rows) != 2 || rows[1].action != AuditBreakGlassKeyAlreadyRevoked {
		t.Errorf("audit after no-op = %+v", rows)
	}
}

// 다른 tenant의 key ID로 폐기를 시도하면 not found이고 그 key는 그대로다. 시도는 grant tenant에만 감사된다.
func TestBreakGlassRevokeIsTenantScoped(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	ctx := context.Background()
	keyB := b.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)

	_, err := a.store.BreakGlassRevokeKey(ctx, grantFor(t, a.tenant, authz.BreakGlassKeysRevoke), keyB.KeyID, "")
	if !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("cross-tenant revoke = %v, want ErrNotFound", err)
	}
	if _, err := b.authenticate(keyB.Token, authz.KindAPIKey); err != nil {
		t.Errorf("other tenant's key affected: %v", err)
	}
	// 다른 tenant의 key ID는 A의 감사에 남지 않는다(resource_id "unknown")
	if rows := auditRows(t, db, a.tenant, "operator"); len(rows) != 1 || rows[0].action != AuditBreakGlassKeyNotFound ||
		rows[0].resourceID != "unknown" || strings.Contains(rows[0].details, keyB.KeyID) {
		t.Errorf("tenant A audit = %+v", rows)
	}
	if rows := auditRows(t, db, b.tenant, "operator"); len(rows) != 0 {
		t.Errorf("tenant B got operator audit: %+v", rows)
	}
}

func TestBreakGlassListKeys(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	ctx := context.Background()
	keyA := a.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	b.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)

	keys, err := a.store.BreakGlassListKeys(ctx, grantFor(t, a.tenant, authz.BreakGlassKeysList), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].KeyID != keyA.KeyID {
		t.Fatalf("keys = %+v, want only tenant A's key", keys)
	}
	rows := auditRows(t, db, a.tenant, "operator")
	if len(rows) != 1 || rows[0].action != AuditBreakGlassKeysListed || !strings.Contains(rows[0].details, `"keys_returned": 1`) {
		t.Errorf("audit = %+v", rows)
	}
	if strings.Contains(rows[0].details, keyA.Token) {
		t.Error("token in audit")
	}
}

// grant 범위 밖 작업·만료된 grant는 DB에 닿지 않는다(감사 행도 없음).
func TestBreakGlassGrantEnforced(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	ctx := context.Background()
	key := f.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)

	if _, err := f.store.BreakGlassRevokeKey(ctx, grantFor(t, f.tenant, authz.BreakGlassKeysList), key.KeyID, ""); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("list-only grant revoke = %v, want ErrForbidden", err)
	}
	expired, err := authz.NewBreakGlassGrant(f.tenant, "op-kim", "sec-lee", "INC-1", bgReason,
		[]authz.BreakGlassAction{authz.BreakGlassKeysList}, time.Now().Add(-31*time.Minute), 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.BreakGlassListKeys(ctx, expired, ""); !errors.Is(err, authz.ErrBreakGlassExpired) {
		t.Errorf("expired grant list = %v, want ErrBreakGlassExpired", err)
	}
	if rows := auditRows(t, db, f.tenant, "operator"); len(rows) != 0 {
		t.Errorf("rejected grants wrote audit: %+v", rows)
	}
	if _, err := f.authenticate(key.Token, authz.KindAPIKey); err != nil {
		t.Errorf("key affected: %v", err)
	}
}

// 감사 쓰기가 실패하면 폐기도 적용되지 않는다(감사 없는 운영자 변경 금지).
func TestBreakGlassRevokeIsAtomic(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	key := f.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	_, err := f.store.BreakGlassRevokeKey(context.Background(), grantFor(t, f.tenant, authz.BreakGlassKeysRevoke), key.KeyID, strings.Repeat("x", 200))
	if err == nil {
		t.Fatal("want audit failure")
	}
	if _, err := f.authenticate(key.Token, authz.KindAPIKey); err != nil {
		t.Fatalf("revoke applied without audit: %v", err)
	}
}

// 형식 밖 key ID는 DB·감사에 닿지 않는다.
func TestBreakGlassRevokeRejectsMalformedKeyID(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	for _, id := range []string{"", "k_1", "0123456789ABCDEF", "0123456789abcdef0", "drop table api_keys"} {
		if _, err := f.store.BreakGlassRevokeKey(context.Background(), grantFor(t, f.tenant, authz.BreakGlassKeysRevoke), id, ""); err == nil {
			t.Errorf("key id %q accepted", id)
		}
	}
	if rows := auditRows(t, db, f.tenant, "operator"); len(rows) != 0 {
		t.Errorf("malformed key ids reached audit: %+v", rows)
	}
}
