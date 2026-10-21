// control-api API key 관리 API (ADR 0069). 조회는 Tenant Admin(keys.read), 발급·폐기는 Tenant Admin + 15분 안의 MFA step-up.
// - 원문 token은 발급 응답에만 있다. 화면은 이 값을 컴포넌트 상태에만 두고 저장·로그하지 않는다.
import type { Schema } from '@montracer/query-schema';
import { apiRequest, type ApiResult } from './client.ts';

export type KeyItem = Schema<'ApiKey'>;
export type KeyKind = KeyItem['kind'];

/** 발급 화면이 고를 수 있는 scope와 설명(authz.IngestScopes·APIKeyScopes와 같다 — 서버가 다시 검사한다). */
export const INGEST_SCOPES: readonly { scope: string; label: string }[] = [
  { scope: 'ingest.traces', label: 'trace 수집' },
  { scope: 'ingest.metrics', label: 'metric 수집' },
  { scope: 'ingest.logs', label: 'log 수집' },
];

export const API_SCOPES: readonly { scope: string; label: string }[] = [
  { scope: 'telemetry.read', label: 'telemetry 조회(trace·log·metric·서비스)' },
  { scope: 'dashboards.read', label: 'dashboard 조회' },
  { scope: 'dashboards.write', label: 'dashboard 만들기·편집' },
  { scope: 'monitors.read', label: 'monitor·알림 policy 조회' },
  { scope: 'monitors.write', label: 'monitor 만들기·편집' },
  { scope: 'saved_searches.write', label: '저장 검색' },
  { scope: 'silences.write', label: '경보 silence' },
  { scope: 'deployments.write', label: '배포 이벤트 기록' },
  { scope: 'policies.propose', label: '수집 정책 제안' },
  { scope: 'policies.read', label: '정책 조회' },
  { scope: 'policies.write', label: '정책 변경(알림 policy 포함)' },
  { scope: 'members.read', label: '멤버 조회' },
  { scope: 'keys.read', label: 'key 목록 조회' },
  { scope: 'audit.read', label: '감사 조회(전체)' },
  { scope: 'audit.operations.read', label: '감사 조회(운영 범주)' },
  { scope: 'usage.read', label: '사용량 조회' },
];

export interface KeyInput {
  kind: KeyKind;
  name: string | null;
  scopes: string[];
  environments: string[];
  expiresInDays: number;
}

export interface KeyCreated {
  data: KeyItem;
  /** 발급 응답에만 있다. 같은 Idempotency-Key 재생 응답에는 없다(null). */
  token: string | null;
}

export function listKeys(signal: AbortSignal): Promise<ApiResult<KeyItem[]>> {
  return apiRequest<KeyItem[]>('/keys', { signal });
}

/** 발급. idempotencyKey는 사용자가 누른 한 번마다 하나다. token은 envelope 최상위(data 밖)에 있어 따로 읽는다. */
export async function createKey(input: KeyInput, idempotencyKey: string): Promise<ApiResult<KeyCreated>> {
  let token: string | null = null;
  const r = await apiRequest<KeyItem>('/keys', {
    method: 'POST',
    body: {
      kind: input.kind,
      ...(input.name === null ? {} : { name: input.name }),
      scopes: input.scopes,
      ...(input.environments.length === 0 ? {} : { environments: input.environments }),
      expires_in_days: input.expiresInDays,
    },
    headers: { 'Idempotency-Key': idempotencyKey },
    onEnvelope: (env: unknown) => {
      const t = (env as { token?: unknown }).token;
      token = typeof t === 'string' ? t : null;
    },
  });
  if (!r.ok) return r;
  return { ...r, data: { data: r.data, token } };
}

/** 폐기. graceSeconds 뒤부터 무효다(0이면 바로, 최대 86,400 — 교체 때 겹침). */
export function revokeKey(keyId: string, graceSeconds: number): Promise<ApiResult<null>> {
  const q = graceSeconds > 0 ? `?grace_seconds=${String(graceSeconds)}` : '';
  return apiRequest<null>(`/keys/${encodeURIComponent(keyId)}${q}`, { method: 'DELETE' });
}

/**
 * key 상태(지금 기준). 만료·폐기 시각은 서버 값 그대로다 — 화면은 그 시각과 지금을 비교만 한다.
 * 실제로 끝나는 시각은 만료와 폐기 중 이른 쪽이다. 시각을 읽지 못하면 unknown이다(정상으로 치지 않는다, 계약 6).
 */
export type KeyStatus =
  | { kind: 'valid' }
  | { kind: 'revoking'; at: string }
  | { kind: 'revoked'; at: string }
  | { kind: 'expired'; at: string }
  | { kind: 'unknown' };

export function keyStatus(k: KeyItem, nowMs: number): KeyStatus {
  const expires = Date.parse(k.expires_at);
  const revoked = k.revoked_at === null ? Number.POSITIVE_INFINITY : Date.parse(k.revoked_at);
  if (!Number.isFinite(expires) || Number.isNaN(revoked)) return { kind: 'unknown' };
  if (revoked <= expires) {
    // 폐기가 먼저(또는 같은 때) 온다
    if (k.revoked_at !== null) return revoked > nowMs ? { kind: 'revoking', at: k.revoked_at } : { kind: 'revoked', at: k.revoked_at };
  }
  return expires > nowMs ? { kind: 'valid' } : { kind: 'expired', at: k.expires_at };
}

/** 목록의 상태가 다음에 바뀌는 시각(ms). 없으면 null. 그때 다시 그린다. */
export function nextStatusChange(keys: readonly KeyItem[], nowMs: number): number | null {
  let next: number | null = null;
  for (const k of keys) {
    for (const t of [k.expires_at, k.revoked_at]) {
      if (t === null) continue;
      const ms = Date.parse(t);
      if (Number.isFinite(ms) && ms > nowMs && (next === null || ms < next)) next = ms;
    }
  }
  return next;
}

/** 목록 상한(서버 ListKeys). 이만큼 왔으면 더 오래된 key가 잘렸을 수 있다. */
export const KEY_LIST_LIMIT = 1000;
