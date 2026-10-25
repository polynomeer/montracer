import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AuditEvent } from '../../api/audit.ts';
import { routes } from '../../app/routes.tsx';
import { controlMeta } from '../../test/fixtures';
import { sessionMock } from '../../test/sessionMock';
import { parseCategory, validAction } from './auditView.ts';

const ACME = '7a1b0000-0000-4000-8000-00000000000a';
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const err = (status: number, code: string, details?: Record<string, unknown>) =>
  json(status, { error: { code, message: 'x', request_id: 'req-a', retryable: false, ...(details === undefined ? {} : { details }) } });

const event = (n: number, over: Partial<AuditEvent> = {}): AuditEvent => ({
  id: `aaaaaaaa-0000-4000-8000-0000000000${String(n).padStart(2, '0')}`,
  occurred_at: `2026-10-20T0${String(n % 10)}:00:00Z`,
  category: 'security',
  action: 'api_key.revoked',
  actor_kind: 'user',
  actor_id: 'oidc:abc:alice',
  resource_type: 'api_key',
  resource_id: '0123456789abcdef',
  request_id: `req-${String(n)}`,
  // 생성 타입의 JSONValue는 재귀라 리터럴을 좁히지 못한다 — 서버는 임의 JSON 객체를 준다
  details: { reason: '<b>교체</b>' } as unknown as AuditEvent['details'],
  ...over,
});

let session: () => Response;
let page: (url: URL) => Response;
const auditCalls = () =>
  vi
    .mocked(fetch)
    .mock.calls.map(([u]) => String(u))
    .filter((u) => u.includes('/audit-events'))
    .map((u) => new URL(u, 'http://x'));

function sessionBody(role: string) {
  return {
    data: {
      user_id: 'oidc:abc:alice',
      display_name: 'Alice',
      tenant_id: ACME,
      tenant_name: 'acme',
      role,
      memberships: [{ tenant_id: ACME, tenant_name: 'acme', role }],
      csrf_token: 'csrf-1',
      idle_expires_at: '2099-10-20T10:30:00Z',
      expires_at: '2099-10-20T22:00:00Z',
      step_up_expires_at: null,
    },
    meta: controlMeta(),
  };
}

