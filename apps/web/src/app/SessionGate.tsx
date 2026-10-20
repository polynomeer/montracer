// 사람 로그인 상태 확인 (ADR 0065).
// - GET /api/v1/auth/session: 200이면 session 모드, 404면 로그인 꺼짐(API key proxy 모드 — 지금처럼 동작), 401이면 로그인으로.
// - URL 조직(/o/{org})은 session 조직과 같아야 한다(D02 §12 "URL tenant와 불일치하면 403").
//   다른 membership 조직이면 바꿀지 묻고(조용히 바꾸지 않는다), member가 아닌 조직이면 접근할 수 없다고 알린다.
// - session 정보는 context로 내려준다(사용자 메뉴). 서버 데이터를 전역 store에 복사하지 않는다 — 이 화면 수명 동안만 둔다.
import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';
import { Link, Navigate, useLocation, useParams } from 'react-router';
import { external, loginURL, setUnauthenticatedHandler } from '../api/authState.ts';
import { getSession, orgSlug, switchTenant, type SessionInfo } from '../api/session.ts';
import { orgPath } from './nav.ts';

const SessionContext = createContext<SessionInfo | null>(null);

/** 지금 session. 로그인 꺼짐(API key 모드)이면 null이다. */
export function useSession(): SessionInfo | null {
  return useContext(SessionContext);
}

// error(5xx·연결 오류)는 확인하지 못한 것이다. 화면을 그리지 않는다 — session 조직과 URL 조직을 맞춰 보지 못한 채
// 그리면, 조회 API는 session 조직의 데이터를 주는데 화면은 URL 조직 이름을 단다(다른 조직 데이터처럼 보임).
type GateState = { kind: 'loading' } | { kind: 'off' } | { kind: 'on'; info: SessionInfo } | { kind: 'error' };

/** 로그인으로 보낸 시각(sessionStorage). 짧은 시간 안에 다시 401이면 되풀이(cookie가 막힘 등)로 보고 로그인 화면에 멈춘다. */
const LOGIN_MARK = 'mt-login-started-at';
const LOOP_WINDOW_MS = 60_000;

/** 401이면 복귀 경로를 담아 로그인으로. 1분 안에 두 번째면 되풀이를 막고 로그인 화면에 이유를 보인다. */
export function startLogin(returnTo: string): void {
  let last = 0;
  try {
    last = Number(window.sessionStorage.getItem(LOGIN_MARK) ?? '0');
  } catch {
    last = 0; // 저장소를 못 쓰면 되풀이 판단 없이 진행
  }
  const now = Date.now();
  if (now - last < LOOP_WINDOW_MS) {
    external.assign(`/login?error=session_loop&return_to=${encodeURIComponent(returnTo)}`);
    return;
  }
  try {
    window.sessionStorage.setItem(LOGIN_MARK, String(now));
  } catch {
    // 무시
  }
  external.assign(loginURL(returnTo));
}

/** session을 받았으면 되풀이 표시를 지운다. */
function clearLoginMark(): void {
  try {
    window.sessionStorage.removeItem(LOGIN_MARK);
  } catch {
    // 무시
  }
}

function useSessionState(): [GateState, () => void] {
  const location = useLocation();
  const [state, setState] = useState<GateState>({ kind: 'loading' });
  const [retry, setRetry] = useState(0);
  const here = location.pathname + location.search;
  useEffect(() => {
    setState({ kind: 'loading' });
    const ctrl = new AbortController();
    getSession(ctrl.signal)
      .then((r) => {
        if (ctrl.signal.aborted) return;
        if (r.ok) {
          clearLoginMark();
          setState({ kind: 'on', info: r.data });
        } else if (r.status === 404) setState({ kind: 'off' });
        else if (r.status === 401) startLogin(here);
        else setState({ kind: 'error' });
      })
      .catch(() => undefined); // 취소
    return () => ctrl.abort();
    // 조회는 화면을 처음 열 때(와 다시 시도 때) 한 번만 한다(화면 이동마다 부르지 않는다). 그 뒤 만료는 API 401이 알린다
  }, [retry]);
  // session으로 쓰는 중 만료·폐기(API 401)면 지금 위치로 돌아오는 로그인으로
  useEffect(() => {
    if (state.kind !== 'on') return;
    setUnauthenticatedHandler(() => startLogin(window.location.pathname + window.location.search));
    return () => setUnauthenticatedHandler(null);
  }, [state.kind]);
  return [state, () => setRetry((n) => n + 1)];
}

