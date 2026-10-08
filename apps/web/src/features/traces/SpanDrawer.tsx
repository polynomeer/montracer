// 선택 span 상세 (D05 §06: attributes/events/links/logs/DB/profile 탭, 연결 상태).
// - 화면 옆 inline 패널이라 focus를 가두지 않는다(D05 §03). Esc·닫기로 닫고 waterfall로 focus를 돌려준다.
// - 값은 텍스트로만 그린다(log·SQL·속성, D05 §04). SQL literal을 복원하지 않는다.
// - log: 이 span의 trace·span ID가 정확히 같은 것은 linked, 같은 서비스·span 앞뒤 30초는 related(D02 §07, D05 §06).
import { useState, type KeyboardEvent } from 'react';
import { Link, useSearchParams } from 'react-router';
import { searchLogs } from '../../api/traces.ts';
import type { LogItem } from '../../api/types.ts';
import { useRemote } from '../../api/useRemote.ts';
import { contextOnly } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { isErrorSeverity, severityLabel } from '../logs/logFilters.ts';
import { formatDateTime, formatMilliseconds } from '../services/format.ts';
import { ErrorNotice, Skeleton, StatusBadge } from '../services/states.tsx';
import { nsBetween, type SpanNode, type TraceTree } from './waterfall.ts';

const TABS = [
  ['attributes', '속성'],
  ['events', '이벤트'],
  ['links', '링크'],
  ['logs', '로그'],
  ['db', 'DB'],
  ['profile', 'Profile'],
] as const;
type Tab = (typeof TABS)[number][0];

/** D02 §07: 서비스 → log 기본 범위는 span 시작 전후 30초(사용자가 넓힐 수 있다) */
export const RELATED_LOG_WINDOW_MS = 30_000;
const LOG_LIMIT = 50;
const MAX_LOG_RANGE_MS = 24 * 60 * 60_000; // ADR 0037

export function SpanDrawer({
  org,
  node,
  tree,
  color,
  traceRange,
  tab,
  onTab,
  onClose,
  timeZone,
}: {
  org: string;
  node: SpanNode;
  tree: TraceTree;
  color: string;
  traceRange: { from: string; to: string };
  tab: string | null;
  onTab: (t: Tab) => void;
  onClose: () => void;
  timeZone: string;
}) {
  const s = node.span;
  const active: Tab = TABS.some(([t]) => t === tab) ? (tab as Tab) : 'attributes';
  const onKey = (e: KeyboardEvent<HTMLElement>) => {
    if (e.key === 'Escape') {
      e.preventDefault();
      onClose();
    }
  };
  const parentGap = node.parent === null ? null : nsBetween(node.parent.span.start_time, s.start_time);
  return (
    <aside className="mt-card mt-span-drawer" aria-labelledby="span-title" onKeyDown={onKey}>
      <div className="mt-span-drawer__head" style={{ borderLeftColor: color }}>
        <div className="mt-span-drawer__title">
          <h2 id="span-title" className="mt-card__title mt-mono">
            {s.name}
          </h2>
          <button type="button" className="mt-icon-button" aria-label="span 상세 닫기 (Esc)" onClick={onClose}>
            ✕
          </button>
        </div>
        <div className="mt-svc-meta">
          <span>{s.service_name}</span>
          <span>{s.kind}</span>
          <span className="mt-num-left">{formatMilliseconds(s.duration_ns / 1e6)}</span>
          <span>
            trace의 {tree.durationNs > 0 ? ((s.duration_ns / tree.durationNs) * 100).toFixed(1) : '0'}% · 시작 +{formatMilliseconds(node.startNs / 1e6)}
          </span>
          {s.status_code === 'error' ? (
            <StatusBadge tone="critical">오류{s.status_message === '' ? '' : ` · ${s.status_message}`}</StatusBadge>
          ) : (
            <span className="mt-muted">status {s.status_code}</span>
          )}
        </div>
        {node.skew && parentGap !== null && (
          <p className="mt-label">
            부모 구간 밖으로 나갑니다(부모 시작 대비 {formatMilliseconds(parentGap / 1e6)}). 서로 다른 host의 시계 차이일 수 있어 시각을 고치지 않고 표시합니다.
          </p>
        )}
        {node.orphan && <p className="mt-label">부모 span({s.parent_span_id})이 이 trace에 없습니다. 수신되지 않았거나 볼 수 있는 범위 밖입니다.</p>}
      </div>
      <nav aria-label="span 상세 탭" className="mt-tabs mt-span-drawer__tabs">
        {TABS.map(([t, label]) => (
          <button key={t} type="button" className="mt-tab mt-tab--button" aria-current={t === active ? 'page' : undefined} onClick={() => onTab(t)}>
            {label}
            {t === 'events' && s.events.length > 0 ? ` ${String(s.events.length)}` : ''}
            {t === 'links' && s.links.length > 0 ? ` ${String(s.links.length)}` : ''}
          </button>
        ))}
      </nav>
      <div className="mt-span-drawer__body">
        {active === 'attributes' && (
          <>
            <KeyValues title="span 속성" values={s.attributes} />
            <KeyValues title="resource 속성" values={s.resource_attributes} />
            <dl className="mt-kv">
              <dt>span_id</dt>
              <dd className="mt-mono">{s.span_id}</dd>
              <dt>parent_span_id</dt>
              <dd className="mt-mono">{s.parent_span_id ?? '— (root)'}</dd>
              <dt>service_id</dt>
              <dd className="mt-mono">{s.service_id}</dd>
            </dl>
          </>
        )}
        {active === 'events' &&
          (s.events.length === 0 ? (
            <p className="mt-muted">이 span에는 event가 없습니다.</p>
          ) : (
            <ol className="mt-events">
              {s.events.map((ev, i) => (
                <li key={`${ev.name}-${String(i)}`}>
                  <span className="mt-mono">{ev.name}</span> <span className="mt-label">+{formatMilliseconds(nsBetween(s.start_time, ev.time) / 1e6)}</span>
                  <KeyValues values={ev.attributes} />
                </li>
              ))}
            </ol>
          ))}
        {active === 'links' &&
          (s.links.length === 0 ? (
            <p className="mt-muted">이 span에는 link가 없습니다.</p>
          ) : (
            <ul className="mt-events">
              {s.links.map((l) => (
                <li key={`${l.trace_id}-${l.span_id}`}>
                  <span className="mt-mono">
                    trace {l.trace_id} · span {l.span_id}
                  </span>
                  {l.trace_id === s.trace_id && <span className="mt-label"> (같은 trace)</span>}
                  <KeyValues values={l.attributes} />
                </li>
              ))}
            </ul>
          ))}
        {active === 'logs' && <SpanLogs org={org} node={node} traceRange={traceRange} timeZone={timeZone} />}
        {active === 'db' && <DbTab attrs={s.attributes} />}
        {active === 'profile' && (
          <p className="mt-muted">
            profile은 아직 수집하지 않습니다(Phase G1). 수집이 생기면 이 span과 정확히 연결된 profile과 시간만 겹치는 profile을 구분해 보입니다.
          </p>
        )}
      </div>
    </aside>
  );
}

