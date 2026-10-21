import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { keyStatus, nextStatusChange, type KeyItem } from '../../api/keys.ts';
import { routes } from '../../app/routes.tsx';
import { controlMeta } from '../../test/fixtures';
import { sessionMock } from '../../test/sessionMock';
import { parseEnvironments, validateKeyForm } from './AccessSettings.tsx';

const ACME = '7a1b0000-0000-4000-8000-00000000000a';
const ALICE = 'oidc:abcdef012345:alice';
const KEY_ID = '0123456789abcdef';
const TOKEN = `mti_fedcba9876543210_${'C'.repeat(43)}`;

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const err = (status: number, code: string, message = 'x', details?: Record<string, unknown>) =>
  json(status, { error: { code, message, request_id: 'r', retryable: false, ...(details === undefined ? {} : { details }) } });

const key = (over: Partial<KeyItem> = {}): KeyItem => ({
  key_id: KEY_ID,
  name: 'checkout collector',
  kind: 'ingest',
  scopes: ['ingest.traces'],
  environments: ['prod'],
  issued_by: ALICE,
  created_at: '2026-10-01T00:00:00Z',
  expires_at: '2099-01-01T00:00:00Z',
  revoked_at: null,
  token_hint: `mti_${KEY_ID}_…`,
  ...over,
});

function sessionBody(role: string, stepUp: string | null) {
  return {
    data: {
      user_id: ALICE,
      display_name: 'Alice',
      tenant_id: ACME,
      tenant_name: 'acme',
      role,
      memberships: [{ tenant_id: ACME, tenant_name: 'acme', role }],
      csrf_token: 'csrf-1',
      idle_expires_at: '2026-10-20T10:30:00Z',
      expires_at: '2026-10-20T22:00:00Z',
      step_up_expires_at: stepUp,
    },
    meta: controlMeta(),
  };
}

let session: () => Response;
let list: () => Response;
let create: () => Response;
let revoke: () => Response;

const calls = (method: string, suffix: string) =>
  vi
    .mocked(fetch)
    .mock.calls.filter(([u, init]) => ((init as RequestInit | undefined)?.method ?? 'GET') === method && String(u).split('?')[0]?.endsWith(suffix))
    .map(([u, init]) => ({ url: String(u), init: init as RequestInit }));

beforeEach(() => {
  sessionMock.passThrough = true;
  session = () => json(200, sessionBody('tenant_admin', '2099-01-01T00:00:00Z'));
  list = () =>
    json(200, {
      data: [
        key(),
        key({ key_id: 'aaaaaaaaaaaaaaaa', name: null, kind: 'api', scopes: ['telemetry.read'], environments: [], token_hint: 'mta_aaaaaaaaaaaaaaaa_…', revoked_at: '2099-01-01T00:00:00Z' }),
        key({ key_id: 'bbbbbbbbbbbbbbbb', name: 'old', revoked_at: '2026-10-02T00:00:00Z', token_hint: 'mti_bbbbbbbbbbbbbbbb_…' }),
      ],
      next_cursor: null,
      meta: controlMeta(),
    });
  create = () => json(201, { data: key({ key_id: 'fedcba9876543210', token_hint: 'mti_fedcba9876543210_…' }), token: TOKEN, meta: controlMeta() });
  revoke = () => new Response(null, { status: 204 });
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET';
      const path = url.replace(/\?.*$/, '');
      if (path === '/api/v1/auth/session') return Promise.resolve(session());
      if (path === '/api/v1/keys' && method === 'GET') return Promise.resolve(list());
      if (path === '/api/v1/keys' && method === 'POST') return Promise.resolve(create());
      if (path === `/api/v1/keys/${KEY_ID}` && method === 'DELETE') return Promise.resolve(revoke());
      if (path === '/api/v1/auth/step-up') return Promise.resolve(json(200, { data: { authorization_url: 'https://idp.example.com/authorize' }, meta: controlMeta() }));
      return Promise.resolve(err(404, 'NOT_FOUND'));
    }),
  );
  vi.stubGlobal('crypto', { ...globalThis.crypto, randomUUID: () => 'idem-k-1' });
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function renderAccess() {
  const router = createMemoryRouter(routes, { initialEntries: ['/o/acme/settings/access'] });
  render(<RouterProvider router={router} />);
}

