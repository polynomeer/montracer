import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { DashboardItem, DashboardSpec, Widget } from '../../api/dashboards.ts';
import type { MetricPoint, MetricQuery } from '../../api/types.ts';
import { routes } from '../../app/routes.tsx';
import type { ApiResult } from '../../api/client.ts';
import type { MetricResult } from '../../api/types.ts';
import { clearDraft, newWidget, saveDraft } from './dashboardSpec.ts';
import { QueryPool } from './queryPool.ts';
import { WidgetView } from './WidgetView.tsx';
import { controlMeta, metricData, queryMeta, serviceFixture } from '../../test/fixtures';

const meta = queryMeta({ request_id: 'req-d' });
const cmeta = controlMeta({ request_id: 'req-d' });
const ID = 'cccccccc-0000-4000-8000-000000000001';
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const err = (status: number, code: string, message = 'x') => json(status, { error: { code, message, request_id: 'r', retryable: false } });

const w = (id: string, kind: Widget['kind'], over: Partial<Widget> = {}): Widget => ({ ...newWidget(id, kind), ...over });

const specOf = (): DashboardSpec => ({
  version: 1,
  title: 'Checkout',
  description: '결제 화면',
  variables: [{ name: 'svc', kind: 'service', default: 'checkout' }],
  widgets: [
    w('lat', 'stat', { title: 'p95', unit: 's', query: { metric: 'http.server.request.duration', aggregation: 'p95', filter: { op: 'eq', field: 'service.name', value: '$svc' }, group_by: [] } }),
    w('lat2', 'stat', { title: 'p95 복제', unit: 's', query: { metric: 'http.server.request.duration', aggregation: 'p95', filter: { op: 'eq', field: 'service.name', value: '$svc' }, group_by: [] } }),
    w('top', 'top_list', { title: '경로', query: { metric: 'http.server.request.duration', aggregation: 'count', filter: null, group_by: ['http.route'] }, display: { decimals: 0, limit: 2, sort: 'desc' } }),
    w('note', 'text', { title: '메모', text: '<b>굵게</b> 아님' }),
  ],
  layout: [
    { widget_id: 'lat', x: 0, y: 0, w: 6, h: 3 },
    { widget_id: 'lat2', x: 6, y: 0, w: 6, h: 3 },
    { widget_id: 'top', x: 12, y: 0, w: 12, h: 6 },
    { widget_id: 'note', x: 0, y: 6, w: 24, h: 2 },
  ],
});

const item = (over: Partial<DashboardItem> = {}): DashboardItem => ({
  id: ID,
  title: 'Checkout',
  revision: 3,
  widget_count: 4,
  created_at: '2026-10-13T00:00:00Z',
  updated_at: '2026-10-13T00:00:00Z',
  created_by: 'dev',
  updated_by: 'dev',
  spec: specOf(),
  ...over,
});

let current: () => DashboardItem;
let update: () => Response;
let create: () => Response;
let metricQueries: MetricQuery[];

