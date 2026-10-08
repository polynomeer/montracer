// S05 Trace 상세 (D05 §06 Waterfall·연결 상태, §11, F02, ADR 0044).
// - waterfall: 행 28px, 서비스 색 띠, 이름·종류·상대 시작·소요시간·상태. 가상화(보이는 행만 그린다)라 10,000 span도 그린다.
// - 키보드: ↑↓ 이동, → 펼침/첫 자식, ← 접힘/부모, Enter 상세, Esc 닫기. 입력창 안에서는 동작하지 않는다(트리에서만 받는다).
// - 원본 시각을 고치지 않는다. 부모 밖으로 나간 span은 clock skew로 표시, 부모가 없는 span은 orphan 그룹.
// - 선택 span은 URL(entity·tab)에 남아 뒤로 가기·공유로 복원된다.
import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react';
import { Link, useOutletContext, useParams, useSearchParams } from 'react-router';
import { getTrace } from '../../api/traces.ts';
import type { TraceDetail as TraceDetailData } from '../../api/types.ts';
import { useRemote } from '../../api/useRemote.ts';
import { contextOnly, resolveRange, type InvestigationContext } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { formatDateTime, formatMilliseconds } from '../services/format.ts';
import { ErrorNotice, Skeleton, StatusBadge } from '../services/states.tsx';
import '../services/services.css';
import { REASON_TEXT } from './filters.ts';
import { SpanDrawer } from './SpanDrawer.tsx';
import { buildTree, criticalPath, errorSpans, matchSpans, visibleRows, type Row, type SpanNode, type TraceTree } from './waterfall.ts';
import './traces.css';

export const MAX_TRACE_LOOKUP_MS = 7 * 24 * 60 * 60_000; // ADR 0022
const ROW_H = 28;
const VIEW_ROWS = 20;
const OVERSCAN = 8;
const TRACE_ID = /^[0-9a-f]{32}$/;

