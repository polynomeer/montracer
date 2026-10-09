package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// lazyQuery는 dry-run(ADR 0052)용 ClickHouse 연결이다. ClickHouse는 control-api의 선택 의존이다:
// 기동 때 닿지 않아도 control-api는 뜨고(감사·monitor API는 PostgreSQL만 쓴다), dry-run 때 다시 연결한다.
// 실패한 뒤에는 retryAfter 동안 다시 시도하지 않는다(요청마다 연결 시도로 지연을 늘리지 않는다).
type lazyQuery struct {
	dsn        string
	retryAfter time.Duration
	// openTimeout은 연결 시도 하나의 상한이다(Ping·readonly 확인).
	openTimeout time.Duration
	open        func(ctx context.Context, dsn string) (*telemetrystore.QueryStore, error)
	logger      *slog.Logger

	mu      sync.Mutex
	store   *telemetrystore.QueryStore
	lastErr error
	next    time.Time
}

func newLazyQuery(dsn string, logger *slog.Logger) *lazyQuery {
	return &lazyQuery{dsn: dsn, retryAfter: 30 * time.Second, openTimeout: 2 * time.Second, open: telemetrystore.OpenQuery, logger: logger}
}

// connect는 연결을 시도한다. 계정이 읽기 전용이 아니면(설정 오류) 그 오류를 그대로 돌려준다 — 기동 때는 실패시킨다.
func (l *lazyQuery) connect(ctx context.Context) (*telemetrystore.QueryStore, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.store != nil {
		return l.store, nil
	}
	if time.Now().Before(l.next) {
		return nil, fmt.Errorf("clickhouse query not connected: %w", l.lastErr)
	}
	// lock을 잡은 채 연결하므로 요청 시간 상한과 별도로 짧게 끊는다(멈춘 ClickHouse가 다른 dry-run을 오래 막지 않게)
	openCtx, cancel := context.WithTimeout(ctx, l.openTimeout)
	defer cancel()
	s, err := l.open(openCtx, l.dsn)
	if err != nil {
		l.lastErr, l.next = err, time.Now().Add(l.retryAfter)
		if errors.Is(err, telemetrystore.ErrNotReadOnly) {
			l.logger.Error("clickhouse query account is not read-only; monitor dry-run stays disabled")
		}
		return nil, err
	}
	l.store = s
	return s, nil
}

func (l *lazyQuery) MetricBuckets(ctx context.Context, p authz.Principal, q telemetrystore.MetricQuery, now time.Time) ([]telemetrystore.MetricBucket, error) {
	s, err := l.connect(ctx)
	if err != nil {
		return nil, err
	}
	return s.MetricBuckets(ctx, p, q, now)
}

func (l *lazyQuery) RollupWatermark(ctx context.Context, p authz.Principal, window time.Duration, since, now time.Time) (time.Time, error) {
	s, err := l.connect(ctx)
	if err != nil {
		return time.Time{}, err
	}
	return s.RollupWatermark(ctx, p, window, since, now)
}

func (l *lazyQuery) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.store != nil {
		_ = l.store.Close()
	}
}
