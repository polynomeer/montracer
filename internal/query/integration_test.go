//go:build integration

package query

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/ingest"
	"github.com/polynomeer/montracer/internal/pipeline"
	"github.com/polynomeer/montracer/internal/telemetry/envelope"
	"github.com/polynomeer/montracer/internal/telemetry/redact"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// 수직 slice 통합 테스트: OTLP/HTTP → ingress → Kafka → worker → ClickHouse → 조회 API.
//
//	MONTRACER_TEST_PG_APP_DSN, MONTRACER_TEST_PG_ADMIN_DSN, MONTRACER_TEST_KAFKA_BROKERS,
//	MONTRACER_TEST_CH_INGEST_DSN, MONTRACER_TEST_CH_QUERY_DSN

func env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("%s 필요 (make test-integration)", k)
	}
	return v
}

// fixture 시각 (tests/fixtures/otlp/traces_checkout.json의 span 시작 시각)
var fixtureTime = time.Unix(1791158400, 0).UTC()

func TestVerticalSliceIngestToQuery(t *testing.T) {
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 제어 DB: tenant 두 개, 각자의 관리자
	db, err := controldb.Open(ctx, env(t, "MONTRACER_TEST_PG_APP_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	admin, err := pgx.Connect(ctx, env(t, "MONTRACER_TEST_PG_ADMIN_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })
	keys := controldb.NewKeyStore(db)
	hasher, _ := authz.NewKeyHasher(bytes.Repeat([]byte{9}, 32))
	newTenant := func() (authz.TenantID, authz.Principal) {
		var id string
		if err := admin.QueryRow(ctx, `INSERT INTO tenants (id, region, cell, status) VALUES (gen_random_uuid(), 'local', 'cell-0', 'active') RETURNING id::text`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		tenant, _ := authz.ParseTenantID(id)
		if err := db.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, 'admin', 'tenant_admin')`, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		n := time.Now()
		p, _ := authz.NewUserPrincipal(tenant, "admin", authz.RoleTenantAdmin, n, n)
		return tenant, p
	}
	issue := func(issuer authz.Principal, kind authz.Kind, scopes []authz.Action, envs []string) string {
		iss, err := authz.ValidateKeyIssuance(issuer, kind, scopes, envs)
		if err != nil {
			t.Fatal(err)
		}
		g, _ := hasher.Generate(kind, nil)
		if err := keys.CreateKey(ctx, iss, g, time.Now().Add(time.Hour), "req_it"); err != nil {
			t.Fatal(err)
		}
		return g.Token
	}
	tenantA, adminA := newTenant()
	_, adminB := newTenant()
	ingestKey := issue(adminA, authz.KindIngestKey, []authz.Action{authz.IngestTraces}, []string{"production"})
	apiKeyA := issue(adminA, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	apiKeyAStaging := issue(adminA, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, []string{"staging"})
	apiKeyB := issue(adminB, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)

	// worker: 이번 테스트가 쓴 record부터 읽도록 새 group의 offset을 현재 끝으로 둔다
	brokers := strings.Split(env(t, "MONTRACER_TEST_KAFKA_BROKERS"), ",")
	kcl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kcl.Close)
	adm := kadm.NewClient(kcl)
	group := fmt.Sprintf("it-slice-%d", time.Now().UnixNano())
	ends, err := adm.ListEndOffsets(ctx, envelope.TopicTraces, envelope.TopicLogs, envelope.TopicMetrics)
	if err != nil {
		t.Fatal(err)
	}
	if err := adm.CommitAllOffsets(ctx, group, ends.Offsets()); err != nil {
		t.Fatal(err)
	}
	sink, err := pipeline.OpenClickHouseSink(ctx, env(t, "MONTRACER_TEST_CH_INGEST_DSN"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	worker, err := pipeline.NewWorker(pipeline.Config{Brokers: brokers, Group: group, Sink: sink, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	t.Cleanup(func() {
		stop()
		<-done
		worker.Close()
	})

	// ingress: 같은 fixture를 두 번 보낸다(client 재전송)
	producer, err := ingest.NewKafkaProducer(brokers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(producer.Close)
	ingress, err := ingest.NewHandler(ingest.Config{
		Authenticate: func(ctx context.Context, token string) (authz.Principal, error) {
			return hasher.Authenticate(ctx, token, authz.KindIngestKey, keys.LookupKey, time.Now())
		},
		Producer: producer, Redactor: redact.New(redact.DefaultPolicy), RoutingEpoch: 1, Logger: quiet,
		Now: func() time.Time { return fixtureTime.Add(time.Second) },
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "otlp", "traces_checkout.json"))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/traces", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+ingestKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		ingress.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("ingest status = %d body=%s", rec.Code, rec.Body)
		}
	}

	// 조회 API
	store, err := telemetrystore.OpenQuery(ctx, env(t, "MONTRACER_TEST_CH_QUERY_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	api, err := NewHandler(Config{
		Authenticate: func(ctx context.Context, token string) (authz.Principal, error) {
			return hasher.Authenticate(ctx, token, authz.KindAPIKey, keys.LookupKey, time.Now())
		},
		Store: store, Logger: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	rng := "?from=" + fixtureTime.Add(-time.Minute).Format(time.RFC3339) + "&to=" + fixtureTime.Add(time.Minute).Format(time.RFC3339)
	query := func(token, extra string) (int, traceResponse) {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/traces/"+traceID+rng+extra, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		var out traceResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// worker가 저장할 때까지 기다린다 (eventually consistent, D02 §12)
	var tr traceResponse
	deadline := time.Now().Add(60 * time.Second)
	for {
		code, out := query(apiKeyA, "")
		if code == http.StatusOK && out.Data.SpanCount == 4 {
			tr = out
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("trace not visible in time: status=%d spans=%d", code, out.Data.SpanCount)
		}
		time.Sleep(300 * time.Millisecond)
	}
	// 재전송했어도 span은 4개, 구조 완결
	if !tr.Data.Complete || len(tr.Data.Reasons) != 0 {
		t.Errorf("complete=%v reasons=%v", tr.Data.Complete, tr.Data.Reasons)
	}
	services := map[string]bool{}
	for _, s := range tr.Data.Spans {
		services[s.ServiceName] = true
		if s.Environment == nil || *s.Environment != "production" {
			t.Errorf("environment = %v", s.Environment)
		}
	}
	if !services["checkout"] || !services["payment"] || tr.Data.Spans[0].Name != "POST /checkout" {
		t.Errorf("spans = %+v", tr.Data.Spans)
	}

	// service_id 필터: payment의 span만 → root·parent가 보이지 않는다
	res := pcommon.NewMap()
	res.PutStr("service.name", "payment")
	res.PutStr("service.namespace", "shop")
	res.PutStr("deployment.environment.name", "production")
	code, onlyPayment := query(apiKeyA, "&service_id="+pipeline.ServiceID(tenantA, res))
	if code != http.StatusOK || onlyPayment.Data.SpanCount != 2 ||
		strings.Join(onlyPayment.Data.Reasons, ",") != ReasonMissingParent+","+ReasonMissingRoot {
		t.Errorf("service filter: status=%d spans=%d reasons=%v", code, onlyPayment.Data.SpanCount, onlyPayment.Data.Reasons)
	}

	// 다른 tenant는 같은 trace ID라도 404 (존재 비노출, row policy + mandatory predicate)
	if code, _ := query(apiKeyB, ""); code != http.StatusNotFound {
		t.Errorf("tenant B status = %d, want 404", code)
	}
	// environment 범위 밖 key도 404
	if code, _ := query(apiKeyAStaging, ""); code != http.StatusNotFound {
		t.Errorf("staging key status = %d, want 404", code)
	}
	// ingest key로는 조회할 수 없다 (D02 §12)
	if code, _ := query(ingestKey, ""); code != http.StatusUnauthorized {
		t.Errorf("ingest key status = %d, want 401", code)
	}
}