function text(v: unknown): string {
  return typeof v === 'string' ? v : JSON.stringify(v);
}

export function KeyValues({ title, values }: { title?: string; values: Record<string, unknown> }) {
  const entries = Object.entries(values).sort(([a], [b]) => a.localeCompare(b));
  if (entries.length === 0) return title === undefined ? null : <p className="mt-muted">{title} 없음</p>;
  return (
    <>
      {title !== undefined && <h3 className="mt-label mt-kv__title">{title}</h3>}
      <dl className="mt-kv">
        {entries.map(([k, v]) => (
          <div key={k} className="mt-kv__row">
            <dt className="mt-mono">{k}</dt>
            <dd className="mt-mono">{text(v)}</dd>
          </div>
        ))}
      </dl>
    </>
  );
}

const DB_KEYS = ['db.system.name', 'db.system', 'db.namespace', 'db.operation.name', 'db.collection.name', 'db.query.summary', 'db.query.text', 'db.statement', 'server.address'];

function DbTab({ attrs }: { attrs: Record<string, unknown> }) {
  const present = DB_KEYS.filter((k) => k in attrs);
  if (!('db.system.name' in attrs) && !('db.system' in attrs)) {
    return <p className="mt-muted">DB 호출 속성(db.system)이 없는 span입니다.</p>;
  }
  return (
    <>
      <dl className="mt-kv">
        {present.map((k) => (
          <div key={k} className="mt-kv__row">
            <dt className="mt-mono">{k}</dt>
            <dd>{k === 'db.query.text' || k === 'db.statement' ? <pre className="mt-pre">{text(attrs[k])}</pre> : <span className="mt-mono">{text(attrs[k])}</span>}</dd>
          </div>
        ))}
      </dl>
      <p className="mt-label">SQL literal은 수집 단계에서 제거되며 화면에서 복원하지 않습니다. query digest·plan은 Databases(DBM) 화면이 생기면 연결합니다.</p>
    </>
  );
}

