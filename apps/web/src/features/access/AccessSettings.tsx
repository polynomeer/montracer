// 설정 > Access (ADR 0070, API는 ADR 0069). API key 목록·발급(원문 1회)·폐기(바로 또는 교체 겹침).
// - 조회는 Tenant Admin, 발급·폐기는 Tenant Admin + 15분 안의 MFA step-up이다(서버가 매번 다시 검사한다).
// - 원문 token은 발급 응답에서 한 번만 보인다. 이 컴포넌트 상태에만 두고 닫으면 사라진다(저장·로그하지 않는다).
// - 서버 목록은 컴포넌트 상태에만 둔다(전역 store에 복사하지 않는다).
import { useEffect, useRef, useState } from 'react';
import { useOutletContext, useParams } from 'react-router';
import type { ApiResult } from '../../api/client.ts';
import { API_SCOPES, createKey, INGEST_SCOPES, keyStatus, listKeys, revokeKey, type KeyCreated, type KeyInput, type KeyItem, type KeyKind } from '../../api/keys.ts';
import { isStepUpRequired } from '../../api/members.ts';
import { useRemote } from '../../api/useRemote.ts';
import type { InvestigationContext } from '../../app/context.ts';
import { useSession } from '../../app/SessionGate.tsx';
import { formatDateTime } from '../services/format.ts';
import { ErrorNotice, Skeleton, StatusBadge } from '../services/states.tsx';
import { StepUpStatus } from '../team/StepUp.tsx';
import '../services/services.css';
import '../monitors/monitors.css';
import '../notifications/notifications.css';
import '../team/team.css';

type Notice = { tone: 'success' | 'warning' | 'critical'; text: string } | null;

const LIFETIMES = [30, 90, 180, 365] as const;
const DEFAULT_LIFETIME: Record<KeyKind, number> = { ingest: 365, api: 90 };
const ENV_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

/** 쉼표·공백으로 나눈 environment 목록. 형식이 틀린 값이 있으면 그 값을 돌려준다. */
export function parseEnvironments(text: string): { envs: string[]; invalid: string | null } {
  const envs: string[] = [];
  for (const raw of text.split(/[\s,]+/)) {
    if (raw === '') continue;
    if (!ENV_PATTERN.test(raw)) return { envs, invalid: raw };
    if (!envs.includes(raw)) envs.push(raw);
  }
  return { envs, invalid: null };
}

function failure(r: Extract<ApiResult<unknown>, { ok: false }>): string {
  switch (r.status) {
    case 409:
      return r.error.message !== '' ? r.error.message : '지금 상태에서는 할 수 없습니다.';
    case 404:
      return '대상이 없습니다(이미 폐기됐습니다). 목록을 새로 불러오세요.';
    case 403:
      return '이 작업을 할 권한이 없습니다(Tenant Admin만 할 수 있고, 내가 가진 권한보다 넓은 key는 만들 수 없습니다).';
    case 400:
      return `요청 값이 올바르지 않습니다: ${r.error.message}`;
    case 0:
      return '서버에 연결할 수 없습니다.';
    default:
      return r.status >= 500 ? '일시적으로 처리하지 못했습니다. 잠시 뒤 다시 시도하세요.' : r.error.message;
  }
}