beforeEach(() => {
  current = () => item();
  update = () => json(200, { data: item({ revision: 4 }), meta: cmeta });
  create = () => json(201, { data: item({ id: 'cccccccc-0000-4000-8000-000000000002', revision: 1 }), meta: cmeta });
  metricQueries = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET';
      const path = url.replace(/\?.*$/, '');
      if (path === '/api/v1/dashboards' && method === 'GET') {
        return Promise.resolve(json(200, { data: [{ ...item(), spec: undefined }], next_cursor: null, meta: cmeta }));
      }
      if (path === '/api/v1/dashboards' && method === 'POST') return Promise.resolve(create());
      if (path === `/api/v1/dashboards/${ID}` && method === 'GET') return Promise.resolve(json(200, { data: current(), meta: cmeta }));
      if (path === `/api/v1/dashboards/${ID}` && method === 'PUT') return Promise.resolve(update());
      if (path === `/api/v1/dashboards/${ID}` && method === 'DELETE') return Promise.resolve(new Response(null, { status: 204 }));
      if (path === '/api/v1/services') {
        return Promise.resolve(json(200, { data: ['checkout', 'cart'].map((n, i) => serviceFixture({ name: n, service_id: `aaaaaaaa-0000-4000-8000-00000000000${String(i + 1)}` })), next_cursor: null, meta }));
      }
      if (path === '/api/v1/query/metrics') {
        const q = JSON.parse(String(init?.body)) as MetricQuery;
        metricQueries.push(q);
        const point = (v: number | null): MetricPoint => ({ t: q.range.from, v, partial: false, ...(v === null ? { reason: 'no_data' as const } : {}) });
        const series =
          q.expression.group_by?.[0] === 'http.route'
            ? [
                { labels: { 'http.route': '/pay' }, unit: null, points: [point(120)], completeness: 1, missing: [], flags: [] },
                { labels: { 'http.route': '/cart' }, unit: null, points: [point(null)], completeness: 0, missing: [], flags: [] },
                { labels: { 'http.route': '/home' }, unit: null, points: [point(300)], completeness: 1, missing: [], flags: [] },
              ]
            : [{ labels: {}, unit: 's', points: [point(0.412)], completeness: 1, missing: [], flags: [] }];
        return Promise.resolve(json(200, { data: metricData(series), meta }));
      }
      return Promise.resolve(err(404, 'NOT_FOUND'));
    }),
  );
  vi.stubGlobal('crypto', { ...globalThis.crypto, randomUUID: () => 'idem-dash-1' });
});

afterEach(() => {
  vi.unstubAllGlobals();
  clearDraft('acme', ID);
  clearDraft('acme', 'new');
});

const calls = (method: string, suffix: string) =>
  vi
    .mocked(fetch)
    .mock.calls.filter(([u, init]) => ((init as RequestInit | undefined)?.method ?? 'GET') === method && String(u).split('?')[0]?.endsWith(suffix))
    .map(([u, init]) => ({ url: String(u), init: init as RequestInit }));

function renderAt(path: string) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return router;
}

describe('Dashboard 목록', () => {
  it('제목·위젯 수, 비었으면 만들기 안내', async () => {
    renderAt('/o/acme/dashboards?tz=UTC');
    const table = await screen.findByRole('table', { name: /dashboard 목록/ });
    expect(within(table).getByRole('link', { name: 'Checkout' }).getAttribute('href')).toBe(`/o/acme/dashboards/${ID}?tz=UTC`);
    expect(table.textContent).toContain('4');
  });
});