export function TraceDetail() {
  const { org = '', traceId = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const [params, setParams] = useSearchParams();
  const rangeKey = JSON.stringify(ctx.range);
  const nowMs = useMemo(() => Date.now(), [rangeKey]);
  const { fromMs, toMs } = resolveRange(ctx.range, nowMs);
  const tooLong = toMs - fromMs > MAX_TRACE_LOOKUP_MS;
  const validId = TRACE_ID.test(traceId);
  const from = new Date(fromMs).toISOString();
  const to = new Date(toMs).toISOString();
  const [retry, setRetry] = useState(0);
  const trace = useRemote(
    validId && !tooLong ? `${org}|trace|${traceId}|${from}|${to}` : null,
    retry,
    (signal) => getTrace(traceId, from, to, signal),
  );
  const explorer = { pathname: orgPath(org, 'traces'), search: contextOnly(params) };

  return (
    <section className="mt-page" aria-labelledby="trace-title">
      <nav aria-label="경로" className="mt-breadcrumb">
        <Link to={explorer}>Traces</Link>
        <span aria-hidden="true">/</span>
        <span className="mt-mono">
          {traceId.slice(0, 8)}…{traceId.slice(-4)}
        </span>
      </nav>
      {!validId ? (
        <Missing title="trace ID 형식이 아닙니다" detail="trace ID는 소문자 hex 32자입니다." explorer={explorer} />
      ) : tooLong ? (
        <div className="mt-notice mt-notice--warning" role="alert">
          trace 하나는 7일 범위 안에서 찾습니다. 시간 범위를 줄이세요.
        </div>
      ) : trace.error?.status === 404 ? (
        <Missing
          title="이 범위에서 trace를 찾을 수 없습니다"
          detail="이유는 확인할 수 없습니다. 범위 밖이거나, 샘플링으로 보존되지 않았거나, 보존 기간이 지났거나, 볼 권한이 없을 수 있습니다. 시간 범위를 넓혀 보세요."
          requestId={trace.error.body.request_id}
          explorer={explorer}
        />
      ) : trace.error !== null ? (
        <ErrorNotice error={trace.error} lastSuccessMs={trace.lastSuccessMs} onRetry={() => setRetry((n) => n + 1)} />
      ) : trace.data === null ? (
        <Skeleton height={320} label="trace를 불러오는 중" />
      ) : (
        <Loaded
          org={org}
          data={trace.data}
          partial={trace.meta?.partial === true}
          range={{ from, to }}
          params={params}
          setParams={setParams}
          timeZone={ctx.timeZone}
        />
      )}
    </section>
  );
}

function Missing({ title, detail, requestId, explorer }: { title: string; detail: string; requestId?: string; explorer: { pathname: string; search: string } }) {
  return (
    <div className="mt-empty">
      <h1 id="trace-title" className="mt-title">
        {title}
      </h1>
      <span>{detail}</span>
      {requestId !== undefined && requestId !== '' && <span className="mt-label mt-mono">request_id {requestId}</span>}
      <Link to={explorer}>Trace Explorer로</Link>
    </div>
  );
}

const serviceColor = (i: number) => `var(--mt-series-${String((i % 4) + 1)})`;

type Mode = 'all' | 'errors' | 'critical';

function Loaded({
  org,
  data,
  partial,
  range,
  params,
  setParams,
  timeZone,
}: {
  org: string;
  data: TraceDetailData;
  partial: boolean;
  range: { from: string; to: string };
  params: URLSearchParams;
  setParams: (p: URLSearchParams, opts?: { replace?: boolean }) => void;
  timeZone: string;
}) {
  const tree = useMemo(() => buildTree(data.spans), [data]);
  const critical = useMemo(() => criticalPath(tree), [tree]);
  const errors = useMemo(() => errorSpans(tree), [tree]);
  const [mode, setMode] = useState<Mode>('all');
  const [query, setQuery] = useState('');
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());
  const [view, setView] = useState<{ a: number; b: number }>({ a: 0, b: Math.max(tree.durationNs, 1) });
  const treeRef = useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = useState(0);

  const only = query.trim() !== '' ? matchSpans(tree, query) : mode === 'errors' ? errors : mode === 'critical' ? critical : null;
  const rows = useMemo(() => visibleRows(tree, { collapsed, only }), [tree, collapsed, only]);
  const selectedId = params.get('entity');
  const selected = selectedId === null ? undefined : tree.byId.get(selectedId);
  // 키보드 focus(화살표로 이동)와 선택(상세 패널, Enter·클릭)은 다르다
  const [focusId, setFocusId] = useState<string | null>(selectedId);
  const focusIndex = rows.findIndex((r) => r.node !== null && r.node.span.span_id === focusId);

  const root = tree.roots[0];
  // 공유 링크·뒤로 가기로 연 선택 span이 보이게 처음 한 번 스크롤한다
  useEffect(() => {
    if (selectedId === null) return;
    const i = rows.findIndex((r) => r.node !== null && r.node.span.span_id === selectedId);
    if (i >= 0) scrollTo(i);
    // 처음 그릴 때만
  }, []);
  const errorCount = errors.size;
  const select = (n: SpanNode | null, open = true) => {
    const next = new URLSearchParams(params);
    if (n === null || !open) {
      next.delete('entity');
      next.delete('tab');
    } else {
      next.set('entity', n.span.span_id);
    }
    setParams(next, { replace: true });
  };
  const scrollTo = (i: number) => {
    const el = treeRef.current;
    if (el === null) return;
    const top = i * ROW_H;
    if (top < el.scrollTop) el.scrollTop = top;
    else if (top + ROW_H > el.scrollTop + el.clientHeight) el.scrollTop = top + ROW_H - el.clientHeight;
  };
  const toggle = (id: string, open?: boolean) =>
    setCollapsed((c) => {
      const next = new Set(c);
      const isOpen = !c.has(id);
      if (open ?? !isOpen) next.delete(id);
      else next.add(id);
      return next;
    });

  const onKey = (e: KeyboardEvent<HTMLDivElement>) => {
    const cur = focusIndex;
    const go = (i: number) => {
      const r = rows[i];
      if (r?.node !== undefined && r.node !== null) {
        setFocusId(r.node.span.span_id);
        scrollTo(i);
      }
    };
    const node = rows[cur]?.node ?? null;
    switch (e.key) {
      case 'ArrowDown': {
        let i = cur + 1;
        while (i < rows.length && rows[i]?.node === null) i++;
        go(i);
        break;
      }
      case 'ArrowUp': {
        let i = cur - 1;
        while (i >= 0 && rows[i]?.node === null) i--;
        go(Math.max(i, 0));
        break;
      }
      case 'ArrowRight':
        if (node !== null && rows[cur]?.hasChildren) {
          if (rows[cur]?.collapsed) toggle(node.span.span_id, true);
          else go(cur + 1);
        }
        break;
      case 'ArrowLeft':
        if (node !== null) {
          if (rows[cur]?.hasChildren && !rows[cur]?.collapsed) toggle(node.span.span_id, false);
          else if (node.parent !== null) go(rows.findIndex((r) => r.node === node.parent));
        }
        break;
      case 'Home':
        go(rows.findIndex((r) => r.node !== null));
        break;
      case 'End':
        go(rows.length - 1);
        break;
      case 'Enter':
        if (node !== null) select(node);
        break;
      case 'Escape':
        select(null);
        break;
      default:
        return;
    }
    e.preventDefault();
  };

  const first = Math.max(0, Math.floor(scrollTop / ROW_H) - OVERSCAN);
  const last = Math.min(rows.length, Math.ceil((scrollTop + VIEW_ROWS * ROW_H) / ROW_H) + OVERSCAN);
  const span = Math.max(view.b - view.a, 1);
  const pct = (ns: number) => `${String(((ns - view.a) / span) * 100)}%`;
  const zoom = (f: number) => {
    const mid = (view.a + view.b) / 2;
    const half = Math.max((span * f) / 2, 1000);
    setView({ a: Math.max(0, mid - half), b: Math.min(tree.durationNs, mid + half) });
  };
  const pan = (f: number) => {
    const d = span * f;
    const a = Math.min(Math.max(0, view.a + d), Math.max(0, tree.durationNs - span));
    setView({ a, b: a + span });
  };

  return (
    <>
      <header className="mt-svc-header">
        <div className="mt-svc-header__title">
          <h1 id="trace-title" className="mt-title mt-trace-title">
            {root === undefined ? 'root span 없음' : `${root.span.service_name} · ${root.span.name}`}
          </h1>
          {data.complete ? (
            <StatusBadge tone="info">구조 완전</StatusBadge>
          ) : (
            <StatusBadge tone="warning">불완전 · {data.reasons.map((r) => REASON_TEXT[r] ?? r).join(', ')}</StatusBadge>
          )}
          {errorCount > 0 ? (
            <StatusBadge tone="critical">오류 span {errorCount}개</StatusBadge>
          ) : (
            // 불완전하면 받지 못한 span에 오류가 있을 수 있다 — 단정하지 않는다
            <span className="mt-label">{data.complete ? '오류 span 없음' : '받은 span 중 오류 없음'}</span>
          )}
          {partial && <StatusBadge tone="warning">일부 저장소 응답 없음 · 결과가 빠졌을 수 있음</StatusBadge>}
          {tree.skewCount > 0 && <StatusBadge tone="neutral">clock skew 의심 {tree.skewCount}개 · 시각은 원본 그대로</StatusBadge>}
        </div>
        <dl className="mt-svc-meta">
          <div>
            <dt>trace ID</dt>
            <dd className="mt-mono">
              {data.trace_id}{' '}
              <button type="button" className="mt-linklike" onClick={() => void navigator.clipboard?.writeText(data.trace_id).catch(() => undefined)}>
                복사
              </button>
            </dd>
          </div>
          <div>
            <dt>시작</dt>
            <dd>{formatDateTime(new Date(tree.traceStartMs).toISOString(), timeZone)}</dd>
          </div>
          <div>
            <dt>duration</dt>
            <dd>{formatMilliseconds(tree.durationNs / 1e6)}</dd>
          </div>
          <div>
            <dt>span</dt>
            <dd>{data.span_count.toLocaleString('ko-KR')}</dd>
          </div>
          <div>
            <dt>마지막 수신</dt>
            <dd>{formatDateTime(data.last_updated_at, timeZone)}</dd>
          </div>
        </dl>
        <ul className="mt-legend-list" aria-label="서비스 색">
          {tree.services.map((s, i) => (
            <li key={s} className="mt-legend">
              <span className="mt-legend__swatch" style={{ background: serviceColor(i) }} aria-hidden="true" />
              {s}
            </li>
          ))}
        </ul>
      </header>

      <div className="mt-trace-layout">
        <section className="mt-card mt-card--flush mt-trace-main" aria-label="Waterfall">
          <div className="mt-waterfall__toolbar">
            <label htmlFor="wf-search" className="mt-visually-hidden">
              span 검색
            </label>
            <input id="wf-search" className="mt-input mt-waterfall__search" type="search" placeholder="span 이름·서비스 검색" value={query} onChange={(e) => setQuery(e.target.value)} maxLength={200} />
            <div role="group" aria-label="보기" className="mt-segmented">
              {(
                [
                  ['all', '전체'],
                  ['errors', `오류만 (${String(errorCount)})`],
                  ['critical', 'Critical path (추정)'],
                ] as const
              ).map(([m, label]) => (
                <button key={m} type="button" aria-pressed={mode === m} className="mt-button" onClick={() => setMode(m)}>
                  {label}
                </button>
              ))}
            </div>
            <div role="group" aria-label="시간축" className="mt-segmented">
              <button type="button" className="mt-button" aria-label="시간축 확대" onClick={() => zoom(0.5)}>
                +
              </button>
              <button type="button" className="mt-button" aria-label="시간축 축소" onClick={() => zoom(2)}>
                −
              </button>
              <button type="button" className="mt-button" aria-label="왼쪽으로" onClick={() => pan(-0.25)}>
                ◀
              </button>
              <button type="button" className="mt-button" aria-label="오른쪽으로" onClick={() => pan(0.25)}>
                ▶
              </button>
              <button type="button" className="mt-button" onClick={() => setView({ a: 0, b: Math.max(tree.durationNs, 1) })}>
                맞춤
              </button>
              {selected !== undefined && (
                <button type="button" className="mt-button" onClick={() => setView({ a: selected.startNs, b: Math.max(selected.endNs, selected.startNs + 1000) })}>
                  선택 span에 맞춤
                </button>
              )}
            </div>
          </div>
          <div className="mt-waterfall__axis mt-num" aria-hidden="true">
            <span>span</span>
            <span className="mt-waterfall__ticks">
              {[0, 0.25, 0.5, 0.75, 1].map((f) => (
                <span key={f}>{formatMilliseconds((view.a + f * span) / 1e6)}</span>
              ))}
            </span>
          </div>
          {rows.length === 0 ? (
            <div className="mt-empty mt-empty--inline">
              <span>{query.trim() !== '' ? '검색어와 맞는 span이 없습니다.' : '이 보기에 해당하는 span이 없습니다.'}</span>
            </div>
          ) : (
            <div
              ref={treeRef}
              className="mt-waterfall"
              role="tree"
              aria-label={`span ${String(data.span_count)}개. 위아래 화살표로 이동, 오른쪽 펼침, 왼쪽 접힘, Enter 상세, Esc 닫기`}
              aria-activedescendant={focusIndex >= 0 ? `row-${focusId ?? ''}` : undefined}
              tabIndex={0}
              onKeyDown={onKey}
              onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
              style={{ height: `${String(Math.min(rows.length, VIEW_ROWS) * ROW_H + 2)}px` }}
            >
              <div style={{ height: `${String(rows.length * ROW_H)}px`, position: 'relative' }}>
                {/* 보이는 구간 + 키보드 focus 행(화면 밖이어도 aria-activedescendant가 가리킬 수 있게) */}
                {rows
                  .map((r, i) => ({ r, i }))
                  .filter(({ i }) => (i >= first && i < last) || i === focusIndex)
                  .map(({ r, i }) => (
                    <WaterfallRow
                      key={r.node === null ? 'orphans' : r.node.span.span_id}
                      row={r}
                      top={i * ROW_H}
                      tree={tree}
                      selected={r.node !== null && r.node.span.span_id === selectedId}
                      focused={r.node !== null && r.node.span.span_id === focusId}
                      critical={r.node !== null && mode === 'critical' && critical.has(r.node.span.span_id)}
                      left={pct}
                      span={span}
                      viewStart={view.a}
                      onSelect={(n) => {
                        setFocusId(n.span.span_id);
                        select(n);
                      }}
                      onToggle={(id) => toggle(id)}
                    />
                  ))}
              </div>
            </div>
          )}
          <p className="mt-label mt-waterfall__note">
            보이는 행 {rows.length.toLocaleString('ko-KR')}개 · critical path는 가장 늦게 끝난 자식을 따라간 추정이며 duration을 합하지 않습니다.
          </p>
        </section>
        {selected !== undefined && (
          <SpanDrawer
            org={org}
            node={selected}
            tree={tree}
            color={serviceColor(tree.services.indexOf(selected.span.service_name))}
            traceRange={range}
            tab={params.get('tab')}
            onTab={(t) => {
              const next = new URLSearchParams(params);
              next.set('tab', t);
              setParams(next, { replace: true });
            }}
            onClose={() => {
              select(null);
              treeRef.current?.focus();
            }}
            timeZone={timeZone}
          />
        )}
      </div>
    </>
  );
}