export function AccessSettings() {
  const { org = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const session = useSession();
  const [tick, setTick] = useState(0);
  const refresh = () => setTick((n) => n + 1);
  const keys = useRemote(`${org}|keys`, tick, (signal) => listKeys(signal));
  const [stepUpRequired, setStepUpRequired] = useState(false);
  const [notice, setNotice] = useState<Notice>(null);
  const [created, setCreated] = useState<KeyCreated | null>(null);

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
    setNotice({ tone: r.status === 409 ? 'warning' : 'critical', text: failure(r) });
    if (r.status === 404) refresh();
    return false;
  };

  const status = keys.error?.status;
  return (
    <section className="mt-page mt-team" aria-labelledby="access-title">
      <header className="mt-page__header">
        <h1 id="access-title" className="mt-title">
          Access
        </h1>
        <span className="mt-label">수집기·자동화용 API key</span>
        <div className="mt-page__action mt-monitor-actions">
          <button type="button" className="mt-button" onClick={refresh}>
            새로고침
          </button>
        </div>
      </header>
      {status === 404 ? (
        <div className="mt-empty">
          <strong>이 배포에서는 key 관리를 쓸 수 없습니다</strong>
          <span>key API가 꺼져 있습니다(ADR 0069 Rollback). 운영팀에 문의하세요.</span>
        </div>
      ) : status === 403 ? (
        <div className="mt-empty">
          <strong>key를 볼 권한이 없습니다</strong>
          <span>Tenant Admin만 볼 수 있습니다.</span>
        </div>
      ) : (
        <>
          {session !== null && (
            <StepUpStatus
              expiresAt={session.step_up_expires_at}
              required={stepUpRequired}
              timeZone={ctx.timeZone}
              subject="key를 발급·폐기하려면"
              actions="key 발급·폐기"
            />
          )}
          {session === null && stepUpRequired && (
            <p className="mt-notice mt-notice--warning" role="alert">
              이 작업에는 사람 로그인과 추가 인증(MFA)이 필요합니다. API key로는 key를 발급·폐기할 수 없습니다.
            </p>
          )}
          {notice !== null && (
            <p className={`mt-notice mt-notice--${notice.tone}`} role={notice.tone === 'success' ? 'status' : 'alert'}>
              {notice.text}
            </p>
          )}
          <KeyForm
            created={created}
            onCreate={async (input) => {
              const r = await createKey(input, crypto.randomUUID());
              const ok = settle(r, 'key를 발급했습니다.');
              if (ok && r.ok) setCreated(r.data);
              return ok;
            }}
            onDone={() => setCreated(null)}
          />
          {keys.error !== null && (
            <>
              <ErrorNotice error={keys.error} lastSuccessMs={keys.lastSuccessMs} timeZone={ctx.timeZone} onRetry={refresh} />
              {keys.data === null && <p className="mt-label">key 목록을 불러오지 못했습니다. key가 없다는 뜻이 아닙니다.</p>}
            </>
          )}
          {keys.refreshing && (
            <p className="mt-label" role="status">
              다시 불러오는 중
            </p>
          )}
          {keys.data === null ? (
            keys.error === null && <Skeleton height={200} label="key 목록을 불러오는 중" />
          ) : (
            <KeyTable
              keys={keys.data}
              timeZone={ctx.timeZone}
              onRevoke={async (k, grace) =>
                settle(
                  await revokeKey(k.key_id, grace),
                  grace === 0 ? `${k.name ?? k.token_hint}을(를) 폐기했습니다.` : `${k.name ?? k.token_hint}은(는) ${grace / 3600}시간 뒤 폐기됩니다.`,
                )
              }
            />
          )}
        </>
      )}
    </section>
  );
}

