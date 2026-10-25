// control-api 감사 이벤트 조회 (D02 §14, ADR 0034).
// - 범주는 principal 권한으로 제한된다: Tenant Admin·Security Auditor는 둘 다, Operator는 operations만.
//   볼 수 없는 범주를 명시해 요청하면 403이다(빈 결과로 바꾸지 않는다 — 계약 6).
// - cursor는 서명돼 있고 15분 뒤 만료된다. 만료·위조·조건 불일치는 모두 400(field cursor)이다.
import type { Schema } from '@montracer/query-schema';
import { apiRequest, type ApiResult } from './client.ts';

export type AuditEvent = Schema<'AuditEvent'>;
export type AuditCategory = AuditEvent['category'];
export type ActorKind = AuditEvent['actor_kind'];

/** 범위 한도(ADR 0034 §1: 보안 감사 보존 365일 + 경계 하루). */
export const MAX_AUDIT_RANGE_MS = 366 * 24 * 3600 * 1000;
export const AUDIT_PAGE_LIMIT = 100;

export interface AuditQuery {
  fromIso: string;
  toIso: string;
  category: AuditCategory | null;
  action: string | null;
}

export function listAuditEvents(q: AuditQuery, cursor: string | null, signal: AbortSignal): Promise<ApiResult<AuditEvent[]>> {
  const p = new URLSearchParams({ from: q.fromIso, to: q.toIso, limit: String(AUDIT_PAGE_LIMIT) });
  if (q.category !== null) p.set('category', q.category);
  if (q.action !== null) p.set('action', q.action);
  if (cursor !== null) p.set('cursor', cursor);
  return apiRequest<AuditEvent[]>(`/audit-events?${p.toString()}`, { signal });
}

/** 400 중 cursor 문제인지(만료 15분·권한 변경 등 — 처음부터 다시 읽어야 한다). */
export function isCursorRejected(r: ApiResult<unknown>): boolean {
  if (r.ok || r.status !== 400) return false;
  const v = (r.error.details as { field_violations?: { field: string }[] } | undefined)?.field_violations ?? [];
  return v.some((f) => f.field === 'cursor');
}
