// S03 Service Map (D05 §07, F03, ADR 0062 데이터·ADR 0063 화면).
// - 원천은 span 쌍 1분 집계다(표본). 수에는 "표본 trace 기반"을 붙이고, SLO가 없어 오류 임계치를 지어내지 않는다.
// - 기본 200 node까지 그리고 나머지는 수를 알린다. 지도와 같은 내용을 표로도 본다(키보드·화면 낭독기, D05 §07).
// - 자동 갱신(1분)은 배치를 다시 계산하지만, 사용자가 옮긴 node 위치는 "배치 초기화"를 누를 때까지 유지한다.
// - 선택한 서비스는 URL `service`에 남는다(공유 링크). 외부 node 선택은 화면 상태다.
import { useEffect, useMemo, useRef, useState } from 'react';
import { Link, useNavigate, useOutletContext, useParams, useSearchParams } from 'react-router';
import { queryServiceMap, type MapEdge, type MapNode, type ServiceMapData, type ServiceMapRequest } from '../../api/serviceMap.ts';
import { useRemote } from '../../api/useRemote.ts';
import { metaWatermark } from '../../api/types.ts';
import { contextOnly, formatRange, resolveRange, type InvestigationContext } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { formatDateTime, formatMilliseconds, formatPercent, formatRate } from '../services/format.ts';
import { EmptyState, ErrorNotice, Skeleton, SourceBadge, StatusBadge } from '../services/states.tsx';
import '../services/services.css';
import { MapCanvas, initialLayoutState, type MapLayoutState } from './MapCanvas.tsx';
import { getServiceHealth } from '../../api/serviceHealth.ts';
import { VerdictBadge, healthVerdicts, type Verdict } from '../services/serviceHealth.tsx';
import { MAX_DRAWN_NODES, aggregatedSeconds, errorRatio, kindText, limitNodes, nodeLabel, requestsPerSecond } from './mapModel.ts';
import './map.css';

/** 서버 조회 상한(D02 §13 metric과 같은 7일, ADR 0062). */
const MAX_RANGE_MS = 7 * 24 * 60 * 60_000;
/** edge rollup은 1분 window다. 더 자주 갱신해도 새 값이 없다. */
const REFRESH_MS = 60_000;

const WARNING_TEXT: Record<string, string> = {
  cross_environment_edges_hidden: '서로 다른 환경의 서비스 사이 호출은 그리지 않았습니다(환경 안에서만 연결).',
  edge_rows_truncated: '호출 종류가 너무 많아 요청이 적은 호출 일부를 읽지 못했습니다. 환경이나 시간 범위를 좁히세요.',
  latency_bounds_mismatch: '지연 분포 경계가 다른 구간이 섞여 일부 호출의 지연시간을 계산하지 않았습니다.',
};

/** 서버 경고(ADR 0062 §4)를 문장으로. 집계 전·잘림·지연 계산 불가·환경 사이 호출 숨김을 알린다(계약 6). S02 의존성도 쓴다. */
export function mapWarningLines(warnings: readonly string[], watermark: string | null, timeZone: string): string[] {
  const lines: string[] = [];
  if (warnings.includes('range_after_watermark')) {
    lines.push(
      watermark === null
        ? '이 범위는 아직 집계 전입니다. 수는 0이 아니라 미계산입니다.'
        : `${formatDateTime(watermark, timeZone)} 이후는 아직 집계 전입니다. 그 구간의 호출은 수에 없습니다(0이 아니라 미계산).`,
    );
  }
  for (const w of warnings) {
    const text = WARNING_TEXT[w];
    if (text !== undefined) lines.push(text);
  }
  return lines;
}

export function edgeKey(e: MapEdge): string {
  return `${e.source}|${e.destination}|${e.connection}|${e.confidence}`;
}