function WaterfallRow({
  row,
  top,
  tree,
  selected,
  focused,
  critical,
  left,
  span,
  viewStart,
  onSelect,
  onToggle,
}: {
  row: Row;
  top: number;
  tree: TraceTree;
  selected: boolean;
  focused: boolean;
  critical: boolean;
  left: (ns: number) => string;
  span: number;
  viewStart: number;
  onSelect: (n: SpanNode) => void;
  onToggle: (id: string) => void;
}) {
  if (row.node === null) {
    return (
      <div
        className="mt-wf-row mt-wf-row--group"
        style={{ top }}
        role="treeitem"
        aria-level={1}
        aria-selected={false}
        aria-posinset={row.posInSet}
        aria-setsize={row.setSize}
      >
        부모 span이 없는 span {tree.orphans.length}개 (orphan · 부모 미수신 또는 권한 범위 밖)
      </div>
    );
  }
  const n = row.node;
  const s = n.span;
  const color = `var(--mt-series-${String((tree.services.indexOf(s.service_name) % 4) + 1)})`;
  const widthPct = Math.max(((n.endNs - n.startNs) / span) * 100, 0.2);
  const startPct = ((n.startNs - viewStart) / span) * 100;
  const label = `${s.service_name} ${s.name}, ${formatMilliseconds(s.duration_ns / 1e6)}${s.status_code === 'error' ? ', 오류' : ''}${n.skew ? ', clock skew 의심' : ''}`;
  return (
    <div
      id={`row-${s.span_id}`}
      className={`mt-wf-row${selected ? ' mt-wf-row--selected' : ''}${focused ? ' mt-wf-row--focused' : ''}${critical ? ' mt-wf-row--critical' : ''}`}
      style={{ top, borderLeftColor: color }}
      role="treeitem"
      aria-level={row.depth + 1}
      aria-posinset={row.posInSet}
      aria-setsize={row.setSize}
      aria-expanded={row.hasChildren ? !row.collapsed : undefined}
      aria-selected={selected}
      aria-label={label}
      onClick={() => onSelect(n)}
    >
      <span className="mt-wf-name" style={{ paddingLeft: `${String(8 + row.depth * 14)}px` }}>
        {row.hasChildren ? (
          <button
            type="button"
            tabIndex={-1}
            className="mt-wf-caret"
            aria-label={row.collapsed ? '펼치기' : '접기'}
            onClick={(e) => {
              e.stopPropagation();
              onToggle(s.span_id);
            }}
          >
            {row.collapsed ? '▸' : '▾'}
          </button>
        ) : (
          <span className="mt-wf-caret" aria-hidden="true" />
        )}
        {s.status_code === 'error' && (
          <span className="mt-wf-err" aria-hidden="true">
            ✕
          </span>
        )}
        <span className="mt-mono mt-wf-label">{s.name}</span>
        <span className="mt-wf-svc">{s.service_name}</span>
        {n.skew && <span className="mt-wf-skew">skew</span>}
      </span>
      <span className="mt-wf-track">
        <span
          className={`mt-wf-bar${s.status_code === 'error' ? ' mt-wf-bar--err' : ''}`}
          style={{ left: `${String(startPct)}%`, width: `${String(widthPct)}%`, background: s.status_code === 'error' ? undefined : color }}
        />
        <span className="mt-wf-dur mt-num" style={{ left: startPct + widthPct > 85 ? `calc(${left(n.startNs)} - 70px)` : `calc(${String(startPct + widthPct)}% + 6px)` }}>
          {formatMilliseconds(s.duration_ns / 1e6)}
        </span>
      </span>
    </div>
  );
}
