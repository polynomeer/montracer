// control-api의 monitor·notification policy API (D02 §14·§20, ADR 0049·0052·0053·0054).
// 쓰기는 Idempotency-Key(POST)와 If-Match(PUT·DELETE)를 붙인다. 응답의 null은 0으로 바꾸지 않는다(계약 6).
import type { operations, Schema } from '@montracer/query-schema';
import { apiRequest, type ApiResult } from './client.ts';

export type Operator = MonitorSpec['condition']['operator'];
export type MonitorKind = MonitorSpec['query']['kind'];
export type GroupStateName = GroupItem['state'];
export type SummaryState = MonitorStatus['state'];
/** label 조건(and·eq) */
export type FilterNode = Schema<'LabelFilter'>;

// 응답 타입은 원천에서 생성한 타입의 별칭이다(@montracer/query-schema, ADR 0061). 손으로 옮기지 않는다.

/** 정규형 MonitorSpec(서버 internal/monitor). */
export type MonitorSpec = Schema<'MonitorSpec'>;
export type EvaluationItem = Schema<'MonitorEvaluation'>;
export type MonitorStatus = Schema<'MonitorStatus'>;
export type MonitorItem = Schema<'Monitor'>;
export type GroupItem = Schema<'MonitorGroup'>;
export type TransitionItem = Schema<'MonitorTransition'>;
/** 알림 기록. event_type은 원천이 pattern으로 둔다 — 롤백한 서버가 `alert.renotified.<n>`을 줄 수 있어 모르는 값은 원문으로 보인다. */
export type NotificationItem = Schema<'MonitorNotification'>;
export type Timeline = Schema<'MonitorTimeline'>;
export type DryRun = Schema<'MonitorDryRun'>;
export type DryRunGroup = Schema<'MonitorDryRunGroup'>;
export type DryRunInterval = DryRunGroup['firing_intervals'][number];
export type ValidateResult = operations['validateMonitor']['responses'][200]['content']['application/json']['data'];

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

export function validateMonitor(spec: Schema<'MonitorSpecInput'>, signal: AbortSignal): Promise<ApiResult<ValidateResult>> {
  return apiRequest<ValidateResult>('/monitors/validate', { method: 'POST', body: spec, signal });
}

/** 생성. idempotencyKey는 편집 화면 하나에 하나다(같은 저장을 다시 눌러도 두 개가 생기지 않게, D02 §20). */
export function createMonitor(spec: Schema<'MonitorSpecInput'>, idempotencyKey: string, signal: AbortSignal): Promise<ApiResult<MonitorItem>> {
  return apiRequest<MonitorItem>('/monitors', { method: 'POST', body: spec, signal, headers: { 'Idempotency-Key': idempotencyKey } });
}

/** 수정. revision이 다르면 412(다른 변경이 먼저 저장됨). */
export function updateMonitor(id: string, spec: Schema<'MonitorSpecInput'>, revision: number, signal: AbortSignal): Promise<ApiResult<MonitorItem>> {
  return apiRequest<MonitorItem>(`/monitors/${enc(id)}`, { method: 'PUT', body: spec, signal, headers: { 'If-Match': `"${String(revision)}"` } });
}

export function deleteMonitor(id: string, revision: number, signal: AbortSignal): Promise<ApiResult<null>> {
  return apiRequest<null>(`/monitors/${enc(id)}`, { method: 'DELETE', signal, headers: { 'If-Match': `"${String(revision)}"` } });
}

export { listPolicies } from './notificationPolicies.ts';
