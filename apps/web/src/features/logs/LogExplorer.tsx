// S08 Logs (D05 §08, F04, ADR 0045).
// 좌측 조건, 상단 범위(조사 context), 결과 표(시간·심각도·서비스·본문)와 선택 log의 속성 패널.
// - 값은 모두 텍스트로만 그린다(본문·속성에 든 HTML은 실행하지 않는다, D05 §04).
// - 표는 고정 높이 행 가상화(누적 최대 10,000행, D05 §12). 선택·focus 행은 화면 밖이어도 그린다.
// - 잘렸거나 일부가 빠진 결과를 "없음"이라 하지 않는다(계약 6).
import { useEffect, useMemo, useRef, useState } from 'react';
import { Link, useOutletContext, useParams, useSearchParams } from 'react-router';
import { listServices } from '../../api/catalog.ts';
import { searchLogs } from '../../api/traces.ts';
import type { LogItem } from '../../api/types.ts';
import { useSearchPages, type SearchPagesState } from '../../api/useSearchPages.ts';
import { useRemote } from '../../api/useRemote.ts';
import { contextOnly, formatRange, resolveRange, type InvestigationContext } from '../../app/context.ts';
import { useDraft } from '../../app/useDraft.ts';
import { orgPath } from '../../app/nav.ts';
import { formatDateTime } from '../services/format.ts';
import { ErrorNotice, Skeleton } from '../services/states.tsx';
import '../services/services.css';
import '../traces/traces.css';
import { LogDrawer } from './LogDrawer.tsx';
import {
  LOG_PAGE,
  MAX_LOG_RANGE_MS,
  MAX_QUERY_LENGTH,
  SEVERITY_LEVELS,
  isErrorSeverity,
  isTraceId,
  parseLogFilter,
  severityLabel,
  serviceLabel,
  toLogFilterNode,
  traceLinkSearch,
  writeLogFilter,
  type LogFilterState,
  type ServiceOption,
} from './logFilters.ts';
import './logs.css';

export const LOG_ROW_H = 32;
const VIEW_ROWS = 20;
const OVERSCAN = 8;

