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

func developer(t *testing.T, f fixture) authz.Principal {
	t.Helper()
	n := time.Now()
	p, err := authz.NewUserPrincipal(f.tenant, "dev", authz.RoleDeveloper, n, n)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func monitorWrite(threshold string) MonitorWrite {
	return MonitorWrite{Name: "Checkout 오류율", Spec: []byte(`{"name":"Checkout 오류율","condition":{"operator":"gt","threshold":` + threshold + `}}`), Enabled: true}
}

func idemReq(key, body string) IdempotencyRequest {
	return IdempotencyRequest{Key: key, MethodPath: "POST /api/v1/monitors", RequestHash: sha256.Sum256([]byte(body))}
}

func respondWith(m Monitor) (StoredResponse, error) {
	return StoredResponse{Status: 201, Body: []byte(fmt.Sprintf(`{"id":%q,"revision":%d}`, m.ID, m.Revision))}, nil
}

// monitor 정의·revision 이력·감사(operations)·outbox가 한 트랜잭션이고, 수정은 If-Match revision으로만, 삭제는 tombstone이다.
func TestMonitorLifecycle(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	store := NewMonitorStore(db)
	ctx := context.Background()
	dev := developer(t, f)

	resp, replayed, err := store.CreateMonitor(ctx, dev, monitorWrite("0.02"), idemReq("k1", "body-1"), "req_1", respondWith)
	if err != nil || replayed || resp.Status != 201 {
		t.Fatalf("create: %+v %v %v", resp, replayed, err)
	}
	ms, _, _, err := store.ListMonitors(ctx, dev, nil, 10)
	if err != nil || len(ms) != 1 || ms[0].Revision != 1 || ms[0].CreatedBy != "dev" {
		t.Fatalf("list: %+v %v", ms, err)
	}
	id := ms[0].ID

	// 낡은 revision은 412, 맞으면 revision 2
	if _, err := store.UpdateMonitor(ctx, dev, id, 9, monitorWrite("0.05"), "req_2"); !errors.Is(err, ErrRevisionMismatch) {
		t.Errorf("stale update: %v", err)
	}
	m, err := store.UpdateMonitor(ctx, dev, id, 1, monitorWrite("0.05"), "req_2")
	if err != nil || m.Revision != 2 {
		t.Fatalf("update: %+v %v", m, err)
	}
	// 삭제: tombstone, revision 3, 그 뒤 조회·수정은 없음과 같다
	if rev, err := store.DeleteMonitor(ctx, dev, id, nil, "req_3"); err != nil || rev != 3 {
		t.Fatalf("delete: %d %v", rev, err)
	}
	if _, err := store.GetMonitor(ctx, dev, id); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("get deleted: %v", err)
	}
	if _, err := store.UpdateMonitor(ctx, dev, id, 3, monitorWrite("0.1"), "req_4"); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("update deleted: %v", err)
	}

	// revision 이력 3개(threshold가 남는다), 감사는 operations 범주 3건, outbox 3건 — 같은 트랜잭션이라 수가 같다
	var (
		revisions, audits, events int
		lastDeleted               bool
		secondThreshold           string
	)
	err = db.WithTenant(ctx, f.tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*), bool_or(deleted) FILTER (WHERE revision = 3), max(spec->'condition'->>'threshold') FILTER (WHERE revision = 2)
			FROM monitor_revisions WHERE monitor_id = $1`, id).Scan(&revisions, &lastDeleted, &secondThreshold); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_id = $1 AND category = 'operations' AND resource_type = 'monitor'
			AND NOT (details ? 'spec')`, id).Scan(&audits); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE resource_id = $1 AND type LIKE 'monitor.%'`, id).Scan(&events)
	})
	if err != nil || revisions != 3 || !lastDeleted || secondThreshold != "0.05" || audits != 3 || events != 3 {
		t.Errorf("history: revisions=%d deleted=%v threshold=%s audits=%d events=%d err=%v", revisions, lastDeleted, secondThreshold, audits, events, err)
	}
}

// Idempotency-Key(D02 §20): 같은 본문은 처음 응답, 다른 본문은 충돌, 동시 요청도 하나만 만든다, 만료된 key는 새 요청이다.
func TestMonitorIdempotency(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	store := NewMonitorStore(db)
	ctx := context.Background()
	dev := developer(t, f)

	first, _, err := store.CreateMonitor(ctx, dev, monitorWrite("0.02"), idemReq("same", "body"), "r", respondWith)
	if err != nil {
		t.Fatal(err)
	}
	again, replayed, err := store.CreateMonitor(ctx, dev, monitorWrite("0.02"), idemReq("same", "body"), "r", respondWith)
	if err != nil || !replayed || string(again.Body) != string(first.Body) {
		t.Errorf("replay: %s %v %v", again.Body, replayed, err)
	}
	if _, _, err := store.CreateMonitor(ctx, dev, monitorWrite("0.03"), idemReq("same", "other body"), "r", respondWith); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("different body: %v", err)
	}
	if n := count(t, db, &f.tenant, "monitors"); n != 1 {
		t.Fatalf("monitors after replay = %d", n)
	}

	// 같은 key 동시 10개: 하나만 만들고 모두 같은 응답
	var wg sync.WaitGroup
	bodies := make([]string, 10)
	errs := make([]error, 10)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, _, err := store.CreateMonitor(ctx, dev, monitorWrite("0.02"), idemReq("race", "race-body"), "r", respondWith)
			bodies[i], errs[i] = string(r.Body), err
		}(i)
	}
	wg.Wait()
	for i := range bodies {
		if errs[i] != nil || bodies[i] != bodies[0] {
			t.Errorf("concurrent %d: %q %v (want %q)", i, bodies[i], errs[i], bodies[0])
		}
	}
	if n := count(t, db, &f.tenant, "monitors"); n != 2 {
		t.Errorf("monitors after race = %d, want 2", n)
	}

	// 같은 key라도 다른 principal이면 별개 요청이다
	admin := f.admin
	if _, replayed, err := store.CreateMonitor(ctx, admin, monitorWrite("0.02"), idemReq("same", "body"), "r", respondWith); err != nil || replayed {
		t.Errorf("other principal: replayed=%v %v", replayed, err)
	}
	// 만료된 key는 새 요청으로 본다
	if _, err := adminConn(t).Exec(ctx, `UPDATE idempotency_keys SET expires_at = now() - interval '1 second' WHERE key = 'same' AND principal = 'user:dev'`); err != nil {
		t.Fatal(err)
	}
	if _, replayed, err := store.CreateMonitor(ctx, dev, monitorWrite("0.07"), idemReq("same", "new body"), "r", respondWith); err != nil || replayed {
		t.Errorf("expired key reuse: replayed=%v %v", replayed, err)
	}
	if n := count(t, db, &f.tenant, "monitors"); n != 4 {
		t.Errorf("monitors = %d, want 4", n)
	}
}

// tenant 격리: 다른 tenant의 monitor·idempotency key는 보이지 않고 바꿀 수 없다. 앱 role은 monitor를 지우지 못한다(tombstone만).
func TestMonitorIsolationAndGrants(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	store := NewMonitorStore(db)
	ctx := context.Background()
	devA, devB := developer(t, a), developer(t, b)
	if _, _, err := store.CreateMonitor(ctx, devA, monitorWrite("0.02"), idemReq("k", "body"), "r", respondWith); err != nil {
		t.Fatal(err)
	}
	ms, _, _, err := store.ListMonitors(ctx, devA, nil, 10)
	if err != nil || len(ms) != 1 {
		t.Fatal(err)
	}
	id := ms[0].ID
	if got, _, _, err := store.ListMonitors(ctx, devB, nil, 10); err != nil || len(got) != 0 {
		t.Errorf("B sees A's monitors: %+v %v", got, err)
	}
	if _, err := store.GetMonitor(ctx, devB, id); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("B get: %v", err)
	}
	if _, err := store.UpdateMonitor(ctx, devB, id, 1, monitorWrite("0.9"), "r"); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("B update: %v", err)
	}
	if _, err := store.DeleteMonitor(ctx, devB, id, nil, "r"); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("B delete: %v", err)
	}
	// B가 같은 idempotency key를 써도 A의 응답이 재생되지 않는다
	if _, replayed, err := store.CreateMonitor(ctx, devB, monitorWrite("0.02"), idemReq("k", "body"), "r", respondWith); err != nil || replayed {
		t.Errorf("B same key: replayed=%v %v", replayed, err)
	}
	// 앱 role에는 DELETE 권한이 없다(정의는 tombstone으로만), revision 이력은 고칠 수 없다
	err = db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM monitors WHERE id = $1`, id)
		return err
	})
	if !isPgCode(err, "42501") {
		t.Errorf("app role DELETE monitors: %v", err)
	}
	err = db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE monitor_revisions SET spec = '{}' WHERE monitor_id = $1`, id)
		return err
	})
	if !isPgCode(err, "42501") {
		t.Errorf("app role UPDATE monitor_revisions: %v", err)
	}
	// tenant context 없이는 아무것도 안 보인다
	if n := count(t, db, nil, "monitors"); n != 0 {
		t.Errorf("no tenant context sees %d monitors", n)
	}
	// 권한 없는 principal(viewer)은 저장소에서도 거절
	n := time.Now()
	viewer, _ := authz.NewUserPrincipal(a.tenant, "viewer", authz.RoleViewer, n, n)
	if _, err := store.GetMonitor(ctx, viewer, id); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("viewer get: %v", err)
	}
}