function KeyForm({ created, onCreate, onDone }: { created: KeyCreated | null; onCreate: (input: KeyInput) => Promise<boolean>; onDone: () => void }) {
  const [kind, setKind] = useState<KeyKind>('ingest');
  const [name, setName] = useState('');
  const [scopes, setScopes] = useState<string[]>(['ingest.traces', 'ingest.metrics', 'ingest.logs']);
  const [envText, setEnvText] = useState('');
  const [days, setDays] = useState<number>(DEFAULT_LIFETIME.ingest);
  const [busy, setBusy] = useState(false);
  const [touched, setTouched] = useState(false);
  const choices = kind === 'ingest' ? INGEST_SCOPES : API_SCOPES;
  const { envs, invalid } = parseEnvironments(envText);
  const trimmed = name.trim();
  const problems = [
    scopes.length === 0 ? 'scope를 하나 이상 고르세요.' : null,
    invalid !== null ? `environment "${invalid}" 형식이 맞지 않습니다(영문·숫자·. _ -, 64자까지).` : null,
    kind === 'ingest' && envs.length === 0 ? '수집 key는 environment가 하나 이상 필요합니다.' : null,
    envs.length > 20 ? 'environment는 20개까지입니다.' : null,
    [...trimmed].length > 64 ? '이름은 64자까지입니다.' : null,
  ].filter((p): p is string => p !== null);

  const switchKind = (k: KeyKind) => {
    setKind(k);
    setScopes(k === 'ingest' ? INGEST_SCOPES.map((s) => s.scope) : ['telemetry.read']);
    setDays(DEFAULT_LIFETIME[k]);
  };

  return (
    <section className="mt-card" aria-labelledby="key-form-title">
      <h2 id="key-form-title" className="mt-subtitle">
        key 발급
      </h2>
      {created !== null ? (
        <TokenReveal created={created} onDone={onDone} />
      ) : (
        <form
          className="mt-team__invite"
          noValidate
          onSubmit={(e) => {
            e.preventDefault();
            setTouched(true);
            if (problems.length > 0) return;
            setBusy(true);
            void onCreate({ kind, name: trimmed === '' ? null : trimmed, scopes, environments: envs, expiresInDays: days }).then((ok) => {
              setBusy(false);
              if (ok) {
                setName('');
                setTouched(false);
              }
            });
          }}
        >
          <fieldset className="mt-access__fieldset">
            <legend className="mt-label">종류</legend>
            <label>
              <input type="radio" name="key-kind" checked={kind === 'ingest'} onChange={() => switchKind('ingest')} /> 수집(OTLP) — 수집기·SDK가 데이터를 보낼 때
            </label>
            <label>
              <input type="radio" name="key-kind" checked={kind === 'api'} onChange={() => switchKind('api')} /> API — 조회·자동화(CI 등). 내 현재 권한을 넘지 못합니다
            </label>
          </fieldset>
          <label className="mt-label" htmlFor="key-name">
            이름(선택 — 어디에 쓰는 key인지)
          </label>
          <input id="key-name" className="mt-input" value={name} maxLength={128} onChange={(e) => setName(e.currentTarget.value)} />
          <fieldset className="mt-access__fieldset">
            <legend className="mt-label">권한(scope)</legend>
            {choices.map((c) => (
              <label key={c.scope}>
                <input
                  type="checkbox"
                  checked={scopes.includes(c.scope)}
                  onChange={(e) => {
                    const on = e.currentTarget.checked;
                    setScopes((prev) => (on ? [...prev, c.scope] : prev.filter((s) => s !== c.scope)));
                  }}
                />{' '}
                {c.label} <code className="mt-muted">{c.scope}</code>
              </label>
            ))}
          </fieldset>
          <label className="mt-label" htmlFor="key-envs">
            environment({kind === 'ingest' ? '필수' : '선택 — 비우면 조직 전체'}, 쉼표로 여럿)
          </label>
          <input id="key-envs" className="mt-input mt-mono" value={envText} placeholder="prod, staging" onChange={(e) => setEnvText(e.currentTarget.value)} />
          <label className="mt-label" htmlFor="key-days">
            유효 기간
          </label>
          <select id="key-days" className="mt-input" value={days} onChange={(e) => setDays(Number(e.currentTarget.value))}>
            {LIFETIMES.map((d) => (
              <option key={d} value={d}>
                {d}일
              </option>
            ))}
          </select>
          {touched && problems.length > 0 && (
            <ul className="mt-label" role="alert">
              {problems.map((p) => (
                <li key={p}>{p}</li>
              ))}
            </ul>
          )}
          <div>
            <button type="submit" className="mt-button mt-button--primary" disabled={busy}>
              key 발급
            </button>
          </div>
        </form>
      )}
    </section>
  );
}

