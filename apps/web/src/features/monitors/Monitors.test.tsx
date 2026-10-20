import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { MonitorItem, MonitorSpec, Timeline, ValidateResult } from '../../api/monitors.ts';
import { routes } from '../../app/routes.tsx';

const NOW = Date.parse('2026-10-13T06:00:00Z');
const meta = { request_id: 'req-m', schema_version: 1, warnings: [] as string[] };
const ID_A = 'aaaaaaaa-0000-4000-8000-000000000001';
const ID_B = 'aaaaaaaa-0000-4000-8000-000000000002';
const ID_C = 'aaaaaaaa-0000-4000-8000-000000000003';
const POLICY = 'bbbbbbbb-0000-4000-8000-000000000001';

const spec = (name: string, over: Partial<MonitorSpec> = {}): MonitorSpec => ({
  name,
  enabled: true,
  query: { kind: 'error_ratio', metric: 'http.server.request.duration', aggregation: null, filter: null, group_by: ['service.name'] },
  window_seconds: 300,
  evaluation_seconds: 60,
  condition: { operator: 'gt', threshold: 0.02 },
  minimum_requests: 100,
  for_seconds: 120,
  recovery_evaluations: 2,
  no_data: { action: 'no_data', after_seconds: null },
  notification_policy_id: null,
  ...over,
});

const monitor = (id: string, name: string, status: MonitorItem['status'], over: Partial<MonitorSpec> = {}): MonitorItem => ({
  id,
  name,
  enabled: over.enabled ?? true,
  revision: 3,
  spec: spec(name, over),
  created_at: '2026-10-10T00:00:00Z',
  updated_at: '2026-10-12T00:00:00Z',
  created_by: 'dev',
  updated_by: 'dev',
  status,
});

const evaluation = (slot: string, status: 'evaluated' | 'no_data' | 'error' = 'evaluated', alerting = 0) => ({
  slot,
  status,
  reason: status === 'error' ? 'query_failed' : null,
  window_start: null,
  window_end: null,
  groups: 2,
  alerting,
  duration_ms: 12,
});

let monitors: MonitorItem[];
let listMeta: typeof meta;
let groupsTruncated: boolean;
let currentTimeline: Timeline;
let policiesResponse: () => Response;
let validateResponse: () => Response;
let createResponse: () => Response;
let updateResponse: () => Response;
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const calls = (method: string, suffix: string) =>
  vi
    .mocked(fetch)
    .mock.calls.filter(([u, init]) => ((init as RequestInit | undefined)?.method ?? 'GET') === method && String(u).includes(suffix))
    .map(([u, init]) => ({ url: String(u), init: init as RequestInit }));

