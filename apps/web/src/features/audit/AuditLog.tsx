// 설정 > Audit (D02 §14, D04 §01·§12, ADR 0034·0072). 감사 이벤트를 시간 역순으로 보인다.
// - 범위는 상단 시간 범위다(최대 366일). 범주·action 조건은 URL(`category`, `action`)에 남아 공유된다.
// - 권한: Tenant Admin·Security Auditor는 두 범주, Operator는 운영 범주만. 볼 수 없는 범주를 고르면 403이고,
//   "기록 없음"이 아니라 권한이 없다고 말한다(계약 6).
// - "더 보기"는 서명 cursor로 이어 받는다. cursor는 15분 뒤 만료된다 — 그러면 처음부터 다시 읽으라고 말한다.
// - 시간 창은 첫 page를 부를 때마다(조건·범위·새로고침) 지금 시각으로 다시 고정하고, 이어 받는 동안만 그대로 둔다.
//   화면의 "기준"은 응답 시각이 아니라 그 창의 끝이다(그 뒤 기록은 새로고침해야 보인다, ADR 0034 snapshot).
// - 새로고침 중·실패 시 이전 행을 남기고 "마지막 성공 기준"을 보인다(D05 §03).
// - details는 저장값 그대로를 글자로만 보인다(HTML 해석 없음). 운영자 행의 직원 ID는 서버가 가린다.
// - 결과는 컴포넌트 상태에만 둔다(전역 store에 복사하지 않는다).
import { useEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import { useOutletContext, useParams, useSearchParams } from 'react-router';
import { AUDIT_PAGE_LIMIT, MAX_AUDIT_RANGE_MS, isCursorRejected, listAuditEvents, type AuditEvent, type AuditQuery } from '../../api/audit.ts';
import type { ApiErrorBody } from '../../api/types.ts';
import { formatRange, resolveRange, type InvestigationContext } from '../../app/context.ts';
import { useSession } from '../../app/SessionGate.tsx';
import { formatDateTime } from '../services/format.ts';
import { EmptyState, ErrorNotice, Skeleton } from '../services/states.tsx';
import { ACTION_RULE, CATEGORY_LABEL, SECURITY_RETENTION_MS, actorLabel, categoryLabel, parseCategory, validAction } from './auditView.ts';
import '../services/services.css';
import './audit.css';

type Failure = { status: number; body: ApiErrorBody; retryAfterSeconds: number | null };

interface Pages {
  status: 'loading' | 'success' | 'error';
  rows: AuditEvent[];
  nextCursor: string | null;
  error: Failure | null;
  /** 다음 page가 실패했을 때(앞의 행은 남긴다) */
  moreError: Failure | null;
  cursorExpired: boolean;
  loadingMore: boolean;
  /** 같은 조건 계열을 다시 읽는 중(이전 행을 남긴다) */
  refreshing: boolean;
  lastSuccessMs: number | null;
  /** 보이는 행의 시간 창 끝(첫 page 기준) */
  windowEndMs: number | null;
}

const initial: Pages = { status: 'loading', rows: [], nextCursor: null, error: null, moreError: null, cursorExpired: false, loadingMore: false, refreshing: false, lastSuccessMs: null, windowEndMs: null };

/**
 * 첫 page와 "더 보기". 진행 중 요청은 새 첫 page가 시작되면 취소한다(늦은 응답이 새 조건을 덮지 않게).
 * family(조건 계열: 조직·범주·action·범위 preset)가 같으면(새로고침) 이전 행을 남기고, 다르면 버린다.
 */
function useAuditPages(key: string | null, family: string, query: AuditQuery | null) {
  const [state, setState] = useState<Pages>(initial);
  const queryRef = useRef(query);
  queryRef.current = query;
  const ctrl = useRef<AbortController | null>(null);
  const lastFamily = useRef<string | null>(null);
  useEffect(() => {
    ctrl.current?.abort();
    const q = queryRef.current;
    if (key === null || q === null) return undefined;
    const c = new AbortController();
    ctrl.current = c;
    const same = lastFamily.current === family;
    lastFamily.current = family;
    setState((s) => (same && s.rows.length > 0 ? { ...s, refreshing: true, error: null, moreError: null, cursorExpired: false, loadingMore: false } : initial));
    const windowEndMs = Date.parse(q.toIso);
    listAuditEvents(q, null, c.signal)
      .then((r) => {
        if (c.signal.aborted) return;
        setState((s) =>
          r.ok
            ? { ...initial, status: 'success', rows: r.data, nextCursor: r.nextCursor, lastSuccessMs: Date.now(), windowEndMs }
            : { ...s, status: 'error', refreshing: false, error: { status: r.status, body: r.error, retryAfterSeconds: r.retryAfterSeconds } },
        );
      })
      .catch(() => undefined);
    return () => c.abort();
  }, [key]);

  const more = () => {
    const q = queryRef.current;
    if (q === null || state.nextCursor === null || state.loadingMore) return;
    const c = ctrl.current ?? new AbortController();
    setState((s) => ({ ...s, loadingMore: true, moreError: null }));
    listAuditEvents(q, state.nextCursor, c.signal)
      .then((r) => {
        if (c.signal.aborted) return;
        if (r.ok) {
          setState((s) => ({ ...s, rows: [...s.rows, ...r.data], nextCursor: r.nextCursor, loadingMore: false, lastSuccessMs: Date.now() }));
          return;
        }
        const expired = isCursorRejected(r);
        setState((s) => ({ ...s, loadingMore: false, cursorExpired: expired, moreError: expired ? null : { status: r.status, body: r.error, retryAfterSeconds: r.retryAfterSeconds } }));
      })
      .catch(() => undefined);
  };
  return { state, more };
}

export function AuditLog() {
  const { org = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const session = useSession();
  const [params, setParams] = useSearchParams();
  const [tick, setTick] = useState(0);
  const category = parseCategory(params.get('category'));
  const action = params.get('action');
  const [actionDraft, setActionDraft] = useState(action ?? '');
  const [actionError, setActionError] = useState<string | null>(null);
  // 뒤로·앞으로 이동으로 URL이 바뀌면 입력칸도 따라간다
  useEffect(() => {
    setActionDraft(action ?? '');
    setActionError(null);
  }, [action]);
  // URL로 들어온 action도 검사한다 — 틀리면 서버에 보내지 않고 입력칸 아래에 이유를 보인다
  const urlActionInvalid = action !== null && !validAction(action);
  const rangeKey = JSON.stringify(ctx.range);
  // 첫 page를 부를 때마다(범위·조건·새로고침) 지금 시각으로 창을 다시 고정한다. 이어 받는 동안은 그대로다(cursor의 query hash).
  const nowMs = useMemo(() => Date.now(), [rangeKey, tick, category, action]);
  const { fromMs, toMs } = resolveRange(ctx.range, nowMs);
  const tooLong = toMs - fromMs > MAX_AUDIT_RANGE_MS;
  const query: AuditQuery | null = tooLong || urlActionInvalid ? null : { fromIso: new Date(fromMs).toISOString(), toIso: new Date(toMs).toISOString(), category, action };
  const key = query === null ? null : `${org}|audit|${JSON.stringify(query)}|${String(tick)}`;
  const family = `${org}|${category ?? ''}|${action ?? ''}|${rangeKey}`;
  const { state, more } = useAuditPages(key, family, query);
  const pastRetention = fromMs < nowMs - SECURITY_RETENTION_MS;
  const filtered = category !== null || action !== null;
  const operatorOnly = session?.role === 'operator';

  const setParam = (name: string, value: string | null) => {
    const next = new URLSearchParams(params);
    if (value === null || value === '') next.delete(name);
    else next.set(name, value);
    setParams(next, { replace: true });
  };
  const submitAction = (e: FormEvent) => {
    e.preventDefault();
    const v = actionDraft.trim();
    if (v !== '' && !validAction(v)) {
      setActionError(ACTION_RULE);
      return;
    }
    setActionError(null);
    setParam('action', v);
  };

  return (
    <section className="mt-page" aria-labelledby="audit-title">
      <header className="mt-page__header">
        <h1 id="audit-title" className="mt-title">
          Audit
        </h1>
        <span className="mt-label">
          {formatRange(ctx.range, nowMs, ctx.timeZone)} · 최근 기록부터
        </span>
        <button type="button" className="mt-button mt-page__action" onClick={() => setTick((n) => n + 1)}>
          새로고침
        </button>
      </header>
      <p className="mt-label">
        누가 언제 무엇을 바꾸거나 열람했는지의 기록입니다. 보안 범주는 Tenant Admin·Security Auditor만, 운영 범주는 Operator도 볼 수 있습니다.
        {operatorOnly && ' 지금 권한으로는 운영 범주만 보입니다.'}
      </p>
      <form className="mt-audit-filters" onSubmit={submitAction} role="search" aria-label="감사 조건">
        <label className="mt-audit-filter">
          <span className="mt-label">범주</span>
          <select className="mt-select" value={category ?? ''} onChange={(e) => setParam('category', e.target.value)}>
            <option value="">볼 수 있는 범주 전체</option>
            <option value="operations">{CATEGORY_LABEL.operations}</option>
            <option value="security">{CATEGORY_LABEL.security}</option>
          </select>
        </label>
        <label className="mt-audit-filter">
          <span className="mt-label">action (정확히 일치)</span>
          <input
            className="mt-input mt-mono"
            value={actionDraft}
            placeholder="예: api_key.revoked"
            onChange={(e) => setActionDraft(e.target.value)}
            aria-invalid={actionError !== null || urlActionInvalid}
            aria-describedby={actionError === null && !urlActionInvalid ? undefined : 'audit-action-error'}
            maxLength={128}
          />
        </label>
        <button type="submit" className="mt-button">
          적용
        </button>
        {action !== null && (
          <button
            type="button"
            className="mt-button"
            onClick={() => {
              setActionDraft('');
              setParam('action', null);
            }}
          >
            action 조건 지우기
          </button>
        )}
        {(actionError ?? (urlActionInvalid ? ACTION_RULE : null)) !== null && (
          <span id="audit-action-error" className="mt-monitor-error" role="alert">
            {actionError ?? ACTION_RULE}
          </span>
        )}
      </form>
      {pastRetention && !tooLong && (
        <p className="mt-label" role="note">
          보안 감사 기록의 보존 기간은 365일입니다(D04 §04). 그보다 오래된 구간에는 기록이 없을 수 있습니다.
        </p>
      )}

      {tooLong ? (
        <div className="mt-notice mt-notice--warning" role="alert">
          감사 기록은 한 번에 366일까지 조회할 수 있습니다. 시간 범위를 줄이세요.
        </div>
      ) : urlActionInvalid ? (
        <EmptyState reason="filtered" title="action 조건이 올바르지 않아 조회하지 않았습니다">
          입력칸의 조건을 고치거나 지우세요.
        </EmptyState>
      ) : state.status === 'error' && state.error !== null && state.rows.length === 0 ? (
        state.error.status === 403 ? (
          <EmptyState reason="forbidden" title={category === 'security' ? '보안 범주를 볼 권한이 없습니다' : '감사 기록을 볼 권한이 없습니다'}>
            {category === 'security'
              ? 'Tenant Admin·Security Auditor만 볼 수 있습니다. 기록이 없다는 뜻이 아닙니다. 운영 범주를 골라 보세요.'
              : 'Tenant Admin·Security Auditor·Operator만 볼 수 있습니다. 기록이 없다는 뜻이 아닙니다.'}
          </EmptyState>
        ) : state.error.status === 404 ? (
          <EmptyState reason="unknown" title="감사 조회 API에 닿지 못했습니다" requestId={state.error.body.request_id}>
            이 배포의 경로 설정(control-api)을 확인하세요. 기록이 없다는 뜻이 아닙니다.
          </EmptyState>
        ) : (
          <ErrorNotice error={state.error} lastSuccessMs={null} timeZone={ctx.timeZone} onRetry={() => setTick((n) => n + 1)} />
        )
      ) : state.status === 'loading' ? (
        <Skeleton height={240} label="감사 기록을 불러오는 중" />
      ) : state.rows.length === 0 ? (
        filtered ? (
          <EmptyState reason="filtered" title="이 기간·조건에 맞는 감사 기록이 없습니다">
            시간 범위를 넓히거나 조건을 바꿔 보세요.
          </EmptyState>
        ) : (
          <EmptyState reason="new" title="이 기간에 볼 수 있는 감사 기록이 없습니다">
            시간 범위를 넓혀 보세요.
          </EmptyState>
        )
      ) : (
        <>
          {state.error !== null && (
            <>
              <ErrorNotice error={state.error} lastSuccessMs={state.lastSuccessMs} timeZone={ctx.timeZone} onRetry={() => setTick((n) => n + 1)} />
              <p className="mt-label">아래는 마지막 성공 기준입니다.</p>
            </>
          )}
          <div aria-busy={state.refreshing}>
            <AuditTable rows={state.rows} timeZone={ctx.timeZone} />
          </div>
        </>
      )}

      {state.rows.length > 0 && (
        <div className="mt-audit-more">
          <span className="mt-label" role="status">
            {state.rows.length.toLocaleString('ko-KR')}개
            {state.nextCursor === null ? ' · 끝까지 봤습니다' : ''}
            {state.windowEndMs !== null && ` · ${formatDateTime(new Date(state.windowEndMs).toISOString(), ctx.timeZone)}까지의 기록(그 뒤 기록은 새로고침)`}
            {state.refreshing && ' · 새로 불러오는 중'}
          </span>
          {state.cursorExpired ? (
            <div className="mt-notice mt-notice--warning" role="alert">
              <span>이어 받을 수 있는 시간(15분)이 지났거나 권한이 바뀌었습니다. 처음부터 다시 불러오세요.</span>
              <button type="button" className="mt-button" onClick={() => setTick((n) => n + 1)}>
                처음부터 다시 불러오기
              </button>
            </div>
          ) : (
            state.nextCursor !== null && (
              <button type="button" className="mt-button" onClick={more} disabled={state.loadingMore} aria-busy={state.loadingMore}>
                {state.loadingMore ? '불러오는 중…' : `${String(AUDIT_PAGE_LIMIT)}개 더 보기`}
              </button>
            )
          )}
          {state.moreError !== null &&
            (state.moreError.status === 403 ? (
              // 이어 받는 중 권한이 바뀌면(강등 등) 서버는 cursor 검사 전에 403이다 — 감사 권한 문구로 말한다
              <div className="mt-notice mt-notice--warning" role="alert">
                감사 기록을 더 볼 권한이 없습니다(역할이 바뀌었을 수 있습니다). 위의 기록은 그 전에 받은 것입니다.
              </div>
            ) : (
              <ErrorNotice error={state.moreError} lastSuccessMs={state.lastSuccessMs} timeZone={ctx.timeZone} onRetry={more} />
            ))}
        </div>
      )}
    </section>
  );
}

function AuditTable({ rows, timeZone }: { rows: AuditEvent[]; timeZone: string }) {
  return (
    <div className="mt-card mt-card--flush mt-scroll-x">
      <table className="mt-table mt-audit-table">
        <caption className="mt-visually-hidden">감사 기록</caption>
        <thead>
          <tr>
            <th scope="col">시각</th>
            <th scope="col">범주</th>
            <th scope="col">action</th>
            <th scope="col">주체</th>
            <th scope="col">대상</th>
            <th scope="col">자세히</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((e) => (
            <tr key={e.id}>
              <td>
                <time dateTime={e.occurred_at}>{formatDateTime(e.occurred_at, timeZone)}</time>
              </td>
              <td>
                {/* 범주는 상태가 아니다 — 경보·오류 색을 쓰지 않는다 */}
                <span className="mt-source-badge">{categoryLabel(e.category)}</span>
              </td>
              <td className="mt-mono">{e.action}</td>
              <td>
                <span className="mt-audit-actor">{actorLabel(e.actor_kind)}</span>
                <span className="mt-mono mt-muted mt-audit-id">{e.actor_id}</span>
              </td>
              <td>
                <span>{e.resource_type}</span>
                <span className="mt-mono mt-muted mt-audit-id">{e.resource_id}</span>
              </td>
              <td>
                <details className="mt-audit-details">
                  <summary>details{e.request_id !== null ? ' · request_id' : ''}</summary>
                  {e.request_id !== null && <p className="mt-label mt-mono">request_id {e.request_id}</p>}
                  {/* 저장값 그대로를 글자로만(HTML 해석 없음) */}
                  <pre className="mt-audit-json">{JSON.stringify(e.details, null, 2)}</pre>
                </details>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
