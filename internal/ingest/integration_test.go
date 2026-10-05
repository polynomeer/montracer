//go:build integration

package ingest

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/telemetry/envelope"
	"github.com/polynomeer/montracer/internal/telemetry/redact"
)

// 통합 테스트: 실제 PostgreSQL(ingest key)과 Kafka(topic은 cmd/migrate kafka up으로 생성).
//
//	MONTRACER_TEST_PG_APP_DSN, MONTRACER_TEST_PG_ADMIN_DSN, MONTRACER_TEST_KAFKA_BROKERS

func env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("%s 필요 (make test-integration)", k)
	}
	return v
}

type e2e struct {
	handler *Handler
	store   *controldb.KeyStore
	admin   authz.Principal
	tenant  authz.TenantID
	hasher  authz.KeyHasher
	brokers []string
}

func newE2E(t *testing.T, producer Producer) e2e {
	t.Helper()
	ctx := context.Background()
	db, err := controldb.Open(ctx, env(t, "MONTRACER_TEST_PG_APP_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	adminConn, err := pgx.Connect(ctx, env(t, "MONTRACER_TEST_PG_ADMIN_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminConn.Close(ctx) })
	var id string
	if err := adminConn.QueryRow(ctx, `INSERT INTO tenants (id, region, cell, status) VALUES (gen_random_uuid(), 'local', 'cell-0', 'active') RETURNING id::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	tenant, _ := authz.ParseTenantID(id)
	if err := db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, 'admin', 'tenant_admin')`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	admin, _ := authz.NewUserPrincipal(tenant, "admin", authz.RoleTenantAdmin, now, now)
	hasher, _ := authz.NewKeyHasher(bytes.Repeat([]byte{5}, 32))
	store := controldb.NewKeyStore(db)
	brokers := strings.Split(env(t, "MONTRACER_TEST_KAFKA_BROKERS"), ",")
	if producer == nil {
		p, err := NewKafkaProducer(brokers)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		producer = p
	}
	h, err := NewHandler(Config{
		Authenticate: func(ctx context.Context, token string) (authz.Principal, error) {
			return hasher.Authenticate(ctx, token, authz.KindIngestKey, store.LookupKey, time.Now())
		},
		Producer:     producer,
		Redactor:     redact.New(redact.DefaultPolicy),
		RoutingEpoch: 1,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:          func() time.Time { return fixtureTime.Add(time.Second) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return e2e{handler: h, store: store, admin: admin, tenant: tenant, hasher: hasher, brokers: brokers}
}

func (e e2e) issue(t *testing.T) authz.GeneratedKey {
	t.Helper()
	iss, err := authz.ValidateKeyIssuance(e.admin, authz.KindIngestKey, allSignals, []string{"production"})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := e.hasher.Generate(authz.KindIngestKey, nil)
	if err := e.store.CreateKey(context.Background(), iss, g, time.Now().Add(time.Hour), "req_it"); err != nil {
		t.Fatal(err)
	}
	return g
}

func (e e2e) post(token string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// endOffsets는 topic의 현재 끝 offset이다 (이번 테스트가 쓴 record만 읽기 위해).
func endOffsets(t *testing.T, brokers []string, topic string) map[int32]kgo.Offset {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ends, err := kadm.NewClient(cl).ListEndOffsets(context.Background(), topic)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int32]kgo.Offset{}
	ends.Each(func(o kadm.ListedOffset) { out[o.Partition] = kgo.NewOffset().At(o.Offset) })
	return out
}

func consume(t *testing.T, brokers []string, topic string, from map[int32]kgo.Offset, tenant string, want int) []*kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{topic: from}))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var got []*kgo.Record
	for len(got) < want {
		fs := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			break
		}
		fs.EachRecord(func(r *kgo.Record) {
			for _, h := range r.Headers {
				if h.Key == envelope.HeaderTenant && string(h.Value) == tenant {
					got = append(got, r)
				}
			}
		})
	}
	return got
}

func TestEndToEndIngestToKafka(t *testing.T) {
	e := newE2E(t, nil)
	key := e.issue(t)
	from := endOffsets(t, e.brokers, envelope.TopicTraces)
	rec := e.post(key.Token, fixture(t, "otlp", "traces_checkout.json"))
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	got := consume(t, e.brokers, envelope.TopicTraces, from, e.tenant.String(), 4)
	if len(got) != 4 {
		t.Fatalf("consumed %d records, want 4", len(got))
	}
	partitions := map[int32]bool{}
	tenantHex := strings.ReplaceAll(e.tenant.String(), "-", "")
	for _, r := range got {
		partitions[r.Partition] = true
		// key = tenant(16B) + trace_id(16B): 앞 16B가 인증된 tenant여야 한다
		if len(r.Key) != 32 || hex.EncodeToString(r.Key[:16]) != tenantHex {
			t.Errorf("key = %x, want tenant prefix %s", r.Key, tenantHex)
		}
	}
	// 같은 trace의 span은 같은 partition (hash(tenant, trace_id), D02 §05)
	if len(partitions) != 1 {
		t.Errorf("spans of one trace spread over %d partitions", len(partitions))
	}

	// 재전송: 같은 event_id가 다시 기록된다 (at-least-once, dedup은 worker 책임)
	rec = e.post(key.Token, fixture(t, "otlp", "traces_checkout.json"))
	if rec.Code != 200 {
		t.Fatalf("retry status = %d", rec.Code)
	}
	got2 := consume(t, e.brokers, envelope.TopicTraces, from, e.tenant.String(), 8)
	ids := map[string]int{}
	for _, r := range got2 {
		for _, h := range r.Headers {
			if h.Key == envelope.HeaderEventID {
				ids[string(h.Value)]++
			}
		}
	}
	if len(ids) != 4 {
		t.Fatalf("distinct event ids = %d, want 4 (retry must reuse ids)", len(ids))
	}
}

func TestRevokedKeyRejected(t *testing.T) {
	e := newE2E(t, nil)
	key := e.issue(t)
	if err := e.store.RevokeKey(context.Background(), e.admin, key.KeyID, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if rec := e.post(key.Token, fixture(t, "otlp", "traces_checkout.json")); rec.Code != 401 {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// broker에 닿지 못하면 ACK하지 않는다 (ADR 0002).
func TestUnreachableKafkaIsNotAcked(t *testing.T) {
	p, err := NewKafkaProducer([]string{"127.0.0.1:1"}, kgo.RecordDeliveryTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	e := newE2E(t, p)
	key := e.issue(t)
	rec := e.post(key.Token, fixture(t, "otlp", "traces_checkout.json"))
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d, want 503 with Retry-After", rec.Code)
	}
}
