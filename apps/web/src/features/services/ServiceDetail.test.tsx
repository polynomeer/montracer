import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { MetricQuery, MetricSeries, ServiceItem } from '../../api/types.ts';
import { routes } from '../../app/routes.tsx';
import { partialReason, shareText } from './ServiceDetail.tsx';
import { METHOD_KEY, ROUTE_KEY, STATUS_KEY } from './red.ts';
import { metricData, queryMeta } from '../../test/fixtures';

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

const meta = queryMeta();

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

const mapData = {
  nodes: [
    { node_id: ID, kind: 'service', name: 'checkout', service_id: ID, namespace: 'shop', environment: 'prod' },
    { node_id: 'db:postgresql@prod', kind: 'database', name: 'postgresql', service_id: null, namespace: null, environment: 'prod' },
    { node_id: 'peer:fraud@prod', kind: 'external', name: 'fraud', service_id: null, namespace: null, environment: 'prod' },
    { node_id: 'aaaaaaaa-0000-4000-8000-000000000009', kind: 'service', name: 'web', service_id: 'aaaaaaaa-0000-4000-8000-000000000009', namespace: '', environment: 'prod' },
  ],
  edges: [
    { source: ID, destination: 'db:postgresql@prod', connection: 'rpc', confidence: 'observed', requests: 200, errors: 0, latency: { p50_ms: 2, p95_ms: 9, p99_ms: 12 }, queue_wait: null, last_seen: '2026-10-04T05:59:00Z' },
    { source: ID, destination: 'peer:fraud@prod', connection: 'rpc', confidence: 'estimated', requests: null, errors: null, latency: null, queue_wait: null, last_seen: '2026-10-04T05:59:00Z' },
    { source: 'aaaaaaaa-0000-4000-8000-000000000009', destination: ID, connection: 'rpc', confidence: 'observed', requests: 0, errors: 0, latency: null, queue_wait: null, last_seen: '2026-10-04T05:59:00Z' },
  ],
  omitted_count: { nodes: 0, edges: 0 },
};

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
      return json(200, { data: metricData(seriesFor(q)), meta });
    }
    if (url.endsWith('/api/v1/service-map/query')) {
      const req = JSON.parse(String(init.body)) as { range: { from: string; to: string } };
      return json(200, { data: { ...mapData, range: req.range }, meta });
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
    // 서비스 상태 API가 없는 배포(404): 판정하지 않는다고 말한다(정상 아님)
    expect(await screen.findByText('판정 없음 · 이 배포에서는 서비스 상태를 쓸 수 없음')).toBeTruthy();
  });

  it('머리 판정: 걸린 monitor의 가장 나쁜 상태·시작 시각과 monitor 링크, 걸린 것이 없으면 판정 없음 (ADR 0073)', async () => {
    const base = handler;
    let services: unknown[] = [
      {
        service_namespace: 'shop', service_name: 'checkout', environment: null, state: 'ALERT', stale: false, state_since: '2026-10-04T05:30:00Z',
        monitor_count: 2, name_only: false, includes_all_environment_monitors: false, partial: false,
        monitors: [{ id: 'bbbbbbbb-0000-4000-8000-000000000001', name: 'checkout 오류율', state: 'ALERT', stale: false, all_environments: true }],
      },
    ];
    handler = (url, init) =>
      url.includes('/api/v1/service-health')
        ? json(200, { data: { services, unattributed: [] }, meta: { request_id: 'h', schema_version: 1, warnings: [] } })
        : base(url, init);
    const first = render(<RouterProvider router={createMemoryRouter(routes, { initialEntries: [`/o/acme/services/${ID}?tz=UTC`] })} />);
    expect(await screen.findByText('경보 · 05:30부터 · 전체 환경 monitor 포함')).toBeTruthy();
    expect(screen.getByRole('link', { name: 'checkout 오류율' }).getAttribute('href')).toContain('/o/acme/monitors/bbbbbbbb-0000-4000-8000-000000000001');
    expect(screen.getByText(/걸린 monitor 2개/)).toBeTruthy();
    const call = vi.mocked(fetch).mock.calls.map(([u]) => String(u)).find((u) => u.includes('/service-health'));
    expect(call).toContain('environment=prod');
    first.unmount();
    services = [];
    renderAt(`/o/acme/services/${ID}?tz=UTC`);
    expect(await screen.findByText('판정 없음 · 걸린 monitor 없음')).toBeTruthy();
  });

  it('환경 속성이 없는 서비스는 environment 없이 조회하고(빈 값은 400) 빈 환경 행에 맞춘다', async () => {
    const base = handler;
    handler = (url, init) => {
      if (url.endsWith(`/api/v1/services/${ID}`)) return json(200, { data: { ...svc, environment: '' }, meta });
      if (url.includes('/api/v1/service-health')) {
        return json(200, {
          data: {
            services: [
              {
                service_namespace: 'shop', service_name: 'checkout', environment: '', state: 'PENDING', stale: true, state_since: null,
                monitor_count: 1, name_only: false, includes_all_environment_monitors: false, partial: false, monitors: [],
              },
            ],
            unattributed: [],
          },
          meta: { request_id: 'h', schema_version: 1, warnings: [] },
        });
      }
      return base(url, init);
    };
    renderAt(`/o/acme/services/${ID}?tz=UTC`);
    expect(await screen.findByText('대기 · 갱신 멈춤')).toBeTruthy();
    const call = vi.mocked(fetch).mock.calls.map(([u]) => String(u)).find((u) => u.includes('/service-health'));
    expect(call).not.toContain('environment=');
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
    // 영역이 먼저 그려지고 조회는 effect에서 나간다 — 7개가 다 나갈 때까지 기다린다(CI에서 0개로 실패한 적 있음)
    const metricCalls = () => vi.mocked(fetch).mock.calls.filter(([u]) => String(u).endsWith('/query/metrics'));
    await waitFor(() => expect(metricCalls().length).toBe(7));
    const calls = metricCalls();
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
    handler = (url, init) => (url.endsWith('/query/metrics') ? json(200, { data: metricData([]), meta }) : base(url, init));
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
            headers: { 'Retry-After': '1', 'Content-Type': 'application/json' },
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
      return queryName(q) === 'route-count' ? unavailable.clone() : json(200, { data: metricData([]), meta });
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

  it('새로고침 실패 안내의 마지막 성공 시각은 브라우저가 아니라 조사 context 시간대(?tz=)로 보인다', async () => {
    const browserTz = process.env.TZ;
    process.env.TZ = 'Asia/Seoul';
    try {
      // 전제: 브라우저 로컬(Asia/Seoul)로는 15시, 조사 context(UTC)로는 06시
      expect(new Date(NOW).getHours()).toBe(15);
      renderAt(`/o/acme/services/${ID}?tz=UTC&range=1h`);
      await screen.findByRole('heading', { name: 'checkout' });
      const base = handler;
      handler = (url, init) =>
        url.endsWith(`/api/v1/services/${ID}`)
          ? json(503, { error: { code: 'UNAVAILABLE', message: 'x', request_id: 'req-503', retryable: true } })
          : base(url, init);
      await userEvent.click(screen.getByRole('button', { name: '새로고침' }));
      expect(await screen.findByText('마지막 성공 06:00:00 기준 데이터를 보여주고 있습니다.')).toBeTruthy();
    } finally {
      if (browserTz === undefined) delete process.env.TZ;
      else process.env.TZ = browserTz;
    }
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
    renderAt(`/o/acme/services/${ID}?tab=instances`);
    expect(await screen.findByText(/instance·version/)).toBeTruthy();
    const tabs = screen.getByRole('navigation', { name: '서비스 탭' });
    expect(within(tabs).getByRole('link', { name: '인스턴스' }).getAttribute('aria-current')).toBe('page');
  });

  it('개요의 하위 의존성: 표본 출처, DB system, 추정은 수 없이, 이 서비스로 좁힌 조회', async () => {
    renderAt(`/o/acme/services/${ID}`);
    const panel = await screen.findByRole('region', { name: '하위 의존성' });
    await waitFor(() => expect(within(panel).getByText('postgresql')).toBeTruthy());
    expect(within(panel).getByText('표본 trace 기반 · 1분 집계')).toBeTruthy();
    expect(within(panel).getByText('DB · postgresql')).toBeTruthy();
    expect(within(panel).getByText('p95 9 ms')).toBeTruthy();
    expect(within(panel).getByText(/^오류율 0(\.0+)?%$/)).toBeTruthy(); // 요청 200·오류 0은 실제 0%
    expect(within(panel).getByText('호출 수 미표시')).toBeTruthy();
    expect(within(panel).getByRole('link', { name: /상위 1/ })).toBeTruthy();
    const call = vi.mocked(fetch).mock.calls.find(([u]) => String(u).endsWith('/service-map/query'));
    expect(JSON.parse(String((call?.[1] as RequestInit).body))).toMatchObject({ service_ids: [ID] });
  });

  it('의존성 탭: 하위·상위 표, 요청 0이면 오류율은 0%가 아니라 없음', async () => {
    renderAt(`/o/acme/services/${ID}?tab=dependencies`);
    const up = await screen.findByRole('region', { name: /상위 — 이 서비스를 부름/ });
    await waitFor(() => expect(within(up).getByText('web')).toBeTruthy());
    const row = within(up).getByText('web').closest('tr');
    const cells = [...(row?.querySelectorAll('td') ?? [])].map((c) => c.textContent);
    expect(cells[5]).toBe('없음'); // 오류율 열: 요청 0이면 계산하지 않는다
    expect(cells.some((c) => /^0(\.0+)?%$/.test(c ?? ''))).toBe(false);
    expect(screen.getByRole('region', { name: /하위 — 이 서비스가 부름 \(2\)/ })).toBeTruthy();
  });

  it('리소스: 소요시간 비율 막대, endpoint 필터(맞는 것 없으면 이유), p95 카드의 느린 trace 링크', async () => {
    renderAt(`/o/acme/services/${ID}?tab=resources`);
    const table = await screen.findByRole('region', { name: '리소스' });
    await waitFor(() => expect(within(table).getByText('/checkout', { exact: false })).toBeTruthy());
    expect(within(table).getAllByRole('img', { name: /전체 소요시간의 \d+%/ }).length).toBeGreaterThan(0);
    const filter = within(table).getByRole('searchbox', { name: 'endpoint 필터' });
    await userEvent.type(filter, 'zzz');
    expect(within(table).getByText('“zzz”에 맞는 endpoint가 없습니다')).toBeTruthy();
    await userEvent.clear(filter);
    await userEvent.type(filter, 'CHECK');
    expect(within(table).getByText('/checkout', { exact: false })).toBeTruthy();
  });

  it('p95 카드에서 p95(올림) 이상 걸린 이 서비스의 trace로 간다 — trace 전체 시간 기준', async () => {
    renderAt(`/o/acme/services/${ID}`);
    const link = await screen.findByRole('link', { name: /이상 걸린 trace 보기/ });
    const href = new URL(link.getAttribute('href') ?? '', 'http://x');
    expect(href.pathname).toBe('/o/acme/traces');
    expect(href.searchParams.get('service')).toBe(ID);
    expect(href.searchParams.get('min_ms')).toBe('2375');
  });

  it('endpoint 필터는 URL(?ep=)에서 복원된다(뒤로 가기·탭 전환)', async () => {
    renderAt(`/o/acme/services/${ID}?tab=resources&ep=zzz`);
    const table = await screen.findByRole('region', { name: '리소스' });
    expect(await within(table).findByText('“zzz”에 맞는 endpoint가 없습니다')).toBeTruthy();
    expect((within(table).getByRole('searchbox', { name: 'endpoint 필터' }) as HTMLInputElement).value).toBe('zzz');
  });

  it('24시간을 넘는 범위에서는 느린 trace 링크 대신 이유를 말한다(trace 검색 상한)', async () => {
    renderAt(`/o/acme/services/${ID}?range=7d`);
    expect(await screen.findByText('느린 trace 찾기는 24시간 이하 범위에서')).toBeTruthy();
    expect(screen.queryByRole('link', { name: /이상 걸린 trace 보기/ })).toBeNull();
  });

  it('의존성 조회가 실패해도 RED 카드는 그대로, 실패는 패널 안에서', async () => {
    const base = handler;
    handler = (url, init) =>
      url.endsWith('/service-map/query') ? json(503, { error: { code: 'UNAVAILABLE', message: 'x', request_id: 'req-map', retryable: true } }) : base(url, init);
    renderAt(`/o/acme/services/${ID}`);
    const panel = await screen.findByRole('region', { name: '하위 의존성' });
    await waitFor(() => expect(within(panel).getByRole('alert').textContent).toContain('req-map'));
    expect(screen.getByRole('region', { name: '요청량' }).textContent).toContain('req/s');
  });

  it('service map이 꺼진 배포(404)는 "의존성 없음"이 아니라 꺼짐이라고 말한다', async () => {
    const base = handler;
    handler = (url, init) =>
      url.endsWith('/service-map/query') ? json(404, { error: { code: 'NOT_FOUND', message: 'x', request_id: 'r', retryable: false } }) : base(url, init);
    renderAt(`/o/acme/services/${ID}`);
    const panel = await screen.findByRole('region', { name: '하위 의존성' });
    expect(await within(panel).findByText('이 배포에서는 의존성을 볼 수 없습니다')).toBeTruthy();
    expect(within(panel).queryByText('이 범위에 부른 서비스가 없습니다')).toBeNull();
  });

  it('의존성의 집계 전·잘림 경고를 패널에 보인다(0이 아니라 미계산)', async () => {
    const base = handler;
    handler = (url, init) => {
      if (!url.endsWith('/service-map/query')) return base(url, init);
      const req = JSON.parse(String(init.body)) as { range: { from: string; to: string } };
      return json(200, { data: { ...mapData, range: req.range }, meta: { ...meta, watermark: null, warnings: ['range_after_watermark', 'edge_rows_truncated'] } });
    };
    renderAt(`/o/acme/services/${ID}`);
    const panel = await screen.findByRole('region', { name: '하위 의존성' });
    expect(await within(panel).findByText(/아직 집계 전입니다/)).toBeTruthy();
    expect(within(panel).getByText(/일부를 읽지 못했습니다/)).toBeTruthy();
  });
});

describe('shareText', () => {
  it('0보다 큰 비율은 0%로 쓰지 않는다', () => {
    expect(shareText(0.003)).toBe('1% 미만');
    expect(shareText(0)).toBe('0%');
    expect(shareText(0.426)).toBe('43%');
  });
});
