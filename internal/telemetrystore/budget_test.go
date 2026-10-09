package telemetrystore

import (
	"testing"
	"time"
)

// 실행 시간 예산은 초 단위로 올림하고 [1, 60]으로 고정한다: 0은 ClickHouse에서 무제한, 60 초과는 profile MAX 위반이다.
func TestExecutionSeconds(t *testing.T) {
	for d, want := range map[time.Duration]int{time.Millisecond: 1, 500 * time.Millisecond: 1, time.Second: 1, 1500 * time.Millisecond: 2, 10 * time.Second: 10, 5 * time.Minute: 60} {
		if got := executionSeconds(d); got != want {
			t.Errorf("executionSeconds(%v) = %d, want %d", d, got, want)
		}
	}
}
