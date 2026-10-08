import { act, fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { LogItem, SearchRequest } from '../../api/types.ts';
import { routes } from '../../app/routes.tsx';

const NOW = Date.parse('2026-10-04T06:00:00Z');
const SVC = 'aaaaaaaa-0000-4000-8000-000000000001';
const UNKNOWN_SVC = 'bbbbbbbb-0000-4000-8000-000000000002';
const TRACE = 'ab'.repeat(16);
const meta = { request_id: 'req-l', schema_version: 1, partial: false, failed_shards: [], warnings: [] };

const log = (n: number, extra: Partial<LogItem> = {}): LogItem => ({
  time: new Date(NOW - n * 1000).toISOString(),
  event_id: `gen:${String(n).padStart(32, '0')}`,
  service_id: SVC,
  severity_number: 9,
  trace_id: null,
  span_id: null,
  body: `line ${String(n)}`,
  attributes: {},
  ...extra,
});

type Handler = (url: string, init: RequestInit) => Response | Promise<Response>;
let handler: Handler;
let pages: { data: LogItem[]; next_cursor: string | null; meta?: typeof meta }[];
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const bodies = () =>
  vi
    .mocked(fetch)
    .mock.calls.filter(([u]) => String(u).endsWith('/query/logs'))
    .map(([, init]) => JSON.parse(String((init as RequestInit).body)) as SearchRequest);

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(NOW);
  pages = [
    {
      data: [
        log(1, { severity_number: 17, trace_id: TRACE, span_id: '1234567890abcdef', body: 'payment failed', attributes: { 'http.route': '/pay' } }),
        log(2, { service_id: UNKNOWN_SVC, severity_number: 0, body: '<img src=x onerror="window.__pwned=1">' }),
      ],
      next_cursor: 'c1',
    },
    { data: [log(3)], next_cursor: null },
  ];
  handler = (url, init) => {
    if (url.includes('/api/v1/services')) {
      return json(200, { data: [{ service_id: SVC, name: 'checkout', namespace: 'shop', environment: 'prod' }], next_cursor: null, meta });
    }
    if (url.endsWith('/query/logs')) {
      const b = JSON.parse(String(init.body)) as SearchRequest;
      const p = b.cursor === undefined ? pages[0] : pages[1];
      return json(200, { data: p?.data ?? [], next_cursor: p?.next_cursor ?? null, meta: p?.meta ?? meta });
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

const resultRows = async () => {
  const table = await screen.findByRole('table', { name: 'log 결과' });
  return within(table).getAllByRole('row').slice(1);
};

describe('LogExplorer (S08)', () => {
  it('URL 조건 → 요청 본문, 행에 시각·심각도·서비스·본문, 모르는 서비스는 ID', async () => {
    renderAt(`/o/acme/logs?tz=UTC&range=1h&service=${SVC}&sev=error&trace=${TRACE}&q=fail`);
    const rows = await resultRows();
    expect(bodies()[0]).toEqual({
      range: { from: '2026-10-04T05:00:00.000Z', to: '2026-10-04T06:00:00.000Z' },
      limit: 100,
      filter: {
        op: 'and',
        args: [
          { op: 'eq', field: 'service_id', value: SVC },
          { op: 'gte', field: 'severity_number', value: 17 },
          { op: 'eq', field: 'trace_id', value: TRACE },
          { op: 'contains', field: 'body', value: 'fail' },
        ],
      },
    });
    expect(rows[0]?.textContent).toContain('ERROR');
    expect(rows[0]?.textContent).toContain('shop/checkout');
    expect(rows[0]?.textContent).toContain('payment failed');
    expect(rows[1]?.textContent).toContain('미지정');
    expect(rows[1]?.textContent).toContain('bbbbbbbb…');
    expect(screen.getByText(/2개 이상/)).toBeTruthy();
    // 다음 page가 있으면 전체 행 수를 모른다
    expect(screen.getByRole('table', { name: 'log 결과' }).getAttribute('aria-rowcount')).toBe('-1');
    // catalog에 없는 서비스: 화면은 줄이고 accessible name은 전체 ID
    expect(rows[1]?.textContent).toContain(`catalog에 없는 서비스 ${UNKNOWN_SVC}`);
  });

  it('URL의 서비스가 선택지에 없어도 실제 조건을 숨기지 않는다', async () => {
    renderAt(`/o/acme/logs?tz=UTC&range=1h&service=${UNKNOWN_SVC}`);
    await resultRows();
    const select = screen.getByLabelText<HTMLSelectElement>('서비스');
    expect(select.value).toBe(UNKNOWN_SVC);
    expect(select.selectedOptions[0]?.textContent).toBe('목록에 없는 서비스 bbbbbbbb…');
  });

  it('본문의 HTML은 실행하지 않고 텍스트로', async () => {
    renderAt('/o/acme/logs?tz=UTC&range=1h');
    await resultRows();
    expect(screen.getByText('<img src=x onerror="window.__pwned=1">')).toBeTruthy();
    expect(document.querySelector('img')).toBeNull();
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined();
  });

  it('trace 링크는 log 시각 기준 범위와 그 span의 로그 탭으로', async () => {
    renderAt('/o/acme/logs?tz=UTC&range=1h');
    await resultRows();
    const href = screen.getByRole('link', { name: `trace ${TRACE} 상세` }).getAttribute('href') ?? '';
    const q = new URLSearchParams(href.split('?')[1]);
    expect(href.startsWith(`/o/acme/traces/${TRACE}?`)).toBe(true);
    expect(q.get('from')).toBe('2026-10-04T04:59:59.000Z');
    expect(q.get('to')).toBe('2026-10-04T06:04:00.000Z'); // log +10분이 미래라 지금 + 4분으로 자름
    expect(q.get('entity')).toBe('1234567890abcdef');
    expect(q.get('tab')).toBe('logs');
  });

  it('행을 열면 속성 패널과 URL entity, Esc로 닫고 그 행으로 focus', async () => {
    const user = userEvent.setup();
    const router = renderAt('/o/acme/logs?tz=UTC&range=1h');
    await resultRows();
    const open = screen.getByRole('button', { name: /payment failed/ });
    await user.click(open);
    const drawer = screen.getByRole('complementary', { name: 'log 상세' });
    expect(within(drawer).getByText('/pay')).toBeTruthy();
    expect(within(drawer).getByText('ERROR (17)')).toBeTruthy();
    expect(new URLSearchParams(router.state.location.search).get('entity')).toBe(log(1).event_id);
    expect(screen.getByRole('button', { name: /payment failed/ }).getAttribute('aria-current')).toBe('true');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('complementary', { name: 'log 상세' })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: /payment failed/ }));
  });

  it('공유 링크의 entity가 불러온 결과에 없으면 추측하지 않는다', async () => {
    renderAt('/o/acme/logs?tz=UTC&range=1h&entity=gen:nope');
    await resultRows();
    expect(screen.getByText(/불러온 결과에 이 log가 없습니다/)).toBeTruthy();
  });

  it('다음 page는 cursor로 이어 붙인다', async () => {
    const user = userEvent.setup();
    renderAt('/o/acme/logs?tz=UTC&range=1h');
    await resultRows();
    await user.click(screen.getByRole('button', { name: '다음 100개' }));
    await screen.findByText('line 3');
    expect(bodies()[1]?.cursor).toBe('c1');
    expect(await resultRows()).toHaveLength(3);
    expect(screen.queryByRole('button', { name: '다음 100개' })).toBeNull();
  });

  it('빈 결과: 조건 유무를 구분, partial이면 "없음"이라 하지 않는다', async () => {
    pages = [{ data: [], next_cursor: null }];
    renderAt('/o/acme/logs?tz=UTC&range=1h&sev=error');
    expect(await screen.findByText('조건에 맞는 log가 없습니다')).toBeTruthy();
  });

  it('partial 응답은 경고하고 빈 결과 문구를 쓰지 않는다', async () => {
    pages = [{ data: [], next_cursor: null, meta: { ...meta, partial: true } }];
    renderAt('/o/acme/logs?tz=UTC&range=1h');
    expect(await screen.findByText(/일부 저장소가 응답하지 않아 빠진 log/)).toBeTruthy();
    expect(screen.queryByText('이 범위에 log가 없습니다')).toBeNull();
  });

  it('환경을 골라도 검색 조건에 넣지 않으며 그렇다고 알린다', async () => {
    renderAt('/o/acme/logs?tz=UTC&range=1h&env=prod');
    await resultRows();
    expect(screen.getByText(/환경 선택은 log 검색에 아직 적용되지 않습니다/)).toBeTruthy();
    expect(bodies()[0]?.filter).toBeUndefined();
  });

  it('24시간을 넘는 범위는 조회하지 않고 안내', async () => {
    renderAt('/o/acme/logs?tz=UTC&range=7d');
    expect(await screen.findByText(/24시간까지 조회할 수 있습니다/)).toBeTruthy();
    expect(bodies()).toHaveLength(0);
  });

  it('조건 입력: 잘못된 trace ID는 반영하지 않고, 조회 버튼은 바로 반영', async () => {
    const user = userEvent.setup();
    const router = renderAt('/o/acme/logs?tz=UTC&range=1h');
    await resultRows();
    await user.type(screen.getByLabelText('Trace ID'), 'xyz');
    expect(screen.getByText('16진수 32자리를 입력하세요.')).toBeTruthy();
    await user.clear(screen.getByLabelText('Trace ID'));
    await user.type(screen.getByLabelText('본문 포함 (대소문자 무시)'), 'timeout');
    await user.click(screen.getByRole('button', { name: '조회' }));
    expect(new URLSearchParams(router.state.location.search).get('q')).toBe('timeout');
    await user.selectOptions(screen.getByLabelText('최소 심각도'), 'warn');
    expect(new URLSearchParams(router.state.location.search).get('sev')).toBe('warn');
    expect(screen.getByText('심각도가 미지정인 log는 빠집니다.')).toBeTruthy();
  });

  it('가상화: 3,000행 중 일부만 그리고, 선택 행은 화면 밖이어도 그린다', async () => {
    const many = Array.from({ length: 3000 }, (_, i) => log(i + 1));
    pages = [{ data: many, next_cursor: null }];
    const target = many[2500];
    renderAt(`/o/acme/logs?tz=UTC&range=1h&entity=${encodeURIComponent(target?.event_id ?? '')}`);
    const rows = await resultRows();
    expect(rows.length).toBeLessThan(60);
    const table = screen.getByRole('table', { name: 'log 결과' });
    expect(table.getAttribute('aria-rowcount')).toBe('3001');
    expect(rows.some((r) => r.getAttribute('aria-rowindex') === '2502')).toBe(true);
    // 스크롤하면 보이는 행이 바뀐다. 먼저 선택 행으로의 첫 스크롤(effect)이 끝나야 한다 —
    // 늦게 돈 effect가 아래 스크롤을 덮으면 CI처럼 느린 환경에서만 실패한다.
    const scroller = table.parentElement as HTMLElement;
    await vi.waitFor(() => expect(scroller.scrollTop).toBe((2500 - 3) * 32));
    act(() => {
      scroller.scrollTop = 1000 * 32;
      fireEvent.scroll(scroller);
    });
    expect(within(table).getAllByRole('row').some((r) => r.getAttribute('aria-rowindex') === '1002')).toBe(true);
  });
});
