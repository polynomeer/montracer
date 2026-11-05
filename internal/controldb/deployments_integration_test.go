//go:build integration

package controldb

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
)

func deployKey(t *testing.T, f fixture, envs []string) authz.Principal {
	t.Helper()
	gen := f.issue(t, authz.KindAPIKey, []authz.Action{authz.DeploymentsWrite, authz.TelemetryRead}, envs)
	p, err := f.authenticate(gen.Token, authz.KindAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func deployIdem(key, body string) IdempotencyRequest {
	return IdempotencyRequest{Key: key, MethodPath: "POST /api/v1/deployments", RequestHash: sha256.Sum256([]byte(body))}
}

func respondDeployment(d Deployment, created bool) (StoredResponse, error) {
	status := 200
	if created {
		status = 201
	}
	b, err := json.Marshal(d)
	return StoredResponse{Status: status, Body: b}, err
}

// 쓰기: 새 행·재생·다른 본문·같은 값 한 행(이름 대소문자, commit 없음 포함)·다른 시각 새 행. 감사·outbox는 새 행만. 권한·환경.
func TestDeploymentRecord(t *testing.T) {
	db := openDB(t)
	a := newTenant(t, db)
	ctx := context.Background()
	s := NewDeploymentStore(db)
	key := deployKey(t, a, nil)
	n := time.Now().UTC().Truncate(time.Microsecond)
	w := DeploymentWrite{ServiceNamespace: "shop", ServiceName: "Checkout", Environment: "prod", Version: "1.4.2", OccurredAt: n.Add(-time.Minute)}
	record := func(p authz.Principal, w DeploymentWrite, k, body string) (DeploymentResult, error) {
		return s.RecordDeployment(ctx, p, w, deployIdem(k, body), "req", n, respondDeployment)
	}
	r1, err := record(key, w, "k1", "b1")
	if err != nil || !r1.Created || r1.Replayed || r1.Stored.Status != 201 {
		t.Fatalf("create: %+v %v", r1, err)
	}
	if r, err := record(key, w, "k1", "b1"); err != nil || !r.Replayed || r.Created || r.Stored.Status != 201 {
		t.Errorf("replay: %+v %v", r, err)
	}
	if _, err := record(key, w, "k1", "other"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("different body: %v", err)
	}
	// 다른 key·같은 값(대소문자만 다름): 기존 행, 감사 없음
	same := w
	same.ServiceName = "CHECKOUT"
	if r, err := record(key, same, "k2", "b2"); err != nil || r.Created || r.Replayed || r.Stored.Status != 200 {
		t.Errorf("same value: %+v %v", r, err)
	}
	// 다른 시각(재배포)은 새 행
	later := w
	later.OccurredAt = n
	if r, err := record(key, later, "k3", "b3"); err != nil || !r.Created {
		t.Errorf("later: %+v %v", r, err)
	}
	var rows, audits, outbox int
	if err := db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM deployments`).Scan(&rows); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'deployment.recorded' AND category = 'operations'`).Scan(&audits); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE type = 'deployment.recorded'`).Scan(&outbox)
	}); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || audits != 2 || outbox != 2 {
		t.Errorf("rows=%d audits=%d outbox=%d, want 2 each", rows, audits, outbox)
	}
	// 권한: Developer 403, environment 제한 key의 다른 환경 403
	if _, err := record(developer(t, a), w, "d", "d"); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("developer: %v", err)
	}
	staging := deployKey(t, a, []string{"staging"})
	if _, err := record(staging, w, "st", "st"); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("other environment: %v", err)
	}
	// 앱 role은 행을 바꿀 수 없다
	err = db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE deployments SET version = 'x'`)
		return err
	})
	if err == nil {
		t.Error("app role must not update deployments")
	}
}

