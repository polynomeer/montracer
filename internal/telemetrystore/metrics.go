package telemetrystore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
)

// metric 조회 한도 (D02 §13, §19).
const (
	// MaxMetricRange는 metric 조회 최대 범위다.
	MaxMetricRange = 7 * 24 * time.Hour
	// MaxPointsPerSeries는 series당 최대 point(step) 수다.
	MaxPointsPerSeries = 2000
	// MaxGroupBy는 group_by key 최대 수다.
	MaxGroupBy = 5
	// MaxLabelFilters는 label 필터 최대 수다 (D02 §13 leaf 20).
	MaxLabelFilters = 20
	// MaxLabelLen은 label key·value 최대 길이다.
	MaxLabelLen = 256
)

// LabelMatch는 label 일치 조건이다. key는 point 속성을 먼저, 없으면 resource 속성을 본다.
type LabelMatch struct {
	Key, Value string
}

// SourceWindow는 step에 맞는 rollup 해상도다: step이 1시간의 배수면 metric_1h, 아니면 metric_1m (ADR 0028).
// 같은 계산(ADR 0025)의 다른 window라 결과 의미는 같고 읽는 행 수만 60배 줄어든다.
func SourceWindow(stepSeconds int) time.Duration {
	if stepSeconds > 0 && stepSeconds%3600 == 0 {
		return time.Hour
	}
	return time.Minute
}

func rollupTable(w time.Duration) string {
	if w == time.Hour {
		return "metric_1h"
	}
	return "metric_1m"
}

// MetricQuery는 rollup(metric_1m·metric_1h) 집계 조회 조건이다.
type MetricQuery struct {
	Metric      string
	Range       TimeRange // From은 step 경계로 맞춘다
	StepSeconds int       // 60의 배수
	Filters     []LabelMatch
	GroupBy     []string
	// Window는 읽을 rollup 해상도다(1분·1시간). 0이면 SourceWindow(StepSeconds). step은 window의 배수여야 한다.
	Window time.Duration
}

func (q MetricQuery) window() time.Duration {
	if q.Window == 0 {
		return SourceWindow(q.StepSeconds)
	}
	return q.Window
}

// MetricBucket은 (group, step) 하나의 집계 상태다. 1분 window들을 저장소에서 합친 값이다.
// 해당 값이 없으면 *Windows가 0이다 — 0을 값으로 쓰지 않는다(계약 6).
type MetricBucket struct {
	Group     []string // GroupBy 순서의 label 값 (없으면 "")
	StepStart time.Time
	Types     []string
	Units     []string
	Windows   int // 합친 1분 window 행 수
	Streams   int // 합친 stream 수 (기대 window 수 = Streams × step/60)
	Samples   uint64

	ValueWindows   int // has_value 행 수
	Total          float64
	Samples4Avg    uint64 // has_value 행의 samples 합 (평균 분모)
	Min, Max, Last float64

	IncreaseWindows int
	Increase        float64

	HistogramWindows int
	Count            uint64
	HistSum          float64
	BoundsVariants   int // 경계 종류 수. 1이 아니면 bucket 병합 불가
	Bounds           []float64
	Buckets          []uint64

	Flags   []string
	Partial bool
}

func validLabel(s string) bool { return s != "" && len(s) <= MaxLabelLen }

