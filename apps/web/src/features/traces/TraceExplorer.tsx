// S04 Trace Explorer (D05 §06, F02·F04, ADR 0043).
// 좌측 조건, 상단 범위(조사 context), 중앙 duration 분포, 하단 결과 표. page 100, "더 보기"는 cursor.
// - 보존된(tail sampling) trace만 나온다. 요청량·오류율은 서비스 지표(비샘플링)에서 본다(계약 5).
// - 오류 span이 없는 trace를 "성공"으로 단정하지 않는다. 구조가 불완전하면 사유를 보인다(D05 §06 연결 상태).
import { useEffect, useMemo, useState } from 'react';
import { Link, useOutletContext, useParams, useSearchParams } from 'react-router';
import { listServices } from '../../api/catalog.ts';
import { useRemote } from '../../api/useRemote.ts';
import { contextOnly, formatRange, resolveRange, writeContext, type InvestigationContext } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { formatDateTime, formatMilliseconds } from '../services/format.ts';
import { ErrorNotice, Skeleton, StatusBadge } from '../services/states.tsx';
import '../services/services.css';
import {
  MAX_MIN_MS,
  MAX_NAME_LENGTH,
  MAX_TRACE_RANGE_MS,
  REASON_TEXT,
  TRACE_PAGE,
  parseTraceFilter,
  toFilterNode,
  traceDetailWindow,
  writeTraceFilter,
  type TraceFilterState,
} from './filters.ts';
import { TraceScatter } from './TraceScatter.tsx';
import { useTraceSearch } from './useTraceSearch.ts';
import './traces.css';

