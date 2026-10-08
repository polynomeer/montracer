import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { LogItem, SearchRequest, TraceDetail, TraceSpanItem } from '../../api/types.ts';
import { routes } from '../../app/routes.tsx';

const TRACE = '4bf92f3577b34da6a3ce929d0e0e4736';
const T0 = Date.parse('2026-10-04T05:00:00Z');
const meta = { request_id: 'req-d', schema_version: 1, partial: false, failed_shards: [], warnings: [] };

function span(id: string, parent: string | null, startMs: number, durMs: number, extra: Partial<TraceSpanItem> = {}): TraceSpanItem {
  return {
    trace_id: TRACE,
    span_id: id,
    parent_span_id: parent,
    service_id: 'aaaaaaaa-0000-4000-8000-000000000001',
    service_name: 'checkout',
    environment: 'prod',
    name: `op-${id}`,
    kind: 'server',
    status_code: 'unset',
    status_message: '',
    start_time: new Date(T0 + startMs).toISOString(),
    end_time: new Date(T0 + startMs + durMs).toISOString(),
    duration_ns: durMs * 1e6,
    attributes: {},
    resource_attributes: { 'service.name': 'checkout' },
    events: [],
    links: [],
    ...extra,
  };
}

const sample = (): TraceDetail => ({
  trace_id: TRACE,
  span_count: 4,
  complete: false,
  reasons: ['missing_parent'],
  last_updated_at: new Date(T0 + 5000).toISOString(),
  spans: [
    span('0000000000000001', null, 0, 840, { name: 'POST /checkout' }),
    span('0000000000000002', '0000000000000001', 60, 500, {
      name: 'charge',
      service_name: 'payment',
      service_id: 'aaaaaaaa-0000-4000-8000-000000000002',
      status_code: 'error',
      status_message: 'card declined',
      attributes: { 'http.route': '/charge', 'user.note': '<img src=x onerror=alert(1)>' },
      events: [{ name: 'exception', time: new Date(T0 + 70).toISOString(), attributes: { 'exception.type': 'Declined' } }],
    }),
    span('0000000000000003', '0000000000000002', 70, 600, {
      name: 'SELECT orders',
      service_name: 'database',
      kind: 'client',
      attributes: { 'db.system.name': 'postgresql', 'db.query.text': 'SELECT * FROM orders WHERE id = ?' },
    }), // 부모(60~560) 밖으로 끝남 → skew
    span('00000000000000aa', '00000000000000ff', 100, 10, { name: 'orphan-op' }),
  ],
});

type Handler = (url: string, init: RequestInit) => Response | Promise<Response>;
let handler: Handler;
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const log = (id: string, extra: Partial<LogItem> = {}): LogItem => ({
  time: new Date(T0 + 65).toISOString(),
  event_id: id,
  service_id: 'aaaaaaaa-0000-4000-8000-000000000002',
  severity_number: 17,
  trace_id: TRACE,
  span_id: '0000000000000002',
  body: 'charge failed: card declined',
  attributes: {},
  ...extra,
});

beforeEach(() => {
  handler = (url, init) => {
    if (url.includes(`/api/v1/traces/${TRACE}`)) return json(200, { data: sample(), meta });
    if (url.endsWith('/query/logs')) {
      const b = JSON.parse(String(init.body)) as SearchRequest;
      return JSON.stringify(b.filter).includes('span_id')
        ? json(200, { data: [log('uid:1')], next_cursor: null, meta })
        : json(200, { data: [log('uid:1'), log('uid:2', { trace_id: null, span_id: null, severity_number: 9, body: 'retrying' })], next_cursor: null, meta });
    }
    return json(404, { error: { code: 'NOT_FOUND', message: 'x', request_id: 'req-404', retryable: false } });
  };
  vi.stubGlobal('fetch', vi.fn((url: string, init: RequestInit) => Promise.resolve(handler(url, init))));
});

afterEach(() => vi.unstubAllGlobals());

const PATH = `/o/acme/traces/${TRACE}?tz=UTC&from=2026-10-04T04:59:00.000Z&to=2026-10-04T05:01:00.000Z`;

function renderAt(path: string) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return router;
}

const rowNames = () => screen.getAllByRole('treeitem').map((r) => r.getAttribute('aria-label') ?? r.textContent);