describe('Dashboard 보기', () => {
  it('위젯을 그린다: 숫자·상위 목록(값 없는 행은 이유와 뒤로)·글은 글자 그대로', async () => {
    renderAt(`/o/acme/dashboards/${ID}?tz=UTC`);
    expect(await screen.findByRole('heading', { name: 'Checkout' })).toBeTruthy();
    const stat = await screen.findByRole('article', { name: 'p95' });
    await waitFor(() => expect(stat.textContent).toContain('0.41'));
    expect(stat.textContent).toContain('$svc');
    const top = screen.getByRole('article', { name: '경로' });
    await waitFor(() => expect(within(top).getAllByRole('row')).toHaveLength(3));
    const rows = within(top).getAllByRole('row').slice(1);
    expect(rows.map((r) => r.querySelector('th')?.textContent)).toEqual(['http.route=/home', 'http.route=/pay']);
    expect(top.textContent).toContain('나머지 1개');
    const note = screen.getByRole('article', { name: '메모' });
    expect(note.textContent).toContain('<b>굵게</b> 아님');
    expect(note.querySelector('b')).toBeNull();
  });

  it('같은 query는 한 번만 부르고, 변수는 기본값으로 걸린다. 변수를 바꾸면 URL에 남고 다시 조회한다', async () => {
    const router = renderAt(`/o/acme/dashboards/${ID}?tz=UTC`);
    await waitFor(() => expect(metricQueries).toHaveLength(2)); // stat 두 개는 같은 query → 한 번
    const statQ = metricQueries.find((q) => q.expression.aggregation === 'p95');
    expect(statQ?.expression.filter).toEqual({ op: 'eq', field: 'service.name', value: 'checkout' });
    const select = await screen.findByLabelText(/\$svc/);
    await waitFor(() => expect(within(select).getAllByRole('option').map((o) => o.textContent)).toEqual(['전체', 'cart', 'checkout']));
    await userEvent.selectOptions(select, '');
    expect(router.state.location.search).toContain('var-svc=');
    await waitFor(() => expect(metricQueries.filter((q) => q.expression.aggregation === 'p95' && q.expression.filter === undefined)).toHaveLength(1));
  });

  it('조회가 실패한 위젯은 실패를 보이고 다른 위젯은 그대로', async () => {
    const real = vi.mocked(fetch).getMockImplementation();
    vi.mocked(fetch).mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const body = init?.body === undefined ? null : (JSON.parse(String(init.body)) as MetricQuery);
      if (String(url).includes('/query/metrics') && body?.expression.aggregation === 'count') return Promise.resolve(err(503, 'UNAVAILABLE', '잠시 뒤'));
      return (real as (u: RequestInfo | URL, i?: RequestInit) => Promise<Response>)(url, init);
    });
    renderAt(`/o/acme/dashboards/${ID}?tz=UTC`);
    const top = await screen.findByRole('article', { name: '경로' });
    await waitFor(() => expect(top.textContent).toContain('잠시 뒤'));
    await waitFor(() => expect(screen.getByRole('article', { name: 'p95' }).textContent).toContain('0.41'));
  });

  it('없는 dashboard는 404 화면', async () => {
    current = () => {
      throw new Error('unused');
    };
    renderAt('/o/acme/dashboards/cccccccc-0000-4000-8000-00000000dead?tz=UTC');
    expect(await screen.findByText('dashboard를 찾을 수 없습니다')).toBeTruthy();
  });
});

