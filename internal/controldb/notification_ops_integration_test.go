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
	"github.com/polynomeer/montracer/internal/notify"
)

// backdate는 kind(LIKE 패턴) 알림을 쌓은 시각을 지금보다 ago 전으로 옮긴다(재알림 때를 만들기 위해).
// owner 계정도 FORCE RLS를 받으므로 tenant를 설정한다.
func backdate(t *testing.T, tenant authz.TenantID, kind string, ago time.Duration) {
	t.Helper()
	ctx := context.Background()
	conn := adminConn(t)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE notification_deliveries SET created_at = now() - $1 * interval '1 second' WHERE event_type LIKE $2`, int(ago.Seconds()), kind); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func stillAlerting(episode string) GroupWrite {
	g := alertGroup("ALERT", &episode)
	v := 0.06
	g.Instance.LastValue = &v
	return g
}

// 재알림: 열린 사건이 ALERT로 남아 있으면 열림 알림 30분×k 뒤 k번째(최대 3번)를 쌓는다. 같은 번호는 하나뿐이고,
// RECOVERING 동안은 쌓지 않고, 열림을 받은 policy에만 간다. deep link는 절대 시간 범위를 담는다.
func TestRenotify(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	sealer := testSealer(t)
	np, _ := createPolicy(t, db, f, sealer, "rn")
	ctx := context.Background()
	monitors := NewMonitorStore(db)
	w := monitorWrite("0.02")
	w.NotificationPolicyID = &np.ID
	if _, _, err := monitors.CreateMonitor(ctx, developer(t, f), w, idemReq("rn", "b"), "r", respondWith); err != nil {
		t.Fatal(err)
	}
	ms, _, _, _ := monitors.ListMonitors(ctx, developer(t, f), nil, 10)
	id := ms[0].ID
	links, err := notify.ParseWebLinks("https://app.example.com")
	if err != nil {
		t.Fatal(err)
	}
	s := NewAlertStore(db).WithLinks(links)
	episode := "0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f40"
	slot := time.Now().UTC().Truncate(time.Minute).Add(6 * time.Hour)
	eval := func(i int, g GroupWrite) {
		t.Helper()
		c, ok, err := s.ClaimEvaluation(ctx, f.tenant, id, slot.Add(time.Duration(i)*time.Minute), time.Minute)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", PolicyID: &np.ID, MonitorName: "m", Groups: []GroupWrite{g}}); err != nil {
			t.Fatal(err)
		}
	}
	reminders := func() []map[string]any {
		var out []map[string]any
		for _, d := range deliveries(t, db, f.tenant) {
			if k, ok := notify.ParseRenotifyKind(d["type"].(string)); ok && d["env"].(map[string]any)["event_type"] == notify.EventRenotified {
				if d["env"].(map[string]any)["payload"].(map[string]any)["renotify_sequence"] != float64(k) {
					t.Errorf("stored kind %d, payload %+v", k, d["env"])
				}
				out = append(out, d)
			}
		}
		return out
	}
	eval(0, openedGroup(episode))
	opened := deliveries(t, db, f.tenant)[0]["env"].(map[string]any)["payload"].(map[string]any)
	link, _ := opened["links"].(map[string]any)["monitor"].(string)
	if !strings.HasPrefix(link, "https://app.example.com/o/"+f.tenant.String()+"/monitors/"+id+"?from=") || !strings.Contains(link, "&tz=UTC") {
		t.Errorf("opened link = %q", link)
	}
	// 30분이 안 지났다: 재알림 없음
	eval(1, stillAlerting(episode))
	if r := reminders(); len(r) != 0 {
		t.Fatalf("early reminder: %+v", r)
	}
	// 31분: 1번째. 같은 평가를 다시 해도(다음 slot) 1번째는 하나다
	backdate(t, f.tenant, "alert.opened", 31*time.Minute)
	eval(2, stillAlerting(episode))
	eval(3, stillAlerting(episode))
	r := reminders()
	if len(r) != 1 {
		t.Fatalf("reminders after 31m: %d", len(r))
	}
	p := r[0]["env"].(map[string]any)["payload"].(map[string]any)
	if p["renotify_sequence"] != float64(1) || p["state"] != "ALERT" || p["episode_id"] != episode || p["value"] != 0.06 || r[0]["policy"] != np.ID {
		t.Errorf("reminder payload = %+v", p)
	}
	// 열린 지 2시간·1번째에서 90분: RECOVERING이면 쌓지 않는다. ALERT로 돌아오면 2번째 하나
	backdate(t, f.tenant, "alert.opened", 2*time.Hour)
	backdate(t, f.tenant, "alert.renotified.1", 90*time.Minute)
	eval(4, alertGroup("RECOVERING", &episode))
	if len(reminders()) != 1 {
		t.Fatal("reminder while recovering")
	}
	eval(5, stillAlerting(episode))
	// 다음 평가(1분 뒤): 열림 90분은 지났어도 직전 재알림에서 30분이 안 지났다 — 몰아서 보내지 않는다
	eval(6, stillAlerting(episode))
	if r := reminders(); len(r) != 2 {
		t.Fatalf("reminders right after returning to ALERT: %d", len(r))
	}
	backdate(t, f.tenant, "alert.renotified.2", 31*time.Minute)
	eval(7, stillAlerting(episode))
	if r := reminders(); len(r) != 3 {
		t.Fatalf("third reminder: %d", len(r))
	}
	// 3번 뒤에는 더 없다(escalation은 F11)
	backdate(t, f.tenant, "alert.%", 10*time.Hour)
	for i := 8; i < 11; i++ {
		eval(i, stillAlerting(episode))
	}
	if r := reminders(); len(r) != notify.RenotifyMax {
		t.Errorf("reminders capped at %d, got %d", notify.RenotifyMax, len(r))
	}
}

// 재알림 끄기(MONTRACER_NOTIFY_RENOTIFY_ENABLED=false): 때가 지나도 재알림은 없고, 열림·닫힘 알림은 그대로다.
func TestRenotifyDisabled(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	np, _ := createPolicy(t, db, f, testSealer(t), "off")
	ctx := context.Background()
	monitors := NewMonitorStore(db)
	w := monitorWrite("0.02")
	w.NotificationPolicyID = &np.ID
	if _, _, err := monitors.CreateMonitor(ctx, developer(t, f), w, idemReq("off", "b"), "r", respondWith); err != nil {
		t.Fatal(err)
	}
	ms, _, _, _ := monitors.ListMonitors(ctx, developer(t, f), nil, 10)
	s := NewAlertStore(db).WithRenotify(false)
	episode := "0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f41"
	slot := time.Now().UTC().Truncate(time.Minute).Add(7 * time.Hour)
	for i, g := range []GroupWrite{openedGroup(episode), stillAlerting(episode)} {
		if i == 1 {
			backdate(t, f.tenant, "alert.opened", 3*time.Hour)
		}
		c, ok, err := s.ClaimEvaluation(ctx, f.tenant, ms[0].ID, slot.Add(time.Duration(i)*time.Minute), time.Minute)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", PolicyID: &np.ID, Groups: []GroupWrite{g}}); err != nil {
			t.Fatal(err)
		}
	}
	ds := deliveries(t, db, f.tenant)
	if len(ds) != 1 || ds[0]["type"] != notify.EventOpened {
		t.Errorf("renotify disabled: %+v", ds)
	}
}

// 시험 전송: Tenant Admin만, 꺼진 policy에도 쌓인다. 진행 중이면 같은 행을 돌려주고, 끝나면 새로 만든다.
// 1시간 20번을 넘으면 거절. 보낸 기록은 security 감사다. 다른 tenant에서는 보이지 않는다. 경보 알림 조회(timeline)에는 섞이지 않는다.
func TestNotificationTestSend(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	sealer := testSealer(t)
	np, _ := createPolicy(t, db, a, sealer, "ts")
	ctx := context.Background()
	store := NewNotificationPolicyStore(db)
	if _, _, err := store.SendTest(ctx, developer(t, a), np.ID, "r"); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("developer: %v", err)
	}
	if _, _, err := store.SendTest(ctx, b.admin, np.ID, "r"); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("other tenant: %v", err)
	}
	first, reused, err := store.SendTest(ctx, a.admin, np.ID, "r")
	if err != nil || reused || first.Status != "pending" || first.PolicyID != np.ID {
		t.Fatalf("first: %+v %v %v", first, reused, err)
	}
	again, reused, err := store.SendTest(ctx, a.admin, np.ID, "r")
	if err != nil || !reused || again.ID != first.ID {
		t.Errorf("while pending: %+v %v %v", again, reused, err)
	}
	ds := deliveries(t, db, a.tenant)
	env := ds[0]["env"].(map[string]any)
	if len(ds) != 1 || ds[0]["type"] != notify.EventTest || env["resource_id"] != np.ID || env["payload"].(map[string]any)["test"] != true {
		t.Fatalf("test delivery = %+v", ds)
	}
	// 보내기: lease를 잡으면 시험 종류로 읽힌다. 실패로 끝내면 다음 시험은 새로 만든다
	ds2 := NewDeliveryStore(db)
	d, ok, err := ds2.ClaimDelivery(ctx, a.tenant, first.ID, time.Now().Add(time.Second), time.Minute)
	if err != nil || !ok || d.EventType != notify.EventTest {
		t.Fatalf("claim test: %+v %v %v", d, ok, err)
	}
	if err := ds2.CompleteDelivery(ctx, d, DeliveryResult{Status: "failed", StatusCode: 404, Error: notify.ErrCodeStatus}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetTest(ctx, a.admin, np.ID, first.ID)
	if err != nil || got.Status != "failed" || got.LastStatusCode == nil || *got.LastStatusCode != 404 || got.FinishedAt == nil {
		t.Errorf("get test: %+v %v", got, err)
	}
	if _, err := store.GetTest(ctx, b.admin, np.ID, first.ID); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("other tenant get: %v", err)
	}
	// 꺼진 policy에도 시험할 수 있다
	if _, err := store.UpdatePolicy(ctx, a.admin, np.ID, np.Revision, policyWrite(t, a, sealer, "off", "https://hooks.example.com/off"), "r"); err != nil {
		t.Fatal(err)
	}
	var n int
	for n = 1; n < MaxTestsPerHour; n++ {
		td, reused, err := store.SendTest(ctx, a.admin, np.ID, "r")
		if err != nil || reused {
			t.Fatalf("test %d: %v %v", n, reused, err)
		}
		d, _, err := ds2.ClaimDelivery(ctx, a.tenant, td.ID, time.Now().Add(time.Second), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := ds2.CompleteDelivery(ctx, d, DeliveryResult{Status: "delivered", StatusCode: 204}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.SendTest(ctx, a.admin, np.ID, "r"); !errors.Is(err, ErrTestRateLimited) {
		t.Errorf("over limit: %v", err)
	}
	var audits int
	if err := db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'notification_policy.tested' AND category = 'security'`).Scan(&audits)
	}); err != nil || audits != MaxTestsPerHour {
		t.Errorf("test audits = %d %v", audits, err)
	}
}

