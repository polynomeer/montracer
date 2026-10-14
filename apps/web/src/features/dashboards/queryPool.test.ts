import { describe, expect, it } from 'vitest';
import type { ApiResult } from '../../api/client.ts';
import { QueryPool } from './queryPool.ts';

const ok = (v: string): ApiResult<string> => ({ ok: true, status: 200, data: v, meta: { request_id: '', schema_version: 1, partial: false, failed_shards: [], warnings: [] }, nextCursor: null });

function deferredFetcher() {
  const calls: { key: string; signal: AbortSignal; resolve: (v: ApiResult<string>) => void }[] = [];
  const fetcher = (key: string, signal: AbortSignal) =>
    new Promise<ApiResult<string>>((resolve) => {
      calls.push({ key, signal, resolve });
    });
  return { calls, fetcher };
}

const tick = () => new Promise((r) => setTimeout(r, 0));

describe('QueryPool', () => {
  it('같은 key는 요청 하나를 같이 기다린다', async () => {
    const { calls, fetcher } = deferredFetcher();
    const pool = new QueryPool(6, fetcher);
    const a = pool.run('q1', new AbortController().signal);
    const b = pool.run('q1', new AbortController().signal);
    await tick();
    expect(calls).toHaveLength(1);
    calls[0]?.resolve(ok('v'));
    expect(await a).toEqual(ok('v'));
    expect(await b).toEqual(ok('v'));
  });

  it('동시에 max개까지만 돌고, 끝나면 다음이 시작한다', async () => {
    const { calls, fetcher } = deferredFetcher();
    const pool = new QueryPool(2, fetcher);
    const rs = ['a', 'b', 'c', 'd'].map((k) => pool.run(k, new AbortController().signal));
    await tick();
    expect(calls.map((c) => c.key)).toEqual(['a', 'b']);
    expect(pool.active).toBe(2);
    calls[0]?.resolve(ok('a'));
    await rs[0];
    await tick();
    expect(calls.map((c) => c.key)).toEqual(['a', 'b', 'c']);
    expect(pool.active).toBe(2);
  });

  it('기다리던 쪽이 모두 떠나면 요청을 취소하고, 줄에서 기다리던 것은 시작하지 않는다', async () => {
    const { calls, fetcher } = deferredFetcher();
    const pool = new QueryPool(1, fetcher);
    const c1 = new AbortController();
    const c2 = new AbortController();
    const first = pool.run('a', c1.signal);
    const queued = pool.run('b', c2.signal);
    await tick();
    c2.abort();
    await expect(queued).rejects.toThrow('aborted');
    c1.abort();
    await expect(first).rejects.toThrow('aborted');
    expect(calls[0]?.signal.aborted).toBe(true);
    calls[0]?.resolve(ok('late'));
    await tick();
    expect(calls).toHaveLength(1); // 취소된 b는 시작하지 않았다
    expect(pool.active).toBe(0);
  });

  it('하나만 떠나면 같은 요청을 기다리는 다른 쪽은 결과를 받는다', async () => {
    const { calls, fetcher } = deferredFetcher();
    const pool = new QueryPool(6, fetcher);
    const leaving = new AbortController();
    const a = pool.run('q', leaving.signal);
    const b = pool.run('q', new AbortController().signal);
    await tick();
    leaving.abort();
    await expect(a).rejects.toThrow('aborted');
    expect(calls[0]?.signal.aborted).toBe(false);
    calls[0]?.resolve(ok('v'));
    expect(await b).toEqual(ok('v'));
  });
});
