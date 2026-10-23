// S02 의존성 (D05 §05 "의존성", 디자인 시안 적용 D2-1). service map API(ADR 0062)를 이 서비스로 좁혀 부른다.
// - 숫자는 표본 trace에서 센 호출이다(SDK metric 아님). 항상 "표본" 출처를 붙인다(계약 5·6).
// - 추정 관계(이름만 맞춘 호출)는 수를 보이지 않는다. 오류가 없으면 "없음"이지 0%가 아니다(요청 0이면 계산하지 않음).
import { Link, useSearchParams } from 'react-router';
import { queryServiceMap, type MapEdge, type MapNode, type ServiceMapData } from '../../api/serviceMap.ts';
import { metaWatermark, type ServiceItem } from '../../api/types.ts';
import { useRemote, type RemoteState } from '../../api/useRemote.ts';
import { contextOnly } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { EdgeTable, edgeKey } from '../map/ServiceMap.tsx';
import { KIND_LABEL, aggregatedSeconds, errorRatio, nodeLabel } from '../map/mapModel.ts';
import '../map/map.css';
import { formatMilliseconds, formatPercent } from './format.ts';
import type { MetricWindow } from './red.ts';
import { EmptyState, ErrorNotice, Skeleton, SourceBadge, StatusBadge } from './states.tsx';

export function useDependencies(org: string, svc: ServiceItem, win: MetricWindow, refreshTick: number): RemoteState<ServiceMapData> {
  const req = { range: { from: win.fromIso, to: win.toIso }, service_ids: [svc.service_id] };
  return useRemote(`${org}|${svc.service_id}|deps|${JSON.stringify(req)}`, refreshTick, (signal) => queryServiceMap(req, signal));
}

/** DB·큐는 node_id의 system(`db:postgresql@prod` → postgresql)을 함께 보인다. */
export function kindText(n: MapNode | undefined): string {
  if (n === undefined) return '';
  const m = /^(?:db|queue):([^/@]+)/.exec(n.node_id);
  return m === null ? KIND_LABEL[n.kind] : `${KIND_LABEL[n.kind]} · ${m[1] ?? ''}`;
}

interface Dep {
  edge: MapEdge;
  node: MapNode | undefined;
}

/** 이 서비스가 부르는 쪽(하위)과 이 서비스를 부르는 쪽(상위). 많이 부른 순, 추정은 뒤. */
export function splitDependencies(data: ServiceMapData, serviceId: string): { down: Dep[]; up: Dep[] } {
  const nodes = new Map(data.nodes.map((n) => [n.node_id, n]));
  const order = (a: Dep, b: Dep) => (b.edge.requests ?? -1) - (a.edge.requests ?? -1) || edgeKey(a.edge).localeCompare(edgeKey(b.edge));
  const down = data.edges.filter((e) => e.source === serviceId).map((edge) => ({ edge, node: nodes.get(edge.destination) }));
  const up = data.edges.filter((e) => e.destination === serviceId).map((edge) => ({ edge, node: nodes.get(edge.source) }));
  return { down: down.sort(order), up: up.sort(order) };
}

function DepRow({ dep, nameOf }: { dep: Dep; nameOf: (id: string) => string }) {
  const { edge, node } = dep;
  const id = node?.node_id ?? edge.destination;
  const ratio = errorRatio(edge);
  const p95 = edge.latency?.p95_ms ?? null;
  return (
    <li className="mt-dep">
      <span className="mt-dep__name">
        <span className="mt-mono">{node === undefined ? nameOf(id) : nodeLabel(node)}</span>
        <span className="mt-label">{kindText(node)}</span>
      </span>
      {edge.confidence === 'estimated' ? (
        <span className="mt-dep__est">
          <StatusBadge tone="neutral" icon="none" variant="dashed">
            추정
          </StatusBadge>
          <span className="mt-label">호출 수 미표시</span>
        </span>
      ) : (
        <span className="mt-dep__nums">
          <span className="mt-num">{p95 === null ? <span className="mt-muted">p95 없음</span> : `p95 ${formatMilliseconds(p95)}`}</span>
          <span className="mt-num">{ratio === null ? <span className="mt-muted">오류율 없음</span> : `${formatPercent(ratio)}%`}</span>
        </span>
      )}
    </li>
  );
}

function linkToMap(params: URLSearchParams, serviceId: string): string {
  const q = new URLSearchParams(contextOnly(params));
  q.set('service', serviceId);
  return `?${q.toString()}`;
}

