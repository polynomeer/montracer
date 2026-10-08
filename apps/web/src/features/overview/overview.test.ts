import { describe, expect, it } from 'vitest';
import type { MetricSeries, ServiceItem } from '../../api/types.ts';
import { overviewRows, sortRows } from './overview.ts';

const svc = (id: string, name: string, extra: Partial<ServiceItem> = {}): ServiceItem => ({
  service_id: id,
  name,
  namespace: 'shop',
  environment: 'prod',
  language: 'java',
  status: 'active',
  first_seen: '2026-10-04T00:00:00Z',
  last_seen: '2026-10-04T05:59:00Z',
  owner_team: null,
  repository_url: null,
  runbook_url: null,
  tier: null,
  tags: [],
  ...extra,
});

const T = '2026-10-04T05:00:00Z';
const count = (name: string, status: string, v: number | null, extra: Partial<MetricSeries['points'][number]> = {}, env = 'prod'): MetricSeries => ({
  labels: { 'service.namespace': 'shop', 'service.name': name, 'deployment.environment.name': env, 'http.response.status_code': status },
  unit: '{request}',
  points: [{ t: T, v, partial: false, ...(v === null ? { reason: 'no_data' as const } : {}), ...extra }],
  completeness: v === null ? 0 : 1,
  missing: [],
  flags: [],
});
const p95 = (name: string, v: number | null, unit: string | null = 's'): MetricSeries => ({
  labels: { 'service.namespace': 'shop', 'service.name': name, 'deployment.environment.name': 'prod' },
  unit,
  points: [{ t: T, v, partial: false, ...(v === null ? { reason: 'bounds_mismatch' as const } : {}) }],
  completeness: v === null ? 0 : 1,
  missing: [],
  flags: [],
});

describe('overviewRows', () => {
  const catalog = [svc('a', 'checkout'), svc('b', 'payment'), svc('c', 'idle', { status: 'inactive' })];
  const rows = overviewRows(
    catalog,
    [
      count('checkout', '200', 980),
      count('checkout', '500', 20),
      count('payment', '200', 600, { partial: true }),
      count('rogue', '200', 60), // catalog 미등록
    ],
    [p95('checkout', 0.25), p95('payment', null), p95('rogue', 40, 'ms')],
    3600,
  );
  const by = (name: string) => rows.find((r) => r.name === name);

  it('서비스 자연 키로 status count를 합쳐 요청/s·5xx 오류율, p95는 ms로', () => {
    expect(by('checkout')).toMatchObject({ serviceId: 'a', catalogStatus: 'active', requestsPerSecond: 1000 / 3600, errorRate: 0.02, partial: false, countReason: null });
    expect(by('checkout')?.p95.value).toBe(250);
    expect(by('rogue')?.p95.value).toBe(40);
  });

  it('값이 없으면 0이 아니라 이유: metric 없는 catalog 서비스, 병합할 수 없는 p95', () => {
    expect(by('idle')).toMatchObject({ requestsPerSecond: null, errorRate: null, countReason: 'no_series', catalogStatus: 'inactive' });
    expect(by('idle')?.p95.reason).toBe('no_series');
    expect(by('payment')?.p95).toMatchObject({ value: null, reason: 'bounds_mismatch' });
    expect(by('payment')?.partial).toBe(true);
  });

  it('catalog에 없는 metric 서비스는 링크 없이(serviceId null) 남는다', () => {
    expect(by('rogue')).toMatchObject({ serviceId: null, catalogStatus: null, namespace: 'shop', environment: 'prod' });
  });

  it('catalog 전체를 받았으면 없는 서비스는 미등록, 아니면 확인할 수 없음', () => {
    expect(by('rogue')?.catalog).toBe('unregistered');
    expect(by('checkout')?.catalog).toBe('registered');
    const partial = overviewRows(catalog, [count('rogue', '200', 1)], [], 60, false);
    expect(partial.find((r) => r.name === 'rogue')?.catalog).toBe('unknown');
  });

  it('같은 이름이라도 environment가 다르면 다른 서비스', () => {
    const r = overviewRows([svc('p', 'checkout'), svc('s', 'checkout', { environment: 'stage' })], [count('checkout', '200', 10), count('checkout', '200', 5, {}, 'stage')], [], 60);
    expect(r.map((x) => `${x.serviceId ?? '-'}:${String(x.requestsPerSecond)}`).sort()).toEqual([`p:${String(10 / 60)}`, `s:${String(5 / 60)}`]);
  });

  it('단위를 모르는 p95는 바꾸지 않고 버린다', () => {
    const r = overviewRows([svc('a', 'checkout')], [count('checkout', '200', 1)], [p95('checkout', 3, 'min')], 60);
    expect(r[0]?.p95).toMatchObject({ value: null, reason: 'unit_conflict' });
  });

  it('정렬: 큰 값부터, 값 없는 서비스는 항상 뒤, 이름은 가나다순', () => {
    expect(sortRows(rows, 'error_rate').map((r) => r.name)).toEqual(['checkout', 'payment', 'rogue', 'idle']);
    expect(sortRows(rows, 'rps').map((r) => r.name)).toEqual(['checkout', 'payment', 'rogue', 'idle']);
    expect(sortRows(rows, 'p95').map((r) => r.name)).toEqual(['checkout', 'rogue', 'payment', 'idle']);
    expect(sortRows(rows, 'name').map((r) => r.name)).toEqual(['checkout', 'idle', 'payment', 'rogue']);
  });
});
