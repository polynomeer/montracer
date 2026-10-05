package telemetrystore

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMetricQueryValidate(t *testing.T) {
	from := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ok := MetricQuery{Metric: "m", StepSeconds: 60, Range: TimeRange{From: from, To: from.Add(time.Hour)}}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("k", MaxLabelLen+1)
	cases := map[string]func(q *MetricQuery){
		"no metric":       func(q *MetricQuery) { q.Metric = "" },
		"step 30s":        func(q *MetricQuery) { q.StepSeconds = 30 },
		"step 90s":        func(q *MetricQuery) { q.StepSeconds = 90 },
		"empty range":     func(q *MetricQuery) { q.Range.To = q.Range.From },
		"too many groups": func(q *MetricQuery) { q.GroupBy = []string{"a", "b", "c", "d", "e", "f"} },
		"long key":        func(q *MetricQuery) { q.GroupBy = []string{long} },
		"long value":      func(q *MetricQuery) { q.Filters = []LabelMatch{{Key: "a", Value: long}} },
	}
	for name, mut := range cases {
		q := ok
		mut(&q)
		var ia interface{ InvalidArgument() (string, string) }
		if err := q.validate(); !errors.As(err, &ia) {
			t.Errorf("%s: err = %v, want invalid argument", name, err)
		}
	}
	// 예산: 8일 범위, step 수 2,000 초과 → QUERY_BUDGET_EXCEEDED
	for name, q := range map[string]MetricQuery{
		"8 days":      {Metric: "m", StepSeconds: 3600, Range: TimeRange{From: from, To: from.Add(8 * 24 * time.Hour)}},
		"2001 points": {Metric: "m", StepSeconds: 60, Range: TimeRange{From: from, To: from.Add(2001 * time.Minute)}},
	} {
		var be interface{ BudgetExceeded() map[string]any }
		if err := q.validate(); !errors.As(err, &be) {
			t.Errorf("%s: err = %v, want budget", name, err)
		}
	}
}
