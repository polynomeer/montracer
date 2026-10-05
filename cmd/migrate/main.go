// migrate는 DB migration을 적용한다 (ADR 0016, 0018).
//
//	migrate [postgres|clickhouse] up|down|status
//
//	postgres   env MONTRACER_MIGRATE_DSN     postgres://owner:...@host:port/db
//	clickhouse env MONTRACER_MIGRATE_CH_DSN  clickhouse://admin:...@host:port/db
//
// target을 생략하면 postgres다. owner(관리자) 계정으로 실행한다. 앱 계정에는 권한만 GRANT된다.
// down은 한 단계만 되돌린다. 비가역 migration은 backup·restore 검증 후 실행한다 (D06 §07).
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2" // database/sql 드라이버 "clickhouse"
	_ "github.com/jackc/pgx/v5/stdlib"         // database/sql 드라이버 "pgx"
	"github.com/pressly/goose/v3"

	"github.com/polynomeer/montracer/migrations"
)

const usage = "usage: migrate [postgres|clickhouse] up|down|status  (env MONTRACER_MIGRATE_DSN / MONTRACER_MIGRATE_CH_DSN)"

type target struct {
	driver  string
	dialect goose.Dialect
	dsnEnv  string
	files   fs.FS
	dir     string
}

var targets = map[string]target{
	"postgres":   {driver: "pgx", dialect: goose.DialectPostgres, dsnEnv: "MONTRACER_MIGRATE_DSN", files: migrations.Postgres, dir: "postgres"},
	"clickhouse": {driver: "clickhouse", dialect: goose.DialectClickHouse, dsnEnv: "MONTRACER_MIGRATE_CH_DSN", files: migrations.ClickHouse, dir: "clickhouse"},
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	name, cmd := "postgres", ""
	switch len(args) {
	case 1:
		cmd = args[0]
	case 2:
		name, cmd = args[0], args[1]
	default:
		return errors.New(usage)
	}
	t, ok := targets[name]
	if !ok {
		return errors.New(usage)
	}
	dsn := os.Getenv(t.dsnEnv)
	if dsn == "" {
		return errors.New(usage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	db, err := sql.Open(t.driver, dsn)
	if err != nil {
		// DSN에는 비밀번호가 있으므로 원인 문자열을 싣지 않는다.
		return errors.New("open: invalid dsn")
	}
	defer func() { _ = db.Close() }()

	dir, err := fs.Sub(t.files, t.dir)
	if err != nil {
		return fmt.Errorf("migrations fs: %w", err)
	}
	p, err := goose.NewProvider(t.dialect, db, dir)
	if err != nil {
		return fmt.Errorf("provider: %w", err)
	}

	switch cmd {
	case "up":
		results, err := p.Up(ctx)
		for _, r := range results {
			fmt.Println(r)
		}
		return err
	case "down":
		r, err := p.Down(ctx)
		if r != nil {
			fmt.Println(r)
		}
		return err
	case "status":
		statuses, err := p.Status(ctx)
		if err != nil {
			return err
		}
		for _, s := range statuses {
			applied := "pending"
			if s.State == goose.StateApplied {
				applied = "applied " + s.AppliedAt.UTC().Format(time.RFC3339)
			}
			fmt.Printf("%-10s %05d %-40s %s\n", name, s.Source.Version, s.Source.Path, applied)
		}
		return nil
	default:
		return errors.New(usage)
	}
}
