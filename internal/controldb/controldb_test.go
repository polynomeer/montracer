package controldb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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
		{"unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"rls / permission", &pgconn.PgError{Code: "42501"}, false},
		{"network", io.ErrUnexpectedEOF, true},
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
