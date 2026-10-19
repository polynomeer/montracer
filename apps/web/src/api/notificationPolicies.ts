// control-api notification policy API (ADR 0053·0055·0056). 쓰기는 Tenant Admin(policies.write), 조회는 monitors.read.
// - 서명 secret 평문은 생성·교체 응답에만 있다. 화면은 이 값을 컴포넌트 상태에만 두고 저장·로그하지 않는다.
// - webhook URL은 host만 돌아온다(전체 주소는 암호문으로만 저장). 수정에서 URL을 빼면 저장된 주소를 그대로 둔다.
import type { Schema } from '@montracer/query-schema';
import { apiRequest, type ApiResult } from './client.ts';

/** 알림 policy(원천 NotificationPolicy, ADR 0061). URL은 scheme·host만 보인다. 교체 겹침 중에는 이전 secret id·만료가 있다. */
export type PolicyItem = Schema<'NotificationPolicy'>;

/** 생성·교체 응답. signing_secret은 이 응답에서만 있다(다시 볼 수 없다). */
export interface PolicyWithSecret {
  data: PolicyItem;
  signingSecret: string | null;
  warnings: string[];
}

export type TestItem = Schema<'NotificationTest'>;

export interface PolicyInput {
  name: string;
  enabled: boolean;
  /** 생성은 필수. 수정에서 null이면 저장된 주소를 그대로 둔다 */
  url: string | null;
}

const enc = encodeURIComponent;
const ifMatch = (revision: number) => ({ 'If-Match': `"${String(revision)}"` });

function body(input: PolicyInput) {
  return { name: input.name, enabled: input.enabled, ...(input.url === null ? {} : { webhook: { url: input.url } }) };
}

/** signing_secret은 envelope 최상위에 있다(data 밖). apiRequest는 data·meta만 꺼내므로 원문 응답을 따로 읽는다. */
async function withSecret(
  path: string,
  init: { method: 'POST'; body?: unknown; headers: Record<string, string>; signal: AbortSignal },
): Promise<ApiResult<PolicyWithSecret>> {
  let secret: string | null = null;
  const r = await apiRequest<PolicyItem>(path, {
    ...init,
    onEnvelope: (env: unknown) => {
      const s = (env as { signing_secret?: unknown }).signing_secret;
      secret = typeof s === 'string' ? s : null;
    },
  });
  if (!r.ok) return r;
  return { ...r, data: { data: r.data, signingSecret: secret, warnings: r.meta.warnings } };
}

export function listPolicies(signal: AbortSignal): Promise<ApiResult<PolicyItem[]>> {
  return apiRequest<PolicyItem[]>('/notification-policies?limit=200', { signal });
}

export function getPolicy(id: string, signal: AbortSignal): Promise<ApiResult<PolicyItem>> {
  return apiRequest<PolicyItem>(`/notification-policies/${enc(id)}`, { signal });
}

/** 생성. idempotencyKey는 생성 화면 하나에 하나다(다시 눌러도 두 개가 생기지 않는다). 재생 응답에는 secret이 없다. */
export function createPolicy(input: PolicyInput, idempotencyKey: string, signal: AbortSignal): Promise<ApiResult<PolicyWithSecret>> {
  return withSecret('/notification-policies', { method: 'POST', body: body(input), headers: { 'Idempotency-Key': idempotencyKey }, signal });
}

export function updatePolicy(id: string, input: PolicyInput, revision: number, signal: AbortSignal): Promise<ApiResult<PolicyItem>> {
  return apiRequest<PolicyItem>(`/notification-policies/${enc(id)}`, { method: 'PUT', body: body(input), headers: ifMatch(revision), signal });
}

export function deletePolicy(id: string, revision: number, signal: AbortSignal): Promise<ApiResult<null>> {
  return apiRequest<null>(`/notification-policies/${enc(id)}`, { method: 'DELETE', headers: ifMatch(revision), signal });
}

/**
 * secret 교체. previousExpiresInSeconds: 0이면 이전 secret을 바로 버린다(최대 86400).
 * idempotencyKey는 한 번의 교체 시도마다 하나다 — 응답을 잃고 같은 key로 다시 보내면 다시 바꾸지 않고, secret 없이 돌려받는다(ADR 0060 §1).
 */
export function rotateSecret(
  id: string,
  revision: number,
  previousExpiresInSeconds: number,
  idempotencyKey: string,
  signal: AbortSignal,
): Promise<ApiResult<PolicyWithSecret>> {
  return withSecret(`/notification-policies/${enc(id)}/rotate-secret`, {
    method: 'POST',
    body: { previous_secret_expires_in_seconds: previousExpiresInSeconds },
    headers: { ...ifMatch(revision), 'Idempotency-Key': idempotencyKey },
    signal,
  });
}

/** 시험 전송. idempotencyKey는 사용자가 누른 한 번마다 하나다(같은 key 재전송은 새로 쌓지 않는다, ADR 0060 §1). */
export function sendTest(id: string, idempotencyKey: string, signal: AbortSignal): Promise<ApiResult<TestItem>> {
  return apiRequest<TestItem>(`/notification-policies/${enc(id)}/test`, { method: 'POST', headers: { 'Idempotency-Key': idempotencyKey }, signal });
}

export function getTest(id: string, testId: string, signal: AbortSignal): Promise<ApiResult<TestItem>> {
  return apiRequest<TestItem>(`/notification-policies/${enc(id)}/tests/${enc(testId)}`, { signal });
}
