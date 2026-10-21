// MFA step-up 안내 (ADR 0066). 민감 작업(멤버·key·삭제)은 15분 안에 IdP에서 다시 인증해야 한다.
// 재인증은 IdP 화면으로 이동했다가 지금 위치로 돌아온다(전체 다시 불러오기 — 새 step-up 시각을 session에서 다시 읽는다).
import { useEffect, useState } from 'react';
import { useLocation } from 'react-router';
import { external } from '../../api/authState.ts';
import { startStepUp } from '../../api/members.ts';
import { formatDateTime } from '../services/format.ts';

/** step-up이 지금 유효한지(만료 시각이 미래). */
export function stepUpActive(expiresAt: string | null | undefined, nowMs: number): boolean {
  if (expiresAt === null || expiresAt === undefined) return false;
  const t = Date.parse(expiresAt);
  return Number.isFinite(t) && t > nowMs;
}

/** 재인증 버튼. 누르면 IdP 재인증 URL을 받아 이동한다. 실패하면 이유를 보인다. */
export function StepUpButton({ label = '추가 인증(MFA)' }: { label?: string }) {
  const location = useLocation();
  const [state, setState] = useState<'idle' | 'busy' | 'failed'>('idle');
  return (
    <>
      <button
        type="button"
        className="mt-button mt-button--primary"
        disabled={state === 'busy'}
        onClick={() => {
          setState('busy');
          void startStepUp(location.pathname + location.search).then((r) => {
            if (r.ok) external.assign(r.data.authorization_url);
            else setState('failed');
          });
        }}
      >
        {label}
      </button>
      {state === 'failed' && <span role="status">추가 인증을 시작하지 못했습니다. 잠시 뒤 다시 시도하세요.</span>}
    </>
  );
}

/**
 * 관리 화면 위의 step-up 상태. required면(서버가 step_up_required로 거절) 경고로, 아니면 안내로 보인다.
 * expiresAt이 유효하면 언제까지인지만 알린다.
 */
export function StepUpStatus({ expiresAt, required, timeZone }: { expiresAt: string | null | undefined; required: boolean; timeZone: string }) {
  const [now, setNow] = useState(() => Date.now());
  // 만료 시각이 되면 다시 그린다("유효합니다"가 지난 뒤에도 남지 않게)
  useEffect(() => {
    if (!stepUpActive(expiresAt, now) || expiresAt == null) return;
    // setTimeout 상한(약 24.8일)을 넘는 지연은 바로 불리므로 자른다
    const delay = Math.min(Math.max(0, Date.parse(expiresAt) - Date.now()) + 50, 2_000_000_000);
    const id = window.setTimeout(() => setNow(Date.now()), delay);
    return () => window.clearTimeout(id);
  }, [expiresAt, now]);
  if (!required && stepUpActive(expiresAt, now) && expiresAt != null) {
    return (
      <p className="mt-label" role="status">
        추가 인증이 {formatDateTime(expiresAt, timeZone)}까지 유효합니다.
      </p>
    );
  }
  return (
    <div className={`mt-notice ${required ? 'mt-notice--warning' : 'mt-notice--info'}`} role={required ? 'alert' : 'note'}>
      <strong>{required ? '이 작업에는 추가 인증이 필요합니다' : '멤버를 바꾸려면 추가 인증이 필요합니다'}</strong>
      <span>조직의 로그인 서비스에서 다시 인증(MFA)하면 15분 동안 멤버 초대·역할 변경·제거를 할 수 있습니다.</span>
      <div>
        <StepUpButton />
      </div>
    </div>
  );
}