describe('Access 화면', () => {
  it('key 목록과 상태(유효·교체 중·폐기됨)를 보이고, 마지막 사용은 없다고 알린다', async () => {
    renderAccess();
    const table = await screen.findByRole('table', { name: 'API key(최신순)' });
    expect(within(table).getByText('checkout collector')).toBeTruthy();
    expect(within(table).getByText('유효')).toBeTruthy();
    expect(screen.getByText(/마지막 사용 시각은 아직 제공하지 않습니다/)).toBeTruthy();
    expect(within(table).getByText(/에 폐기$/)).toBeTruthy();
    expect(within(table).getByText('폐기됨')).toBeTruthy();
    expect(within(table).getByText('조직 전체')).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Access' })).toBeTruthy();
  });

  it('수집 key 발급: environment가 필요하고, 원문은 한 번만 보이며 닫으면 사라진다', async () => {
    renderAccess();
    await screen.findByRole('table', { name: 'API key(최신순)' });
    await userEvent.click(screen.getByRole('button', { name: 'key 발급' }));
    expect(await screen.findByText('수집 key는 environment가 하나 이상 필요합니다.')).toBeTruthy();
    expect(calls('POST', '/keys')).toHaveLength(0);
    // 오류는 그 입력에 묶이고, focus가 그 입력으로 간다
    const envInput = screen.getByLabelText(/environment/);
    expect(envInput.getAttribute('aria-describedby')).toBe('key-envs-error');
    expect(document.activeElement).toBe(envInput);
    await userEvent.type(screen.getByLabelText(/environment/), 'prod, staging');
    await userEvent.type(screen.getByLabelText(/이름/), 'checkout collector');
    await userEvent.click(screen.getByRole('button', { name: 'key 발급' }));
    const field = await screen.findByLabelText(/key \(mti_fedcba9876543210_…\)/);
    expect((field as HTMLInputElement).value).toBe(TOKEN);
    const sent = calls('POST', '/keys')[0];
    expect(sent?.init.headers).toMatchObject({ 'Idempotency-Key': 'idem-k-1', 'X-CSRF-Token': 'csrf-1' });
    expect(JSON.parse(String(sent?.init.body))).toEqual({
      kind: 'ingest',
      name: 'checkout collector',
      scopes: ['ingest.traces', 'ingest.metrics', 'ingest.logs'],
      environments: ['prod', 'staging'],
      expires_in_days: 365,
    });
    const stored = [window.localStorage, window.sessionStorage].flatMap((st) => Object.keys(st).map((k) => st.getItem(k) ?? ''));
    expect(stored.join('|')).not.toContain(TOKEN);
    expect(window.location.href).not.toContain(TOKEN);
    await userEvent.click(screen.getByRole('button', { name: '닫기' }));
    expect(screen.queryByDisplayValue(TOKEN)).toBeNull();
    expect(document.body.textContent).not.toContain(TOKEN);
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('heading', { name: 'key 발급' })));
  });

  it('응답을 잃고 다시 보내면 같은 Idempotency-Key이고, 재생 응답(원문 없음)이면 새로 발급하라고 알린다', async () => {
    let first = true;
    create = () => {
      if (first) {
        first = false;
        return err(503, 'UNAVAILABLE');
      }
      return json(201, { data: key({ key_id: 'fedcba9876543210', token_hint: 'mti_fedcba9876543210_…' }), meta: controlMeta() });
    };
    let n = 0;
    vi.stubGlobal('crypto', { ...globalThis.crypto, randomUUID: () => `idem-${String(++n)}` });
    renderAccess();
    await screen.findByRole('table', { name: 'API key(최신순)' });
    await userEvent.type(screen.getByLabelText(/environment/), 'prod');
    await userEvent.click(screen.getByRole('button', { name: 'key 발급' }));
    await screen.findByText('일시적으로 처리하지 못했습니다. 잠시 뒤 다시 시도하세요.');
    await userEvent.click(screen.getByRole('button', { name: 'key 발급' }));
    await screen.findByText('같은 요청을 다시 보내 key를 받지 못했습니다. 이 key를 폐기하고 새로 발급하세요.');
    const keysSent = calls('POST', '/keys').map((c) => (c.init.headers as Record<string, string>)['Idempotency-Key']);
    expect(keysSent).toEqual(['idem-1', 'idem-1']);
  });

  it('API key로 바꾸면 기본 scope·기간이 바뀌고, step-up이 없으면 재인증을 안내한다', async () => {
    session = () => json(200, sessionBody('tenant_admin', null));
    create = () => err(403, 'FORBIDDEN', 'step-up', { step_up_required: true });
    renderAccess();
    await screen.findByText('key를 발급·폐기하려면 추가 인증이 필요합니다');
    await userEvent.click(screen.getByLabelText(/API — 조회·자동화/));
    expect((screen.getByLabelText('유효 기간') as HTMLSelectElement).value).toBe('90');
    await userEvent.click(screen.getByRole('button', { name: 'key 발급' }));
    await screen.findByText('이 작업에는 추가 인증이 필요합니다');
    expect(JSON.parse(String(calls('POST', '/keys')[0]?.init.body))).toEqual({ kind: 'api', scopes: ['telemetry.read'], expires_in_days: 90 });
  });

  it('폐기는 확인 뒤 보내고, 교체 겹침(1시간)을 고를 수 있다', async () => {
    renderAccess();
    const table = await screen.findByRole('table', { name: 'API key(최신순)' });
    const row = within(table).getByText('checkout collector').closest('tr') as HTMLElement;
    await userEvent.click(within(row).getByRole('button', { name: '폐기…' }));
    const group = within(row).getByRole('group', { name: 'checkout collector 폐기 확인' });
    expect(document.activeElement).toBe(within(group).getByLabelText('폐기 시점'));
    await userEvent.selectOptions(within(group).getByLabelText('폐기 시점'), '3600');
    await userEvent.click(within(group).getByRole('button', { name: '폐기' }));
    expect(within(group).getByText(/이 key를 쓰는 수집기·SDK는 .*부터\(교체 겹침\) 401로 거절됩니다/)).toBeTruthy();
    await userEvent.click(within(group).getByRole('button', { name: '폐기' }));
    await screen.findByText('checkout collector의 폐기를 예약했습니다. 실제 폐기 시각은 목록의 상태를 보세요.');
    expect(calls('DELETE', `/keys/${KEY_ID}`)[0]?.url).toBe(`/api/v1/keys/${KEY_ID}?grace_seconds=3600`);
  });

  it('폐기 확인을 취소하면 "폐기…"로 focus가 돌아오고, step-up이 없으면 재인증을 안내한다', async () => {
    revoke = () => err(403, 'FORBIDDEN', 'step-up', { step_up_required: true });
    renderAccess();
    const table = await screen.findByRole('table', { name: 'API key(최신순)' });
    const row = within(table).getByText('checkout collector').closest('tr') as HTMLElement;
    await userEvent.click(within(row).getByRole('button', { name: '폐기…' }));
    await userEvent.click(within(row).getByRole('button', { name: '취소' }));
    await waitFor(() => expect(document.activeElement).toBe(within(row).getByRole('button', { name: '폐기…' })));
    await userEvent.click(within(row).getByRole('button', { name: '폐기…' }));
    await userEvent.click(within(row).getByRole('button', { name: '폐기' }));
    await screen.findByText('이 작업에는 추가 인증이 필요합니다');
  });

  it('새로고침이 실패하면 마지막 목록을 남기되 "유효"라고 단정하지 않는다', async () => {
    renderAccess();
    await screen.findByText('유효');
    list = () => err(503, 'UNAVAILABLE');
    await userEvent.click(screen.getByRole('button', { name: '새로고침' }));
    await screen.findByText(/마지막 성공/);
    expect(screen.getByText('유효(마지막 확인 기준)')).toBeTruthy();
    expect(screen.queryByText('유효')).toBeNull();
  });

  it('만료된 key는 만료됨이고 동작이 없다. 1,000개면 잘렸다고 알린다', async () => {
    list = () => json(200, { data: Array.from({ length: 1000 }, (_, i) => key({ key_id: i.toString(16).padStart(16, '0'), name: `k${String(i)}`, token_hint: 'mti_x_…', expires_at: '2020-01-01T00:00:00Z' })), next_cursor: null, meta: controlMeta() });
    renderAccess();
    expect(await screen.findByText('key 1,000개 이상')).toBeTruthy();
    expect(screen.getByText(/최근 1,000개만 보입니다/)).toBeTruthy();
    expect(screen.getAllByText('만료됨')).toHaveLength(1000);
    expect(screen.queryByRole('button', { name: '폐기…' })).toBeNull();
  });

  it('열어 둔 동안 폐기 시각이 지나면 상태가 바뀐다', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const soon = new Date(Date.now() + 60_000).toISOString();
    list = () => json(200, { data: [key({ revoked_at: soon })], next_cursor: null, meta: controlMeta() });
    renderAccess();
    expect(await screen.findByText(/에 폐기$/)).toBeTruthy();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(61_000);
    });
    expect(screen.getByText('폐기됨')).toBeTruthy();
    vi.useRealTimers();
  });

  it('권한이 없으면(403) 이유를 보이고, 꺼져 있으면(404) 쓸 수 없다고 알린다', async () => {
    list = () => err(403, 'FORBIDDEN');
    renderAccess();
    await screen.findByText('key를 볼 권한이 없습니다');
  });

  it('API가 꺼져 있으면(404) 쓸 수 없다고 알린다', async () => {
    list = () => err(404, 'NOT_FOUND');
    renderAccess();
    await screen.findByText('이 배포에서는 key 관리를 쓸 수 없습니다');
  });

  it('상한(409)은 서버 문장을 보이고, 쓴 이름을 남긴다', async () => {
    create = () => err(409, 'CONFLICT', '살아 있는 key가 너무 많습니다(100개). 쓰지 않는 key를 폐기하세요');
    renderAccess();
    await screen.findByRole('table', { name: 'API key(최신순)' });
    await userEvent.type(screen.getByLabelText(/environment/), 'prod');
    await userEvent.type(screen.getByLabelText(/이름/), 'x');
    await userEvent.click(screen.getByRole('button', { name: 'key 발급' }));
    await screen.findByText('살아 있는 key가 너무 많습니다(100개). 쓰지 않는 key를 폐기하세요');
    await waitFor(() => expect((screen.getByLabelText(/이름/) as HTMLInputElement).value).toBe('x'));
  });
});

