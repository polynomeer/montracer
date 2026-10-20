import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { external } from '../../api/authState.ts';
import type { InvitationItem, MemberItem } from '../../api/members.ts';
import { routes } from '../../app/routes.tsx';
import { controlMeta } from '../../test/fixtures';
import { sessionMock } from '../../test/sessionMock';

const ACME = '7a1b0000-0000-4000-8000-00000000000a';
const ALICE = 'oidc:abcdef012345:alice';
const BOB = 'oidc:abcdef012345:bob';
const INV = 'cccccccc-0000-4000-8000-000000000001';
const TOKEN = `mtinv_${'A'.repeat(43)}`;

const json = (status: number, body: unknown, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } });
const err = (status: number, code: string, message = 'x', details?: Record<string, unknown>) =>
  json(status, { error: { code, message, request_id: 'r', retryable: false, ...(details === undefined ? {} : { details }) } });

const member = (over: Partial<MemberItem> = {}): MemberItem => ({
  user_id: ALICE,
  role: 'tenant_admin',
  display_name: 'Alice',
  created_at: '2026-10-01T00:00:00Z',
  last_login_at: '2026-10-20T01:00:00Z',
  is_self: true,
  revision: 1,
  ...over,
});
const invitation = (over: Partial<InvitationItem> = {}): InvitationItem => ({
  id: INV,
  role: 'developer',
  label: '결제팀',
  created_by: ALICE,
  created_at: '2026-10-20T00:00:00Z',
  expires_at: '2026-10-27T00:00:00Z',
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
let members: () => Response;
let patch: () => Response;
let remove: () => Response;
let create: () => Response;
let revoke: () => Response;
let assigned: string[];

const calls = (method: string, suffix: string) =>
  vi
    .mocked(fetch)
    .mock.calls.filter(([u, init]) => ((init as RequestInit | undefined)?.method ?? 'GET') === method && String(u).split('?')[0]?.endsWith(suffix))
    .map(([u, init]) => ({ url: String(u), init: init as RequestInit }));

beforeEach(() => {
  sessionMock.passThrough = true;
  session = () => json(200, sessionBody('tenant_admin', null));
  members = () => json(200, { data: [member(), member({ user_id: BOB, role: 'developer', display_name: null, is_self: false, last_login_at: null, revision: 3 })], next_cursor: null, meta: controlMeta() });
  patch = () => json(200, { data: member({ user_id: BOB, role: 'operator', display_name: null, is_self: false, revision: 4 }), meta: controlMeta() }, { ETag: '"4"' });
  remove = () => new Response(null, { status: 204 });
  create = () => json(201, { data: invitation({ role: 'viewer', label: null }), invitation_url: `https://app.example.com/invite#${TOKEN}`, meta: controlMeta() });
  revoke = () => new Response(null, { status: 204 });
  assigned = [];
  vi.spyOn(external, 'assign').mockImplementation((u: string) => {
    assigned.push(u);
  });
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET';
      const path = url.replace(/\?.*$/, '');
      if (path === '/api/v1/auth/session') return Promise.resolve(session());
      if (path === '/api/v1/members' && method === 'GET') return Promise.resolve(members());
      if ((path === `/api/v1/members/${encodeURIComponent(BOB)}` || path === `/api/v1/members/${encodeURIComponent(ALICE)}`) && method === 'PATCH') return Promise.resolve(patch());
      if (path === `/api/v1/members/${encodeURIComponent(BOB)}` && method === 'DELETE') return Promise.resolve(remove());
      if (path === '/api/v1/invitations' && method === 'GET') return Promise.resolve(json(200, { data: [invitation()], next_cursor: null, meta: controlMeta() }));
      if (path === '/api/v1/invitations' && method === 'POST') return Promise.resolve(create());
      if (path === `/api/v1/invitations/${INV}` && method === 'DELETE') return Promise.resolve(revoke());
      if (path === '/api/v1/auth/step-up') return Promise.resolve(json(200, { data: { authorization_url: 'https://idp.example.com/authorize?prompt=login' }, meta: controlMeta() }));
      return Promise.resolve(err(404, 'NOT_FOUND'));
    }),
  );
  vi.stubGlobal('crypto', { ...globalThis.crypto, randomUUID: () => 'idem-t-1' });
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function renderTeam() {
  const router = createMemoryRouter(routes, { initialEntries: ['/o/acme/settings/team'] });
  render(<RouterProvider router={router} />);
  return router;
}

describe('Team 화면', () => {
  it('멤버·대기 중 초대를 보이고, Admin 메뉴에 Team이 있다', async () => {
    renderTeam();
    const table = await screen.findByRole('table', { name: '조직 멤버(user id 순)' });
    expect(within(table).getByText('Alice')).toBeTruthy();
    expect(within(table).getByText('(나)')).toBeTruthy();
    // 이름이 없으면 user id
    expect(within(table).getByText(BOB)).toBeTruthy();
    expect(within(table).getByText('로그인 기록 없음')).toBeTruthy();
    const pending = await screen.findByRole('table', { name: '대기 중인 초대(만든 순)' });
    expect(within(pending).getByText('결제팀')).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Team' })).toBeTruthy();
    // step-up이 없으면 안내
    expect(screen.getByText('멤버를 바꾸려면 추가 인증이 필요합니다')).toBeTruthy();
  });

  it('역할 변경은 If-Match로 보내고, step-up이 없다는 403이면 재인증으로 안내한다', async () => {
    patch = () => err(403, 'FORBIDDEN', 'step-up', { step_up_required: true });
    renderTeam();
    const select = await screen.findByLabelText(`${BOB} 역할`);
    await userEvent.selectOptions(select, 'operator');
    // 고르기만으로는 보내지 않는다 — 적용을 눌러야 한다
    expect(calls('PATCH', `/members/${encodeURIComponent(BOB)}`)).toHaveLength(0);
    await userEvent.click(screen.getByRole('button', { name: '적용' }));
    await screen.findByText('이 작업에는 추가 인증이 필요합니다');
    const sent = calls('PATCH', `/members/${encodeURIComponent(BOB)}`)[0];
    expect(sent?.init.headers).toMatchObject({ 'If-Match': '"3"', 'X-CSRF-Token': 'csrf-1' });
    expect(JSON.parse(String(sent?.init.body))).toEqual({ role: 'operator' });
    // 추가 인증: IdP 재인증 URL로 이동, 돌아올 곳은 지금 화면
    await userEvent.click(screen.getAllByRole('button', { name: '추가 인증(MFA)' })[0] as HTMLElement);
    await waitFor(() => expect(assigned).toEqual(['https://idp.example.com/authorize?prompt=login']));
    expect(JSON.parse(String(calls('POST', '/auth/step-up')[0]?.init.body))).toEqual({ return_to: '/o/acme/settings/team' });
  });

  it('step-up 뒤에는 역할을 바꾸고, 412·409는 이유를 보인다', async () => {
    session = () => json(200, sessionBody('tenant_admin', '2099-01-01T00:00:00Z'));
    renderTeam();
    expect(await screen.findByText(/추가 인증이 .*까지 유효합니다/)).toBeTruthy();
    const pick = async (role: string) => {
      await userEvent.selectOptions(screen.getByLabelText(`${BOB} 역할`), role);
      await userEvent.click(screen.getByRole('button', { name: '적용' }));
    };
    await screen.findByLabelText(`${BOB} 역할`);
    await pick('operator');
    await screen.findByText(`${BOB}의 역할을 Operator(으)로 바꿨습니다.`);
    // 적용 뒤 focus는 역할 선택으로 돌아온다
    expect(document.activeElement).toBe(screen.getByLabelText(`${BOB} 역할`));
    patch = () => err(412, 'REVISION_MISMATCH');
    await pick('viewer');
    await screen.findByText('다른 관리자가 먼저 바꿨습니다. 목록을 새로 불러온 뒤 다시 시도하세요.');
    patch = () => err(409, 'CONFLICT', '조직에는 Tenant Admin이 한 명 이상 있어야 합니다');
    await pick('viewer');
    await screen.findByText('조직에는 Tenant Admin이 한 명 이상 있어야 합니다');
  });

  it('빼기는 확인을 받은 뒤 보낸다', async () => {
    session = () => json(200, sessionBody('tenant_admin', '2099-01-01T00:00:00Z'));
    renderTeam();
    const table = await screen.findByRole('table', { name: '조직 멤버(user id 순)' });
    const bobRow = within(table).getByText(BOB).closest('tr') as HTMLElement;
    await userEvent.click(within(bobRow).getByRole('button', { name: '빼기…' }));
    expect(calls('DELETE', `/members/${encodeURIComponent(BOB)}`)).toHaveLength(0);
    await userEvent.click(within(bobRow).getByRole('button', { name: '취소' }));
    await userEvent.click(within(bobRow).getByRole('button', { name: '빼기…' }));
    await userEvent.click(within(bobRow).getByRole('button', { name: '빼기' }));
    await screen.findByText(`${BOB}을(를) 조직에서 뺐습니다.`);
    expect(calls('DELETE', `/members/${encodeURIComponent(BOB)}`)[0]?.init.headers).toMatchObject({ 'If-Match': '"3"' });
  });

  it('초대 링크는 한 번만 보이고, 닫으면 사라진다. 폐기도 할 수 있다', async () => {
    session = () => json(200, sessionBody('tenant_admin', '2099-01-01T00:00:00Z'));
    renderTeam();
    await userEvent.selectOptions(await screen.findByLabelText('역할'), 'viewer');
    await userEvent.click(screen.getByRole('button', { name: '초대 링크 만들기' }));
    const link = await screen.findByLabelText('초대 링크');
    expect((link as HTMLInputElement).value).toBe(`https://app.example.com/invite#${TOKEN}`);
    const sent = calls('POST', '/invitations')[0];
    expect(sent?.init.headers).toMatchObject({ 'Idempotency-Key': 'idem-t-1' });
    expect(JSON.parse(String(sent?.init.body))).toEqual({ role: 'viewer' });
    await userEvent.click(screen.getByRole('button', { name: '닫기' }));
    expect(screen.queryByLabelText('초대 링크')).toBeNull();
    expect(document.body.textContent).not.toContain(TOKEN);
    // 폐기
    const pending = await screen.findByRole('table', { name: '대기 중인 초대(만든 순)' });
    await userEvent.click(within(pending).getByRole('button', { name: '폐기' }));
    await screen.findByText('초대를 폐기했습니다. 그 링크로는 더 들어올 수 없습니다.');
  });

  it('키보드로 역할을 훑어도 보내지 않고, 내 역할 변경은 권한 상실을 알린 뒤 성공하면 session을 다시 읽는다', async () => {
    session = () => json(200, sessionBody('tenant_admin', '2099-01-01T00:00:00Z'));
    patch = () => json(200, { data: member({ role: 'security_auditor', revision: 2 }), meta: controlMeta() }, { ETag: '"2"' });
    vi.mocked(fetch).mockClear();
    renderTeam();
    const bob = await screen.findByLabelText(`${BOB} 역할`);
    bob.focus();
    await userEvent.keyboard('{ArrowDown}{ArrowDown}');
    expect(calls('PATCH', `/members/${encodeURIComponent(BOB)}`)).toHaveLength(0);
    const own = screen.getByLabelText('Alice 역할');
    await userEvent.selectOptions(own, 'security_auditor');
    const confirm = screen.getByRole('group', { name: 'Alice 역할 바꾸기 확인' });
    expect(within(confirm).getByText(/관리 권한을 잃을 수 있습니다/)).toBeTruthy();
    // 확인이 열리면 focus는 적용 버튼으로
    expect(document.activeElement).toBe(within(confirm).getByRole('button', { name: '적용' }));
    await userEvent.click(within(confirm).getByRole('button', { name: '취소' }));
    expect((own as HTMLSelectElement).value).toBe('tenant_admin');
    expect(document.activeElement).toBe(own);
    expect(calls('PATCH', `/members/${encodeURIComponent(ALICE)}`)).toHaveLength(0);
    await userEvent.selectOptions(own, 'security_auditor');
    await userEvent.click(screen.getByRole('button', { name: '적용' }));
    await waitFor(() => expect(assigned).toEqual(['/']));
  });

  it('Tenant Admin으로 올릴 때는 그 권한을 알린다', async () => {
    session = () => json(200, sessionBody('tenant_admin', '2099-01-01T00:00:00Z'));
    renderTeam();
    await userEvent.selectOptions(await screen.findByLabelText(`${BOB} 역할`), 'tenant_admin');
    expect(screen.getByText(`${BOB}에게 Tenant Admin(멤버·key·삭제 관리) 권한을 줍니다.`)).toBeTruthy();
  });

  it('멤버 API가 꺼져 있으면(404) 쓸 수 없다고 알린다', async () => {
    members = () => err(404, 'NOT_FOUND');
    renderTeam();
    await screen.findByText('이 배포에서는 멤버 관리를 쓸 수 없습니다');
  });

  it('목록을 불러오지 못하면 비어 있다고 하지 않고, 다시 불러오기에 실패해도 마지막 목록을 남긴다', async () => {
    let fail = true;
    members = () => (fail ? err(503, 'UNAVAILABLE') : json(200, { data: [member()], next_cursor: null, meta: controlMeta() }));
    renderTeam();
    await screen.findByText('멤버 목록을 불러오지 못했습니다. 멤버가 없다는 뜻이 아닙니다.');
    fail = false;
    await userEvent.click(screen.getByRole('button', { name: '새로고침' }));
    await screen.findByRole('table', { name: '조직 멤버(user id 순)' });
    fail = true;
    await userEvent.click(screen.getByRole('button', { name: '새로고침' }));
    await screen.findByText(/마지막 성공/);
    expect(screen.getByRole('table', { name: '조직 멤버(user id 순)' })).toBeTruthy();
  });

  it('1,000명이 넘으면 수를 "이상"으로 보인다', async () => {
    members = () => json(200, { data: [member()], next_cursor: 'c1', meta: controlMeta() });
    renderTeam();
    expect(await screen.findByText('멤버 1명 이상')).toBeTruthy();
  });

  it('초대 만들기가 실패하면 쓴 메모를 남긴다', async () => {
    session = () => json(200, sessionBody('tenant_admin', '2099-01-01T00:00:00Z'));
    create = () => err(409, 'CONFLICT', '대기 중인 초대가 너무 많습니다. 쓰지 않는 초대를 폐기하세요');
    renderTeam();
    await userEvent.type(await screen.findByLabelText(/메모/), '결제팀');
    await userEvent.click(screen.getByRole('button', { name: '초대 링크 만들기' }));
    await screen.findByText('대기 중인 초대가 너무 많습니다. 쓰지 않는 초대를 폐기하세요');
    expect((screen.getByLabelText(/메모/) as HTMLInputElement).value).toBe('결제팀');
  });

  it('API key 모드(session 없음)에서는 Admin 메뉴를 숨긴다', async () => {
    sessionMock.passThrough = false;
    renderTeam();
    await screen.findByRole('table', { name: '조직 멤버(user id 순)' });
    expect(screen.queryByRole('link', { name: 'Team' })).toBeNull();
  });

  it('Security Auditor는 보기만 한다(변경 버튼 없음)', async () => {
    session = () => json(200, sessionBody('security_auditor', null));
    renderTeam();
    await screen.findByRole('table', { name: '조직 멤버(user id 순)' });
    expect(screen.getByText('Security Auditor는 멤버를 볼 수만 있습니다. 바꾸기는 Tenant Admin이 합니다.')).toBeTruthy();
    expect(screen.queryByRole('button', { name: '빼기…' })).toBeNull();
    expect(screen.queryByRole('button', { name: '초대 링크 만들기' })).toBeNull();
    expect(screen.queryByText('멤버를 바꾸려면 추가 인증이 필요합니다')).toBeNull();
  });

  it('권한이 없으면(403) 이유를 보이고, 조회자에게는 Admin 메뉴가 없다', async () => {
    session = () => json(200, sessionBody('viewer', null));
    members = () => err(403, 'FORBIDDEN');
    renderTeam();
    await screen.findByText('멤버 목록을 볼 권한이 없습니다');
    expect(screen.queryByRole('link', { name: 'Team' })).toBeNull();
  });
});
