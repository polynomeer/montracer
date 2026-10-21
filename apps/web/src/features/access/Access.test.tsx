import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { keyStatus, type KeyItem } from '../../api/keys.ts';
import { routes } from '../../app/routes.tsx';
import { controlMeta } from '../../test/fixtures';
import { sessionMock } from '../../test/sessionMock';
import { parseEnvironments } from './AccessSettings.tsx';

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
  it('key 목록과 상태(사용 중·교체 중·폐기됨)를 보이고 원문은 없다', async () => {
    renderAccess();
    const table = await screen.findByRole('table', { name: 'API key(최신순)' });
    expect(within(table).getByText('checkout collector')).toBeTruthy();
    expect(within(table).getByText('사용 중')).toBeTruthy();
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
    await userEvent.click(screen.getByRole('button', { name: '닫기' }));
    expect(document.body.textContent).not.toContain(TOKEN);
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
    await screen.findByText('checkout collector은(는) 1시간 뒤 폐기됩니다.');
    expect(calls('DELETE', `/keys/${KEY_ID}`)[0]?.url).toBe(`/api/v1/keys/${KEY_ID}?grace_seconds=3600`);
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

  it('상태는 서버 시각과 지금을 비교만 한다', () => {
    const now = Date.parse('2026-10-21T00:00:00Z');
    expect(keyStatus(key({ expires_at: '2026-10-22T00:00:00Z' }), now)).toEqual({ kind: 'active' });
    expect(keyStatus(key({ expires_at: '2026-10-20T00:00:00Z' }), now).kind).toBe('expired');
    expect(keyStatus(key({ revoked_at: '2026-10-21T01:00:00Z' }), now).kind).toBe('revoking');
    expect(keyStatus(key({ revoked_at: '2026-10-20T01:00:00Z' }), now).kind).toBe('revoked');
  });
});
