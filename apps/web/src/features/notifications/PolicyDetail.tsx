// 알림 policy 상세 (ADR 0056): 정의 수정(If-Match), 시험 전송(D05 §09 "실제 알림과 분리"), 서명 secret 교체(ADR 0055), 삭제.
// - 시험 결과는 보낸 뒤 상태를 몇 초 간격으로 다시 읽는다. 보내는 중을 성공으로 보이지 않는다.
// - 교체·생성 응답의 secret은 이 화면 상태에만 있고 닫으면 사라진다.
import { useEffect, useRef, useState } from 'react';
import { Link, useNavigate, useOutletContext, useParams, useSearchParams } from 'react-router';
import {
  deletePolicy,
  getPolicy,
  getTest,
  rotateSecret,
  sendTest,
  updatePolicy,
  type PolicyItem,
  type PolicyWithSecret,
  type TestItem,
} from '../../api/notificationPolicies.ts';
import { useRemote } from '../../api/useRemote.ts';
import { contextOnly, type InvestigationContext } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { formatDateTime } from '../services/format.ts';
import { ErrorNotice, Skeleton, StatusBadge } from '../services/states.tsx';
import '../services/services.css';
import '../monitors/monitors.css';
import { PolicyForm, policyFieldErrors, type PolicyFormValue } from './PolicyForm.tsx';
import { SecretReveal } from './SecretReveal.tsx';
import { testResultText } from './policyText.ts';
import './notifications.css';

/** 시험 결과를 다시 읽는 간격과 횟수(요청 10초 + 여유). 그 뒤에도 보내는 중이면 그렇다고 적는다. 시험은 간격을 줄인다. */
export const testPoll = { ms: 1500, max: 20 };

const ADMIN_ONLY = 'Tenant Admin만 할 수 있습니다.';

/** 확인 대화가 열리면 첫 조작으로, 닫히면(렌더 뒤 — 여는 버튼이 다시 켜진 다음) 여는 버튼으로 focus를 옮긴다. */
function useDialogFocus(open: boolean, first: { current: HTMLElement | null }, trigger: { current: HTMLElement | null }) {
  const wasOpen = useRef(false);
  useEffect(() => {
    if (open) first.current?.focus();
    else if (wasOpen.current) trigger.current?.focus();
    wasOpen.current = open;
  }, [open, first, trigger]);
}