func (q MetricQuery) validate() error {
	if q.Metric == "" || len(q.Metric) > MaxLabelLen {
		return invalidArg("metric", "required, at most 256 bytes")
	}
	if q.StepSeconds < 60 || q.StepSeconds%60 != 0 {
		return invalidArg("step_seconds", "must be a positive multiple of 60")
	}
	// 범위는 step 경계로 맞춘 뒤라 최대 한 step만큼 길 수 있다. 원 범위의 7일 검사는 API 경계에서 한다.
	if err := q.Range.validate(MaxMetricRange + time.Duration(q.StepSeconds)*time.Second); err != nil {
		return err
	}
	if w := q.window(); (w != time.Minute && w != time.Hour) || q.StepSeconds%int(w/time.Second) != 0 {
		return invalidArg("step_seconds", "must be a multiple of the rollup window")
	}
	if steps := int(q.Range.To.Sub(q.Range.From) / (time.Duration(q.StepSeconds) * time.Second)); steps > MaxPointsPerSeries {
		return &budgetError{op: "metric query", details: map[string]any{"max_points_per_series": MaxPointsPerSeries}}
	}
	if len(q.GroupBy) > MaxGroupBy {
		return invalidArg("group_by", fmt.Sprintf("at most %d keys", MaxGroupBy))
	}
	for _, k := range q.GroupBy {
		if !validLabel(k) {
			return invalidArg("group_by", "keys must be 1..256 bytes")
		}
	}
	if len(q.Filters) > MaxLabelFilters {
		return invalidArg("filter", fmt.Sprintf("at most %d conditions", MaxLabelFilters))
	}
	for _, f := range q.Filters {
		if !validLabel(f.Key) || len(f.Value) > MaxLabelLen {
			return invalidArg("filter", "key 1..256 bytes, value at most 256 bytes")
		}
	}
	return nil
}

// MetricBuckets는 metric_1m을 (group, step)으로 합친다 (D02 §10, §13, ADR 0027).
//   - tenant·시간·expires_at은 mandatory predicate다(D02 §15). row policy도 함께 걸린다(ADR 0018).
//   - (stream, window)는 revision이 가장 큰 행 하나만 쓴다(ADR 0026).
//   - environment가 제한된 principal은 허용 environment의 stream만 본다. environment가 없는 stream은 볼 수 없다.
//   - label key·value는 모두 parameter binding으로 넣는다. SQL에는 고정 template만 쓴다.
//
// now는 expires_at 판정 시각이다. 삭제 tombstone predicate는 삭제 원장(F09) 구현 시 추가한다 (ADR 0018 §7).
func (s *QueryStore) MetricBuckets(ctx context.Context, p authz.Principal, q MetricQuery, now time.Time) ([]MetricBucket, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return nil, err
	}
	if now.IsZero() {
		return nil, errors.New("telemetrystore: now is required")
	}
	if err := q.validate(); err != nil {
		return nil, err
	}
	tenant := p.Tenant()
	args := []any{tenant.String(), q.Metric, q.Range.From, q.Range.To, now, q.StepSeconds}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	label := func(key string) string {
		k := arg(key)
		return fmt.Sprintf("coalesce(nullIf(JSONExtractString(attributes_json, %s), ''), JSONExtractString(resource_json, %s))", k, k)
	}
	var where []string
	for _, f := range q.Filters {
		where = append(where, fmt.Sprintf("%s = %s", label(f.Key), arg(f.Value)))
	}
	if envs := p.Environments(); envs != nil {
		where = append(where, fmt.Sprintf("has(%s, JSONExtractString(resource_json, 'deployment.environment.name'))", arg(envs)))
	}
	group := "[]::Array(String)"
	if len(q.GroupBy) > 0 {
		parts := make([]string, len(q.GroupBy))
		for i, k := range q.GroupBy {
			parts[i] = label(k)
		}
		group = "[" + strings.Join(parts, ", ") + "]"
	}
	filter := ""
	if len(where) > 0 {
		filter = "WHERE " + strings.Join(where, " AND ")
	}
	ctx = tenantContext(ctx, tenant)
	query := `
		WITH dedup AS (
			SELECT * FROM ` + rollupTable(q.window()) + `
			WHERE tenant_id = $1 AND metric_name = $2
			  AND window_start >= $3 AND window_start < $4 AND expires_at > $5
			ORDER BY stream_id, window_start, revision DESC
			LIMIT 1 BY stream_id, window_start
		)
		SELECT ` + group + ` AS g,
		       toDateTime(toUnixTimestamp($3) + intDiv(toUnixTimestamp(window_start) - toUnixTimestamp($3), $6) * $6, 'UTC') AS step,
		       groupUniqArray(toString(type)), groupUniqArray(unit), count(), uniqExact(stream_id), sum(samples),
		       countIf(has_value), sumIf(total, has_value), sumIf(samples, has_value),
		       minIf(min, has_value), maxIf(max, has_value), argMaxIf(last, window_start, has_value),
		       countIf(has_increase), sumIf(increase, has_increase),
		       countIf(has_histogram), sumIf(count, has_histogram), sumIf(hist_sum, has_histogram),
		       uniqExactIf(bounds, has_histogram), anyIf(bounds, has_histogram), sumForEachIf(buckets, has_histogram),
		       groupUniqArrayArray(flags), max(partial)
		FROM dedup
		` + filter + `
		GROUP BY g, step
		ORDER BY g, step`
	rows, err := s.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, classify("metric query", err)
	}
	defer func() { _ = rows.Close() }()
	var out []MetricBucket
	for rows.Next() {
		var (
			b                              MetricBucket
			windows, streams, valueW, incW uint64
			histW, varn                    uint64
		)
		if err := rows.Scan(&b.Group, &b.StepStart, &b.Types, &b.Units, &windows, &streams, &b.Samples,
			&valueW, &b.Total, &b.Samples4Avg, &b.Min, &b.Max, &b.Last,
			&incW, &b.Increase, &histW, &b.Count, &b.HistSum, &varn, &b.Bounds, &b.Buckets,
			&b.Flags, &b.Partial); err != nil {
			return nil, fmt.Errorf("telemetrystore: scan metric bucket: %w", err)
		}
		b.Windows, b.Streams, b.ValueWindows, b.IncreaseWindows, b.HistogramWindows, b.BoundsVariants =
			int(windows), int(streams), int(valueW), int(incW), int(histW), int(varn) //nolint:gosec // 결과 행 예산 안의 수
		b.StepStart = b.StepStart.UTC()
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, classify("metric query", err)
	}
	return out, nil
}

