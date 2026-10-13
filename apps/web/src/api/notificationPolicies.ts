// control-api notification policy API (ADR 0053·0055·0056). 쓰기는 Tenant Admin(policies.write), 조회는 monitors.read.
// - 서명 secret 평문은 생성·교체 응답에만 있다. 화면은 이 값을 컴포넌트 상태에만 두고 저장·로그하지 않는다.
// - webhook URL은 host만 돌아온다(전체 주소는 암호문으로만 저장). 수정에서 URL을 빼면 저장된 주소를 그대로 둔다.
import { apiRequest, type ApiResult } from './client.ts';

export interface PolicyItem {
  id: string;
  name: string;
  enabled: boolean;
  revision: number;
  webhook: {
    url: string;
    signing_key_id: string;
    /** secret 교체 뒤 겹침 기간 동안 함께 서명하는 이전 secret id. 겹침이 끝났으면 null */
    previous_signing_key_id?: string | null;
    previous_signing_key_expires_at?: string | null;
  };
  created_at?: string;
  updated_at?: string;
  created_by?: string;
  updated_by?: string;
}

/** 생성·교체 응답. signing_secret은 이 응답에서만 있다(다시 볼 수 없다). */
export interface PolicyWithSecret {
  data: PolicyItem;
  signingSecret: string | null;
  warnings: string[];
}

export interface TestItem {
  id: string;
  policy_id: string;
  status: 'pending' | 'delivered' | 'failed' | 'canceled';
  attempts: number;
  last_status_code: number | null;
  last_error: string | null;
  created_at: string;
  finished_at: string | null;
}

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

/** secret 교체. previousExpiresInSeconds: 0이면 이전 secret을 바로 버린다(최대 86400). */
export function rotateSecret(id: string, revision: number, previousExpiresInSeconds: number, signal: AbortSignal): Promise<ApiResult<PolicyWithSecret>> {
  return withSecret(`/notification-policies/${enc(id)}/rotate-secret`, {
    method: 'POST',
    body: { previous_secret_expires_in_seconds: previousExpiresInSeconds },
    headers: ifMatch(revision),
    signal,
  });
}

export function sendTest(id: string, signal: AbortSignal): Promise<ApiResult<TestItem>> {
  return apiRequest<TestItem>(`/notification-policies/${enc(id)}/test`, { method: 'POST', signal });
}

export function getTest(id: string, testId: string, signal: AbortSignal): Promise<ApiResult<TestItem>> {
  return apiRequest<TestItem>(`/notification-policies/${enc(id)}/tests/${enc(testId)}`, { signal });
}
