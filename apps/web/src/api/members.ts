// control-api 멤버·초대 API (ADR 0067)와 MFA step-up 시작 (ADR 0066).
// - 조회는 Tenant Admin·Security Auditor, 변경은 Tenant Admin + 15분 안의 step-up이다. step-up이 없으면 403에
//   details.step_up_required가 온다 — 화면은 재인증을 안내한다(isStepUpRequired).
// - 초대 링크(invitation_url)는 생성 응답에만 있다. 화면은 이 값을 컴포넌트 상태에만 두고 저장·로그하지 않는다.
import type { Schema } from '@montracer/query-schema';
import { apiRequest, type ApiResult } from './client.ts';

export type MemberItem = Schema<'Member'>;
export type MemberRole = Schema<'MemberRole'>;
export type InvitationItem = Schema<'Invitation'>;
export type InvitableRole = InvitationItem['role'];

/** 초대로 줄 수 있는 역할(tenant_admin은 가입 뒤에 올린다, ADR 0067 §4). */
export const INVITABLE_ROLES: readonly InvitableRole[] = ['viewer', 'developer', 'operator', 'security_auditor'];

export interface InvitationCreated {
  data: InvitationItem;
  /** 생성 응답에만 있다. 같은 Idempotency-Key 재생 응답에는 없다(null). */
  invitationURL: string | null;
}

export interface InvitationStart {
  authorization_url: string;
  tenant_name: string | null;
  role: InvitableRole;
}

const enc = encodeURIComponent;
const ifMatch = (revision: number) => ({ 'If-Match': `"${String(revision)}"` });

/** 403 중 step-up이 없어서인지(재인증하면 할 수 있다). */
export function isStepUpRequired(r: ApiResult<unknown>): boolean {
  return !r.ok && r.status === 403 && (r.error.details as { step_up_required?: unknown } | undefined)?.step_up_required === true;
}

/** 멤버 목록(user_id 순). 1,000명이 넘으면 next_cursor가 있다. */
export function listMembers(signal: AbortSignal, cursor?: string): Promise<ApiResult<MemberItem[]>> {
  return apiRequest<MemberItem[]>(`/members?limit=1000${cursor === undefined ? '' : `&cursor=${enc(cursor)}`}`, { signal });
}

export function updateMemberRole(userId: string, role: MemberRole, revision: number): Promise<ApiResult<MemberItem>> {
  return apiRequest<MemberItem>(`/members/${enc(userId)}`, { method: 'PATCH', body: { role }, headers: ifMatch(revision) });
}

export function removeMember(userId: string, revision: number): Promise<ApiResult<null>> {
  return apiRequest<null>(`/members/${enc(userId)}`, { method: 'DELETE', headers: ifMatch(revision) });
}

export function listInvitations(signal: AbortSignal): Promise<ApiResult<InvitationItem[]>> {
  return apiRequest<InvitationItem[]>('/invitations', { signal });
}

/** 초대 만들기. idempotencyKey는 사용자가 누른 한 번마다 하나다. 링크는 envelope 최상위(data 밖)에 있어 따로 읽는다. */
export async function createInvitation(role: InvitableRole, label: string | null, idempotencyKey: string): Promise<ApiResult<InvitationCreated>> {
  let url: string | null = null;
  const r = await apiRequest<InvitationItem>('/invitations', {
    method: 'POST',
    body: label === null ? { role } : { role, label },
    headers: { 'Idempotency-Key': idempotencyKey },
    onEnvelope: (env: unknown) => {
      const u = (env as { invitation_url?: unknown }).invitation_url;
      url = typeof u === 'string' ? u : null;
    },
  });
  if (!r.ok) return r;
  return { ...r, data: { data: r.data, invitationURL: url } };
}

export function revokeInvitation(id: string): Promise<ApiResult<null>> {
  return apiRequest<null>(`/invitations/${enc(id)}`, { method: 'DELETE' });
}

/** 초대 수락 시작: 들어갈 조직·역할과 IdP 로그인 URL. token은 본문으로만 보낸다(URL·로그에 남지 않게). */
export function startInvitation(token: string, signal?: AbortSignal): Promise<ApiResult<InvitationStart>> {
  return apiRequest<InvitationStart>('/auth/invitation', { method: 'POST', body: { token }, ...(signal === undefined ? {} : { signal }) });
}

/** MFA step-up 시작(ADR 0066): IdP 재인증 URL. 재인증 뒤 returnTo로 돌아온다. */
export function startStepUp(returnTo: string): Promise<ApiResult<{ authorization_url: string }>> {
  return apiRequest<{ authorization_url: string }>('/auth/step-up', { method: 'POST', body: { return_to: returnTo } });
}
