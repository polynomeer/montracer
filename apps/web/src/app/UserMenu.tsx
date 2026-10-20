// 사용자 메뉴 (ADR 0065): 이름·역할, 조직 전환, 로그아웃. session 모드에서만 보인다.
// 조직 전환과 로그아웃은 화면 전체를 다시 불러온다 — 이전 조직의 조회 결과·상태가 남지 않게(D05 §01 조직 전환은 cache·query 취소).
import { useEffect, useId, useRef, useState } from 'react';
import { external } from '../api/authState.ts';
import { ROLE_LABEL, logout, orgSlug, switchTenant, type SessionInfo } from '../api/session.ts';
import { orgPath } from './nav.ts';
import './session.css';

export function UserMenu({ info }: { info: SessionInfo }) {
  const [open, setOpen] = useState(false);
  const [status, setStatus] = useState('');
  // 전환·로그아웃 요청이 진행 중이면 버튼을 막는다(두 번 눌러 POST가 두 번 가지 않게)
  const [busy, setBusy] = useState(false);
  const menuId = useId();
  const buttonRef = useRef<HTMLButtonElement>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const label = info.display_name ?? info.user_id;
  useEffect(() => {
    if (!open) return;
    // 열면 첫 버튼으로 focus. Esc는 닫고 메뉴 버튼으로 돌려준다. 바깥을 누르면 닫는다
    panelRef.current?.querySelector<HTMLElement>('button')?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setOpen(false);
        buttonRef.current?.focus();
      }
    };
    const onPointer = (e: PointerEvent) => {
      if (rootRef.current !== null && !rootRef.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('keydown', onKey);
    document.addEventListener('pointerdown', onPointer);
    return () => {
      document.removeEventListener('keydown', onKey);
      document.removeEventListener('pointerdown', onPointer);
    };
  }, [open]);
  const others = info.memberships.filter((m) => m.tenant_id !== info.tenant_id);
  return (
    <div ref={rootRef} className="mt-user-menu">
      <button
        ref={buttonRef}
        type="button"
        className="mt-button"
        aria-expanded={open}
        aria-controls={menuId}
        onClick={() => setOpen((v) => !v)}
      >
        {label} · {ROLE_LABEL[info.role]}
      </button>
      {open && (
        <div ref={panelRef} id={menuId} className="mt-card mt-user-menu__panel" role="group" aria-label="사용자 메뉴">
          <p className="mt-label">
            {orgSlug(info)} 조직 · {ROLE_LABEL[info.role]}
          </p>
          {others.length > 0 && (
            <section aria-label="조직 바꾸기">
              <h2 className="mt-label">조직 바꾸기</h2>
              <ul>
                {others.map((m) => (
                  <li key={m.tenant_id}>
                    <button
                      type="button"
                      className="mt-button mt-button--link"
                      disabled={busy}
                      onClick={() => {
                        setBusy(true);
                        setStatus('바꾸는 중…');
                        void switchTenant(m.tenant_id).then((r) => {
                          if (r.ok) external.assign(orgPath(orgSlug(r.data), 'overview'));
                          else {
                            setBusy(false);
                            setStatus('바꾸지 못했습니다.');
                          }
                        });
                      }}
                    >
                      {orgSlug(m)} ({ROLE_LABEL[m.role]})
                    </button>
                  </li>
                ))}
              </ul>
            </section>
          )}
          <button
            type="button"
            className="mt-button"
            disabled={busy}
            onClick={() => {
              setBusy(true);
              setStatus('로그아웃하는 중…');
              void logout().then((r) => {
                // 이미 끝난 session(401)도 로그아웃한 것과 같다
                if (r.ok || r.status === 401) external.assign('/login?logged_out=1');
                else {
                  setBusy(false);
                  setStatus('로그아웃하지 못했습니다.');
                }
              });
            }}
          >
            로그아웃
          </button>
          <span role="status" className="mt-label">
            {status}
          </span>
        </div>
      )}
    </div>
  );
}