describe('Access 순수 함수', () => {
  it('environment 목록을 나누고 형식이 틀린 값을 알린다', () => {
    expect(parseEnvironments(' prod, staging prod ')).toEqual({ envs: ['prod', 'staging'], invalid: null });
    expect(parseEnvironments('prod, -bad')).toEqual({ envs: ['prod'], invalid: '-bad' });
    expect(parseEnvironments('')).toEqual({ envs: [], invalid: null });
  });

  it('검증은 서버 규칙과 같다(이름 제어 문자·environment·scope)', () => {
    expect(validateKeyForm('ingest', 'a\tb', ['ingest.traces'], 'prod').errors.name).toMatch(/제어 문자/);
    expect(validateKeyForm('ingest', 'x'.repeat(65), ['ingest.traces'], 'prod').errors.name).toMatch(/64자/);
    expect(validateKeyForm('api', '', [], '').errors.scopes).toBeTruthy();
    expect(validateKeyForm('api', '', ['telemetry.read'], '').errors).toEqual({});
    expect(validateKeyForm('ingest', '', ['ingest.traces'], Array.from({ length: 21 }, (_, i) => `e${String(i)}`).join(',')).errors.envs).toMatch(/20개/);
  });

  it('상태는 서버 시각과 지금을 비교만 하고, 만료가 폐기보다 먼저면 만료로 본다. 시각을 못 읽으면 확인할 수 없음', () => {
    const now = Date.parse('2026-10-21T00:00:00Z');
    expect(keyStatus(key({ expires_at: '2026-10-22T00:00:00Z' }), now)).toEqual({ kind: 'valid' });
    expect(keyStatus(key({ expires_at: '2026-10-21T00:30:00Z', revoked_at: '2026-10-21T01:00:00Z' }), now)).toEqual({ kind: 'valid' });
    expect(keyStatus(key({ expires_at: '2026-10-20T00:30:00Z', revoked_at: '2026-10-21T01:00:00Z' }), now).kind).toBe('expired');
    expect(keyStatus(key({ expires_at: 'nope' }), now)).toEqual({ kind: 'unknown' });
    expect(nextStatusChange([key({ expires_at: '2026-10-22T00:00:00Z', revoked_at: '2026-10-21T01:00:00Z' })], now)).toBe(Date.parse('2026-10-21T01:00:00Z'));
    expect(keyStatus(key({ expires_at: '2026-10-20T00:00:00Z' }), now).kind).toBe('expired');
    expect(keyStatus(key({ revoked_at: '2026-10-21T01:00:00Z' }), now).kind).toBe('revoking');
    expect(keyStatus(key({ revoked_at: '2026-10-20T01:00:00Z' }), now).kind).toBe('revoked');
  });
});
