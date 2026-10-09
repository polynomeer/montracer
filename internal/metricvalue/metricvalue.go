// Package metricvalue는 metric rollup bucket의 연산 값 계산이다 (ADR 0027·0050).
//
// query-api의 metric 조회와 alert-worker의 monitor 평가가 같은 의미를 쓰도록 한 곳에 둔다.
// 분위수는 bucket 병합 뒤 계산한다(계약 5). 값이 없으면 0이 아니라 사유다(계약 6).
package metricvalue

import (
	"slices"
	"time"

	"github.com/polynomeer/montracer/internal/metricagg"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// 결측 사유. 값이 null이면 반드시 하나가 붙는다 (계약 6).
const (
	ReasonNoData          = "no_data"          // 그 step에 window가 없다
	ReasonMissingBaseline = "missing_baseline" // counter 기준점이 없어 증가량을 모른다 (ADR 0025 §3)
	ReasonNotApplicable   = "not_applicable"   // 그 step의 유형에 이 연산 값이 없다
	ReasonBoundsMismatch  = "bounds_mismatch"  // histogram 경계가 달라 병합할 수 없다
	ReasonUnitConflict    = "unit_conflict"    // 단위가 다른 stream이 한 group에 섞였다
	ReasonTypeConflict    = "type_conflict"    // 유형이 다른 stream이 한 group·step에 섞였다
	ReasonPending         = "pending"          // rollup이 아직 이 step을 계산하지 않았다(watermark 이후)
)

var quantiles = map[string]float64{"p50": 0.5, "p90": 0.9, "p95": 0.95, "p99": 0.99}

// Value는 (group, step) bucket 하나의 연산 값이다. 값이 없으면 0과 사유를 돌려준다 — 사유가 있으면 값을 쓰지 않는다(계약 6).
// query-api(/query/metrics)와 alert-worker(monitor 평가)가 같은 계산을 쓴다.
func Value(agg string, b telemetrystore.MetricBucket, step time.Duration) (float64, string) {
	if len(b.Units) > 1 {
		return 0, ReasonUnitConflict
	}
	if len(b.Types) > 1 {
		return 0, ReasonTypeConflict
	}
	switch agg {
	case "rate", "increase", "sum":
		if b.IncreaseWindows == 0 {
			if slices.Contains(b.Flags, metricagg.FlagMissingBaseline) {
				return 0, ReasonMissingBaseline
			}
			return 0, ReasonNotApplicable
		}
		if agg == "rate" {
			return b.Increase / step.Seconds(), ""
		}
		return b.Increase, ""
	case "avg", "min", "max":
		if b.ValueWindows == 0 || b.Samples4Avg == 0 {
			return 0, ReasonNotApplicable
		}
		switch agg {
		case "avg":
			return b.Total / float64(b.Samples4Avg), ""
		case "min":
			return b.Min, ""
		default:
			return b.Max, ""
		}
	case "count":
		if b.HistogramWindows == 0 {
			return 0, ReasonNotApplicable
		}
		return float64(b.Count), ""
	case "hist_sum":
		// 관측값의 합(예: 요청 소요시간 합계). 서비스 상세의 endpoint "total time" 열(D05 §05)이다.
		if b.HistogramWindows == 0 {
			return 0, ReasonNotApplicable
		}
		return b.HistSum, ""
	default: // quantile
		if b.HistogramWindows == 0 {
			return 0, ReasonNotApplicable
		}
		if b.BoundsVariants != 1 {
			return 0, ReasonBoundsMismatch
		}
		v, ok := metricagg.Quantile(quantiles[agg], b.Bounds, b.Buckets)
		if !ok {
			return 0, ReasonNoData // 관측 0건 — 0ms가 아니다
		}
		return v, ""
	}
}

// MissingWindows는 합산형 값(증가량·histogram)에서 step 안의 1분 window가 빠졌는지 본다.
// 빠진 window는 0으로 더해진 셈이라 값이 실제보다 작을 수 있다 → partial (계약 6). gauge 값 통계는 해당 없다.
func MissingWindows(agg string, b telemetrystore.MetricBucket, perWindow time.Duration) bool {
	expected := b.Streams * int(perWindow)
	switch agg {
	case "rate", "increase", "sum":
		return b.IncreaseWindows < expected
	case "count", "hist_sum", "p50", "p90", "p95", "p99":
		return b.HistogramWindows < expected
	default:
		return false
	}
}
