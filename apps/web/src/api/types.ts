// query-api 응답 타입 (D02 §19 공통 응답, ADR 0027 metric, ADR 0038 catalog).
// 서버가 계산하지 않은 값은 null이다. null을 0으로 바꾸지 않는다 (계약 6).

export interface Meta {
  request_id: string;
  schema_version: number;
  partial: boolean;
  failed_shards: string[];
  watermark?: string | null;
  sampled?: boolean | null;
  coverage?: number | null;
  resolution_seconds?: number | null;
  scan_bytes?: number | null;
  warnings: string[];
}

export interface ServiceItem {
  service_id: string;
  name: string;
  namespace: string;
  environment: string;
  language: string | null;
  status: 'active' | 'inactive' | 'archived';
  first_seen: string;
  last_seen: string;
  owner_team: string | null;
  repository_url: string | null;
  runbook_url: string | null;
  tier: string | null;
  tags: string[];
}

export type PointReason =
  | 'no_data'
  | 'missing_baseline'
  | 'not_applicable'
  | 'bounds_mismatch'
  | 'unit_conflict'
  | 'type_conflict'
  | 'pending';

export interface MetricPoint {
  t: string;
  v: number | null;
  reason?: PointReason;
  partial: boolean;
}

export interface MetricSeries {
  labels: Record<string, string>;
  unit: string | null;
  points: MetricPoint[];
  completeness: number;
  missing: { from: string; to: string }[];
  flags: string[];
}

export type MetricAggregation =
  | 'rate'
  | 'increase'
  | 'sum'
  | 'avg'
  | 'min'
  | 'max'
  | 'count'
  | 'hist_sum'
  | 'p50'
  | 'p90'
  | 'p95'
  | 'p99';

export interface MetricFilter {
  op: 'and' | 'eq';
  field?: string;
  value?: string;
  args?: MetricFilter[];
}

export interface MetricQuery {
  range: { from: string; to: string };
  step_seconds: number;
  expression: {
    metric: string;
    aggregation: MetricAggregation;
    filter?: MetricFilter;
    group_by?: string[];
  };
}

/** GET /api/v1/metrics 항목의 조합 (ADR 0046). aggregations가 비면 rollup 조회가 지원하지 않는 유형이다. */
export interface MetricVariant {
  type: 'gauge' | 'sum' | 'histogram' | 'exponential_histogram' | 'summary' | string;
  temporality: 'unspecified' | 'delta' | 'cumulative' | string;
  monotonic: boolean;
  unit: string;
  series: number;
  last_seen: string;
  aggregations: MetricAggregation[];
}

/** GET /api/v1/metrics 항목. conflict는 한 이름에 조합이 둘 이상(계측 충돌)이라는 뜻이다. */
export interface MetricDescriptor {
  name: string;
  variants: MetricVariant[];
  conflict: boolean;
}

/** GET /api/v1/metrics/labels 결과 (ADR 0046). */
export interface MetricLabels {
  metric: string;
  keys: { key: string; sources: ('attribute' | 'resource' | string)[]; series: number }[];
}

export interface MetricResult {
  series: MetricSeries[];
  /** 실제로 읽은 rollup 해상도(60 = 1분, 3600 = 1시간, ADR 0028·0047). 이전 서버는 보내지 않는다. */
  source_window_seconds?: number;
  /** step 경계로 맞춘 실제 조회 범위 */
  range?: { from: string; to: string };
}

/** POST /api/v1/query/traces 결과 행 (D02 §13 + D05 §06, ADR 0043). 모르는 값은 null이다. */
export interface TraceSummary {
  trace_id: string;
  start_time: string;
  duration_ms: number;
  span_count: number;
  has_error: boolean;
  /** 구조 완결성(D02 §22). meta.partial(실행 일부 실패)과 다르다. */
  complete: boolean;
  reasons: ('missing_root' | 'missing_parent' | 'span_limit_reached' | string)[];
  root_service: string | null;
  root_service_id: string | null;
  root_name: string | null;
}

/** query filter AST (D02 §15, ADR 0037). */
export interface FilterNode {
  op: 'and' | 'or' | 'eq' | 'neq' | 'in' | 'exists' | 'gt' | 'gte' | 'lt' | 'lte' | 'contains';
  field?: string;
  value?: string | number | boolean | (string | number)[];
  args?: FilterNode[];
}

export interface SearchRequest {
  range: { from: string; to: string };
  filter?: FilterNode;
  limit?: number;
  cursor?: string;
}

export interface ApiErrorBody {
  code: string;
  message: string;
  request_id: string;
  retryable: boolean;
  details?: Record<string, unknown>;
}

/** GET /api/v1/traces/{trace_id} span (ADR 0022). 시각은 RFC3339 UTC, 길이는 ns. */
export interface TraceSpanItem {
  trace_id: string;
  span_id: string;
  parent_span_id: string | null;
  service_id: string;
  service_name: string;
  environment: string | null;
  name: string;
  kind: 'internal' | 'server' | 'client' | 'producer' | 'consumer' | 'unspecified';
  status_code: 'ok' | 'error' | 'unset';
  status_message: string;
  start_time: string;
  end_time: string;
  duration_ns: number;
  attributes: Record<string, unknown>;
  resource_attributes: Record<string, unknown>;
  events: { name: string; time: string; attributes: Record<string, unknown> }[];
  links: { trace_id: string; span_id: string; attributes: Record<string, unknown> }[];
}

export interface TraceDetail {
  trace_id: string;
  spans: TraceSpanItem[];
  span_count: number;
  complete: boolean;
  reasons: string[];
  last_updated_at: string;
}

/** POST /api/v1/query/logs 행 (ADR 0037). 없는 trace·span ID는 null. */
export interface LogItem {
  time: string;
  event_id: string;
  service_id: string;
  severity_number: number;
  trace_id: string | null;
  span_id: string | null;
  body: string;
  attributes: Record<string, string>;
}