describe('Dashboard 편집', () => {
  it('새 dashboard: 제목 없으면 저장하지 않고, 위젯을 추가·편집해 Idempotency-Key로 만든다', async () => {
    const router = renderAt('/o/acme/dashboards/new?tz=UTC');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '만들기' }));
    expect(screen.getByText('제목을 입력하세요')).toBeTruthy();
    await user.type(screen.getByLabelText('제목'), '새 화면');
    await user.selectOptions(screen.getByLabelText('새 위젯'), 'stat');
    await user.click(screen.getByRole('button', { name: '위젯 추가' }));
    const editor = screen.getByRole('group', { name: '위젯 w1 편집' });
    await user.click(within(editor).getByRole('button', { name: '위젯 적용' }));
    expect(within(editor).getByText('metric 이름을 입력하세요')).toBeTruthy();
    await user.type(within(editor).getByLabelText('metric'), 'http.server.request.duration');
    await user.type(within(editor).getByLabelText('제목'), '지연');
    await user.click(within(editor).getByRole('button', { name: '조건 추가' }));
    await user.type(within(editor).getByLabelText('label 1'), 'service.name');
    await user.type(within(editor).getByLabelText('값 1'), '$nope');
    await user.click(within(editor).getByRole('button', { name: '위젯 적용' }));
    expect(within(editor).getByText('변수 $nope가 없습니다')).toBeTruthy();
    await user.clear(within(editor).getByLabelText('값 1'));
    await user.type(within(editor).getByLabelText('값 1'), 'checkout');
    await user.click(within(editor).getByRole('button', { name: '위젯 적용' }));
    expect(screen.queryByRole('group', { name: '위젯 w1 편집' })).toBeNull();
    await user.click(screen.getByRole('button', { name: '만들기' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/o/acme/dashboards/cccccccc-0000-4000-8000-000000000002'));
    const post = calls('POST', '/dashboards')[0];
    expect((post?.init.headers as Record<string, string>)['Idempotency-Key']).toBe('idem-dash-1');
    const body = JSON.parse(String(post?.init.body)) as DashboardSpec;
    expect(body.title).toBe('새 화면');
    expect(body.widgets[0]).toMatchObject({ id: 'w1', kind: 'stat', title: '지연', query: { metric: 'http.server.request.duration', filter: { op: 'eq', field: 'service.name', value: 'checkout' } } });
    expect(body.layout).toEqual([{ widget_id: 'w1', x: 0, y: 0, w: 6, h: 3 }]);
  });

  it('위치·크기는 키보드로 고치고, 겹치면 적용하지 않는다', async () => {
    renderAt(`/o/acme/dashboards/${ID}/edit?tz=UTC`);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '메모 위치·크기' }));
    const dialog = screen.getByRole('group', { name: 'note 위치·크기' });
    expect(document.activeElement).toBe(within(dialog).getByLabelText('가로 위치(0~23)'));
    await user.click(within(dialog).getByRole('button', { name: '↑ 위' }));
    await user.click(within(dialog).getByRole('button', { name: '↑ 위' }));
    expect(within(dialog).getByText(/겹칩니다/)).toBeTruthy();
    expect(within(dialog).getByRole('button', { name: '적용' }).hasAttribute('disabled')).toBe(true);
    await user.click(within(dialog).getByRole('button', { name: '↓ 아래' }));
    await user.click(within(dialog).getByRole('button', { name: '↓ 아래' }));
    await user.click(within(dialog).getByRole('button', { name: '↓ 아래' }));
    await user.click(within(dialog).getByRole('button', { name: '적용' }));
    expect(screen.getByText(/위치 0,7 크기 24×2/)).toBeTruthy();
  });

  it('다른 변경이 먼저 저장됐으면(412) 덮어쓰지 않고 차이를 보이고, 최신 위에 다시 적용한다', async () => {
    update = () => json(412, { error: { code: 'REVISION_MISMATCH', message: 'x', request_id: 'r', retryable: false } });
    const router = renderAt(`/o/acme/dashboards/${ID}/edit?tz=UTC`);
    const user = userEvent.setup();
    const title = await screen.findByLabelText('제목');
    await user.clear(title);
    await user.type(title, '내 제목');
    current = () => item({ revision: 5, updated_by: 'other', spec: { ...specOf(), description: '남이 바꿈' } });
    await user.click(screen.getByRole('button', { name: '저장(공유 화면에 반영)' }));
    const alert = await screen.findByText(/다른 변경이 먼저 저장되었습니다\(revision 5, other\)/);
    const box = alert.closest('[role="alert"]') as HTMLElement;
    expect(box.textContent).toContain('그 사이 바뀐 것: 설명이 바뀌었습니다');
    expect(box.textContent).toContain('내 편집: 제목: “Checkout” → “내 제목”');
    update = () => json(200, { data: item({ revision: 6 }), meta: cmeta });
    await user.click(within(box).getByRole('button', { name: '내 편집을 최신 위에 다시 적용' }));
    await waitFor(() => expect(router.state.location.pathname).toBe(`/o/acme/dashboards/${ID}`));
    const puts = calls('PUT', `/dashboards/${ID}`);
    expect((puts[0]?.init.headers as Record<string, string>)['If-Match']).toBe('"3"');
    expect((puts[1]?.init.headers as Record<string, string>)['If-Match']).toBe('"5"');
  });

  it('충돌 때 복제로 저장하면 새 dashboard(복사본)를 만든다', async () => {
    update = () => json(412, { error: { code: 'REVISION_MISMATCH', message: 'x', request_id: 'r', retryable: false } });
    renderAt(`/o/acme/dashboards/${ID}/edit?tz=UTC`);
    const user = userEvent.setup();
    await screen.findByLabelText('제목');
    await user.click(screen.getByRole('button', { name: '저장(공유 화면에 반영)' }));
    await user.click(await screen.findByRole('button', { name: '새 dashboard로 복제해 저장' }));
    await waitFor(() => expect(calls('POST', '/dashboards')).toHaveLength(1));
    expect((JSON.parse(String(calls('POST', '/dashboards')[0]?.init.body)) as DashboardSpec).title).toBe('Checkout (복사본)');
  });

  it('저장하지 않은 초안이 있으면 복원을 묻는다', async () => {
    saveDraft('acme', ID, { baseRevision: 3, baseSpec: specOf(), spec: { ...specOf(), title: '초안 제목' }, savedAt: Date.parse('2026-10-14T01:02:03Z') });
    renderAt(`/o/acme/dashboards/${ID}/edit?tz=UTC`);
    const user = userEvent.setup();
    expect(await screen.findByText(/저장하지 않은 초안이 있습니다\(01:02:03\)/)).toBeTruthy();
    await user.click(screen.getByRole('button', { name: '초안 복원' }));
    expect(screen.getByLabelText<HTMLInputElement>('제목').value).toBe('초안 제목');
  });

  it('Developer가 아니면(403) 저장하지 못한 이유를 보인다', async () => {
    update = () => err(403, 'FORBIDDEN');
    renderAt(`/o/acme/dashboards/${ID}/edit?tz=UTC`);
    const user = userEvent.setup();
    await screen.findByLabelText('제목');
    await user.click(screen.getByRole('button', { name: '저장(공유 화면에 반영)' }));
    expect(await screen.findByText('dashboard를 바꿀 권한이 없습니다(Developer 이상).')).toBeTruthy();
  });
});

