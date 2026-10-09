//go:build integration

package controldb

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

func createMonitor(t *testing.T, db *DB, f fixture) string {
	t.Helper()
	ctx := context.Background()
	store := NewMonitorStore(db)
	dev := developer(t, f)
	if _, _, err := store.CreateMonitor(ctx, dev, monitorWrite("0.02"), idemReq("k-"+f.tenant.String(), "b"), "r", respondWith); err != nil {
		t.Fatal(err)
	}
	ms, _, _, err := store.ListMonitors(ctx, dev, nil, 10)
	if err != nil || len(ms) != 1 {
		t.Fatalf("list: %+v %v", ms, err)
	}
	return ms[0].ID
}

func isDue(t *testing.T, s *AlertStore, f fixture, id string, now time.Time) bool {
	t.Helper()
	due, err := s.DueMonitors(context.Background(), now, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.Tenant == f.tenant && d.MonitorID == id {
			return true
		}
	}
	return false
}

type alertEvent struct {
	Revision int64
	Payload  map[string]any
}

func alertEvents(t *testing.T, db *DB, tenant authz.TenantID) []alertEvent {
	t.Helper()
	ctx := context.Background()
	var out []alertEvent
	err := db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT revision, payload FROM outbox WHERE type = 'alert.state_changed' ORDER BY occurred_at, event_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				e   alertEvent
				raw []byte
			)
			if err := rows.Scan(&e.Revision, &raw); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &e.Payload); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func alertGroup(state string, episode *string) GroupWrite {
	reason := "violation"
	in := StoredInstance{GroupKey: "service.name=checkout", Labels: map[string]string{"service.name": "checkout"}, State: state, OKStreak: 0}
	if episode != nil {
		in.EpisodeID, in.EpisodeReason = episode, &reason
	}
	return GroupWrite{Instance: in}
}

