// Package controldb는 제어 DB(PostgreSQL) 접근 계층이다 (D02 §11, ADR 0016).
//
// 모든 tenant 범위 작업은 WithTenant를 거친다. 트랜잭션마다 app.tenant_id를
// parameter binding으로 설정하므로 RLS가 다른 tenant의 행을 가린다.
// raw SQL은 이 패키지 안에만 둔다 (D06 §10).
package controldb

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/puddle/v2"

	"github.com/polynomeer/montracer/internal/authz"
)

// DB는 앱 role(montracer_rw 멤버)로 연결한 pool이다.
type DB struct {
	pool *pgxpool.Pool
}

// DB 쪽 실행 예산 (ADR 0016 §3, 설계 가정). 호출자 ctx와 별도로 DB가 스스로 끊는다.
const (
	statementTimeout       = "5s"
	idleInTxSessionTimeout = "30s"
)

// ErrPrivilegedRole은 앱 DSN이 superuser·BYPASSRLS·테이블 owner 계정일 때 반환한다.
// 이런 계정은 FORCE RLS를 무시하거나 우회할 수 있어 tenant 격리가 사라진다 (D02 §11).
var ErrPrivilegedRole = errors.New("controldb: app role must not be superuser, bypassrls, or table owner")

// Open은 pool을 만들고 연결과 role 권한을 확인한다. dsn은 앱 role 계정이어야 한다.
func Open(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// dsn에는 비밀번호가 있으므로 원인 오류 문자열을 그대로 싣지 않는다.
		return nil, errors.New("controldb: invalid dsn")
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "montracer"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = statementTimeout
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = idleInTxSessionTimeout
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, classify("open pool", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, classify("ping", err)
	}
	if err := checkRole(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{pool: pool}, nil
}

// checkRole은 접속 role이 RLS를 무시·우회할 수 없는지 확인한다.
// 제어 테이블의 owner(또는 owner role의 멤버)이면 FORCE RLS가 있어도 ALTER로 끌 수 있으므로 거부한다.
func checkRole(ctx context.Context, pool *pgxpool.Pool) error {
	var privileged bool
	err := pool.QueryRow(ctx, `
		SELECT r.rolsuper OR r.rolbypassrls OR EXISTS (
			SELECT 1 FROM pg_class c
			WHERE c.relnamespace = 'public'::regnamespace
			  AND c.relname IN ('tenants','memberships','api_keys','audit_events','outbox','monitors','monitor_revisions','idempotency_keys')
			  AND pg_has_role(current_user, c.relowner, 'MEMBER'))
		FROM pg_roles r WHERE r.rolname = current_user`).Scan(&privileged)
	if err != nil {
		return classify("check role", err)
	}
	if privileged {
		return ErrPrivilegedRole
	}
	return nil
}

// Close는 pool을 닫는다.
func (db *DB) Close() { db.pool.Close() }

// Ping은 제어 DB 연결을 확인한다(readiness). 장애는 classify로 unavailable이 된다.
func (db *DB) Ping(ctx context.Context) error { return classify("ping", db.pool.Ping(ctx)) }

// WithTenant는 tenant context를 설정한 트랜잭션 안에서 fn을 실행한다.
// fn이 오류를 반환하면 rollback한다. 변경·감사·outbox는 같은 fn 안에서 쓴다 (D02 §11).
func (db *DB) WithTenant(ctx context.Context, tenant authz.TenantID, fn func(pgx.Tx) error) error {
	if tenant.IsZero() {
		return errors.New("controldb: tenant is required")
	}
	return db.inTx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant.String()); err != nil {
			return classify("set tenant", err)
		}
		return fn(tx)
	})
}

func (db *DB) inTx(ctx context.Context, opts pgx.TxOptions, fn func(pgx.Tx) error) error {
	tx, err := db.pool.BeginTx(ctx, opts)
	if err != nil {
		return classify("begin", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return classify("commit", err)
	}
	return nil
}

// unavailableError는 의존 서비스(DB) 장애다. apierr는 Unavailable() 메서드로 503에 매핑한다 (ADR 0014).
type unavailableError struct {
	op  string
	err error
}

func (e *unavailableError) Error() string {
	return "controldb: " + e.op + ": unavailable: " + e.err.Error()
}
func (e *unavailableError) Unwrap() error     { return e.err }
func (e *unavailableError) Unavailable() bool { return true }

// unavailableSQLState는 의존 서비스 장애·일시 상태로 보는 SQLSTATE다 (ADR 0016 §3).
var unavailableSQLState = map[string]bool{
	"53300": true, // too_many_connections
	"57P01": true, // admin_shutdown
	"57P03": true, // cannot_connect_now
	"25006": true, // read_only_sql_transaction: failover 직후 replica에 연결됨
	"57014": true, // query_canceled: statement_timeout 초과(과부하)
	"40001": true, // serialization_failure: 재시도로 해소되는 일시 충돌
	"40P01": true, // deadlock_detected
}

// classify는 연결·가용성 오류만 골라 unavailableError로 감싼다 (503).
// 그 외(제약 위반, scan 타입 불일치, tx 사용 오류 등 코드 결함)는 그대로 wrap해 500이 되게 한다.
// pgconn.PgError.Error()는 값이 들어 있는 Detail을 포함하지 않는다.
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("controldb: %s: %w", op, err)
	}
	if isUnavailable(err) {
		return &unavailableError{op: op, err: err}
	}
	return fmt.Errorf("controldb: %s: %w", op, err)
}

func isUnavailable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return (len(pgErr.Code) == 5 && pgErr.Code[:2] == "08") || unavailableSQLState[pgErr.Code]
	}
	var connectErr *pgconn.ConnectError
	var netErr net.Error
	return errors.As(err, &connectErr) ||
		errors.As(err, &netErr) ||
		errors.Is(err, puddle.ErrClosedPool) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// isUniqueViolation은 unique 제약 위반인지 보고한다.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// newUUID는 RFC 9562 version 4 UUID를 만든다.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("controldb: uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// now는 테스트에서 고정할 수 있게 둔다.
var now = time.Now
