//go:build integration

package controldb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/notify"
)

func testSealer(t *testing.T) *notify.Sealer {
	t.Helper()
	s, err := notify.NewSealer("v1", bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func policyWrite(t *testing.T, f fixture, sealer *notify.Sealer, name, rawURL string) PolicyWrite {
	t.Helper()
	return PolicyWrite{Name: name, Display: notify.RedactURL(rawURL), Enabled: true, SealURL: func(id string) (Sealed, error) {
		c, err := sealer.SealURL(f.tenant.String(), id, rawURL)
		return Sealed{Ciphertext: c, Version: sealer.Version()}, err
	}}
}

func createPolicy(t *testing.T, db *DB, f fixture, sealer *notify.Sealer, key string) (NotificationPolicy, notify.Secret) {
	t.Helper()
	sec, err := notify.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	var np NotificationPolicy
	_, _, err = NewNotificationPolicyStore(db).CreatePolicy(context.Background(), f.admin,
		policyWrite(t, f, sealer, "On-call "+key, "https://hooks.example.com/"+key+"?token=t0k3n"),
		func(id string) (SealedSecret, error) {
			sealed, err := sealer.Seal(f.tenant.String(), id, sec)
			return SealedSecret{KeyID: sec.KeyID, Ciphertext: sealed, SealingKey: sealer.Version()}, err
		},
		IdempotencyRequest{Key: key, MethodPath: "POST /api/v1/notification-policies", RequestHash: sha256.Sum256([]byte(key))}, "req",
		func(p NotificationPolicy) (StoredResponse, error) {
			np = p
			return StoredResponse{Status: 201, Body: []byte(`{}`)}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return np, sec
}

// policy: 생성·수정(If-Match)·tombstone, security 감사·outbox(URL은 host만), 저장된 암호문은 그 tenant·policy로만 풀린다.
// 앱 role은 secret 열을 바꾸거나 policy를 지울 수 없다. 다른 tenant에서는 보이지 않는다.
func TestNotificationPolicyLifecycle(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	sealer := testSealer(t)
	store := NewNotificationPolicyStore(db)
	ctx := context.Background()
	np, sec := createPolicy(t, db, a, sealer, "p1")
	if np.Revision != 1 || !np.Enabled || np.SecretKeyID != sec.KeyID {
		t.Fatalf("created = %+v", np)
	}
	// 같은 key 재생은 새로 만들지 않는다
	if _, replayed, err := store.CreatePolicy(ctx, a.admin, policyWrite(t, a, sealer, "x", "https://hooks.example.com/p1"),
		func(string) (SealedSecret, error) { t.Fatal("sealed on replay"); return SealedSecret{}, nil },
		IdempotencyRequest{Key: "p1", MethodPath: "POST /api/v1/notification-policies", RequestHash: sha256.Sum256([]byte("p1"))}, "r",
		func(NotificationPolicy) (StoredResponse, error) { return StoredResponse{}, nil }); err != nil || !replayed {
		t.Errorf("replay: %v %v", replayed, err)
	}
	var sealed, sealedURL []byte
	var version, urlVersion string
	if err := db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT secret_ciphertext, secret_sealing_key, webhook_url_sealed, url_sealing_key FROM notification_policies WHERE id = $1`,
			np.ID).Scan(&sealed, &version, &sealedURL, &urlVersion)
	}); err != nil {
		t.Fatal(err)
	}
	// URL 전체는 암호문으로만 있다(token이 평문으로 남지 않는다). 화면 표시는 host만
	if u, err := sealer.OpenURL(a.tenant.String(), np.ID, urlVersion, sealedURL); err != nil || u != "https://hooks.example.com/p1?token=t0k3n" ||
		np.WebhookURL != "https://hooks.example.com/…" || bytes.Contains(sealedURL, []byte("t0k3n")) {
		t.Errorf("url: %q %v display=%s", u, err, np.WebhookURL)
	}
	if got, err := sealer.Open(a.tenant.String(), np.ID, sec.KeyID, version, sealed); err != nil || !bytes.Equal(got.Key, sec.Key) {
		t.Errorf("open stored secret: %v", err)
	}
	if _, err := sealer.Open(b.tenant.String(), np.ID, sec.KeyID, version, sealed); err == nil {
		t.Error("secret opened under another tenant")
	}

	if _, err := store.UpdatePolicy(ctx, a.admin, np.ID, 9, policyWrite(t, a, sealer, "x", "https://other.example.com/y"), "r"); !errors.Is(err, ErrRevisionMismatch) {
		t.Errorf("stale update: %v", err)
	}
	off := policyWrite(t, a, sealer, "Renamed", "https://other.example.com/y?token=secret")
	off.Enabled = false
	up, err := store.UpdatePolicy(ctx, a.admin, np.ID, 1, off, "r")
	if err != nil || up.Revision != 2 || up.Enabled || up.SecretKeyID != sec.KeyID {
		t.Fatalf("update: %+v %v", up, err)
	}
	// developer는 고를 수 있지만(조회) 바꿀 수 없다
	dev := developer(t, a)
	if _, err := store.GetPolicy(ctx, dev, np.ID); err != nil {
		t.Errorf("developer get: %v", err)
	}
	if _, err := store.UpdatePolicy(ctx, dev, np.ID, 2, policyWrite(t, a, sealer, "x", "https://x.example.com"), "r"); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("developer update: %v", err)
	}
	// 다른 tenant: 보이지 않고 바꿀 수 없다
	if _, err := store.GetPolicy(ctx, b.admin, np.ID); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("B get: %v", err)
	}
	if _, err := store.DeletePolicy(ctx, b.admin, np.ID, nil, "r"); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("B delete: %v", err)
	}
	if ps, _, _, err := store.ListPolicies(ctx, b.admin, nil, 10); err != nil || len(ps) != 0 {
		t.Errorf("B list: %+v %v", ps, err)
	}
	if rev, err := store.DeletePolicy(ctx, a.admin, np.ID, nil, "r"); err != nil || rev != 3 {
		t.Fatalf("delete: %d %v", rev, err)
	}
	if _, err := store.GetPolicy(ctx, a.admin, np.ID); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("get deleted: %v", err)
	}
	// 감사: security 범주 3건, URL은 host만(token 없음)
	var n int
	var leaked bool
	if err := db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*), bool_or(details::text LIKE '%token%' OR details::text LIKE '%/y%' OR details::text LIKE '%t0k3n%') FROM audit_events
			WHERE resource_type = 'notification_policy' AND category = 'security'`).Scan(&n, &leaked)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 3 || leaked {
		t.Errorf("audit rows %d, leaked URL %v", n, leaked)
	}
	// 앱 role: secret 열 수정·policy 삭제 불가
	for _, q := range []string{`UPDATE notification_policies SET secret_ciphertext = '\x00'`, `UPDATE notification_policies SET tenant_id = tenant_id`,
		`DELETE FROM notification_policies`, `DELETE FROM notification_delivery_bodies`, `UPDATE notification_delivery_bodies SET body = '\x00'`} {
		err := db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		if !isPgCode(err, "42501") {
			t.Errorf("%s: %v", q, err)
		}
	}
}

func deliveries(t *testing.T, db *DB, tenant authz.TenantID) []map[string]any {
	t.Helper()
	ctx := context.Background()
	var out []map[string]any
	if err := db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT d.id::text, d.policy_id::text, d.event_type, b.body, d.status
			FROM notification_deliveries d JOIN notification_delivery_bodies b ON b.delivery_id = d.id ORDER BY d.created_at, d.event_type DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, policy, typ, status string
			var body []byte
			if err := rows.Scan(&id, &policy, &typ, &body, &status); err != nil {
				return err
			}
			var env map[string]any
			if err := json.Unmarshal(body, &env); err != nil {
				return err
			}
			out = append(out, map[string]any{"id": id, "policy": policy, "type": typ, "env": env, "status": status})
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func openedGroup(episode string) GroupWrite {
	g := alertGroup("ALERT", &episode)
	v := 0.05
	g.Instance.LastValue = &v
	g.Changed, g.From, g.To, g.Opened, g.EpisodeOut = true, "OK", "ALERT", true, &episode
	return g
}

// 사건 열림·닫힘은 경보 전이와 같은 트랜잭션에 알림으로 쌓인다(같은 사건은 한 번). 닫힘은 열림을 받은 policy에만 간다.
// 정의 변경으로 닫힌 사건도 알린다(close_reason). 지운 policy·없는 policy를 가리키면 쌓지 않거나 저장을 막는다.
func TestNotificationsEnqueue(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	sealer := testSealer(t)
	np, _ := createPolicy(t, db, f, sealer, "q1")
	ctx := context.Background()
	monitors := NewMonitorStore(db)
	dev := developer(t, f)

	missing := "00000000-0000-4000-8000-000000000000"
	w := monitorWrite("0.02")
	w.NotificationPolicyID = &missing
	if _, _, err := monitors.CreateMonitor(ctx, dev, w, idemReq("m0", "b0"), "r", respondWith); !errors.Is(err, ErrNotificationPolicyNotFound) {
		t.Fatalf("missing policy: %v", err)
	}
	w.NotificationPolicyID = &np.ID
	if _, _, err := monitors.CreateMonitor(ctx, dev, w, idemReq("m1", "b1"), "r", respondWith); err != nil {
		t.Fatal(err)
	}
	ms, _, _, _ := monitors.ListMonitors(ctx, dev, nil, 10)
	id := ms[0].ID
	// monitor가 가리키는 policy는 지울 수 없다(tombstone도 남지 않는다)
	if _, err := NewNotificationPolicyStore(db).DeletePolicy(ctx, f.admin, np.ID, nil, "r"); !errors.Is(err, ErrPolicyInUse) {
		t.Fatalf("delete policy in use: %v", err)
	}
	if _, err := NewNotificationPolicyStore(db).GetPolicy(ctx, f.admin, np.ID); err != nil {
		t.Fatalf("policy gone after refused delete: %v", err)
	}
	s := NewAlertStore(db)
	slot := time.Now().UTC().Truncate(time.Minute).Add(5 * time.Hour)
	episode := "0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f10"

	c, ok, err := s.ClaimEvaluation(ctx, f.tenant, id, slot, time.Minute)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", PolicyID: &np.ID, MonitorName: "Checkout 오류율", Groups: []GroupWrite{openedGroup(episode)}}); err != nil {
		t.Fatal(err)
	}
	ds := deliveries(t, db, f.tenant)
	if len(ds) != 1 || ds[0]["type"] != notify.EventOpened || ds[0]["policy"] != np.ID || ds[0]["status"] != "pending" {
		t.Fatalf("deliveries = %+v", ds)
	}
	env := ds[0]["env"].(map[string]any)
	payload := env["payload"].(map[string]any)
	if env["event_id"] != ds[0]["id"] || env["tenant_public_id"] != f.tenant.String() || env["resource_id"] != id || payload["episode_id"] != episode ||
		payload["state"] != "ALERT" || payload["value"] != 0.05 || payload["monitor_name"] != "Checkout 오류율" || payload["labels"].(map[string]any)["service.name"] != "checkout" {
		t.Errorf("envelope = %+v", env)
	}
	// 같은 사건의 열림을 다시 쓰면(재평가·중복 scheduler) 알림은 하나다
	if err := db.WithTenant(ctx, f.tenant, func(tx pgx.Tx) error {
		return enqueueOpened(ctx, tx, f.tenant, np.ID, notice{monitorID: id, groupHash: GroupHash("service.name=checkout"), episodeID: episode, at: slot})
	}); err != nil {
		t.Fatal(err)
	}
	if ds := deliveries(t, db, f.tenant); len(ds) != 1 {
		t.Errorf("duplicate opened: %d", len(ds))
	}
	// 복구로 닫힘 → resolved(close_reason recovered)
	c, _, _ = s.ClaimEvaluation(ctx, f.tenant, id, slot.Add(time.Minute), time.Minute)
	closed := alertGroup("OK", nil)
	closed.Changed, closed.From, closed.To, closed.Closed, closed.EpisodeOut = true, "RECOVERING", "OK", true, &episode
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", PolicyID: &np.ID, Groups: []GroupWrite{closed}}); err != nil {
		t.Fatal(err)
	}
	ds = deliveries(t, db, f.tenant)
	if len(ds) != 2 || ds[1]["type"] != notify.EventResolved || ds[1]["env"].(map[string]any)["payload"].(map[string]any)["close_reason"] != "recovered" {
		t.Fatalf("resolved = %+v", ds)
	}
	// 새 사건이 열린 뒤 정의가 바뀌면 닫힘 알림(monitor_revised, 상태 없음)
	episode2 := "0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f11"
	c, _, _ = s.ClaimEvaluation(ctx, f.tenant, id, slot.Add(2*time.Minute), time.Minute)
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", PolicyID: &np.ID, Groups: []GroupWrite{openedGroup(episode2)}}); err != nil {
		t.Fatal(err)
	}
	w.NotificationPolicyID = nil // 정의에서 policy를 뗐다: 열린 사건의 닫힘은 열림을 받은 곳으로 간다
	if _, err := monitors.UpdateMonitor(ctx, dev, id, 1, w, "r"); err != nil {
		t.Fatal(err)
	}
	ds = deliveries(t, db, f.tenant)
	last := ds[len(ds)-1]
	lp := last["env"].(map[string]any)["payload"].(map[string]any)
	if len(ds) != 4 || last["type"] != notify.EventResolved || lp["close_reason"] != "monitor_revised" || lp["state"] != nil || lp["episode_id"] != episode2 {
		t.Fatalf("revised = %+v", ds)
	}
	// 이제 이 policy를 가리키는 monitor가 없으므로 지울 수 있다. 낡은 revision 평가가 지워진 policy로 사건을 열면
	// 조용히 버리지 않고 취소된 알림(policy_deleted)으로 남는다
	if _, err := NewNotificationPolicyStore(db).DeletePolicy(ctx, f.admin, np.ID, nil, "r"); err != nil {
		t.Fatal(err)
	}
	c, _, _ = s.ClaimEvaluation(ctx, f.tenant, id, slot.Add(3*time.Minute), time.Minute)
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", PolicyID: &np.ID, Groups: []GroupWrite{openedGroup("0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f12")}}); err != nil {
		t.Fatal(err)
	}
	if ds := deliveries(t, db, f.tenant); len(ds) != 5 || ds[4]["status"] != "canceled" {
		t.Errorf("deleted policy: %+v", ds)
	}
}

