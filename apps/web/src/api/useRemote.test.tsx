import { act, render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { ApiResult } from './client.ts';
import type { Meta } from './types.ts';
import { useRemote } from './useRemote.ts';

const meta: Meta = { request_id: 'r', schema_version: 1, partial: false, failed_shards: [], warnings: [] };

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

const ok = (data: string): ApiResult<string> => ({ ok: true, status: 200, data, meta, nextCursor: null });

function Probe({
  k,
  r = 0,
  fetchers,
}: {
  k: string;
  r?: number;
  fetchers: Record<string, (s: AbortSignal) => Promise<ApiResult<string>>>;
}) {
  const s = useRemote(k, r, (signal) => fetchers[`${k}${String(r)}`]?.(signal) ?? fetchers[k]?.(signal) ?? Promise.reject(new Error('no fetcher')));
  return (
    <p>
      {s.status}|{s.data ?? '-'}|{s.refreshing ? 'refreshing' : 'idle'}|{s.error?.status ?? '-'}
    </p>
  );
}

describe('useRemote', () => {
  it('늦게 도착한 이전 응답이 최신 결과를 덮어쓰지 않는다 (D05 §05)', async () => {
    const a = deferred<ApiResult<string>>();
    const b = deferred<ApiResult<string>>();
    const signals: AbortSignal[] = [];
    const fetchers = {
      a: (s: AbortSignal) => {
        signals.push(s);
        return a.promise;
      },
      b: () => b.promise,
    };
    const { rerender } = render(<Probe k="a" fetchers={fetchers} />);
    rerender(<Probe k="b" fetchers={fetchers} />);
    expect(signals[0]?.aborted).toBe(true);
    await act(async () => b.resolve(ok('new')));
    await act(async () => a.resolve(ok('old')));
    expect(screen.getByText('success|new|idle|-')).toBeTruthy();
  });

  it('조회 조건이 바뀌면 이전 결과를 버리고 다시 loading', async () => {
    const second = deferred<ApiResult<string>>();
    const fetchers = { a: () => Promise.resolve(ok('first')), b: () => second.promise };
    const { rerender } = render(<Probe k="a" fetchers={fetchers} />);
    expect(await screen.findByText('success|first|idle|-')).toBeTruthy();
    rerender(<Probe k="b" fetchers={fetchers} />);
    expect(screen.getByText('loading|-|idle|-')).toBeTruthy();
  });

  it('같은 조건을 새로고침하는 동안·실패 후에도 마지막 성공 데이터를 유지한다 (stale)', async () => {
    const second = deferred<ApiResult<string>>();
    const fetchers = {
      a0: () => Promise.resolve(ok('first')),
      a1: () => second.promise,
    };
    const { rerender } = render(<Probe k="a" r={0} fetchers={fetchers} />);
    expect(await screen.findByText('success|first|idle|-')).toBeTruthy();
    rerender(<Probe k="a" r={1} fetchers={fetchers} />);
    expect(screen.getByText('success|first|refreshing|-')).toBeTruthy();
    await act(async () =>
      second.resolve({ ok: false, status: 503, error: { code: 'UNAVAILABLE', message: 'x', request_id: 'r', retryable: true }, retryAfterSeconds: null }),
    );
    expect(screen.getByText('error|first|idle|503')).toBeTruthy();
  });
});
