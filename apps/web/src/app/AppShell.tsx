import { useEffect, useId, useRef, useState, type RefObject } from 'react';
import { NavLink, Outlet, useLocation, useParams, useSearchParams } from 'react-router';
import {
  RELATIVE_PRESETS,
  browserTimeZone,
  formatRange,
  parseContext,
  shareSearchParams,
  writeContext,
  type ContextError,
  type InvestigationContext,
  type RelativePreset,
} from './context.ts';
import { orgPath, visibleGroups, type Entitlement } from './nav.ts';
import { applyThemePreference, isThemePreference, loadThemePreference, type ThemePreference } from './theme.ts';

const PRESET_LABELS: Record<RelativePreset, string> = {
  '15m': '최근 15분',
  '1h': '최근 1시간',
  '4h': '최근 4시간',
  '1d': '최근 1일',
  '7d': '최근 7일',
};

const CONTEXT_KEYS = ['env', 'range', 'from', 'to', 'tz'];

/** 메뉴 이동 시 조사 context만 들고 간다. 화면별 filter·tab은 남기지 않는다. */
function contextOnly(params: URLSearchParams): string {
  const out = new URLSearchParams();
  for (const k of CONTEXT_KEYS) {
    const v = params.get(k);
    if (v !== null) out.set(k, v);
  }
  const s = out.toString();
  return s === '' ? '' : `?${s}`;
}

export interface AppShellProps {
  /** capabilities API 연결 전까지는 빈 집합이다. 권한이 없는 그룹은 메뉴에서 숨긴다. */
  entitlements?: ReadonlySet<Entitlement>;
  /** 알려진 environment 목록. catalog API 연결 전에는 비어 있고 URL의 값만 보인다. */
  environments?: readonly string[];
}

/**
 * 조직마다 shell을 새로 만든다. 조직 전환 시 화면 상태(와 이후 붙을 조회 hook)가
 * 이전 조직에서 넘어오지 않게 한다 (D05 §01: 조직 전환은 cache·query·stream 취소).
 */
export function OrgShell(props: AppShellProps) {
  const { org = '' } = useParams();
  return <AppShell key={org} {...props} />;
}

export function AppShell({ entitlements = new Set(), environments = [] }: AppShellProps) {
  const { org = '' } = useParams();
  const [params, setParams] = useSearchParams();
  const location = useLocation();
  const [drawerOpen, setDrawerOpen] = useState(false);
  const menuButtonRef = useRef<HTMLButtonElement>(null);
  const sidebarRef = useRef<HTMLElement>(null);
  const { context, errors } = parseContext(params, browserTimeZone(), Date.now());

  // 화면 이동 시 overlay 메뉴를 닫는다.
  useEffect(() => setDrawerOpen(false), [location.pathname]);

  // overlay 메뉴: 열면 첫 링크로 focus, Esc로 닫고 메뉴 버튼으로 focus를 돌려준다.
  // 열린 동안 본문은 inert라 focus가 메뉴 안에 머문다 (D05 §03 drawer).
  useEffect(() => {
    if (!drawerOpen) return;
    sidebarRef.current?.querySelector<HTMLElement>('a')?.focus();
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setDrawerOpen(false);
        menuButtonRef.current?.focus();
      }
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [drawerOpen]);

  // 넓은 화면(고정 sidebar)으로 바뀌면 overlay 상태를 풀어 본문 inert가 남지 않게 한다.
  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return;
    const wide = window.matchMedia('(min-width: 1440px)');
    const onChange = () => {
      if (wide.matches) setDrawerOpen(false);
    };
    wide.addEventListener('change', onChange);
    return () => wide.removeEventListener('change', onChange);
  }, []);

  const closeDrawer = () => {
    setDrawerOpen(false);
    menuButtonRef.current?.focus();
  };

  const updateContext = (next: InvestigationContext) => setParams(writeContext(params, next));

  return (
    <div className="mt-shell" data-drawer-open={drawerOpen ? 'true' : 'false'}>
      <a className="mt-skip-link" href="#main">
        본문으로 건너뛰기
      </a>
      <Sidebar ref={sidebarRef} org={org} search={contextOnly(params)} entitlements={entitlements} />
      <div className="mt-shell__body" inert={drawerOpen}>
        <Topbar
          org={org}
          drawerOpen={drawerOpen}
          menuButtonRef={menuButtonRef}
          onToggleDrawer={() => setDrawerOpen((v) => !v)}
        />
        <ContextRow
          context={context}
          errors={errors}
          environments={environments}
          onChange={updateContext}
          share={() => shareSearchParams(params, context, Date.now())}
        />
        <main id="main" className="mt-main" tabIndex={-1}>
          <Outlet context={context} />
        </main>
      </div>
      {drawerOpen && (
        <button type="button" className="mt-scrim" aria-label="메뉴 닫기" onClick={closeDrawer} />
      )}
    </div>
  );
}

interface SidebarProps {
  ref: RefObject<HTMLElement | null>;
  org: string;
  search: string;
  entitlements: ReadonlySet<Entitlement>;
}