// slot 하나는 lease를 잡은 worker 하나만 평가하고, 끝난 slot은 다시 잡히지 않는다. 상태·평가 기록·전이 event는 한 트랜잭션이다.
func TestAlertEvaluationLeaseAndSlot(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	id := createMonitor(t, db, f)
	s := NewAlertStore(db)
	ctx := context.Background()
	slot := time.Now().UTC().Truncate(time.Minute).Add(time.Hour) // 다른 테스트의 시각과 섞이지 않게 미래 slot
	now := slot.Add(10 * time.Second)

	if !isDue(t, s, f, id, now) {
		t.Fatal("new monitor not due")
	}
	c, ok, err := s.ClaimEvaluation(ctx, f.tenant, id, now, time.Minute)
	if err != nil || !ok || c.Revision != 1 || !c.Slot.Equal(slot) || len(c.Spec) == 0 || len(c.Instances) != 0 {
		t.Fatalf("claim: %+v %v %v", c, ok, err)
	}
	// lease가 살아 있으면 다른 worker는 못 잡고, 할 일 목록에도 없다
	if _, ok, err := s.ClaimEvaluation(ctx, f.tenant, id, now, time.Minute); err != nil || ok {
		t.Errorf("second claim: %v %v", ok, err)
	}
	if isDue(t, s, f, id, now) {
		t.Error("leased monitor listed as due")
	}
	// lease token이 다르면(같은 worker 이름이라도) 결과를 쓸 수 없다
	other := c
	other.Token = "0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9fff"
	if err := s.CompleteEvaluation(ctx, other, "w1", Completion{Status: "evaluated"}); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("complete with another token: %v", err)
	}

	episode := "0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f01"
	g := alertGroup("ALERT", &episode)
	g.Changed, g.From, g.To, g.Opened, g.EpisodeOut = true, "OK", "ALERT", true, &episode
	end := slot
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", WindowEnd: &end, Groups: []GroupWrite{g}, Duration: 12 * time.Millisecond}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if n := count(t, db, &f.tenant, "monitor_evaluations"); n != 1 {
		t.Errorf("evaluations = %d", n)
	}
	if ev := alertEvents(t, db, f.tenant); len(ev) != 1 || ev[0].Payload["to"] != "ALERT" || ev[0].Payload["opened"] != true || ev[0].Payload["episode_id"] != episode {
		t.Errorf("events = %+v", ev)
	}
	// 끝난 slot은 다시 잡히지 않는다(같은 slot 두 번 평가 없음)
	if _, ok, err := s.ClaimEvaluation(ctx, f.tenant, id, now.Add(20*time.Second), time.Minute); err != nil || ok {
		t.Errorf("claim finished slot: %v %v", ok, err)
	}

	// 다음 slot: 저장 상태(원문 key·labels·사건 id)를 그대로 돌려받는다. 변화 없는 평가는 event를 내지 않는다
	c2, ok, err := s.ClaimEvaluation(ctx, f.tenant, id, now.Add(time.Minute), 5*time.Second)
	if err != nil || !ok || len(c2.Instances) != 1 {
		t.Fatalf("claim next: %+v %v %v", c2, ok, err)
	}
	in := c2.Instances[0]
	if in.GroupKey != "service.name=checkout" || in.Labels["service.name"] != "checkout" || in.State != "ALERT" || in.EpisodeID == nil || *in.EpisodeID != episode {
		t.Errorf("stored instance = %+v", in)
	}
	// lease 만료(crash) → 다른 worker가 같은 slot을 가져가고, 원래 worker의 늦은 결과는 버려진다
	c3, ok, err := s.ClaimEvaluation(ctx, f.tenant, id, now.Add(time.Minute+10*time.Second), time.Minute)
	if err != nil || !ok || !c3.Slot.Equal(c2.Slot) {
		t.Fatalf("retake expired lease: %+v %v %v", c3, ok, err)
	}
	if err := s.CompleteEvaluation(ctx, c2, "w1", Completion{Status: "evaluated", Groups: []GroupWrite{alertGroup("ALERT", &episode)}}); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("late complete: %v", err)
	}
	if err := s.CompleteEvaluation(ctx, c3, "w2", Completion{Status: "evaluated", Groups: []GroupWrite{alertGroup("ALERT", &episode)}}); err != nil {
		t.Fatalf("complete retaken: %v", err)
	}
	if n := count(t, db, &f.tenant, "monitor_evaluations"); n != 2 {
		t.Errorf("evaluations = %d", n)
	}
	if ev := alertEvents(t, db, f.tenant); len(ev) != 1 {
		t.Errorf("unchanged evaluation emitted events: %+v", ev)
	}
}