describe('TraceDetail (S05)', () => {
  it('URL 범위로 단건 조회, 헤더에 root·불완전 사유·오류 수·clock skew', async () => {
    renderAt(PATH);
    expect(await screen.findByRole('heading', { level: 1, name: 'checkout · POST /checkout' })).toBeTruthy();
    const call = vi.mocked(fetch).mock.calls.find(([u]) => String(u).includes('/traces/'));
    expect(String(call?.[0])).toContain('from=2026-10-04T04%3A59%3A00.000Z');
    expect(screen.getByText('불완전 · 부모 span 누락')).toBeTruthy();
    expect(screen.getByText('오류 span 1개')).toBeTruthy();
    expect(screen.getByText(/clock skew 의심 1개/)).toBeTruthy();
  });

  it('waterfall: 트리 순서, orphan 그룹, 오류만·critical path 보기', async () => {
    renderAt(PATH);
    await screen.findByRole('tree');
    expect(rowNames()).toEqual([
      'checkout POST /checkout, 840 ms',
      'payment charge, 500 ms, 오류',
      'database SELECT orders, 600 ms, clock skew 의심',
      expect.stringContaining('부모 span이 없는 span 1개') as unknown as string,
      'checkout orphan-op, 10 ms',
    ]);
    await userEvent.click(screen.getByRole('button', { name: '오류만 (1)' }));
    expect(rowNames()).toEqual(['checkout POST /checkout, 840 ms', 'payment charge, 500 ms, 오류']);
    await userEvent.click(screen.getByRole('button', { name: 'Critical path (추정)' }));
    expect(screen.getAllByRole('treeitem').filter((r) => r.className.includes('critical'))).toHaveLength(3);
  });

  it('키보드: ↓ 이동, ← 접기, Enter 상세, Esc 닫기 — 선택은 URL에 남는다', async () => {
    const router = renderAt(PATH);
    const tree = await screen.findByRole('tree');
    tree.focus();
    await userEvent.keyboard('{ArrowDown}{ArrowDown}');
    expect(tree.getAttribute('aria-activedescendant')).toBe('row-0000000000000002');
    expect(screen.queryByRole('complementary', { name: 'charge' })).toBeNull(); // 화살표만으로는 열지 않는다
    await userEvent.keyboard('{ArrowLeft}');
    expect(rowNames()).not.toContain('database SELECT orders, 600 ms, clock skew 의심');
    await userEvent.keyboard('{Enter}');
    expect(await screen.findByRole('complementary', { name: 'charge' })).toBeTruthy();
    expect(new URLSearchParams(router.state.location.search).get('entity')).toBe('0000000000000002');
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('complementary', { name: 'charge' })).toBeNull();
    expect(new URLSearchParams(router.state.location.search).has('entity')).toBe(false);
  });

  it('상세: 속성은 텍스트로만(HTML 실행 없음), 이벤트, DB는 SQL 텍스트', async () => {
    renderAt(`${PATH}&entity=0000000000000002`);
    const drawer = await screen.findByRole('complementary', { name: 'charge' });
    expect(within(drawer).getByText('<img src=x onerror=alert(1)>')).toBeTruthy();
    expect(drawer.querySelector('img')).toBeNull();
    expect(within(drawer).getByText(/card declined/)).toBeTruthy();
    await userEvent.click(within(drawer).getByRole('button', { name: '이벤트 1' }));
    expect(within(drawer).getByText('exception')).toBeTruthy();
    await userEvent.click(within(drawer).getByRole('button', { name: 'DB' }));
    expect(within(drawer).getByText(/db\.system\)이 없는 span/)).toBeTruthy();
  });

  it('로그: trace·span ID가 같은 것은 linked, 같은 서비스·시간대 나머지는 related', async () => {
    renderAt(`${PATH}&entity=0000000000000002&tab=logs`);
    const drawer = await screen.findByRole('complementary', { name: 'charge' });
    const lists = await within(drawer).findAllByRole('list');
    await vi.waitFor(() => expect(within(drawer).getByText('retrying')).toBeTruthy());
    expect(lists[0]?.textContent).toContain('charge failed');
    expect(within(drawer).getAllByText('charge failed: card declined')).toHaveLength(1); // linked가 related에 중복되지 않는다
    const bodies = vi
      .mocked(fetch)
      .mock.calls.filter(([u]) => String(u).endsWith('/query/logs'))
      .map(([, init]) => JSON.parse(String((init as RequestInit).body)) as SearchRequest);
    expect(JSON.stringify(bodies[0]?.filter)).toContain('"field":"span_id","value":"0000000000000002"');
    expect(bodies[0]?.range).toEqual({ from: '2026-10-04T04:59:00.000Z', to: '2026-10-04T05:01:00.000Z' }); // linked: trace 조회 범위
    expect(bodies[1]?.filter).toEqual({ op: 'eq', field: 'service_id', value: 'aaaaaaaa-0000-4000-8000-000000000002' });
    // related: span 시작 ±30초(D02 §07)
    expect(bodies[1]?.range).toEqual({ from: new Date(T0 + 60 - 30_000).toISOString(), to: new Date(T0 + 60 + 30_000 + 1).toISOString() });
  });

  it('log가 잘렸으면 "없음"이라 하지 않고 더 있다고 알린다, 실패는 다시 시도', async () => {
    let calls = 0;
    handler = (url, init) => {
      if (url.includes('/traces/')) return json(200, { data: sample(), meta });
      calls++;
      const linkedReq = JSON.stringify((JSON.parse(String(init.body)) as SearchRequest).filter).includes('span_id');
      if (linkedReq && calls <= 2) return json(503, { error: { code: 'UNAVAILABLE', message: 'x', request_id: 'req-503', retryable: true } });
      // related: linked와 같은 log만 50개 받고 다음 page가 있다 → linked를 빼면 0행이지만 "없음"이 아니다
      return json(200, { data: [log('uid:1')], next_cursor: linkedReq ? null : 'c2', meta });
    };
    renderAt(`${PATH}&entity=0000000000000002&tab=logs`);
    const drawer = await screen.findByRole('complementary', { name: 'charge' });
    expect(await within(drawer).findByText(/이 구간에 log가 더 있습니다/)).toBeTruthy();
    expect(within(drawer).queryByText('이 구간에 다른 log가 없습니다.')).toBeNull();
    await userEvent.click(await within(drawer).findByRole('button', { name: '다시 시도' }));
    expect(await within(drawer).findByText('charge failed: card declined')).toBeTruthy();
  });

  it('긴 span은 related 범위를 끝 30초 뒤까지 넓힐 수 있다', async () => {
    handler = (url, init) => {
      if (url.includes('/traces/')) {
        const d = sample();
        d.spans[1] = { ...d.spans[1]!, end_time: new Date(T0 + 60 + 90_000).toISOString(), duration_ns: 90_000 * 1e6 };
        return json(200, { data: d, meta });
      }
      void init;
      return json(200, { data: [], next_cursor: null, meta });
    };
    renderAt(`${PATH}&entity=0000000000000002&tab=logs`);
    const drawer = await screen.findByRole('complementary', { name: 'charge' });
    await userEvent.click(await within(drawer).findByRole('button', { name: /범위 넓히기/ }));
    await vi.waitFor(() => {
      const last = vi.mocked(fetch).mock.calls.filter(([u]) => String(u).endsWith('/query/logs')).at(-1);
      const b = JSON.parse(String((last?.[1] as RequestInit).body)) as SearchRequest;
      expect(b.range.to).toBe(new Date(T0 + 60 + 90_000 + 30_000 + 1).toISOString());
    });
  });

  it('없는 trace는 이유를 추측하지 않고 request ID와 범위 넓히기 안내', async () => {
    handler = () => json(404, { error: { code: 'NOT_FOUND', message: 'x', request_id: 'req-404', retryable: false } });
    renderAt(PATH);
    expect(await screen.findByRole('heading', { name: '이 범위에서 trace를 찾을 수 없습니다' })).toBeTruthy();
    expect(screen.getByText(/이유는 확인할 수 없습니다/)).toBeTruthy();
    expect(screen.getByText('request_id req-404')).toBeTruthy();
  });

  it('형식이 틀린 trace ID는 조회하지 않는다', async () => {
    renderAt('/o/acme/traces/not-a-trace?tz=UTC');
    expect(await screen.findByText('trace ID 형식이 아닙니다')).toBeTruthy();
    expect(vi.mocked(fetch).mock.calls.filter(([u]) => String(u).includes('/traces/'))).toHaveLength(0);
  });

  it('span이 많으면 보이는 행만 그린다(가상화)', async () => {
    const many: TraceSpanItem[] = [span('0000000000000001', null, 0, 5000)];
    for (let i = 2; i <= 3000; i++) many.push(span(i.toString(16).padStart(16, '0'), '0000000000000001', i, 1));
    handler = (url) => (url.includes('/traces/') ? json(200, { data: { ...sample(), spans: many, span_count: many.length, complete: true, reasons: [] }, meta }) : json(404, {}));
    renderAt(PATH);
    await screen.findByRole('tree');
    expect(screen.getAllByRole('treeitem').length).toBeLessThan(60);
    expect(screen.getByText(/보이는 행 3,000개/)).toBeTruthy();
  });
});
