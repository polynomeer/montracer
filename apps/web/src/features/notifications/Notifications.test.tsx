import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PolicyItem, TestItem } from '../../api/notificationPolicies.ts';
import { routes } from '../../app/routes.tsx';
import { testPoll } from './PolicyDetail.tsx';
import { testResultText, validatePolicyInput } from './policyText.ts';

const meta = { request_id: 'req-p', schema_version: 1, warnings: [] as string[] };
const ID = 'bbbbbbbb-0000-4000-8000-000000000001';
const SECRET = 'whsec_AAAABBBBCCCC';

const policy = (over: Partial<PolicyItem> = {}): PolicyItem => ({
  id: ID,
  name: 'On-call',
  enabled: true,
  revision: 3,
  webhook: { url: 'https://hooks.example.com', signing_key_id: 'wk_cur', previous_signing_key_id: null, previous_signing_key_expires_at: null },
  created_at: '2026-10-10T00:00:00Z',
  updated_at: '2026-10-12T00:00:00Z',
  created_by: 'admin',
  updated_by: 'admin',
  ...over,
});

const json = (status: number, body: unknown, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } });
const err = (status: number, code: string, message = 'x', details?: unknown) =>
  json(status, { error: { code, message, request_id: 'r', retryable: false, ...(details === undefined ? {} : { details }) } }, status === 429 ? { 'Retry-After': '300' } : {});
const calls = (method: string, suffix: string) =>
  vi
    .mocked(fetch)
    .mock.calls.filter(([u, init]) => ((init as RequestInit | undefined)?.method ?? 'GET') === method && String(u).split('?')[0]?.endsWith(suffix))
    .map(([u, init]) => ({ url: String(u), init: init as RequestInit }));

let list: () => Response;
let create: () => Response;
let update: () => Response;
let rotate: () => Response;
let remove: () => Response;
let test: () => Response;
let testReads: TestItem[];
let current: () => PolicyItem;
let testRead: () => Response | null;

const pendingTest: TestItem = { id: 'cccccccc-0000-4000-8000-000000000001', policy_id: ID, status: 'pending', attempts: 0, last_status_code: null, last_error: null, created_at: '2026-10-13T06:00:00Z', finished_at: null };

beforeEach(() => {
  testPoll.ms = 1;
  list = () => json(200, { data: [policy(), policy({ id: 'bbbbbbbb-0000-4000-8000-000000000002', name: 'Slack bridge', enabled: false, webhook: { url: 'https://slack.example.com', signing_key_id: 'wk_s', previous_signing_key_id: 'wk_old', previous_signing_key_expires_at: '2026-10-14T00:00:00Z' } })], next_cursor: null, meta });
  create = () => json(201, { data: policy({ revision: 1 }), signing_secret: SECRET, meta });
  update = () => json(200, { data: policy({ revision: 4, name: 'Renamed' }), meta });
  rotate = () => json(200, { data: policy({ revision: 4, webhook: { url: 'https://hooks.example.com', signing_key_id: 'wk_new', previous_signing_key_id: 'wk_cur', previous_signing_key_expires_at: '2026-10-14T06:00:00Z' } }), signing_secret: SECRET, meta });
  remove = () => new Response(null, { status: 204 });
  test = () => json(202, { data: pendingTest, meta });
  testReads = [{ ...pendingTest, status: 'delivered', attempts: 1, last_status_code: 204, finished_at: '2026-10-13T06:00:01Z' }];
  current = () => policy();
  testRead = () => null;
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET';
      const path = url.replace(/\?.*$/, '');
      if (path === '/api/v1/notification-policies' && method === 'GET') return Promise.resolve(list());
      if (path === '/api/v1/notification-policies' && method === 'POST') return Promise.resolve(create());
      if (path === `/api/v1/notification-policies/${ID}` && method === 'GET') return Promise.resolve(json(200, { data: current(), meta }));
      if (path === `/api/v1/notification-policies/${ID}` && method === 'PUT') return Promise.resolve(update());
      if (path === `/api/v1/notification-policies/${ID}` && method === 'DELETE') return Promise.resolve(remove());
      if (path === `/api/v1/notification-policies/${ID}/rotate-secret`) return Promise.resolve(rotate());
      if (path === `/api/v1/notification-policies/${ID}/test`) return Promise.resolve(test());
      if (path.startsWith(`/api/v1/notification-policies/${ID}/tests/`)) return Promise.resolve(testRead() ?? json(200, { data: testReads.shift() ?? pendingTest, meta }));
      return Promise.resolve(err(404, 'NOT_FOUND'));
    }),
  );
  vi.stubGlobal('crypto', { ...globalThis.crypto, randomUUID: () => 'idem-p-1' });
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function renderAt(path: string) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return router;
}

