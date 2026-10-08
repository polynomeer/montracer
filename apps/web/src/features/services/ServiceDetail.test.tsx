import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { MetricQuery, MetricSeries, ServiceItem } from '../../api/types.ts';
import { routes } from '../../app/routes.tsx';
import { partialReason } from './ServiceDetail.tsx';
import { METHOD_KEY, ROUTE_KEY, STATUS_KEY } from './red.ts';

const ID = 'aaaaaaaa-0000-4000-8000-000000000001';
const NOW = Date.parse('2026-10-04T06:00:00Z');

const svc: ServiceItem = {
  service_id: ID,
  name: 'checkout',
  namespace: 'shop',
  environment: 'prod',
  language: 'java',
  status: 'active',
  first_seen: '2026-10-04T05:00:00Z',
  last_seen: '2026-10-04T05:59:00Z',
  owner_team: null,
  repository_url: null,
  runbook_url: null,
  tier: null,
  tags: [],
};

const meta = { request_id: 'req-1', schema_version: 1, partial: false, failed_shards: [], warnings: [], watermark: null };

function seriesFor(q: MetricQuery): MetricSeries[] {
  const whole = q.step_seconds === (Date.parse(q.range.to) - Date.parse(q.range.from)) / 1000;
  const steps = whole ? 1 : 3;
  const pts = (vals: (number | null)[]) =>
    vals.slice(0, steps).map((v, i) => ({
      t: new Date(Date.parse(q.range.from) + i * q.step_seconds * 1000).toISOString(),
      v,
      partial: false,
      ...(v === null ? { reason: 'no_data' as const } : {}),
    }));
  const mk = (labels: Record<string, string>, vals: (number | null)[], unit: string | null): MetricSeries => ({
    labels,
    unit,
    points: pts(vals),
    completeness: 1,
    missing: [],
    flags: [],
  });
  const g = q.expression.group_by ?? [];
  const route = { [ROUTE_KEY]: '/checkout', [METHOD_KEY]: 'POST' };
  switch (q.expression.aggregation) {
    case 'count':
      if (g.includes(ROUTE_KEY)) {
        return [mk({ ...route, [STATUS_KEY]: '200' }, [980], '{request}'), mk({ ...route, [STATUS_KEY]: '500' }, [20], '{request}')];
      }
      // 차트: 두 번째 step은 값이 없다(0으로 그리면 안 된다)
      return whole
        ? [mk({ [STATUS_KEY]: '200' }, [980], '{request}'), mk({ [STATUS_KEY]: '500' }, [20], '{request}')]
        : [mk({ [STATUS_KEY]: '200' }, [490, null, 490], '{request}'), mk({ [STATUS_KEY]: '500' }, [10, null, 10], '{request}')];
    case 'p95':
      return g.includes(ROUTE_KEY) ? [mk(route, [2.375], 's')] : [mk({}, [2.375, null, 2.375], 's')];
    case 'hist_sum':
      return [mk(route, [358], 's')];
    default:
      return [];
  }
}

type Handler = (url: string, init: RequestInit) => Response | Promise<Response>;
let handler: Handler;
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(NOW);
  handler = (url, init) => {
    if (url.endsWith(`/api/v1/services/${ID}`)) return json(200, { data: svc, meta });
    if (url.endsWith('/api/v1/query/metrics')) {
      const q = JSON.parse(String(init.body)) as MetricQuery;
      return json(200, { data: { series: seriesFor(q) }, meta });
    }
    return json(404, { error: { code: 'NOT_FOUND', message: 'not found', request_id: 'r', retryable: false } });
  };
  vi.stubGlobal('fetch', vi.fn((url: string, init: RequestInit) => Promise.resolve(handler(url, init))));
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function renderAt(path: string) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return router;
}