export function TraceExplorer() {
  const { org = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const [params, setParams] = useSearchParams();
  const filter = parseTraceFilter(params);
  const [refreshTick, setRefreshTick] = useState(0);
  const rangeKey = JSON.stringify(ctx.range);
  const nowMs = useMemo(() => Date.now(), [rangeKey, refreshTick]);
  const { fromMs, toMs } = resolveRange(ctx.range, nowMs);
  const tooLong = toMs - fromMs > MAX_TRACE_RANGE_MS;
  const filterNode = toFilterNode(filter);
  const request = tooLong
    ? null
    : { range: { from: new Date(fromMs).toISOString(), to: new Date(toMs).toISOString() }, limit: TRACE_PAGE, ...(filterNode === undefined ? {} : { filter: filterNode }) };
  const queryKey = request === null ? null : `${org}|traces|${JSON.stringify(request)}`;
  const { state, loadMore } = useTraceSearch(queryKey, request, refreshTick);

  // 서비스 선택지: catalog (environment 조사 context를 따른다)
  const services = useRemote(`${org}|services|${ctx.environment ?? ''}`, 0, (signal) =>
    listServices({ environment: ctx.environment, cursor: null, limit: 1000 }, signal),
  );

  const setFilter = (next: TraceFilterState) => setParams(writeTraceFilter(params, next));
  const brush = (a: number, b: number) =>
    setParams(writeContext(params, { ...ctx, range: { kind: 'absolute', fromMs: a, toMs: b } }), { replace: false });
  const search = contextOnly(params);

  return (
    <section className="mt-page" aria-labelledby="traces-title">
      <header className="mt-page__header">
        <h1 id="traces-title" className="mt-title">
          Traces
        </h1>
        <span className="mt-label">{formatRange(ctx.range, nowMs, ctx.timeZone)} · 시작 시각 최신순</span>
        <button type="button" className="mt-button mt-page__action" onClick={() => setRefreshTick((n) => n + 1)}>
          새로고침
        </button>
      </header>
      <div className="mt-notice mt-notice--info" role="note">
        보존된(샘플링된) trace만 표시합니다. 요청량·오류율은{' '}
        {filter.serviceId === null ? (
          <Link to={{ pathname: orgPath(org, 'services'), search }}>서비스 지표</Link>
        ) : (
          <Link to={{ pathname: orgPath(org, `services/${filter.serviceId}`), search }}>이 서비스의 지표</Link>
        )}
        에서 비샘플링 값으로 확인하세요.
      </div>

      <div className="mt-explorer">
        <FilterPanel
          filter={filter}
          onChange={setFilter}
          services={services.data ?? []}
          servicesLoading={services.status === 'loading'}
        />
        <div className="mt-explorer__main">
          {tooLong && (
            <div className="mt-notice mt-notice--warning" role="alert">
              trace 검색은 24시간까지 조회할 수 있습니다. 시간 범위를 줄이세요.
            </div>
          )}
          {state.error !== null && (
            <ErrorNotice
              error={state.error}
              lastSuccessMs={state.rows.length > 0 ? state.lastSuccessMs : null}
              onRetry={state.rows.length > 0 && state.nextCursor !== null ? loadMore : () => setRefreshTick((n) => n + 1)}
            />
          )}
          {state.warnings.includes('interactive_row_limit_reached') && (
            <div className="mt-notice mt-notice--warning" role="note">
              화면에서 이어 볼 수 있는 10,000개에 닿았습니다. 조건이나 범위를 좁히세요.
            </div>
          )}
          {!tooLong && (
            <section className="mt-card" aria-labelledby="dist-title">
              <div className="mt-card__head mt-card__head--inset">
                <h2 id="dist-title" className="mt-card__title">
                  Duration 분포
                </h2>
                <span className="mt-legend">
                  <span className="mt-legend__ok" aria-hidden="true" />
                  오류 span 없음
                </span>
                <span className="mt-legend">
                  <span className="mt-legend__err" aria-hidden="true" />
                  오류 span 있음
                </span>
                <span className="mt-label" id="dist-hint">
                  불러온 {state.rows.length.toLocaleString('ko-KR')}개 · 끌어서 구간 선택(키보드는 상단 시간 선택)
                </span>
                {ctx.range.kind === 'absolute' && (
                  <button type="button" className="mt-button" onClick={() => window.history.back()}>
                    범위 되돌리기
                  </button>
                )}
              </div>
              {state.status === 'loading' ? (
                <Skeleton height={240} label="trace를 불러오는 중" />
              ) : (
                <TraceScatter rows={state.rows} fromMs={fromMs} toMs={toMs} timeZone={ctx.timeZone} onBrush={brush} />
              )}
            </section>
          )}
          {!tooLong && (
            <ResultTable org={org} params={params} state={state} timeZone={ctx.timeZone} onMore={loadMore} filterActive={filterNode !== undefined} />
          )}
        </div>
      </div>
    </section>
  );
}

function FilterPanel({
  filter,
  onChange,
  services,
  servicesLoading,
}: {
  filter: TraceFilterState;
  onChange: (f: TraceFilterState) => void;
  services: { service_id: string; name: string; namespace: string; environment: string }[];
  servicesLoading: boolean;
}) {
  // 입력 중에는 URL을 바로 바꾸지 않는다: 300ms 뒤 또는 Enter에 반영(D05 §03)
  const [name, setName] = useState(filter.name);
  const [minMs, setMinMs] = useState(filter.minMs === null ? '' : String(filter.minMs));
  useEffect(() => setName(filter.name), [filter.name]);
  useEffect(() => setMinMs(filter.minMs === null ? '' : String(filter.minMs)), [filter.minMs]);
  const minValid = minMs === '' || (/^[0-9]{1,9}$/.test(minMs) && Number(minMs) <= MAX_MIN_MS);
  const commit = (n: string, m: string) => {
    if (m !== '' && !(/^[0-9]{1,9}$/.test(m) && Number(m) <= MAX_MIN_MS)) return;
    onChange({ ...filter, name: n.slice(0, MAX_NAME_LENGTH), minMs: m === '' ? null : Number(m) });
  };
  // 입력값이 바뀔 때만 예약한다(filter가 바뀌어 입력이 동기화될 때는 비교로 건너뛴다)
  useEffect(() => {
    if (name === filter.name && minMs === (filter.minMs === null ? '' : String(filter.minMs))) return;
    const t = window.setTimeout(() => commit(name, minMs), 300);
    return () => window.clearTimeout(t);
  }, [name, minMs]);

  return (
    <aside className="mt-card mt-explorer__filters" aria-label="검색 조건">
      <form
        onSubmit={(e) => {
          e.preventDefault();
          commit(name, minMs);
        }}
      >
        <div className="mt-field">
          <label htmlFor="tf-service">서비스 (span이 있는)</label>
          <select
            id="tf-service"
            className="mt-select"
            value={filter.serviceId ?? ''}
            disabled={servicesLoading}
            onChange={(e) => onChange({ ...filter, serviceId: e.target.value === '' ? null : e.target.value })}
          >
            <option value="">전체</option>
            {services.map((s) => (
              <option key={s.service_id} value={s.service_id}>
                {s.namespace === '' ? s.name : `${s.namespace}/${s.name}`} · {s.environment}
              </option>
            ))}
          </select>
        </div>
        <div className="mt-field mt-field--check">
          <input id="tf-errors" type="checkbox" checked={filter.errorsOnly} onChange={(e) => onChange({ ...filter, errorsOnly: e.target.checked })} />
          <label htmlFor="tf-errors">오류 span이 있는 trace만</label>
        </div>
        <div className="mt-field">
          <label htmlFor="tf-min">최소 duration (ms)</label>
          <input
            id="tf-min"
            className="mt-input"
            inputMode="numeric"
            value={minMs}
            aria-invalid={!minValid}
            aria-describedby={minValid ? undefined : 'tf-min-err'}
            onChange={(e) => setMinMs(e.target.value.trim())}
          />
          {!minValid && (
            <span id="tf-min-err" className="mt-field__error">
              0 이상 정수(최대 {MAX_MIN_MS.toLocaleString('ko-KR')})를 입력하세요.
            </span>
          )}
        </div>
        <div className="mt-field">
          <label htmlFor="tf-name">span 이름 포함</label>
          <input id="tf-name" className="mt-input" type="search" maxLength={MAX_NAME_LENGTH} value={name} onChange={(e) => setName(e.target.value)} />
          <span className="mt-label">공유 링크에는 들어가지 않습니다.</span>
        </div>
        <button type="submit" className="mt-button">
          조회
        </button>
      </form>
    </aside>
  );
}

function ResultTable({
  org,
  params,
  state,
  timeZone,
  onMore,
  filterActive,
}: {
  org: string;
  params: URLSearchParams;
  state: ReturnType<typeof useTraceSearch>['state'];
  timeZone: string;
  onMore: () => void;
  filterActive: boolean;
}) {
  const detailSearch = (startIso: string, durationMs: number) => {
    const q = new URLSearchParams(contextOnly(params));
    const w = traceDetailWindow(startIso, durationMs);
    q.delete('range');
    q.set('from', w.from);
    q.set('to', w.to);
    return `?${q.toString()}`;
  };
  return (
    <section className="mt-card mt-card--flush" aria-labelledby="results-title">
      <div className="mt-card__head">
        <h2 id="results-title" className="mt-card__title">
          결과
        </h2>
        <span className="mt-label">
          {state.rows.length.toLocaleString('ko-KR')}개{state.nextCursor !== null ? ' 이상' : ''} · {TRACE_PAGE}개씩 · 시작 시각 최신순(동률은 trace ID)
        </span>
      </div>
      {state.status === 'loading' ? (
        <Skeleton height={160} label="결과를 불러오는 중" />
      ) : state.status === 'success' && state.rows.length === 0 ? (
        <div className="mt-empty mt-empty--inline">
          <strong>{filterActive ? '조건에 맞는 trace가 없습니다' : '이 범위에 보존된 trace가 없습니다'}</strong>
          <span>
            {filterActive
              ? '조건을 줄이거나 시간 범위를 넓혀 보세요.'
              : '샘플링으로 보존되지 않았거나 수신 전일 수 있습니다. 서비스 지표에서 요청이 있었는지 확인하세요.'}
          </span>
        </div>
      ) : state.rows.length > 0 ? (
        <div className="mt-scroll-x">
          <table className="mt-table">
            <thead>
              <tr>
                <th scope="col">Trace ID</th>
                <th scope="col">Root</th>
                <th scope="col">시작</th>
                <th scope="col" className="mt-num">
                  Duration
                </th>
                <th scope="col">오류</th>
                <th scope="col" className="mt-num">
                  Spans
                </th>
                <th scope="col">완전성</th>
              </tr>
            </thead>
            <tbody>
              {state.rows.map((r) => (
                <tr key={r.trace_id}>
                  <td>
                    <Link className="mt-mono mt-trace-id" to={{ pathname: orgPath(org, `traces/${r.trace_id}`), search: detailSearch(r.start_time, r.duration_ms) }}>
                      {r.trace_id.slice(0, 8)}…{r.trace_id.slice(-4)}
                    </Link>
                  </td>
                  <td className="mt-trace-root">
                    {r.root_name === null ? (
                      <span className="mt-muted">root 없음</span>
                    ) : (
                      <>
                        <span className="mt-mono">{r.root_service ?? r.root_service_id ?? '알 수 없는 서비스'}</span>{' '}
                        <span className="mt-muted">{r.root_name}</span>
                      </>
                    )}
                  </td>
                  <td className="mt-num-left mt-mono">{formatDateTime(r.start_time, timeZone)}</td>
                  <td className="mt-num">{formatMilliseconds(r.duration_ms)}</td>
                  <td>
                    {r.has_error ? <StatusBadge tone="critical">오류 span 있음</StatusBadge> : <span className="mt-muted">오류 span 없음</span>}
                  </td>
                  <td className="mt-num">{r.span_count.toLocaleString('ko-KR')}</td>
                  <td>
                    {r.complete ? (
                      <span className="mt-muted">완전</span>
                    ) : (
                      <StatusBadge tone="warning">불완전 · {r.reasons.map((x) => REASON_TEXT[x] ?? x).join(', ')}</StatusBadge>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      <div className="mt-card__footer">
        {state.nextCursor !== null && (
          <button type="button" className="mt-button" onClick={onMore} disabled={state.loadingMore}>
            {state.loadingMore ? '불러오는 중…' : `다음 ${String(TRACE_PAGE)}개`}
          </button>
        )}
        {state.requestId !== null && <span className="mt-label mt-mono">request_id {state.requestId}</span>}
      </div>
    </section>
  );
}
