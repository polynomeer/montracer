import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { MetricAggregation, MetricDescriptor, MetricQuery, MetricSeries, MetricVariant } from '../../api/types.ts';
import { routes } from '../../app/routes.tsx';

const NOW = Date.parse('2026-10-04T06:00:00Z');
const meta = { request_id: 'req-m', schema_version: 1, partial: false, failed_shards: [], warnings: [] as string[] };
const last = '2026-10-04T05:59:00Z';

const v = (type: string, unit: string, aggregations: MetricAggregation[], extra: Partial<MetricVariant> = {}): MetricVariant => ({
  type,
  temporality: 'cumulative',
  monotonic: false,
  unit,
  series: 3,
  last_seen: last,
  aggregations,
  ...extra,
});

const DICT: MetricDescriptor[] = [
  { name: 'http.server.requests', conflict: false, variants: [v('sum', '{request}', ['rate', 'increase', 'sum'], { monotonic: true })] },
  { name: 'http.server.request.duration', conflict: false, variants: [v('histogram', 'ms', ['p50', 'p90', 'p95', 'p99', 'count', 'hist_sum'], { temporality: 'delta' })] },
  { name: 'jvm.memory.used', conflict: false, variants: [v('gauge', 'By', ['avg', 'min', 'max'], { temporality: 'unspecified' })] },
  {
    name: 'mixed.latency',
    conflict: true,
    variants: [v('histogram', 'ms', ['p50', 'p90', 'p95', 'p99', 'count', 'hist_sum']), v('histogram', 's', ['p50', 'p90', 'p95', 'p99', 'count', 'hist_sum'])],
  },
  { name: 'rpc.summary', conflict: false, variants: [v('summary', 'ms', [], { temporality: 'unspecified' })] },
  // 같은 이름에 delta counter(증가량)와 delta up-down counter(순변화) — 교집합 [sum]으로 합치면 섞인 값이 정상값처럼 보인다
  {
    name: 'mixed.sum',
    conflict: true,
    variants: [v('sum', '1', ['rate', 'increase', 'sum'], { temporality: 'delta', monotonic: true }), v('sum', '1', ['sum'], { temporality: 'delta' })],
  },
];

const pts = (vals: (number | null)[], extra: Partial<MetricSeries['points'][number]>[] = []) =>
  vals.map((val, i) => ({ t: new Date(NOW - (vals.length - i) * 60_000).toISOString(), v: val, partial: false, ...(val === null ? { reason: 'no_data' as const } : {}), ...extra[i] }));

let seriesResult: MetricSeries[];
let resultMeta: typeof meta & { watermark?: string | null };

