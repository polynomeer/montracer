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
  root_service_version: null,
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
    const base = handler;
    handler = (url, init) => (url.endsWith('/query/traces') ? json(200, { data: [], next_cursor: null, meta }) : base(url, init));
    renderAt('/o/acme/traces?tz=UTC&range=1h&errors=1');
    expect(await screen.findByText('조건에 맞는 trace가 없습니다')).toBeTruthy();
  });

  it('잘못된 최소 duration은 입력 옆에 오류를 보이고 조건에 넣지 않는다', async () => {
    renderAt('/o/acme/traces?tz=UTC&range=1h');
    await userEvent.type(await screen.findByLabelText('최소 duration (ms)'), '12a');
    expect(screen.getByText(/0 이상 정수/)).toBeTruthy();
    expect(screen.getByLabelText('최소 duration (ms)').getAttribute('aria-invalid')).toBe('true');
  });

  describe('건수·facet·분포 (ADR 0074)', () => {
    const SVC2 = 'bbbbbbbb-0000-4000-8000-000000000002';
    const bucket = (i: number, count: number | null, err = 0, extra: Record<string, unknown> = {}) => ({
      start: new Date(NOW - 3_600_000 + i * 60_000).toISOString(),
      end: new Date(NOW - 3_600_000 + (i + 1) * 60_000).toISOString(),
      count,
      ...(count === null ? {} : { error_count: err }),
      incomplete: false,
      reason: count === null ? 'future' : null,
      ...extra,
    });
    const aggBody = (facets: unknown[]) => ({
      data: {
        total: 1834,
        histogram: { step_seconds: 60, buckets: [bucket(0, 12, 2), bucket(1, 0), bucket(2, 5, 0, { incomplete: true }), bucket(3, null)] },
        facets,
      },
      meta: { ...meta, sampled: true, resolution_seconds: 60 },
    });
    const facets = [
      { field: 'root_service_id', values: [{ value: SVC, count: 1200 }, { value: SVC2, count: 600 }], other_count: 0, missing_count: 34, reason: null, excludes_own_filter: false },
      { field: 'has_error', values: [{ value: 'false', count: 1700 }, { value: 'true', count: 134 }], other_count: 0, missing_count: 0, reason: null, excludes_own_filter: false },
    ];
    const aggBodies = () =>
      vi
        .mocked(fetch)
        .mock.calls.filter(([u]) => String(u).endsWith('/query/traces/aggregate'))
        .map(([, init]) => JSON.parse(String((init as RequestInit).body)) as { filter?: unknown; facets: unknown; histogram: boolean });
    const withAgg = (status: number, body: unknown) => {
      const base = handler;
      handler = (url, init) => (url.endsWith('/query/traces/aggregate') ? json(status, body) : base(url, init));
    };

    it('총 수·오류 수·root 서비스 건수를 보이고, 분포의 미래 칸은 0이 아니라 "아직 없음"이다', async () => {
      withAgg(200, aggBody(facets));
      renderAt('/o/acme/traces?tz=UTC&range=1h');
      const table = await screen.findByRole('region', { name: '결과' });
      await vi.waitFor(() => expect(within(table).getByText(/저장된 trace 1,834개 중 2개 이상 불러옴/)).toBeTruthy());
      const root = screen.getByRole('group', { name: 'Root 서비스' });
      expect(within(root).getByText('1,200')).toBeTruthy();
      expect(within(root).getByText('값 없음 34개')).toBeTruthy();
      expect(screen.getByText('134')).toBeTruthy();
      const hist = screen.getByRole('region', { name: '시작 시각 분포' });
      expect(within(hist).getByText(/오류 비율이 아니라 표본의 수/)).toBeTruthy();
      const cells = within(hist).getAllByRole('row').map((r) => r.textContent ?? '');
      expect(cells.some((c) => c.includes('아직 없음'))).toBe(true);
      expect(cells.some((c) => c.includes('5 (수집 중일 수 있음)'))).toBe(true);
      expect(hist.textContent).not.toMatch(/%/);
      // 같은 범위·조건, facet·분포 요청
      expect(aggBodies()[0]).toMatchObject({ facets: [{ field: 'root_service_id' }, { field: 'has_error' }, { field: 'root_service_version' }], histogram: true });
    });

    it('root 서비스를 여러 개 고르면 URL root와 요약 조건 in이 된다', async () => {
      withAgg(200, aggBody(facets));
      const router = renderAt('/o/acme/traces?tz=UTC&range=1h');
      const root = await screen.findByRole('group', { name: 'Root 서비스' });
      await userEvent.click(await within(root).findByLabelText('shop/checkout · prod'));
      await userEvent.click(within(root).getByLabelText(/목록에 없는 서비스 bbbbbbbb/));
      // 300ms 뒤 한 번에 반영(D05 §03 facet debounce): 두 번 눌러도 집계·검색은 한 번씩만 더 간다
      const aggBefore = aggBodies().length;
      await vi.waitFor(() => expect(new URLSearchParams(router.state.location.search).get('root')).toBe(`${SVC},${SVC2}`));
      await vi.waitFor(() => expect(bodies().at(-1)?.filter).toEqual({ op: 'in', field: 'root_service_id', value: [SVC, SVC2] }));
      await vi.waitFor(() => expect(aggBodies().length).toBe(aggBefore + 1));
      await new Promise((r) => setTimeout(r, 400));
      expect(aggBodies().length).toBe(aggBefore + 1);
      const bar = screen.getByRole('group', { name: '적용된 조건' });
      expect(within(bar).getByText(/root 서비스 = shop\/checkout · prod 또는 목록에 없는 서비스 bbbbbbbb/)).toBeTruthy();
    });

    it('Root 버전(ADR 0075): 줄마다 root 서비스, 같은 값 줄은 함께 체크, 고르면 URL rver와 요약 조건, 형식이 틀린 값은 막힘', async () => {
      const verFacet = {
        field: 'root_service_version',
        values: [
          { value: '1.2.0', count: 9, context: { service_id: SVC } },
          { value: '1.2.0', count: 4, context: { service_id: SVC2 } },
          { value: 'bad version', count: 1, context: { service_id: SVC } },
        ],
        other_count: 0,
        missing_count: 3,
        reason: null,
        excludes_own_filter: false,
      };
      withAgg(200, aggBody([...facets, verFacet]));
      const router = renderAt('/o/acme/traces?tz=UTC&range=1h');
      const ver = await screen.findByRole('group', { name: 'Root 버전' });
      await vi.waitFor(() => expect(within(ver).getByLabelText('1.2.0 · shop/checkout · prod')).toBeTruthy());
      expect(within(ver).getByText(/root span이 없는 trace도 든다/)).toBeTruthy();
      expect((within(ver).getByLabelText(/^bad version/) as HTMLInputElement).disabled).toBe(true);
      await userEvent.click(within(ver).getByLabelText('1.2.0 · shop/checkout · prod'));
      await vi.waitFor(() => expect(new URLSearchParams(router.state.location.search).get('rver')).toBe('1.2.0'));
      await vi.waitFor(() => expect(bodies().at(-1)?.filter).toEqual({ op: 'eq', field: 'root_service_version', value: '1.2.0' }));
      await vi.waitFor(() => expect((within(ver).getByLabelText(/^1\.2\.0 · 목록에 없는 서비스/) as HTMLInputElement).checked).toBe(true));
      expect(within(screen.getByRole('group', { name: '적용된 조건' })).getByText('root 버전 = 1.2.0')).toBeTruthy();
    });

    it('셀 수 없는 facet은 이유를 보이고, 고른 값은 "상위 밖"으로 남는다', async () => {
      withAgg(200, aggBody([{ ...facets[0], values: null, other_count: null, missing_count: null, reason: 'timeout' }, facets[1]]));
      renderAt(`/o/acme/traces?tz=UTC&range=1h&root=${SVC}`);
      const root = await screen.findByRole('group', { name: 'Root 서비스' });
      await vi.waitFor(() => expect(within(root).getByText('시간 안에 셀 수 없음')).toBeTruthy());
      // 셀 수 없으면 고른 값의 수 칸은 비운다("상위 밖"·0이 아니다)
      expect(within(root).queryByText('상위 밖')).toBeNull();
      expect(within(root).getByRole('checkbox', { checked: true })).toBeTruthy();
    });

    it('집계가 실패하면 목록은 그대로, 건수는 "볼 수 없음"이고 0으로 보이지 않는다', async () => {
      withAgg(422, { error: { code: 'QUERY_BUDGET_EXCEEDED', message: 'x', request_id: 'r', retryable: false } });
      renderAt('/o/acme/traces?tz=UTC&range=1h');
      const table = await screen.findByRole('region', { name: '결과' });
      await vi.waitFor(() => expect(within(table).getByText(/2개 이상 불러옴 · 저장된 trace 수를 볼 수 없음 · 범위를 줄이세요/)).toBeTruthy());
      expect(within(table).getAllByRole('row')).toHaveLength(3);
      expect(screen.getByText(/분포를 볼 수 없습니다/)).toBeTruthy();
      // 오류 trace 수도 0이 아니라 "볼 수 없음"
      expect(screen.getByLabelText('오류 span이 있는 trace만').parentElement?.textContent).toContain('볼 수 없음');
    });

    it('고른 값이 응답에 없으면: 그 밖이 있으면 "상위 밖", 없으면 0(저장된 행 없음)', async () => {
      const SVC3 = 'cccccccc-0000-4000-8000-000000000003';
      withAgg(200, aggBody([{ ...facets[0], other_count: 5 }, facets[1]]));
      renderAt(`/o/acme/traces?tz=UTC&range=1h&root=${SVC3}`);
      const root = await screen.findByRole('group', { name: 'Root 서비스' });
      await vi.waitFor(() => expect(within(root).getByText('상위 밖')).toBeTruthy());
    });

    it('새로고침이 실패하면 지난 건수를 지금 값처럼 보이지 않는다', async () => {
      withAgg(200, aggBody(facets));
      renderAt('/o/acme/traces?tz=UTC&range=1h');
      const table = await screen.findByRole('region', { name: '결과' });
      await vi.waitFor(() => expect(within(table).getByText(/저장된 trace 1,834개 중/)).toBeTruthy());
      withAgg(503, { error: { code: 'UNAVAILABLE', message: 'x', request_id: 'r', retryable: true } });
      await userEvent.click(screen.getByRole('button', { name: '새로고침' }));
      await vi.waitFor(() => expect(within(table).getByText(/저장된 trace 수를 볼 수 없음/)).toBeTruthy());
      expect(within(table).queryByText(/1,834/)).toBeNull();
      expect(screen.queryByText('1,200')).toBeNull();
    });

    it('집계가 꺼졌으면(404) 건수·분포 자리가 없다', async () => {
      renderAt('/o/acme/traces?tz=UTC&range=1h');
      const table = await screen.findByRole('region', { name: '결과' });
      await vi.waitFor(() => expect(aggBodies()).toHaveLength(1));
      await vi.waitFor(() => expect(screen.queryByRole('region', { name: '시작 시각 분포' })).toBeNull());
      expect(within(table).queryByText(/저장된 trace .*개 중/)).toBeNull();
      expect(screen.queryByRole('group', { name: 'Root 서비스' })).toBeNull();
    });
  });
});
