import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useDebouncedCommit } from './useDebouncedCommit.ts';

type Props = { draft: string; pending: boolean; other: string };

// PS-0012: 타이머가 예약한 render의 commit(그 render의 조건)을 불러서, 기다리는 사이 바뀐 다른 조건을 되돌렸다.
describe('useDebouncedCommit', () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  const setup = (initial: Props) => {
    const calls: string[] = [];
    const hook = renderHook(({ draft, pending, other }: Props) => useDebouncedCommit(draft, pending, () => calls.push(`${draft}|${other}`)), {
      initialProps: initial,
    });
    return { calls, ...hook };
  };

  it('입력이 바뀌면 300ms 뒤 한 번, 발화 시점의 최신 commit으로 반영한다', () => {
    const { calls, rerender } = setup({ draft: '', pending: false, other: 'a' });
    rerender({ draft: 'x', pending: true, other: 'a' });
    rerender({ draft: 'xy', pending: true, other: 'a' });
    act(() => vi.advanceTimersByTime(299));
    expect(calls).toEqual([]);
    rerender({ draft: 'xy', pending: true, other: 'b' }); // 기다리는 사이 다른 조건이 바뀜 — 예약은 다시 걸지 않는다
    act(() => vi.advanceTimersByTime(1));
    expect(calls).toEqual(['xy|b']);
    act(() => vi.advanceTimersByTime(1000));
    expect(calls).toEqual(['xy|b']);
  });

  it('pending이 아니면(URL과 같은 값으로 동기화) 예약하지 않는다', () => {
    const { calls, rerender } = setup({ draft: '', pending: false, other: 'a' });
    rerender({ draft: 'url', pending: false, other: 'a' });
    act(() => vi.advanceTimersByTime(1000));
    expect(calls).toEqual([]);
  });

  it('flush는 바로 반영하고 남은 예약을 지운다', () => {
    const { calls, rerender, result } = setup({ draft: '', pending: false, other: 'a' });
    rerender({ draft: 'x', pending: true, other: 'a' });
    act(() => result.current());
    expect(calls).toEqual(['x|a']);
    act(() => vi.advanceTimersByTime(1000));
    expect(calls).toEqual(['x|a']);
  });

  it('unmount하면 남은 예약을 지운다', () => {
    const { calls, rerender, unmount } = setup({ draft: '', pending: false, other: 'a' });
    rerender({ draft: 'x', pending: true, other: 'a' });
    unmount();
    act(() => vi.advanceTimersByTime(1000));
    expect(calls).toEqual([]);
  });
});
