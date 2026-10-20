// S03 Map의 순수 계산 (D05 §07, ADR 0063). 그리기와 나눠 단위 시험한다.
// - 배치는 왼쪽(호출하는 쪽) → 오른쪽(불리는 쪽) 층 배치다. 같은 입력이면 같은 좌표다(자동 갱신에 흔들리지 않게).
// - 굵기는 조회 범위 안 요청/s를 그 화면의 최대값으로 정규화한다(범례에 밝힌다). 색은 오류 유무다 — SLO가 없어 임계치를 지어내지 않는다.
// - 추정 edge(confidence=estimated)는 수가 없다(null). 굵기·오류 색을 주지 않고 점선으로 그린다.
import type { MapEdge, MapNode } from '../../api/serviceMap.ts';

/** 기본으로 그리는 node 상한(D05 §07 "기본 node 200개"). 나머지는 수를 알린다. */
export const MAX_DRAWN_NODES = 200;

export const NODE_W = 168;
export const NODE_H = 44;
export const COL_GAP = 96;
export const ROW_GAP = 20;

export interface Point {
  x: number;
  y: number;
}

/** node별 요청 수(들어오고 나가는 관측 edge의 합). 추정 edge는 0으로 센다. */
export function nodeTraffic(edges: readonly MapEdge[]): Map<string, number> {
  const t = new Map<string, number>();
  for (const e of edges) {
    const n = e.requests ?? 0;
    t.set(e.source, (t.get(e.source) ?? 0) + n);
    t.set(e.destination, (t.get(e.destination) ?? 0) + n);
  }
  return t;
}

/**
 * 요청이 많은 node부터 max개를 남기고 그 사이 edge만 남긴다. 숨긴 node 수를 돌려준다.
 * 같은 요청 수는 node_id 순이다(결정적).
 */
export function limitNodes(
  nodes: readonly MapNode[],
  edges: readonly MapEdge[],
  max = MAX_DRAWN_NODES,
): { nodes: MapNode[]; edges: MapEdge[]; hidden: number } {
  if (nodes.length <= max) return { nodes: [...nodes], edges: [...edges], hidden: 0 };
  const traffic = nodeTraffic(edges);
  const ranked = [...nodes].sort((a, b) => (traffic.get(b.node_id) ?? 0) - (traffic.get(a.node_id) ?? 0) || cmp(a.node_id, b.node_id));
  const keep = new Set(ranked.slice(0, max).map((n) => n.node_id));
  return {
    nodes: nodes.filter((n) => keep.has(n.node_id)),
    edges: edges.filter((e) => keep.has(e.source) && keep.has(e.destination)),
    hidden: nodes.length - keep.size,
  };
}

