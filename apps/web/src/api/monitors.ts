// control-api의 monitor·notification policy API (D02 §14·§20, ADR 0049·0052·0053·0054).
// 쓰기는 Idempotency-Key(POST)와 If-Match(PUT·DELETE)를 붙인다. 응답의 null은 0으로 바꾸지 않는다(계약 6).
import { apiRequest, type ApiResult } from './client.ts';

export type Operator = 'gt' | 'gte' | 'lt' | 'lte';
export type MonitorKind = 'error_ratio' | 'metric';
export type GroupStateName = 'OK' | 'PENDING' | 'ALERT' | 'RECOVERING' | 'NO_DATA' | 'EVALUATION_ERROR';
export type SummaryState = GroupStateName | 'NOT_EVALUATED' | 'DISABLED';

export interface FilterNode {
  op: 'and' | 'eq';
  field?: string;
  value?: string;
  args?: FilterNode[];
}

/** 정규형 MonitorSpec(서버 internal/monitor). */
export interface MonitorSpec {
  name: string;
  enabled: boolean;
  query: { kind: MonitorKind; metric: string; aggregation: string | null; filter: FilterNode | null; group_by: string[] };
  window_seconds: number;
  evaluation_seconds: number;
  condition: { operator: Operator; threshold: number };
  minimum_requests: number | null;
  for_seconds: number;
  recovery_evaluations: number;
  no_data: { action: 'no_data' | 'alert'; after_seconds: number | null };
  notification_policy_id: string | null;
}

export interface EvaluationItem {
  slot: string;
  status: 'evaluated' | 'no_data' | 'error';
  reason: string | null;
  window_start: string | null;
  window_end: string | null;
  groups: number;
  alerting: number;
  duration_ms: number;
}

export interface MonitorStatus {
  state: SummaryState;
  groups_by_state: Partial<Record<GroupStateName, number>>;
  last_evaluation: EvaluationItem | null;
  stale: boolean;
}

export interface MonitorItem {
  id: string;
  name: string;
  enabled: boolean;
  revision: number;
  spec: MonitorSpec;
  created_at: string;
  updated_at: string;
  created_by: string;
  updated_by: string;
  status: MonitorStatus | null;
}

export interface GroupItem {
  group_hash: string;
  labels: Record<string, string>;
  state: GroupStateName;
  episode_id: string | null;
  episode_reason: 'violation' | 'no_data' | null;
  violation_since: string | null;
  nodata_since: string | null;
  last_window_end: string | null;
  last_value: number | null;
  last_reason: string | null;
  updated_at: string;
}

export interface TransitionItem {
  at: string;
  group_hash: string;
  labels: Record<string, string>;
  from: string;
  to: string | null;
  opened: boolean;
  closed: boolean;
  episode_id: string | null;
  reason: string | null;
  value: number | null;
}

export interface NotificationItem {
  id: string;
  policy_id: string;
  event_type: 'alert.opened' | 'alert.resolved' | 'alert.renotified';
  /** 재알림 번호(1..3). 그 밖은 null */
  renotify_sequence: number | null;
  group_hash: string;
  episode_id: string;
  status: 'pending' | 'delivered' | 'failed' | 'canceled';
  attempts: number;
  last_status_code: number | null;
  last_error: string | null;
  created_at: string;
  finished_at: string | null;
}

export interface Timeline {
  from: string;
  to: string;
  evaluations: EvaluationItem[];
  transitions: TransitionItem[];
  notifications: NotificationItem[];
  evaluations_truncated: boolean;
  transitions_truncated: boolean;
  notifications_truncated: boolean;
}

export interface DryRunInterval {
  start: string;
  end: string | null;
  reason: 'violation' | 'no_data';
}

export interface DryRunGroup {
  group_key: string;
  labels: Record<string, string>;
  firing_intervals: DryRunInterval[];
  firing_intervals_omitted: number;
  firing_seconds: number;
  final_state: GroupStateName;
  evaluated: number;
  no_data: number;
  partial: number;
  first_value_at: string | null;
  max_value: number | null;
  last_value: number | null;
  last_reason: string;
  transitions: number;
}

export interface DryRun {
  from: string | null;
  to: string | null;
  warmup_seconds: number;
  data_until: string | null;
  step_seconds: number;
  evaluations: number;
  coverage: { evaluated: number; no_data: number; error: number };
  groups: DryRunGroup[];
  groups_total: number;
  groups_omitted: number;
  reason: string;
}

export interface ValidateResult {
  normalized_spec: MonitorSpec;
  warnings: string[];
  dry_run: DryRun | null;
}

export type { PolicyItem } from './notificationPolicies.ts';

const enc = encodeURIComponent;

export function listMonitors(cursor: string | null, signal: AbortSignal): Promise<ApiResult<MonitorItem[]>> {
  const q = new URLSearchParams({ limit: '200' });
  if (cursor !== null) q.set('cursor', cursor);
  return apiRequest<MonitorItem[]>(`/monitors?${q.toString()}`, { signal });
}

export function getMonitor(id: string, signal: AbortSignal): Promise<ApiResult<MonitorItem>> {
  return apiRequest<MonitorItem>(`/monitors/${enc(id)}`, { signal });
}

export function monitorGroups(id: string, signal: AbortSignal): Promise<ApiResult<GroupItem[]>> {
  return apiRequest<GroupItem[]>(`/monitors/${enc(id)}/groups?limit=200`, { signal });
}

export function monitorTimeline(id: string, from: string, to: string, signal: AbortSignal): Promise<ApiResult<Timeline>> {
  return apiRequest<Timeline>(`/monitors/${enc(id)}/timeline?${new URLSearchParams({ from, to }).toString()}`, { signal });
}

export function validateMonitor(spec: unknown, signal: AbortSignal): Promise<ApiResult<ValidateResult>> {
  return apiRequest<ValidateResult>('/monitors/validate', { method: 'POST', body: spec, signal });
}

/** 생성. idempotencyKey는 편집 화면 하나에 하나다(같은 저장을 다시 눌러도 두 개가 생기지 않게, D02 §20). */
export function createMonitor(spec: unknown, idempotencyKey: string, signal: AbortSignal): Promise<ApiResult<MonitorItem>> {
  return apiRequest<MonitorItem>('/monitors', { method: 'POST', body: spec, signal, headers: { 'Idempotency-Key': idempotencyKey } });
}

/** 수정. revision이 다르면 412(다른 변경이 먼저 저장됨). */
export function updateMonitor(id: string, spec: unknown, revision: number, signal: AbortSignal): Promise<ApiResult<MonitorItem>> {
  return apiRequest<MonitorItem>(`/monitors/${enc(id)}`, { method: 'PUT', body: spec, signal, headers: { 'If-Match': `"${String(revision)}"` } });
}

export function deleteMonitor(id: string, revision: number, signal: AbortSignal): Promise<ApiResult<null>> {
  return apiRequest<null>(`/monitors/${enc(id)}`, { method: 'DELETE', signal, headers: { 'If-Match': `"${String(revision)}"` } });
}

export { listPolicies } from './notificationPolicies.ts';