// 정의가 바뀌거나 꺼지거나 지워지면 평가 중이던 결과는 버려지고, 열린 사건은 닫힘 event를 내고 끝난다.
func TestAlertStateEndsOnRevisionAndDelete(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	id := createMonitor(t, db, f)
	s := NewAlertStore(db)
	monitors := NewMonitorStore(db)
	ctx := context.Background()
	dev := developer(t, f)
	slot := time.Now().UTC().Truncate(time.Minute).Add(2 * time.Hour)

	episode := "0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f02"
	c, ok, err := s.ClaimEvaluation(ctx, f.tenant, id, slot, time.Minute)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	g := alertGroup("ALERT", &episode)
	g.Changed, g.From, g.To, g.Opened, g.EpisodeOut = true, "OK", "ALERT", true, &episode
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", Groups: []GroupWrite{g}}); err != nil {
		t.Fatal(err)
	}
	// 다음 slot 평가 중에 정의가 바뀐다
	c, ok, err = s.ClaimEvaluation(ctx, f.tenant, id, slot.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := monitors.UpdateMonitor(ctx, dev, id, 1, monitorWrite("0.05"), "r2"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", Groups: []GroupWrite{alertGroup("ALERT", &episode)}}); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("complete after revision change: %v", err)
	}
	if n := count(t, db, &f.tenant, "alert_instances"); n != 0 {
		t.Errorf("instances after revision = %d", n)
	}
	ev := alertEvents(t, db, f.tenant)
	if len(ev) != 2 || ev[1].Revision != 2 || ev[1].Payload["closed"] != true || ev[1].Payload["episode_id"] != episode || ev[1].Payload["reason"] != "monitor_revised" {
		t.Fatalf("events = %+v", ev)
	}
	// 새 revision은 다음 slot부터 새 상태로 평가한다
	c, ok, err = s.ClaimEvaluation(ctx, f.tenant, id, slot.Add(2*time.Minute), time.Minute)
	if err != nil || !ok || c.Revision != 2 || len(c.Instances) != 0 {
		t.Fatalf("claim revision 2: %+v %v %v", c, ok, err)
	}
	episode2 := "0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f03"
	g = alertGroup("ALERT", &episode2)
	g.Changed, g.From, g.To, g.Opened, g.EpisodeOut = true, "OK", "ALERT", true, &episode2
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", Groups: []GroupWrite{g}}); err != nil {
		t.Fatal(err)
	}
	// 끄기(enabled=false): 할 일에서 빠지고 열린 사건은 닫힌다. 다시 켜면 다음 slot부터 빈 상태로 평가한다
	off := monitorWrite("0.05")
	off.Enabled = false
	if _, err := monitors.UpdateMonitor(ctx, dev, id, 2, off, "r3"); err != nil {
		t.Fatal(err)
	}
	if isDue(t, s, f, id, slot.Add(5*time.Minute)) {
		t.Error("disabled monitor still due")
	}
	ev = alertEvents(t, db, f.tenant)
	if last := ev[len(ev)-1]; last.Payload["closed"] != true || last.Payload["episode_id"] != episode2 || last.Payload["reason"] != "monitor_disabled" {
		t.Errorf("disable event = %+v", last)
	}
	if _, err := monitors.UpdateMonitor(ctx, dev, id, 3, monitorWrite("0.05"), "r4"); err != nil {
		t.Fatal(err)
	}
	c, ok, err = s.ClaimEvaluation(ctx, f.tenant, id, slot.Add(6*time.Minute), time.Minute)
	if err != nil || !ok || c.Revision != 4 || len(c.Instances) != 0 {
		t.Fatalf("claim re-enabled: %+v %v %v", c, ok, err)
	}
	episode2 = "0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f05"
	g = alertGroup("ALERT", &episode2)
	g.Changed, g.From, g.To, g.Opened, g.EpisodeOut = true, "OK", "ALERT", true, &episode2
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", Groups: []GroupWrite{g}}); err != nil {
		t.Fatal(err)
	}
	// 삭제: 평가 대상에서 빠지고 열린 사건은 닫힌다
	if _, err := monitors.DeleteMonitor(ctx, dev, id, nil, "r5"); err != nil {
		t.Fatal(err)
	}
	if isDue(t, s, f, id, slot.Add(10*time.Minute)) {
		t.Error("deleted monitor still due")
	}
	if _, ok, err := s.ClaimEvaluation(ctx, f.tenant, id, slot.Add(10*time.Minute), time.Minute); err != nil || ok {
		t.Errorf("claim deleted: %v %v", ok, err)
	}
	ev = alertEvents(t, db, f.tenant)
	if last := ev[len(ev)-1]; last.Payload["closed"] != true || last.Payload["episode_id"] != episode2 || last.Payload["reason"] != "monitor_deleted" {
		t.Errorf("delete event = %+v", last)
	}
	if n := count(t, db, &f.tenant, "alert_instances"); n != 0 {
		t.Errorf("instances after delete = %d", n)
	}
}