function Sidebar({ ref, org, search, entitlements }: SidebarProps) {
  return (
    <nav ref={ref} id="mt-sidebar" className="mt-sidebar" aria-label="주 메뉴">
      <NavLink className="mt-brand" to={{ pathname: orgPath(org, 'overview'), search }}>
        <svg width="24" height="24" viewBox="0 0 24 24" aria-hidden="true">
          <rect x="2" y="4" width="14" height="4" rx="1" fill="var(--mt-series-1)" />
          <rect x="7" y="10" width="9" height="4" rx="1" fill="var(--mt-series-2)" />
          <rect x="10" y="16" width="12" height="4" rx="1" fill="var(--mt-series-3)" />
        </svg>
        <span>Montracer</span>
      </NavLink>
      {visibleGroups(entitlements).map((g) => (
        <section key={g.name} className="mt-nav-group" aria-label={g.name}>
          <h2 className="mt-nav-group__title">{g.name}</h2>
          <ul>
            {g.items.map((it) => (
              <li key={it.screen.path}>
                <NavLink className="mt-nav-link" to={{ pathname: orgPath(org, it.screen.path), search }}>
                  {it.label}
                </NavLink>
              </li>
            ))}
          </ul>
        </section>
      ))}
    </nav>
  );
}

interface TopbarProps {
  org: string;
  drawerOpen: boolean;
  menuButtonRef: RefObject<HTMLButtonElement | null>;
  onToggleDrawer: () => void;
}

function Topbar({ org, drawerOpen, menuButtonRef, onToggleDrawer }: TopbarProps) {
  const searchId = useId();
  const themeId = useId();
  const [theme, setTheme] = useState<ThemePreference>(loadThemePreference);

  useEffect(() => applyThemePreference(theme), [theme]);

  return (
    <header className="mt-topbar">
      <button
        ref={menuButtonRef}
        type="button"
        className="mt-icon-button mt-menu-button"
        aria-label="메뉴"
        aria-expanded={drawerOpen}
        aria-controls="mt-sidebar"
        onClick={onToggleDrawer}
      >
        <svg width="18" height="18" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
          <path d="M2 4h12M2 8h12M2 12h12" />
        </svg>
      </button>
      <span className="mt-org">
        <span className="mt-label">조직</span>
        <span className="mt-org__name">{org}</span>
      </span>
      <div className="mt-search">
        <label htmlFor={searchId} className="mt-visually-hidden">
          전역 검색
        </label>
        <input id={searchId} type="search" className="mt-input" placeholder="서비스, trace ID, 화면 검색" maxLength={200} />
      </div>
      <div className="mt-topbar__end">
        <label htmlFor={themeId} className="mt-label">
          테마
        </label>
        <select
          id={themeId}
          className="mt-select"
          value={theme}
          onChange={(e) => {
            if (isThemePreference(e.target.value)) setTheme(e.target.value);
          }}
        >
          <option value="system">시스템</option>
          <option value="light">Light</option>
          <option value="dark">Dark</option>
        </select>
      </div>
    </header>
  );
}

interface ContextRowProps {
  context: InvestigationContext;
  errors: ContextError[];
  environments: readonly string[];
  onChange: (next: InvestigationContext) => void;
  share: () => URLSearchParams;
}

function ContextRow({ context, errors, environments, onChange, share }: ContextRowProps) {
  const envId = useId();
  const rangeId = useId();
  const errorId = useId();
  const [copyStatus, setCopyStatus] = useState('');
  const envOptions = [...new Set([...environments, ...(context.environment === null ? [] : [context.environment])])];
  const rangeValue = context.range.kind === 'relative' ? context.range.preset : 'absolute';

  const copyLink = async () => {
    const url = new URL(window.location.href);
    url.search = share().toString();
    url.hash = '';
    try {
      await navigator.clipboard.writeText(url.toString());
      setCopyStatus('절대시간 링크를 복사했습니다.');
    } catch {
      setCopyStatus('복사하지 못했습니다. 주소창의 링크를 쓰세요.');
    }
  };

  return (
    <div className="mt-context-row" role="group" aria-label="조사 범위">
      <label htmlFor={envId} className="mt-label">
        환경
      </label>
      <select
        id={envId}
        className="mt-select"
        value={context.environment ?? ''}
        onChange={(e) => onChange({ ...context, environment: e.target.value === '' ? null : e.target.value })}
      >
        <option value="">전체 환경</option>
        {envOptions.map((env) => (
          <option key={env} value={env}>
            {env}
          </option>
        ))}
      </select>
      <label htmlFor={rangeId} className="mt-label">
        시간
      </label>
      <select
        id={rangeId}
        className="mt-select"
        value={rangeValue}
        aria-describedby={errors.length > 0 ? errorId : undefined}
        aria-invalid={errors.some((e) => e.field === 'range')}
        onChange={(e) => {
          const v = e.target.value;
          if (Object.hasOwn(RELATIVE_PRESETS, v)) {
            onChange({ ...context, range: { kind: 'relative', preset: v as RelativePreset } });
          }
        }}
      >
        {Object.entries(PRESET_LABELS).map(([value, label]) => (
          <option key={value} value={value}>
            {label}
          </option>
        ))}
        {context.range.kind === 'absolute' && <option value="absolute">지정한 구간</option>}
      </select>
      <span className="mt-range-text" data-testid="range-text">
        {formatRange(context.range, Date.now(), context.timeZone)}
      </span>
      <div className="mt-context-row__end">
        <button type="button" className="mt-button" onClick={() => void copyLink()}>
          절대시간 링크 복사
        </button>
        <span className="mt-label" role="status" aria-live="polite">
          {copyStatus}
        </span>
      </div>
      {errors.length > 0 && (
        <ul id={errorId} className="mt-context-errors">
          {errors.map((e) => (
            <li key={e.field}>{e.message} 기본값을 적용했습니다.</li>
          ))}
        </ul>
      )}
    </div>
  );
}
