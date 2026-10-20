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

// error(5xx·연결 오류)는 확인하지 못한 것이다. 화면은 off처럼 그리고, 화면의 API 호출이 같은 서버 판단을 받는다.
type GateState = { kind: 'loading' } | { kind: 'off' } | { kind: 'on'; info: SessionInfo } | { kind: 'error' };

function useSessionState(): GateState {
  const location = useLocation();
  const [state, setState] = useState<GateState>({ kind: 'loading' });
  const here = location.pathname + location.search;
  useEffect(() => {
    const ctrl = new AbortController();
    getSession(ctrl.signal)
      .then((r) => {
        if (ctrl.signal.aborted) return;
        if (r.ok) setState({ kind: 'on', info: r.data });
        else if (r.status === 404) setState({ kind: 'off' });
        else if (r.status === 401) external.assign(loginURL(here));
        else setState({ kind: 'error' });
      })
      .catch(() => undefined); // 취소
    return () => ctrl.abort();
    // 조회는 화면을 처음 열 때 한 번만 한다(화면 이동마다 부르지 않는다). 그 뒤 만료는 API 401이 알린다
  }, []);
  // session으로 쓰는 중 만료·폐기(API 401)면 지금 위치로 돌아오는 로그인으로
  useEffect(() => {
    if (state.kind !== 'on') return;
    setUnauthenticatedHandler(() => external.assign(loginURL(window.location.pathname + window.location.search)));
    return () => setUnauthenticatedHandler(null);
  }, [state.kind]);
  return state;
}

/**
 * 조직 화면을 감싼다. 확인을 기다리는 동안과 확인 실패(5xx·연결 오류) 때는 화면을 그대로 그린다 —
 * 화면의 API 호출이 같은 서버 판단(401·403·503)을 받고 각자 상태로 보인다. 로그인이 필요하거나(401) 조직이 다르면 그때 바꾼다.
 */
export function SessionGate({ children }: { children: ReactNode }) {
  const { org = '' } = useParams();
  const state = useSessionState();
  if (state.kind !== 'on') return <>{children}</>;
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
  const state = useSessionState();
  if (state.kind === 'loading') return null;
  // 확인 실패(5xx·연결 오류)는 기본 조직으로 보낸다. 거기서 화면의 API 호출이 서버 판단을 받는다
  const org = state.kind === 'on' ? orgSlug(state.info) : fallbackOrg;
  return <Navigate to={orgPath(org, 'overview')} replace />;
}
