import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { external, setCSRFToken } from '../api/authState.ts';
import { controlMeta } from '../test/fixtures';
import { routes } from './routes.tsx';

const ACME = '7a1b0000-0000-4000-8000-00000000000a';
const GLOBEX = '7a1b0000-0000-4000-8000-00000000000b';

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const err = (status: number, code: string) => json(status, { error: { code, message: 'x', request_id: 'req-e', retryable: false } });

function sessionBody(tenant: string, name: string, role: string, csrf = 'csrf-1') {
  return {
    data: {
      user_id: 'oidc:abcdef012345:dev-admin',
      display_name: 'Dev Admin',
      tenant_id: tenant,
      tenant_name: name,
      role,
      memberships: [
        { tenant_id: ACME, tenant_name: 'acme', role: 'tenant_admin' },
        { tenant_id: GLOBEX, tenant_name: 'globex', role: 'viewer' },
      ],
      csrf_token: csrf,
      idle_expires_at: '2026-10-20T10:30:00Z',
      expires_at: '2026-10-20T22:00:00Z',
    },
    meta: controlMeta(),
  };
}

let session: () => Response;
let servicesStatus: number;
let assigned: string[];

beforeEach(() => {
  session = () => json(200, sessionBody(ACME, 'acme', 'tenant_admin'));
  servicesStatus = 404;
  assigned = [];
  vi.spyOn(external, 'assign').mockImplementation((u: string) => {
    assigned.push(u);
  });
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string, init?: RequestInit) => {
      if (url.endsWith('/auth/session')) return Promise.resolve(session());
      if (url.endsWith('/auth/logout')) return Promise.resolve(new Response(null, { status: 204 }));
      if (url.endsWith('/auth/session/tenant')) {
        const b = JSON.parse(String(init?.body)) as { tenant_id: string };
        return Promise.resolve(json(200, sessionBody(b.tenant_id, b.tenant_id === GLOBEX ? 'globex' : 'acme', 'viewer', 'csrf-2')));
      }
      if (url.includes('/api/v1/services') && servicesStatus === 401) return Promise.resolve(err(401, 'UNAUTHENTICATED'));
      return Promise.resolve(err(404, 'NOT_FOUND'));
    }),
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  setCSRFToken(null);
});

function renderAt(path: string) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return router;
}

const calls = (suffix: string) => vi.mocked(fetch).mock.calls.filter(([u]) => String(u).endsWith(suffix));

