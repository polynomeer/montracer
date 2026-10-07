import { describe, expect, it } from 'vitest';
import type { MetricPoint, MetricSeries } from '../../api/types.ts';
import {
  METHOD_KEY,
  ROUTE_KEY,
  STATUS_KEY,
  combineStatus,
  isServerError,
  metricWindow,
  resourceRows,
  serviceFilter,
  single,
  toMilliseconds,
  toPoints,
} from './red.ts';

const T0 = '2026-10-04T05:00:00Z';
const T1 = '2026-10-04T05:01:00Z';

function series(labels: Record<string, string>, points: Partial<MetricPoint>[], unit: string | null = null): MetricSeries {
  return {
    labels,
    unit,
    points: points.map((p, i) => ({ t: i === 0 ? T0 : T1, v: null, partial: false, ...p })),
    completeness: 1,
    missing: [],
    flags: [],
  };
}

describe('metricWindow', () => {
  it('시작은 분 내림, 끝은 분 올림, 차트 step은 60의 배수로 점 120개 이하', () => {
    const w = metricWindow(Date.parse('2026-10-04T05:00:30Z'), Date.parse('2026-10-04T06:00:10Z'));
    expect(w).toEqual({ fromIso: '2026-10-04T05:00:00.000Z', toIso: '2026-10-04T06:01:00.000Z', rangeSeconds: 3660, chartStepSeconds: 60 });
    const week = metricWindow(Date.parse('2026-09-27T06:00:00Z'), Date.parse('2026-10-04T06:00:00Z'));
    expect(week?.chartStepSeconds).toBe(7200); // 1시간 배수로 올림 → 1시간 rollup
    expect((week?.rangeSeconds ?? 0) / (week?.chartStepSeconds ?? 1)).toBeLessThanOrEqual(120);
  });

  it('7일을 넘거나 빈 범위면 null', () => {
    expect(metricWindow(Date.parse('2026-09-26T06:00:00Z'), Date.parse('2026-10-04T06:00:00Z'))).toBeNull();
    expect(metricWindow(1000, 1000)).toBeNull();
  });
});

describe('serviceFilter', () => {
  it('catalog 자연 키(name·namespace·environment)로 고른다', () => {
    expect(serviceFilter({ name: 'checkout', namespace: 'shop', environment: 'prod' })).toEqual({
      op: 'and',
      args: [
        { op: 'eq', field: 'service.name', value: 'checkout' },
        { op: 'eq', field: 'service.namespace', value: 'shop' },
        { op: 'eq', field: 'deployment.environment.name', value: 'prod' },
      ],
    });
  });
});

describe('isServerError', () => {
  it('5xx만 오류(4xx는 client 오류), 형식이 다르면 오류로 세지 않는다', () => {
    expect(['500', '503', '599'].map(isServerError)).toEqual([true, true, true]);
    expect(['200', '404', '499', '600', '', '5xx', undefined].map(isServerError)).toEqual([false, false, false, false, false, false, false]);
  });
});

describe('combineStatus', () => {
  it('요청 1,000·오류 20 → 오류율 정확히 2% (D06 §05 oracle)', () => {
    const c = combineStatus([series({ [STATUS_KEY]: '200' }, [{ v: 980 }]), series({ [STATUS_KEY]: '500' }, [{ v: 20 }])], 300);
    expect(c.requests[0]?.value).toBe(1000);
    expect(c.errorRate[0]?.value).toBe(0.02);
    expect(c.requestsPerSecond[0]?.value).toBeCloseTo(1000 / 300);
    expect(c.errorRate[0]?.partial).toBe(false);
  });

  it('한 status만 no_data면 그 status 요청이 없던 것 — 오류율 0, partial 아님', () => {
    const c = combineStatus([series({ [STATUS_KEY]: '200' }, [{ v: 50 }]), series({ [STATUS_KEY]: '500' }, [{ reason: 'no_data' }])], 60);
    expect(c.errorRate[0]).toMatchObject({ value: 0, partial: false });
  });

  it('모든 status가 비면 0이 아니라 null + 이유, pending이 no_data보다 우선', () => {
    const c = combineStatus(
      [series({ [STATUS_KEY]: '200' }, [{ v: 10 }, { reason: 'no_data' }]), series({ [STATUS_KEY]: '500' }, [{ v: 1 }, { reason: 'pending' }])],
      60,
    );
    expect(c.requestsPerSecond[1]).toMatchObject({ value: null, reason: 'pending' });
    expect(c.errorRate[1]).toMatchObject({ value: null, reason: 'pending' });
  });

  it('pending 등 no_data가 아닌 이유로 빈 status가 섞이면 합은 하한 → partial', () => {
    const c = combineStatus([series({ [STATUS_KEY]: '200' }, [{ v: 10 }]), series({ [STATUS_KEY]: '500' }, [{ reason: 'pending' }])], 60);
    expect(c.requests[0]).toMatchObject({ value: 10, partial: true });
  });

  it('서버의 partial 표시를 그대로 전달', () => {
    const c = combineStatus([series({ [STATUS_KEY]: '200' }, [{ v: 10, partial: true }])], 60);
    expect(c.requestsPerSecond[0]?.partial).toBe(true);
  });

  it('요청이 0건이면 오류율은 0%가 아니라 null + no_requests', () => {
    const c = combineStatus([series({ [STATUS_KEY]: '200' }, [{ v: 0 }])], 60);
    expect(c.errorRate[0]).toMatchObject({ value: null, reason: 'no_requests' });
  });

  it('series가 없으면 빈 결과(화면이 데이터 없음으로 표시)', () => {
    expect(combineStatus([], 60).requests).toEqual([]);
  });
});

