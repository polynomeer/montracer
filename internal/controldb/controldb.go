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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/polynomeer/montracer/internal/authz"
)

// DB는 앱 role(montracer_rw 멤버)로 연결한 pool이다.
type DB struct {
	pool *pgxpool.Pool
}

// Open은 pool을 만들고 연결을 확인한다. dsn은 앱 role 계정이어야 한다 (owner·superuser 금지).
func Open(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// dsn에는 비밀번호가 있으므로 원인 오류 문자열을 그대로 싣지 않는다.
		return nil, errors.New("controldb: invalid dsn")
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "montracer"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, classify("open pool", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, classify("ping", err)
	}
	return &DB{pool: pool}, nil
}

// Close는 pool을 닫는다.
func (db *DB) Close() { db.pool.Close() }

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

// classify는 연결·가용성 오류를 unavailableError로 감싼다.
// SQL 오류(제약 위반 등)는 그대로 wrap한다. pgconn.PgError.Error()는 값이 들어 있는 Detail을 포함하지 않는다.
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("controldb: %s: %w", op, err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case len(pgErr.Code) == 5 && pgErr.Code[:2] == "08", // connection exception
			pgErr.Code == "53300", // too_many_connections
			pgErr.Code == "57P01", // admin_shutdown
			pgErr.Code == "57P03": // cannot_connect_now
			return &unavailableError{op: op, err: err}
		}
		return fmt.Errorf("controldb: %s: %w", op, err)
	}
	// 서버 응답이 아닌 오류는 네트워크·pool 장애다.
	return &unavailableError{op: op, err: err}
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
