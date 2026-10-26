import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { SearchRequest, TraceSummary } from '../../api/types.ts';
import { routes } from '../../app/routes.tsx';
import { queryMeta, serviceFixture } from '../../test/fixtures';

const NOW = Date.parse('2026-10-04T06:00:00Z');
const SVC = 'aaaaaaaa-0000-4000-8000-000000000001';
const meta = queryMeta({ request_id: 'req-t' });

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
  last_updated_at: new Date(NOW - n * 60_000 + 5_000).toISOString(),
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
      return json(200, { data: [serviceFixture({ service_id: SVC, name: 'checkout', namespace: 'shop' })], next_cursor: null, meta });
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

  it('적용된 조건을 chip으로 보이고, 하나씩·모두 지운다(공유 URL과 검색 조건도 함께)', async () => {
    const router = renderAt(`/o/acme/traces?tz=UTC&range=1h&service=${SVC}&errors=1&min_ms=500`);
    const bar = await screen.findByRole('group', { name: '적용된 조건' });
    await vi.waitFor(() => expect(within(bar).getByText('서비스 = shop/checkout · prod')).toBeTruthy());
    expect(within(bar).getByText('오류 span 있음')).toBeTruthy();
    expect(within(bar).getByText('duration ≥ 500 ms')).toBeTruthy();
    await userEvent.click(within(bar).getByRole('button', { name: '조건 지우기: duration ≥ 500 ms' }));
    expect(new URLSearchParams(router.state.location.search).get('min_ms')).toBeNull();
    expect(new URLSearchParams(router.state.location.search).get('errors')).toBe('1');
    await userEvent.click(within(bar).getByRole('button', { name: '모두 지우기' }));
    const q = new URLSearchParams(router.state.location.search);
    expect(q.get('service')).toBeNull();
    expect(q.get('errors')).toBeNull();
    expect(q.get('range')).toBe('1h');
    expect(within(bar).getByText('조건 없음 · 이 범위의 보존된 trace 전체')).toBeTruthy();
    await vi.waitFor(() => expect(bodies().at(-1)?.filter).toBeUndefined());
  });

  it('name·min_ms chip을 지우면 왼쪽 입력도 비워진다(PS-0008)', async () => {
    renderAt('/o/acme/traces?tz=UTC&range=1h&name=foo&min_ms=500');
    const bar = await screen.findByRole('group', { name: '적용된 조건' });
    await userEvent.click(within(bar).getByRole('button', { name: '조건 지우기: span 이름 ⊃ “foo”' }));
    await userEvent.click(within(bar).getByRole('button', { name: '조건 지우기: duration ≥ 500 ms' }));
    expect((screen.getByLabelText('span 이름 포함') as HTMLInputElement).value).toBe('');
    expect((screen.getByLabelText('최소 duration (ms)') as HTMLInputElement).value).toBe('');
  });

  it('모두 지우기는 아직 입력 중인 조건도 버린다(뒤늦게 다시 생기지 않는다)', async () => {
    const router = renderAt('/o/acme/traces?tz=UTC&range=1h&errors=1&min_ms=500');
    const bar = await screen.findByRole('group', { name: '적용된 조건' });
    await userEvent.type(screen.getByLabelText('span 이름 포함'), 'foo');
    await userEvent.click(within(bar).getByRole('button', { name: '모두 지우기' }));
    expect((screen.getByLabelText('span 이름 포함') as HTMLInputElement).value).toBe('');
    await new Promise((r) => setTimeout(r, 400));
    expect(new URLSearchParams(router.state.location.search).get('name')).toBeNull();
  });

  it('목록에 없는 서비스는 chip도 그렇게 말한다', async () => {
    renderAt('/o/acme/traces?tz=UTC&range=1h&service=bbbbbbbb-0000-4000-8000-000000000009');
    const bar = await screen.findByRole('group', { name: '적용된 조건' });
    await vi.waitFor(() => expect(within(bar).getByText('서비스 = 목록에 없는 서비스 bbbbbbbb…')).toBeTruthy());
  });

  it('저장해 둔 촘촘히 보기로 시작하고, 저장소가 막혀도 동작한다', async () => {
    window.localStorage.setItem('montracer.traces.dense', '1');
    renderAt('/o/acme/traces?tz=UTC&range=1h');
    expect((await screen.findByRole('button', { name: '촘촘히 보기' })).getAttribute('aria-pressed')).toBe('true');
    window.localStorage.removeItem('montracer.traces.dense');
  });

  it('저장소를 쓸 수 없으면 기본 보기로 시작하고 눌러도 깨지지 않는다', async () => {
    const getItem = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    const setItem = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    renderAt('/o/acme/traces?tz=UTC&range=1h');
    const toggle = await screen.findByRole('button', { name: '촘촘히 보기' });
    expect(toggle.getAttribute('aria-pressed')).toBe('false');
    await userEvent.click(toggle);
    expect(toggle.getAttribute('aria-pressed')).toBe('true');
    getItem.mockRestore();
    setItem.mockRestore();
  });

  it('자정을 넘는 선택 범위는 날짜를 붙인다', async () => {
    renderAt('/o/acme/traces?tz=UTC&from=2026-10-03T23:30:00Z&to=2026-10-04T00:10:00Z');
    expect(await screen.findByText(/^선택 범위 10\. 3\. 23:30–10\. 4\. 00:10$/)).toBeTruthy();
  });

  it('촘촘히 보기는 표만 바꾸고 이 브라우저에 기억한다', async () => {
    renderAt('/o/acme/traces?tz=UTC&range=1h');
    const toggle = await screen.findByRole('button', { name: '촘촘히 보기' });
    expect(toggle.getAttribute('aria-pressed')).toBe('false');
    await userEvent.click(toggle);
    expect(toggle.getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByRole('table').className).toContain('mt-table--dense');
    expect(window.localStorage.getItem('montracer.traces.dense')).toBe('1');
    window.localStorage.removeItem('montracer.traces.dense');
  });

  it('절대 범위면 선택 범위를 시각으로 보인다', async () => {
    renderAt('/o/acme/traces?tz=UTC&from=2026-10-04T05:10:00Z&to=2026-10-04T05:40:00Z');
    expect(await screen.findByText('선택 범위 05:10–05:40')).toBeTruthy();
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
