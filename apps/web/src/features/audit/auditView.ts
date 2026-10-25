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