describe('알림 policy 표시', () => {
  it('입력 검사: 빈 이름·https 아닌 주소, 수정에서는 주소를 비워도 된다', () => {
    expect(validatePolicyInput(' ', 'https://x', true).name).toBeDefined();
    expect(validatePolicyInput('a', 'http://x', true).url).toBe('https:// 주소만 받습니다');
    expect(validatePolicyInput('a', '', true).url).toBeDefined();
    expect(validatePolicyInput('a', '', false)).toEqual({});
  });
  it('시험 결과: 보내는 중을 성공으로, 실패를 진행으로 보이지 않는다', () => {
    expect(testResultText(pendingTest)).toEqual({ tone: 'info', text: '보내는 중…' });
    expect(testResultText({ ...pendingTest, status: 'failed', last_status_code: 503, last_error: 'http_status' }).text).toBe('실패: 2xx가 아닌 응답 · 응답 503');
    expect(testResultText({ ...pendingTest, status: 'failed', last_error: 'destination_blocked' }).tone).toBe('critical');
    expect(testResultText({ ...pendingTest, status: 'failed', last_error: 'new_code' }).text).toBe('실패: new_code');
  });
});

describe('알림 policy 목록', () => {
  it('이름·켜짐·목적지(host)·서명 key·교체 중을 보인다', async () => {
    renderAt('/o/acme/notifications?tz=UTC');
    const table = await screen.findByRole('table', { name: /알림 policy 목록/ });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows[0]?.textContent).toContain('On-call');
    expect(rows[0]?.textContent).toContain('켜짐');
    expect(rows[0]?.textContent).toContain('https://hooks.example.com');
    expect(rows[1]?.textContent).toContain('꺼짐');
    expect(rows[1]?.textContent).toContain('교체 중');
    expect(screen.getByRole('link', { name: 'Notifications' })).toBeTruthy();
  });
  it('API가 꺼진 배포(404)·조회 실패는 "없음"이 아니다', async () => {
    list = () => err(404, 'NOT_FOUND');
    renderAt('/o/acme/notifications?tz=UTC');
    expect(await screen.findByText('이 배포에서는 알림을 쓸 수 없습니다')).toBeTruthy();
    expect(screen.queryByText('아직 알림 policy가 없습니다')).toBeNull();
  });
  it('200개를 넘으면 일부만 보인다고 적는다', async () => {
    list = () => json(200, { data: [policy()], next_cursor: 'c2', meta });
    renderAt('/o/acme/notifications?tz=UTC');
    expect(await screen.findByText('처음 200개만 보입니다(이름순).')).toBeTruthy();
  });
  it('조회 실패는 실패로, 비었으면 만들기 안내', async () => {
    list = () => err(503, 'UNAVAILABLE');
    renderAt('/o/acme/notifications?tz=UTC');
    expect(await screen.findByText('policy 목록을 불러오지 못했습니다. policy가 없다는 뜻이 아닙니다.')).toBeTruthy();
  });
});