function cmp(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/**
 * 층(column)을 정한다: 들어오는 edge가 없는 node가 0층이고, 각 node는 앞 node들의 가장 깊은 층 + 1이다.
 * 순환은 남은 것 중 들어오는 edge가 가장 적은 node(같으면 id 순)를 먼저 놓아 끊는다. 결정적이다.
 */
export function layers(nodes: readonly MapNode[], edges: readonly MapEdge[]): Map<string, number> {
  const ids = nodes.map((n) => n.node_id).sort(cmp);
  const known = new Set(ids);
  const preds = new Map<string, string[]>(ids.map((id) => [id, []]));
  const succs = new Map<string, string[]>(ids.map((id) => [id, []]));
  for (const e of edges) {
    if (!known.has(e.source) || !known.has(e.destination) || e.source === e.destination) continue;
    preds.get(e.destination)?.push(e.source);
    succs.get(e.source)?.push(e.destination);
  }
  const indeg = new Map(ids.map((id) => [id, new Set(preds.get(id)).size]));
  const layer = new Map<string, number>();
  const placed = new Set<string>();
  while (placed.size < ids.length) {
    let ready = ids.filter((id) => !placed.has(id) && indeg.get(id) === 0);
    if (ready.length === 0) {
      // 순환: 남은 것 중 아직 놓지 않은 앞 node가 가장 적은 하나를 놓는다
      const rest = ids.filter((id) => !placed.has(id));
      rest.sort((a, b) => (indeg.get(a) ?? 0) - (indeg.get(b) ?? 0) || cmp(a, b));
      ready = rest.slice(0, 1);
    }
    for (const id of ready) {
      let l = 0;
      for (const p of preds.get(id) ?? []) if (placed.has(p)) l = Math.max(l, (layer.get(p) ?? 0) + 1);
      layer.set(id, l);
      placed.add(id);
    }
    for (const id of ready) {
      for (const s of new Set(succs.get(id))) if (!placed.has(s)) indeg.set(s, (indeg.get(s) ?? 1) - 1);
    }
  }
  return layer;
}

/**
 * 층 안 순서는 이름순으로 시작해 앞 층 이웃 위치의 평균(barycenter)으로 두 번 고친다. 좌표는 node 왼쪽 위다.
 */
export function layout(nodes: readonly MapNode[], edges: readonly MapEdge[]): Map<string, Point> {
  const layerOf = layers(nodes, edges);
  const byId = new Map(nodes.map((n) => [n.node_id, n]));
  const columns: string[][] = [];
  for (const [id, l] of layerOf) (columns[l] ??= []).push(id);
  const label = (id: string) => byId.get(id)?.name ?? id;
  for (const col of columns) col?.sort((a, b) => cmp(label(a), label(b)) || cmp(a, b));
  const neighbors = new Map<string, string[]>();
  for (const e of edges) {
    (neighbors.get(e.destination) ?? neighbors.set(e.destination, []).get(e.destination))?.push(e.source);
    (neighbors.get(e.source) ?? neighbors.set(e.source, []).get(e.source))?.push(e.destination);
  }
  for (let pass = 0; pass < 2; pass++) {
    const index = new Map<string, number>();
    columns.forEach((col) => col?.forEach((id, i) => index.set(id, i)));
    for (let l = 1; l < columns.length; l++) {
      const col = columns[l];
      if (col === undefined) continue;
      const bary = new Map<string, number>();
      for (const id of col) {
        const prev = (neighbors.get(id) ?? []).filter((n) => layerOf.get(n) === l - 1).map((n) => index.get(n) ?? 0);
        bary.set(id, prev.length === 0 ? (index.get(id) ?? 0) : prev.reduce((s, v) => s + v, 0) / prev.length);
      }
      col.sort((a, b) => (bary.get(a) ?? 0) - (bary.get(b) ?? 0) || cmp(label(a), label(b)) || cmp(a, b));
      col.forEach((id, i) => index.set(id, i));
    }
  }
  const tallest = Math.max(1, ...columns.map((c) => c?.length ?? 0));
  const pos = new Map<string, Point>();
  columns.forEach((col, l) => {
    if (col === undefined) return;
    const offset = ((tallest - col.length) * (NODE_H + ROW_GAP)) / 2;
    col.forEach((id, i) => pos.set(id, { x: l * (NODE_W + COL_GAP), y: offset + i * (NODE_H + ROW_GAP) }));
  });
  return pos;
}

/** 조회 범위 안 초당 요청 수. 추정 edge는 null이다. */
export function requestsPerSecond(e: MapEdge, rangeSeconds: number): number | null {
  if (e.requests === null || rangeSeconds <= 0) return null;
  return e.requests / rangeSeconds;
}

/** 오류율(0~1). 요청이 없거나 추정이면 null이다(0이 아니다, 계약 6). */
export function errorRatio(e: MapEdge): number | null {
  if (e.requests === null || e.errors === null || e.requests === 0) return null;
  return e.errors / e.requests;
}

/** 선 굵기(px): 화면 안 최대 요청 수 대비 제곱근 비율, 1~6px. 추정 edge는 1px. */
export function strokeWidth(e: MapEdge, maxRequests: number): number {
  if (e.requests === null || maxRequests <= 0) return 1;
  return 1 + 5 * Math.sqrt(e.requests / maxRequests);
}

/** node와 바로 붙은 node(위·아래 1hop). */
export function oneHop(id: string, edges: readonly MapEdge[]): Set<string> {
  const out = new Set<string>([id]);
  for (const e of edges) {
    if (e.source === id) out.add(e.destination);
    if (e.destination === id) out.add(e.source);
  }
  return out;
}

/** 노드 간 곡선 경로(SVG d). 오른쪽으로 가면 수평 bezier, 같은 층·뒤로 가면 위로 휘는 호다. */
export function edgePath(from: Point, to: Point): string {
  const sx = from.x + NODE_W;
  const sy = from.y + NODE_H / 2;
  const tx = to.x;
  const ty = to.y + NODE_H / 2;
  if (tx > sx) {
    const dx = (tx - sx) / 2;
    return `M ${sx} ${sy} C ${sx + dx} ${sy}, ${tx - dx} ${ty}, ${tx} ${ty}`;
  }
  // 뒤로 가는 edge(순환): node 위쪽을 돌아간다
  const top = Math.min(from.y, to.y) - 40;
  return `M ${from.x + NODE_W / 2} ${from.y} C ${from.x + NODE_W / 2} ${top}, ${to.x + NODE_W / 2} ${top}, ${to.x + NODE_W / 2} ${to.y}`;
}

/** node 종류 표시(색만으로 구분하지 않는다). */
export const KIND_LABEL: Record<MapNode['kind'], string> = {
  service: '서비스',
  database: 'DB',
  queue: '큐',
  external: '외부',
};

/** 화면 이름: 서비스는 namespace/name, 외부는 이름(host:internal은 "내부 host"). */
export function nodeLabel(n: MapNode): string {
  if (n.kind === 'service') return n.namespace === null || n.namespace === '' ? n.name : `${n.namespace}/${n.name}`;
  if (n.node_id.startsWith('host:internal@')) return '내부 host (이름 숨김)';
  if (n.node_id.startsWith('host:ip@')) return 'IP 주소 (숨김)';
  return n.name;
}

/** 검색어(대소문자 무시)를 이름에 포함하는 node. 빈 검색어는 없음이다. */
export function matchNodes(nodes: readonly MapNode[], q: string): MapNode[] {
  const needle = q.trim().toLowerCase();
  if (needle === '') return [];
  return nodes.filter((n) => nodeLabel(n).toLowerCase().includes(needle));
}
