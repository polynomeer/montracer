import { cleanup } from '@testing-library/react';
import { afterEach, beforeEach, vi } from 'vitest';
import { contractFetch, takeContractViolations } from './contract';
import { sessionMock } from './sessionMock';

const notFound = () =>
  new Response(JSON.stringify({ error: { code: 'NOT_FOUND', message: 'login disabled', request_id: 'req-setup', retryable: false } }), {
    status: 404,
    headers: { 'Content-Type': 'application/json' },
  });

// 로그인 꺼짐 기본값: 조직 화면의 session 확인(GET /auth/session)은 404다(시험의 fetch 대역보다 먼저).
// fetch를 대역하지 않는 시험도 같은 답을 받는다(연결 오류로 "확인 실패" 화면이 되지 않게).
function withSessionDefault(fetchImpl: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>) {
  return (input: RequestInfo | URL, init?: RequestInit) =>
    !sessionMock.passThrough && String(input).endsWith('/api/v1/auth/session') ? Promise.resolve(notFound()) : fetchImpl(input, init);
}

beforeEach(() => {
  sessionMock.passThrough = false;
  vi.stubGlobal('fetch', () => Promise.resolve(notFound()));
});

// 시험이 넣는 fetch 대역은 모두 OpenAPI 원천으로 검사한다(consumer contract, ADR 0059 §4).
// 감싼 함수도 vi.fn이라 vi.mocked(fetch).mock.calls·mockImplementation은 그대로 쓸 수 있다
// (mockImplementation으로 바꾼 구현은 검사하지 않는다).
const stubGlobal = vi.stubGlobal.bind(vi);
vi.stubGlobal = ((name: string | symbol | number, value: unknown) =>
  stubGlobal(
    name,
    name === 'fetch' && typeof value === 'function'
      ? vi.fn(withSessionDefault(contractFetch(value as (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>)))
      : value,
  )) as typeof vi.stubGlobal;

afterEach(() => {
  cleanup();
  document.documentElement.removeAttribute('data-theme');
  window.localStorage.clear();
  const violations = takeContractViolations();
  if (violations.length > 0) {
    throw new Error(`UI fixture가 OpenAPI 원천(api/openapi/montracer.yaml)과 다르다 (ADR 0059):\n  - ${violations.join('\n  - ')}`);
  }
});
