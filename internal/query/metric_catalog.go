package query

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// MetricCatalogStore는 metric 사전 저장소다 (telemetrystore.QueryStore, ADR 0046).
type MetricCatalogStore interface {
	MetricCatalog(ctx context.Context, p authz.Principal, q telemetrystore.MetricCatalogQuery, now time.Time) ([]telemetrystore.MetricDescriptor, bool, error)
	MetricLabelKeys(ctx context.Context, p authz.Principal, metric string, r telemetrystore.TimeRange, now time.Time) ([]telemetrystore.MetricLabelKey, bool, error)
}

// MetricVariantItem은 이름 하나에서 관측된 (유형, 단위, temporality, monotonic) 조합이다.
// Aggregations는 그 조합에 의미가 있는 연산이다(D02 §07: gauge에 rate 금지, summary quantile 평균 금지).
// 비어 있으면 rollup 조회가 지원하지 않는 유형이다(summary·exponential histogram은 원본 표시만, ADR 0025).
type MetricVariantItem struct {
	Type         string   `json:"type"`
	Temporality  string   `json:"temporality"`
	Monotonic    bool     `json:"monotonic"`
	Unit         string   `json:"unit"`
	Series       int      `json:"series"`
	LastSeen     string   `json:"last_seen"`
	Aggregations []string `json:"aggregations"`
}

// MetricItem은 GET /api/v1/metrics의 항목이다. Conflict는 한 이름에 조합이 둘 이상이라는 뜻이다(계측 충돌).
type MetricItem struct {
	Name     string              `json:"name"`
	Variants []MetricVariantItem `json:"variants"`
	Conflict bool                `json:"conflict"`
}

type metricsCatalogResponse struct {
	Data       []MetricItem `json:"data"`
	NextCursor *string      `json:"next_cursor"`
	Meta       Meta         `json:"meta"`
}

// MetricLabelItem은 metric label key다. Sources는 attribute·resource 중 관측된 곳이다.
type MetricLabelItem struct {
	Key     string   `json:"key"`
	Sources []string `json:"sources"`
	Series  int      `json:"series"`
}

type metricLabelsResponse struct {
	Data struct {
		Metric string            `json:"metric"`
		Keys   []MetricLabelItem `json:"keys"`
	} `json:"data"`
	Meta Meta `json:"meta"`
}

// AllowedAggregations는 metric 유형별로 의미가 있는 연산이다 (D02 §07 Metric 정규화 계약, ADR 0027 §1, ADR 0046 §1).
// rollup(metricagg.Compute)이 실제로 계산하는 값과 맞춘다:
//   - gauge, non-monotonic cumulative sum, unspecified sum(단조 여부 무관): 원시 값 → 값 통계. rate는 의미가 없다("임의 rate 적용 금지")
//   - monotonic delta·cumulative sum(counter): 증가량 → 증가율·증가량
//   - non-monotonic delta sum: 증가량(순변화) → step 합계만. 증가율이 아니다
//   - histogram: bucket 병합 분위수·관측 수·합
//   - summary·exponential histogram: rollup이 집계하지 않는다(원본 표시만) → 빈 목록
func AllowedAggregations(typ, temporality string, monotonic bool) []string {
	switch typ {
	case "gauge":
		return []string{"avg", "min", "max"}
	case "sum":
		switch {
		case temporality == "delta" && monotonic, temporality == "cumulative" && monotonic:
			return []string{"rate", "increase", "sum"}
		case temporality == "delta":
			return []string{"sum"}
		default: // non-monotonic cumulative, unspecified
			return []string{"avg", "min", "max"}
		}
	case "histogram":
		return []string{"p50", "p90", "p95", "p99", "count", "hist_sum"}
	default:
		return []string{}
	}
}

type metricCatalogPosition struct {
	Name string `json:"n"`
}

func parseRange(r *http.Request) (telemetrystore.TimeRange, error) {
	from, err := parseTime(r, "from")
	if err != nil {
		return telemetrystore.TimeRange{}, err
	}
	to, err := parseTime(r, "to")
	if err != nil {
		return telemetrystore.TimeRange{}, err
	}
	if !from.Before(to) {
		return telemetrystore.TimeRange{}, invalid("range", "from must be before to")
	}
	if to.Sub(from) > telemetrystore.MaxMetricCatalogRange {
		e := apierr.New(apierr.QueryBudgetExceeded, "조회 범위를 줄이세요")
		e.Details = map[string]any{"max_range_seconds": int64(telemetrystore.MaxMetricCatalogRange / time.Second)}
		return telemetrystore.TimeRange{}, e
	}
	return telemetrystore.TimeRange{From: from, To: to}, nil
}

