import type { Schema } from '@montracer/query-schema';
import { setCSRFToken } from './authState.ts';
import { apiRequest, type ApiResult } from './client.ts';

// 사람 로그인 session API (control-api, ADR 0064). 응답 타입은 원천에서 생성한 타입이다(ADR 0061).
export type SessionInfo = Schema<'SessionResponse'>['data'];
export type Membership = SessionInfo['memberships'][number];
export type Role = Schema<'Role'>;

/** GET /api/v1/auth/session. 성공하면 CSRF token을 기억한다. 404 = 로그인 꺼짐(API key 모드), 401 = 로그인 필요. */
export async function getSession(signal?: AbortSignal): Promise<ApiResult<SessionInfo>> {
  const r = await apiRequest<SessionInfo>('/auth/session', signal === undefined ? {} : { signal });
  setCSRFToken(r.ok ? r.data.csrf_token : null);
  return r;
}

export function logout(): Promise<ApiResult<null>> {
  return apiRequest<null>('/auth/logout', { method: 'POST' });
}

/** 다른 membership 조직으로 바꾼다. 새 session의 CSRF token을 기억한다. */
export async function switchTenant(tenantId: string): Promise<ApiResult<SessionInfo>> {
  const r = await apiRequest<SessionInfo>('/auth/session/tenant', { method: 'POST', body: { tenant_id: tenantId } });
  if (r.ok) setCSRFToken(r.data.csrf_token);
  return r;
}

/** 화면 URL의 조직 이름(/o/{org}): 표시 이름, 없으면 tenant id. */
export function orgSlug(m: { tenant_id: string; tenant_name: string | null }): string {
  return m.tenant_name ?? m.tenant_id;
}

export const ROLE_LABEL: Record<Role, string> = {
  viewer: 'Viewer',
  developer: 'Developer',
  operator: 'Operator',
  tenant_admin: 'Tenant Admin',
  security_auditor: 'Security Auditor',
};
