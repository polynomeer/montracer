// 감사 화면의 표시 규칙(순수 함수). 이름은 API 값과 1:1이다 — 모르는 값을 짐작해 바꾸지 않는다.
import type { ActorKind, AuditCategory } from '../../api/audit.ts';

export const CATEGORY_LABEL: Record<AuditCategory, string> = {
  operations: '운영',
  security: '보안',
};

export const ACTOR_LABEL: Record<ActorKind, string> = {
  user: '사용자',
  api_key: 'API key',
  ingest_key: '수집 key',
  system: '시스템',
  operator: 'Montracer 운영자',
};

/** 서버가 새 값을 더해도 빈칸이 되지 않게 원문으로 보인다(짐작해 바꾸지 않는다). */
export const categoryLabel = (c: string): string => (CATEGORY_LABEL as Record<string, string | undefined>)[c] ?? c;
export const actorLabel = (k: string): string => (ACTOR_LABEL as Record<string, string | undefined>)[k] ?? k;

export const ACTION_RULE = '공백·제어 문자 없이 128 byte까지 입력하세요(정확히 일치하는 action).';

/** 보안 감사 기록 보존 기간(D04 §04 제안 365일). 그보다 오래된 구간의 빈 결과는 "없었음"이 아닐 수 있다. */
export const SECURITY_RETENTION_MS = 365 * 24 * 3600 * 1000;

/** URL의 category. 모르는 값은 조건 없음(볼 수 있는 범주 전체)으로 본다. */
export function parseCategory(v: string | null): AuditCategory | null {
  return v === 'operations' || v === 'security' ? v : null;
}

/** action 조건: 공백·제어 문자 없는 출력 가능 문자, 128 byte 이하(ADR 0034 §1). */
export function validAction(v: string): boolean {
  if (v === '' || new TextEncoder().encode(v).length > 128) return false;
  // eslint-disable-next-line no-control-regex
  return !/[\s\u0000-\u001f\u007f]/u.test(v);
}
