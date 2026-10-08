import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { MetricQuery, MetricSeries } from '../../api/types.ts';
import { routes } from '../../app/routes.tsx';

const NOW = Date.parse('2026-10-04T06:00:00Z');
const meta = { request_id: 'req-o', schema_version: 1, partial: false, failed_shards: [], warnings: [], watermark: '2026-10-04T05:58:00Z' };
const SVC_A = 'aaaaaaaa-0000-4000-8000-000000000001';
const SVC_B = 'aaaaaaaa-0000-4000-8000-000000000002';

const catalog = [
  { service_id: SVC_A, name: 'checkout', namespace: 'shop', environment: 'prod', language: 'java', status: 'active', first_seen: '2026-10-01T00:00:00Z', last_seen: '2026-10-04T05:59:00Z', owner_team: null, repository_url: null, runbook_url: null, tier: null, tags: [] },
  { service_id: SVC_B, name: 'payment', namespace: 'shop', environment: 'prod', language: 'go', status: 'inactive', first_seen: '2026-10-01T00:00:00Z', last_seen: '2026-10-03T00:00:00Z', owner_team: null, repository_url: null, runbook_url: null, tier: null, tags: [] },
];

const series = (labels: Record<string, string>, v: number | null, unit = '{request}', n = 1): MetricSeries => ({
  labels,
  unit,
  points: Array.from({ length: n }, (_, i) => ({ t: new Date(NOW - (n - i) * 60_000).toISOString(), v, partial: false, ...(v === null ? { reason: 'no_data' as const } : {}) })),
  completeness: 1,
  missing: [],
  flags: [],
});
const svcLabels = (name: string) => ({ 'service.namespace': 'shop', 'service.name': name, 'deployment.environment.name': 'prod' });

let metricHandler: (q: MetricQuery) => MetricSeries[] | 'fail';
let catalogResponse: () => Response;
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const metricBodies = () =>
  vi
    .mocked(fetch)
    .mock.calls.filter(([u]) => String(u).endsWith('/query/metrics'))
    .map(([, init]) => JSON.parse(String((init as RequestInit).body)) as MetricQuery);

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(NOW);
  catalogResponse = () => json(200, { data: catalog, next_cursor: null, meta });
  metricHandler = (q) => {
    const g = q.expression.group_by ?? [];
    const n = q.step_seconds === 3600 ? 1 : 60;
    if (q.expression.aggregation === 'count') {
      if (g.includes('service.name')) {
        return [
          series({ ...svcLabels('checkout'), 'http.response.status_code': '200' }, 900),
          series({ ...svcLabels('checkout'), 'http.response.status_code': '503' }, 100),
          series({ ...svcLabels('rogue'), 'http.response.status_code': '200' }, 36),
        ];
      }
      return [series({ 'http.response.status_code': '200' }, 936 / n, '{request}', n), series({ 'http.response.status_code': '503' }, 100 / n, '{request}', n)];
    }
    if (g.includes('service.name')) return [series(svcLabels('checkout'), 0.42, 's'), series(svcLabels('rogue'), 0.05, 's')];
    return [series({}, 0.4, 's', n)];
  };
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string, init: RequestInit) => {
      if (url.includes('/api/v1/services')) return Promise.resolve(catalogResponse());
      if (url.endsWith('/query/metrics')) {
        const q = JSON.parse(String(init.body)) as MetricQuery;
        const out = metricHandler(q);
        return Promise.resolve(
          out === 'fail'
            ? json(503, { error: { code: 'UNAVAILABLE', message: 'x', request_id: 'req-fail', retryable: true } })
            : json(200, { data: { series: out }, meta }),
        );
      }
      return Promise.resolve(json(404, { error: { code: 'NOT_FOUND', message: 'x', request_id: 'r', retryable: false } }));
    }),
  );
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

const bodyRows = async () => {
  const table = await screen.findByRole('table', { name: '서비스 요약' });
  return within(table).getAllByRole('row').slice(1);
};