// 오래된 평가 기록은 그 monitor를 평가할 때 정리된다(7일).
func TestAlertEvaluationRetention(t *testing.T) {
	db := openDB(t)
	f := newTenant(t, db)
	id := createMonitor(t, db, f)
	s := NewAlertStore(db)
	ctx := context.Background()
	slot := time.Now().UTC().Truncate(time.Minute).Add(3 * time.Hour)
	err := db.WithTenant(ctx, f.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO monitor_evaluations (tenant_id, monitor_id, slot_start, revision, status, groups, alerting, worker, duration_ms)
			VALUES (app_tenant_id(), $1, $2, 1, 'evaluated', 0, 0, 'old', 0), (app_tenant_id(), $1, $3, 1, 'evaluated', 0, 0, 'old', 0)`,
			id, slot.Add(-EvaluationRetention-time.Minute), slot.Add(-time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	c, ok, err := s.ClaimEvaluation(ctx, f.tenant, id, slot, time.Minute)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "no_data", Reason: "no_window"}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, &f.tenant, "monitor_evaluations"); n != 2 {
		t.Errorf("evaluations after prune = %d (want the recent one and the new one)", n)
	}
}

// 가로 조회(app.monitor_scan)는 monitor_schedule SELECT만 연다. 다른 tenant의 상태·정의는 보이지도 바뀌지도 않는다.
func TestAlertIsolationAndGrants(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	id := createMonitor(t, db, a)
	s := NewAlertStore(db)
	ctx := context.Background()
	slot := time.Now().UTC().Truncate(time.Minute).Add(4 * time.Hour)
	episode := "0b6c2b4e-6f4f-4a52-9a0f-6f6f2b1f9f04"
	c, ok, err := s.ClaimEvaluation(ctx, a.tenant, id, slot, time.Minute)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.CompleteEvaluation(ctx, c, "w1", Completion{Status: "evaluated", Groups: []GroupWrite{alertGroup("ALERT", &episode)}}); err != nil {
		t.Fatal(err)
	}

	// B의 tenant로는 A의 monitor를 잡을 수 없고, A의 상태도 보이지 않는다
	if _, ok, err := s.ClaimEvaluation(ctx, b.tenant, id, slot.Add(time.Minute), time.Minute); err != nil || ok {
		t.Errorf("B claims A's monitor: %v %v", ok, err)
	}
	forged := Claim{Tenant: b.tenant, MonitorID: id, Token: c.Token, Revision: 1, Slot: slot.Add(time.Minute)}
	if err := s.CompleteEvaluation(ctx, forged, "w1", Completion{Status: "evaluated"}); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("B completes A's monitor: %v", err)
	}
	for _, table := range []string{"monitor_schedule", "monitor_evaluations", "alert_instances"} {
		if n := count(t, db, &b.tenant, table); n != 0 {
			t.Errorf("B sees %d rows of %s", n, table)
		}
		if n := count(t, db, nil, table); n != 0 {
			t.Errorf("no tenant context sees %d rows of %s", n, table)
		}
	}

	// scan 설정은 schedule SELECT만 연다: 다른 표는 여전히 안 보이고 schedule도 바꿀 수 없다
	scan := func(q string) (int64, error) {
		var n int64
		err := db.inTx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.monitor_scan', 'on', true)`); err != nil {
				return err
			}
			if q[0] == 'S' {
				return tx.QueryRow(ctx, q).Scan(&n)
			}
			tag, err := tx.Exec(ctx, q)
			n = tag.RowsAffected()
			return err
		})
		return n, err
	}
	if n, err := scan(`SELECT count(*) FROM monitor_schedule WHERE tenant_id = '` + a.tenant.String() + `'`); err != nil || n != 1 {
		t.Errorf("scan schedule: %d %v", n, err)
	}
	for _, table := range []string{"monitor_evaluations", "alert_instances", "monitors", "monitor_revisions", "outbox"} {
		if n, err := scan(`SELECT count(*) FROM ` + table); err != nil || n != 0 {
			t.Errorf("scan sees %s: %d %v", table, n, err)
		}
	}
	if n, err := scan(`UPDATE monitor_schedule SET lease_token = gen_random_uuid()`); err != nil || n != 0 {
		t.Errorf("scan updated schedule: %d %v", n, err)
	}

	// 앱 role은 평가 기록을 고칠 수 없고, schedule의 tenant·monitor id를 바꿀 수 없다
	err = db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE monitor_evaluations SET status = 'error'`)
		return err
	})
	if !isPgCode(err, "42501") {
		t.Errorf("app role UPDATE monitor_evaluations: %v", err)
	}
	err = db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE monitor_schedule SET tenant_id = $1`, b.tenant.String())
		return err
	})
	if !isPgCode(err, "42501") {
		t.Errorf("app role UPDATE monitor_schedule.tenant_id: %v", err)
	}
}
