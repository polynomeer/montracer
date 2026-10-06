package query

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/polynomeer/montracer/internal/apierr"
)

// tenantGate는 tenant별 interactive 조회 동시성 상한이다 (D02 §15: 조직별 동시 5개·대기 20개).
// replica마다 따로 센다(Cell 전체 상한 100은 gateway·LB에서, 후속). 대기열이 차면 429 RATE_LIMITED다.
type tenantGate struct {
	active, waiting int
	mu              sync.Mutex
	slots           map[string]*gateSlot
}

type gateSlot struct {
	running chan struct{}
	waiting atomic.Int32
}

func newTenantGate(active, waiting int) *tenantGate {
	return &tenantGate{active: active, waiting: waiting, slots: map[string]*gateSlot{}}
}

func (g *tenantGate) slot(tenant string) *gateSlot {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.slots[tenant]
	if !ok {
		s = &gateSlot{running: make(chan struct{}, g.active)}
		g.slots[tenant] = s
	}
	return s
}

// acquire는 실행 slot을 얻는다. 대기열이 차면 즉시 429, 대기 중 요청이 끝나면(ctx) 그 오류다.
func (g *tenantGate) acquire(ctx context.Context, tenant string) (func(), error) {
	s := g.slot(tenant)
	release := func() { <-s.running }
	select {
	case s.running <- struct{}{}:
		return release, nil
	default:
	}
	if int(s.waiting.Add(1)) > g.waiting {
		s.waiting.Add(-1)
		return nil, apierr.NewRateLimited("조회 동시 실행 한도를 넘었습니다. 잠시 뒤 다시 시도하세요", 1)
	}
	defer s.waiting.Add(-1)
	select {
	case s.running <- struct{}{}:
		return release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
