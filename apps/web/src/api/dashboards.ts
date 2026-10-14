// control-api dashboard API (D02 §14, ADR 0057·0058). 정의만 저장한다 — 위젯 query는 보는 사람의 권한으로 query-api가 실행한다.
// 쓰기는 Idempotency-Key(POST)·If-Match(PUT·DELETE)를 붙인다.
import { apiRequest, type ApiResult } from './client.ts';
import type { MetricAggregation } from './types.ts';

export type WidgetKind = 'time_series' | 'stat' | 'table' | 'top_list' | 'text';
export type VariableKind = 'environment' | 'service';
export type TimeOverrideRange = '15m' | '1h' | '4h' | '1d' | '7d';

export interface DashFilter {
  op: 'and' | 'eq';
  field?: string;
  /** "$이름"이면 변수다 */
  value?: string;
  args?: DashFilter[];
}

export interface WidgetQuery {
  metric: string;
  aggregation: MetricAggregation;
  filter: DashFilter | null;
  group_by: string[];
}

export interface WidgetDisplay {
  stack?: boolean;
  decimals?: number;
  limit?: number;
  sort?: 'desc' | 'asc';
}

export interface Widget {
  id: string;
  kind: WidgetKind;
  title: string;
  unit: string | null;
  display: WidgetDisplay;
  time_override: { range: TimeOverrideRange } | null;
  query: WidgetQuery | null;
  text: string | null;
}

export interface Variable {
  name: string;
  kind: VariableKind;
  default: string | null;
}

export interface Placement {
  widget_id: string;
  x: number;
  y: number;
  w: number;
  h: number;
}

/** 정규형 DashboardSpec (ADR 0057 §1) */
export interface DashboardSpec {
  version: 1;
  title: string;
  description: string;
  variables: Variable[];
  layout: Placement[];
  widgets: Widget[];
}

export interface DashboardSummary {
  id: string;
  title: string;
  revision: number;
  widget_count: number;
  created_at: string;
  updated_at: string;
  created_by: string;
  updated_by: string;
}

export interface DashboardItem extends DashboardSummary {
  spec: DashboardSpec;
}

const enc = encodeURIComponent;
const ifMatch = (revision: number) => ({ 'If-Match': `"${String(revision)}"` });

export function listDashboards(signal: AbortSignal): Promise<ApiResult<DashboardSummary[]>> {
  return apiRequest<DashboardSummary[]>('/dashboards?limit=200', { signal });
}

export function getDashboard(id: string, signal: AbortSignal): Promise<ApiResult<DashboardItem>> {
  return apiRequest<DashboardItem>(`/dashboards/${enc(id)}`, { signal });
}

/** 생성. idempotencyKey는 편집 화면 하나에 하나다(다시 눌러도 둘이 생기지 않는다). */
export function createDashboard(spec: DashboardSpec, idempotencyKey: string, signal: AbortSignal): Promise<ApiResult<DashboardItem>> {
  return apiRequest<DashboardItem>('/dashboards', { method: 'POST', body: spec, headers: { 'Idempotency-Key': idempotencyKey }, signal });
}

/** 수정. revision이 다르면 412(다른 변경이 먼저 저장됨). */
export function updateDashboard(id: string, spec: DashboardSpec, revision: number, signal: AbortSignal): Promise<ApiResult<DashboardItem>> {
  return apiRequest<DashboardItem>(`/dashboards/${enc(id)}`, { method: 'PUT', body: spec, headers: ifMatch(revision), signal });
}

export function deleteDashboard(id: string, revision: number, signal: AbortSignal): Promise<ApiResult<null>> {
  return apiRequest<null>(`/dashboards/${enc(id)}`, { method: 'DELETE', headers: ifMatch(revision), signal });
}
