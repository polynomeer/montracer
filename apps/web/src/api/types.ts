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

export interface MetricResult {
  series: MetricSeries[];
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
