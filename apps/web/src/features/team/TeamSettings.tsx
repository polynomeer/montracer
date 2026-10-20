// 설정 > Team (ADR 0068, API는 ADR 0067). 멤버 목록·역할 변경·제거, 초대 링크 만들기·폐기.
// - 조회는 Tenant Admin·Security Auditor, 변경은 Tenant Admin + 15분 안의 MFA step-up이다(서버가 매번 다시 검사한다).
//   화면은 역할로 버튼을 고를 뿐이다 — 보안 경계가 아니다(D05 §04).
// - 초대 링크는 만든 응답에서 한 번만 보인다. 이 컴포넌트 상태에만 두고 닫으면 사라진다(저장·로그하지 않는다).
// - 서버 목록은 컴포넌트 상태에만 둔다(전역 store에 복사하지 않는다).
import { useEffect, useRef, useState } from 'react';
import { useOutletContext, useParams } from 'react-router';
import { external } from '../../api/authState.ts';
import type { ApiResult } from '../../api/client.ts';
import {
  createInvitation,
  INVITABLE_ROLES,
  isStepUpRequired,
  listInvitations,
  listMembers,
  removeMember,
  revokeInvitation,
  updateMemberRole,
  type InvitableRole,
  type InvitationCreated,
  type InvitationItem,
  type MemberItem,
  type MemberRole,
} from '../../api/members.ts';
import { ROLE_LABEL } from '../../api/session.ts';
import { useRemote, type RemoteState } from '../../api/useRemote.ts';
import type { InvestigationContext } from '../../app/context.ts';
import { useSession } from '../../app/SessionGate.tsx';
import { formatDateTime } from '../services/format.ts';
import { ErrorNotice, Skeleton } from '../services/states.tsx';
import { StepUpStatus } from './StepUp.tsx';
import '../services/services.css';
import '../monitors/monitors.css';
import './team.css';

const ROLES: readonly MemberRole[] = ['viewer', 'developer', 'operator', 'tenant_admin', 'security_auditor'];

type Notice = { tone: 'success' | 'warning' | 'critical'; text: string } | null;

/** 변경 실패를 사람이 읽을 문장으로. step-up 거절은 따로 다룬다(호출자가 isStepUpRequired로 먼저 본다). */
export function mutationError(r: Extract<ApiResult<unknown>, { ok: false }>): string {
  switch (r.status) {
    case 409:
      return r.error.message !== '' ? r.error.message : '지금 상태에서는 할 수 없습니다.';
    case 412:
      return '다른 관리자가 먼저 바꿨습니다. 목록을 새로 불러온 뒤 다시 시도하세요.';
    case 404:
      return '대상이 없습니다(이미 빠졌거나 쓰였습니다). 목록을 새로 불러오세요.';
    case 403:
      return '이 작업을 할 권한이 없습니다(Tenant Admin만 할 수 있습니다).';
    case 0:
      return '서버에 연결할 수 없습니다.';
    default:
      return r.status >= 500 ? '일시적으로 처리하지 못했습니다. 잠시 뒤 다시 시도하세요.' : r.error.message;
  }
}

