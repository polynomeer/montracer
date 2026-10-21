import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { external } from '../api/authState.ts';
import { controlMeta } from '../test/fixtures';
import { routes } from './routes.tsx';

const TOKEN = `mti_${'B'.repeat(43)}`;
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const err = (status: number, code: string) => json(status, { error: { code, message: 'x', request_id: 'r', retryable: false } });

let start: () => Response;
let assigned: string[];

beforeEach(() => {
  start = () => json(200, { data: { authorization_url: 'https://idp.example.com/authorize?state=s', tenant_name: 'acme', role: 'developer' }, meta: controlMeta() });
  assigned = [];
  vi.spyOn(external, 'assign').mockImplementation((u: string) => {
    assigned.push(u);
  });
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => Promise.resolve(url.endsWith('/api/v1/auth/invitation') ? start() : err(404, 'NOT_FOUND'))),
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  window.history.replaceState(null, '', '/');
});

function openInvite(hash: string) {
  window.history.replaceState(null, '', `/invite${hash}`);
  const router = createMemoryRouter(routes, { initialEntries: ['/invite'] });
  render(<RouterProvider router={router} />);
}

const sentBodies = () =>
  vi
    .mocked(fetch)
    .mock.calls.filter(([u]) => String(u).endsWith('/auth/invitation'))
    .map(([, init]) => JSON.parse(String((init as RequestInit).body)) as unknown);

describe('초대 수락 페이지', () => {
  it('fragment의 token을 본문으로 보내고 주소에서 지운 뒤, 조직·역할을 확인받고 로그인으로 보낸다', async () => {
    openInvite(`#${TOKEN}`);
    expect(await screen.findByText('acme')).toBeTruthy();
    expect(screen.getByText('Developer')).toBeTruthy();
    expect(window.location.hash).toBe('');
    expect(sentBodies()[0]).toEqual({ token: TOKEN });
    // 확인 전에는 이동하지 않는다
    expect(assigned).toEqual([]);
    await userEvent.click(screen.getByRole('button', { name: '참여하고 로그인' }));
    expect(assigned).toEqual(['https://idp.example.com/authorize?state=s']);
  });

  it('쓸 수 없는 초대(404)·token 없음은 새 링크를 요청하라고 안내한다', async () => {
    start = () => err(404, 'NOT_FOUND');
    openInvite(`#${TOKEN}`);
    await screen.findByText('초대가 없거나 이미 쓰였거나 만료됐습니다. 초대한 관리자에게 새 링크를 요청하세요.');
    expect(screen.queryByRole('button', { name: '참여하고 로그인' })).toBeNull();
  });

  it('다른 사이트에서 연 초대(403)·서버 오류(5xx)는 각각 안내한다', async () => {
    start = () => err(403, 'FORBIDDEN');
    openInvite(`#${TOKEN}`);
    await screen.findByText('이 주소에서는 초대를 열 수 없습니다. 받은 링크를 브라우저 주소창에 그대로 붙여 넣어 여세요.');
    cleanup();
    start = () => err(503, 'UNAVAILABLE');
    openInvite(`#${TOKEN}`);
    await screen.findByText('초대를 확인하지 못했습니다. 잠시 뒤 받은 링크를 다시 여세요.');
  });

  it('token을 저장소·콘솔에 남기지 않는다', async () => {
    const log = vi.spyOn(console, 'log');
    openInvite(`#${TOKEN}`);
    await screen.findByText('acme');
    const stored = [window.localStorage, window.sessionStorage].flatMap((st) => Object.keys(st).map((k) => st.getItem(k) ?? ''));
    expect(stored.join('|')).not.toContain(TOKEN);
    expect(JSON.stringify(log.mock.calls)).not.toContain(TOKEN);
  });

  it('token이 없으면 서버를 부르지 않는다', async () => {
    openInvite('');
    await screen.findByText('초대가 없거나 이미 쓰였거나 만료됐습니다. 초대한 관리자에게 새 링크를 요청하세요.');
    await waitFor(() => expect(sentBodies()).toEqual([]));
  });
});

describe('로그인 화면의 step-up·초대 오류', () => {
  it('step-up 실패는 이유와 하던 화면으로 돌아갈 링크를 보인다', async () => {
    const router = createMemoryRouter(routes, { initialEntries: ['/login?error=step_up_rejected&return_to=%2Fo%2Facme%2Fsettings%2Fteam'] });
    render(<RouterProvider router={router} />);
    expect(await screen.findByText(/추가 인증을 마치지 못했습니다/)).toBeTruthy();
    expect(screen.getByRole('link', { name: '하던 화면으로 돌아가기' }).getAttribute('href')).toBe('/o/acme/settings/team');
  });

  // 브라우저는 `\`를 `/`로, tab·줄바꿈을 지우고 읽는다 — 이 값들은 다른 사이트로 간다(서버 SafeReturnTo와 같은 규칙)
  it.each(['%2F%2Fevil.example', '%2F%5Cevil.example', '%2F%09%2Fevil.example', '%2F%5C%5Cevil', 'https%3A%2F%2Fevil.example', '%2F%0A%2Fevil'])(
    '외부 주소(%s)로는 돌아가기 링크를 만들지 않는다',
    async (rt) => {
      const router = createMemoryRouter(routes, { initialEntries: [`/login?error=step_up_rejected&return_to=${rt}`] });
      render(<RouterProvider router={router} />);
      await screen.findByText(/추가 인증을 마치지 못했습니다/);
      expect(screen.queryByRole('link', { name: '하던 화면으로 돌아가기' })).toBeNull();
      // SSO 버튼의 return_to도 같은 사이트 경로로만
      expect(screen.getByRole('link', { name: 'SSO로 로그인' }).getAttribute('href')).toBe('/api/v1/auth/login?return_to=%2F');
    },
  );

  it('초대 오류를 안내한다', async () => {
    const router = createMemoryRouter(routes, { initialEntries: ['/login?error=invitation_invalid'] });
    render(<RouterProvider router={router} />);
    expect(await screen.findByText('초대가 없거나 이미 쓰였거나 만료됐습니다. 초대한 관리자에게 새 링크를 요청하세요.')).toBeTruthy();
  });
});