function SpanLogs({ org, node, traceRange, timeZone }: { org: string; node: SpanNode; traceRange: { from: string; to: string }; timeZone: string }) {
  const s = node.span;
  const [wide, setWide] = useState(false);
  const [retry, setRetry] = useState(0);
  const startMs = Date.parse(s.start_time);
  const endMs = Date.parse(s.end_time);
  // linked: ID가 정확히 같으면 시각이 어긋나도(비동기·시계 차이) 찾게 trace 조회 범위 전체에서 찾는다
  const linkedBody = {
    range: traceRange,
    filter: {
      op: 'and' as const,
      args: [
        { op: 'eq' as const, field: 'trace_id', value: s.trace_id },
        { op: 'eq' as const, field: 'span_id', value: s.span_id },
      ],
    },
    limit: LOG_LIMIT,
  };
  // related: 기본은 span 시작 ±30초(D02 §07). 넓히면 시작 −30초 ~ 끝 +30초(최대 24시간)
  const relFrom = startMs - RELATED_LOG_WINDOW_MS;
  const relTo = Math.min(wide ? endMs + RELATED_LOG_WINDOW_MS : startMs + RELATED_LOG_WINDOW_MS, relFrom + MAX_LOG_RANGE_MS);
  const relatedBody = {
    range: { from: new Date(relFrom).toISOString(), to: new Date(relTo + 1).toISOString() },
    filter: { op: 'eq' as const, field: 'service_id', value: s.service_id },
    limit: LOG_LIMIT,
  };
  const linked = useRemote(`${org}|logs-linked|${s.span_id}|${traceRange.from}`, retry, (signal) => searchLogs(linkedBody, signal));
  const related = useRemote(`${org}|logs-related|${s.span_id}|${String(wide)}`, retry, (signal) => searchLogs(relatedBody, signal));
  const linkedIds = new Set((linked.data ?? []).map((l) => l.event_id));
  const relatedOnly = (related.data ?? []).filter((l) => !linkedIds.has(l.event_id));
  const onRetry = () => setRetry((n) => n + 1);
  // S08 Logs: 이 trace의 log 전체(trace 조회 범위, trace_id 조건)
  const [params] = useSearchParams();
  const logsSearch = new URLSearchParams(contextOnly(params));
  logsSearch.delete('range');
  logsSearch.set('from', traceRange.from);
  // log 검색 한도(24시간)를 넘는 trace 범위는 앞쪽 24시간으로 자른다
  logsSearch.set('to', new Date(Math.min(Date.parse(traceRange.to), Date.parse(traceRange.from) + MAX_LOG_RANGE_MS)).toISOString());
  logsSearch.set('trace', s.trace_id);
  return (
    <div className="mt-span-logs">
      <Link to={{ pathname: orgPath(org, 'logs'), search: `?${logsSearch.toString()}` }}>Logs에서 이 trace의 log 모두 보기</Link>
      <h3 className="mt-label mt-kv__title">
        <StatusBadge tone="success">linked</StatusBadge> trace·span ID가 같은 log
      </h3>
      <LogList state={linked} rows={linked.data ?? []} empty="이 trace 조회 범위에서 이 span의 ID가 붙은 log가 없습니다." timeZone={timeZone} onRetry={onRetry} />
      <h3 className="mt-label mt-kv__title">
        <StatusBadge tone="neutral">related</StatusBadge> 같은 서비스·{wide ? 'span 시작 30초 전 ~ 끝 30초 뒤' : 'span 시작 앞뒤 30초'} (ID로 연결되지 않음)
      </h3>
      <LogList state={related} rows={relatedOnly} empty="이 구간에 다른 log가 없습니다." timeZone={timeZone} onRetry={onRetry} />
      {!wide && endMs - startMs > RELATED_LOG_WINDOW_MS && (
        <button type="button" className="mt-button" onClick={() => setWide(true)}>
          범위 넓히기: span 끝 30초 뒤까지
        </button>
      )}
    </div>
  );
}

function LogList({
  state,
  rows,
  empty,
  timeZone,
  onRetry,
}: {
  state: ReturnType<typeof useRemote<LogItem[]>>;
  rows: LogItem[];
  empty: string;
  timeZone: string;
  onRetry: () => void;
}) {
  if (state.error !== null) return <ErrorNotice error={state.error} lastSuccessMs={state.lastSuccessMs} timeZone={timeZone} onRetry={onRetry} />;
  if (state.status === 'loading') return <Skeleton height={60} label="log를 불러오는 중" />;
  // 잘렸거나(다음 page가 있음) 일부 저장소가 응답하지 않았으면 "없음"이라 하지 않는다(계약 6)
  const truncated = state.nextCursor !== null || state.meta?.partial === true;
  const note = truncated ? (
    <p className="mt-label" role="note">
      {state.meta?.partial === true ? '일부 저장소가 응답하지 않아 빠진 log가 있을 수 있습니다.' : `가장 최근 ${String(LOG_LIMIT)}개까지만 받았습니다. 이 구간에 log가 더 있습니다.`}
    </p>
  ) : null;
  if (rows.length === 0) return truncated ? note : <p className="mt-muted">{empty}</p>;
  return (
    <>
      {note}
      <ul className="mt-log-list">
        {rows.map((l) => (
          <li key={l.event_id}>
            <span className="mt-mono mt-label">{formatDateTime(l.time, timeZone)}</span>{' '}
            <span className={isErrorSeverity(l.severity_number) ? 'mt-log-sev mt-log-sev--err' : 'mt-log-sev'}>{severityLabel(l.severity_number)}</span>
            <pre className="mt-pre mt-log-body">{l.body}</pre>
          </li>
        ))}
      </ul>
    </>
  );
}