export function TeamSettings() {
  const { org = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const session = useSession();
  // session이 없으면(API key 모드) 역할을 모른다 — 버튼은 보이고 서버가 판단한다
  const canManage = session === null || session.role === 'tenant_admin';
  const [tick, setTick] = useState(0);
  const refresh = () => setTick((n) => n + 1);
  const members = useRemote(`${org}|members`, tick, (signal) => listMembers(signal));
  const invitations = useRemote(`${org}|invitations`, tick, (signal) => listInvitations(signal));
  const [stepUpRequired, setStepUpRequired] = useState(false);
  const [notice, setNotice] = useState<Notice>(null);
  const [created, setCreated] = useState<InvitationCreated | null>(null);

  /** 변경 결과를 반영한다. 성공이면 목록을 다시 읽는다. */
  const settle = (r: ApiResult<unknown>, ok: string): boolean => {
    if (r.ok) {
      setNotice({ tone: 'success', text: ok });
      refresh();
      return true;
    }
    if (isStepUpRequired(r)) {
      setStepUpRequired(true);
      setNotice(null);
      return false;
    }
    setNotice({ tone: r.status === 412 || r.status === 409 ? 'warning' : 'critical', text: mutationError(r) });
    if (r.status === 412 || r.status === 404) refresh();
    return false;
  };

  const status = members.error?.status;
  return (
    <section className="mt-page mt-team" aria-labelledby="team-title">
      <header className="mt-page__header">
        <h1 id="team-title" className="mt-title">
          Team
        </h1>
        <span className="mt-label">조직의 멤버와 초대</span>
        <div className="mt-page__action mt-monitor-actions">
          <button type="button" className="mt-button" onClick={refresh}>
            새로고침
          </button>
        </div>
      </header>
      {status === 404 ? (
        <div className="mt-empty">
          <strong>이 배포에서는 멤버 관리를 쓸 수 없습니다</strong>
          <span>멤버 API가 꺼져 있습니다(ADR 0067 Rollback). 운영팀에 문의하세요.</span>
        </div>
      ) : status === 403 ? (
        <div className="mt-empty">
          <strong>멤버 목록을 볼 권한이 없습니다</strong>
          <span>Tenant Admin 또는 Security Auditor만 볼 수 있습니다.</span>
        </div>
      ) : (
        <>
          {canManage && session !== null && <StepUpStatus expiresAt={session.step_up_expires_at} required={stepUpRequired} timeZone={ctx.timeZone} />}
          {canManage && session === null && stepUpRequired && (
            <p className="mt-notice mt-notice--warning" role="alert">
              이 작업에는 사람 로그인과 추가 인증(MFA)이 필요합니다. API key로는 멤버를 바꿀 수 없습니다.
            </p>
          )}
          {!canManage && (
            <p className="mt-label" role="note">
              Security Auditor는 멤버를 볼 수만 있습니다. 바꾸기는 Tenant Admin이 합니다.
            </p>
          )}
          {notice !== null && (
            <p className={`mt-notice mt-notice--${notice.tone}`} role={notice.tone === 'success' ? 'status' : 'alert'}>
              {notice.text}
            </p>
          )}
          {members.error !== null && (
            <>
              <ErrorNotice error={members.error} lastSuccessMs={members.lastSuccessMs} timeZone={ctx.timeZone} onRetry={refresh} />
              {members.data === null && <p className="mt-label">멤버 목록을 불러오지 못했습니다. 멤버가 없다는 뜻이 아닙니다.</p>}
            </>
          )}
          {members.refreshing && (
            <p className="mt-label" role="status">
              다시 불러오는 중
            </p>
          )}
          {members.data === null ? (
            members.error === null && <Skeleton height={200} label="멤버 목록을 불러오는 중" />
          ) : (
            <MemberTable
              members={members.data}
              truncated={members.nextCursor !== null}
              canManage={canManage}
              timeZone={ctx.timeZone}
              onRole={async (m, role) => {
                const ok = settle(await updateMemberRole(m.user_id, role, m.revision), `${memberName(m)}의 역할을 ${ROLE_LABEL[role]}(으)로 바꿨습니다.`);
                // 내 역할이 바뀌면 session(메뉴·버튼·사용자 메뉴의 역할)을 다시 읽는다 — 전체 다시 불러오기(ADR 0065)
                if (ok && m.is_self) external.assign(window.location.pathname + window.location.search);
                return ok;
              }}
              onRemove={async (m) => settle(await removeMember(m.user_id, m.revision), `${memberName(m)}을(를) 조직에서 뺐습니다.`)}
            />
          )}
          {canManage && (
            <InviteForm
              created={created}
              onCreate={async (role, label) => {
                const r = await createInvitation(role, label, crypto.randomUUID());
                const ok = settle(r, '초대 링크를 만들었습니다.');
                if (ok && r.ok) setCreated(r.data);
                return ok;
              }}
              onDone={() => setCreated(null)}
            />
          )}
          <InvitationTable
            remote={invitations}
            canManage={canManage}
            timeZone={ctx.timeZone}
            onRevoke={async (id) => settle(await revokeInvitation(id), '초대를 폐기했습니다. 그 링크로는 더 들어올 수 없습니다.')}
            onRetry={refresh}
          />
        </>
      )}
    </section>
  );
}

function memberName(m: MemberItem): string {
  return m.display_name ?? m.user_id;
}

function MemberTable({
  members,
  truncated,
  canManage,
  timeZone,
  onRole,
  onRemove,
}: {
  members: MemberItem[];
  truncated: boolean;
  canManage: boolean;
  timeZone: string;
  onRole: (m: MemberItem, role: MemberRole) => Promise<boolean>;
  onRemove: (m: MemberItem) => Promise<boolean>;
}) {
  return (
    <section className="mt-card mt-card--flush" aria-labelledby="members-title">
      <h2 id="members-title" className="mt-subtitle mt-team__heading">
        {truncated ? `멤버 ${members.length.toLocaleString('ko-KR')}명 이상` : `멤버 ${String(members.length)}명`}
      </h2>
      {truncated && (
        <p className="mt-label mt-monitor-note" role="note">
          처음 1,000명만 보입니다(user id 순).
        </p>
      )}
      <div className="mt-scroll-x">
        <table className="mt-table">
          <caption className="mt-visually-hidden">조직 멤버(user id 순)</caption>
          <thead>
            <tr>
              <th scope="col">이름</th>
              <th scope="col">역할</th>
              <th scope="col">마지막 로그인</th>
              {canManage && <th scope="col">동작</th>}
            </tr>
          </thead>
          <tbody>
            {members.map((m) => (
              <MemberRow key={m.user_id} member={m} canManage={canManage} timeZone={timeZone} onRole={onRole} onRemove={onRemove} />
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

function MemberRow({
  member: m,
  canManage,
  timeZone,
  onRole,
  onRemove,
}: {
  member: MemberItem;
  canManage: boolean;
  timeZone: string;
  onRole: (m: MemberItem, role: MemberRole) => Promise<boolean>;
  onRemove: (m: MemberItem) => Promise<boolean>;
}) {
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);
  // 역할은 고른 뒤 "적용"을 눌러야 보낸다. 닫힌 select에서 화살표 키만 눌러도 change가 나는 브라우저가 있다
  // (Windows Chrome·Edge) — 한 번 누름이 곧바로 권한 부여(예: Operator → Tenant Admin)가 되지 않게
  const [pendingRole, setPendingRole] = useState<MemberRole | null>(null);
  const selectRef = useRef<HTMLSelectElement>(null);
  const removeRef = useRef<HTMLButtonElement>(null);
  const roleConfirmRef = useRef<HTMLButtonElement>(null);
  const removeConfirmRef = useRef<HTMLButtonElement>(null);
  // 확인 단계가 열리면 첫 버튼으로, 닫히면 연 쪽으로 focus를 옮긴다(누른 버튼이 사라져 focus를 잃지 않게)
  useEffect(() => {
    if (pendingRole !== null) roleConfirmRef.current?.focus();
  }, [pendingRole]);
  useEffect(() => {
    if (confirming) removeConfirmRef.current?.focus();
  }, [confirming]);
  // 보내는 동안 select가 비활성이라 바로 focus할 수 없다 — 끝난 뒤 돌려준다
  const [refocus, setRefocus] = useState(false);
  useEffect(() => {
    if (refocus && !busy) {
      selectRef.current?.focus();
      setRefocus(false);
    }
  }, [refocus, busy]);
  const selectId = `role-${m.user_id}`;
  const cancelRole = () => {
    setPendingRole(null);
    selectRef.current?.focus();
  };
  const applyRole = (role: MemberRole) => {
    setBusy(true);
    void onRole(m, role).finally(() => {
      setBusy(false);
      setPendingRole(null);
      setRefocus(true);
    });
  };
  const cancelRemove = () => {
    setConfirming(false);
    removeRef.current?.focus();
  };
  const roleWarning = (role: MemberRole) =>
    m.is_self
      ? `내 역할을 ${ROLE_LABEL[role]}(으)로 바꾸면 이 화면의 관리 권한을 잃을 수 있습니다.`
      : role === 'tenant_admin'
        ? `${memberName(m)}에게 Tenant Admin(멤버·key·삭제 관리) 권한을 줍니다.`
        : `${memberName(m)}의 역할을 ${ROLE_LABEL[role]}(으)로 바꿉니다.`;
  return (
    <tr>
      <th scope="row">
        <span className="mt-team__name">
          {memberName(m)}
          {m.is_self && <span className="mt-muted"> (나)</span>}
        </span>
        {m.display_name !== null && <span className="mt-label mt-mono mt-team__id">{m.user_id}</span>}
      </th>
      <td>
        {canManage ? (
          <>
            <label className="mt-visually-hidden" htmlFor={selectId}>
              {memberName(m)} 역할
            </label>
            <select
              id={selectId}
              ref={selectRef}
              className="mt-input"
              value={pendingRole ?? m.role}
              disabled={busy}
              onChange={(e) => {
                const role = e.currentTarget.value as MemberRole;
                setPendingRole(role === m.role ? null : role);
              }}
            >
              {ROLES.map((r) => (
                <option key={r} value={r}>
                  {ROLE_LABEL[r]}
                </option>
              ))}
            </select>
            {pendingRole !== null && (
              <span className="mt-monitor-actions" role="group" aria-label={`${memberName(m)} 역할 바꾸기 확인`}>
                <span>{roleWarning(pendingRole)}</span>
                <button ref={roleConfirmRef} type="button" className="mt-button mt-button--primary" disabled={busy} onClick={() => applyRole(pendingRole)}>
                  적용
                </button>
                <button type="button" className="mt-button" disabled={busy} onClick={cancelRole}>
                  취소
                </button>
              </span>
            )}
          </>
        ) : (
          ROLE_LABEL[m.role]
        )}
      </td>
      <td className="mt-nowrap">{m.last_login_at === null ? <span className="mt-muted">로그인 기록 없음</span> : formatDateTime(m.last_login_at, timeZone)}</td>
      {canManage && (
        <td className="mt-nowrap">
          {confirming ? (
            <span className="mt-monitor-actions" role="group" aria-label={`${memberName(m)} 빼기 확인`}>
              <span>{m.is_self ? '나를 빼면 이 조직에 다시 들어올 수 없습니다. 뺄까요?' : '조직에서 뺄까요? 그 사람의 로그인이 끝납니다.'}</span>
              <button
                ref={removeConfirmRef}
                type="button"
                className="mt-button mt-button--danger"
                disabled={busy}
                onClick={() => {
                  setBusy(true);
                  void onRemove(m).then((ok) => {
                    setBusy(false);
                    // 성공하면 행이 사라진다. 실패면 확인을 닫고 연 버튼으로 돌아간다
                    if (!ok) cancelRemove();
                  });
                }}
              >
                빼기
              </button>
              <button type="button" className="mt-button" disabled={busy} onClick={cancelRemove}>
                취소
              </button>
            </span>
          ) : (
            <button ref={removeRef} type="button" className="mt-button" onClick={() => setConfirming(true)}>
              빼기…
            </button>
          )}
        </td>
      )}
    </tr>
  );
}

function InviteForm({
  created,
  onCreate,
  onDone,
}: {
  created: InvitationCreated | null;
  onCreate: (role: InvitableRole, label: string | null) => Promise<boolean>;
  onDone: () => void;
}) {
  const [role, setRole] = useState<InvitableRole>('viewer');
  const [label, setLabel] = useState('');
  const [busy, setBusy] = useState(false);
  const trimmed = label.trim();
  const labelTooLong = [...trimmed].length > 64;
  return (
    <section className="mt-card" aria-labelledby="invite-title">
      <h2 id="invite-title" className="mt-subtitle">
        초대
      </h2>
      <p className="mt-label">
        링크는 7일 동안 한 번 쓸 수 있습니다. 받은 사람은 조직의 로그인 서비스로 로그인하면 선택한 역할로 들어옵니다. Tenant Admin은 들어온 뒤 역할을 바꿔 줍니다.
      </p>
      {created !== null ? (
        <InvitationLinkReveal created={created} onDone={onDone} />
      ) : (
        <form
          className="mt-team__invite"
          onSubmit={(e) => {
            e.preventDefault();
            if (labelTooLong) return;
            setBusy(true);
            void onCreate(role, trimmed === '' ? null : trimmed).then((ok) => {
              setBusy(false);
              if (ok) setLabel(''); // 실패면 쓴 메모를 남긴다
            });
          }}
        >
          <label className="mt-label" htmlFor="invite-role">
            역할
          </label>
          <select id="invite-role" className="mt-input" value={role} onChange={(e) => setRole(e.currentTarget.value as InvitableRole)}>
            {INVITABLE_ROLES.map((r) => (
              <option key={r} value={r}>
                {ROLE_LABEL[r]}
              </option>
            ))}
          </select>
          <label className="mt-label" htmlFor="invite-label">
            메모(선택, 64자까지 — 이 초대를 구분할 때만 씁니다)
          </label>
          <input
            id="invite-label"
            className="mt-input"
            value={label}
            maxLength={128}
            aria-invalid={labelTooLong}
            aria-describedby={labelTooLong ? 'invite-label-error' : undefined}
            onChange={(e) => setLabel(e.currentTarget.value)}
          />
          {labelTooLong && (
            <span id="invite-label-error" className="mt-label" role="alert">
              메모는 64자까지입니다.
            </span>
          )}
          <div>
            <button type="submit" className="mt-button mt-button--primary" disabled={busy || labelTooLong}>
              초대 링크 만들기
            </button>
          </div>
        </form>
      )}
    </section>
  );
}

/** 초대 링크 1회 표시. 값은 이 컴포넌트 상태에만 있고, 닫으면 사라진다. */
function InvitationLinkReveal({ created, onDone }: { created: InvitationCreated; onDone: () => void }) {
  const [copied, setCopied] = useState<'idle' | 'ok' | 'failed'>('idle');
  // 나타나면 제목으로 focus. role=alert는 쓰지 않는다 — 링크를 소리 내어 읽게 된다
  const title = useRef<HTMLElement>(null);
  useEffect(() => title.current?.focus(), []);
  const url = created.invitationURL;
  return (
    <section className="mt-notice mt-notice--warning mt-policy-secret" aria-labelledby="invite-link-title">
      <strong id="invite-link-title" ref={title} tabIndex={-1}>
        초대 링크 — 지금만 볼 수 있습니다
      </strong>
      {url === null ? (
        <span>같은 요청을 다시 보내 링크를 받지 못했습니다. 이 초대를 폐기하고 새로 만드세요.</span>
      ) : (
        <>
          <span>
            {ROLE_LABEL[created.data.role]} 역할 초대입니다. 들어올 사람에게만 전하세요 — 링크를 가진 누구나 쓸 수 있습니다. 이 화면을 떠나면 다시 볼 수 없습니다.
          </span>
          <label className="mt-label" htmlFor="invitation-url">
            초대 링크
          </label>
          <div className="mt-monitor-actions">
            <input id="invitation-url" className="mt-input mt-mono mt-policy-secret__value" readOnly value={url} onFocus={(e) => e.currentTarget.select()} autoComplete="off" spellCheck={false} />
            <button
              type="button"
              className="mt-button"
              onClick={() => {
                void navigator.clipboard.writeText(url).then(
                  () => setCopied('ok'),
                  () => setCopied('failed'),
                );
              }}
            >
              복사
            </button>
          </div>
          {copied === 'ok' && <span role="status">복사했습니다.</span>}
          {copied === 'failed' && <span role="status">복사하지 못했습니다. 위 값을 직접 선택해 복사하세요.</span>}
        </>
      )}
      <div>
        <button type="button" className="mt-button mt-button--primary" onClick={onDone}>
          닫기
        </button>
      </div>
    </section>
  );
}

function InvitationTable({
  remote,
  canManage,
  timeZone,
  onRevoke,
  onRetry,
}: {
  remote: RemoteState<InvitationItem[]>;
  canManage: boolean;
  timeZone: string;
  onRevoke: (id: string) => Promise<boolean>;
  onRetry: () => void;
}) {
  const [busy, setBusy] = useState<string | null>(null);
  return (
    <section className="mt-card mt-card--flush" aria-labelledby="pending-title">
      <h2 id="pending-title" className="mt-subtitle mt-team__heading">
        대기 중인 초대
      </h2>
      {remote.error !== null && (
        <div className="mt-team__empty">
          <ErrorNotice error={remote.error} lastSuccessMs={remote.lastSuccessMs} timeZone={timeZone} onRetry={onRetry} />
          {remote.data === null && <p className="mt-label">대기 중인 초대를 불러오지 못했습니다. 초대가 없다는 뜻이 아닙니다.</p>}
        </div>
      )}
      {remote.refreshing && (
        <p className="mt-label mt-team__empty" role="status">
          다시 불러오는 중
        </p>
      )}
      {remote.data === null ? (
        remote.error === null && <Skeleton height={80} label="대기 중인 초대를 불러오는 중" />
      ) : remote.data.length === 0 ? (
        <p className="mt-label mt-team__empty">대기 중인 초대가 없습니다.</p>
      ) : (
        <div className="mt-scroll-x">
          <table className="mt-table">
            <caption className="mt-visually-hidden">대기 중인 초대(만든 순)</caption>
            <thead>
              <tr>
                <th scope="col">역할</th>
                <th scope="col">메모</th>
                <th scope="col">만든 사람</th>
                <th scope="col">만료</th>
                {canManage && <th scope="col">동작</th>}
              </tr>
            </thead>
            <tbody>
              {remote.data.map((inv) => (
                <tr key={inv.id}>
                  <td>{ROLE_LABEL[inv.role]}</td>
                  <td>{inv.label ?? <span className="mt-muted">—</span>}</td>
                  <td className="mt-mono">{inv.created_by}</td>
                  <td className="mt-nowrap">{formatDateTime(inv.expires_at, timeZone)}</td>
                  {canManage && (
                    <td>
                      <button
                        type="button"
                        className="mt-button"
                        disabled={busy === inv.id}
                        onClick={() => {
                          setBusy(inv.id);
                          void onRevoke(inv.id).finally(() => setBusy(null));
                        }}
                      >
                        폐기
                      </button>
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