describe('toPoints · single', () => {
  it('null 값은 이유를 유지하고, series가 없으면 no_series', () => {
    expect(toPoints(series({}, [{ reason: 'bounds_mismatch' }]))[0]).toMatchObject({ value: null, reason: 'bounds_mismatch' });
    expect(single(toPoints(undefined))).toMatchObject({ value: null, reason: 'no_series' });
  });
});

describe('resourceRows', () => {
  const r = (route: string, method: string, extra: Record<string, string> = {}) => ({ [ROUTE_KEY]: route, [METHOD_KEY]: method, ...extra });

  it('endpoint별 요청/s·오류율·p95·total time, total time 내림차순·값 없는 행은 뒤로', () => {
    const rows = resourceRows(
      [
        series(r('/cart', 'GET', { [STATUS_KEY]: '200' }), [{ v: 600 }]),
        series(r('/checkout', 'POST', { [STATUS_KEY]: '200' }), [{ v: 290 }]),
        series(r('/checkout', 'POST', { [STATUS_KEY]: '503' }), [{ v: 10 }]),
        series(r('/health', 'GET', { [STATUS_KEY]: '200' }), [{ v: 60 }]),
      ],
      [series(r('/cart', 'GET'), [{ v: 0.04 }], 's'), series(r('/checkout', 'POST'), [{ v: 1.2 }], 's')],
      [series(r('/cart', 'GET'), [{ v: 20 }], 's'), series(r('/checkout', 'POST'), [{ v: 300 }], 's'), series(r('/health', 'GET'), [{ reason: 'no_data' }], 's')],
      600,
    );
    expect(rows.map((x) => `${x.method} ${x.route}`)).toEqual(['POST /checkout', 'GET /cart', 'GET /health']);
    expect(rows[0]).toMatchObject({ requestsPerSecond: 0.5, errorRate: 10 / 300 });
    expect(rows[0]?.p95.value).toBe(1.2);
    expect(rows[2]?.totalSeconds).toMatchObject({ value: null, reason: 'no_data' });
    expect(rows[2]?.p95).toMatchObject({ value: null, reason: 'no_series' });
    expect(rows[0]?.countReason).toBeNull();
  });

  it('endpoint의 요청 수가 모두 비면 "요청 없음"으로 단정하지 않고 이유를 남긴다', () => {
    const rows = resourceRows([series(r('/checkout', 'POST', { [STATUS_KEY]: '200' }), [{ reason: 'pending' }])], [], [], 600);
    expect(rows[0]).toMatchObject({ requestsPerSecond: null, errorRate: null, countReason: 'pending' });
  });
});

describe('toMilliseconds', () => {
  it('s·ms만 변환하고 모르는 단위는 null', () => {
    expect(toMilliseconds(0.642, 's')).toBeCloseTo(642);
    expect(toMilliseconds(642, 'ms')).toBe(642);
    expect(toMilliseconds(1, '{request}')).toBeNull();
    expect(toMilliseconds(1, null)).toBeNull();
  });
});
