import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { MapEdge, MapNode, ServiceMapRequest } from '../../api/serviceMap.ts';
import { routes } from '../../app/routes.tsx';
import { queryMeta } from '../../test/fixtures';

const NOW = Date.parse('2026-10-04T06:00:00Z');
const CHECKOUT = 'aaaaaaaa-0000-4000-8000-000000000001';
const PAYMENT = 'aaaaaaaa-0000-4000-8000-000000000002';
const LEDGER = 'aaaaaaaa-0000-4000-8000-000000000003';

const svc = (id: string, name: string): MapNode => ({ node_id: id, kind: 'service', name, service_id: id, namespace: 'shop', environment: 'prod' });
const latency = { p50_ms: 12, p95_ms: 48, p99_ms: 90 };
const observed = (source: string, destination: string, requests: number, errors: number, over: Partial<MapEdge> = {}): MapEdge => ({
  source,
  destination,
  connection: 'rpc',
  confidence: 'observed',
  requests,
  errors,
  latency,
  queue_wait: null,
  last_seen: '2026-10-04T05:59:00Z',
  ...over,
});

const baseNodes: MapNode[] = [
  svc(CHECKOUT, 'checkout'),
  svc(PAYMENT, 'payment'),
  svc(LEDGER, 'ledger'),
  { node_id: 'db:postgresql/orders@prod', kind: 'database', name: 'postgresql/orders', service_id: null, namespace: null, environment: 'prod' },
];
const baseEdges: MapEdge[] = [
  observed(CHECKOUT, PAYMENT, 3600, 36),
  observed(PAYMENT, 'db:postgresql/orders@prod', 7200, 0),
  {
    source: CHECKOUT,
    destination: LEDGER,
    connection: 'rpc',
    confidence: 'estimated',
    requests: null,
    errors: null,
    latency: null,
    queue_wait: null,
    last_seen: '2026-10-04T05:58:00Z',
  },
];

