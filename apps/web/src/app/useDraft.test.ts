import { act, renderHook } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { useDraft } from './useDraft.ts';

// PS-0008: debounce로 URL에 반영한 뒤 "URL → 입력" 동기화가 그 사이에 더 친 글자를 지웠다.
describe('useDraft', () => {
  it('우리가 반영한 값으로 URL이 바뀌면 입력(그 뒤 더 친 글자)을 되돌리지 않는다', () => {
    const { result, rerender } = renderHook(({ v }) => useDraft(v, (x: string) => x), { initialProps: { v: '' } });
    act(() => result.current[1]('tim')); // 입력
    act(() => result.current[2]('tim')); // debounce가 'tim'을 URL에 반영
    act(() => result.current[1]('timeo')); // 반영이 끝나기 전에 더 침
    rerender({ v: 'tim' }); // URL이 'tim'이 됨
    expect(result.current[0]).toBe('timeo');
  });

  it('바깥에서 바뀐 URL(뒤로 가기·링크)은 입력에 다시 넣는다', () => {
    const { result, rerender } = renderHook(({ v }) => useDraft(v, (x: string) => x), { initialProps: { v: 'a' } });
    act(() => result.current[1]('abc'));
    rerender({ v: 'zzz' });
    expect(result.current[0]).toBe('zzz');
    // 이전 값으로 돌아가는 뒤로 가기도 바깥 변경이다
    rerender({ v: 'a' });
    expect(result.current[0]).toBe('a');
  });

  it('배열 값(조건 행)도 같은 규칙: 정규화한 값을 반영으로 알리면 편집 중인 빈 행이 남는다', () => {
    type F = { key: string; value: string }[];
    const { result, rerender } = renderHook(({ v }) => useDraft<F>(v), { initialProps: { v: [] as F } });
    act(() => result.current[1]([{ key: 'a', value: '1' }, { key: '', value: '' }]));
    act(() => result.current[2]([{ key: 'a', value: '1' }])); // 빈 행을 뺀 값을 URL에 씀
    rerender({ v: [{ key: 'a', value: '1' }] });
    expect(result.current[0]).toHaveLength(2);
  });
});