/** 1분 자동 갱신. 숨은 탭에서는 멈추고, 다시 보이면 놓친 갱신을 한 번 한다. 절대 범위면 끈다. */
function useAutoRefresh(enabled: boolean): [number, () => void] {
  const [tick, setTick] = useState(0);
  const missed = useRef(false);
  useEffect(() => {
    if (!enabled) return;
    const id = setInterval(() => {
      if (document.hidden) missed.current = true;
      else setTick((n) => n + 1);
    }, REFRESH_MS);
    const onVisible = () => {
      if (!document.hidden && missed.current) {
        missed.current = false;
        setTick((n) => n + 1);
      }
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      clearInterval(id);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [enabled]);
  return [tick, () => setTick((n) => n + 1)];
}

export function ServiceMap() {
  const { org = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const [params, setParams] = useSearchParams();
  const live = ctx.range.kind === 'relative';
  const [tick, refresh] = useAutoRefresh(live);
  const rangeKey = JSON.stringify(ctx.range);
  const nowMs = useMemo(() => Date.now(), [rangeKey, tick]);
  const { fromMs, toMs } = resolveRange(ctx.range, nowMs);
  const tooLong = toMs - fromMs > MAX_RANGE_MS;
  const req: ServiceMapRequest = {
    range: { from: new Date(fromMs).toISOString(), to: new Date(toMs).toISOString() },
    ...(ctx.environment === null ? {} : { environment: ctx.environment }),
  };
  const state = useRemote(tooLong ? null : `${org}|map|${ctx.environment ?? ''}|${rangeKey}`, tick, (signal) => queryServiceMap(req, signal));
  // node 상태(ADR 0073): 서비스 node만 판정한다. DB·큐·외부는 monitor가 걸리지 않아 "판정 없음"이다
  const health = useRemote(`${org}|service-health|${ctx.environment ?? ''}`, tick, (signal) => getServiceHealth(ctx.environment, signal));
  const verdictOf = healthVerdicts(health);
  const statusOf = (n: MapNode): Verdict | null =>
    verdictOf === null ? null : n.kind !== 'service' ? { kind: 'none' } : verdictOf({ namespace: n.namespace ?? '', name: n.name, environment: n.environment });
  const view = params.get('tab') === 'table' ? 'table' : 'graph';
  const setParam = (key: string, value: string | null) => {
    const next = new URLSearchParams(params);
    if (value === null) next.delete(key);
    else next.set(key, value);
    setParams(next, { replace: true });
  };

  return (
    <section className="mt-page" aria-labelledby="map-title">
      <header className="mt-page__header">
        <h1 id="map-title" className="mt-title">
          Service Map
        </h1>
        <span className="mt-label">
          {ctx.environment === null ? '전체 환경' : `환경 ${ctx.environment}`} · {formatRange(ctx.range, nowMs, ctx.timeZone)}
          {live ? ' · 1분마다 갱신' : ''}
        </span>
        <div className="mt-page__action mt-map-actions" role="group" aria-label="보기">
          <button type="button" className="mt-button" aria-pressed={view === 'graph'} onClick={() => setParam('tab', null)}>
            지도
          </button>
          <button type="button" className="mt-button" aria-pressed={view === 'table'} onClick={() => setParam('tab', 'table')}>
            표
          </button>
          <button type="button" className="mt-button" onClick={refresh} disabled={tooLong}>
            새로고침
          </button>
        </div>
      </header>
      {tooLong && (
        <div className="mt-notice mt-notice--warning" role="alert">
          Service Map은 7일까지 조회할 수 있습니다. 시간 범위를 줄이세요.
        </div>
      )}
      {/* 범위가 너무 길면 조회하지 않으므로 이전 범위의 결과·오류도 보이지 않는다(다른 범위 값을 새 범위처럼 보이지 않게) */}
      {!tooLong && state.error !== null && (
        <ErrorNotice error={state.error} lastSuccessMs={state.lastSuccessMs} timeZone={ctx.timeZone} onRetry={refresh} />
      )}
      {!tooLong && state.status === 'loading' && <Skeleton height={480} label="Service Map을 불러오는 중" />}
      {!tooLong && state.data !== null && (
        <MapBody
          org={org}
          ctx={ctx}
          data={state.data}
          warnings={state.meta?.warnings ?? []}
          watermark={metaWatermark(state.meta)}
          view={view}
          selectedService={params.get('service')}
          onSelectService={(id) => setParam('service', id)}
          search={contextOnly(params)}
          statusOf={statusOf}
        />
      )}
    </section>
  );
}

function MapBody({
  org,
  ctx,
  data,
  warnings,
  watermark,
  view,
  selectedService,
  onSelectService,
  search,
  statusOf,
}: {
  org: string;
  ctx: InvestigationContext;
  data: ServiceMapData;
  warnings: string[];
  watermark: string | null;
  view: 'graph' | 'table';
  selectedService: string | null;
  onSelectService: (id: string | null) => void;
  search: string;
  /** null이면 서비스 상태 API가 없는 배포다(상태 표시 없음) */
  statusOf: (n: MapNode) => Verdict | null;
}) {
  const navigate = useNavigate();
  // 외부 node 선택은 URL에 남기지 않는다(node id 형식이 공유 값 규칙 밖이다)
  const [localSelected, setLocalSelected] = useState<string | null>(null);
  const selectedId = selectedService ?? localSelected;
  // 선택한 node는 요청이 적어도 그린다(공유 링크·S02 link가 그 서비스를 연다)
  const drawn = useMemo(() => limitNodes(data.nodes, data.edges, undefined, selectedId), [data, selectedId]);
  // 옮긴 배치·보기는 자동 갱신과 지도/표 전환에도 남는다(D05 §07, "배치 초기화"로만 되돌림)
  const [layoutState, setLayoutState] = useState<MapLayoutState>(initialLayoutState);
  const rangeSeconds = aggregatedSeconds(data.range, watermark);
  const missingService = selectedService !== null && !data.nodes.some((n) => n.node_id === selectedService);
  const selected = drawn.nodes.find((n) => n.node_id === selectedId) ?? null;
  const select = (n: MapNode | null) => {
    if (n !== null && n.kind === 'service') {
      setLocalSelected(null);
      onSelectService(n.node_id);
    } else {
      onSelectService(null);
      setLocalSelected(n?.node_id ?? null);
    }
  };
  const openService = (n: MapNode) => {
    if (n.kind === 'service') void navigate({ pathname: orgPath(org, `services/${n.node_id}`), search });
  };
  const names = useMemo(() => new Map(data.nodes.map((n) => [n.node_id, nodeLabel(n)])), [data]);

  const notices = (
    <MapNotices
      warnings={warnings}
      watermark={watermark}
      timeZone={ctx.timeZone}
      hidden={drawn.hidden}
      drawnCount={drawn.nodes.length}
      omitted={data.omitted_count}
      environment={ctx.environment}
      missingService={missingService}
      onClearService={() => onSelectService(null)}
    />
  );
  if (data.nodes.length === 0) {
    return (
      <>
        {notices}
        <EmptyState reason="unknown" title="이 범위에 서비스 간 호출이 없습니다">
          서비스 사이 호출은 trace의 client·server span 짝으로 만듭니다. 서비스가 하나뿐이거나, 계측이 다른 서비스 호출을 span으로 남기지 않으면 비어 있습니다.
          {watermark === null ? ' 아직 집계한 구간이 없을 수도 있습니다(수집 후 약 3분).' : ''}
        </EmptyState>
      </>
    );
  }
  return (
    <>
      {notices}
      {view === 'graph' ? (
        <div className={selected === null ? 'mt-map-layout' : 'mt-map-layout mt-map-layout--with-drawer'}>
          <MapCanvas
            nodes={drawn.nodes}
            edges={drawn.edges}
            selectedId={selected?.node_id ?? null}
            onSelect={select}
            onOpen={openService}
            layoutState={layoutState}
            setLayoutState={setLayoutState}
            rangeSeconds={rangeSeconds}
            statusOf={statusOf}
            timeZone={ctx.timeZone}
          />
          {selected !== null && (
            <NodeDrawer
              org={org}
              node={selected}
              edges={data.edges}
              names={names}
              rangeSeconds={rangeSeconds}
              timeZone={ctx.timeZone}
              search={search}
              status={statusOf(selected)}
              onClose={() => select(null)}
            />
          )}
        </div>
      ) : (
        <EdgeTable edges={drawn.edges} names={names} rangeSeconds={rangeSeconds} timeZone={ctx.timeZone} />
      )}
    </>
  );
}

function MapNotices({
  warnings,
  watermark,
  timeZone,
  hidden,
  drawnCount,
  omitted,
  environment,
  missingService,
  onClearService,
}: {
  warnings: string[];
  watermark: string | null;
  timeZone: string;
  hidden: number;
  drawnCount: number;
  omitted: ServiceMapData['omitted_count'];
  environment: string | null;
  missingService: boolean;
  onClearService: () => void;
}) {
  const lines = mapWarningLines(warnings, watermark, timeZone);
  const cut = hidden + omitted.nodes;
  if (cut > 0 || omitted.edges > 0) {
    lines.push(
      `요청이 적은 node ${String(cut)}개${omitted.edges > 0 ? `·호출 ${String(omitted.edges)}개` : ''}를 그리지 않았습니다.` +
        (environment === null ? ' 환경을 고르면 좁혀집니다.' : ''),
    );
  }
  return (
    <div className="mt-map-notices">
      <SourceBadge>표본 trace 기반 · 1분 집계</SourceBadge>
      <span className="mt-label">
        node {drawnCount.toLocaleString('ko-KR')}개 · 최대 {MAX_DRAWN_NODES}
        {hidden + omitted.nodes > 0 ? ` · 그리지 않은 node ${(hidden + omitted.nodes).toLocaleString('ko-KR')}개` : ''}
      </span>
      {missingService && (
        <div className="mt-notice mt-notice--info" role="status">
          고른 서비스는 이 범위·환경의 지도에 없습니다(다른 서비스와 주고받은 호출이 없거나, 볼 수 없는 환경입니다).
          <button type="button" className="mt-button" onClick={onClearService}>
            선택 해제
          </button>
        </div>
      )}
      {lines.map((l) => (
        <div key={l} className="mt-notice mt-notice--warning" role="status">
          {l}
        </div>
      ))}
    </div>
  );
}

function latencyText(e: MapEdge, which: 'p50_ms' | 'p95_ms' | 'p99_ms'): string {
  if (e.confidence === 'estimated') return '—';
  const v = e.latency?.[which] ?? null;
  return v === null ? '없음' : formatMilliseconds(v);
}

function rateText(e: MapEdge, rangeSeconds: number | null): string {
  if (e.requests === null) return '추정 — 수 없음';
  const r = requestsPerSecond(e, rangeSeconds);
  return r === null ? '미계산' : `${formatRate(r)}/s`;
}

function errorText(e: MapEdge): string {
  const r = errorRatio(e);
  if (e.confidence === 'estimated') return '—';
  return r === null ? '없음' : `${formatPercent(r)}%`;
}

function queueWaitText(e: MapEdge): string {
  const v = e.queue_wait?.p95_ms ?? null;
  return v === null ? '—' : formatMilliseconds(v);
}

function ConfidenceBadge({ edge }: { edge: MapEdge }) {
  return edge.confidence === 'estimated' ? <StatusBadge tone="neutral" icon="none" variant="dashed">추정</StatusBadge> : <StatusBadge tone="info">관측</StatusBadge>;
}

/** 지도와 같은 호출을 표로 (D05 §07 "표 보기"). 요청이 많은 순, 추정은 뒤. */
export function EdgeTable({
  edges,
  names,
  rangeSeconds,
  timeZone,
}: {
  edges: readonly MapEdge[];
  names: Map<string, string>;
  rangeSeconds: number | null;
  timeZone: string;
}) {
  const rows = [...edges].sort((a, b) => (b.requests ?? -1) - (a.requests ?? -1) || edgeKey(a).localeCompare(edgeKey(b)));
  return (
    <div className="mt-card mt-card--flush">
      <div className="mt-scroll-x">
        <table className="mt-table" aria-label="서비스 간 호출">
          <thead>
            <tr>
              <th scope="col">호출하는 쪽</th>
              <th scope="col">불리는 쪽</th>
              <th scope="col">연결</th>
              <th scope="col">근거</th>
              <th scope="col">요청/s</th>
              <th scope="col">오류율</th>
              <th scope="col">p50</th>
              <th scope="col">p95</th>
              <th scope="col">p99</th>
              <th scope="col">큐 대기 p95</th>
              <th scope="col">마지막 관측</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((e) => (
              <tr key={edgeKey(e)}>
                <td className="mt-mono">{names.get(e.source) ?? e.source}</td>
                <td className="mt-mono">{names.get(e.destination) ?? e.destination}</td>
                <td className="mt-map-nowrap">{e.connection === 'messaging' ? '메시지' : '요청'}</td>
                <td className="mt-map-nowrap">
                  <ConfidenceBadge edge={e} />
                </td>
                <td className="mt-num mt-map-nowrap">{rateText(e, rangeSeconds)}</td>
                <td className="mt-num">{errorText(e)}</td>
                <td className="mt-num">{latencyText(e, 'p50_ms')}</td>
                <td className="mt-num">{latencyText(e, 'p95_ms')}</td>
                <td className="mt-num">{latencyText(e, 'p99_ms')}</td>
                <td className="mt-num">{queueWaitText(e)}</td>
                <td className="mt-num-left">{formatDateTime(e.last_seen, timeZone)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function NodeDrawer({
  org,
  node,
  edges,
  names,
  rangeSeconds,
  timeZone,
  search,
  status,
  onClose,
}: {
  org: string;
  status: Verdict | null;
  node: MapNode;
  edges: readonly MapEdge[];
  names: Map<string, string>;
  rangeSeconds: number | null;
  timeZone: string;
  search: string;
  onClose: () => void;
}) {
  const incoming = edges.filter((e) => e.destination === node.node_id);
  const outgoing = edges.filter((e) => e.source === node.node_id);
  const traceSearch = useMemo(() => {
    const p = new URLSearchParams(search);
    p.set('service', node.node_id);
    return p.toString();
  }, [search, node.node_id]);
  return (
    <aside
      className="mt-card mt-span-drawer mt-map-drawer"
      aria-labelledby="map-drawer-title"
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          e.preventDefault();
          onClose();
        }
      }}
    >
      <div className="mt-span-drawer__head">
        <div className="mt-span-drawer__title">
          <h2 id="map-drawer-title" className="mt-card__title">
            {nodeLabel(node)}
          </h2>
          <button type="button" className="mt-icon-button" aria-label="선택 해제 (Esc)" onClick={onClose}>
            ✕
          </button>
        </div>
        <div className="mt-svc-meta">
          <span>{kindText(node)}</span>
          <span>환경 {node.environment}</span>
        </div>
        {status !== null && (
          <div>
            <VerdictBadge verdict={status} timeZone={timeZone} />
          </div>
        )}
        {node.kind === 'service' && (
          <div className="mt-map-drawer__links">
            <Link to={{ pathname: orgPath(org, `services/${node.node_id}`), search }}>서비스 상세</Link>
            <Link to={{ pathname: orgPath(org, 'traces'), search: traceSearch }}>이 서비스의 trace</Link>
          </div>
        )}
      </div>
      <EdgeList title="상위 — 이 node를 부름" edges={incoming} other={(e) => names.get(e.source) ?? e.source} rangeSeconds={rangeSeconds} timeZone={timeZone} />
      <EdgeList title="하위 — 이 node가 부름" edges={outgoing} other={(e) => names.get(e.destination) ?? e.destination} rangeSeconds={rangeSeconds} timeZone={timeZone} />
    </aside>
  );
}

function EdgeList({
  title,
  edges,
  other,
  rangeSeconds,
  timeZone,
}: {
  title: string;
  edges: MapEdge[];
  other: (e: MapEdge) => string;
  rangeSeconds: number | null;
  timeZone: string;
}) {
  return (
    <section className="mt-map-drawer__section" aria-label={title}>
      <h3 className="mt-label">
        {title} {edges.length}
      </h3>
      {edges.length === 0 ? (
        <p className="mt-muted">없음</p>
      ) : (
        <ul className="mt-map-edge-list">
          {edges.map((e) => (
            <li key={edgeKey(e)}>
              <span className="mt-mono">{other(e)}</span> <ConfidenceBadge edge={e} />
              <span className="mt-map-edge-list__nums">
                {rateText(e, rangeSeconds)} · 오류 {errorText(e)} · p95 {latencyText(e, 'p95_ms')}
                {e.connection === 'messaging' ? ` · 큐 대기 p95 ${queueWaitText(e)}` : ''}
              </span>
              <span className="mt-muted">마지막 {formatDateTime(e.last_seen, timeZone)}</span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
