// demo는 로컬 개발용 seed·smoke 도구다 (make seed SCENARIO=checkout, make smoke — D06 §04 통합 시나리오, §10~11).
//
//	demo seed  — demo tenant 2개(acme, globex)와 key를 만들고 checkout 시나리오를 실제 ingress로 보낸다
//	             (acme trace는 OTLP/HTTP, globex trace는 OTLP/gRPC — 두 transport를 모두 지난다)
//	demo smoke — 조회·cross-tenant 격리·감사·비샘플링 metric oracle(요청 1,000·오류 20)을 확인한다
//
// localhost 전용이다. ingress·query·control URL과 DB 주소가 localhost가 아니면 거절한다(seed key가 다른 환경에 쓰이지 않게).
// production image에 넣지 않는다(scripts/ 아래, cmd/가 아니다).
//
// 재실행해도 logical 중복이 없다: 시각 기준(anchor)을 상태 파일에 두고 같은 기준으로 같은 ID(trace·span·log uid·metric
// window)를 다시 만든다. 수집 경로의 event_id dedup이 흡수한다. 기준이 수집 허용 범위(과거 24시간)에 가까워지면 새 기준으로
// 새 dataset을 만든다.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

const usage = "usage: demo seed|smoke  (make seed SCENARIO=checkout / make smoke)"

// demo tenant. UUID는 고정이다(재실행 시 같은 tenant).
var demoTenants = []demoTenant{
	{Name: "acme", ID: "7a1b0000-0000-4000-8000-00000000000a", Requests: 1000, Service: "checkout"},
	{Name: "globex", ID: "7a1b0000-0000-4000-8000-00000000000b", Requests: 50, Service: "inventory", Transport: "grpc"},
}

type demoTenant struct {
	Name     string
	ID       string
	Requests int
	Service  string
	// Transport는 trace 전송 경로다("" = OTLP/HTTP, "grpc" = OTLP/gRPC).
	Transport string
}

func transportOf(dt demoTenant) string {
	if dt.Transport == "grpc" {
		return "OTLP/gRPC"
	}
	return "OTLP/HTTP"
}

// state는 seed 결과다(.seed/demo.json, git 밖, 0600). token은 로컬 전용 fake credential이다.
type state struct {
	Anchor  time.Time     `json:"anchor"`
	Tenants []tenantState `json:"tenants"`
}

type tenantState struct {
	Name        string `json:"name"`
	ID          string `json:"id"`
	IngestKeyID string `json:"ingest_key_id"`
	IngestToken string `json:"ingest_token"`
	APIKeyID    string `json:"api_key_id"`
	APIToken    string `json:"api_token"`
}