// RollupWatermark는 tenant의 해당 해상도 rollup이 계산을 마친 시각(가장 늦은 window의 끝)이다
// (ADR 0026 §2: rollup은 tenant별로 진행). 이보다 늦은 step은 아직 계산되지 않았거나 바뀔 수 있다. 행이 없으면 zero time.
func (s *QueryStore) RollupWatermark(ctx context.Context, p authz.Principal, window time.Duration, since, now time.Time) (time.Time, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return time.Time{}, err
	}
	tenant := p.Tenant()
	var (
		last time.Time
		n    uint64
	)
	if err := s.conn.QueryRow(tenantContext(ctx, tenant), `SELECT max(window_start), count() FROM `+rollupTable(window)+`
		WHERE tenant_id = $1 AND window_start >= $2 AND expires_at > $3`, tenant.String(), since, now).Scan(&last, &n); err != nil {
		return time.Time{}, classify("rollup watermark", err)
	}
	if n == 0 {
		return time.Time{}, nil
	}
	return last.UTC().Add(window), nil
}

// RollupCoverageStart는 tenant의 해당 해상도 rollup이 가진 가장 이른 window다. 행이 없으면 zero time.
// 1시간 rollup은 배포 시점부터만 채워지므로, 조회 범위가 이보다 이르면 1분 rollup을 읽어야 한다 (ADR 0028 §2).
func (s *QueryStore) RollupCoverageStart(ctx context.Context, p authz.Principal, window time.Duration, now time.Time) (time.Time, error) {
	if err := authz.Authorize(p, authz.TelemetryRead); err != nil {
		return time.Time{}, err
	}
	tenant := p.Tenant()
	var (
		first time.Time
		n     uint64
	)
	if err := s.conn.QueryRow(tenantContext(ctx, tenant), `SELECT min(window_start), count() FROM `+rollupTable(window)+`
		WHERE tenant_id = $1 AND expires_at > $2`, tenant.String(), now).Scan(&first, &n); err != nil {
		return time.Time{}, classify("rollup coverage", err)
	}
	if n == 0 {
		return time.Time{}, nil
	}
	return first.UTC(), nil
}