export function LogExplorer() {
  const { org = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const [params, setParams] = useSearchParams();
  const filter = parseLogFilter(params);
  const [refreshTick, setRefreshTick] = useState(0);
  const rangeKey = JSON.stringify(ctx.range);
  const nowMs = useMemo(() => Date.now(), [rangeKey, refreshTick]);
  const { fromMs, toMs } = resolveRange(ctx.range, nowMs);
  const tooLong = toMs - fromMs > MAX_LOG_RANGE_MS;
  const filterNode = toLogFilterNode(filter);
  const request = tooLong
    ? null
    : { range: { from: new Date(fromMs).toISOString(), to: new Date(toMs).toISOString() }, limit: LOG_PAGE, ...(filterNode === undefined ? {} : { filter: filterNode }) };
  const queryKey = request === null ? null : `${org}|logs|${JSON.stringify(request)}`;
  const { state, loadMore } = useSearchPages(queryKey, request, refreshTick, searchLogs);

  const services = useRemote(`${org}|services|${ctx.environment ?? ''}`, 0, (signal) =>
    listServices({ environment: ctx.environment, cursor: null, limit: 1000 }, signal),
  );
  const serviceById = useMemo(() => new Map((services.data ?? []).map((s) => [s.service_id, s])), [services.data]);

  const selectedId = params.get('entity');
  const selected = selectedId === null ? null : (state.rows.find((r) => r.event_id === selectedId) ?? null);
  const setFilter = (next: LogFilterState) => setParams(writeLogFilter(params, next));
  const select = (eventId: string | null) => {
    const next = new URLSearchParams(params);
    if (eventId === null) next.delete('entity');
    else next.set('entity', eventId);
    setParams(next, { replace: true });
  };
  const search = contextOnly(params);

  return (
    <section className="mt-page" aria-labelledby="logs-title">
      <header className="mt-page__header">
        <h1 id="logs-title" className="mt-title">
          Logs
        </h1>
        <span className="mt-label">{formatRange(ctx.range, nowMs, ctx.timeZone)} · 최신순</span>
        <button type="button" className="mt-button mt-page__action" onClick={() => setRefreshTick((n) => n + 1)}>
          새로고침
        </button>
      </header>
      {ctx.environment !== null && (
        <div className="mt-notice mt-notice--info" role="note">
          환경 선택은 log 검색에 아직 적용되지 않습니다(log 행에 environment가 없음). 이 환경의 서비스를 고르면 그 서비스의 log만 봅니다.
        </div>
      )}

      <div className={selectedId === null ? 'mt-explorer' : 'mt-explorer mt-explorer--with-drawer'}>
        <LogFilterPanel filter={filter} onChange={setFilter} services={services.data ?? []} servicesLoading={services.status === 'loading'} />
        <div className="mt-explorer__main">
          {tooLong && (
            <div className="mt-notice mt-notice--warning" role="alert">
              log 검색은 24시간까지 조회할 수 있습니다. 시간 범위를 줄이세요.
            </div>
          )}
          {state.error !== null && (
            <ErrorNotice
              error={state.error}
              lastSuccessMs={state.rows.length > 0 ? state.lastSuccessMs : null}
              timeZone={ctx.timeZone}
              onRetry={state.rows.length > 0 && state.nextCursor !== null ? loadMore : () => setRefreshTick((n) => n + 1)}
            />
          )}
          {state.partial && (
            <div className="mt-notice mt-notice--warning" role="note">
              일부 저장소가 응답하지 않아 빠진 log가 있을 수 있습니다.
            </div>
          )}
          {state.warnings.includes('interactive_row_limit_reached') && (
            <div className="mt-notice mt-notice--warning" role="note">
              화면에서 이어 볼 수 있는 10,000개에 닿았습니다. 조건이나 범위를 좁히세요.
            </div>
          )}
          {!tooLong && (
            <LogTable
              org={org}
              search={search}
              state={state}
              serviceById={serviceById}
              timeZone={ctx.timeZone}
              selectedId={selectedId}
              onSelect={select}
              onMore={loadMore}
              filterActive={filterNode !== undefined}
            />
          )}
        </div>
        {selectedId !== null && (
          <LogDrawer
            org={org}
            search={search}
            log={selected}
            service={selected === null ? null : (serviceById.get(selected.service_id) ?? null)}
            timeZone={ctx.timeZone}
            loading={state.status === 'loading'}
            onClose={() => {
              select(null);
              document.getElementById(`log-row-${selectedId}`)?.focus();
            }}
          />
        )}
      </div>
    </section>
  );
}

function LogFilterPanel({
  filter,
  onChange,
  services,
  servicesLoading,
}: {
  filter: LogFilterState;
  onChange: (f: LogFilterState) => void;
  services: ServiceOption[];
  servicesLoading: boolean;
}) {
  // 입력 중에는 URL을 바로 바꾸지 않는다: 300ms 뒤 또는 Enter에 반영(D05 §03).
  // 우리가 반영한 값으로 URL이 바뀌어도 입력을 되돌리지 않는다(입력 중 글자가 지워지던 문제, PS-0008).
  const [query, setQuery, queryCommitted] = useDraft(filter.query, (v) => v);
  const [trace, setTrace, traceCommitted] = useDraft(filter.traceId ?? '', (v) => v);
  const traceValid = trace === '' || isTraceId(trace.toLowerCase());
  const commit = (q: string, t: string) => {
    const tl = t.toLowerCase();
    if (tl !== '' && !isTraceId(tl)) return;
    const nq = q.slice(0, MAX_QUERY_LENGTH);
    queryCommitted(nq.trim());
    traceCommitted(tl);
    onChange({ ...filter, query: nq, traceId: tl === '' ? null : tl });
  };
  useEffect(() => {
    if (query === filter.query && trace === (filter.traceId ?? '')) return;
    const t = window.setTimeout(() => commit(query, trace), 300);
    return () => window.clearTimeout(t);
  }, [query, trace]);

  return (
    <aside className="mt-card mt-explorer__filters" aria-label="검색 조건">
      <form
        onSubmit={(e) => {
          e.preventDefault();
          commit(query, trace);
        }}
      >
        <div className="mt-field">
          <label htmlFor="lf-service">서비스</label>
          <select
            id="lf-service"
            className="mt-select"
            value={filter.serviceId ?? ''}
            disabled={servicesLoading}
            onChange={(e) => onChange({ ...filter, serviceId: e.target.value === '' ? null : e.target.value })}
          >
            <option value="">전체</option>
            {/* URL의 서비스가 선택지에 없어도(다른 환경·catalog 미등록·로딩 중) 실제 조건을 숨기지 않는다 */}
            {filter.serviceId !== null && !services.some((s) => s.service_id === filter.serviceId) && (
              <option value={filter.serviceId}>목록에 없는 서비스 {filter.serviceId.slice(0, 8)}…</option>
            )}
            {services.map((s) => (
              <option key={s.service_id} value={s.service_id}>
                {serviceLabel(s)} · {s.environment}
              </option>
            ))}
          </select>
        </div>
        <div className="mt-field">
          <label htmlFor="lf-sev">최소 심각도</label>
          <select
            id="lf-sev"
            className="mt-select"
            value={filter.minSeverity ?? ''}
            aria-describedby="lf-sev-hint"
            onChange={(e) => onChange({ ...filter, minSeverity: SEVERITY_LEVELS.find((l) => l.key === e.target.value)?.key ?? null })}
          >
            <option value="">전체</option>
            {SEVERITY_LEVELS.map((l) => (
              <option key={l.key} value={l.key}>
                {l.label} 이상
              </option>
            ))}
          </select>
          {filter.minSeverity !== null && (
            <span id="lf-sev-hint" className="mt-label">
              심각도가 미지정인 log는 빠집니다.
            </span>
          )}
        </div>
        <div className="mt-field">
          <label htmlFor="lf-trace">Trace ID</label>
          <input
            id="lf-trace"
            className="mt-input mt-mono"
            value={trace}
            spellCheck={false}
            aria-invalid={!traceValid}
            aria-describedby={traceValid ? undefined : 'lf-trace-err'}
            onChange={(e) => setTrace(e.target.value.trim())}
          />
          {!traceValid && (
            <span id="lf-trace-err" className="mt-field__error">
              16진수 32자리를 입력하세요.
            </span>
          )}
        </div>
        <div className="mt-field">
          <label htmlFor="lf-q">본문 포함 (대소문자 무시)</label>
          <input id="lf-q" className="mt-input" type="search" maxLength={MAX_QUERY_LENGTH} value={query} onChange={(e) => setQuery(e.target.value)} />
          <span className="mt-label">공유 링크에는 들어가지 않습니다.</span>
        </div>
        <button type="submit" className="mt-button">
          조회
        </button>
      </form>
    </aside>
  );
}

function LogTable({
  org,
  search,
  state,
  serviceById,
  timeZone,
  selectedId,
  onSelect,
  onMore,
  filterActive,
}: {
  org: string;
  search: string;
  state: SearchPagesState<LogItem>;
  serviceById: Map<string, ServiceOption>;
  timeZone: string;
  selectedId: string | null;
  onSelect: (eventId: string) => void;
  onMore: () => void;
  filterActive: boolean;
}) {
  const rows = state.rows;
  const [scrollTop, setScrollTop] = useState(0);
  const [focusIdx, setFocusIdx] = useState<number | null>(null);
  const scroller = useRef<HTMLDivElement>(null);
  const truncated = state.nextCursor !== null;

  // 공유 링크·뒤로 가기로 연 선택 log로 처음 한 번 스크롤한다
  const scrolledTo = useRef(false);
  useEffect(() => {
    if (scrolledTo.current || selectedId === null || scroller.current === null) return;
    const i = rows.findIndex((r) => r.event_id === selectedId);
    if (i < 0) return;
    scrolledTo.current = true;
    scroller.current.scrollTop = Math.max(0, (i - 3) * LOG_ROW_H);
    setScrollTop(scroller.current.scrollTop);
  }, [rows, selectedId]);

  const first = Math.max(0, Math.floor(scrollTop / LOG_ROW_H) - OVERSCAN);
  const last = Math.min(rows.length, Math.ceil((scrollTop + VIEW_ROWS * LOG_ROW_H) / LOG_ROW_H) + OVERSCAN);
  const indices = new Set<number>();
  for (let i = first; i < last; i++) indices.add(i);
  // 선택·focus 행은 화면 밖이어도 그린다(focus를 잃지 않게, aria-rowindex로 위치를 알린다)
  const selectedIdx = selectedId === null ? -1 : rows.findIndex((r) => r.event_id === selectedId);
  if (selectedIdx >= 0) indices.add(selectedIdx);
  if (focusIdx !== null && focusIdx < rows.length) indices.add(focusIdx);

  return (
    <section className="mt-card mt-card--flush" aria-labelledby="log-results-title">
      <div className="mt-card__head">
        <h2 id="log-results-title" className="mt-card__title">
          결과
        </h2>
        <span className="mt-label">
          {rows.length.toLocaleString('ko-KR')}개{truncated ? ' 이상' : ''} · {LOG_PAGE}개씩 · 시각 최신순(동률은 event ID)
        </span>
      </div>
      {state.status === 'loading' ? (
        <Skeleton height={200} label="log를 불러오는 중" />
      ) : state.status === 'success' && rows.length === 0 && !truncated ? (
        state.partial ? null : (
          <div className="mt-empty mt-empty--inline">
            <strong>{filterActive ? '조건에 맞는 log가 없습니다' : '이 범위에 log가 없습니다'}</strong>
            <span>
              {filterActive
                ? '조건을 줄이거나 시간 범위를 넓혀 보세요.'
                : '수신 전이거나 보존 기간이 지났을 수 있습니다. 설치 상태와 시간 범위를 확인하세요.'}
            </span>
          </div>
        )
      ) : rows.length > 0 ? (
        <div
          ref={scroller}
          className="mt-log-scroll"
          onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
          style={{ height: `${String((Math.min(rows.length, VIEW_ROWS) + 1) * LOG_ROW_H + 2)}px` }}
        >
          <div role="table" aria-label="log 결과" aria-rowcount={truncated ? -1 : rows.length + 1} className="mt-log-table">
            <div role="rowgroup" className="mt-log-head">
              <div role="row" aria-rowindex={1} className="mt-log-row mt-log-row--head">
                <span role="columnheader">시각</span>
                <span role="columnheader">심각도</span>
                <span role="columnheader">서비스</span>
                <span role="columnheader">본문</span>
                <span role="columnheader" className="mt-log-col-trace">
                  Trace
                </span>
              </div>
            </div>
            <div role="rowgroup" style={{ height: `${String(rows.length * LOG_ROW_H)}px`, position: 'relative' }}>
              {[...indices]
                .sort((a, b) => a - b)
                .map((i) => {
                  const r = rows[i];
                  if (r === undefined) return null;
                  const svc = serviceById.get(r.service_id);
                  return (
                    <div
                      key={r.event_id}
                      role="row"
                      aria-rowindex={i + 2}
                      className={r.event_id === selectedId ? 'mt-log-row mt-log-row--selected' : 'mt-log-row'}
                      style={{ top: `${String(i * LOG_ROW_H)}px` }}
                    >
                      <span role="cell" className="mt-mono mt-label">
                        {formatDateTime(r.time, timeZone)}
                      </span>
                      <span role="cell">
                        <span className={isErrorSeverity(r.severity_number) ? 'mt-log-sev mt-log-sev--err' : 'mt-log-sev'}>{severityLabel(r.severity_number)}</span>
                      </span>
                      <span role="cell" className="mt-log-cell mt-mono" title={svc === undefined ? r.service_id : serviceLabel(svc)}>
                        {svc === undefined ? (
                          <>
                            <span className="mt-muted" aria-hidden="true">
                              {r.service_id.slice(0, 8)}…
                            </span>
                            <span className="mt-visually-hidden">catalog에 없는 서비스 {r.service_id}</span>
                          </>
                        ) : (
                          serviceLabel(svc)
                        )}
                      </span>
                      <span role="cell" className="mt-log-cell">
                        <button
                          id={`log-row-${r.event_id}`}
                          type="button"
                          className="mt-log-open mt-mono"
                          aria-label={`${severityLabel(r.severity_number)} ${formatDateTime(r.time, timeZone)} log 상세: ${r.body.slice(0, 120)}`}
                          aria-current={r.event_id === selectedId ? 'true' : undefined}
                          onFocus={() => setFocusIdx(i)}
                          onClick={() => onSelect(r.event_id)}
                        >
                          {r.body === '' ? <span className="mt-muted">(본문 없음)</span> : r.body}
                        </button>
                      </span>
                      <span role="cell" className="mt-log-col-trace">
                        {r.trace_id === null ? (
                          <span className="mt-muted">-</span>
                        ) : (
                          <Link
                            className="mt-mono mt-trace-id"
                            aria-label={`trace ${r.trace_id} 상세`}
                            to={{ pathname: orgPath(org, `traces/${r.trace_id}`), search: traceLinkSearch(search, r, Date.now()) }}
                          >
                            {r.trace_id.slice(0, 8)}…
                          </Link>
                        )}
                      </span>
                    </div>
                  );
                })}
            </div>
          </div>
        </div>
      ) : null}
      <div className="mt-card__footer">
        {truncated && (
          <button type="button" className="mt-button" onClick={onMore} disabled={state.loadingMore}>
            {state.loadingMore ? '불러오는 중…' : `다음 ${String(LOG_PAGE)}개`}
          </button>
        )}
        {state.requestId !== null && <span className="mt-label mt-mono">request_id {state.requestId}</span>}
      </div>
    </section>
  );
}