/** 원문 token 1회 표시. 값은 이 컴포넌트 상태에만 있고, 닫으면 사라진다. */
function TokenReveal({ created, onDone }: { created: KeyCreated; onDone: () => void }) {
  const [copied, setCopied] = useState<'idle' | 'ok' | 'failed'>('idle');
  // 나타나면 제목으로 focus. role=alert는 쓰지 않는다 — token을 소리 내어 읽게 된다
  const title = useRef<HTMLElement>(null);
  useEffect(() => title.current?.focus(), []);
  const token = created.token;
  return (
    <section className="mt-notice mt-notice--warning mt-policy-secret" aria-labelledby="token-title">
      <strong id="token-title" ref={title} tabIndex={-1}>
        key — 지금만 볼 수 있습니다
      </strong>
      {token === null ? (
        <span>같은 요청을 다시 보내 key를 받지 못했습니다. 이 key를 폐기하고 새로 발급하세요.</span>
      ) : (
        <>
          <span>
            secret manager나 수집기 설정에 저장하세요. 이 화면을 떠나면 다시 볼 수 없습니다. 요청에는{' '}
            <code>Authorization: Bearer &lt;key&gt;</code>로 보냅니다.
          </span>
          <label className="mt-label" htmlFor="key-token">
            key ({created.data.token_hint})
          </label>
          <div className="mt-monitor-actions">
            <input id="key-token" className="mt-input mt-mono mt-policy-secret__value" readOnly value={token} onFocus={(e) => e.currentTarget.select()} autoComplete="off" spellCheck={false} />
            <button
              type="button"
              className="mt-button"
              onClick={() => {
                void navigator.clipboard.writeText(token).then(
                  () => setCopied('ok'),
                  () => setCopied('failed'),
                );
              }}
            >
              복사
            </button>
          </div>
          {copied === 'ok' && <span role="status">복사했습니다. 저장한 뒤 클립보드를 비우세요.</span>}
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

function KeyTable({ keys, timeZone, onRevoke }: { keys: KeyItem[]; timeZone: string; onRevoke: (k: KeyItem, graceSeconds: number) => Promise<boolean> }) {
  const nowMs = Date.now();
  return (
    <section className="mt-card mt-card--flush" aria-labelledby="keys-title">
      <h2 id="keys-title" className="mt-subtitle mt-team__heading">
        key {keys.length}개
      </h2>
      {keys.length === 0 ? (
        <p className="mt-label mt-team__empty">아직 key가 없습니다. 수집기를 연결하려면 수집 key를 발급하세요.</p>
      ) : (
        <div className="mt-scroll-x">
          <table className="mt-table">
            <caption className="mt-visually-hidden">API key(최신순)</caption>
            <thead>
              <tr>
                <th scope="col">key</th>
                <th scope="col">종류·권한</th>
                <th scope="col">environment</th>
                <th scope="col">상태</th>
                <th scope="col">만료</th>
                <th scope="col">동작</th>
              </tr>
            </thead>
            <tbody>
              {keys.map((k) => (
                <KeyRow key={k.key_id} item={k} nowMs={nowMs} timeZone={timeZone} onRevoke={onRevoke} />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

function KeyRow({ item: k, nowMs, timeZone, onRevoke }: { item: KeyItem; nowMs: number; timeZone: string; onRevoke: (k: KeyItem, graceSeconds: number) => Promise<boolean> }) {
  const [confirming, setConfirming] = useState(false);
  const [grace, setGrace] = useState(0);
  const [busy, setBusy] = useState(false);
  const openRef = useRef<HTMLButtonElement>(null);
  const confirmRef = useRef<HTMLSelectElement>(null);
  useEffect(() => {
    if (confirming) confirmRef.current?.focus();
  }, [confirming]);
  const st = keyStatus(k, nowMs);
  const label = k.name ?? k.token_hint;
  const cancel = () => {
    setConfirming(false);
    openRef.current?.focus();
  };
  return (
    <tr>
      <th scope="row">
        <span className="mt-team__name">{k.name ?? <span className="mt-muted">이름 없음</span>}</span>
        <span className="mt-label mt-mono mt-team__id">{k.token_hint}</span>
        <span className="mt-label mt-team__id">발급 {k.issued_by} · {formatDateTime(k.created_at, timeZone)}</span>
      </th>
      <td>
        <span>{k.kind === 'ingest' ? '수집' : 'API'}</span>
        <span className="mt-label mt-mono mt-team__id">{k.scopes.join(', ')}</span>
      </td>
      <td className="mt-mono">{k.environments.length === 0 ? <span className="mt-muted">조직 전체</span> : k.environments.join(', ')}</td>
      <td className="mt-nowrap">
        {st.kind === 'active' && <StatusBadge tone="success">사용 중</StatusBadge>}
        {st.kind === 'revoking' && (
          <StatusBadge tone="warning">{formatDateTime(st.at, timeZone)}에 폐기</StatusBadge>
        )}
        {st.kind === 'revoked' && <StatusBadge tone="neutral">폐기됨</StatusBadge>}
        {st.kind === 'expired' && <StatusBadge tone="neutral">만료됨</StatusBadge>}
      </td>
      <td className="mt-nowrap">{formatDateTime(k.expires_at, timeZone)}</td>
      <td className="mt-nowrap">
        {st.kind === 'revoked' || st.kind === 'expired' ? (
          <span className="mt-muted">—</span>
        ) : confirming ? (
          <span className="mt-monitor-actions" role="group" aria-label={`${label} 폐기 확인`}>
            <label className="mt-visually-hidden" htmlFor={`grace-${k.key_id}`}>
              폐기 시점
            </label>
            <select id={`grace-${k.key_id}`} ref={confirmRef} className="mt-input" value={grace} onChange={(e) => setGrace(Number(e.currentTarget.value))}>
              <option value={0}>지금 폐기</option>
              <option value={3600}>1시간 뒤(교체 중)</option>
              <option value={86400}>24시간 뒤(교체 중)</option>
            </select>
            <button
              type="button"
              className="mt-button mt-button--danger"
              disabled={busy}
              onClick={() => {
                setBusy(true);
                void onRevoke(k, grace).then((ok) => {
                  setBusy(false);
                  if (ok) setConfirming(false);
                  else cancel();
                });
              }}
            >
              폐기
            </button>
            <button type="button" className="mt-button" disabled={busy} onClick={cancel}>
              취소
            </button>
          </span>
        ) : (
          <button ref={openRef} type="button" className="mt-button" onClick={() => setConfirming(true)}>
            폐기…
          </button>
        )}
      </td>
    </tr>
  );
}