type config struct {
	pgAdminDSN, pgAppDSN  string
	pepper                []byte
	ingressURL, queryURL  string
	ingressGRPC           string // host:port (OTLP/gRPC)
	controlURL, stateFile string
	scenario              string
}

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "seed" && os.Args[1] != "smoke") {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cfg, err := loadConfig()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if os.Args[1] == "seed" {
			err = seed(ctx, cfg)
		} else {
			err = smoke(ctx, cfg)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

func loadConfig() (config, error) {
	c := config{
		pgAdminDSN: os.Getenv("DEMO_PG_ADMIN_DSN"), pgAppDSN: os.Getenv("DEMO_PG_APP_DSN"),
		ingressURL:  strings.TrimRight(os.Getenv("DEMO_INGRESS_URL"), "/"),
		ingressGRPC: os.Getenv("DEMO_INGRESS_GRPC_ADDR"),
		queryURL:    strings.TrimRight(os.Getenv("DEMO_QUERY_URL"), "/"),
		controlURL:  strings.TrimRight(os.Getenv("DEMO_CONTROL_URL"), "/"),
		stateFile:   os.Getenv("DEMO_STATE_FILE"), scenario: os.Getenv("DEMO_SCENARIO"),
	}
	if c.scenario == "" {
		c.scenario = "checkout"
	}
	if c.scenario != "checkout" {
		return c, fmt.Errorf("unknown SCENARIO %q (checkout only)", c.scenario)
	}
	pepper, err := hex.DecodeString(os.Getenv("DEMO_PEPPER_HEX"))
	if err != nil || len(pepper) < 32 {
		return c, errors.New("DEMO_PEPPER_HEX must be hex of at least 32 bytes (run via make)")
	}
	c.pepper = pepper
	if c.stateFile == "" {
		return c, errors.New("DEMO_STATE_FILE is required (run via make)")
	}
	for name, raw := range map[string]string{"DEMO_PG_ADMIN_DSN": c.pgAdminDSN, "DEMO_PG_APP_DSN": c.pgAppDSN,
		"DEMO_INGRESS_URL": c.ingressURL, "DEMO_QUERY_URL": c.queryURL, "DEMO_CONTROL_URL": c.controlURL,
		"DEMO_INGRESS_GRPC_ADDR": "grpc://" + c.ingressGRPC} {
		if err := requireLocal(name, raw); err != nil {
			return c, err
		}
	}
	return c, nil
}

// requireLocal은 주소가 loopback인지 확인한다. seed key와 demo 데이터가 다른 환경에 가지 않게 한다(D06 §11).
func requireLocal(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s is not a URL/DSN", name)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%s must point to localhost (got host %q) — demo data is local only", name, host)
}

func loadState(path string) (state, bool, error) {
	b, err := os.ReadFile(path) //nolint:gosec // make가 넘긴 로컬 상태 파일 경로
	if errors.Is(err, os.ErrNotExist) {
		return state{}, false, nil
	}
	if err != nil {
		return state{}, false, fmt.Errorf("read state: %w", err)
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return state{}, false, fmt.Errorf("state file %s is corrupt — delete it and re-run make seed", path)
	}
	return s, true, nil
}

func saveState(path string, s state) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("state dir: %w", err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	return os.WriteFile(path, b, 0o600)
}

// provision은 tenant·membership을 만들고(있으면 그대로), 동작하는 key가 없으면 새로 발급한다.
func provision(ctx context.Context, cfg config, prev map[string]tenantState) ([]tenantState, error) {
	admin, err := pgx.Connect(ctx, cfg.pgAdminDSN)
	if err != nil {
		return nil, fmt.Errorf("pg admin connect: %w", err)
	}
	defer func() { _ = admin.Close(ctx) }()
	db, err := controldb.Open(ctx, cfg.pgAppDSN)
	if err != nil {
		return nil, fmt.Errorf("control db: %w", err)
	}
	defer db.Close()
	keys := controldb.NewKeyStore(db)
	hasher, err := authz.NewKeyHasher(cfg.pepper)
	if err != nil {
		return nil, err
	}

	var out []tenantState
	for _, dt := range demoTenants {
		tid, err := authz.ParseTenantID(dt.ID)
		if err != nil {
			return nil, err
		}
		// tenant 생성은 provisioning(owner) 경로만 할 수 있다. membership은 앱 role + RLS로 만든다.
		if _, err := admin.Exec(ctx, `INSERT INTO tenants (id, region, cell, status) VALUES ($1, 'local', 'cell-0', 'active')
			ON CONFLICT (id) DO NOTHING`, dt.ID); err != nil {
			return nil, fmt.Errorf("provision tenant %s: %w", dt.Name, err)
		}
		err = db.WithTenant(ctx, tid, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, 'demo-admin', 'tenant_admin')
				ON CONFLICT (tenant_id, user_id) DO NOTHING`, dt.ID)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("membership %s: %w", dt.Name, err)
		}
		ts := tenantState{Name: dt.Name, ID: dt.ID}
		if p, ok := prev[dt.Name]; ok && p.ID == dt.ID && works(ctx, hasher, keys, p) {
			out = append(out, p) // 기존 key 재사용 — 재실행마다 key가 늘지 않게
			continue
		}
		now := time.Now()
		issuer, err := authz.NewUserPrincipal(tid, "demo-admin", authz.RoleTenantAdmin, now, now)
		if err != nil {
			return nil, err
		}
		ingest, err := issueKey(ctx, hasher, keys, issuer, authz.KindIngestKey, []string{"prod"},
			authz.IngestTraces, authz.IngestLogs, authz.IngestMetrics)
		if err != nil {
			return nil, err
		}
		api, err := issueKey(ctx, hasher, keys, issuer, authz.KindAPIKey, nil, authz.TelemetryRead, authz.AuditRead)
		if err != nil {
			return nil, err
		}
		ts.IngestKeyID, ts.IngestToken, ts.APIKeyID, ts.APIToken = ingest.KeyID, ingest.Token, api.KeyID, api.Token
		out = append(out, ts)
	}
	return out, nil
}

func works(ctx context.Context, h authz.KeyHasher, keys *controldb.KeyStore, s tenantState) bool {
	now := time.Now()
	_, e1 := h.Authenticate(ctx, s.IngestToken, authz.KindIngestKey, keys.LookupKey, now)
	_, e2 := h.Authenticate(ctx, s.APIToken, authz.KindAPIKey, keys.LookupKey, now)
	return e1 == nil && e2 == nil
}

func issueKey(ctx context.Context, h authz.KeyHasher, keys *controldb.KeyStore, issuer authz.Principal, kind authz.Kind,
	envs []string, scopes ...authz.Action) (authz.GeneratedKey, error) {
	iss, err := authz.ValidateKeyIssuance(issuer, kind, scopes, envs)
	if err != nil {
		return authz.GeneratedKey{}, err
	}
	gen, err := h.Generate(kind, nil)
	if err != nil {
		return authz.GeneratedKey{}, err
	}
	// 로컬 demo key는 30일 유효(재실행 시 재사용).
	if err := keys.CreateKey(ctx, iss, gen, time.Now().Add(30*24*time.Hour), "demo-seed"); err != nil {
		return authz.GeneratedKey{}, fmt.Errorf("create key: %w", err)
	}
	return gen, nil
}

// id는 (tenant, anchor, 이름, i)에서 결정적으로 n byte ID를 만든다.
func id(tenant string, anchor time.Time, name string, i, n int) []byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%d", tenant, anchor.Unix(), name, i)))
	return sum[:n]
}