const timeline: Timeline = {
  from: '2026-10-12T06:00:00Z',
  to: '2026-10-13T06:00:00Z',
  evaluations: [evaluation('2026-10-13T05:57:00Z'), evaluation('2026-10-13T05:58:00Z', 'evaluated', 1), evaluation('2026-10-13T05:59:00Z', 'error')],
  transitions: [
    { at: '2026-10-13T05:58:00Z', group_hash: 'h1', labels: { 'service.name': 'checkout' }, from: 'OK', to: 'ALERT', opened: true, closed: false, episode_id: 'e1', reason: null, value: 0.051 },
  ],
  notifications: [
    { id: 'n1', policy_id: POLICY, event_type: 'alert.opened', renotify_sequence: null, group_hash: 'h1', episode_id: 'e1', status: 'failed', attempts: 30, last_status_code: 503, last_error: 'http_status', created_at: '2026-10-13T05:58:01Z', finished_at: '2026-10-13T05:59:00Z' },
    // 이전 control-api(롤백)는 재알림 저장 종류를 그대로 준다 — 닫힘으로 짐작하지 않고 원문으로
    { id: 'n3', policy_id: POLICY, event_type: 'alert.renotified.1' as 'alert.renotified', renotify_sequence: null, group_hash: 'h1', episode_id: 'e1', status: 'delivered', attempts: 1, last_status_code: 200, last_error: null, created_at: '2026-10-13T05:59:10Z', finished_at: '2026-10-13T05:59:11Z' },
    { id: 'n2', policy_id: POLICY, event_type: 'alert.renotified', renotify_sequence: 2, group_hash: 'h1', episode_id: 'e1', status: 'delivered', attempts: 1, last_status_code: 200, last_error: null, created_at: '2026-10-13T05:59:30Z', finished_at: '2026-10-13T05:59:31Z' },
  ],
  evaluations_truncated: false,
  transitions_truncated: false,
  notifications_truncated: false,
};

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(NOW);
  listMeta = meta;
  groupsTruncated = false;
  currentTimeline = timeline;
  policiesResponse = () =>
    json(200, { data: [{ id: POLICY, name: 'On-call', enabled: true, revision: 1, webhook: { url: 'https://hooks.example.com/…', signing_key_id: 'wk_1', previous_signing_key_id: null, previous_signing_key_expires_at: null }, created_at: '2026-10-12T00:00:00Z', updated_at: '2026-10-12T00:00:00Z', created_by: 'user:u1', updated_by: 'user:u1' }], next_cursor: null, meta });
  monitors = [
    monitor(ID_A, 'cart 지연', { state: 'OK', groups_by_state: { OK: 3 }, last_evaluation: evaluation('2026-10-13T05:59:00Z'), stale: false }, {
      query: { kind: 'metric', metric: 'http.server.request.duration', aggregation: 'p95', filter: null, group_by: [] },
      condition: { operator: 'gt', threshold: 0.5 },
      minimum_requests: null,
    }),
    monitor(ID_B, 'checkout 오류율', { state: 'ALERT', groups_by_state: { ALERT: 1, NO_DATA: 1, OK: 2 }, last_evaluation: evaluation('2026-10-13T05:59:00Z', 'evaluated', 1), stale: false }, {
      notification_policy_id: POLICY,
    }),
    monitor(ID_C, 'search 오류율', { state: 'EVALUATION_ERROR', groups_by_state: {}, last_evaluation: evaluation('2026-10-13T05:40:00Z', 'error'), stale: true }),
  ];
  const dryRun: ValidateResult['dry_run'] = {
    from: '2026-10-12T05:59:00Z',
    to: '2026-10-13T05:58:00Z',
    warmup_seconds: 120,
    data_until: '2026-10-13T05:58:00Z',
    step_seconds: 60,
    evaluations: 1440,
    coverage: { evaluated: 1400, no_data: 40, error: 0 },
    groups: [
      {
        group_key: 'g1', labels: { 'service.name': 'checkout' }, firing_intervals: [{ start: '2026-10-12T20:00:00Z', end: '2026-10-12T21:00:00Z', reason: 'violation' }],
        firing_intervals_omitted: 0, firing_seconds: 3600, final_state: 'OK', evaluated: 1400, no_data: 40, partial: 0, first_value_at: null, max_value: 0.061, last_value: 0.01, last_reason: '', transitions: 4,
      },
    ],
    groups_total: 1,
    groups_omitted: 0,
    reason: '',
  };
  validateResponse = () => json(200, { data: { normalized_spec: spec('x'), warnings: ['for_seconds_zero'], dry_run: dryRun }, meta });
  createResponse = () => json(201, { data: monitor(ID_A, 'new', null), meta });
  updateResponse = () => json(200, { data: monitor(ID_B, 'checkout 오류율', null), meta });
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET';
      const path = url.replace(/\?.*$/, '');
      if (path === '/api/v1/monitors' && method === 'GET') return Promise.resolve(json(200, { data: monitors, next_cursor: null, meta: listMeta }));
      if (path === '/api/v1/monitors' && method === 'POST') return Promise.resolve(createResponse());
      if (path === '/api/v1/monitors/validate') return Promise.resolve(validateResponse());
      if (path === '/api/v1/notification-policies') return Promise.resolve(policiesResponse());
      if (path.endsWith('/groups')) {
        return Promise.resolve(
          json(200, {
            data: [
              { group_hash: 'h1', labels: { 'service.name': 'checkout' }, state: 'ALERT', episode_id: 'e1', episode_reason: 'violation', violation_since: '2026-10-13T05:56:00Z', nodata_since: null, last_window_end: '2026-10-13T05:58:00Z', last_value: 0.051, last_reason: null, updated_at: '2026-10-13T05:58:00Z' },
              { group_hash: 'h2', labels: { 'service.name': 'search' }, state: 'NO_DATA', episode_id: null, episode_reason: null, violation_since: null, nodata_since: '2026-10-13T05:50:00Z', last_window_end: null, last_value: null, last_reason: 'below_minimum_requests', updated_at: '2026-10-13T05:58:00Z' },
            ],
            meta: { ...meta, truncated: groupsTruncated },
          }),
        );
      }
      if (path.endsWith('/timeline')) return Promise.resolve(json(200, { data: currentTimeline, meta }));
      const m = monitors.find((x) => path === `/api/v1/monitors/${x.id}`);
      if (m !== undefined && method === 'GET') return Promise.resolve(json(200, { data: m, meta: m.status === null ? { ...meta, warnings: ['status_forbidden'] } : meta }));
      if (m !== undefined && method === 'PUT') return Promise.resolve(updateResponse());
      if (m !== undefined && method === 'DELETE') return Promise.resolve(new Response(null, { status: 204 }));
      return Promise.resolve(json(404, { error: { code: 'NOT_FOUND', message: 'x', request_id: 'r', retryable: false } }));
    }),
  );
  vi.stubGlobal('crypto', { ...globalThis.crypto, randomUUID: () => 'idem-key-1' });
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

