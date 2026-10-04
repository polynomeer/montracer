// migrate는 제어 DB(PostgreSQL) migration을 적용한다 (ADR 0016).
//
//	MONTRACER_MIGRATE_DSN=postgres://owner:...@host:port/db migrate up|down|status
//
// owner 계정으로 실행한다. 앱 role에는 테이블 권한만 GRANT된다.
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

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql 드라이버 "pgx"
	"github.com/pressly/goose/v3"

	"github.com/polynomeer/montracer/migrations"
)

const usage = "usage: migrate up|down|status  (env MONTRACER_MIGRATE_DSN 필요)"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return errors.New(usage)
	}
	dsn := os.Getenv("MONTRACER_MIGRATE_DSN")
	if dsn == "" {
		return errors.New(usage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() { _ = db.Close() }()

	dir, err := fs.Sub(migrations.Postgres, "postgres")
	if err != nil {
		return fmt.Errorf("migrations fs: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, dir)
	if err != nil {
		return fmt.Errorf("provider: %w", err)
	}

	switch args[0] {
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
			fmt.Printf("%05d %-40s %s\n", s.Source.Version, s.Source.Path, applied)
		}
		return nil
	default:
		return errors.New(usage)
	}
}