// 보낼 알림 찾기는 tenant를 가로질러 delivery만(SELECT) 본다. lease는 하나의 worker만, token이 다르면 결과를 쓰지 않는다.
// 재시도 시각 전에는 다시 잡히지 않는다. 끝난 기록은 30일 뒤 지우되 열린 사건의 열림 기록은 남긴다.
func TestDeliveryQueue(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	sealer := testSealer(t)
	np, _ := createPolicy(t, db, a, sealer, "d1")
	ctx := context.Background()
	monitors := NewMonitorStore(db)
	w := monitorWrite("0.02")
	w.NotificationPolicyID = &np.ID
	if _, _, err := monitors.CreateMonitor(ctx, developer(t, a), w, idemReq("m", "b"), "r", respondWith); err != nil {
		t.Fatal(err)
	}
	ms, _, _, _ := monitors.ListMonitors(ctx, developer(t, a), nil, 10)
	s := NewAlertStore(db)
	slot := time.Now().UTC().Truncate(time.Minute).Add(6 * time.Hour)
	c, _, _ := s.ClaimEvaluation(ctx, a.tenant, ms[0].ID, slot, time.Minute)
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", PolicyID: &np.ID, Groups: []GroupWrite{openedGroup("0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f20")}}); err != nil {
		t.Fatal(err)
	}
	q := NewDeliveryStore(db)
	now := time.Now().Add(time.Second)
	findMine := func(at time.Time) (DueDelivery, bool) {
		due, _, err := q.DueDeliveries(ctx, at, 1000, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range due {
			if d.Tenant == a.tenant {
				return d, true
			}
		}
		return DueDelivery{}, false
	}
	due, ok := findMine(now)
	if !ok {
		t.Fatal("delivery not due")
	}
	d, ok, err := q.ClaimDelivery(ctx, a.tenant, due.ID, now, time.Minute)
	u, uerr := sealer.OpenURL(a.tenant.String(), np.ID, d.URLSealingKey, d.URLSealed)
	if err != nil || !ok || d.PolicyID != np.ID || uerr != nil || u != "https://hooks.example.com/d1?token=t0k3n" || d.PolicyDisabled || len(d.Sealed) == 0 ||
		d.Attempts != 1 || len(d.Body) == 0 || due.PolicyID != np.ID {
		t.Fatalf("claim: %+v %v %v", d, ok, err)
	}
	if _, ok, _ := q.ClaimDelivery(ctx, a.tenant, due.ID, now, time.Minute); ok {
		t.Error("second claim succeeded")
	}
	if _, ok := findMine(now); ok {
		t.Error("leased delivery still due")
	}
	other := d
	other.Token = "0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9fff"
	if err := q.CompleteDelivery(ctx, other, DeliveryResult{Status: "delivered"}); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("complete with another token: %v", err)
	}
	// 다른 tenant 이름으로는 잡을 수도 쓸 수도 없다
	if _, ok, _ := q.ClaimDelivery(ctx, b.tenant, due.ID, now, time.Minute); ok {
		t.Error("B claimed A's delivery")
	}
	forged := d
	forged.Tenant = b.tenant
	if err := q.CompleteDelivery(ctx, forged, DeliveryResult{Status: "delivered"}); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("B completed A's delivery: %v", err)
	}
	// 재시도: 다음 시도 시각 전에는 다시 잡히지 않는다
	next := now.Add(30 * time.Second)
	if err := q.CompleteDelivery(ctx, d, DeliveryResult{Status: "pending", NextAttemptAt: next, StatusCode: 503, Error: notify.ErrCodeStatus}); err != nil {
		t.Fatal(err)
	}
	if _, ok := findMine(now.Add(10 * time.Second)); ok {
		t.Error("retried before next attempt")
	}
	d2, ok, err := q.ClaimDelivery(ctx, a.tenant, due.ID, next.Add(time.Second), time.Minute)
	if err != nil || !ok || d2.Attempts != 2 {
		t.Fatalf("claim retry: %+v %v %v", d2, ok, err)
	}
	if err := q.CompleteDelivery(ctx, d2, DeliveryResult{Status: "delivered", StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempts int
	if err := db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, attempts FROM notification_deliveries WHERE id = $1`, due.ID).Scan(&status, &attempts)
	}); err != nil || status != "delivered" || attempts != 2 {
		t.Errorf("final: %s %d %v", status, attempts, err)
	}

	// scan 설정은 delivery SELECT만 연다: policy(URL·암호문)·다른 표는 안 보이고, delivery를 바꿀 수 없다
	scan := func(sql string) (int64, error) {
		var n int64
		err := db.inTx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.delivery_scan', 'on', true)`); err != nil {
				return err
			}
			if sql[0] == 'S' {
				return tx.QueryRow(ctx, sql).Scan(&n)
			}
			tag, err := tx.Exec(ctx, sql)
			n = tag.RowsAffected()
			return err
		})
		return n, err
	}
	for _, table := range []string{"notification_policies", "notification_delivery_bodies", "alert_instances", "monitors", "outbox"} {
		if n, err := scan(`SELECT count(*) FROM ` + table); err != nil || n != 0 {
			t.Errorf("scan sees %s: %d %v", table, n, err)
		}
	}
	if n, err := scan(`UPDATE notification_deliveries SET status = 'canceled'`); err != nil || n != 0 {
		t.Errorf("scan updated deliveries: %d %v", n, err)
	}
	if n := count(t, db, &b.tenant, "notification_deliveries"); n != 0 {
		t.Errorf("B sees %d deliveries", n)
	}

	// 보존: 30일 지난 끝난 기록은 주기 정리가 지운다(본문은 CASCADE). 단 아직 열린 사건의 열림 기록은 남긴다(닫힘을 보낼 곳)
	if err := db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE notification_deliveries SET finished_at = now() - interval '31 days' WHERE id = $1`, due.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n, _, err := q.PruneFinished(ctx, time.Now(), "", 1000); err != nil || n != 0 {
		t.Errorf("pruned an open episode's record: %d %v", n, err)
	}
	// 같은 group에 새 사건이 열려 첫 사건(…f20)이 끝났다 → 그 열림 기록은 지운다
	c, _, _ = s.ClaimEvaluation(ctx, a.tenant, ms[0].ID, slot.Add(time.Minute), time.Minute)
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", PolicyID: &np.ID, Groups: []GroupWrite{openedGroup("0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f21")}}); err != nil {
		t.Fatal(err)
	}
	if n, _, err := q.PruneFinished(ctx, time.Now(), "", 1000); err != nil || n != 1 {
		t.Errorf("prune: %d %v", n, err)
	}
	if n := count(t, db, &a.tenant, "notification_delivery_bodies"); n != 1 {
		t.Errorf("bodies after prune: %d (cascade)", n)
	}
}

// 한 policy에 알림이 몰려도(큰 monitor가 한꺼번에 열림) scan은 policy마다 perPolicy개까지만 골라 다른 policy의 알림이 밀려나지 않는다.
// 정리 cursor는 tenant 순으로 넘어간다.
func TestDeliveryScanFairness(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	sealer := testSealer(t)
	busy, _ := createPolicy(t, db, f, sealer, "busy")
	quiet, _ := createPolicy(t, db, f, sealer, "quiet")
	ctx := context.Background()
	w := monitorWrite("0.02")
	if _, _, err := NewMonitorStore(db).CreateMonitor(ctx, developer(t, f), w, idemReq("m", "b"), "r", respondWith); err != nil {
		t.Fatal(err)
	}
	ms, _, _, _ := NewMonitorStore(db).ListMonitors(ctx, developer(t, f), nil, 10)
	err := db.WithTenant(ctx, f.tenant, func(tx pgx.Tx) error {
		for i := 0; i < 30; i++ {
			if err := enqueueOpened(ctx, tx, f.tenant, busy.ID, notice{monitorID: ms[0].ID, groupHash: GroupHash(fmt.Sprint("g", i)), episodeID: newTestUUID(t), at: time.Now()}); err != nil {
				return err
			}
		}
		return enqueueOpened(ctx, tx, f.tenant, quiet.ID, notice{monitorID: ms[0].ID, groupHash: GroupHash("q"), episodeID: newTestUUID(t), at: time.Now()})
	})
	if err != nil {
		t.Fatal(err)
	}
	due, _, err := NewDeliveryStore(db).DueDeliveries(ctx, time.Now().Add(time.Second), 1000, 5)
	if err != nil {
		t.Fatal(err)
	}
	per := map[string]int{}
	for _, d := range due {
		if d.Tenant == f.tenant {
			per[d.PolicyID]++
		}
	}
	if per[busy.ID] != 5 || per[quiet.ID] != 1 {
		t.Errorf("per policy = %v", per)
	}
}

func newTestUUID(t *testing.T) string {
	t.Helper()
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