describe('Monitor 목록 (S09)', () => {
  it('나쁜 상태 먼저, 평가 오류·갱신 멈춤을 정상과 구분한다', async () => {
    renderAt('/o/acme/monitors?tz=UTC');
    const table = await screen.findByRole('table', { name: /monitor 목록/ });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows.map((r) => within(r).getByRole('rowheader').textContent)).toEqual(['checkout 오류율', 'search 오류율', 'cart 지연']);
    expect(rows[0]?.textContent).toContain('경보');
    expect(rows[0]?.textContent).toContain('경보 1 · 데이터 없음 1 / 4개');
    expect(rows[0]?.textContent).toContain('연결됨');
    expect(rows[1]?.textContent).toContain('평가 오류');
    expect(rows[1]?.textContent).toContain('갱신 멈춤');
    expect(rows[1]?.textContent).toContain('조회 실패');
    expect(rows[2]?.textContent).toContain('정상');
    expect(rows[2]?.textContent).toContain('http.server.request.duration p95 > 0.5');
    expect(screen.getByText('3개 · 경보 1')).toBeTruthy();
  });

  it('상태로 거르고(URL에 남는다), monitor가 없으면 만들기 안내', async () => {
    const router = renderAt('/o/acme/monitors?tz=UTC');
    await screen.findByRole('table', { name: /monitor 목록/ });
    await userEvent.selectOptions(screen.getByLabelText('상태'), 'ALERT');
    expect(router.state.location.search).toContain('state=ALERT');
    expect(within(screen.getByRole('table', { name: /monitor 목록/ })).getAllByRole('row')).toHaveLength(2);
  });

  it('목록 조회 실패는 "없음"이 아니라 실패로', async () => {
    // 로그인 꺼짐(session 확인 404)은 그대로, 목록 조회만 실패
    vi.mocked(fetch).mockImplementation((u) =>
      Promise.resolve(
        String(u).endsWith('/auth/session')
          ? json(404, { error: { code: 'NOT_FOUND', message: 'x', request_id: 'req-s', retryable: false } })
          : json(503, { error: { code: 'UNAVAILABLE', message: 'x', request_id: 'req-x', retryable: true } }),
      ),
    );
    renderAt('/o/acme/monitors?tz=UTC');
    expect(await screen.findByText('monitor 목록을 불러오지 못했습니다. monitor가 없다는 뜻이 아닙니다.')).toBeTruthy();
    expect(screen.queryByText('아직 monitor가 없습니다')).toBeNull();
  });

  it('상태 권한이 없으면 "평가 전"이 아니라 "상태를 볼 수 없음"', async () => {
    monitors = monitors.map((m) => ({ ...m, status: null }));
    listMeta = { ...meta, warnings: ['status_forbidden'] };
    renderAt('/o/acme/monitors?tz=UTC');
    const table = await screen.findByRole('table', { name: /monitor 목록/ });
    expect(within(table).getAllByText('상태를 볼 수 없음')).toHaveLength(3);
    expect(within(table).queryByText('평가 전')).toBeNull();
    expect(screen.getByText(/이 권한으로는 monitor 상태를 볼 수 없습니다/)).toBeTruthy();
  });

  it('갱신이 멈춘 정상은 정상보다 먼저, "갱신 멈춤"으로 거를 수 있다', async () => {
    const a = monitors[0];
    if (a?.status == null) throw new Error('fixture');
    monitors[0] = { ...a, status: { ...a.status, stale: true } };
    monitors.push(monitor('aaaaaaaa-0000-4000-8000-000000000004', 'api 지연', { state: 'OK', groups_by_state: { OK: 1 }, last_evaluation: evaluation('2026-10-13T05:59:00Z'), stale: false }));
    const router = renderAt('/o/acme/monitors?tz=UTC');
    const table = await screen.findByRole('table', { name: /monitor 목록/ });
    const names = within(table).getAllByRole('rowheader').map((h) => h.textContent);
    expect(names.indexOf('cart 지연')).toBeLessThan(names.indexOf('api 지연'));
    await userEvent.selectOptions(screen.getByLabelText('상태'), 'STALE');
    expect(router.state.location.search).toContain('state=STALE');
    expect(within(screen.getByRole('table', { name: /monitor 목록/ })).getAllByRole('rowheader').map((h) => h.textContent)).toEqual(['search 오류율', 'cart 지연']);
  });

  it('monitor API가 꺼진 배포(404)도 "없음"이 아니라 조회 실패로', async () => {
    vi.mocked(fetch).mockImplementation(() => Promise.resolve(json(404, { error: { code: 'NOT_FOUND', message: 'monitor API를 사용할 수 없습니다', request_id: 'r', retryable: false } })));
    renderAt('/o/acme/monitors?tz=UTC');
    expect(await screen.findByText('monitor 목록을 불러오지 못했습니다. monitor가 없다는 뜻이 아닙니다.')).toBeTruthy();
    expect(screen.queryByText('아직 monitor가 없습니다')).toBeNull();
  });

  it('비어 있으면 첫 monitor 만들기', async () => {
    monitors = [];
    renderAt('/o/acme/monitors?tz=UTC');
    expect(await screen.findByText('아직 monitor가 없습니다')).toBeTruthy();
    expect(screen.getByRole('link', { name: '첫 monitor 만들기' }).getAttribute('href')).toBe('/o/acme/monitors/new?tz=UTC');
  });
});