describe('알림 policy 만들기', () => {
  it('Idempotency-Key로 만들고 secret을 한 번만 보인 뒤 상세로 간다', async () => {
    const router = renderAt('/o/acme/notifications/new?tz=UTC');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '만들기' }));
    expect(screen.getByText('이름을 1~200자로 입력하세요')).toBeTruthy();
    expect(calls('POST', '/notification-policies')).toHaveLength(0);
    await user.type(screen.getByLabelText('이름'), 'On-call');
    await user.type(screen.getByLabelText('webhook 주소'), 'https://hooks.example.com/T0/B0/token');
    await user.click(screen.getByRole('button', { name: '만들기' }));
    const secret = await screen.findByLabelText(/secret \(key id wk_cur\)/);
    expect((secret as HTMLInputElement).value).toBe(SECRET);
    expect(screen.getByText(/지금만 볼 수 있습니다/)).toBeTruthy();
    const post = calls('POST', '/notification-policies')[0];
    expect((post?.init.headers as Record<string, string>)['Idempotency-Key']).toBe('idem-p-1');
    expect(JSON.parse(String(post?.init.body))).toEqual({ name: 'On-call', enabled: true, webhook: { url: 'https://hooks.example.com/T0/B0/token' } });
    await user.click(screen.getByRole('button', { name: '저장했습니다 — policy 보기' }));
    await waitFor(() => expect(router.state.location.pathname).toBe(`/o/acme/notifications/${ID}`));
    expect(screen.queryByDisplayValue(SECRET)).toBeNull();
  });
  it('재생 응답(secret 없음)은 다시 볼 수 없다고 하고, 권한·필드 오류를 보인다', async () => {
    create = () => json(201, { data: policy(), meta: { ...meta, warnings: ['signing_secret_not_replayed'] } });
    renderAt('/o/acme/notifications/new?tz=UTC');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('이름'), 'x');
    await user.type(screen.getByLabelText('webhook 주소'), 'https://hooks.example.com/x');
    await user.click(screen.getByRole('button', { name: '만들기' }));
    expect(await screen.findByText('이미 만들어진 policy입니다')).toBeTruthy();
  });
  it('Developer(403)·주소 거절(400)', async () => {
    create = () => err(400, 'INVALID_ARGUMENT', 'notification policy가 올바르지 않습니다', { field_violations: [{ field: 'webhook.url', reason: 'host must not be private' }] });
    renderAt('/o/acme/notifications/new?tz=UTC');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('이름'), 'x');
    await user.type(screen.getByLabelText('webhook 주소'), 'https://10.0.0.1/x');
    await user.click(screen.getByRole('button', { name: '만들기' }));
    expect(await screen.findByText('host must not be private')).toBeTruthy();
    create = () => err(403, 'FORBIDDEN');
    await user.click(screen.getByRole('button', { name: '만들기' }));
    expect(await screen.findByText('Tenant Admin만 알림 policy를 만들 수 있습니다.')).toBeTruthy();
  });
});

