// 서명 secret 1회 표시 (ADR 0053 §1·0055 §3). 값은 이 컴포넌트 상태에만 있고, 닫으면 사라진다(저장·로그하지 않는다).
import { useEffect, useRef, useState } from 'react';

export function SecretReveal({ secret, keyId, onDone, doneLabel }: { secret: string; keyId: string; onDone: () => void; doneLabel: string }) {
  const [copied, setCopied] = useState<'idle' | 'ok' | 'failed'>('idle');
  // 나타나면 제목으로 focus(키보드·화면 낭독 사용자가 놓치지 않게). role=alert는 쓰지 않는다 — secret 값을 소리 내어 읽게 된다
  const title = useRef<HTMLElement>(null);
  useEffect(() => title.current?.focus(), []);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(secret);
      setCopied('ok');
    } catch {
      setCopied('failed');
    }
  };
  return (
    <section className="mt-notice mt-notice--warning mt-policy-secret" aria-labelledby="secret-title">
      <strong id="secret-title" ref={title} tabIndex={-1}>
        서명 secret — 지금만 볼 수 있습니다
      </strong>
      <span>receiver에 저장하세요. 이 화면을 떠나면 다시 볼 수 없고, 잃어버리면 secret을 교체해야 합니다. 복사했다면 저장한 뒤 클립보드를 비우세요.</span>
      <label className="mt-label" htmlFor="signing-secret">
        secret (key id {keyId})
      </label>
      <div className="mt-monitor-actions">
        <input id="signing-secret" className="mt-input mt-mono mt-policy-secret__value" readOnly value={secret} onFocus={(e) => e.currentTarget.select()} autoComplete="off" spellCheck={false} />
        <button type="button" className="mt-button" onClick={() => void copy()}>
          복사
        </button>
      </div>
      {copied === 'ok' && <span role="status">복사했습니다.</span>}
      {copied === 'failed' && <span role="status">복사하지 못했습니다. 위 값을 직접 선택해 복사하세요.</span>}
      <span className="mt-label">
        서명 검증: <code>Montracer-Signature</code>의 <code>v1=</code> 값(교체 중에는 쉼표로 여럿)이 HMAC-SHA256(secret,{' '}
        <code>timestamp.event_id.본문</code>)과 하나라도 같으면 받습니다. timestamp가 5분 넘게 차이 나면 거절합니다.
      </span>
      <div>
        <button type="button" className="mt-button mt-button--primary" onClick={onDone}>
          {doneLabel}
        </button>
      </div>
    </section>
  );
}