describe('Monitor 상세 (S09)', () => {
  it('정의·group 상태(값 없으면 이유)·timeline(평가 띠, 전이, 알림)을 보인다', async () => {
    renderAt(`/o/acme/monitors/${ID_B}?tz=UTC&range=1d`);
    expect(await screen.findByRole('heading', { name: 'checkout 오류율' })).toBeTruthy();
    expect(screen.getByText('5분 오류율(5xx) > 2% · 최소 100요청 · 2분 유지')).toBeTruthy();
    expect(await screen.findByText('On-call')).toBeTruthy();
    const groups = await screen.findByRole('table', { name: 'group별 지금 상태' });
    const rows = within(groups).getAllByRole('row').slice(1);
    expect(rows[0]?.textContent).toContain('5.1%');
    expect(rows[1]?.textContent).toContain('최소 요청 수 미달');
    expect(rows[1]?.textContent).not.toContain('0%');
    expect(await screen.findByRole('img', { name: /평가 3회: 경보 있음 1, 경보 없음 1, 데이터 없음 0, 오류 1/ })).toBeTruthy();
    const events = screen.getByRole('list', { name: '상태 전이와 알림' });
    expect(events.textContent).toContain('경보 열림: 정상 → 경보');
    expect(events.textContent).toContain('전달 실패(포기) · 시도 30회 · 응답 503');
    expect(events.textContent).toContain('알림 (재알림 2/3): 전달됨');
    expect(events.textContent).toContain('알림 (alert.renotified.1): 전달됨');
    expect(events.textContent).not.toContain('(닫힘)');
    const tl = calls('GET', '/timeline')[0];
    expect(tl?.url).toContain('from=2026-10-12T06%3A00%3A00.000Z');
  });

  it('삭제는 확인 뒤 If-Match로, 끝나면 목록으로', async () => {
    const router = renderAt(`/o/acme/monitors/${ID_B}?tz=UTC`);
    await screen.findByRole('heading', { name: 'checkout 오류율' });
    await userEvent.click(screen.getByRole('button', { name: '삭제' }));
    const dialog = screen.getByRole('alertdialog');
    await userEvent.click(within(dialog).getByRole('button', { name: '삭제' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/o/acme/monitors'));
    const del = calls('DELETE', `/monitors/${ID_B}`)[0];
    expect((del?.init.headers as Record<string, string>)['If-Match']).toBe('"3"');
  });

  it('삭제 확인 대화: 열리면 확인 버튼에 focus, Esc로 닫히면 삭제 버튼으로 돌아온다', async () => {
    renderAt(`/o/acme/monitors/${ID_B}?tz=UTC`);
    await screen.findByRole('heading', { name: 'checkout 오류율' });
    const trigger = screen.getByRole('button', { name: '삭제' });
    await userEvent.click(trigger);
    const dialog = screen.getByRole('alertdialog');
    expect(document.activeElement).toBe(within(dialog).getByRole('button', { name: '삭제' }));
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('alertdialog')).toBeNull();
    expect(document.activeElement).toBe(trigger);
    expect(calls('DELETE', '/monitors/')).toHaveLength(0);
  });

  it('상태 권한이 없으면 group·timeline을 부르지 않고 안내한다', async () => {
    monitors = monitors.map((m) => ({ ...m, status: null }));
    renderAt(`/o/acme/monitors/${ID_B}?tz=UTC`);
    expect(await screen.findByText('상태를 볼 수 없음')).toBeTruthy();
    expect(await screen.findByText(/이 권한으로는 상태를 볼 수 없습니다/)).toBeTruthy();
    expect(screen.queryByRole('table', { name: 'group별 지금 상태' })).toBeNull();
    expect(screen.getByText('5분 오류율(5xx) > 2% · 최소 100요청 · 2분 유지')).toBeTruthy();
  });

  it('잘린 목록은 잘렸다고 말한다(group 한도, 이벤트 200개, 평가 띠 앞부분)', async () => {
    groupsTruncated = true;
    const many = Array.from({ length: 205 }, (_, i) => ({
      at: new Date(NOW - (i + 1) * 60_000).toISOString(), group_hash: `h${String(i)}`, labels: { 'service.name': 'checkout' },
      from: 'OK', to: 'ALERT', opened: true, closed: false, episode_id: `e${String(i)}`, reason: null, value: 0.05,
    }));
    currentTimeline = { ...timeline, transitions: many, evaluations_truncated: true };
    renderAt(`/o/acme/monitors/${ID_B}?tz=UTC`);
    expect(await screen.findByText(/처음 2개만/)).toBeTruthy();
    expect(await screen.findByText(/최근 200개만 보입니다\(전체 208개\)/)).toBeTruthy();
    expect(screen.getByRole('img', { name: /앞부분은 표시 한도로 잘렸습니다/ })).toBeTruthy();
  });

  it('없는 monitor는 404 화면', async () => {
    renderAt('/o/acme/monitors/aaaaaaaa-0000-4000-8000-00000000dead?tz=UTC');
    expect(await screen.findByText('monitor를 찾을 수 없습니다')).toBeTruthy();
  });
});

describe('Monitor 편집 (S09)', () => {
  it('단계를 거쳐 오류율은 %→fraction으로, 미리보기(24시간 dry-run) 뒤 Idempotency-Key로 저장한다', async () => {
    const router = renderAt('/o/acme/monitors/new?tz=UTC');
    const user = userEvent.setup({ advanceTimers: () => undefined });
    // 1단계: 이름 없으면 다음으로 가지 않는다
    await user.click(await screen.findByRole('button', { name: '다음' }));
    expect(screen.getByText('이름을 입력하세요')).toBeTruthy();
    await user.type(screen.getByLabelText('이름'), 'checkout 오류율');
    await user.click(screen.getByRole('button', { name: '다음' }));
    await user.type(screen.getByLabelText('서비스(service.name)'), 'checkout');
    await user.click(screen.getByRole('button', { name: '다음' }));
    const threshold = screen.getByLabelText('임계값(%)');
    await user.clear(threshold);
    await user.type(threshold, '2.5');
    await user.click(screen.getByRole('button', { name: '다음' }));
    await user.click(screen.getByRole('button', { name: '다음' }));
    await user.selectOptions(await screen.findByLabelText('알림 policy'), POLICY);
    await user.click(screen.getByRole('button', { name: '다음' }));
    // 미리보기: 경고 설명, 후보 발화, 예측 아님
    expect(await screen.findByText('지난 24시간 미리보기')).toBeTruthy();
    expect(screen.getByText(/한 번 위반으로 바로 경보가 열립니다/)).toBeTruthy();
    expect(screen.getByText(/앞으로의 알림 횟수를 예측하지 않습니다/)).toBeTruthy();
    expect(screen.getByRole('img', { name: /1번 발화/ })).toBeTruthy();
    const validated = JSON.parse(String(calls('POST', '/monitors/validate')[0]?.init.body)) as Record<string, unknown>;
    expect(validated).toMatchObject({
      name: 'checkout 오류율',
      condition: { operator: 'gt', threshold: 0.025 },
      notification_policy_id: POLICY,
      query: { kind: 'error_ratio', filter: { op: 'and', args: [{ op: 'eq', field: 'service.name', value: 'checkout' }] } },
    });
    await user.click(screen.getByRole('button', { name: '저장' }));
    await waitFor(() => expect(router.state.location.pathname).toBe(`/o/acme/monitors/${ID_A}`));
    const created = calls('POST', '/api/v1/monitors').find((c) => !c.url.includes('validate'));
    expect((created?.init.headers as Record<string, string>)['Idempotency-Key']).toBe('idem-key-1');
  });

  it('서버 field violation은 그 단계로 돌아가 필드에 보인다', async () => {
    validateResponse = () =>
      json(400, { error: { code: 'INVALID_ARGUMENT', message: 'monitor 정의가 올바르지 않습니다', request_id: 'r', retryable: false, details: { field_violations: [{ field: 'window_seconds', reason: 'must be at most 86400' }] } } });
    renderAt('/o/acme/monitors/new?tz=UTC');
    const user = userEvent.setup({ advanceTimers: () => undefined });
    await user.type(await screen.findByLabelText('이름'), 'x');
    await user.click(screen.getByRole('button', { name: '6 미리보기·저장' }));
    expect(await screen.findByText('must be at most 86400')).toBeTruthy();
    expect(screen.getByRole('button', { name: '3 조건·window' }).getAttribute('aria-current')).toBe('step');
  });

  it('편집: 기존 값(% 표시)으로 시작하고, 다른 변경이 먼저 저장됐으면(412) 덮어쓰지 않는다', async () => {
    updateResponse = () => json(412, { error: { code: 'REVISION_MISMATCH', message: 'x', request_id: 'r', retryable: false } });
    renderAt(`/o/acme/monitors/${ID_B}/edit?tz=UTC`);
    const user = userEvent.setup({ advanceTimers: () => undefined });
    expect((await screen.findByLabelText<HTMLInputElement>('이름')).value).toBe('checkout 오류율');
    await user.click(screen.getByRole('button', { name: '3 조건·window' }));
    expect(screen.getByLabelText<HTMLInputElement>('임계값(%)').value).toBe('2');
    await user.click(screen.getByRole('button', { name: '6 미리보기·저장' }));
    await user.click(await screen.findByRole('button', { name: '변경 저장' }));
    expect(await screen.findByText('다른 사람이 이 monitor를 먼저 바꿨습니다. 덮어쓰지 않았습니다.')).toBeTruthy();
    const put = calls('PUT', `/monitors/${ID_B}`)[0];
    expect((put?.init.headers as Record<string, string>)['If-Match']).toBe('"3"');
  });

  it('미리보기를 못 하면 이유를 보이고 그래도 저장할 수 있다', async () => {
    validateResponse = () => json(200, { data: { normalized_spec: spec('x'), warnings: ['dry_run_unavailable'], dry_run: null }, meta });
    renderAt('/o/acme/monitors/new?tz=UTC');
    const user = userEvent.setup({ advanceTimers: () => undefined });
    await user.type(await screen.findByLabelText('이름'), 'x');
    await user.click(screen.getByRole('button', { name: '6 미리보기·저장' }));
    expect(await screen.findByText('이 배포에서는 24시간 미리보기를 할 수 없습니다.')).toBeTruthy();
    expect(screen.getByRole('button', { name: '저장' }).hasAttribute('disabled')).toBe(false);
  });

  it('form이 다루지 않는 범위 조건은 보여 주고 그대로 저장한다', async () => {
    const b = monitors[1];
    if (b === undefined) throw new Error('fixture');
    monitors[1] = { ...b, spec: { ...b.spec, query: { ...b.spec.query, filter: { op: 'and', args: [{ op: 'eq', field: 'service.name', value: 'checkout' }, { op: 'eq', field: 'http.route', value: '/pay' }] } } } };
    renderAt(`/o/acme/monitors/${ID_B}/edit?tz=UTC`);
    const user = userEvent.setup({ advanceTimers: () => undefined });
    await screen.findByLabelText('이름');
    await user.click(screen.getByRole('button', { name: '2 query·group' }));
    expect(screen.getByText(/이 화면에서 바꿀 수 없는 범위 조건 1개/)).toBeTruthy();
    expect(screen.getByText('http.route = "/pay"')).toBeTruthy();
    await user.click(screen.getByRole('button', { name: '6 미리보기·저장' }));
    await user.click(await screen.findByRole('button', { name: '변경 저장' }));
    await waitFor(() => expect(calls('PUT', `/monitors/${ID_B}`)).toHaveLength(1));
    const put = JSON.parse(String(calls('PUT', `/monitors/${ID_B}`)[0]?.init.body)) as { query: { filter: unknown } };
    expect(put.query.filter).toEqual({ op: 'and', args: [{ op: 'eq', field: 'service.name', value: 'checkout' }, { op: 'eq', field: 'http.route', value: '/pay' }] });
  });

  it('policy 목록을 못 불러와도 연결된 policy가 "알림 없음"으로 바뀌지 않는다', async () => {
    policiesResponse = () => json(503, { error: { code: 'UNAVAILABLE', message: '잠시 뒤 다시', request_id: 'r', retryable: true } });
    renderAt(`/o/acme/monitors/${ID_B}/edit?tz=UTC`);
    const user = userEvent.setup({ advanceTimers: () => undefined });
    await screen.findByLabelText('이름');
    await user.click(screen.getByRole('button', { name: '5 알림' }));
    expect(await screen.findByText(/알림 policy 목록을 불러오지 못했습니다/)).toBeTruthy();
    expect(screen.getByLabelText<HTMLSelectElement>('알림 policy').value).toBe(POLICY);
  });

  it('같은 Idempotency-Key에 다른 내용(409)이면 몰래 다시 보내지 않고, 확인 뒤 새 key로 저장한다', async () => {
    let n = 0;
    createResponse = () => (n++ === 0 ? json(409, { error: { code: 'CONFLICT', message: 'x', request_id: 'r', retryable: false } }) : json(201, { data: monitor(ID_A, 'new', null), meta }));
    let k = 0;
    vi.stubGlobal('crypto', { ...globalThis.crypto, randomUUID: () => `idem-key-${String(++k)}` });
    const router = renderAt('/o/acme/monitors/new?tz=UTC');
    const user = userEvent.setup({ advanceTimers: () => undefined });
    await user.type(await screen.findByLabelText('이름'), 'x');
    await user.click(screen.getByRole('button', { name: '6 미리보기·저장' }));
    await user.click(await screen.findByRole('button', { name: '저장' }));
    expect(await screen.findByText(/앞서 보낸 저장이 이미 처리됐을 수 있습니다/)).toBeTruthy();
    expect(screen.getByRole('link', { name: 'monitor 목록 보기' })).toBeTruthy();
    await user.click(screen.getByRole('button', { name: '확인했고, 새 monitor로 저장' }));
    await waitFor(() => expect(router.state.location.pathname).toBe(`/o/acme/monitors/${ID_A}`));
    const keys = calls('POST', '/api/v1/monitors').filter((c) => !c.url.includes('validate')).map((c) => (c.init.headers as Record<string, string>)['Idempotency-Key']);
    expect(keys).toHaveLength(2);
    expect(keys[0]).not.toBe(keys[1]);
  });
});
