package telemetrystore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string // unavailable | timeout | budget | other
	}{
		{"too many queries", &clickhouse.Exception{Code: 202}, "unavailable"},
		{"server memory", &clickhouse.Exception{Code: 241}, "unavailable"},
		{"execution timeout", &clickhouse.Exception{Code: 159}, "timeout"},
		{"bytes budget", &clickhouse.Exception{Code: 307}, "budget"},
		{"rows budget", &clickhouse.Exception{Code: 158}, "budget"},
		{"result rows", &clickhouse.Exception{Code: 396}, "budget"},
		{"syntax error is a bug", &clickhouse.Exception{Code: 62}, "other"},
		{"access denied is a bug", &clickhouse.Exception{Code: 497}, "other"},
		{"network", &net.OpError{Op: "read", Err: errors.New("reset")}, "unavailable"},
		{"eof", io.ErrUnexpectedEOF, "unavailable"},
		{"canceled", context.Canceled, "other"},
		{"scan bug", errors.New("converting UInt8 to *string is unsupported"), "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify("op", tc.err)
			var u interface{ Unavailable() bool }
			var to interface{ QueryTimeout() bool }
			var b interface{ BudgetExceeded() map[string]any }
			kind := "other"
			switch {
			case errors.As(got, &u):
				kind = "unavailable"
			case errors.As(got, &to):
				kind = "timeout"
			case errors.As(got, &b):
				kind = "budget"
			}
			if kind != tc.want || !errors.Is(got, tc.err) {
				t.Fatalf("classify = %v (%s), want %s with cause", got, kind, tc.want)
			}
		})
	}
	if classify("op", nil) != nil {
		t.Fatal("classify(nil) must be nil")
	}
}

func TestRangeValidationErrors(t *testing.T) {
	now := time.Now()
	var ia interface{ InvalidArgument() (string, string) }
	if err := (TimeRange{now, now}).validate(time.Hour); !errors.As(err, &ia) {
		t.Errorf("empty range: %v", err)
	}
	var b interface{ BudgetExceeded() map[string]any }
	err := (TimeRange{now.Add(-2 * time.Hour), now}).validate(time.Hour)
	if !errors.As(err, &b) || fmt.Sprint(b.BudgetExceeded()["max_range_seconds"]) != "3600" {
		t.Errorf("over max: %v", err)
	}
}
