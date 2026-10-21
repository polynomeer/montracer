// 초대 수락 페이지 /invite#mti_… (ADR 0067·0068). token은 URL fragment에만 있다 — 서버 로그·Referer로 가지 않는다.
// 1) fragment를 읽고 바로 주소에서 지운다(뒤로 가기·화면 공유에 남지 않게).
// 2) token을 본문으로 보내 들어갈 조직·역할을 받는다. 3) 사용자가 확인한 뒤에만 조직의 로그인 서비스로 이동한다
//    (남이 보낸 링크로 모르는 사이 다른 조직에 들어가지 않게, ADR 0067 §5).
import { useEffect, useState } from 'react';
import { Link } from 'react-router';
import { external } from '../api/authState.ts';
import { startInvitation, type InvitationStart } from '../api/members.ts';
import { ROLE_LABEL } from '../api/session.ts';
import './session.css';

type State =
  | { kind: 'loading' }
  | { kind: 'ready'; start: InvitationStart }
  | { kind: 'invalid' }
  | { kind: 'forbidden' }
  | { kind: 'error' };

function readToken(): string {
  return window.location.hash.startsWith('#') ? window.location.hash.slice(1) : '';
}

export function InvitePage() {
  const [token] = useState(readToken);
  const [state, setState] = useState<State>(token === '' ? { kind: 'invalid' } : { kind: 'loading' });
  useEffect(() => {
    // 주소에서 token을 지운다(이 컴포넌트 상태에만 남는다)
    if (window.location.hash !== '') window.history.replaceState(window.history.state, '', window.location.pathname);
    if (token === '') return;
    const ctrl = new AbortController();
    void startInvitation(token, ctrl.signal)
      .then((r) => {
        if (ctrl.signal.aborted) return;
        if (r.ok) setState({ kind: 'ready', start: r.data });
        else if (r.status === 404 || r.status === 400) setState({ kind: 'invalid' });
        else if (r.status === 403) setState({ kind: 'forbidden' });
        else setState({ kind: 'error' });
      })
      .catch(() => undefined); // 취소
    return () => ctrl.abort();
  }, [token]);

  return (
    <main id="main" className="mt-login" aria-labelledby="invite-title">
      <div className="mt-card mt-login__card">
        <h1 id="invite-title" className="mt-title">
          조직 초대
        </h1>
        {state.kind === 'loading' && <p role="status">초대를 확인하는 중</p>}
        {state.kind === 'invalid' && (
          <p role="alert" className="mt-notice mt-notice--warning">
            초대가 없거나 이미 쓰였거나 만료됐습니다. 초대한 관리자에게 새 링크를 요청하세요.
          </p>
        )}
        {state.kind === 'forbidden' && (
          <p role="alert" className="mt-notice mt-notice--warning">
            이 주소에서는 초대를 열 수 없습니다. 받은 링크를 브라우저 주소창에 그대로 붙여 넣어 여세요.
          </p>
        )}
        {state.kind === 'error' && (
          <p role="alert" className="mt-notice mt-notice--critical">
            초대를 확인하지 못했습니다. 잠시 뒤 받은 링크를 다시 여세요.
          </p>
        )}
        {state.kind === 'ready' && (
          <>
            <p>
              <strong>{state.start.tenant_name ?? '이름이 없는 조직'}</strong> 조직에 <strong>{ROLE_LABEL[state.start.role]}</strong>(으)로 들어갑니다.
            </p>
            <p className="mt-muted">
              조직의 로그인 서비스(SSO)로 로그인하면 바로 들어갑니다. 모르는 조직이거나 요청한 적 없는 초대라면 들어가지 마세요.
            </p>
            <button type="button" className="mt-button mt-button--primary" onClick={() => external.assign(state.start.authorization_url)}>
              참여하고 로그인
            </button>
          </>
        )}
        <Link to="/">들어가지 않고 돌아가기</Link>
      </div>
    </main>
  );
}
