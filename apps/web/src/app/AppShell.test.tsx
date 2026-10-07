import { act, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter, type RouteObject } from 'react-router';
import { describe, expect, it, vi } from 'vitest';
import { OrgShell } from './AppShell.tsx';
import type { Entitlement } from './nav.ts';
import { routes } from './routes.tsx';

function renderAt(path: string, routeTable: RouteObject[] = routes) {
  const router = createMemoryRouter(routeTable, { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return router;
}

function withEntitlements(entitlements: ReadonlySet<Entitlement>): RouteObject[] {
  return routes.map((r) => (r.path === '/o/:org' ? { ...r, element: <OrgShell entitlements={entitlements} /> } : r));
}

describe('AppShell', () => {
  it('루트는 개발용 조직의 Overview로 이동', async () => {
    const router = renderAt('/');
    expect(await screen.findByRole('heading', { level: 1, name: 'Overview' })).toBeTruthy();
    expect(router.state.location.pathname).toBe('/o/demo/overview');
  });

  it('현재 화면 메뉴에 aria-current, 메뉴 링크는 조사 context만 들고 간다', async () => {
    renderAt('/o/acme/traces?env=prod&range=4h&tab=spans');
    const nav = await screen.findByRole('navigation', { name: '주 메뉴' });
    expect(within(nav).getByRole('link', { name: 'Traces' }).getAttribute('aria-current')).toBe('page');
    const logs = within(nav).getByRole('link', { name: 'Logs' });
    expect(logs.getAttribute('href')).toBe('/o/acme/logs?env=prod&range=4h');
  });

  it('권한이 없으면 Experience·Admin 그룹을 숨긴다', async () => {
    renderAt('/o/acme/overview');
    const nav = await screen.findByRole('navigation', { name: '주 메뉴' });
    expect(within(nav).queryByRole('link', { name: 'RUM' })).toBeNull();
    expect(within(nav).queryByRole('link', { name: 'Audit' })).toBeNull();
  });

  it('권한이 있으면 Experience·Admin 그룹을 보인다', async () => {
    renderAt('/o/acme/overview', withEntitlements(new Set(['experience', 'admin'])));
    const nav = await screen.findByRole('navigation', { name: '주 메뉴' });
    expect(within(nav).getByRole('link', { name: 'RUM' })).toBeTruthy();
    for (const name of ['Team', 'Access', 'Data Controls', 'Usage', 'Audit']) {
      expect(within(nav).getByRole('link', { name })).toBeTruthy();
    }
  });

  it('공유 링크는 절대시간·허용 키만 담고 fragment를 버린다', async () => {
    const writeText = vi.fn<(text: string) => Promise<void>>().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    window.history.replaceState(null, '', '/o/acme/traces?range=1h&q=secret#frag');
    renderAt('/o/acme/traces?range=1h&q=secret');
    await userEvent.click(await screen.findByRole('button', { name: '절대시간 링크 복사' }));
    expect(await screen.findByText('절대시간 링크를 복사했습니다.')).toBeTruthy();
    const copied = new URL(writeText.mock.calls[0]?.[0] ?? '');
    expect(copied.hash).toBe('');
    expect(copied.searchParams.has('q')).toBe(false);
    expect(copied.searchParams.has('from')).toBe(true);
  });

  it('조직을 바꾸면 shell 상태를 새로 시작한다', async () => {
    const writeText = vi.fn<(text: string) => Promise<void>>().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    const router = renderAt('/o/acme/overview');
    await userEvent.click(await screen.findByRole('button', { name: '절대시간 링크 복사' }));
    expect(await screen.findByText('절대시간 링크를 복사했습니다.')).toBeTruthy();
    await act(() => router.navigate('/o/other/overview'));
    expect(screen.queryByText('절대시간 링크를 복사했습니다.')).toBeNull();
  });

  it('overlay 메뉴: 열면 메뉴 안으로 focus, 본문은 inert, Esc로 닫고 메뉴 버튼으로 복귀', async () => {
    renderAt('/o/acme/overview');
    const menuButton = await screen.findByRole('button', { name: '메뉴' });
    await userEvent.click(menuButton);
    const nav = screen.getByRole('navigation', { name: '주 메뉴', hidden: true });
    expect(nav.contains(document.activeElement)).toBe(true);
    expect(menuButton.closest('.mt-shell__body')?.hasAttribute('inert')).toBe(true);
    await userEvent.keyboard('{Escape}');
    expect(menuButton.getAttribute('aria-expanded')).toBe('false');
    expect(document.activeElement).toBe(menuButton);
  });

  it('시간 범위를 바꿔도 화면과 다른 query를 유지', async () => {
    const router = renderAt('/o/acme/traces?tab=spans');
    await userEvent.selectOptions(await screen.findByLabelText('시간'), '15m');
    expect(router.state.location.pathname).toBe('/o/acme/traces');
    const q = new URLSearchParams(router.state.location.search);
    expect(q.get('range')).toBe('15m');
    expect(q.get('tab')).toBe('spans');
  });

  it('잘못된 구간은 입력 옆에 이유를 보이고 기본값을 쓴다', async () => {
    renderAt('/o/acme/traces?from=2026-10-04T06:00:00Z&to=2026-10-04T05:00:00Z');
    expect(await screen.findByText(/시작 시각이 끝 시각보다 앞서야 합니다/)).toBeTruthy();
    expect((screen.getByLabelText('시간') as HTMLSelectElement).value).toBe('1h');
    expect(screen.getByLabelText('시간').getAttribute('aria-invalid')).toBe('true');
  });

  it('테마 선택은 html data-theme에 반영하고, 시스템이면 속성을 지운다', async () => {
    renderAt('/o/acme/overview');
    const select = await screen.findByLabelText('테마');
    await userEvent.selectOptions(select, 'dark');
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
    await userEvent.selectOptions(select, 'system');
    expect(document.documentElement.hasAttribute('data-theme')).toBe(false);
  });

  it('없는 화면은 존재를 드러내지 않는 404', async () => {
    renderAt('/o/acme/no-such-screen');
    expect(await screen.findByRole('heading', { name: '페이지를 찾을 수 없습니다' })).toBeTruthy();
  });
});
