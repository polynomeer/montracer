import { cleanup } from '@testing-library/react';
import { afterEach, vi } from 'vitest';
import { contractFetch, takeContractViolations } from './contract';

// 시험이 넣는 fetch 대역은 모두 OpenAPI 원천으로 검사한다(consumer contract, ADR 0059 §4).
// 감싼 함수도 vi.fn이라 vi.mocked(fetch).mock.calls·mockImplementation은 그대로 쓸 수 있다
// (mockImplementation으로 바꾼 구현은 검사하지 않는다).
const stubGlobal = vi.stubGlobal.bind(vi);
vi.stubGlobal = ((name: string | symbol | number, value: unknown) =>
  stubGlobal(
    name,
    name === 'fetch' && typeof value === 'function'
      ? vi.fn(contractFetch(value as (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>))
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