describe('로그인 session (ADR 0065)', () => {
  it('session이면 사용자 메뉴에 이름·역할, 다른 조직으로 바꾸기와 로그아웃(CSRF header)', async () => {
    renderAt('/o/acme/overview?range=1h');
    const button = await screen.findByRole('button', { name: 'Dev Admin · Tenant Admin' });
    await userEvent.click(button);
    const menu = screen.getByRole('group', { name: '사용자 메뉴' });
    expect(within(menu).getByText('acme 조직 · Tenant Admin')).toBeTruthy();

    await userEvent.click(within(menu).getByRole('button', { name: 'globex (Viewer)' }));
    await waitFor(() => expect(assigned).toEqual(['/o/globex/overview']));
    const sw = calls('/auth/session/tenant')[0]?.[1] as RequestInit;
    expect((sw.headers as Record<string, string>)['X-CSRF-Token']).toBe('csrf-1');

    await userEvent.click(within(menu).getByRole('button', { name: '로그아웃' }));
    await waitFor(() => expect(assigned).toContain('/login?logged_out=1'));
    // 전환 뒤에는 새 session의 CSRF token을 쓴다
    const out = calls('/auth/logout')[0]?.[1] as RequestInit;
    expect((out.headers as Record<string, string>)['X-CSRF-Token']).toBe('csrf-2');
  });

  it('로그인하지 않았으면 지금 위치로 돌아오는 로그인으로 보낸다', async () => {
    session = () => err(401, 'UNAUTHENTICATED');
    renderAt('/o/acme/map?tab=table');
    await waitFor(() => expect(assigned).toEqual([`/api/v1/auth/login?return_to=${encodeURIComponent('/o/acme/map?tab=table')}`]));
  });

  it('session으로 쓰는 중 API가 401이면(만료·폐기) 로그인으로', async () => {
    servicesStatus = 401;
    renderAt('/o/acme/services');
    await screen.findByRole('button', { name: 'Dev Admin · Tenant Admin' });
    await waitFor(() => expect(assigned[0]).toBe(`/api/v1/auth/login?return_to=${encodeURIComponent('/')}`));
  });

  it('URL 조직이 다른 membership이면 바꿀지 묻고, 바꾸면 화면을 다시 불러온다', async () => {
    renderAt('/o/globex/overview?range=1h');
    expect(await screen.findByText('지금 로그인한 조직은 acme입니다.')).toBeTruthy();
    expect(screen.queryByRole('navigation', { name: '주 메뉴' })).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: 'globex 조직으로 바꾸기' }));
    await waitFor(() => expect(assigned).toEqual(['/o/globex/overview?range=1h']));
  });

  it('member가 아닌 조직은 접근할 수 없다고 알리고 내 조직으로 안내한다(D02 §12 403)', async () => {
    renderAt('/o/initech/overview');
    expect(await screen.findByText('이 조직(initech)에 접근할 수 없습니다.')).toBeTruthy();
    expect(screen.getByRole('link', { name: 'acme 조직으로 가기' }).getAttribute('href')).toBe('/o/acme/overview');
    expect(screen.queryByRole('button', { name: /조직으로 바꾸기/ })).toBeNull();
  });

  it('로그인이 꺼져 있으면(404) 지금처럼 그리고 사용자 메뉴가 없다', async () => {
    session = () => err(404, 'NOT_FOUND');
    renderAt('/o/acme/overview');
    expect(await screen.findByRole('navigation', { name: '주 메뉴' })).toBeTruthy();
    await waitFor(() => expect(calls('/auth/session')).toHaveLength(1));
    expect(screen.queryByRole('button', { name: /Dev Admin/ })).toBeNull();
    expect(assigned).toEqual([]);
  });

  it('루트는 session 조직으로', async () => {
    session = () => json(200, sessionBody(GLOBEX, 'globex', 'viewer'));
    const router = renderAt('/');
    await waitFor(() => expect(router.state.location.pathname).toBe('/o/globex/overview'));
  });
});

describe('로그인 화면', () => {
  it('실패 사유를 사람이 읽는 문장으로, 로그인 버튼은 복귀 경로를 담는다', async () => {
    renderAt('/login?error=no_membership&return_to=%2Fo%2Facme%2Fmap');
    expect(await screen.findByRole('alert')).toHaveProperty('textContent', '이 계정은 아직 어느 조직의 member도 아닙니다. 조직 관리자에게 초대를 요청하세요.');
    expect(screen.getByRole('link', { name: 'SSO로 로그인' }).getAttribute('href')).toBe(
      `/api/v1/auth/login?return_to=${encodeURIComponent('/o/acme/map')}`,
    );
  });

  it('외부 복귀 주소는 쓰지 않고, 모르는 사유는 일반 문장', async () => {
    renderAt('/login?error=weird&return_to=https://evil.example');
    expect((await screen.findByRole('alert')).textContent).toBe('로그인하지 못했습니다. 다시 시도하세요.');
    expect(screen.getByRole('link', { name: 'SSO로 로그인' }).getAttribute('href')).toBe('/api/v1/auth/login?return_to=%2F');
  });

  it('로그아웃 뒤 안내', async () => {
    renderAt('/login?logged_out=1');
    expect((await screen.findByRole('status')).textContent).toBe('로그아웃했습니다.');
  });
});