// listMetrics는 GET /api/v1/metrics?from&to&q&limit&cursor다 (D05 §08 metric dictionary, ADR 0046).
// 범위 안에서 관측된 metric을 이름순으로 돌려준다. 범위는 필수이고 24시간까지다.
func (h *Handler) listMetrics(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.MetricCatalog == nil || h.cfg.Cursor == nil {
		return apierr.New(apierr.NotFound, "metric 사전을 사용할 수 없습니다")
	}
	rng, err := parseRange(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	contains := q.Get("q")
	if len(contains) > telemetrystore.MaxLabelLen {
		return invalid("q", "at most 256 bytes")
	}
	limit := 200
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > telemetrystore.MaxMetricCatalogPage {
			return invalid("limit", "must be within [1, 1000]")
		}
		limit = n
	}
	fp := append([]string{p.Kind().String(), p.Subject(), string(authz.TelemetryRead)}, p.Environments()...)
	binding := apicursor.Binding{
		Tenant:      p.Tenant().String(),
		Fingerprint: apicursor.Fingerprint(fp...),
		QueryHash:   apicursor.Fingerprint("metrics", rng.From.Format(time.RFC3339Nano), rng.To.Format(time.RFC3339Nano), contains),
	}
	now := h.cfg.Now().UTC()
	mq := telemetrystore.MetricCatalogQuery{Range: rng, Contains: contains, Limit: limit}
	if c := q.Get("cursor"); c != "" {
		claims, err := h.cfg.Cursor.Decode(c, binding)
		var pos metricCatalogPosition
		if err != nil || json.Unmarshal(claims.Position, &pos) != nil || pos.Name == "" {
			return invalid("cursor", "invalid, expired, or not for this query")
		}
		mq.After = pos.Name
		now = claims.Snapshot // 만료 판정 시각을 page 사이에 고정한다
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.QueryTimeout)
	defer cancel()
	items, more, err := h.cfg.MetricCatalog.MetricCatalog(ctx, p, mq, now)
	if err != nil {
		return err
	}
	resp := metricsCatalogResponse{Data: make([]MetricItem, 0, len(items)), Meta: newMeta(r)}
	for _, it := range items {
		m := MetricItem{Name: it.Name, Variants: make([]MetricVariantItem, 0, len(it.Variants)), Conflict: len(it.Variants) > 1}
		for _, v := range it.Variants {
			m.Variants = append(m.Variants, MetricVariantItem{
				Type: v.Type, Temporality: v.Temporality, Monotonic: v.Monotonic, Unit: v.Unit, Series: v.Series,
				LastSeen: v.LastSeen.Format(time.RFC3339Nano), Aggregations: AllowedAggregations(v.Type, v.Temporality, v.Monotonic),
			})
		}
		resp.Data = append(resp.Data, m)
	}
	if more && len(items) > 0 {
		tok, err := h.cfg.Cursor.Encode(binding, now, metricCatalogPosition{Name: items[len(items)-1].Name})
		if err != nil {
			return err
		}
		resp.NextCursor = &tok
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	return json.NewEncoder(w).Encode(resp)
}

// listMetricLabels는 GET /api/v1/metrics/labels?metric&from&to다 (group_by·filter 선택지, ADR 0046).
// key만 돌려준다(값 목록 없음). 200개를 넘으면 series가 많은 순으로 자르고 meta.warnings에 label_keys_truncated를 단다.
func (h *Handler) listMetricLabels(w http.ResponseWriter, r *http.Request, p authz.Principal) error {
	if h.cfg.MetricCatalog == nil {
		return apierr.New(apierr.NotFound, "metric 사전을 사용할 수 없습니다")
	}
	metric := r.URL.Query().Get("metric")
	if metric == "" || len(metric) > telemetrystore.MaxLabelLen {
		return invalid("metric", "required, at most 256 bytes")
	}
	rng, err := parseRange(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.QueryTimeout)
	defer cancel()
	keys, truncated, err := h.cfg.MetricCatalog.MetricLabelKeys(ctx, p, metric, rng, h.cfg.Now().UTC())
	if err != nil {
		return err
	}
	var resp metricLabelsResponse
	resp.Meta = newMeta(r)
	resp.Data.Metric = metric
	resp.Data.Keys = make([]MetricLabelItem, 0, len(keys))
	for _, k := range keys {
		resp.Data.Keys = append(resp.Data.Keys, MetricLabelItem{Key: k.Key, Sources: k.Sources, Series: k.Series})
	}
	if truncated {
		resp.Meta.Warnings = append(resp.Meta.Warnings, "label_keys_truncated")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	return json.NewEncoder(w).Encode(resp)
}
