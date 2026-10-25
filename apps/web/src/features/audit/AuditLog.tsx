// 설정 > Audit (D02 §14, D04 §01·§12, ADR 0034·0072). 감사 이벤트를 시간 역순으로 보인다.
// - 범위는 상단 시간 범위다(최대 366일). 범주·action 조건은 URL(`category`, `action`)에 남아 공유된다.
// - 권한: Tenant Admin·Security Auditor는 두 범주, Operator는 운영 범주만. 볼 수 없는 범주를 고르면 403이고,
//   "기록 없음"이 아니라 권한이 없다고 말한다(계약 6).
// - "더 보기"는 서명 cursor로 이어 받는다. cursor는 15분 뒤 만료된다 — 그러면 처음부터 다시 읽으라고 말한다.
//   첫 page 시각에 고정되므로 그 뒤에 생긴 기록은 새로 불러와야 보인다.
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
import { ACTOR_LABEL, CATEGORY_LABEL, parseCategory, validAction } from './auditView.ts';
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
  lastSuccessMs: number | null;
}

const initial: Pages = { status: 'loading', rows: [], nextCursor: null, error: null, moreError: null, cursorExpired: false, loadingMore: false, lastSuccessMs: null };

/** 첫 page와 "더 보기". 조건(key)이 바뀌면 이전 행을 버리고 진행 중 요청을 취소한다(늦은 응답이 새 조건을 덮지 않게). */
function useAuditPages(key: string | null, query: AuditQuery | null, refresh: number) {
  const [state, setState] = useState<Pages>(initial);
  const queryRef = useRef(query);
  queryRef.current = query;
  const ctrl = useRef<AbortController | null>(null);
  useEffect(() => {
    ctrl.current?.abort();
    const q = queryRef.current;
    if (key === null || q === null) return undefined;
    const c = new AbortController();
    ctrl.current = c;
    setState(initial);
    listAuditEvents(q, null, c.signal)
      .then((r) => {
        if (c.signal.aborted) return;
        setState(
          r.ok
            ? { ...initial, status: 'success', rows: r.data, nextCursor: r.nextCursor, lastSuccessMs: Date.now() }
            : { ...initial, status: 'error', error: { status: r.status, body: r.error, retryAfterSeconds: r.retryAfterSeconds } },
        );
      })
      .catch(() => undefined);
    return () => c.abort();
  }, [key, refresh]);

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
  const rangeKey = JSON.stringify(ctx.range);
  // 범위는 화면에 들어온 시각(또는 새로고침)에 고정한다 — 이어 받는 page가 같은 조건이어야 cursor가 맞는다
  const nowMs = useMemo(() => Date.now(), [rangeKey, tick]);
  const { fromMs, toMs } = resolveRange(ctx.range, nowMs);
  const tooLong = toMs - fromMs > MAX_AUDIT_RANGE_MS;
  const query: AuditQuery | null = tooLong ? null : { fromIso: new Date(fromMs).toISOString(), toIso: new Date(toMs).toISOString(), category, action };
  const key = query === null ? null : `${org}|audit|${JSON.stringify(query)}`;
  const { state, more } = useAuditPages(key, query, tick);
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
      setActionError('공백·제어 문자 없이 128 byte까지 입력하세요(정확히 일치하는 action).');
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
            aria-invalid={actionError !== null}
            aria-describedby={actionError === null ? undefined : 'audit-action-error'}
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
        {actionError !== null && (
          <span id="audit-action-error" className="mt-monitor-error" role="alert">
            {actionError}
          </span>
        )}
      </form>

      {tooLong ? (
        <div className="mt-notice mt-notice--warning" role="alert">
          감사 기록은 한 번에 366일까지 조회할 수 있습니다. 시간 범위를 줄이세요.
        </div>
      ) : state.status === 'error' && state.error !== null ? (
        state.error.status === 403 ? (
          <EmptyState reason="forbidden" title={category === 'security' ? '보안 범주를 볼 권한이 없습니다' : '감사 기록을 볼 권한이 없습니다'}>
            {category === 'security'
              ? 'Tenant Admin·Security Auditor만 볼 수 있습니다. 기록이 없다는 뜻이 아닙니다. 운영 범주를 골라 보세요.'
              : 'Tenant Admin·Security Auditor·Operator만 볼 수 있습니다. 기록이 없다는 뜻이 아닙니다.'}
          </EmptyState>
        ) : state.error.status === 404 ? (
          <EmptyState reason="disabled" title="이 배포에서는 감사 조회를 쓸 수 없습니다">
            감사 조회 API가 꺼져 있습니다. 기록이 없다는 뜻이 아닙니다.
          </EmptyState>
        ) : (
          <ErrorNotice error={state.error} lastSuccessMs={null} timeZone={ctx.timeZone} onRetry={() => setTick((n) => n + 1)} />
        )
      ) : state.status === 'loading' ? (
        <Skeleton height={240} label="감사 기록을 불러오는 중" />
      ) : state.rows.length === 0 ? (
        <EmptyState reason="filtered" title="이 기간·조건에 감사 기록이 없습니다">
          시간 범위를 넓히거나 조건을 바꿔 보세요.
        </EmptyState>
      ) : (
        <AuditTable rows={state.rows} timeZone={ctx.timeZone} />
      )}

      {state.status === 'success' && state.rows.length > 0 && (
        <div className="mt-audit-more">
          <span className="mt-label" role="status">
            {state.rows.length.toLocaleString('ko-KR')}개
            {state.nextCursor === null ? ' · 끝까지 봤습니다' : ''}
            {state.lastSuccessMs !== null && ` · ${formatDateTime(new Date(state.lastSuccessMs).toISOString(), ctx.timeZone)} 기준(그 뒤 기록은 새로고침)`}
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
          {state.moreError !== null && <ErrorNotice error={state.moreError} lastSuccessMs={state.lastSuccessMs} timeZone={ctx.timeZone} onRetry={more} />}
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
                <span className="mt-source-badge">{CATEGORY_LABEL[e.category]}</span>
              </td>
              <td className="mt-mono">{e.action}</td>
              <td>
                <span className="mt-audit-actor">{ACTOR_LABEL[e.actor_kind]}</span>
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
