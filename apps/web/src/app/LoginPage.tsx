// 로그인 화면 (ADR 0065). 비밀번호를 받지 않는다 — 조직의 IdP(OIDC)로 보낸다(ADR 0064).
// 실패 사유는 callback이 붙인 error 값이다. IdP 문구는 오지 않는다(서버가 옮기지 않는다).
import { useSearchParams } from 'react-router';
import { loginURL, safeReturnTo } from '../api/authState.ts';
import './session.css';

const ERRORS: Record<string, string> = {
  state_mismatch: '로그인 시간이 지났거나 다른 창에서 시작한 로그인입니다. 다시 로그인하세요.',
  idp_error: '로그인 서비스가 로그인을 마치지 않았습니다(취소했거나 거절됨).',
  token_invalid: '로그인 서비스의 응답을 확인하지 못했습니다. 다시 로그인하세요. 계속되면 관리자에게 알리세요.',
  no_membership: '이 계정은 아직 어느 조직의 member도 아닙니다. 조직 관리자에게 초대를 요청하세요.',
  unavailable: '로그인 서비스에 연결하지 못했습니다. 잠시 뒤 다시 시도하세요.',
  step_up_rejected:
    '추가 인증을 마치지 못했습니다. 같은 계정으로, 조직이 요구하는 MFA까지 다시 인증하세요. 로그인은 그대로 유지됩니다.',
  invitation_invalid: '초대가 없거나 이미 쓰였거나 만료됐습니다. 초대한 관리자에게 새 링크를 요청하세요.',
  session_loop:
    '로그인했지만 session을 받지 못했습니다. 브라우저가 보안 cookie를 막았을 수 있습니다(개인정보 보호 설정, http 주소). 설정을 확인한 뒤 다시 로그인하세요.',
};

export function LoginPage() {
  const [params] = useSearchParams();
  const error = params.get('error');
  const returnTo = params.get('return_to') ?? '/';
  const back = safeReturnTo(returnTo);
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
        {error === 'step_up_rejected' && back !== null && (
          // 재인증만 실패했다 — 로그인은 살아 있으니 하던 화면으로 돌아갈 수 있다. 같은 사이트 경로만(서버와 같은 규칙)
          <a href={back}>하던 화면으로 돌아가기</a>
        )}
        <p className="mt-muted">조직의 로그인 서비스(SSO)로 이동합니다.</p>
        <a className="mt-button mt-button--primary" href={loginURL(returnTo)}>
          SSO로 로그인
        </a>
      </div>
    </main>
  );
}