// 한도: 새 행만 센다. 넘으면 ErrDeploymentRateLimited이고 key를 남기지 않는다(나중에 다시 보낼 수 있다). 같은 값은 한도에 막히지 않는다.
func TestDeploymentRateLimit(t *testing.T) {
	db := openDB(t)
	a := newTenant(t, db)
	ctx := context.Background()
	s := NewDeploymentStore(db)
	key := deployKey(t, a, nil)
	n := time.Now().UTC().Truncate(time.Microsecond)
	write := func(i int) DeploymentWrite {
		return DeploymentWrite{ServiceName: "svc", Environment: "prod", Version: fmt.Sprintf("1.0.%d", i), OccurredAt: n}
	}
	for i := 0; i < DeploymentsPerMinute; i++ {
		if _, err := s.RecordDeployment(ctx, key, write(i), deployIdem(fmt.Sprintf("k%d", i), "b"), "r", n, respondDeployment); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecordDeployment(ctx, key, write(1000), deployIdem("over", "b"), "r", n, respondDeployment); !errors.Is(err, ErrDeploymentRateLimited) {
		t.Fatalf("over limit: %v", err)
	}
	if r, err := s.RecordDeployment(ctx, key, write(0), deployIdem("same", "b"), "r", n, respondDeployment); err != nil || r.Stored.Status != 200 {
		t.Errorf("same value at the limit: %+v %v", r, err)
	}
	// 1분 뒤 같은 key로 다시 보낼 수 있다
	if r, err := s.RecordDeployment(ctx, key, write(1000), deployIdem("over", "b"), "r", n.Add(2*time.Minute), respondDeployment); err != nil || !r.Created {
		t.Errorf("after the window: %+v %v", r, err)
	}
}

// 조회: 정렬·keyset·이름 대소문자·namespace·환경 제한·expires_at·다른 tenant. 만료 행은 쓰기 때 그 tenant 것만 지운다.
func TestDeploymentList(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	ctx := context.Background()
	s := NewDeploymentStore(db)
	keyA, keyB := deployKey(t, a, nil), deployKey(t, b, nil)
	n := time.Now().UTC().Truncate(time.Microsecond)
	add := func(p authz.Principal, ns, env, version string, ago time.Duration) {
		t.Helper()
		w := DeploymentWrite{ServiceNamespace: ns, ServiceName: "checkout", Environment: env, Version: version, OccurredAt: n.Add(-ago)}
		if _, err := s.RecordDeployment(ctx, p, w, deployIdem(ns+env+version, "b"), "r", n, respondDeployment); err != nil {
			t.Fatal(err)
		}
	}
	add(keyA, "shop", "prod", "1", 3*time.Hour)
	add(keyA, "shop", "prod", "2", 2*time.Hour)
	add(keyA, "", "staging", "3", time.Hour)
	add(keyB, "shop", "prod", "tenant-b", time.Hour)
	list := func(p authz.Principal, mut func(*DeploymentQuery)) []Deployment {
		t.Helper()
		q := DeploymentQuery{ServiceName: "CheckOut", From: n.Add(-24 * time.Hour), To: n, RecordedBefore: n, Limit: 100}
		if mut != nil {
			mut(&q)
		}
		got, _, err := s.ListDeployments(ctx, p, q, n)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	versions := func(ds []Deployment) string {
		out := ""
		for _, d := range ds {
			out += d.Version + ","
		}
		return out
	}
	if got := versions(list(keyA, nil)); got != "3,2,1," {
		t.Errorf("all = %s", got)
	}
	shop, none, prod := "shop", "", "prod"
	if got := versions(list(keyA, func(q *DeploymentQuery) { q.ServiceNamespace = &shop })); got != "2,1," {
		t.Errorf("namespace = %s", got)
	}
	if got := versions(list(keyA, func(q *DeploymentQuery) { q.ServiceNamespace = &none })); got != "3," {
		t.Errorf("no namespace = %s", got)
	}
	if got := versions(list(keyA, func(q *DeploymentQuery) { q.Environment = &prod })); got != "2,1," {
		t.Errorf("environment = %s", got)
	}
	// keyset
	page, more, err := s.ListDeployments(ctx, keyA, DeploymentQuery{ServiceName: "checkout", From: n.Add(-24 * time.Hour), To: n, RecordedBefore: n, Limit: 1}, n)
	if err != nil || !more || page[0].Version != "3" {
		t.Fatalf("page 1: %+v %v %v", page, more, err)
	}
	if got := versions(list(keyA, func(q *DeploymentQuery) {
		q.After = &DeploymentPosition{OccurredAt: page[0].OccurredAt, ID: page[0].ID}
	})); got != "2,1," {
		t.Errorf("after = %s", got)
	}
	// environment 제한: 생략하면 허용 환경만, 허용 밖 명시는 403
	stagingKey := deployKey(t, a, []string{"staging"})
	if got := versions(list(stagingKey, nil)); got != "3," {
		t.Errorf("restricted = %s", got)
	}
	if _, _, err := s.ListDeployments(ctx, stagingKey, DeploymentQuery{ServiceName: "checkout", Environment: &prod, From: n.Add(-time.Hour), To: n, RecordedBefore: n, Limit: 1}, n); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("out of scope: %v", err)
	}
	// 다른 tenant
	if got := versions(list(keyB, nil)); got != "tenant-b," {
		t.Errorf("tenant B = %s", got)
	}
	// 첫 page 뒤 기록된 행은 읽지 않는다
	if got := versions(list(keyA, func(q *DeploymentQuery) { q.RecordedBefore = n.Add(-time.Second) })); got != "" {
		t.Errorf("snapshot = %s", got)
	}
	// 보존: 만료 시각이 지나면 조회에서 빠지고, 다음 쓰기가 그 tenant 행만 지운다(B의 행은 남는다)
	future := n.Add(DeploymentRetention + 3*time.Hour + time.Minute)
	got, _, err := s.ListDeployments(ctx, keyA, DeploymentQuery{ServiceName: "checkout", From: n.Add(-24 * time.Hour), To: n, RecordedBefore: n, Limit: 10}, future)
	if err != nil || len(got) != 0 {
		t.Errorf("expired rows visible: %+v %v", got, err)
	}
	r, err := s.RecordDeployment(ctx, keyA, DeploymentWrite{ServiceName: "checkout", Environment: "prod", Version: "9", OccurredAt: future}, deployIdem("purge", "b"), "r", future, respondDeployment)
	if err != nil || r.Expired != 3 {
		t.Errorf("purge: %+v %v", r, err)
	}
	if got := versions(list(keyB, nil)); got != "tenant-b," {
		t.Errorf("tenant B after A's purge = %s", got)
	}
}

// RLS 자체(저장소 SQL의 tenant predicate 없이): 다른 tenant 행을 읽거나 지우거나 그 tenant로 넣을 수 없고, tenant context가 없으면 0행이다.
// 표에 ENABLE·FORCE가 켜져 있다(ADR 0016).
func TestDeploymentRLS(t *testing.T) {
	db := openDB(t)
	a, b := newTenant(t, db), newTenant(t, db)
	ctx := context.Background()
	s := NewDeploymentStore(db)
	n := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := s.RecordDeployment(ctx, deployKey(t, a, nil), DeploymentWrite{ServiceName: "svc", Environment: "prod", Version: "1", OccurredAt: n},
		deployIdem("k", "b"), "r", n, respondDeployment); err != nil {
		t.Fatal(err)
	}
	err := db.WithTenant(ctx, b.tenant, func(tx pgx.Tx) error {
		var c int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM deployments`).Scan(&c); err != nil {
			return err
		}
		if c != 0 {
			t.Errorf("B sees %d of A's rows", c)
		}
		tag, err := tx.Exec(ctx, `DELETE FROM deployments`)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("B deleted %d rows", tag.RowsAffected())
		}
		var rowSec, force bool
		if err := tx.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'deployments'`).Scan(&rowSec, &force); err != nil {
			return err
		}
		if !rowSec || !force {
			t.Errorf("row security = %v force = %v", rowSec, force)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// WITH CHECK: B의 context에서 A의 tenant_id로 넣을 수 없다
	err = db.WithTenant(ctx, b.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO deployments (tenant_id, id, service_name, environment, version, occurred_at, recorded_at, expires_at)
			VALUES ($1, gen_random_uuid(), 'x', 'prod', '1', now(), now(), now())`, a.tenant.String())
		return err
	})
	if err == nil {
		t.Error("insert with another tenant_id must fail")
	}
	// tenant context 없는 읽기는 0행이다
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var c int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM deployments`).Scan(&c); err != nil {
		t.Fatal(err)
	}
	if c != 0 {
		t.Errorf("no tenant context sees %d rows", c)
	}
}

// 같은 값을 다른 key로 동시에 보내면 한 행이다(새 행 하나, 나머지는 기존 행). 감사도 하나다.
func TestDeploymentConcurrentSameValue(t *testing.T) {
	db := openDB(t)
	a := newTenant(t, db)
	ctx := context.Background()
	s := NewDeploymentStore(db)
	key := deployKey(t, a, nil)
	n := time.Now().UTC().Truncate(time.Microsecond)
	w := DeploymentWrite{ServiceName: "svc", Environment: "prod", Version: "1", OccurredAt: n}
	const workers = 8
	results := make(chan DeploymentResult, workers)
	errs := make(chan error, workers)
	for i := range workers {
		go func() {
			r, err := s.RecordDeployment(ctx, key, w, deployIdem(fmt.Sprintf("c%d", i), "b"), "r", n, respondDeployment)
			errs <- err
			results <- r
		}()
	}
	created := 0
	for range workers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		r := <-results
		if r.Created {
			created++
		} else if r.Stored.Status != 200 {
			t.Errorf("existing status = %d", r.Stored.Status)
		}
	}
	var rows, audits int
	if err := db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM deployments`).Scan(&rows); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'deployment.recorded'`).Scan(&audits)
	}); err != nil {
		t.Fatal(err)
	}
	if created != 1 || rows != 1 || audits != 1 {
		t.Errorf("created=%d rows=%d audits=%d, want 1 each", created, rows, audits)
	}
}

// 만료 행은 쓰기 하나에 최대 1,000개만 지운다.
func TestDeploymentExpireBatch(t *testing.T) {
	db := openDB(t)
	a := newTenant(t, db)
	ctx := context.Background()
	s := NewDeploymentStore(db)
	n := time.Now().UTC().Truncate(time.Microsecond)
	if err := db.WithTenant(ctx, a.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO deployments (tenant_id, id, service_name, environment, version, occurred_at, recorded_at, expires_at)
			SELECT app_tenant_id(), gen_random_uuid(), 'old', 'prod', i::text, $1::timestamptz - interval '400 days', $1::timestamptz - interval '400 days', $1::timestamptz - interval '1 day'
			FROM generate_series(1, $2::int) AS i`, n, deploymentExpireBatch+5)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r, err := s.RecordDeployment(ctx, deployKey(t, a, nil), DeploymentWrite{ServiceName: "svc", Environment: "prod", Version: "1", OccurredAt: n},
		deployIdem("k", "b"), "r", n, respondDeployment)
	if err != nil || r.Expired != deploymentExpireBatch {
		t.Fatalf("expired = %d %v", r.Expired, err)
	}
}