describe('Overview (S01)', () => {
  it('조직 전체 RED: 비샘플링 histogram을 서비스 키로 group한 한 번의 조회, 서비스별 요청을 따로 부르지 않는다', async () => {
    renderAt('/o/acme/overview?tz=UTC&range=1h');
    await bodyRows();
    const bodies = metricBodies();
    expect(bodies.every((b) => b.expression.metric === 'http.server.request.duration')).toBe(true);
    expect(bodies).toHaveLength(6); // 차트 2 + 합계 2 + 서비스 표 2 — 서비스 수와 무관
    const svcCount = bodies.find((b) => b.expression.aggregation === 'count' && b.expression.group_by?.includes('service.name'));
    expect(svcCount).toMatchObject({
      step_seconds: 3600,
      expression: { group_by: ['service.namespace', 'service.name', 'deployment.environment.name', 'http.response.status_code'] },
    });
    expect(svcCount?.expression.filter).toBeUndefined();
    expect(screen.getByText('SDK metric · 비샘플링')).toBeTruthy();
    expect(screen.getByRole('region', { name: '오류율 (5xx)' }).textContent).toContain('9.65'); // 100 / 1036
    expect(screen.getByRole('region', { name: 'p95 지연' }).textContent).toContain('400 ms');
  });

  it('서비스 표: 오류율 큰 순, 값 없는 서비스는 뒤에 이유와 함께, 미등록 서비스는 링크 없이', async () => {
    renderAt('/o/acme/overview?tz=UTC&range=1h');
    const rows = await bodyRows();
    expect(rows.map((r) => within(r).getByRole('rowheader').textContent)).toEqual(['shop/checkout', 'shop/rogue catalog 미등록', 'shop/payment']);
    expect(rows[0]?.textContent).toContain('10%');
    expect(rows[0]?.textContent).toContain('420 ms');
    expect(within(rows[0] as HTMLElement).getByRole('link', { name: 'shop/checkout' }).getAttribute('href')).toBe(`/o/acme/services/${SVC_A}?range=1h&tz=UTC`);
    expect(within(rows[1] as HTMLElement).queryByRole('link')).toBeNull();
    expect(rows[2]?.textContent).toContain('요청 metric 없음');
    expect(rows[2]?.textContent).toContain('24시간 미수신');
    expect(screen.getByText(/정상·이상 판정은 하지 않습니다/)).toBeTruthy();
  });

  it('정렬은 URL에 남고 열 머리에 aria-sort', async () => {
    const user = userEvent.setup();
    const router = renderAt('/o/acme/overview?tz=UTC&range=1h');
    await bodyRows();
    await user.click(screen.getByRole('button', { name: /요청량/ }));
    expect(new URLSearchParams(router.state.location.search).get('sort')).toBe('rps');
    expect(screen.getByRole('columnheader', { name: /요청량/ }).getAttribute('aria-sort')).toBe('descending');
    const rows = await bodyRows();
    expect(within(rows[0] as HTMLElement).getByRole('rowheader').textContent).toBe('shop/checkout');
    expect(within(rows[1] as HTMLElement).getByRole('rowheader').textContent).toContain('rogue');
  });

  it('환경 context는 deployment.environment.name 조건과 catalog 환경으로', async () => {
    renderAt('/o/acme/overview?tz=UTC&range=1h&env=prod');
    await bodyRows();
    expect(metricBodies().every((b) => JSON.stringify(b.expression.filter) === JSON.stringify({ op: 'eq', field: 'deployment.environment.name', value: 'prod' }))).toBe(true);
    const svcCall = vi.mocked(fetch).mock.calls.find(([u]) => String(u).includes('/api/v1/services'));
    expect(String(svcCall?.[0])).toContain('environment=prod');
  });

  it('metric이 하나도 없으면 0 요청이 아니라 수신 없음으로', async () => {
    metricHandler = () => [];
    renderAt('/o/acme/overview?tz=UTC&range=1h');
    expect(await screen.findByText('이 범위에 HTTP 서버 metric이 없습니다')).toBeTruthy();
    expect(screen.getByText(/요청이 0건이라는 뜻이 아닙니다/)).toBeTruthy();
    const rows = await bodyRows();
    expect(rows.every((r) => r.textContent?.includes('요청 metric 없음') === true)).toBe(true);
  });

  it('서비스 표 조회가 실패하면 "서비스 없음"이 아니라 실패를 보인다', async () => {
    const ok = metricHandler;
    metricHandler = (q) => (q.expression.group_by?.includes('service.name') === true ? 'fail' : ok(q));
    renderAt('/o/acme/overview?tz=UTC&range=1h');
    expect(await screen.findByText(/서비스 요약을 불러오지 못했습니다/)).toBeTruthy();
    expect(screen.queryByText('표시할 서비스가 없습니다')).toBeNull();
    expect(screen.getAllByText(/req-fail/)).toHaveLength(1); // 같은 실패를 위·아래 두 번 보이지 않는다
  });

  it('합계 조회가 실패하면 카드는 "metric 없음"이 아니라 확인할 수 없음', async () => {
    const ok = metricHandler;
    metricHandler = (q) => (q.expression.aggregation === 'p95' && (q.expression.group_by ?? []).length === 0 ? 'fail' : ok(q));
    renderAt('/o/acme/overview?tz=UTC&range=1h');
    const card = await screen.findByRole('region', { name: 'p95 지연' });
    await vi.waitFor(() => expect(card.textContent).toContain('확인할 수 없음(조회 실패)'));
    expect(card.textContent).not.toContain('받은 metric 없음');
  });

  it('catalog가 실패하거나 잘리면 metric 서비스를 "미등록"으로 단정하지 않는다', async () => {
    catalogResponse = () => json(503, { error: { code: 'UNAVAILABLE', message: 'x', request_id: 'req-cat', retryable: true } });
    renderAt('/o/acme/overview?tz=UTC&range=1h');
    const rows = await bodyRows();
    expect(rows.every((r) => r.textContent?.includes('catalog 확인 안 됨') === true)).toBe(true);
    expect(screen.queryByText('catalog 미등록')).toBeNull();
    expect(screen.getByText(/등록 여부·수신 상태를 확인할 수 없습니다/)).toBeTruthy();
  });

  it('catalog가 page 한도에서 잘리면 목록 밖 서비스는 확인 안 됨', async () => {
    catalogResponse = () => json(200, { data: catalog, next_cursor: 'more', meta });
    renderAt('/o/acme/overview?tz=UTC&range=1h');
    const rows = await bodyRows();
    const rogue = rows.find((r) => r.textContent?.includes('rogue') === true);
    expect(rogue?.textContent).toContain('catalog 확인 안 됨');
    expect(within(rows[0] as HTMLElement).getByRole('link', { name: 'shop/checkout' })).toBeTruthy();
  });

  it('조직 p95를 병합할 수 없으면(서비스마다 bucket 경계가 다름) 값 대신 이유', async () => {
    const ok = metricHandler;
    metricHandler = (q) =>
      q.expression.aggregation === 'p95' && (q.expression.group_by ?? []).length === 0
        ? [{ ...series({}, null, 's'), points: [{ t: new Date(NOW - 60_000).toISOString(), v: null, reason: 'bounds_mismatch' as const, partial: false }] }]
        : ok(q);
    renderAt('/o/acme/overview?tz=UTC&range=1h');
    const card = await screen.findByRole('region', { name: 'p95 지연' });
    await vi.waitFor(() => expect(card.textContent).toContain('bucket 경계 불일치'));
  });

  it('7일을 넘는 범위는 조회하지 않는다', async () => {
    renderAt('/o/acme/overview?tz=UTC&from=2026-09-20T00:00:00Z&to=2026-10-04T00:00:00Z');
    expect(await screen.findByText(/7일까지 조회할 수 있습니다/)).toBeTruthy();
    expect(metricBodies()).toHaveLength(0);
  });
});
