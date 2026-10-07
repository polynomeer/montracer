import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { SearchRequest, TraceSummary } from '../../api/types.ts';
import { routes } from '../../app/routes.tsx';

const NOW = Date.parse('2026-10-04T06:00:00Z');
const SVC = 'aaaaaaaa-0000-4000-8000-000000000001';
const meta = { request_id: 'req-t', schema_version: 1, partial: false, failed_shards: [], warnings: [] };

const row = (n: number, extra: Partial<TraceSummary> = {}): TraceSummary => ({
  trace_id: String(n).repeat(32).slice(0, 32),
  start_time: new Date(NOW - n * 60_000).toISOString(),
  duration_ms: 100 * n,
  span_count: 3,
  has_error: false,
  complete: true,
  reasons: [],
  root_service: 'checkout',
  root_service_id: SVC,
  root_name: 'POST /checkout',
  ...extra,
});

type Handler = (url: string, init: RequestInit) => Response | Promise<Response>;
let handler: Handler;
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const bodies = () =>
  vi
    .mocked(fetch)
    .mock.calls.filter(([u]) => String(u).endsWith('/query/traces'))
    .map(([, init]) => JSON.parse(String((init as RequestInit).body)) as SearchRequest);

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(NOW);
  handler = (url, init) => {
    if (url.includes('/api/v1/services')) {
      return json(200, { data: [{ service_id: SVC, name: 'checkout', namespace: 'shop', environment: 'prod' }], next_cursor: null, meta });
    }
    if (url.endsWith('/query/traces')) {
      const b = JSON.parse(String(init.body)) as SearchRequest;
      return b.cursor === undefined
        ? json(200, {
            data: [row(1, { has_error: true }), row(2, { complete: false, reasons: ['missing_root'], root_name: null, root_service: null, root_service_id: null })],
            next_cursor: 'c1',
            meta,
          })
        : json(200, { data: [row(3)], next_cursor: null, meta });
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

describe('TraceExplorer (S04)', () => {
  it('결과: 오류 span 있음/없음을 구별하고, 불완전 trace는 사유, root가 없으면 그렇게 표시', async () => {
    renderAt('/o/acme/traces?tz=UTC&range=1h');
    const table = await screen.findByRole('region', { name: '결과' });
    const rows = await within(table).findAllByRole('row');
    expect(rows[1]?.textContent).toContain('오류 span 있음');
    expect(rows[1]?.textContent).toContain('checkout');
    expect(rows[2]?.textContent).toContain('오류 span 없음');
    expect(rows[2]?.textContent).toContain('root 없음');
    expect(rows[2]?.textContent).toContain('불완전 · root span 없음');
    expect(screen.getByText(/보존된\(샘플링된\) trace만/)).toBeTruthy();
  });

  it('URL 조건이 filter AST·범위로 서버에 간다', async () => {
    renderAt(`/o/acme/traces?tz=UTC&range=1h&service=${SVC}&errors=1&min_ms=500`);
    await screen.findByRole('region', { name: '결과' });
    const b = bodies()[0];
    expect(b?.range).toEqual({ from: '2026-10-04T05:00:00.000Z', to: '2026-10-04T06:00:00.000Z' });
    expect(b?.limit).toBe(100);
    expect(b?.filter).toEqual({
      op: 'and',
      args: [
        { op: 'eq', field: 'service_id', value: SVC },
        { op: 'eq', field: 'has_error', value: true },
        { op: 'gte', field: 'duration_ms', value: 500 },
      ],
    });
  });

  it('"다음 100개"는 cursor로 이어 붙인다', async () => {
    renderAt('/o/acme/traces?tz=UTC&range=1h');
    await userEvent.click(await screen.findByRole('button', { name: '다음 100개' }));
    const table = screen.getByRole('region', { name: '결과' });
    await vi.waitFor(() => expect(within(table).getAllByRole('row')).toHaveLength(4));
    expect(bodies()[1]?.cursor).toBe('c1');
    expect(screen.queryByRole('button', { name: '다음 100개' })).toBeNull();
  });

  it('trace ID는 상세(S05)로 trace 구간과 함께 연결', async () => {
    renderAt('/o/acme/traces?tz=UTC&range=1h');
    const link = await screen.findByRole('link', { name: /^11111111…/ });
    const href = new URL(link.getAttribute('href') ?? '', 'http://x');
    expect(href.pathname).toBe(`/o/acme/traces/${'1'.repeat(32)}`);
    expect(href.searchParams.get('from')).toBe('2026-10-04T05:58:00.000Z');
    expect(href.searchParams.has('range')).toBe(false);
  });

  it('오류만 체크하면 URL과 검색 조건이 바뀐다', async () => {
    const router = renderAt('/o/acme/traces?tz=UTC&range=1h');
    await userEvent.click(await screen.findByLabelText('오류 span이 있는 trace만'));
    expect(new URLSearchParams(router.state.location.search).get('errors')).toBe('1');
    await vi.waitFor(() => expect(bodies().at(-1)?.filter).toEqual({ op: 'eq', field: 'has_error', value: true }));
  });

  it('다음 page가 실패해도 불러온 결과와 마지막 성공 시각을 유지한다', async () => {
    const base = handler;
    handler = (url, init) =>
      url.endsWith('/query/traces') && (JSON.parse(String(init.body)) as SearchRequest).cursor !== undefined
        ? json(503, { error: { code: 'UNAVAILABLE', message: 'x', request_id: 'req-503', retryable: true } })
        : base(url, init);
    renderAt('/o/acme/traces?tz=UTC&range=1h');
    await userEvent.click(await screen.findByRole('button', { name: '다음 100개' }));
    expect(await screen.findByText(/마지막 성공 .* 기준 데이터를 보여주고 있습니다/)).toBeTruthy();
    expect(within(screen.getByRole('region', { name: '결과' })).getAllByRole('row')).toHaveLength(3);
  });

  it('24시간을 넘는 범위는 조회하지 않고 이유를 보인다', async () => {
    renderAt('/o/acme/traces?tz=UTC&range=7d');
    expect(await screen.findByText(/24시간까지 조회할 수 있습니다/)).toBeTruthy();
    expect(bodies()).toHaveLength(0);
  });

  it('결과가 없으면 조건 유무에 따라 다른 이유', async () => {
    handler = (url, init) =>
      url.endsWith('/query/traces') ? json(200, { data: [], next_cursor: null, meta }) : json(200, { data: [], next_cursor: null, meta });
    renderAt('/o/acme/traces?tz=UTC&range=1h&errors=1');
    expect(await screen.findByText('조건에 맞는 trace가 없습니다')).toBeTruthy();
  });

  it('잘못된 최소 duration은 입력 옆에 오류를 보이고 조건에 넣지 않는다', async () => {
    renderAt('/o/acme/traces?tz=UTC&range=1h');
    await userEvent.type(await screen.findByLabelText('최소 duration (ms)'), '12a');
    expect(screen.getByText(/0 이상 정수/)).toBeTruthy();
    expect(screen.getByLabelText('최소 duration (ms)').getAttribute('aria-invalid')).toBe('true');
  });
});