describe('ServiceDetail (S02)', () => {
  it('헤더: 이름·환경, 모르는 값은 미설정·확인할 수 없음, monitor 없음은 판정 없음', async () => {
    renderAt(`/o/acme/services/${ID}?tz=UTC`);
    expect(await screen.findByRole('heading', { level: 1, name: 'checkout' })).toBeTruthy();
    expect(screen.getAllByText('미설정').length).toBe(2);
    expect(screen.getByText('확인할 수 없음')).toBeTruthy();
    expect(screen.getByText('판정 없음 · monitor 없음')).toBeTruthy();
  });

  it('RED 카드: 비샘플링 source badge, 요청 1,000·오류 20 → 오류율 2%, p95는 ms', async () => {
    renderAt(`/o/acme/services/${ID}?tz=UTC`);
    const value = async (name: string) => {
      const card = await screen.findByRole('region', { name });
      await vi.waitFor(() => expect(card.querySelector('.mt-metric')?.textContent).not.toContain('불러오는 중'));
      return card.querySelector('.mt-metric')?.textContent?.replace(/\s+/g, ' ').trim();
    };
    expect(await value('오류율 (5xx)')).toBe('2 %');
    expect(await value('p95 지연')).toBe('2,375 ms');
    expect(screen.getByText('SDK metric · 비샘플링')).toBeTruthy();
  });

  it('차트의 빈 step은 0이 아니라 "데이터 없음"', async () => {
    renderAt(`/o/acme/services/${ID}?tz=UTC`);
    const errors = await screen.findByRole('region', { name: '오류율 (5xx)' });
    await userEvent.click(await within(errors).findByText('표로 보기'));
    const rows = within(errors).getAllByRole('row');
    expect(rows[2]?.textContent).toContain('데이터 없음');
    expect(rows[2]?.textContent).not.toContain('0%');
  });

  it('리소스 표: endpoint별 요청/s·오류율·p95·총 소요시간', async () => {
    renderAt(`/o/acme/services/${ID}?tz=UTC`);
    const table = await screen.findByRole('region', { name: '리소스' });
    const row = (await within(table).findByText('POST /checkout')).closest('tr');
    expect(row?.textContent).toContain('2%');
    expect(row?.textContent).toContain('2,375 ms');
    expect(row?.textContent).toContain('6 min');
  });

  it('모든 metric query가 같은 범위·서비스 조건을 쓴다', async () => {
    renderAt(`/o/acme/services/${ID}?tz=UTC&range=1h`);
    await screen.findByRole('region', { name: '리소스' });
    const calls = vi.mocked(fetch).mock.calls.filter(([u]) => String(u).endsWith('/query/metrics'));
    expect(calls.length).toBe(7);
    const qs = calls.map(([, init]) => JSON.parse(String((init as RequestInit).body)) as MetricQuery);
    expect(new Set(qs.map((q) => `${q.range.from}|${q.range.to}`)).size).toBe(1);
    expect(qs[0]?.range).toEqual({ from: '2026-10-04T05:00:00.000Z', to: '2026-10-04T06:00:00.000Z' });
    for (const q of qs) {
      expect(q.expression.metric).toBe('http.server.request.duration');
      expect(JSON.stringify(q.expression.filter)).toContain('"deployment.environment.name","value":"prod"');
    }
  });

  it('없거나 볼 수 없는 서비스는 존재를 드러내지 않는 안내', async () => {
    handler = () => json(404, { error: { code: 'NOT_FOUND', message: 'x', request_id: 'r', retryable: false } });
    renderAt(`/o/acme/services/${ID}`);
    expect(await screen.findByRole('heading', { name: '서비스를 찾을 수 없습니다' })).toBeTruthy();
  });

  it('metric이 하나도 없으면 0 카드가 아니라 계측 안내', async () => {
    const base = handler;
    handler = (url, init) => (url.endsWith('/query/metrics') ? json(200, { data: { series: [] }, meta }) : base(url, init));
    renderAt(`/o/acme/services/${ID}`);
    expect(await screen.findByText('이 서비스에서 HTTP 서버 metric을 받지 못했습니다')).toBeTruthy();
    expect(screen.queryByRole('region', { name: '요청량' })).toBeNull();
  });

  it('조회 실패는 이유와 request ID를 보인다(429는 Retry-After)', async () => {
    const base = handler;
    handler = (url, init) =>
      url.endsWith('/query/metrics')
        ? new Response(JSON.stringify({ error: { code: 'RATE_LIMITED', message: 'x', request_id: 'req-429', retryable: true } }), {
            status: 429,
            headers: { 'Retry-After': '1' },
          })
        : base(url, init);
    renderAt(`/o/acme/services/${ID}`);
    // 카드 조회 실패는 위쪽, 리소스 표 조회 실패는 표 자리에 한 번씩
    expect((await screen.findAllByText('1초 후 다시 시도하세요.')).length).toBe(2);
    expect(screen.getAllByText('request_id req-429').length).toBe(2);
    expect(within(screen.getByRole('region', { name: '리소스' })).getByText('request_id req-429')).toBeTruthy();
  });

  // query 하나가 503이면 그 query가 채우는 자리만 "확인할 수 없음"이고, "받은 metric 없음"·빈 표로 보이지 않는다(D05 §03 실패 > empty).
  const queryName = (q: MetricQuery): string => {
    const whole = q.step_seconds === (Date.parse(q.range.to) - Date.parse(q.range.from)) / 1000;
    const g = q.expression.group_by ?? [];
    const scope = g.includes(ROUTE_KEY) ? 'route' : whole ? 'total' : 'chart';
    return `${scope}-${q.expression.aggregation}`;
  };
  const unavailable = json(503, { error: { code: 'UNAVAILABLE', message: 'x', request_id: 'req-503', retryable: true } });
  const CARD_FAILURES: { name: string; values: string[]; charts: string[] }[] = [
    { name: 'total-count', values: ['요청량', '오류율 (5xx)'], charts: [] },
    { name: 'total-p95', values: ['p95 지연'], charts: [] },
    { name: 'chart-count', values: [], charts: ['요청량', '오류율 (5xx)'] },
    { name: 'chart-p95', values: [], charts: ['p95 지연'] },
  ];
  const CARDS = ['요청량', '오류율 (5xx)', 'p95 지연'];

  it.each(CARD_FAILURES)('카드 query $name 503: 그 값·차트만 "확인할 수 없음", 알림은 위에 한 번, 표는 정상', async ({ name, values, charts }) => {
    const base = handler;
    handler = (url, init) =>
      url.endsWith('/query/metrics') && queryName(JSON.parse(String(init.body)) as MetricQuery) === name ? unavailable.clone() : base(url, init);
    renderAt(`/o/acme/services/${ID}?tz=UTC`);
    const table = await screen.findByRole('region', { name: '리소스' });
    expect(await within(table).findByText('POST /checkout')).toBeTruthy();
    await vi.waitFor(() => expect(screen.getAllByText('request_id req-503').length).toBe(1));
    for (const title of CARDS) {
      const card = screen.getByRole('region', { name: title });
      await vi.waitFor(() => expect(card.querySelector('.mt-metric')?.textContent).not.toContain('불러오는 중'));
      const value = card.querySelector('.mt-metric')?.textContent ?? '';
      expect(value).not.toContain('받은 metric 없음');
      if (values.includes(title)) expect(value).toContain('확인할 수 없음(조회 실패)');
      else expect(value).not.toContain('확인할 수 없음');
      if (charts.includes(title)) expect(within(card).getByText('차트를 확인할 수 없음(조회 실패)')).toBeTruthy();
      else expect(within(card).queryByText('차트를 확인할 수 없음(조회 실패)')).toBeNull();
    }
    expect(screen.queryByText('이 서비스에서 HTTP 서버 metric을 받지 못했습니다')).toBeNull();
  });

  it.each(['route-count', 'route-p95', 'route-hist_sum'])('표 query %s 503: 표 자리에 실패와 "없다는 뜻이 아닙니다", 카드는 정상', async (name) => {
    const base = handler;
    handler = (url, init) =>
      url.endsWith('/query/metrics') && queryName(JSON.parse(String(init.body)) as MetricQuery) === name ? unavailable.clone() : base(url, init);
    renderAt(`/o/acme/services/${ID}?tz=UTC`);
    const table = await screen.findByRole('region', { name: '리소스' });
    expect(await within(table).findByText('endpoint가 없다는 뜻이 아닙니다.', { exact: false })).toBeTruthy();
    expect(within(table).getByText('request_id req-503')).toBeTruthy();
    expect(within(table).queryByRole('table')).toBeNull();
    expect(within(table).queryByText(/요청이 없습니다/)).toBeNull();
    expect(within(table).queryByText(/endpoint \d+개/)).toBeNull();
    // 같은 실패를 위쪽에 또 보이지 않는다
    expect(screen.getAllByText('request_id req-503').length).toBe(1);
    const card = screen.getByRole('region', { name: '오류율 (5xx)' });
    await vi.waitFor(() => expect(card.querySelector('.mt-metric')?.textContent?.replace(/\s+/g, ' ').trim()).toBe('2 %'));
  });

  it('리소스 탭에서 표 query가 실패해도 metric 없음 안내로 바꾸지 않는다', async () => {
    const base = handler;
    handler = (url, init) => {
      if (!url.endsWith('/query/metrics')) return base(url, init);
      const q = JSON.parse(String(init.body)) as MetricQuery;
      return queryName(q) === 'route-count' ? unavailable.clone() : json(200, { data: { series: [] }, meta });
    };
    renderAt(`/o/acme/services/${ID}?tz=UTC&tab=resources`);
    expect(await screen.findByText('endpoint가 없다는 뜻이 아닙니다.', { exact: false })).toBeTruthy();
    expect(screen.queryByText('이 서비스에서 HTTP 서버 metric을 받지 못했습니다')).toBeNull();
  });

  it('새로고침 실패로 이전 값을 보이는 카드가 있으면 대표 실패가 처음부터 실패한 query여도 "마지막 성공 기준"을 알린다', async () => {
    const base = handler;
    let failing = new Set(['chart-count']);
    handler = (url, init) =>
      url.endsWith('/query/metrics') && failing.has(queryName(JSON.parse(String(init.body)) as MetricQuery)) ? unavailable.clone() : base(url, init);
    renderAt(`/o/acme/services/${ID}?tz=UTC`);
    const card = await screen.findByRole('region', { name: '오류율 (5xx)' });
    await vi.waitFor(() => expect(card.querySelector('.mt-metric')?.textContent?.replace(/\s+/g, ' ').trim()).toBe('2 %'));
    expect(screen.getByRole('alert').textContent).not.toContain('마지막 성공');
    failing = new Set(['chart-count', 'total-count']);
    // 같은 조건의 새로고침(같은 분)이어야 이전 결과가 남는다. 지우지 말 것: vi.waitFor는 fake timer일 때 재시도마다
    // 시계(fake Date 포함)를 interval만큼 진행시키고, NOW가 분 경계라 1ms만 지나도 범위 끝(to)이 다음 분으로 올라가 조건이 바뀐다.
    vi.setSystemTime(NOW);
    await userEvent.click(screen.getByRole('button', { name: '새로고침' }));
    await vi.waitFor(() => expect(screen.getAllByRole('alert')[0]?.textContent).toContain('마지막 성공'));
    // 이전 값은 그대로 보이되 위 알림이 그 값의 기준 시각을 말한다
    expect(card.querySelector('.mt-metric')?.textContent?.replace(/\s+/g, ' ').trim()).toBe('2 %');
    expect(within(card).getByText('차트를 확인할 수 없음(조회 실패)')).toBeTruthy();
  });

  it('분이 바뀐 뒤 새로고침이 실패하면 이전 범위 값을 보이지 않고 "확인할 수 없음"(마지막 성공 기준 아님)', async () => {
    const base = handler;
    let failing = new Set<string>();
    handler = (url, init) =>
      url.endsWith('/query/metrics') && failing.has(queryName(JSON.parse(String(init.body)) as MetricQuery)) ? unavailable.clone() : base(url, init);
    renderAt(`/o/acme/services/${ID}?tz=UTC`);
    const card = await screen.findByRole('region', { name: '오류율 (5xx)' });
    await vi.waitFor(() => expect(card.querySelector('.mt-metric')?.textContent?.replace(/\s+/g, ' ').trim()).toBe('2 %'));
    failing = new Set(['total-count']);
    vi.setSystemTime(NOW + 60_000); // 상대 범위의 끝이 다음 분으로 → 조건이 바뀐 조회
    await userEvent.click(screen.getByRole('button', { name: '새로고침' }));
    await vi.waitFor(() => expect(card.querySelector('.mt-metric')?.textContent).toContain('확인할 수 없음(조회 실패)'));
    expect(screen.getByRole('region', { name: '요청량' }).querySelector('.mt-metric')?.textContent).toContain('확인할 수 없음(조회 실패)');
    expect(screen.getByRole('alert').textContent).not.toContain('마지막 성공');
  });

  it.each(['route-p95', 'route-hist_sum'])('표 query %s가 아직 오지 않으면 열을 "받은 metric 없음"으로 채우지 않고 skeleton', async (name) => {
    const base = handler;
    handler = (url, init) =>
      url.endsWith('/query/metrics') && queryName(JSON.parse(String(init.body)) as MetricQuery) === name ? new Promise<Response>(() => {}) : base(url, init);
    renderAt(`/o/acme/services/${ID}?tz=UTC`);
    const table = await screen.findByRole('region', { name: '리소스' });
    const card = screen.getByRole('region', { name: '오류율 (5xx)' });
    await vi.waitFor(() => expect(card.querySelector('.mt-metric')?.textContent?.replace(/\s+/g, ' ').trim()).toBe('2 %'));
    expect(within(table).getByRole('status', { name: '리소스를 불러오는 중' })).toBeTruthy();
    expect(within(table).queryByRole('table')).toBeNull();
    expect(within(table).queryByText('받은 metric 없음')).toBeNull();
  });

  it('범위를 바꾸면 이전 범위 값을 새 범위 값처럼 보이지 않는다(다시 불러오는 중)', async () => {
    const router = renderAt(`/o/acme/services/${ID}?tz=UTC&range=1h`);
    const card = await screen.findByRole('region', { name: '오류율 (5xx)' });
    await vi.waitFor(() => expect(card.querySelector('.mt-metric')?.textContent).toContain('2'));
    const base = handler;
    handler = (url, init) => (url.endsWith('/query/metrics') ? new Promise<Response>(() => {}) : base(url, init));
    await router.navigate(`/o/acme/services/${ID}?tz=UTC&range=4h`);
    await vi.waitFor(() => expect(screen.getByRole('region', { name: '오류율 (5xx)' }).querySelector('.mt-metric')?.textContent).toContain('불러오는 중'));
  });

  it('partial 이유: 범위 끝이 집계 확정 전이면 집계 중, 아니면 빠진 window, 모르면 확인할 수 없음', () => {
    const to = Date.parse('2026-10-04T06:00:00Z');
    expect(partialReason('2026-10-04T05:58:00Z', to)).toContain('최근 구간 집계 중');
    expect(partialReason('2026-10-04T06:05:00Z', to)).toContain('빠진 window');
    expect(partialReason(null, to)).toContain('확인할 수 없음');
  });

  it('리소스 표에서 이 서비스의 오류 trace 검색(S04)으로 간다 — 서비스 → 실패 trace', async () => {
    renderAt(`/o/acme/services/${ID}?tz=UTC&range=1h`);
    const link = await screen.findByRole('link', { name: '오류 trace 보기' });
    const href = new URL(link.getAttribute('href') ?? '', 'http://x');
    expect(href.pathname).toBe('/o/acme/traces');
    expect(Object.fromEntries(href.searchParams)).toEqual({ tz: 'UTC', range: '1h', service: ID, errors: '1' });
  });

  it('이 서비스의 오류 log(S08)로 간다', async () => {
    renderAt(`/o/acme/services/${ID}?tz=UTC&range=1h`);
    const link = await screen.findByRole('link', { name: '이 서비스의 오류 log' });
    const href = new URL(link.getAttribute('href') ?? '', 'http://x');
    expect(href.pathname).toBe('/o/acme/logs');
    expect(Object.fromEntries(href.searchParams)).toEqual({ tz: 'UTC', range: '1h', service: ID, sev: 'error' });
  });

  it('API가 없는 탭은 비어 있는 척하지 않고 이유를 말한다', async () => {
    renderAt(`/o/acme/services/${ID}?tab=dependencies`);
    expect(await screen.findByText(/service map API/)).toBeTruthy();
    const tabs = screen.getByRole('navigation', { name: '서비스 탭' });
    expect(within(tabs).getByRole('link', { name: '의존성' }).getAttribute('aria-current')).toBe('page');
  });
});
