import { describe, expect, it } from 'vitest';
import { shareSearchParams } from '../../app/context.ts';
import {
  autoStep,
  dictionaryRange,
  formatValue,
  parseMetricState,
  pickAggregation,
  resultUnit,
  rollupLabel,
  seriesName,
  stepChoices,
  toMetricQuery,
  writeMetricState,
} from './metricQuery.ts';

const H = 3600_000;

describe('metric URL 상태', () => {
  it('왕복, 잘못된 값은 무시, group은 5개까지·중복 제거', () => {
    const s = parseMetricState(new URLSearchParams('metric=http.server.request.duration&agg=p95&group=service.name,http.route,service.name&step=300&f=http.route%3D%2Fcart&f=k%3Da%3Db'));
    expect(s).toEqual({
      metric: 'http.server.request.duration',
      agg: 'p95',
      groupBy: ['service.name', 'http.route'],
      filters: [
        { key: 'http.route', value: '/cart' },
        { key: 'k', value: 'a=b' }, // 첫 '='에서 나눈다
      ],
      step: 300,
    });
    expect(writeMetricState(new URLSearchParams('tz=UTC'), s).toString()).toBe(
      'tz=UTC&metric=http.server.request.duration&agg=p95&group=service.name%2Chttp.route&step=300&f=http.route%3D%2Fcart&f=k%3Da%3Db',
    );
    // 한도는 byte: 한글 86자(258 byte)는 256 byte를 넘는다
    expect(parseMetricState(new URLSearchParams(`metric=${'가'.repeat(86)}`)).metric).toBeNull();
    expect(parseMetricState(new URLSearchParams(`metric=${'가'.repeat(85)}`)).metric).toBe('가'.repeat(85));
    expect(parseMetricState(new URLSearchParams('agg=median&step=61&f=novalue&f=%3Dx&group=a,b,c,d,e,f'))).toEqual({
      metric: null,
      agg: null,
      groupBy: ['a', 'b', 'c', 'd', 'e'],
      filters: [],
      step: null,
    });
  });

  it('공유 링크에는 metric·연산·group·step만, 조건 값은 빠진다', () => {
    const params = new URLSearchParams('range=1h&tz=UTC&metric=jvm.memory.used&agg=avg&group=service.name&step=300&f=user.email%3Da%40b.c');
    const ctx = { environment: null, range: { kind: 'relative' as const, preset: '1h' as const }, timeZone: 'UTC' };
    const out = shareSearchParams(params, ctx, Date.parse('2026-10-04T06:00:00Z'));
    expect(out.get('metric')).toBe('jvm.memory.used');
    expect(out.get('agg')).toBe('avg');
    expect(out.get('group')).toBe('service.name');
    expect(out.get('step')).toBe('300');
    expect(out.has('f')).toBe(false);
  });
});

describe('해상도', () => {
  it('자동 step: 120점 이하인 가장 작은 선택지, 7일은 1시간 배수(1시간 rollup)', () => {
    expect(autoStep(H)).toBe(60);
    expect(autoStep(4 * H)).toBe(120);
    expect(autoStep(24 * H)).toBe(900);
    expect(autoStep(7 * 24 * H)).toBe(7200);
  });
  it('선택지는 범위보다 짧고 series당 2,000점 이하', () => {
    expect(stepChoices(H)).toEqual([60, 120, 300, 600, 900, 1800]);
    expect(stepChoices(7 * 24 * H)[0]).toBe(600); // 60·120·300초는 2,000점을 넘는다
  });
  it('rollup 이름은 서버가 알려준 값만, 모르면 추측하지 않는다', () => {
    expect(rollupLabel(60)).toBe('1분 rollup');
    expect(rollupLabel(3600)).toBe('1시간 rollup');
    expect(rollupLabel(undefined)).toBeNull();
  });
  it('사전 범위는 조사 범위 끝에서 24시간까지', () => {
    const to = Date.parse('2026-10-04T06:00:00Z');
    expect(dictionaryRange(to - 7 * 24 * H, to)).toEqual({ from: '2026-10-03T06:00:00.000Z', to: '2026-10-04T06:00:00.000Z', clipped: true });
    expect(dictionaryRange(to - H, to).clipped).toBe(false);
  });
});

describe('QuerySpec', () => {
  const base = { metric: 'm', agg: 'rate' as const, groupBy: [], filters: [], step: null };
  it('조건이 하나면 eq, 여럿이면 and, group_by는 있을 때만', () => {
    expect(toMetricQuery(base, 0, H, 60)).toEqual({
      range: { from: '1970-01-01T00:00:00.000Z', to: '1970-01-01T01:00:00.000Z' },
      step_seconds: 60,
      expression: { metric: 'm', aggregation: 'rate' },
    });
    const q = toMetricQuery({ ...base, groupBy: ['service.name'], filters: [{ key: 'a', value: '1' }, { key: 'b', value: '' }] }, 0, H, 60);
    expect(q.expression.filter).toEqual({ op: 'and', args: [{ op: 'eq', field: 'a', value: '1' }, { op: 'eq', field: 'b', value: '' }] });
    expect(q.expression.group_by).toEqual(['service.name']);
  });
  it('기본 연산: counter는 rate, gauge는 avg, histogram은 p95, 없으면 null', () => {
    expect(pickAggregation(['rate', 'increase', 'sum'])).toBe('rate');
    expect(pickAggregation(['avg', 'min', 'max'])).toBe('avg');
    expect(pickAggregation(['p50', 'p90', 'p95', 'p99', 'count', 'hist_sum'])).toBe('p95');
    expect(pickAggregation(['sum'])).toBe('sum');
    expect(pickAggregation([])).toBeNull();
  });
  it('series 이름: 빈 label은 "(없음)", group이 없으면 "전체"', () => {
    expect(seriesName({ 'service.name': 'checkout', 'http.route': '' }, ['service.name', 'http.route'])).toBe('service.name=checkout, http.route=(없음)');
    expect(seriesName({}, [])).toBe('전체');
  });
  it('값 표기: 1만 이상은 축약, 아주 작은 값은 지수', () => {
    expect(formatValue(0)).toBe('0');
    expect(formatValue(1234.5)).toBe('1,235');
    expect(formatValue(1.08e9)).toBe('10.8억');
    expect(formatValue(0.0001)).toBe('1.00e-4');
  });
  it('결과 단위: rate는 초당, 관측 수는 건, 무차원(1)은 빈 단위', () => {
    expect(resultUnit('rate', '{request}')).toBe('{request}/s');
    expect(resultUnit('rate', '1')).toBe('/s');
    expect(resultUnit('count', 'ms')).toBe('건');
    expect(resultUnit('p95', 'ms')).toBe('ms');
    expect(resultUnit('avg', '1')).toBe('');
  });
});
