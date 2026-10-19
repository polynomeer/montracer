import { describe, expect, it } from 'vitest';
import { checkExchange, takeContractViolations } from './contract';
import { queryMeta, serviceFixture } from './fixtures';

const json = (status: number, body: unknown, type = 'application/json') => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': type } });
const ok = { data: serviceFixture({ name: 'checkout' }), meta: queryMeta() };
const URL_ = '/api/v1/services/aaaaaaaa-0000-4000-8000-000000000001';
const err = (code: string) => ({ error: { code, message: 'x', request_id: 'r', retryable: false } });

async function check(input: RequestInfo | URL, init: RequestInit | undefined, res: Response): Promise<string[]> {
  takeContractViolations();
  await checkExchange(input, init, res);
  return takeContractViolations();
}

// consumer 검사기가 헛돌지 않는다: operation을 찾고, 어긋난 fixture를 잡는다.
describe('consumer contract 검사기', () => {
  it('맞는 응답은 통과하고 어긋난 응답은 잡는다', async () => {
    expect(await check(URL_, undefined, json(200, ok))).toEqual([]);
    const { status: _status, ...noStatus } = ok.data;
    expect(await check(URL_, undefined, json(200, { ...ok, data: noStatus }))).toHaveLength(1); // 필수 field 누락
    expect(await check(URL_, undefined, json(200, { ...ok, data: { ...ok.data, extra: 1 } }))).toHaveLength(1); // 선언 안 된 field
    expect(await check(URL_, undefined, json(200, { ...ok, meta: { ...ok.meta, coverage: 0, watermark: 0 } }))).toHaveLength(1); // null 자리에 0
    expect(await check(URL_, undefined, json(418, {}))).toHaveLength(1); // 선언 안 된 status
    expect(await check(URL_, undefined, json(200, ok, 'text/plain'))).toHaveLength(1); // JSON이 아닌 응답
  });

  it('status와 error.code를 묶는다', async () => {
    expect(await check(URL_, undefined, json(404, err('NOT_FOUND')))).toEqual([]);
    expect(await check(URL_, undefined, json(404, err('RATE_LIMITED')))).toHaveLength(1);
  });

  it('받아들인 요청의 본문과 필수 header를 본다', async () => {
    const created = json(201, {}, 'application/json');
    const v = await check('/api/v1/dashboards', { method: 'POST', body: '{"title":1}' }, created);
    expect(v.some((m) => m.includes('Idempotency-Key'))).toBe(true);
    expect(v.some((m) => m.includes('request'))).toBe(true);
  });

  it('Request 객체의 method를 쓰고, 원천 밖 경로는 보지 않는다', async () => {
    expect(await check(new Request('http://localhost/api/v1/dashboards', { method: 'POST' }), undefined, json(418, {}))).toHaveLength(1);
    expect(await check('/assets/app.js', undefined, json(200, {}))).toEqual([]);
  });
});
