// 로그인 화면 (ADR 0065). 비밀번호를 받지 않는다 — 조직의 IdP(OIDC)로 보낸다(ADR 0064).
// 실패 사유는 callback이 붙인 error 값이다. IdP 문구는 오지 않는다(서버가 옮기지 않는다).
import { useSearchParams } from 'react-router';
import { loginURL } from '../api/authState.ts';
import './session.css';

const ERRORS: Record<string, string> = {
  state_mismatch: '로그인 시간이 지났거나 다른 창에서 시작한 로그인입니다. 다시 로그인하세요.',
  idp_error: '로그인 서비스가 로그인을 마치지 않았습니다(취소했거나 거절됨).',
  token_invalid: '로그인 서비스의 응답을 확인하지 못했습니다. 다시 로그인하세요. 계속되면 관리자에게 알리세요.',
  no_membership: '이 계정은 아직 어느 조직의 member도 아닙니다. 조직 관리자에게 초대를 요청하세요.',
  unavailable: '로그인 서비스에 연결하지 못했습니다. 잠시 뒤 다시 시도하세요.',
};

export function LoginPage() {
  const [params] = useSearchParams();
  const error = params.get('error');
  const returnTo = params.get('return_to') ?? '/';
  return (
    <main id="main" className="mt-login" aria-labelledby="login-title">
      <div className="mt-card mt-login__card">
        <h1 id="login-title" className="mt-title">
          Montracer 로그인
        </h1>
        {params.get('logged_out') === '1' && (
          <p role="status" className="mt-notice mt-notice--info">
            로그아웃했습니다.
          </p>
        )}
        {error !== null && (
          <p role="alert" className="mt-notice mt-notice--warning">
            {ERRORS[error] ?? '로그인하지 못했습니다. 다시 시도하세요.'}
          </p>
        )}
        <p className="mt-muted">조직의 로그인 서비스(SSO)로 이동합니다.</p>
        <a className="mt-button mt-button--primary" href={loginURL(returnTo)}>
          SSO로 로그인
        </a>
      </div>
    </main>
  );
}