let response: () => Response;
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const mapBody = (nodes: MapNode[], edges: MapEdge[], over: { warnings?: string[]; omitted?: { nodes: number; edges: number }; watermark?: string | null } = {}) => ({
  data: { nodes, edges, omitted_count: over.omitted ?? { nodes: 0, edges: 0 }, range: { from: '2026-10-04T05:00:00Z', to: '2026-10-04T06:00:00Z' } },
  meta: queryMeta({ sampled: true, resolution_seconds: 60, watermark: over.watermark === undefined ? '2026-10-04T06:00:00Z' : over.watermark, warnings: over.warnings ?? [] }),
});
const requests = () =>
  vi
    .mocked(fetch)
    .mock.calls.filter(([u]) => String(u).endsWith('/service-map/query'))
    .map(([, init]) => JSON.parse(String((init as RequestInit).body)) as ServiceMapRequest);

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(NOW);
  response = () => json(200, mapBody(baseNodes, baseEdges));
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => {
      if (url.endsWith('/service-map/query')) return Promise.resolve(response());
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

const node = (name: RegExp) => screen.findByRole('button', { name });

describe('Service Map (S03)', () => {
  it('조사 context(환경·범위)로 한 번 조회하고 node를 종류와 함께 그린다', async () => {
    renderAt('/o/acme/map?tz=UTC&range=1h&env=prod');
    expect(await node(/^shop\/checkout, 서비스/)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'postgresql/orders, DB' })).toBeTruthy();
    expect(requests()).toEqual([{ range: { from: '2026-10-04T05:00:00.000Z', to: '2026-10-04T06:00:00.000Z' }, environment: 'prod' }]);
    expect(screen.getByText('표본 trace 기반 · 1분 집계')).toBeTruthy();
    expect(screen.getByText(/굵기 = 요청량\(이 화면의 최대값 기준 정규화\)/)).toBeTruthy();
    // 오류가 있는 호출은 오류 색, 추정은 점선(수 없음)
    const svg = screen.getByRole('group', { name: /서비스 지도/ });
    expect(svg.querySelector(`[data-edge="${CHECKOUT}|${PAYMENT}|rpc|observed"]`)?.getAttribute('class')).toContain('mt-map-edge--error');
    expect(svg.querySelector(`[data-edge="${PAYMENT}|db:postgresql/orders@prod|rpc|observed"]`)?.getAttribute('class')).not.toContain('error');
    expect(svg.querySelector(`[data-edge="${CHECKOUT}|${LEDGER}|rpc|estimated"]`)?.getAttribute('class')).toContain('mt-map-edge--estimated');
  });

  it('node를 고르면 1hop을 강조하고 들어오고 나가는 호출을 보인다. 서비스 선택은 URL에 남는다', async () => {
    const router = renderAt('/o/acme/map?tz=UTC&range=1h');
    await userEvent.click(await node(/^shop\/payment, 서비스/));
    const drawer = await screen.findByRole('complementary', { name: 'shop/payment' });
    expect(within(drawer).getByRole('region', { name: '들어오는 호출' }).textContent).toContain('shop/checkout');
    expect(within(drawer).getByRole('region', { name: '들어오는 호출' }).textContent).toContain('1/s'); // 3,600 / 1시간
    expect(within(drawer).getByRole('region', { name: '들어오는 호출' }).textContent).toContain('1%');
    expect(within(drawer).getByRole('region', { name: '나가는 호출' }).textContent).toContain('postgresql/orders');
    expect(router.state.location.search).toContain(`service=${PAYMENT}`);
    // 1hop 밖(ledger)은 흐리게
    expect(screen.getByRole('button', { name: /^shop\/ledger/ }).getAttribute('class')).toContain('mt-map-dim');
    expect(screen.getByRole('button', { name: /^shop\/checkout/ }).getAttribute('class')).not.toContain('mt-map-dim');
    expect(within(drawer).getByRole('link', { name: '서비스 상세' }).getAttribute('href')).toContain(`/o/acme/services/${PAYMENT}`);
    // Esc로 닫는다
    await userEvent.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('complementary')).toBeNull());
    expect(router.state.location.search).not.toContain('service=');
  });

  it('공유 링크의 서비스 선택을 연다', async () => {
    renderAt(`/o/acme/map?tz=UTC&range=1h&service=${CHECKOUT}`);
    const drawer = await screen.findByRole('complementary', { name: 'shop/checkout' });
    // 추정 호출은 수를 붙이지 않는다
    expect(within(drawer).getByRole('region', { name: '나가는 호출' }).textContent).toContain('추정 — 수 없음');
  });

  it('키보드: →는 불리는 쪽, ←는 호출하는 쪽으로 focus를 옮기고 Enter로 고른다', async () => {
    renderAt('/o/acme/map?tz=UTC&range=1h');
    const checkout = await node(/^shop\/checkout, 서비스/);
    checkout.focus();
    await userEvent.keyboard('{ArrowRight}');
    expect(document.activeElement?.getAttribute('aria-label')).toMatch(/^shop\/(payment|ledger)/);
    await userEvent.keyboard('{ArrowLeft}');
    expect(document.activeElement?.getAttribute('aria-label')).toMatch(/^shop\/checkout/);
    await userEvent.keyboard('{Enter}');
    expect(await screen.findByRole('complementary', { name: 'shop/checkout' })).toBeTruthy();
  });

  it('검색은 맞는 node를 강조하고 Enter로 고른다(검색어는 URL에 넣지 않는다)', async () => {
    const router = renderAt('/o/acme/map?tz=UTC&range=1h');
    await node(/^shop\/checkout/);
    await userEvent.type(screen.getByPlaceholderText('서비스·DB 이름'), 'postgres{Enter}');
    expect(screen.getByText('1개 맞음')).toBeTruthy();
    expect(await screen.findByRole('complementary', { name: 'postgresql/orders' })).toBeTruthy();
    expect(router.state.location.search).not.toContain('postgres');
  });

  it('표 보기: 같은 호출을 요청 많은 순으로, 추정은 수 없이', async () => {
    renderAt('/o/acme/map?tz=UTC&range=1h&tab=table');
    const table = await screen.findByRole('table', { name: '서비스 간 호출' });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(3);
    expect(rows[0]?.textContent).toContain('postgresql/orders');
    expect(rows[0]?.textContent).toContain('2/s');
    expect(rows[1]?.textContent).toContain('1%');
    expect(rows[1]?.textContent).toContain('48 ms');
    expect(rows[2]?.textContent).toContain('추정 — 수 없음');
    expect(rows[2]?.textContent).toContain('추정');
  });

  it('미계산 구간·환경 밖 호출·서버 상한을 0이나 정상으로 숨기지 않고 알린다', async () => {
    response = () =>
      json(
        200,
        mapBody(baseNodes, baseEdges, {
          warnings: ['cross_environment_edges_hidden', 'range_after_watermark'],
          omitted: { nodes: 3, edges: 7 },
          watermark: '2026-10-04T05:57:00Z',
        }),
      );
    renderAt('/o/acme/map?tz=UTC&range=1h');
    await node(/^shop\/checkout/);
    const notes = screen.getAllByRole('status').map((n) => n.textContent ?? '');
    expect(notes.some((t) => t.includes('05:57') && t.includes('미계산'))).toBe(true);
    expect(notes.some((t) => t.includes('서로 다른 환경'))).toBe(true);
    expect(notes.some((t) => t.includes('node 3개·호출 7개') && t.includes('환경을 고르면'))).toBe(true);
  });

  it('200 node를 넘으면 요청이 적은 node를 그리지 않고 수를 알린다', async () => {
    const many: MapNode[] = [svc(CHECKOUT, 'checkout')];
    const edges: MapEdge[] = [];
    for (let i = 0; i < 205; i++) {
      const id = `host:h${String(i).padStart(3, '0')}.example@prod`;
      many.push({ node_id: id, kind: 'external', name: `h${String(i)}.example`, service_id: null, namespace: null, environment: 'prod' });
      edges.push(observed(CHECKOUT, id, i + 1, 0));
    }
    response = () => json(200, mapBody(many, edges));
    renderAt('/o/acme/map?tz=UTC&range=1h&env=prod');
    await node(/^shop\/checkout/);
    expect(screen.getAllByRole('button', { name: /, 외부$/ })).toHaveLength(199);
    expect(screen.getByText(/요청이 적은 node 6개를 그리지 않았습니다/)).toBeTruthy();
  });

  it('호출이 없으면 이유를 설명한다', async () => {
    response = () => json(200, mapBody([], [], { watermark: null, warnings: ['range_after_watermark'] }));
    renderAt('/o/acme/map?tz=UTC&range=1h');
    expect(await screen.findByText('이 범위에 서비스 간 호출이 없습니다')).toBeTruthy();
    expect(screen.getByText(/아직 집계한 구간이 없을 수도/)).toBeTruthy();
  });

  it('오류는 이유와 다시 시도를 보인다', async () => {
    response = () => json(503, { error: { code: 'UNAVAILABLE', message: 'down', request_id: 'req-x', retryable: true } });
    renderAt('/o/acme/map?tz=UTC&range=1h');
    expect(await screen.findByRole('button', { name: '다시 시도' })).toBeTruthy();
  });

  it('7일을 넘는 범위는 조회하지 않고 줄이라고 한다', async () => {
    renderAt('/o/acme/map?tz=UTC&from=2026-09-01T00:00:00Z&to=2026-10-04T00:00:00Z');
    expect(await screen.findByText(/7일까지 조회할 수 있습니다/)).toBeTruthy();
    expect(requests()).toHaveLength(0);
  });

  it('배치 초기화는 옮긴 node를 되돌리고, 확대·축소 버튼이 있다', async () => {
    renderAt('/o/acme/map?tz=UTC&range=1h');
    const checkout = await node(/^shop\/checkout/);
    const before = checkout.getAttribute('transform');
    const svg = screen.getByRole('group', { name: /서비스 지도/ });
    await act(async () => {
      checkout.dispatchEvent(new MouseEvent('pointerdown', { bubbles: true, button: 0, clientX: 10, clientY: 10 }));
      svg.dispatchEvent(new MouseEvent('pointermove', { bubbles: true, clientX: 60, clientY: 90 }));
      svg.dispatchEvent(new MouseEvent('pointerup', { bubbles: true, clientX: 60, clientY: 90 }));
    });
    expect(checkout.getAttribute('transform')).not.toBe(before);
    await userEvent.click(screen.getByRole('button', { name: '배치 초기화' }));
    expect(checkout.getAttribute('transform')).toBe(before);
    expect(screen.getByRole('button', { name: '확대' })).toBeTruthy();
  });

  it('7일을 넘는 범위로 바꾸면 이전 범위의 지도를 남기지 않는다', async () => {
    const router = renderAt('/o/acme/map?tz=UTC&range=1h');
    await node(/^shop\/checkout/);
    await act(async () => {
      await router.navigate('/o/acme/map?tz=UTC&from=2026-09-01T00:00:00Z&to=2026-10-04T00:00:00Z');
    });
    expect(await screen.findByText(/7일까지 조회할 수 있습니다/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: /^shop\/checkout/ })).toBeNull();
    expect(requests()).toHaveLength(1);
  });

  it('요청/s는 집계한 구간(watermark까지)으로 나눈다 — 미계산 구간을 0 트래픽으로 세지 않는다', async () => {
    response = () => json(200, mapBody(baseNodes, baseEdges, { watermark: '2026-10-04T05:30:00Z', warnings: ['range_after_watermark'] }));
    renderAt('/o/acme/map?tz=UTC&range=1h&tab=table');
    const table = await screen.findByRole('table', { name: '서비스 간 호출' });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows[0]?.textContent).toContain('4/s'); // 7,200 / 30분
    expect(rows[1]?.textContent).toContain('2/s'); // 3,600 / 30분
  });

  it('서비스 node 두 번 클릭은 서비스 상세로 간다', async () => {
    const router = renderAt('/o/acme/map?tz=UTC&range=1h');
    await userEvent.dblClick(await node(/^shop\/payment, 서비스/));
    await waitFor(() => expect(router.state.location.pathname).toBe(`/o/acme/services/${PAYMENT}`));
  });

  it('drawer를 닫으면 focus를 그 node로 돌려준다', async () => {
    renderAt('/o/acme/map?tz=UTC&range=1h');
    await userEvent.click(await node(/^shop\/payment, 서비스/));
    const drawer = await screen.findByRole('complementary', { name: 'shop/payment' });
    await userEvent.click(within(drawer).getByRole('button', { name: '선택 해제 (Esc)' }));
    await waitFor(() => expect(document.activeElement?.getAttribute('aria-label')).toBe('shop/payment, 서비스'));
  });

  it('빈 결과에도 경고를 보인다(미계산·환경 밖 숨김을 "호출 없음"으로 읽지 않게)', async () => {
    response = () => json(200, mapBody([], [], { warnings: ['cross_environment_edges_hidden'] }));
    renderAt('/o/acme/map?tz=UTC&range=1h');
    expect(await screen.findByText('이 범위에 서비스 간 호출이 없습니다')).toBeTruthy();
    expect(screen.getByText(/서로 다른 환경의 서비스/)).toBeTruthy();
  });

  it('공유 링크의 서비스가 지도에 없으면 그렇게 알리고 선택을 풀 수 있다', async () => {
    const router = renderAt('/o/acme/map?tz=UTC&range=1h&service=aaaaaaaa-0000-4000-8000-0000000000ff');
    expect(await screen.findByText(/고른 서비스는 이 범위·환경의 지도에 없습니다/)).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: '선택 해제' }));
    expect(router.state.location.search).not.toContain('service=');
  });

  it('옮긴 배치는 표 보기 전환과 새로고침 뒤에도 남는다', async () => {
    renderAt('/o/acme/map?tz=UTC&range=1h');
    const checkout = await node(/^shop\/checkout/);
    const svg = screen.getByRole('group', { name: /서비스 지도/ });
    await act(async () => {
      checkout.dispatchEvent(new MouseEvent('pointerdown', { bubbles: true, button: 0, clientX: 10, clientY: 10 }));
      svg.dispatchEvent(new MouseEvent('pointermove', { bubbles: true, clientX: 60, clientY: 90 }));
      svg.dispatchEvent(new MouseEvent('pointerup', { bubbles: true, clientX: 60, clientY: 90 }));
    });
    const moved = checkout.getAttribute('transform');
    await userEvent.click(screen.getByRole('button', { name: '표' }));
    await userEvent.click(screen.getByRole('button', { name: '지도' }));
    expect((await node(/^shop\/checkout/)).getAttribute('transform')).toBe(moved);
    await userEvent.click(screen.getByRole('button', { name: '새로고침' }));
    await waitFor(() => expect(requests()).toHaveLength(2));
    expect((await node(/^shop\/checkout/)).getAttribute('transform')).toBe(moved);
  });
});