export function PolicyDetail() {
  const { org = '', policyId = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const [params] = useSearchParams();
  const search = contextOnly(params);
  const [refreshTick, setRefreshTick] = useState(0);
  const remote = useRemote(`${org}|policy|${policyId}`, refreshTick, (signal) => getPolicy(policyId, signal));
  // 저장·교체 응답으로 받은 최신 정의(다시 불러오기 전까지 화면에 쓴다)
  const [local, setLocal] = useState<PolicyItem | null>(null);
  const p = local !== null && remote.data !== null && local.revision >= remote.data.revision ? local : remote.data;
  // 412 뒤 "최신 내용 불러오기": 다시 읽은 서버 값으로 form을 새로 시작한다(예전 입력을 새 revision으로 보내 남의 변경을 덮지 않게)
  const [formEpoch, setFormEpoch] = useState(0);
  const resync = useRef(false);
  useEffect(() => {
    if (resync.current && remote.data !== null && !remote.refreshing) {
      resync.current = false;
      setFormEpoch((e) => e + 1);
    }
  }, [remote.data, remote.refreshing]);
  const reloadLatest = () => {
    resync.current = true;
    setLocal(null);
    setRefreshTick((n) => n + 1);
  };

  if (remote.error?.status === 404) {
    return (
      <section className="mt-placeholder">
        <h1 className="mt-title">알림 policy를 찾을 수 없습니다</h1>
        <p>지워졌거나, 볼 수 있는 권한이 없거나, 이 배포에서 알림이 꺼져 있습니다.</p>
        <Link to={{ pathname: orgPath(org, 'notifications'), search }}>policy 목록</Link>
      </section>
    );
  }
  return (
    <section className="mt-page" aria-labelledby="policy-title">
      <nav className="mt-breadcrumb" aria-label="위치">
        <Link to={{ pathname: orgPath(org, 'notifications'), search }}>Notifications</Link>
      </nav>
      <header className="mt-page__header">
        <h1 id="policy-title" className="mt-title">
          {p?.name ?? '…'}
        </h1>
        {p != null && <StatusBadge tone={p.enabled ? 'success' : 'neutral'}>{p.enabled ? '켜짐' : '꺼짐'}</StatusBadge>}
        <div className="mt-page__action mt-monitor-actions">
          <button type="button" className="mt-button" onClick={() => setRefreshTick((n) => n + 1)}>
            새로고침
          </button>
        </div>
      </header>
      {remote.error !== null && <ErrorNotice error={remote.error} lastSuccessMs={remote.lastSuccessMs} timeZone={ctx.timeZone} />}
      {p == null ? (
        remote.error === null && <Skeleton height={200} label="policy를 불러오는 중" />
      ) : (
        <>
          <Definition key={`${p.id}:${String(formEpoch)}`} p={p} onSaved={setLocal} reload={reloadLatest} />
          <TestSend p={p} timeZone={ctx.timeZone} />
          <Secret p={p} timeZone={ctx.timeZone} onRotated={setLocal} reload={reloadLatest} />
          <Delete p={p} org={org} search={search} />
        </>
      )}
    </section>
  );
}

function Definition({ p, onSaved, reload }: { p: PolicyItem; onSaved: (p: PolicyItem) => void; reload: () => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ text: string; conflict: boolean } | null>(null);
  const [fieldErrors, setFieldErrors] = useState<{ name?: string; url?: string }>({});
  const [saved, setSaved] = useState(false);
  const save = async (v: PolicyFormValue) => {
    setBusy(true);
    setError(null);
    setSaved(false);
    setFieldErrors({});
    const r = await updatePolicy(p.id, { name: v.name, enabled: v.enabled, url: v.url === '' ? null : v.url }, p.revision, new AbortController().signal);
    setBusy(false);
    if (r.ok) {
      setSaved(true);
      onSaved(r.data);
      return;
    }
    setFieldErrors(policyFieldErrors(r.error.details));
    setError(
      r.status === 412
        ? { text: '다른 변경이 먼저 저장되었습니다. 덮어쓰지 않았습니다.', conflict: true }
        : { text: r.status === 403 ? ADMIN_ONLY : r.error.message, conflict: false },
    );
  };
  return (
    <section className="mt-card" aria-labelledby="def-title">
      <h2 id="def-title" className="mt-card__title">
        정의
      </h2>
      <PolicyForm
        initial={{ name: p.name, url: '', enabled: p.enabled }}
        currentHost={p.webhook.url}
        busy={busy}
        serverErrors={fieldErrors}
        submitLabel="변경 저장"
        onSubmit={(v) => void save(v)}
      />
      {saved && <p role="status">저장했습니다(revision {p.revision}).</p>}
      {error !== null && (
        <div className="mt-notice mt-notice--critical" role="alert">
          <strong>저장하지 못했습니다</strong>
          <span>{error.text}</span>
          {error.conflict && (
            <button type="button" className="mt-button" onClick={reload}>
              최신 내용 불러오기
            </button>
          )}
        </div>
      )}
    </section>
  );
}

function TestSend({ p, timeZone }: { p: PolicyItem; timeZone: string }) {
  const [test, setTest] = useState<TestItem | null>(null);
  const [state, setState] = useState<'idle' | 'sending' | 'polling' | 'done' | 'slow' | 'unknown'>('idle');
  const [error, setError] = useState<string | null>(null);
  const [reused, setReused] = useState(false);
  // 화면을 떠나면 보내기·다시 읽기를 멈춘다(요청 취소, 결과를 쓰지 않음)
  const ctrl = useRef<AbortController | null>(null);
  useEffect(() => () => ctrl.current?.abort(), []);
  const run = async () => {
    ctrl.current?.abort();
    const c = new AbortController();
    ctrl.current = c;
    try {
      await poll(c.signal);
    } catch (e) {
      if (!(e instanceof DOMException && e.name === 'AbortError')) throw e;
    }
  };
  const poll = async (signal: AbortSignal) => {
    setState('sending');
    setError(null);
    setTest(null);
    const r = await sendTest(p.id, signal);
    if (!r.ok) {
      setState('idle');
      setError(
        r.status === 403
          ? ADMIN_ONLY
          : r.status === 429
            ? `시험 전송은 policy마다 1시간에 20번까지입니다${r.retryAfterSeconds === null ? '' : ` (${String(Math.ceil(r.retryAfterSeconds / 60))}분 뒤 다시)`}.`
            : r.error.message,
      );
      return;
    }
    setReused(r.meta.warnings.includes('test_in_progress'));
    let t = r.data;
    setTest(t);
    setState('polling');
    // 결과를 못 읽은 것을 "보내는 중"으로 보이지 않는다: 4xx이거나 3번 연속 실패면 확인하지 못했다고 적는다
    let failures = 0;
    for (let i = 0; i < testPoll.max && t.status === 'pending'; i++) {
      await new Promise<void>((resolve, reject) => {
        const id = setTimeout(resolve, testPoll.ms);
        signal.addEventListener('abort', () => {
          clearTimeout(id);
          reject(new DOMException('aborted', 'AbortError'));
        });
      });
      const g = await getTest(p.id, t.id, signal);
      if (g.ok) {
        failures = 0;
        t = g.data;
        setTest(t);
        continue;
      }
      failures++;
      if ((g.status >= 400 && g.status < 500) || failures >= 3) {
        setState('unknown');
        setError(`시험 결과를 확인하지 못했습니다: ${g.error.message}`);
        return;
      }
    }
    setState(t.status === 'pending' ? 'slow' : 'done');
  };
  const result = test === null ? null : testResultText(test);
  return (
    <section className="mt-card" aria-labelledby="test-title">
      <h2 id="test-title" className="mt-card__title">
        시험 전송
      </h2>
      <p className="mt-label">
        이 주소로 시험 요청(<code>notification.test</code>, <code>payload.test: true</code>)을 한 번 보냅니다. 실제 경보가 아니고, 실패해도 다시 보내지 않습니다.
        {!p.enabled && ' policy가 꺼져 있어도 보냅니다.'}
      </p>
      <div className="mt-monitor-actions">
        <button type="button" className="mt-button" onClick={() => void run()} disabled={state === 'sending' || state === 'polling'}>
          {state === 'sending' || state === 'polling' ? '보내는 중…' : '시험 보내기'}
        </button>
        {result !== null && state !== 'unknown' && (
          <span role="status" className="mt-policy-test">
            <StatusBadge tone={result.tone}>{result.text}</StatusBadge>
            {test?.finished_at != null && <span className="mt-muted"> · {formatDateTime(test.finished_at, timeZone)}</span>}
          </span>
        )}
      </div>
      {reused && <p className="mt-label">진행 중이던 시험의 결과를 보입니다(새로 보내지 않았습니다).</p>}
      {state === 'slow' && <p className="mt-label">아직 보내는 중입니다. 알림 전송이 밀려 있을 수 있습니다. 잠시 뒤 다시 확인하세요.</p>}
      {error !== null && (
        <p className="mt-monitor-error" role="alert">
          {error}
        </p>
      )}
    </section>
  );
}

function Secret({ p, timeZone, onRotated, reload }: { p: PolicyItem; timeZone: string; onRotated: (p: PolicyItem) => void; reload: () => void }) {
  const [dialog, setDialog] = useState(false);
  const [overlap, setOverlap] = useState<'overlap' | 'now'>('overlap');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [revealed, setRevealed] = useState<PolicyWithSecret | null>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const first = useRef<HTMLInputElement>(null);
  useDialogFocus(dialog, first, trigger);
  const close = () => setDialog(false);
  // secret 표시를 닫으면 교체 버튼으로 focus를 돌려준다(표시가 열리면 SecretReveal이 제목으로 focus를 옮긴다)
  const shown = revealed?.signingSecret != null;
  const wasShown = useRef(false);
  useEffect(() => {
    if (!shown && wasShown.current) trigger.current?.focus();
    wasShown.current = shown;
  }, [shown]);
  const rotate = async () => {
    setBusy(true);
    setError(null);
    const r = await rotateSecret(p.id, p.revision, overlap === 'now' ? 0 : 86400, new AbortController().signal);
    setBusy(false);
    if (r.ok) {
      setDialog(false);
      setRevealed(r.data);
      onRotated(r.data.data);
      return;
    }
    setError(r.status === 412 ? '다른 변경이 먼저 저장되었습니다. 최신 내용을 불러온 뒤 다시 교체하세요.' : r.status === 403 ? ADMIN_ONLY : r.error.message);
    if (r.status === 412) reload();
  };
  const prev = p.webhook.previous_signing_key_id ?? null;
  return (
    <section className="mt-card" aria-labelledby="secret-card-title">
      <h2 id="secret-card-title" className="mt-card__title">
        서명 secret
      </h2>
      <dl className="mt-monitor-def">
        <dt>지금 key</dt>
        <dd className="mt-mono">{p.webhook.signing_key_id}</dd>
        <dt>이전 key</dt>
        <dd>
          {prev === null ? (
            <span className="mt-muted">없음</span>
          ) : (
            <>
              <span className="mt-mono">{prev}</span> —{' '}
              {p.webhook.previous_signing_key_expires_at == null
                ? '함께 서명합니다(만료 시각 알 수 없음)'
                : `${formatDateTime(p.webhook.previous_signing_key_expires_at, timeZone)}까지 함께 서명합니다`}
            </>
          )}
        </dd>
      </dl>
      {revealed?.signingSecret != null ? (
        <SecretReveal secret={revealed.signingSecret} keyId={revealed.data.webhook.signing_key_id} doneLabel="저장했습니다 — 닫기" onDone={() => setRevealed(null)} />
      ) : (
        <div className="mt-monitor-actions">
          <button ref={trigger} type="button" className="mt-button" onClick={() => setDialog(true)} disabled={dialog}>
            secret 교체
          </button>
        </div>
      )}
      {dialog && (
        <div
          className="mt-notice mt-notice--warning"
          role="alertdialog"
          aria-labelledby="rotate-title"
          onKeyDown={(e) => {
            if (e.key === 'Escape' && !busy) close();
          }}
        >
          <strong id="rotate-title">서명 secret을 교체할까요?</strong>
          <span>새 secret은 교체 직후 한 번만 보입니다. receiver가 쉼표로 이어진 서명 여러 개를 받을 수 있어야 합니다(겹침 동안 두 서명).</span>
          <div className="mt-monitor-field" role="radiogroup" aria-label="이전 secret">
            <label>
              <input ref={first} type="radio" name="overlap" checked={overlap === 'overlap'} onChange={() => setOverlap('overlap')} />
              24시간 동안 이전 secret으로도 함께 서명 (receiver를 바꿀 시간, 권장)
            </label>
            <label>
              <input type="radio" name="overlap" checked={overlap === 'now'} onChange={() => setOverlap('now')} />
              이전 secret을 바로 버리기 (secret이 유출됐을 때 — 이전 secret으로 검증하는 receiver는 바로 거절합니다)
            </label>
          </div>
          {error !== null && <span role="alert">{error}</span>}
          <div className="mt-monitor-actions">
            <button type="button" className="mt-button mt-button--primary" onClick={() => void rotate()} disabled={busy}>
              {busy ? '교체 중…' : '교체'}
            </button>
            <button type="button" className="mt-button" onClick={close} disabled={busy}>
              취소
            </button>
          </div>
        </div>
      )}
    </section>
  );
}

function Delete({ p, org, search }: { p: PolicyItem; org: string; search: string }) {
  const navigate = useNavigate();
  const [dialog, setDialog] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const confirm = useRef<HTMLButtonElement>(null);
  useDialogFocus(dialog, confirm, trigger);
  const close = () => {
    setDialog(false);
    setError(null);
  };
  const remove = async () => {
    setBusy(true);
    setError(null);
    const r = await deletePolicy(p.id, p.revision, new AbortController().signal);
    setBusy(false);
    if (r.ok) {
      void navigate({ pathname: orgPath(org, 'notifications'), search });
      return;
    }
    setError(
      r.status === 409
        ? '이 policy를 쓰는 monitor가 있습니다. monitor 편집의 알림 단계에서 먼저 떼어 내세요.'
        : r.status === 412
          ? '다른 변경이 먼저 저장되었습니다. 새로고침한 뒤 다시 시도하세요.'
          : r.status === 403
            ? ADMIN_ONLY
            : r.error.message,
    );
  };
  return (
    <section className="mt-card" aria-labelledby="delete-card-title">
      <h2 id="delete-card-title" className="mt-card__title">
        삭제
      </h2>
      <p className="mt-label">지우면 아직 보내지 않은 알림은 보내지 않습니다. monitor가 쓰고 있으면 지울 수 없습니다.</p>
      <button ref={trigger} type="button" className="mt-button mt-button--danger" onClick={() => setDialog(true)} disabled={dialog}>
        policy 삭제
      </button>
      {dialog && (
        <div
          className="mt-notice mt-notice--warning"
          role="alertdialog"
          aria-labelledby="delete-policy-title"
          onKeyDown={(e) => {
            if (e.key === 'Escape' && !busy) close();
          }}
        >
          <strong id="delete-policy-title">“{p.name}”를 삭제할까요?</strong>
          {error !== null && <span role="alert">{error}</span>}
          <div className="mt-monitor-actions">
            <button ref={confirm} type="button" className="mt-button mt-button--danger" onClick={() => void remove()} disabled={busy}>
              삭제
            </button>
            <button type="button" className="mt-button" onClick={close} disabled={busy}>
              취소
            </button>
          </div>
        </div>
      )}
    </section>
  );
}
