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

export interface ApiErrorBody {
  code: string;
  message: string;
  request_id: string;
  retryable: boolean;
  details?: Record<string, unknown>;
}
