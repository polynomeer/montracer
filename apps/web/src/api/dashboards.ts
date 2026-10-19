// control-api dashboard API (D02 §14, ADR 0057·0058). 정의만 저장한다 — 위젯 query는 보는 사람의 권한으로 query-api가 실행한다.
// 쓰기는 Idempotency-Key(POST)·If-Match(PUT·DELETE)를 붙인다.
import type { Schema } from '@montracer/query-schema';
import { apiRequest, type ApiResult } from './client.ts';

/** 정규형 DashboardSpec (ADR 0057 §1). 편집기가 만드는 spec도 같은 타입이다 — 원천과 다른 모양을 만들 수 없다. */
export type DashboardSpec = Schema<'DashboardSpec'>;
export type Widget = Schema<'DashboardWidget'>;
/** 서버가 받는 정의 입력(원천 DashboardSpecInput). 편집기의 정규형 spec이 입력으로 받아들여지는지 타입이 본다. */
export type DashboardSpecInput = Schema<'DashboardSpecInput'>;
export type WidgetKind = Widget['kind'];
export type WidgetQuery = NonNullable<Widget['query']>;
export type WidgetDisplay = Widget['display'];
export type TimeOverrideRange = NonNullable<Widget['time_override']>['range'];
export type Variable = DashboardSpec['variables'][number];
export type VariableKind = Variable['kind'];
export type Placement = DashboardSpec['layout'][number];
/** label 조건(and·eq). value가 "$이름"이면 변수다 */
export type DashFilter = Schema<'LabelFilter'>;

export type DashboardSummary = Schema<'DashboardSummary'>;
export type DashboardItem = Schema<'Dashboard'>;

const enc = encodeURIComponent;
const ifMatch = (revision: number) => ({ 'If-Match': `"${String(revision)}"` });

export function listDashboards(signal: AbortSignal): Promise<ApiResult<DashboardSummary[]>> {
  return apiRequest<DashboardSummary[]>('/dashboards?limit=200', { signal });
}

export function getDashboard(id: string, signal: AbortSignal): Promise<ApiResult<DashboardItem>> {
  return apiRequest<DashboardItem>(`/dashboards/${enc(id)}`, { signal });
}

/** 생성. idempotencyKey는 편집 화면 하나에 하나다(다시 눌러도 둘이 생기지 않는다). */
export function createDashboard(spec: DashboardSpecInput, idempotencyKey: string, signal: AbortSignal): Promise<ApiResult<DashboardItem>> {
  return apiRequest<DashboardItem>('/dashboards', { method: 'POST', body: spec, headers: { 'Idempotency-Key': idempotencyKey }, signal });
}

/** 수정. revision이 다르면 412(다른 변경이 먼저 저장됨). */
export function updateDashboard(id: string, spec: DashboardSpecInput, revision: number, signal: AbortSignal): Promise<ApiResult<DashboardItem>> {
  return apiRequest<DashboardItem>(`/dashboards/${enc(id)}`, { method: 'PUT', body: spec, headers: ifMatch(revision), signal });
}

export function deleteDashboard(id: string, revision: number, signal: AbortSignal): Promise<ApiResult<null>> {
  return apiRequest<null>(`/dashboards/${enc(id)}`, { method: 'DELETE', headers: ifMatch(revision), signal });
}