// secret 교체: 새 secret이 지금 secret이 되고 이전 secret은 겹침 기간 동안 함께 읽힌다(claim). 0이면 바로 버린다.
// 낡은 revision은 거절. 감사에는 key id만 있다. 교체한 secret은 그 tenant·policy로만 풀린다.
func TestRotateSecret(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	sealer := testSealer(t)
	np, old := createPolicy(t, db, f, sealer, "rot")
	ctx := context.Background()
	store := NewNotificationPolicyStore(db)
	fresh, _ := notify.NewSecret()
	seal := func(sec notify.Secret) func(string) (SealedSecret, error) {
		return func(id string) (SealedSecret, error) {
			c, err := sealer.Seal(f.tenant.String(), id, sec)
			return SealedSecret{KeyID: sec.KeyID, Ciphertext: c, SealingKey: sealer.Version()}, err
		}
	}
	if _, err := store.RotateSecret(ctx, developer(t, f), np.ID, np.Revision, time.Hour, seal(fresh), "r"); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("developer: %v", err)
	}
	if _, err := store.RotateSecret(ctx, f.admin, np.ID, np.Revision+5, time.Hour, seal(fresh), "r"); !errors.Is(err, ErrRevisionMismatch) {
		t.Errorf("stale revision: %v", err)
	}
	rot, err := store.RotateSecret(ctx, f.admin, np.ID, np.Revision, time.Hour, seal(fresh), "r")
	if err != nil || rot.SecretKeyID != fresh.KeyID || rot.PreviousKeyID == nil || *rot.PreviousKeyID != old.KeyID || rot.PreviousKeyExpiresAt == nil ||
		rot.Revision != np.Revision+1 {
		t.Fatalf("rotate: %+v %v", rot, err)
	}
	// 알림을 보낼 때: 지금 secret과 이전 secret을 함께 읽고, 둘 다 풀린다
	td, _, err := store.SendTest(ctx, f.admin, np.ID, "r")
	if err != nil {
		t.Fatal(err)
	}
	d, ok, err := NewDeliveryStore(db).ClaimDelivery(ctx, f.tenant, td.ID, time.Now().Add(time.Second), time.Minute)
	if err != nil || !ok || d.SecretKeyID != fresh.KeyID || d.PreviousKeyID == nil || *d.PreviousKeyID != old.KeyID {
		t.Fatalf("claim after rotate: %+v %v", d, err)
	}
	if cur, err := sealer.Open(f.tenant.String(), np.ID, d.SecretKeyID, d.SealingKey, d.Sealed); err != nil || string(cur.Key) != string(fresh.Key) {
		t.Errorf("current secret: %v", err)
	}
	if prev, err := sealer.Open(f.tenant.String(), np.ID, *d.PreviousKeyID, *d.PreviousSealingKey, d.PreviousSealed); err != nil || string(prev.Key) != string(old.Key) {
		t.Errorf("previous secret: %v", err)
	}
	// 겹침 기간이 지나면 이전 secret은 읽히지 않는다
	later, ok, err := NewDeliveryStore(db).ClaimDelivery(ctx, f.tenant, td.ID, time.Now().Add(2*time.Hour), time.Minute)
	if err != nil || !ok || later.PreviousKeyID != nil || later.PreviousSealed != nil {
		t.Errorf("after overlap: %+v %v", later, err)
	}
	if err := NewDeliveryStore(db).CompleteDelivery(ctx, later, DeliveryResult{Status: "delivered", StatusCode: 204}); err != nil {
		t.Fatal(err)
	}
	// 겹침 중 다시 교체(응답을 잃은 재시도 등): 이전 secret(old)과 그 만료는 그대로, 지금 secret만 바뀐다
	again, _ := notify.NewSecret()
	rotAgain, err := store.RotateSecret(ctx, f.admin, np.ID, rot.Revision, time.Hour, seal(again), "r")
	if err != nil || rotAgain.SecretKeyID != again.KeyID || rotAgain.PreviousKeyID == nil || *rotAgain.PreviousKeyID != old.KeyID ||
		!rotAgain.PreviousKeyExpiresAt.Equal(*rot.PreviousKeyExpiresAt) {
		t.Fatalf("rotate during overlap: %+v %v", rotAgain, err)
	}
	// policy 수정(PUT)은 secret 열을 바꾸지 않는다(secret 열 UPDATE 권한은 교체 API만 쓴다)
	upd, err := store.UpdatePolicy(ctx, f.admin, np.ID, rotAgain.Revision, policyWrite(t, f, sealer, "renamed", "https://hooks.example.com/renamed"), "r")
	if err != nil || upd.SecretKeyID != again.KeyID || upd.PreviousKeyID == nil || *upd.PreviousKeyID != old.KeyID {
		t.Fatalf("update after rotate: %+v %v", upd, err)
	}
	td2, _, err := store.SendTest(ctx, f.admin, np.ID, "r")
	if err != nil {
		t.Fatal(err)
	}
	if d, ok, err := NewDeliveryStore(db).ClaimDelivery(ctx, f.tenant, td2.ID, time.Now().Add(time.Second), time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	} else if cur, err := sealer.Open(f.tenant.String(), np.ID, d.SecretKeyID, d.SealingKey, d.Sealed); err != nil || string(cur.Key) != string(again.Key) {
		t.Errorf("secret after update: %v", err)
	}
	// URL 없이 수정(이름·켜기만): 저장된 URL 암호문·표시가 그대로다
	keep := policyWrite(t, f, sealer, "renamed again", "https://unused.example.com/x")
	keep.SealURL, keep.Display = nil, ""
	keep.Enabled, keep.KeepEnabled = !upd.Enabled, true // enabled를 보내지 않음: 저장된 값 유지
	kept, err := store.UpdatePolicy(ctx, f.admin, np.ID, upd.Revision, keep, "r")
	if err != nil || kept.Name != "renamed again" || kept.WebhookURL != upd.WebhookURL || kept.Enabled != upd.Enabled {
		t.Fatalf("update without url/enabled: %+v %v", kept, err)
	}
	var changed []bool
	if err := db.WithTenant(ctx, f.tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT (details->>'webhook_changed')::boolean FROM audit_events
			WHERE action = 'notification_policy.updated' AND resource_id = $1 ORDER BY occurred_at`, np.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c bool
			if err := rows.Scan(&c); err != nil {
				return err
			}
			changed = append(changed, c)
		}
		return rows.Err()
	}); err != nil || len(changed) != 2 || !changed[0] || changed[1] {
		t.Errorf("audit webhook_changed = %v %v", changed, err)
	}
	if d, ok, err := NewDeliveryStore(db).ClaimDelivery(ctx, f.tenant, td2.ID, time.Now().Add(3*time.Minute), time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	} else if u, err := sealer.OpenURL(f.tenant.String(), np.ID, d.URLSealingKey, d.URLSealed); err != nil || u != "https://hooks.example.com/renamed" {
		t.Errorf("url after update without url: %q %v", u, err)
	}
	// 0이면 이전 secret 없이 바로 바꾼다
	third, _ := notify.NewSecret()
	rot2, err := store.RotateSecret(ctx, f.admin, np.ID, kept.Revision, 0, seal(third), "r")
	if err != nil || rot2.PreviousKeyID != nil || rot2.SecretKeyID != third.KeyID {
		t.Errorf("immediate rotate: %+v %v", rot2, err)
	}
	var details string
	if err := db.WithTenant(ctx, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT details::text FROM audit_events WHERE action = 'notification_policy.secret_rotated' ORDER BY occurred_at DESC LIMIT 1`).Scan(&details)
	}); err != nil || !strings.Contains(details, third.KeyID) || !strings.Contains(details, again.KeyID) || strings.Contains(details, "whsec_") {
		t.Errorf("rotate audit = %s %v", details, err)
	}
	if _, err := store.RotateSecret(ctx, f.admin, np.ID, rot2.Revision, MaxSecretOverlap+time.Second, seal(third), "r"); err == nil {
		t.Error("overlap beyond 24h accepted")
	}
}

