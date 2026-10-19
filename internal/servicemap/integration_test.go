//go:build integration

package servicemap

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// 통합 테스트: ClickHouse service map 계정(migration 00006) → service_edges_1m → query 계정.
//
//	MONTRACER_TEST_CH_SERVICEMAP_DSN, MONTRACER_TEST_CH_INGEST_DSN, MONTRACER_TEST_CH_QUERY_DSN, MONTRACER_TEST_CH_ROLLUP_DSN

func env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("%s 필요 (make test-integration)", k)
	}
	return v
}

func randomUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6], b[8] = (b[6]&0x0f)|0x40, (b[8]&0x3f)|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randomBytes(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return string(b)
}

type span struct {
	tenant, service   string
	trace, id, parent string // trace 16바이트, id·parent 8바이트(parent ""이면 root)
	kind, status      uint8
	start             time.Time
	duration          time.Duration
	attrs             map[string]string
}

// insert는 ingest 계정으로 span을 쓴다(실제 수집 경로와 같은 권한).
func insert(t *testing.T, spans ...span) {
	t.Helper()
	ctx := context.Background()
	opts, err := clickhouse.ParseDSN(env(t, "MONTRACER_TEST_CH_INGEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO spans_local (tenant_id, service_id, trace_id, span_id, parent_span_id, name,
		event_time, duration_ns, received_at, status, span_kind, attributes, payload, payload_hash, version, expires_at)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range spans {
		parent := s.parent
		if parent == "" {
			parent = string(make([]byte, 8))
		}
		attrs := s.attrs
		if attrs == nil {
			attrs = map[string]string{}
		}
		if err := batch.Append(s.tenant, s.service, s.trace, s.id, parent, "op", s.start, uint64(s.duration), s.start, //nolint:gosec // 시험 span 길이는 양수
			s.status, s.kind,
			attrs, "{}", string(make([]byte, 32)), uint64(1), s.start.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatal(err)
	}
}

func principal(t *testing.T, tenant string) authz.Principal {
	t.Helper()
	tid, err := authz.ParseTenantID(tenant)
	if err != nil {
		t.Fatal(err)
	}
	p, err := authz.NewUserPrincipal(tid, "viewer", authz.RoleViewer, time.Time{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEdgeRollupEndToEnd(t *testing.T) {
	ctx := context.Background()
	store, err := OpenClickHouse(ctx, env(t, "MONTRACER_TEST_CH_SERVICEMAP_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	query, err := telemetrystore.OpenQuery(ctx, env(t, "MONTRACER_TEST_CH_QUERY_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = query.Close() })

	tenantA, tenantB := randomUUID(t), randomUUID(t)
	checkout, payment, ledger := randomUUID(t), randomUUID(t), randomUUID(t)
	now := time.Now().UTC().Truncate(time.Second)
	w := now.Add(-5 * time.Minute).Truncate(time.Minute) // 닫힌 window
	at := func(sec int) time.Time { return w.Add(time.Duration(sec) * time.Second) }

	t1, t2, t3, t4 := randomBytes(16), randomBytes(16), randomBytes(16), randomBytes(16)
	root, call, srv := randomBytes(8), randomBytes(8), randomBytes(8)
	internalSpan, fallbackSrv := randomBytes(8), randomBytes(8)
	dbCall, produce, consume, ipCall, late := randomBytes(8), randomBytes(8), randomBytes(8), randomBytes(8), randomBytes(8)
	clientCall := span{tenant: tenantA, service: checkout, trace: t1, id: call, parent: root, kind: KindClient, start: at(1), duration: 30 * time.Millisecond}
	insert(t,
		// t1: checkout client → payment server (observed). client 행이 두 번 들어와도(merge 전 중복) 한 번 센다
		span{tenant: tenantA, service: checkout, trace: t1, id: root, kind: KindServer, start: at(0), duration: 50 * time.Millisecond},
		clientCall, clientCall,
		span{tenant: tenantA, service: payment, trace: t1, id: srv, parent: call, kind: KindServer, status: 2, start: at(2), duration: 20 * time.Millisecond},
		// t1: payment → postgres (계측되지 않은 DB)
		span{tenant: tenantA, service: payment, trace: t1, id: dbCall, parent: srv, kind: KindClient, start: at(3), duration: 4 * time.Millisecond,
			attrs: map[string]string{"db.system.name": "postgresql", "db.namespace": "orders"}},
		// t2: client span 없이 checkout internal → payment server (fallback)
		span{tenant: tenantA, service: checkout, trace: t2, id: internalSpan, kind: 1, start: at(10), duration: 40 * time.Millisecond},
		span{tenant: tenantA, service: payment, trace: t2, id: fallbackSrv, parent: internalSpan, kind: KindServer, start: at(11), duration: 600 * time.Millisecond},
		// t3: checkout producer → ledger consumer, 큐 대기 = 100ms
		span{tenant: tenantA, service: checkout, trace: t3, id: produce, kind: KindProducer, start: at(20), duration: 2 * time.Millisecond,
			attrs: map[string]string{"messaging.system": "kafka"}},
		span{tenant: tenantA, service: ledger, trace: t3, id: consume, parent: produce, kind: KindConsumer,
			start: at(20).Add(102 * time.Millisecond), duration: 8 * time.Millisecond},
		// t4: IP로 부른 외부 호출 — IP를 저장하지 않는다
		span{tenant: tenantA, service: checkout, trace: t4, id: ipCall, kind: KindClient, start: at(30), duration: time.Millisecond,
			attrs: map[string]string{"server.address": "10.1.2.3"}},
		// t4: 짝이 아직 오지 않은 호출(내부 host 이름) — 처음엔 peer, 늦은 server span 뒤엔 service edge
		span{tenant: tenantA, service: checkout, trace: t4, id: late, kind: KindClient, start: at(31), duration: time.Millisecond,
			attrs: map[string]string{"server.address": "payment.shop.svc.cluster.local"}},
		// tenant B: 같은 서비스 id로 부른 호출. A에게 보이면 안 된다
		span{tenant: tenantB, service: checkout, trace: t1, id: randomBytes(8), kind: KindClient, start: at(1), duration: time.Millisecond,
			attrs: map[string]string{"peer.service": "b-secret"}},
		// watermark용 최신 span (tenant마다)
		span{tenant: tenantA, service: checkout, trace: randomBytes(16), id: randomBytes(8), kind: 1, start: now.Add(-time.Second), duration: time.Millisecond},
		span{tenant: tenantB, service: checkout, trace: randomBytes(16), id: randomBytes(8), kind: 1, start: now.Add(-time.Second), duration: time.Millisecond},
	)

	clock := now
	j := New(Config{Store: store, Logger: quiet(), Now: func() time.Time { return clock }})
	if err := j.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	read := func(tenant string) map[string]telemetrystore.ServiceEdge {
		t.Helper()
		edges, truncated, err := query.ServiceEdges(ctx, principal(t, tenant),
			telemetrystore.ServiceEdgeQuery{Range: telemetrystore.TimeRange{From: w.Add(-time.Minute), To: now}}, now)
		if err != nil || truncated {
			t.Fatalf("ServiceEdges: %v truncated=%v", err, truncated)
		}
		out := map[string]telemetrystore.ServiceEdge{}
		for _, e := range edges {
			out[e.Source+">"+e.Destination+">"+e.Connection] = e
		}
		return out
	}
	edges := read(tenantA)
	rpc := edges[checkout+">"+payment+">rpc"]
	// client 기준 1건(중복 행 무시, server 오류 포함) + fallback 1건(600ms)
	if rpc.Requests != 2 || rpc.Errors != 1 || rpc.DestinationKind != "service" {
		t.Fatalf("checkout → payment = %+v (all: %v)", rpc, edges)
	}
	if rpc.Buckets[4] != 1 || rpc.Buckets[9] != 1 { // client가 본 30ms는 (25, 50], fallback 600ms는 (500, 750]
		t.Errorf("latency buckets = %v", rpc.Buckets)
	}
	if e := edges[payment+">db:postgresql/orders>rpc"]; e.Requests != 1 || e.DestinationKind != "database" {
		t.Errorf("payment → db = %+v", e)
	}
	msg := edges[checkout+">"+ledger+">messaging"]
	// 처리 8ms는 (5, 10], 대기 100ms(consumer 시작 − producer 끝)는 (75, 100]
	if msg.Requests != 1 || msg.Buckets[2] != 1 || msg.WaitCount != 1 || msg.WaitBuckets[6] != 1 {
		t.Errorf("messaging = %+v", msg)
	}
	if e := edges[checkout+">host:ip>rpc"]; e.Requests != 1 {
		t.Errorf("ip edge = %+v", e)
	}
	if e := edges[checkout+">peer:host/payment>rpc"]; e.Requests != 1 {
		t.Errorf("unmatched call = %+v", e)
	}
	for k, e := range edges {
		if e.DestinationKind == DestWindow {
			t.Errorf("marker row returned as an edge: %s", k)
		}
		if bytes.Contains([]byte(k), []byte("10.1.2.3")) || bytes.Contains([]byte(k), []byte("b-secret")) {
			t.Errorf("edge key leaks host or another tenant: %s", k)
		}
	}
	if b := read(tenantB); len(b) != 1 || b[checkout+">peer:b-secret>rpc"].Requests != 1 {
		t.Errorf("tenant B edges = %v", b)
	}

	// 늦은 server span: 같은 window가 새 revision으로 다시 쓰이고, 옛 peer 행은 보이지 않는다(이중 계수 없음)
	insert(t, span{tenant: tenantA, service: payment, trace: t4, id: randomBytes(8), parent: late, kind: KindServer, start: at(32), duration: time.Millisecond})
	clock = now.Add(30 * time.Second)
	if err := j.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	edges = read(tenantA)
	if _, ok := edges[checkout+">peer:host/payment>rpc"]; ok {
		t.Errorf("superseded peer edge still visible: %v", edges)
	}
	if e := edges[checkout+">"+payment+">rpc"]; e.Requests != 3 {
		t.Errorf("checkout → payment after late span = %+v", e)
	}

	// 재시작한 job은 저장된 진행 위치·revision을 이어받는다(같은 내용이면 새 revision이 앞 revision보다 크다)
	j2 := New(Config{Store: store, Logger: quiet(), Now: func() time.Time { return clock }})
	if err := j2.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if e := read(tenantA)[checkout+">"+payment+">rpc"]; e.Requests != 3 {
		t.Errorf("after restart = %+v", e)
	}
	wm, err := query.ServiceEdgeWatermark(ctx, principal(t, tenantA), w.Add(-time.Hour), now)
	if err != nil || wm.Before(w.Add(time.Minute)) {
		t.Errorf("watermark = %v %v", wm, err)
	}
}

// service map 계정은 span payload·log·metric을 읽지 못하고, query 계정 정책 밖의 행도 쓰지 못한다.
func TestServiceMapAccountIsNarrow(t *testing.T) {
	ctx := context.Background()
	opts, err := clickhouse.ParseDSN(env(t, "MONTRACER_TEST_CH_SERVICEMAP_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	for _, q := range []string{
		`SELECT payload FROM spans_local LIMIT 1`,
		`SELECT name FROM spans_local LIMIT 1`,
		`SELECT count() FROM logs_local`,
		`SELECT count() FROM metric_1m`,
		`SELECT requests FROM service_edges_1m LIMIT 1`,
		`INSERT INTO spans_local (tenant_id) VALUES (generateUUIDv4())`,
	} {
		err := conn.Exec(ctx, q)
		var ex *clickhouse.Exception
		if !errors.As(err, &ex) || ex.Code != 497 {
			t.Errorf("%s: err = %v, want ACCESS_DENIED", q, err)
		}
	}
	// metric rollup 계정(metric을 읽는다)은 service map store로 열리지 않는다
	if _, err := OpenClickHouse(ctx, env(t, "MONTRACER_TEST_CH_ROLLUP_DSN")); err == nil {
		t.Error("rollup account opened as service map store")
	}
}