beforeEach(() => {
  sessionMock.passThrough = true;
  session = () => json(200, sessionBody('security_auditor'));
  page = () => json(200, { data: [event(1), event(2, { category: 'operations', action: 'monitor.updated', actor_kind: 'operator', actor_id: 'montracer-operator', request_id: null })], next_cursor: null, meta: controlMeta() });
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => {
      const u = new URL(url, 'http://x');
      if (u.pathname === '/api/v1/auth/session') return Promise.resolve(session());
      if (u.pathname === '/api/v1/audit-events') return Promise.resolve(page(u));
      return Promise.resolve(err(404, 'NOT_FOUND'));
    }),
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function renderAt(search = '?tz=UTC&range=7d') {
  const router = createMemoryRouter(routes, { initialEntries: [`/o/acme/settings/audit${search}`] });
  render(<RouterProvider router={router} />);
  return router;
}

describe('Audit 화면', () => {
  it('기록을 시간 역순 표로 보이고, details는 글자 그대로, 운영자는 이름 대신 운영자로', async () => {
    renderAt();
    const table = await screen.findByRole('table', { name: '감사 기록' });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(2);
    expect(rows[0]?.textContent).toContain('보안');
    expect(rows[0]?.textContent).toContain('api_key.revoked');
    expect(rows[1]?.textContent).toContain('Montracer 운영자');
    expect(rows[1]?.textContent).toContain('montracer-operator');
    const pre = rows[0]?.querySelector('pre');
    expect(pre?.textContent).toContain('<b>교체</b>');
    expect(pre?.querySelector('b')).toBeNull();
    expect(screen.getByText(/2개 · 끝까지 봤습니다/)).toBeTruthy();
    const q = auditCalls()[0];
    expect(q?.searchParams.get('limit')).toBe('100');
    expect(Date.parse(q?.searchParams.get('to') ?? '') - Date.parse(q?.searchParams.get('from') ?? '')).toBe(7 * 24 * 3600 * 1000);
    expect(q?.searchParams.has('category')).toBe(false);
  });

  it('범주·action 조건은 URL에 남고 그대로 서버에 간다. 틀린 action은 보내지 않는다', async () => {
    const router = renderAt();
    await screen.findByRole('table', { name: '감사 기록' });
    await userEvent.selectOptions(screen.getByLabelText('범주'), 'operations');
    expect(router.state.location.search).toContain('category=operations');
    const input = screen.getByLabelText('action (정확히 일치)');
    await userEvent.type(input, 'bad action');
    await userEvent.click(screen.getByRole('button', { name: '적용' }));
    expect(await screen.findByText(/공백·제어 문자 없이/)).toBeTruthy();
    expect(router.state.location.search).not.toContain('action=');
    await userEvent.clear(input);
    await userEvent.type(input, 'monitor.updated');
    await userEvent.click(screen.getByRole('button', { name: '적용' }));
    await waitFor(() => expect(auditCalls().at(-1)?.searchParams.get('action')).toBe('monitor.updated'));
    expect(auditCalls().at(-1)?.searchParams.get('category')).toBe('operations');
  });

  it('더 보기는 cursor로 이어 붙이고, cursor가 만료되면(400 cursor) 처음부터 다시 읽게 한다', async () => {
    page = (u) =>
      u.searchParams.get('cursor') === null
        ? json(200, { data: [event(1)], next_cursor: 'c1', meta: controlMeta() })
        : u.searchParams.get('cursor') === 'c1'
          ? json(200, { data: [event(2)], next_cursor: 'c2', meta: controlMeta() })
          : err(400, 'INVALID_ARGUMENT', { field_violations: [{ field: 'cursor', reason: 'expired' }] });
    renderAt();
    await screen.findByRole('table', { name: '감사 기록' });
    await userEvent.click(screen.getByRole('button', { name: '100개 더 보기' }));
    await waitFor(() => expect(within(screen.getByRole('table', { name: '감사 기록' })).getAllByRole('row')).toHaveLength(3));
    await userEvent.click(screen.getByRole('button', { name: '100개 더 보기' }));
    expect(await screen.findByText(/이어 받을 수 있는 시간\(15분\)이 지났거나/)).toBeTruthy();
    // 앞의 기록은 남는다
    expect(within(screen.getByRole('table', { name: '감사 기록' })).getAllByRole('row')).toHaveLength(3);
    await userEvent.click(screen.getByRole('button', { name: '처음부터 다시 불러오기' }));
    await waitFor(() => expect(within(screen.getByRole('table', { name: '감사 기록' })).getAllByRole('row')).toHaveLength(2));
    expect(auditCalls().at(-1)?.searchParams.has('cursor')).toBe(false);
  });

  it('보안 범주 권한이 없으면(403) "기록 없음"이 아니라 권한 없음', async () => {
    page = (u) => (u.searchParams.get('category') === 'security' ? err(403, 'FORBIDDEN') : json(200, { data: [], next_cursor: null, meta: controlMeta() }));
    session = () => json(200, sessionBody('operator'));
    renderAt('?tz=UTC&range=7d&category=security');
    expect(await screen.findByText('보안 범주를 볼 권한이 없습니다')).toBeTruthy();
    expect(screen.getByText(/기록이 없다는 뜻이 아닙니다/)).toBeTruthy();
    expect(screen.getByText(/지금 권한으로는 운영 범주만 보입니다/)).toBeTruthy();
  });

  it('조건에 맞는 기록이 없으면 빈 상태, 조회 실패는 실패로', async () => {
    page = () => json(200, { data: [], next_cursor: null, meta: controlMeta() });
    const { unmount } = render(<RouterProvider router={createMemoryRouter(routes, { initialEntries: ['/o/acme/settings/audit?tz=UTC&range=7d'] })} />);
    expect(await screen.findByText('이 기간·조건에 감사 기록이 없습니다')).toBeTruthy();
    unmount();
    page = () => err(503, 'UNAVAILABLE');
    renderAt();
    expect(await screen.findByText(/req-a/)).toBeTruthy();
    expect(screen.queryByText('이 기간·조건에 감사 기록이 없습니다')).toBeNull();
  });

  it('366일을 넘는 범위는 조회하지 않는다', async () => {
    renderAt('?tz=UTC&from=2025-10-01T00:00:00Z&to=2026-10-20T00:00:00Z'); // 384일(조사 context 한도 395일 안)
    expect(await screen.findByText(/366일까지 조회할 수 있습니다/)).toBeTruthy();
    expect(auditCalls()).toHaveLength(0);
  });
});

describe('감사 표시 규칙', () => {
  it('action은 공백·제어 문자 없이 128 byte까지', () => {
    expect(validAction('api_key.revoked')).toBe(true);
    expect(validAction('a b')).toBe(false);
    expect(validAction('a\tb')).toBe(false);
    expect(validAction('a'.repeat(128))).toBe(true);
    expect(validAction('가'.repeat(43))).toBe(false); // 129 byte
    expect(validAction('')).toBe(false);
  });

  it('모르는 범주는 조건 없음', () => {
    expect(parseCategory('security')).toBe('security');
    expect(parseCategory('all')).toBeNull();
    expect(parseCategory(null)).toBeNull();
  });
});