describe('알림 policy 상세', () => {
  it('주소를 비우고 저장하면 webhook 없이 If-Match로 보낸다. 412면 덮어쓰지 않는다', async () => {
    renderAt(`/o/acme/notifications/${ID}?tz=UTC`);
    const user = userEvent.setup();
    const name = await screen.findByLabelText('이름');
    expect(screen.getByText(/지금 목적지: https:\/\/hooks.example.com/)).toBeTruthy();
    await user.clear(name);
    await user.type(name, 'Renamed');
    await user.click(screen.getByRole('button', { name: '변경 저장' }));
    expect(await screen.findByText(/저장했습니다/)).toBeTruthy();
    const put = calls('PUT', `/notification-policies/${ID}`)[0];
    expect(JSON.parse(String(put?.init.body))).toEqual({ name: 'Renamed', enabled: true });
    expect((put?.init.headers as Record<string, string>)['If-Match']).toBe('"3"');
    update = () => err(412, 'REVISION_MISMATCH');
    await user.type(screen.getByLabelText('새 webhook 주소(바꿀 때만)'), 'https://new.example.com/h');
    await user.click(screen.getByRole('button', { name: '변경 저장' }));
    expect(await screen.findByText('다른 변경이 먼저 저장되었습니다. 덮어쓰지 않았습니다.')).toBeTruthy();
    expect(JSON.parse(String(calls('PUT', `/notification-policies/${ID}`)[1]?.init.body))).toMatchObject({ webhook: { url: 'https://new.example.com/h' } });
  });

  it('시험 전송: 보내는 중에서 결과까지 다시 읽어 응답 code를 보인다', async () => {
    renderAt(`/o/acme/notifications/${ID}?tz=UTC`);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '시험 보내기' }));
    expect(await screen.findByText('전달됨 · 응답 204')).toBeTruthy();
    expect(calls('POST', '/test')).toHaveLength(1);
  });

  it('시험 전송 실패는 이유와 함께, 한도(429)는 다시 할 시간과 함께', async () => {
    testReads = [{ ...pendingTest, status: 'failed', attempts: 1, last_status_code: null, last_error: 'destination_blocked', finished_at: '2026-10-13T06:00:01Z' }];
    renderAt(`/o/acme/notifications/${ID}?tz=UTC`);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '시험 보내기' }));
    expect(await screen.findByText('실패: 사설·내부 주소로 풀려 막았습니다')).toBeTruthy();
    test = () => err(429, 'RATE_LIMITED');
    await user.click(screen.getByRole('button', { name: '시험 보내기' }));
    expect(await screen.findByText(/1시간에 20번까지입니다 \(5분 뒤 다시\)/)).toBeTruthy();
  });

  it('secret 교체: 대화(focus·Esc), 바로 버리기는 0초로, 새 secret을 한 번 보인다', async () => {
    renderAt(`/o/acme/notifications/${ID}?tz=UTC`);
    const user = userEvent.setup();
    const trigger = await screen.findByRole('button', { name: 'secret 교체' });
    await user.click(trigger);
    const dialog = screen.getByRole('alertdialog', { name: /서명 secret을 교체할까요/ });
    expect(document.activeElement).toBe(within(dialog).getAllByRole('radio')[0]);
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('alertdialog')).toBeNull();
    expect(document.activeElement).toBe(trigger);
    await user.click(trigger);
    await user.click(screen.getByLabelText(/이전 secret을 바로 버리기/));
    await user.click(screen.getByRole('button', { name: '교체' }));
    expect(((await screen.findByLabelText(/secret \(key id wk_new\)/)) as HTMLInputElement).value).toBe(SECRET);
    const post = calls('POST', '/rotate-secret')[0];
    expect(JSON.parse(String(post?.init.body))).toEqual({ previous_secret_expires_in_seconds: 0 });
    expect((post?.init.headers as Record<string, string>)['If-Match']).toBe('"3"');
    expect(screen.getByText(/wk_cur/).closest('dd')?.textContent).toContain('10월 14일 06:00:00까지 함께 서명합니다');
    await user.click(screen.getByRole('button', { name: '저장했습니다 — 닫기' }));
    expect(screen.queryByDisplayValue(SECRET)).toBeNull();
  });

  it('삭제: monitor가 쓰면(409) 이유를 보이고, 지우면 목록으로', async () => {
    remove = () => err(409, 'CONFLICT');
    const router = renderAt(`/o/acme/notifications/${ID}?tz=UTC`);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'policy 삭제' }));
    await user.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: '삭제' }));
    expect(await screen.findByText(/이 policy를 쓰는 monitor가 있습니다/)).toBeTruthy();
    remove = () => new Response(null, { status: 204 });
    await user.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: '삭제' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/o/acme/notifications'));
    expect((calls('DELETE', `/notification-policies/${ID}`)[1]?.init.headers as Record<string, string>)['If-Match']).toBe('"3"');
  });

  it('412 뒤 최신 내용을 불러오면 form이 서버 값으로 바뀐다(예전 입력으로 덮어쓰지 않는다)', async () => {
    update = () => err(412, 'REVISION_MISMATCH');
    renderAt(`/o/acme/notifications/${ID}?tz=UTC`);
    const user = userEvent.setup();
    const name = await screen.findByLabelText('이름');
    await user.clear(name);
    await user.type(name, 'Mine');
    await user.click(screen.getByRole('button', { name: '변경 저장' }));
    current = () => policy({ revision: 5, name: 'Theirs', enabled: false });
    await user.click(await screen.findByRole('button', { name: '최신 내용 불러오기' }));
    await waitFor(() => expect(screen.getByLabelText<HTMLInputElement>('이름').value).toBe('Theirs'));
    expect(screen.getByRole<HTMLInputElement>('checkbox').checked).toBe(false);
  });

  it('시험 결과를 읽지 못하면(404) 보내는 중이 아니라 확인하지 못했다고 한다', async () => {
    testRead = () => err(404, 'NOT_FOUND', '시험 전송을 찾을 수 없습니다');
    renderAt(`/o/acme/notifications/${ID}?tz=UTC`);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '시험 보내기' }));
    expect(await screen.findByText('시험 결과를 확인하지 못했습니다: 시험 전송을 찾을 수 없습니다')).toBeTruthy();
    expect(screen.queryByText('보내는 중…')).toBeNull();
  });

  it('새 secret이 나타나면 그 제목으로, 닫으면 교체 버튼으로 focus가 간다', async () => {
    renderAt(`/o/acme/notifications/${ID}?tz=UTC`);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'secret 교체' }));
    await user.click(screen.getByRole('button', { name: '교체' }));
    await screen.findByLabelText(/secret \(key id wk_new\)/);
    expect(document.activeElement?.textContent).toBe('서명 secret — 지금만 볼 수 있습니다');
    await user.click(screen.getByRole('button', { name: '저장했습니다 — 닫기' }));
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'secret 교체' }));
  });

  it('없는 policy는 404 화면', async () => {
    renderAt('/o/acme/notifications/bbbbbbbb-0000-4000-8000-00000000dead?tz=UTC');
    expect(await screen.findByText('알림 policy를 찾을 수 없습니다')).toBeTruthy();
  });
});
