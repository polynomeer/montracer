import { describe, expect, it } from 'vitest';
import { shareSearchParams, parseContext } from '../../app/context.ts';
import { EMPTY_FILTER, parseTraceFilter, toFilterNode, traceDetailWindow, writeTraceFilter } from './filters.ts';

const SVC = 'aaaaaaaa-0000-4000-8000-000000000001';

describe('trace filter ↔ URL ↔ AST', () => {
  it('URL 값을 검증해 읽고, 잘못된 값은 조건 없음', () => {
    expect(parseTraceFilter(new URLSearchParams(`service=${SVC}&errors=1&min_ms=500&name=charge`))).toEqual({
      serviceId: SVC,
      errorsOnly: true,
      minMs: 500,
      name: 'charge',
    });
    expect(parseTraceFilter(new URLSearchParams(`service=nope&errors=yes&min_ms=-1&name=${'x'.repeat(201)}`))).toEqual(EMPTY_FILTER);
    expect(parseTraceFilter(new URLSearchParams('min_ms=999999999')).minMs).toBeNull(); // 3일 초과
  });

  it('쓰기는 다른 query를 유지하고 빈 조건은 지운다', () => {
    const next = writeTraceFilter(new URLSearchParams('range=1h&errors=1&name=x'), { ...EMPTY_FILTER, minMs: 100 });
    expect(next.toString()).toBe('range=1h&min_ms=100');
  });

  it('AST: 조건 하나면 그 잎, 여럿이면 and, 없으면 undefined', () => {
    expect(toFilterNode(EMPTY_FILTER)).toBeUndefined();
    expect(toFilterNode({ ...EMPTY_FILTER, errorsOnly: true })).toEqual({ op: 'eq', field: 'has_error', value: true });
    expect(toFilterNode({ serviceId: SVC, errorsOnly: true, minMs: 500, name: ' charge ' })).toEqual({
      op: 'and',
      args: [
        { op: 'eq', field: 'service_id', value: SVC },
        { op: 'contains', field: 'name', value: 'charge' },
        { op: 'eq', field: 'has_error', value: true },
        { op: 'gte', field: 'duration_ms', value: 500 },
      ],
    });
  });

  it('공유 링크: service·errors·min_ms는 남고 자유 입력 name은 빠진다', () => {
    const params = new URLSearchParams(`range=1h&service=${SVC}&errors=1&min_ms=500&name=user@example.com`);
    const shared = shareSearchParams(params, parseContext(params, 'UTC', Date.parse('2026-10-04T06:00:00Z')).context, Date.parse('2026-10-04T06:00:00Z'));
    expect(shared.get('service')).toBe(SVC);
    expect(shared.get('errors')).toBe('1');
    expect(shared.get('min_ms')).toBe('500');
    expect(shared.has('name')).toBe(false);
  });

  it('상세 링크 범위: 시작 1분 전 ~ 끝 1분 뒤', () => {
    expect(traceDetailWindow('2026-10-04T05:00:00.000Z', 842.4)).toEqual({ from: '2026-10-04T04:59:00.000Z', to: '2026-10-04T05:01:00.843Z' });
  });
});