describe('리뷰 반영', () => {
  const statW = w('s', 'stat', { title: '지연', unit: 's', query: { metric: 'm', aggregation: 'p95', filter: null, group_by: [] } });
  const result = (v: number, partial = false): ApiResult<MetricResult> => ({
    ok: true,
    status: 200,
    data: metricData([{ labels: {}, unit: 's', points: [{ t: '2026-10-14T00:00:00Z', v, partial: false }], completeness: 1, missing: [], flags: [] }]),
    meta: { ...meta, partial },
    nextCursor: null,
  });

  it('자동 갱신으로 상대 범위가 1분 움직여도 값을 유지하고, 갱신이 실패하면 마지막 값과 "갱신 실패"를 보인다', async () => {
    let fail = false;
    const pool = new QueryPool<MetricResult>(6, () =>
      Promise.resolve(fail ? { ok: false as const, status: 503, error: { code: 'UNAVAILABLE', message: '잠시 뒤', request_id: 'r1', retryable: true }, retryAfterSeconds: null } : result(0.412)),
    );
    const T = Date.parse('2026-10-14T00:00:30Z');
    const props = { widget: statW, values: {}, rangeKey: 'rel-1h', globalEnv: null, pool, timeZone: 'UTC' };
    const view = render(<WidgetView {...props} fromMs={T - 3600_000} toMs={T} nowMs={T} refreshTick={0} />);
    await waitFor(() => expect(screen.getByRole('article', { name: '지연' }).textContent).toContain('0.41'));
    fail = true;
    const T2 = T + 60_000;
    view.rerender(<WidgetView {...props} fromMs={T2 - 3600_000} toMs={T2} nowMs={T2} refreshTick={1} />);
    expect(screen.getByRole('article', { name: '지연' }).textContent).toContain('0.41'); // skeleton으로 바뀌지 않는다
    await waitFor(() => expect(screen.getByRole('article', { name: '지연' }).textContent).toContain('갱신 실패'));
    expect(screen.getByRole('article', { name: '지연' }).textContent).toContain('0.41');
    expect(screen.getByRole('button', { name: '지연 다시 시도' })).toBeTruthy();
  });

  it('갱신이 연속으로 실패하면 횟수를 붙인다', async () => {
    let fail = false;
    const pool = new QueryPool<MetricResult>(6, () =>
      Promise.resolve(fail ? { ok: false as const, status: 503, error: { code: 'UNAVAILABLE', message: '잠시 뒤', request_id: 'r1', retryable: true }, retryAfterSeconds: null } : result(0.412)),
    );
    const T = Date.parse('2026-10-14T00:00:30Z');
    const props = { widget: statW, values: {}, rangeKey: 'rel-1h', globalEnv: null, pool, timeZone: 'UTC', fromMs: T - 3600_000, toMs: T, nowMs: T };
    const view = render(<WidgetView {...props} refreshTick={0} />);
    const card = () => screen.getByRole('article', { name: '지연' });
    await waitFor(() => expect(card().textContent).toMatch(/\d{2}:\d{2}:\d{2} 갱신/));
    expect(card().textContent).toContain('숫자 하나 · p95 · s');
    expect(card().textContent).toContain('bucket 병합 · 평균 아님');
    fail = true;
    view.rerender(<WidgetView {...props} refreshTick={1} />);
    await waitFor(() => expect(card().textContent).toContain('갱신 실패'));
    view.rerender(<WidgetView {...props} refreshTick={2} />);
    await waitFor(() => expect(card().textContent).toContain('갱신 2회 실패'));
    fail = false;
    view.rerender(<WidgetView {...props} refreshTick={3} />);
    await waitFor(() => expect(card().textContent).not.toContain('실패'));
  });

  it('변수나 상단 환경을 무시하는 고정 조건은 배지로 보인다', async () => {
    const fixedW = w('f', 'stat', { title: '결제 504', query: { metric: 'm', aggregation: 'count', filter: { op: 'and', args: [{ op: 'eq', field: 'service.name', value: 'payment-gateway' }, { op: 'eq', field: 'deployment.environment.name', value: 'prod' }] }, group_by: [] } });
    const pool = new QueryPool<MetricResult>(6, () => Promise.resolve(result(3)));
    const T = Date.parse('2026-10-14T00:00:00Z');
    render(
      <WidgetView widget={fixedW} variables={[{ name: 'svc', kind: 'service', default: null }]} values={{}} rangeKey="r" globalEnv="stage" pool={pool} timeZone="UTC" fromMs={T - 3600_000} toMs={T} nowMs={T} refreshTick={0} />,
    );
    const card = await screen.findByRole('article', { name: '결제 504' });
    expect(card.textContent).toContain('고정 service = payment-gateway · $svc 무시');
    expect(card.textContent).toContain('고정 environment = prod · 상단 환경 무시');
    expect(card.textContent).not.toContain('bucket 병합');
  });

  it('보기 머리: 게시본 revision·다음 갱신·일시 정지, 변수 chip, query 묶음 줄', async () => {
    renderAt(`/o/acme/dashboards/${ID}?tz=UTC`);
    expect(await screen.findByText(/게시본 r3/)).toBeTruthy();
    expect(screen.getByText(/자동 갱신 30초 · 마지막 .* · 다음/)).toBeTruthy();
    expect(screen.getByText(/\$svc =/)).toBeTruthy();
    expect(screen.getByText(/볼 수 있는 값만 고를 수 있습니다/)).toBeTruthy();
    // 위젯 4개(글 포함), 조회 3개 중 같은 query 1개 합침 → 요청 2개
    expect(screen.getByText(/위젯 4\/50 · query 3개\(같은 query 1개 합침, 요청 2개\) · 동시 6개까지/)).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: '일시 정지' }));
    expect(screen.getByRole('button', { name: '자동 갱신 켜기' }).getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByText(/자동 갱신 멈춤/)).toBeTruthy();
  });

  it('링크 복사는 지금 주소를 복사하고, 실패하면 그렇다고 말한다', async () => {
    const writeText = vi.fn((): Promise<void> => Promise.reject(new Error('denied')));
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    renderAt(`/o/acme/dashboards/${ID}?tz=UTC`);
    await userEvent.click(await screen.findByRole('button', { name: '링크 복사' }));
    expect(writeText).toHaveBeenCalledWith(window.location.href);
    expect(await screen.findByText('복사하지 못했습니다')).toBeTruthy();
  });

  it('일부 저장소가 응답하지 않은 결과(meta.partial)는 "일부"로 보인다', async () => {
    const pool = new QueryPool<MetricResult>(6, () => Promise.resolve(result(0.412, true)));
    const T = Date.parse('2026-10-14T00:00:00Z');
    render(<WidgetView widget={statW} values={{}} rangeKey="r" globalEnv={null} pool={pool} timeZone="UTC" fromMs={T - 3600_000} toMs={T} nowMs={T} refreshTick={0} />);
    const card = await screen.findByRole('article', { name: '지연' });
    await waitFor(() => expect(card.textContent).toContain('일부 저장소 응답 없음'));
    expect(card.textContent).toContain('(일부)');
  });

  it('상단 환경을 위젯 조회에 건다', async () => {
    renderAt(`/o/acme/dashboards/${ID}?tz=UTC&env=prod`);
    await waitFor(() => expect(metricQueries.length).toBeGreaterThan(0));
    const top = metricQueries.find((q) => q.expression.aggregation === 'count');
    expect(top?.expression.filter).toEqual({ op: 'eq', field: 'deployment.environment.name', value: 'prod' });
    expect(await screen.findByText(/상단 환경\(prod\)으로 거릅니다/)).toBeTruthy();
  });

  it('이전 revision 기준 초안을 복원해 저장하면, 그 사이 남의 변경을 초안 기준으로 보인다(덮어쓰기 전에)', async () => {
    saveDraft('acme', ID, { baseRevision: 3, baseSpec: specOf(), spec: { ...specOf(), title: '초안 제목' }, savedAt: Date.parse('2026-10-14T01:02:03Z') });
    current = () => item({ revision: 5, spec: { ...specOf(), description: '남이 바꿈' } });
    update = () => json(412, { error: { code: 'REVISION_MISMATCH', message: 'x', request_id: 'r', retryable: false } });
    renderAt(`/o/acme/dashboards/${ID}/edit?tz=UTC`);
    const user = userEvent.setup();
    expect(await screen.findByText(/revision 3 기준입니다/)).toBeTruthy();
    await user.click(screen.getByRole('button', { name: '초안 복원' }));
    await user.click(screen.getByRole('button', { name: '저장(공유 화면에 반영)' }));
    const alert = await screen.findByText(/다른 변경이 먼저 저장되었습니다/);
    const box = alert.closest('[role="alert"]') as HTMLElement;
    expect(box.textContent).toContain('그 사이 바뀐 것: 설명이 바뀌었습니다');
    expect(box.textContent).toContain('내 편집: 제목: “Checkout” → “초안 제목”');
    expect((calls('PUT', `/dashboards/${ID}`)[0]?.init.headers as Record<string, string>)['If-Match']).toBe('"3"');
  });

  it('새 위젯 편집을 취소하면 위젯이 남지 않고, 저장 전 검사가 끊긴 변수 참조를 막는다', async () => {
    renderAt(`/o/acme/dashboards/${ID}/edit?tz=UTC`);
    const user = userEvent.setup();
    await screen.findByLabelText('제목');
    await user.click(screen.getByRole('button', { name: '위젯 추가' }));
    expect(screen.getByRole('group', { name: '위젯 w5 편집' })).toBeTruthy();
    await user.click(within(screen.getByRole('group', { name: '위젯 w5 편집' })).getByRole('button', { name: '취소' }));
    expect(screen.queryByText(/위치 0,8/)).toBeNull();
    expect(document.activeElement?.id).toBe('dash-add-kind');
    await user.click(screen.getByRole('button', { name: '변수 svc 지우기' }));
    await user.click(screen.getByRole('button', { name: '저장(공유 화면에 반영)' }));
    expect(await screen.findByText('저장하기 전에 고칠 것이 있습니다')).toBeTruthy();
    expect(screen.getByText('“p95”: 없는 변수 $svc를 씁니다')).toBeTruthy();
    expect(calls('PUT', `/dashboards/${ID}`)).toHaveLength(0);
  });

  it('위젯 편집에서 미리보기로 지금 입력의 값을 그린다', async () => {
    renderAt(`/o/acme/dashboards/${ID}/edit?tz=UTC`);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'p95 편집' }));
    const editor = screen.getByRole('group', { name: '위젯 lat 편집' });
    expect(document.activeElement).toBe(within(editor).getByLabelText('종류'));
    await user.click(within(editor).getByRole('button', { name: '미리보기' }));
    const preview = await within(editor).findByLabelText('위젯 미리보기');
    await waitFor(() => expect(preview.textContent).toContain('0.41'));
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('group', { name: '위젯 lat 편집' })).toBeNull();
    expect(document.activeElement?.id).toBe('dash-edit-lat');
  });
});
