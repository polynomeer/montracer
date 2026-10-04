package controldb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		unavailable bool
	}{
		{"nil", nil, false},
		{"connection exception", &pgconn.PgError{Code: "08006"}, true},
		{"too many connections", &pgconn.PgError{Code: "53300"}, true},
		{"admin shutdown", &pgconn.PgError{Code: "57P01"}, true},
		{"failover read-only", &pgconn.PgError{Code: "25006"}, true},
		{"statement timeout", &pgconn.PgError{Code: "57014"}, true},
		{"serialization", &pgconn.PgError{Code: "40001"}, true},
		{"deadlock", &pgconn.PgError{Code: "40P01"}, true},
		{"unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"rls / permission", &pgconn.PgError{Code: "42501"}, false},
		{"network eof", io.ErrUnexpectedEOF, true},
		{"net error", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"closed pool", puddle.ErrClosedPool, true},
		// 코드 결함은 503으로 가리지 않는다.
		{"tx misuse", pgx.ErrTxClosed, false},
		{"scan mismatch", errors.New("can't scan into dest[0]"), false},
		{"canceled", context.Canceled, false},
		{"deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify("op", tc.err)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("classify(nil) = %v", got)
				}
				return
			}
			var u interface{ Unavailable() bool }
			isU := errors.As(got, &u) && u.Unavailable()
			if isU != tc.unavailable {
				t.Fatalf("unavailable = %v, want %v (%v)", isU, tc.unavailable, got)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("cause lost: %v", got)
			}
		})
	}
}

func TestNewUUID(t *testing.T) {
	v4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for range 100 {
		id, err := newUUID()
		if err != nil {
			t.Fatal(err)
		}
		if !v4.MatchString(id) || seen[id] {
			t.Fatalf("bad or duplicate uuid %q", id)
		}
		seen[id] = true
	}
}

func TestOpenDoesNotLeakDSN(t *testing.T) {
	_, err := Open(context.Background(), "postgres://user:s3cret-pw@[bad")
	if err == nil {
		t.Fatal("want error")
	}
	if regexp.MustCompile(`s3cret`).MatchString(err.Error()) {
		t.Fatalf("dsn leaked: %v", err)
	}
}

// app.key_lookup은 RLS 한 행 정책을 여는 GUC다. LookupKey 한 곳에서만 설정해야 한다 (ADR 0016 §4).
// 다른 코드가 tenant 트랜잭션 안에서 설정하면 다른 tenant의 key 행이 열린다.
func TestKeyLookupGUCOnlyInLookupKey(t *testing.T) {
	// 저장소 루트를 fs.FS로 열어 읽는다 (경로 탈출 없음).
	fsys := os.DirFS(filepath.Join("..", ".."))
	var hits []string
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git") {
			return fs.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		if n := strings.Count(string(b), "'app.key_lookup'"); n > 0 {
			hits = append(hits, fmt.Sprintf("%s:%d", path, n))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.HasSuffix(hits[0], "internal/controldb/keys.go:1") {
		t.Fatalf("app.key_lookup must be set exactly once, in keys.go LookupKey; found %v", hits)
	}
}
