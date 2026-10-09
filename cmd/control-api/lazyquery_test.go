package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// ClickHouse가 닿지 않으면 오류를 돌려주고 retryAfter 동안 다시 시도하지 않는다. 그 뒤에는 다시 시도한다.
func TestLazyQueryRetry(t *testing.T) {
	down := errors.New("dial refused")
	attempts := 0
	l := newLazyQuery("clickhouse://x", slog.Default())
	l.retryAfter = 50 * time.Millisecond
	l.open = func(context.Context, string) (*telemetrystore.QueryStore, error) {
		attempts++
		return nil, down
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := l.RollupWatermark(ctx, authz.Principal{}, time.Minute, time.Time{}, time.Now()); !errors.Is(err, down) {
			t.Fatalf("err = %v", err)
		}
	}
	if attempts != 1 {
		t.Errorf("attempts within retry window = %d", attempts)
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := l.MetricBuckets(ctx, authz.Principal{}, telemetrystore.MetricQuery{}, time.Now()); !errors.Is(err, down) || attempts != 2 {
		t.Errorf("after retry window: attempts=%d err=%v", attempts, err)
	}
	// 읽기 전용이 아닌 계정도 오류로 남는다(기동 때는 main이 거부한다)
	l.next = time.Time{}
	l.open = func(context.Context, string) (*telemetrystore.QueryStore, error) {
		return nil, telemetrystore.ErrNotReadOnly
	}
	if _, err := l.connect(ctx); !errors.Is(err, telemetrystore.ErrNotReadOnly) {
		t.Errorf("not read-only: %v", err)
	}
}
