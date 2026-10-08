import { describe, expect, it } from 'vitest';
import type { LogItem } from '../../api/types.ts';
import { shareSearchParams } from '../../app/context.ts';
import { parseLogFilter, severityLabel, toLogFilterNode, traceLinkSearch, writeLogFilter } from './logFilters.ts';

const SVC = 'aaaaaaaa-0000-4000-8000-000000000001';
const TRACE = 'ab'.repeat(16);

describe('severityLabel', () => {
  it('OTel SeverityNumber 구간, 0·범위 밖은 이름을 지어내지 않는다', () => {
    expect([1, 4, 5, 9, 12, 13, 17, 20, 21, 24].map(severityLabel)).toEqual(['TRACE', 'TRACE', 'DEBUG', 'INFO', 'INFO', 'WARN', 'ERROR', 'ERROR', 'FATAL', 'FATAL']);
    expect(severityLabel(0)).toBe('미지정');
    expect(severityLabel(25)).toBe('미지정');
  });
});

describe('log filter URL', () => {
  it('왕복, 잘못된 값은 조건 없음, trace ID는 소문자로', () => {
    const f = parseLogFilter(new URLSearchParams(`service=${SVC}&sev=warn&trace=${TRACE.toUpperCase()}&q=timeout`));
    expect(f).toEqual({ serviceId: SVC, minSeverity: 'warn', traceId: TRACE, query: 'timeout' });
    expect(writeLogFilter(new URLSearchParams('tz=UTC&entity=e1'), f).toString()).toBe(`tz=UTC&service=${SVC}&sev=warn&trace=${TRACE}&q=timeout`);
    expect(parseLogFilter(new URLSearchParams(`service=x&sev=loud&trace=${'0'.repeat(32)}&q=${'a'.repeat(201)}`))).toEqual({
      serviceId: null,
      minSeverity: null,
      traceId: null,
      query: '',
    });
  });

  it('filter AST: 조건이 하나면 그대로, 여럿이면 and', () => {
    expect(toLogFilterNode(parseLogFilter(new URLSearchParams()))).toBeUndefined();
    expect(toLogFilterNode(parseLogFilter(new URLSearchParams('sev=error')))).toEqual({ op: 'gte', field: 'severity_number', value: 17 });
    expect(toLogFilterNode(parseLogFilter(new URLSearchParams(`service=${SVC}&trace=${TRACE}&q=%20x%20`)))).toEqual({
      op: 'and',
      args: [
        { op: 'eq', field: 'service_id', value: SVC },
        { op: 'eq', field: 'trace_id', value: TRACE },
        { op: 'contains', field: 'body', value: 'x' },
      ],
    });
  });

  it('공유 링크에는 서비스·심각도·trace만, 본문 검색어는 빠진다', () => {
    const params = new URLSearchParams(`range=1h&tz=UTC&service=${SVC}&sev=error&trace=${TRACE}&q=secret-token`);
    const ctx = { environment: null, range: { kind: 'relative' as const, preset: '1h' as const }, timeZone: 'UTC' };
    const out = shareSearchParams(params, ctx, Date.parse('2026-10-04T06:00:00Z'));
    expect(out.get('service')).toBe(SVC);
    expect(out.get('sev')).toBe('error');
    expect(out.get('trace')).toBe(TRACE);
    expect(out.has('q')).toBe(false);
  });
});

describe('traceLinkSearch', () => {
  const log: LogItem = {
    time: '2026-10-04T05:30:00.123456789Z',
    event_id: 'gen:1',
    service_id: SVC,
    severity_number: 17,
    trace_id: TRACE,
    span_id: '1234567890abcdef',
    body: 'x',
    attributes: {},
  };
  const LATER = Date.parse('2026-10-04T07:00:00Z');
  it('log 시각 1시간 전 ~ 10분 뒤, span이 있으면 그 span의 로그 탭', () => {
    const q = new URLSearchParams(traceLinkSearch('?range=1h&tz=UTC', log, LATER));
    expect(q.has('range')).toBe(false);
    expect(q.get('from')).toBe('2026-10-04T04:30:00.123Z');
    expect(q.get('to')).toBe('2026-10-04T05:40:00.123Z');
    expect(q.get('entity')).toBe('1234567890abcdef');
    expect(q.get('tab')).toBe('logs');
    expect(q.get('tz')).toBe('UTC');
  });
  it('최근 log면 끝을 지금 + 4분으로 자른다(5분 넘게 미래인 끝 시각은 context가 거절한다)', () => {
    const now = Date.parse('2026-10-04T05:32:00Z');
    expect(new URLSearchParams(traceLinkSearch('', log, now)).get('to')).toBe('2026-10-04T05:36:00.000Z');
    // log 시각이 브라우저 시계보다 앞서도(시계 차이) 끝은 context 허용 범위 안
    const behind = Date.parse('2026-10-04T05:27:00Z');
    expect(Date.parse(new URLSearchParams(traceLinkSearch('', log, behind)).get('to') ?? '')).toBeLessThanOrEqual(behind + 5 * 60_000);
  });
  it('span이 없으면 선택하지 않는다', () => {
    const q = new URLSearchParams(traceLinkSearch('', { ...log, span_id: null }, LATER));
    expect(q.has('entity')).toBe(false);
    expect(q.has('tab')).toBe(false);
  });
});
