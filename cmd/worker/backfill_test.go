package main

import (
	"errors"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/rollup"
)

const tenant = "11111111-1111-4111-8111-111111111111"

var now = time.Date(2026, 10, 6, 12, 0, 30, 0, time.UTC)

func plan(t *testing.T, args ...string) (backfillPlan, error) {
	t.Helper()
	return planBackfill(args, now, rollup.BackfillOptions{RawRetention: 15 * 24 * time.Hour})
}

func TestPlanBackfillUsage(t *testing.T) {
	for name, args := range map[string][]string{
		"no tenant":      {"--from", "2026-10-06T08:00:00Z", "--to", "2026-10-06T09:00:00Z"},
		"bad from":       {"--tenant", tenant, "--from", "yesterday", "--to", "2026-10-06T09:00:00Z"},
		"bad resolution": {"--tenant", tenant, "--from", "2026-10-06T08:00:00Z", "--to", "2026-10-06T09:00:00Z", "--resolution", "5m"},
		"stray arg":      {"--tenant", tenant, "--from", "2026-10-06T08:00:00Z", "--to", "2026-10-06T09:00:00Z", "extra"},
	} {
		if _, err := plan(t, args...); !errors.Is(err, errUsage) {
			t.Errorf("%s: %v, want usage", name, err)
		}
	}
}

// all: 1m과 1h를 모두 쓰기 전에 검증한다. 1h는 live 1h 재계산 구간(10:00부터) 앞까지로 줄인다.
func TestPlanBackfillAllClampsHour(t *testing.T) {
	p, err := plan(t, "--tenant", tenant, "--from", "2026-10-06T08:00:00Z", "--to", "2026-10-06T11:00:00Z", "--job-id", "j1")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.jobs) != 2 || p.jobs[0].name != "1m" || p.jobs[1].name != "1h" || p.jobID != "j1" {
		t.Fatalf("plan = %+v", p)
	}
	if !p.jobs[1].req.To.Equal(time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("1h to = %s, want clamped to 10:00", p.jobs[1].req.To)
	}
	// 1m은 기본값(Window 0 → 1분), 1h는 hourConfig 그대로
	if p.jobs[0].cfg.Window != 0 || p.jobs[1].cfg.Window != time.Hour || p.jobs[1].cfg.Recompute != time.Hour {
		t.Error("plan must use the live job configs")
	}
}

// all인데 닫힌 시간이 없으면 1h를 건너뛰고 1m만 한다.
func TestPlanBackfillSkipsHourWhenNoClosedHour(t *testing.T) {
	p, err := plan(t, "--tenant", tenant, "--from", "2026-10-06T10:05:00Z", "--to", "2026-10-06T11:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.jobs) != 1 || p.jobs[0].name != "1m" || !p.skipped1h {
		t.Fatalf("plan = %+v", p)
	}
}

// 1m이 허용 밖이면 아무 해상도도 실행하지 않는다(1m을 쓴 뒤 1h에서 실패하는 일이 없다).
func TestPlanBackfillRejectsBeforeWriting(t *testing.T) {
	for name, args := range map[string][]string{
		"1m overlaps live":     {"--tenant", tenant, "--from", "2026-10-06T11:00:00Z", "--to", "2026-10-06T11:55:00Z"},
		"1h only overlaps":     {"--tenant", tenant, "--from", "2026-10-06T09:00:00Z", "--to", "2026-10-06T11:00:00Z", "--resolution", "1h"},
		"beyond raw retention": {"--tenant", tenant, "--from", "2026-09-20T00:00:00Z", "--to", "2026-09-21T00:00:00Z"},
	} {
		if p, err := plan(t, args...); !errors.Is(err, rollup.ErrBackfillRange) || len(p.jobs) != 0 {
			t.Errorf("%s: plan=%+v err=%v", name, p, err)
		}
	}
}