// Idempotency-Key(ADR 0060 §1): 같은 key 재요청은 부작용 없이 저장된 응답을 돌려주고, 다른 요청이면 충돌이다.
// 시험 전송은 새 행을 쌓지 않고, secret 교체는 두 번 바뀌지 않는다(revision 그대로). 부작용과 응답 저장은 한 트랜잭션이다.
func TestIdempotentTestAndRotate(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	sealer := testSealer(t)
	np, _ := createPolicy(t, db, f, sealer, "idem")
	ctx := context.Background()
	store := NewNotificationPolicyStore(db)
	idem := func(path, key string, body []byte) IdempotencyRequest {
		return IdempotencyRequest{Key: key, MethodPath: "POST " + path, RequestHash: sha256.Sum256(body)}
	}
	testPath := "/api/v1/notification-policies/" + np.ID + "/test"
	calls := 0
	respondTest := func(td TestDelivery, _ bool) (StoredResponse, error) {
		calls++
		return StoredResponse{Status: 202, Body: []byte(`{"id":"` + td.ID + `"}`)}, nil
	}
	first, replayed, err := store.SendTestIdempotent(ctx, f.admin, np.ID, "r1", idem(testPath, "k1", nil), respondTest)
	if err != nil || replayed || calls != 1 {
		t.Fatalf("first: %s %v %v", first.Body, replayed, err)
	}
	again, replayed, err := store.SendTestIdempotent(ctx, f.admin, np.ID, "r2", idem(testPath, "k1", nil), respondTest)
	if err != nil || !replayed || string(again.Body) != string(first.Body) || calls != 1 {
		t.Errorf("replay: %s %v %v calls=%d", again.Body, replayed, err, calls)
	}
	if n := len(deliveries(t, db, f.tenant)); n != 1 {
		t.Errorf("deliveries after replay = %d, want 1", n)
	}
	if _, _, err := store.SendTestIdempotent(ctx, f.admin, np.ID, "r3", idem(testPath, "k1", []byte("other")), respondTest); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("same key, different request: %v", err)
	}

	rotPath := "/api/v1/notification-policies/" + np.ID + "/rotate-secret"
	secret, _ := notify.NewSecret()
	seal := func(id string) (SealedSecret, error) {
		c, err := sealer.Seal(f.tenant.String(), id, secret)
		return SealedSecret{KeyID: secret.KeyID, Ciphertext: c, SealingKey: sealer.Version()}, err
	}
	var revs []int64
	respondRot := func(p NotificationPolicy) (StoredResponse, error) {
		revs = append(revs, p.Revision)
		return StoredResponse{Status: 200, Body: []byte(fmt.Sprintf(`{"revision":%d}`, p.Revision))}, nil
	}
	r1, replayed, err := store.RotateSecretIdempotent(ctx, f.admin, np.ID, np.Revision, time.Hour, seal, "r4", idem(rotPath, "k2", nil), respondRot)
	if err != nil || replayed {
		t.Fatalf("rotate: %s %v %v", r1.Body, replayed, err)
	}
	r2, replayed, err := store.RotateSecretIdempotent(ctx, f.admin, np.ID, np.Revision, time.Hour, seal, "r5", idem(rotPath, "k2", nil), respondRot)
	if err != nil || !replayed || string(r2.Body) != string(r1.Body) || len(revs) != 1 {
		t.Errorf("rotate replay: %s %v %v revs=%v", r2.Body, replayed, err, revs)
	}
	got, err := store.GetPolicy(ctx, f.admin, np.ID)
	if err != nil || got.Revision != np.Revision+1 {
		t.Errorf("rotated twice: revision %d (want %d) %v", got.Revision, np.Revision+1, err)
	}
	// 실패한 요청(낡은 If-Match)은 key를 남기지 않는다: 같은 key로 올바른 요청을 다시 보낼 수 있다
	if _, _, err := store.RotateSecretIdempotent(ctx, f.admin, np.ID, np.Revision, time.Hour, seal, "r6", idem(rotPath, "k3", nil), respondRot); !errors.Is(err, ErrRevisionMismatch) {
		t.Errorf("stale: %v", err)
	}
	if _, replayed, err := store.RotateSecretIdempotent(ctx, f.admin, np.ID, got.Revision, time.Hour, seal, "r7", idem(rotPath, "k3", nil), respondRot); err != nil || replayed {
		t.Errorf("retry after failure with same key: %v %v", replayed, err)
	}
}