/** 개요 탭 오른쪽 패널: 하위 의존성 상위 6개. */
export function DependencyPanel({ org, svc, deps, timeZone }: { org: string; svc: ServiceItem; deps: RemoteState<ServiceMapData>; timeZone: string }) {
  const [params] = useSearchParams();
  const depsTab = new URLSearchParams(params);
  depsTab.set('tab', 'dependencies');
  const data = deps.data;
  const split = data === null ? null : splitDependencies(data, svc.service_id);
  const names = new Map(data?.nodes.map((n) => [n.node_id, nodeLabel(n)]) ?? []);
  return (
    <section className="mt-card mt-dep-panel" aria-labelledby="deps-title">
      <div className="mt-card__head">
        <h2 id="deps-title" className="mt-card__title">
          하위 의존성
        </h2>
        <Link to={{ pathname: orgPath(org, 'map'), search: linkToMap(params, svc.service_id) }}>Service Map</Link>
      </div>
      <SourceBadge>표본 trace 기반 · 1분 집계</SourceBadge>
      {deps.error !== null && <ErrorNotice error={deps.error} lastSuccessMs={deps.lastSuccessMs} timeZone={timeZone} />}
      {split === null ? (
        deps.error === null && <Skeleton height={120} label="의존성을 불러오는 중" />
      ) : split.down.length === 0 ? (
        <EmptyState inline reason="unknown" title="이 범위에 부른 서비스가 없습니다">
          trace의 client span이 없거나 다른 서비스를 부르지 않았습니다. 0건이라는 뜻이 아닙니다.
        </EmptyState>
      ) : (
        <ul className="mt-dep-list">
          {split.down.slice(0, 6).map((d) => (
            <DepRow key={edgeKey(d.edge)} dep={d} nameOf={(id) => names.get(id) ?? id} />
          ))}
        </ul>
      )}
      {split !== null && (split.down.length > 6 || split.up.length > 0) && (
        <Link to={{ search: `?${depsTab.toString()}` }}>
          모든 의존성 보기 (하위 {split.down.length} · 상위 {split.up.length})
        </Link>
      )}
    </section>
  );
}

/** 의존성 탭: 상위·하위 호출을 Service Map 표와 같은 열로. */
export function DependencyTab({ org, svc, deps, timeZone }: { org: string; svc: ServiceItem; deps: RemoteState<ServiceMapData>; timeZone: string }) {
  const [params] = useSearchParams();
  const data = deps.data;
  if (data === null) {
    return deps.error !== null ? <ErrorNotice error={deps.error} lastSuccessMs={deps.lastSuccessMs} timeZone={timeZone} /> : <Skeleton height={200} label="의존성을 불러오는 중" />;
  }
  const { down, up } = splitDependencies(data, svc.service_id);
  const names = new Map(data.nodes.map((n) => [n.node_id, nodeLabel(n)]));
  const seconds = aggregatedSeconds(data.range, metaWatermark(deps.meta));
  return (
    <div className="mt-dep-tab">
      {deps.error !== null && <ErrorNotice error={deps.error} lastSuccessMs={deps.lastSuccessMs} timeZone={timeZone} />}
      <div className="mt-red__source">
        <SourceBadge>표본 trace 기반 · 1분 집계</SourceBadge>
        <Link to={{ pathname: orgPath(org, 'map'), search: linkToMap(params, svc.service_id) }}>Service Map에서 보기</Link>
      </div>
      {(data.omitted_count.edges > 0 || data.omitted_count.nodes > 0) && (
        <p className="mt-notice mt-notice--info" role="note">
          상한을 넘어 호출 {data.omitted_count.edges}개·node {data.omitted_count.nodes}개를 빼고 보여 줍니다.
        </p>
      )}
      <section aria-labelledby="deps-down-title">
        <h2 id="deps-down-title" className="mt-section-title">
          하위 — 이 서비스가 부름 ({down.length})
        </h2>
        {down.length === 0 ? (
          <EmptyState inline title="이 범위에 부른 서비스가 없습니다" />
        ) : (
          <EdgeTable edges={down.map((d) => d.edge)} names={names} rangeSeconds={seconds} timeZone={timeZone} />
        )}
      </section>
      <section aria-labelledby="deps-up-title">
        <h2 id="deps-up-title" className="mt-section-title">
          상위 — 이 서비스를 부름 ({up.length})
        </h2>
        {up.length === 0 ? (
          <EmptyState inline title="이 범위에 이 서비스를 부른 호출이 없습니다" />
        ) : (
          <EdgeTable edges={up.map((d) => d.edge)} names={names} rangeSeconds={seconds} timeZone={timeZone} />
        )}
      </section>
    </div>
  );
}