type Handler = (url: string, init: RequestInit) => Response | Promise<Response>;
let handler: Handler;
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const calls = (part: string) => vi.mocked(fetch).mock.calls.filter(([u]) => String(u).includes(part));
const metricBodies = () => calls('/query/metrics').map(([, init]) => JSON.parse(String((init as RequestInit).body)) as MetricQuery);

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(NOW);
  seriesResult = [
    { labels: { 'service.name': 'checkout' }, unit: '{request}', points: pts([1, 2, null, 4], [{}, { partial: true }, { reason: 'pending' }]), completeness: 0.75, missing: [], flags: [] },
  ];
  resultMeta = { ...meta, watermark: '2026-10-04T05:58:00Z' };
  handler = (url) => {
    const u = new URL(url, 'http://x');
    if (u.pathname === '/api/v1/metrics') {
      const q = (u.searchParams.get('q') ?? '').toLowerCase();
      return json(200, { data: DICT.filter((d) => d.name.toLowerCase().includes(q)), next_cursor: null, meta });
    }
    if (u.pathname === '/api/v1/metrics/labels') {
      return json(200, {
        data: { metric: u.searchParams.get('metric'), keys: [{ key: 'service.name', sources: ['resource'], series: 3 }, { key: 'http.route', sources: ['attribute'], series: 2 }] },
        meta,
      });
    }
    if (u.pathname === '/api/v1/query/metrics') {
      return json(200, { data: { series: seriesResult, source_window_seconds: 60, range: { from: '2026-10-04T05:00:00Z', to: '2026-10-04T06:00:00Z' } }, meta: resultMeta });
    }
    return json(404, { error: { code: 'NOT_FOUND', message: 'x', request_id: 'r', retryable: false } });
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

const search = (router: ReturnType<typeof renderAt>) => new URLSearchParams(router.state.location.search);

describe('MetricsExplorer (S08)', () => {
  it('사전: 유형·단위를 먼저, 충돌은 배지, 7일 범위면 마지막 24시간으로 조회', async () => {
    renderAt('/o/acme/metrics?tz=UTC&range=7d');
    const list = await screen.findByRole('list', { name: 'metric 목록' });
    const items = within(list).getAllByRole('button');
    expect(items[0]?.textContent).toContain('counter · {request}');
    expect(items[2]?.textContent).toContain('gauge · By');
    expect(items[3]?.textContent).toContain('계측 충돌');
    const u = new URL(String(calls('/api/v1/metrics?')[0]?.[0]), 'http://x');
    expect(u.searchParams.get('from')).toBe('2026-10-03T06:00:00.000Z');
    expect(u.searchParams.get('to')).toBe('2026-10-04T06:00:00.000Z');
    expect(screen.getByText(/마지막 24시간에 관측된 metric/)).toBeTruthy();
    expect(screen.getByText('metric을 고르세요')).toBeTruthy();
    expect(calls('/query/metrics')).toHaveLength(0);
  });

  it('counter를 고르면 rate로 조회하고 legend에 해상도·rollup·집계 완료 시각, 결측·집계 중·일부 집계를 밝힌다', async () => {
    const user = userEvent.setup();
    const router = renderAt('/o/acme/metrics?tz=UTC&range=1h');
    await user.click(await screen.findByRole('button', { name: /http\.server\.requests/ }));
    expect(search(router).get('metric')).toBe('http.server.requests');
    await screen.findByRole('heading', { name: /초당 증가율/ });
    expect(metricBodies().at(-1)).toEqual({
      range: { from: '2026-10-04T05:00:00.000Z', to: '2026-10-04T06:00:00.000Z' },
      step_seconds: 60,
      expression: { metric: 'http.server.requests', aggregation: 'rate' },
    });
    expect(screen.getByRole('heading', { name: '초당 증가율 ({request}/s)' })).toBeTruthy();
    expect(screen.getByText(/해상도 1분 · 1분 rollup · .*까지 집계 완료/)).toBeTruthy();
    expect(screen.getByText(/끊긴 선: 값 없음\(0이 아님/)).toBeTruthy();
    expect(screen.getByText(/음영: 집계 중/)).toBeTruthy();
    expect(screen.getByText(/빈 원: 일부만 집계/)).toBeTruthy();
    // series 요약: 값 있는 step 75%, 결측 사유
    const row = screen.getByRole('rowheader', { name: /전체$/ }).closest('tr') as HTMLElement;
    expect(row.textContent).toContain('75%');
    expect(row.textContent).toContain('값 없는 step: 집계 중');
    // 연산은 counter가 허용하는 것만
    const opts = within(screen.getByLabelText('연산')).getAllByRole('option').map((o) => o.getAttribute('value'));
    expect(opts).toEqual(['rate', 'increase', 'sum']);
  });

  it('gauge에는 rate를 고를 수 없고, URL의 rate는 무시하고 avg로', async () => {
    renderAt('/o/acme/metrics?tz=UTC&range=1h&metric=jvm.memory.used&agg=rate');
    await screen.findByRole('heading', { name: /평균/ });
    const opts = within(screen.getByLabelText('연산')).getAllByRole('option').map((o) => o.getAttribute('value'));
    expect(opts).toEqual(['avg', 'min', 'max']);
    expect(metricBodies().every((b) => b.expression.aggregation === 'avg')).toBe(true);
  });

  it('summary는 조회하지 않고 이유를 보인다(quantile 평균 금지)', async () => {
    renderAt('/o/acme/metrics?tz=UTC&range=1h&metric=rpc.summary');
    expect(await screen.findByText(/summary quantile은 합칠 수 없음/)).toBeTruthy();
    expect((screen.getByLabelText<HTMLSelectElement>('연산')).disabled).toBe(true);
    expect(calls('/query/metrics')).toHaveLength(0);
  });

  it('단위만 다른 충돌: 경고하고 같은 연산으로 조회(단위가 섞인 series는 서버가 단위 충돌로)', async () => {
    renderAt('/o/acme/metrics?tz=UTC&range=1h&metric=mixed.latency');
    expect(await screen.findByText(/단위·temporality가 다른 계측이 섞여/)).toBeTruthy();
    await vi.waitFor(() => expect(metricBodies().at(-1)?.expression.aggregation).toBe('p95'));
  });

  it('의미가 다른 조합(counter + up-down counter)은 합쳐 그리지 않는다', async () => {
    renderAt('/o/acme/metrics?tz=UTC&range=1h&metric=mixed.sum');
    expect(await screen.findByText(/의미가 다른 계측.*섞여 있어 합쳐 그릴 수 없습니다/)).toBeTruthy();
    expect(screen.getByLabelText<HTMLSelectElement>('연산').disabled).toBe(true);
    expect(calls('/query/metrics')).toHaveLength(0);
  });

  it('환경 context는 deployment.environment.name 조건으로 자동 적용, group by·조건은 URL과 요청에', async () => {
    const user = userEvent.setup();
    const router = renderAt('/o/acme/metrics?tz=UTC&range=1h&env=prod&metric=http.server.request.duration');
    await screen.findByRole('heading', { name: /p95/ });
    expect(metricBodies().at(-1)?.expression.filter).toEqual({ op: 'eq', field: 'deployment.environment.name', value: 'prod' });
    expect(screen.getByText(/상단 환경 선택에서 자동 적용/)).toBeTruthy();

    await user.click(await screen.findByRole('checkbox', { name: 'service.name' }));
    expect(search(router).get('group')).toBe('service.name');
    await user.click(screen.getByRole('button', { name: '조건 더하기' }));
    await user.selectOptions(screen.getByLabelText('조건 1 label'), 'http.route');
    await user.type(screen.getByLabelText('조건 1 값'), '/cart');
    await user.click(screen.getByRole('button', { name: '조회' }));
    expect(search(router).getAll('f')).toEqual(['http.route=/cart']);
    await vi.waitFor(() =>
      expect(metricBodies().at(-1)?.expression).toEqual({
        metric: 'http.server.request.duration',
        aggregation: 'p95',
        group_by: ['service.name'],
        filter: {
          op: 'and',
          args: [
            { op: 'eq', field: 'deployment.environment.name', value: 'prod' },
            { op: 'eq', field: 'http.route', value: '/cart' },
          ],
        },
      }),
    );
    expect(screen.getByText(/bucket을 합친 뒤 계산/)).toBeTruthy();
  });

  it('사전에 없는 metric은 유형을 추측하지 않고 연산을 고를 때까지 조회하지 않는다', async () => {
    const user = userEvent.setup();
    renderAt('/o/acme/metrics?tz=UTC&range=1h&metric=old.metric');
    expect(await screen.findByText(/관측되지 않은 metric입니다/)).toBeTruthy();
    expect(calls('/query/metrics')).toHaveLength(0);
    await user.selectOptions(screen.getByLabelText('연산'), 'max');
    await vi.waitFor(() => expect(metricBodies().at(-1)?.expression.aggregation).toBe('max'));
  });

  it('사전에 없어도 공유 링크에 연산이 있으면 그 연산으로 조회한다(맞지 않으면 서버가 해당 없음)', async () => {
    renderAt('/o/acme/metrics?tz=UTC&range=1h&metric=old.metric&agg=p99');
    await vi.waitFor(() => expect(metricBodies().at(-1)?.expression.aggregation).toBe('p99'));
  });

  it('series가 많으면 일부만 그린다고 알리고 표에는 모두, 빈 결과는 0과 구분', async () => {
    seriesResult = Array.from({ length: 12 }, (_, i) => ({
      labels: { 'http.route': `/r${String(i)}` },
      unit: 'By',
      points: pts([i, i + 1]),
      completeness: 1,
      missing: [],
      flags: i === 0 ? ['unit_conflict'] : [],
    }));
    renderAt('/o/acme/metrics?tz=UTC&range=1h&metric=jvm.memory.used&group=http.route');
    expect(await screen.findByText(/series 12개 중 8개만 그렸습니다/)).toBeTruthy();
    expect(screen.getAllByRole('rowheader')).toHaveLength(12);
    expect(screen.getByRole('rowheader', { name: /http\.route=\/r0/ }).closest('tr')?.textContent).toContain('단위 충돌');
  });

  it('조건에 맞는 series가 없으면 0이 아니라 없다고', async () => {
    seriesResult = [];
    renderAt('/o/acme/metrics?tz=UTC&range=1h&metric=jvm.memory.used');
    expect(await screen.findByText('조건에 맞는 series가 없습니다')).toBeTruthy();
  });

  it('7일을 넘는 범위는 조회하지 않고 안내', async () => {
    renderAt('/o/acme/metrics?tz=UTC&from=2026-09-20T00:00:00Z&to=2026-10-04T00:00:00Z&metric=jvm.memory.used');
    expect(await screen.findByText(/7일까지 조회할 수 있습니다/)).toBeTruthy();
    expect(calls('/query/metrics')).toHaveLength(0);
  });
});
