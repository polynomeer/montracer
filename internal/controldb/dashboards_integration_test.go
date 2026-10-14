//go:build integration

package controldb

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

func dashboardWrite(title string, widgets int) DashboardWrite {
	return DashboardWrite{Title: title, Spec: []byte(fmt.Sprintf(`{"version":1,"title":%q,"widgets":[]}`, title)), WidgetCount: widgets}
}

func dashIdem(key, body string) IdempotencyRequest {
	return IdempotencyRequest{Key: key, MethodPath: "POST /api/v1/dashboards", RequestHash: sha256.Sum256([]byte(body))}
}

func respondDashboard(d Dashboard) (StoredResponse, error) {
	return StoredResponse{Status: 201, Body: []byte(fmt.Sprintf(`{"id":%q,"revision":%d}`, d.ID, d.Revision))}, nil
}

// dashboard: 생성(Idempotency-Key 재생·충돌)·조회·제목순 목록·If-Match 수정·tombstone 삭제, operations 감사·outbox가 한 트랜잭션.
// Viewer는 읽기만, Security Auditor는 못 본다. 다른 tenant에는 없는 것과 같다. 앱 role은 행을 지우거나 만든 사람을 바꿀 수 없다.
func TestDashboardLifecycle(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	ctx := context.Background()
	s := NewDashboardStore(db)
	dev := developer(t, a)
	n := time.Now()
	viewer, _ := authz.NewUserPrincipal(a.tenant, "viewer", authz.RoleViewer, n, n)
	auditor, _ := authz.NewUserPrincipal(a.tenant, "aud", authz.RoleSecurityAuditor, n, n)

	if _, _, err := s.CreateDashboard(ctx, viewer, dashboardWrite("x", 0), dashIdem("v", "v"), "r", respondDashboard); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("viewer create: %v", err)
	}
	resp, replayed, err := s.CreateDashboard(ctx, dev, dashboardWrite("Checkout", 3), dashIdem("k1", "body1"), "r", respondDashboard)
	if err != nil || replayed || resp.Status != 201 {
		t.Fatalf("create: %+v %v %v", resp, replayed, err)
	}
	again, replayed, err := s.CreateDashboard(ctx, dev, dashboardWrite("Checkout", 3), dashIdem("k1", "body1"), "r", respondDashboard)
	if err != nil || !replayed || string(again.Body) != string(resp.Body) {
		t.Errorf("replay: %s %v %v", again.Body, replayed, err)
	}
	if _, _, err := s.CreateDashboard(ctx, dev, dashboardWrite("Other", 1), dashIdem("k1", "body2"), "r", respondDashboard); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("conflict: %v", err)
	}
	if _, _, err := s.CreateDashboard(ctx, dev, dashboardWrite("api 지연", 1), dashIdem("k2", "b"), "r", respondDashboard); err != nil {
		t.Fatal(err)
	}
	list, _, more, err := s.ListDashboards(ctx, viewer, nil, 10)
	if err != nil || more || len(list) != 2 || list[0].Title != "api 지연" || list[1].Title != "Checkout" || list[1].WidgetCount != 3 || list[1].Spec != nil {
		t.Fatalf("list: %+v %v", list, err)
	}
	if _, _, _, err := s.ListDashboards(ctx, auditor, nil, 10); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("auditor list: %v", err)
	}
	page, pos, more, err := s.ListDashboards(ctx, viewer, nil, 1)
	if err != nil || !more || len(page) != 1 {
		t.Fatalf("page 1: %v %v", more, err)
	}
	if rest, _, more, err := s.ListDashboards(ctx, viewer, &pos[0], 1); err != nil || more || len(rest) != 1 || rest[0].ID == page[0].ID {
		t.Errorf("page 2: %+v %v %v", rest, more, err)
	}
	id := list[1].ID
	got, err := s.GetDashboard(ctx, viewer, id)
	if err != nil || !strings.Contains(string(got.Spec), `"Checkout"`) || got.Revision != 1 {
		t.Fatalf("get: %+v %v", got, err)
	}
	// 수정: revision이 다르면 거절(마지막 쓰기가 덮어쓰지 않는다), 맞으면 revision + 1
	if _, err := s.UpdateDashboard(ctx, dev, id, 5, dashboardWrite("Checkout v2", 4), "r"); !errors.Is(err, ErrRevisionMismatch) {
		t.Errorf("stale update: %v", err)
	}
	upd, err := s.UpdateDashboard(ctx, dev, id, 1, dashboardWrite("Checkout v2", 4), "r")
	if err != nil || upd.Revision != 2 || upd.Title != "Checkout v2" || upd.WidgetCount != 4 {
		t.Fatalf("update: %+v %v", upd, err)
	}
	if _, err := s.UpdateDashboard(ctx, viewer, id, 2, dashboardWrite("x", 0), "r"); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("viewer update: %v", err)
	}
	// 다른 tenant: 없는 것과 같다
	bdev := developer(t, b)
	if _, err := s.GetDashboard(ctx, bdev, id); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("other tenant get: %v", err)
	}
	if _, err := s.UpdateDashboard(ctx, bdev, id, 2, dashboardWrite("hijack", 0), "r"); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("other tenant update: %v", err)
	}
	if _, err := s.DeleteDashboard(ctx, bdev, id, nil, "r"); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("other tenant delete: %v", err)
	}
	if l, _, _, _ := s.ListDashboards(ctx, bdev, nil, 10); len(l) != 0 {
		t.Errorf("other tenant list: %+v", l)
	}
	// 삭제: tombstone, 이후 없음·목록에서 빠짐
	if _, err := s.DeleteDashboard(ctx, dev, id, new(int64(1)), "r"); !errors.Is(err, ErrRevisionMismatch) {
		t.Errorf("stale delete: %v", err)
	}
	if rev, err := s.DeleteDashboard(ctx, dev, id, new(int64(2)), "r"); err != nil || rev != 3 {
		t.Fatalf("delete: %d %v", rev, err)
	}
	if _, err := s.GetDashboard(ctx, dev, id); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	// 감사(operations)·outbox: 생성 2·수정 1·삭제 1. 감사에는 spec 본문이 없다(hash만)
	var audits, outbox int
	var leaked bool
	if err := db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*), bool_or(details::text LIKE '%Checkout%') FROM audit_events WHERE category = 'operations' AND resource_type = 'dashboard'`).Scan(&audits, &leaked); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE type LIKE 'dashboard.%'`).Scan(&outbox)
	}); err != nil || audits != 4 || outbox != 4 || leaked {
		t.Errorf("audit %d outbox %d leaked %v (%v)", audits, outbox, leaked, err)
	}
	// 앱 role 최소 권한: 행 삭제·만든 사람·tenant 변경 불가
	for _, q := range []string{`DELETE FROM dashboards`, `UPDATE dashboards SET created_by = 'x'`, `UPDATE dashboards SET tenant_id = tenant_id`} {
		err := db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		if !isPgCode(err, "42501") {
			t.Errorf("%s: %v", q, err)
		}
	}
}