function GateWaiting() {
  return (
    <div className="mt-session-gate" role="status">
      로그인 상태를 확인하는 중
    </div>
  );
}

function GateError({ onRetry }: { onRetry: () => void }) {
  return (
    <div className="mt-session-gate">
      <div className="mt-notice mt-notice--critical" role="alert">
        <strong>로그인 상태를 확인하지 못했습니다.</strong>
        <span>서버에 연결할 수 없거나 일시적인 오류입니다. 확인 전에는 조직 데이터를 보이지 않습니다.</span>
        <button type="button" className="mt-button" onClick={onRetry}>
          다시 시도
        </button>
      </div>
    </div>
  );
}

/**
 * 조직 화면을 감싼다. session 조직과 URL 조직을 맞춰 본 뒤에만 화면을 그린다(D02 §12) — 확인 중·확인 실패면 그리지 않는다.
 * 로그인 꺼짐(404)이면 그대로 그린다.
 */
export function SessionGate({ children }: { children: ReactNode }) {
  const { org = '' } = useParams();
  const [state, retry] = useSessionState();
  if (state.kind === 'loading') return <GateWaiting />;
  if (state.kind === 'error') return <GateError onRetry={retry} />;
  if (state.kind === 'off') return <>{children}</>;
  const info = state.info;
  if (org !== orgSlug(info)) return <OrgMismatch org={org} info={info} />;
  return <SessionContext.Provider value={info}>{children}</SessionContext.Provider>;
}

function OrgMismatch({ org, info }: { org: string; info: SessionInfo }) {
  const location = useLocation();
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  const target = info.memberships.find((m) => orgSlug(m) === org);
  const current = orgPath(orgSlug(info), 'overview');
  return (
    <div className="mt-session-gate">
      <div className="mt-notice mt-notice--warning" role="alert">
        {target === undefined ? (
          <>
            <strong>이 조직({org})에 접근할 수 없습니다.</strong>
            <span>member인 조직만 볼 수 있습니다.</span>
            <Link to={current}>{orgSlug(info)} 조직으로 가기</Link>
          </>
        ) : (
          <>
            <strong>지금 로그인한 조직은 {orgSlug(info)}입니다.</strong>
            <span>{org} 조직으로 바꾸면 화면을 다시 불러옵니다.</span>
            <button
              type="button"
              className="mt-button"
              disabled={busy}
              onClick={() => {
                setBusy(true);
                void switchTenant(target.tenant_id).then((r) => {
                  if (r.ok) external.assign(location.pathname + location.search);
                  else {
                    setBusy(false);
                    setFailed(true);
                  }
                });
              }}
            >
              {org} 조직으로 바꾸기
            </button>
            <Link to={current}>{orgSlug(info)}에 머물기</Link>
            {failed && <span role="status">바꾸지 못했습니다. 다시 시도하세요.</span>}
          </>
        )}
      </div>
    </div>
  );
}

/** `/`: session이면 그 조직, 로그인 꺼짐이면 개발 기본 조직으로. */
export function RootRedirect({ fallbackOrg }: { fallbackOrg: string }) {
  const [state, retry] = useSessionState();
  if (state.kind === 'loading') return <GateWaiting />;
  // 확인 실패는 개발 기본 조직으로 보내지 않는다(production에서 엉뚱한 조직 URL로 가지 않게)
  if (state.kind === 'error') return <GateError onRetry={retry} />;
  const org = state.kind === 'on' ? orgSlug(state.info) : fallbackOrg;
  return <Navigate to={orgPath(org, 'overview')} replace />;
}
