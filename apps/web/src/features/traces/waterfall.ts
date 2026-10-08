// trace waterfall 계산 (D05 §06·§11, ADR 0044). 화면과 분리한 순수 함수다.
// - 원본 timestamp를 고치지 않는다. 자식이 부모 바깥에 있으면 clock skew로 표시만 한다.
// - 부모가 없는(수신되지 않은) span은 orphan 그룹으로 모은다. 숨기지 않는다.
// - critical path는 "가장 늦게 끝난 자식을 따라 거슬러 가는" 추정이다(Jaeger 방식). duration을 단순 합하지 않는다.
import type { TraceSpanItem } from '../../api/types.ts';

export interface SpanNode {
  span: TraceSpanItem;
  startNs: number; // trace 시작 기준 상대 시각(ns)
  endNs: number;
  children: SpanNode[];
  parent: SpanNode | null;
  /** 부모가 이 trace에 없다(수신되지 않았거나 권한 범위 밖) */
  orphan: boolean;
  /** 부모 구간 밖으로 나간다(clock skew 의심) */
  skew: boolean;
}

export interface TraceTree {
  roots: SpanNode[];
  orphans: SpanNode[];
  byId: Map<string, SpanNode>;
  traceStartMs: number; // 절대 시각(ms)
  durationNs: number;
  services: string[]; // 등장 순서(색 배정)
  skewCount: number;
}

// RFC3339 nano 문자열을 (epoch ms, ms 안의 ns)로 읽는다. epoch ns는 2^53을 넘어 number로 정확히 담을 수 없다.
// 소수부는 떼어 따로 읽는다 — 엔진마다 ms 아래 자릿수를 버리거나 반올림하는 방식이 다를 수 있다. offset(+09:00)도 받는다.
export function parseTime(iso: string): { ms: number; subNs: number } {
  const m = /^(.*T\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})$/.exec(iso);
  if (m === null || m[1] === undefined || m[3] === undefined) return { ms: Date.parse(iso), subNs: 0 };
  const frac = ((m[2] ?? '') + '000000000').slice(0, 9);
  const ms = Date.parse(`${m[1]}${m[3]}`) + Number(frac.slice(0, 3));
  return { ms, subNs: Number(frac.slice(3)) };
}

/** b − a (ns). 두 시각이 104일 이내면 정확하다. */
export function nsBetween(a: string, b: string): number {
  const x = parseTime(a);
  const y = parseTime(b);
  return (y.ms - x.ms) * 1e6 + (y.subNs - x.subNs);
}

const byStart = (a: SpanNode, b: SpanNode) => a.startNs - b.startNs || (a.span.span_id < b.span.span_id ? -1 : 1);

export function buildTree(spans: TraceSpanItem[]): TraceTree {
  // 기준 = 가장 이른 시작
  let first: string | null = null;
  for (const s of spans) if (first === null || nsBetween(s.start_time, first) > 0) first = s.start_time;
  const byId = new Map<string, SpanNode>();
  const services: string[] = [];
  for (const s of spans) {
    const start = first === null ? 0 : nsBetween(first, s.start_time);
    byId.set(s.span_id, { span: s, startNs: start, endNs: start + s.duration_ns, children: [], parent: null, orphan: false, skew: false });
    if (!services.includes(s.service_name)) services.push(s.service_name);
  }
  const roots: SpanNode[] = [];
  const orphans: SpanNode[] = [];
  for (const n of byId.values()) {
    const pid = n.span.parent_span_id;
    if (pid === null) {
      roots.push(n);
      continue;
    }
    const p = byId.get(pid);
    if (p === undefined || p === n) {
      n.orphan = true;
      orphans.push(n);
      continue;
    }
    n.parent = p;
    p.children.push(n);
  }
  // 순환(잘못된 parent 사슬)은 root에서 닿지 않는다 — orphan으로 돌려 숨기지 않는다
  const reached = new Set<SpanNode>();
  const walk = (n: SpanNode) => {
    if (reached.has(n)) return;
    reached.add(n);
    n.children.forEach(walk);
  };
  [...roots, ...orphans].forEach(walk);
  for (const n of byId.values()) {
    if (!reached.has(n)) {
      if (n.parent !== null) n.parent.children = n.parent.children.filter((c) => c !== n);
      n.parent = null;
      n.orphan = true;
      orphans.push(n);
      walk(n);
    }
  }
  let skewCount = 0;
  for (const n of byId.values()) {
    n.children.sort(byStart);
    if (n.parent !== null && (n.startNs < n.parent.startNs || n.endNs > n.parent.endNs)) {
      n.skew = true;
      skewCount++;
    }
  }
  roots.sort(byStart);
  orphans.sort(byStart);
  const durationNs = byId.size === 0 ? 0 : Math.max(...[...byId.values()].map((n) => n.endNs));
  return { roots, orphans, byId, traceStartMs: first === null ? 0 : Date.parse(first), durationNs, services, skewCount };
}

/**
 * critical path 추정(Jaeger 방식): root(여럿이면 가장 늦게 끝난 것)에서 시작해, 현재 경계(처음엔 span 끝)보다 먼저 끝난
 * 자식 중 가장 늦게 끝난 것을 따라간다. 그 자식의 시작이 새 경계다. 경계를 넘어 이어지는(겹치는·병렬) 자식은 경로에 들지 않는다.
 * 부모 밖으로 끝나는 자식(clock skew)은 부모 끝으로 잘라 본다.
 */
export function criticalPath(tree: TraceTree): Set<string> {
  const out = new Set<string>();
  const visit = (n: SpanNode) => {
    out.add(n.span.span_id);
    let t = n.endNs;
    for (;;) {
      let best: SpanNode | null = null;
      let bestEnd = -Infinity;
      for (const c of n.children) {
        const end = Math.min(c.endNs, n.endNs);
        if (out.has(c.span.span_id) || end > t || c.startNs >= t) continue;
        if (end > bestEnd) {
          best = c;
          bestEnd = end;
        }
      }
      if (best === null) return;
      visit(best);
      t = best.startNs;
    }
  };
  const root = [...tree.roots].sort((a, b) => b.endNs - a.endNs)[0];
  if (root !== undefined) visit(root);
  return out;
}

export interface Row {
  node: SpanNode | null; // null = orphan 그룹 머리글
  depth: number;
  hasChildren: boolean;
  collapsed: boolean;
  /** 같은 부모 아래 보이는 형제 중 위치(1부터)와 수 — 가상화로 일부만 그려도 위치를 알린다(aria-posinset·setsize) */
  posInSet: number;
  setSize: number;
}

export interface RowOptions {
  collapsed: ReadonlySet<string>;
  /** 이 span만 보인다(조상은 맥락으로 함께). null이면 전체 */
  only: ReadonlySet<string> | null;
}

/** 보이는 행: root 트리를 깊이 우선으로, 그다음 orphan 그룹. 접힌 span의 자손은 뺀다. */
export function visibleRows(tree: TraceTree, opts: RowOptions): Row[] {
  let keep: Set<string> | null = null;
  if (opts.only !== null) {
    keep = new Set();
    for (const id of opts.only) {
      for (let n = tree.byId.get(id); n !== undefined && n !== null; n = n.parent ?? undefined) keep.add(n.span.span_id);
    }
  }
  const rows: Row[] = [];
  const visible = (ns: SpanNode[]) => (keep === null ? ns : ns.filter((c) => keep.has(c.span.span_id)));
  const add = (n: SpanNode, depth: number, pos: number, size: number) => {
    const kids = visible(n.children);
    const collapsed = opts.collapsed.has(n.span.span_id);
    rows.push({ node: n, depth, hasChildren: kids.length > 0, collapsed, posInSet: pos, setSize: size });
    if (!collapsed) kids.forEach((c, i) => add(c, depth + 1, i + 1, kids.length));
  };
  const roots = visible(tree.roots);
  const orphanRoots = visible(tree.orphans);
  const top = roots.length + (orphanRoots.length > 0 ? 1 : 0);
  roots.forEach((r, i) => add(r, 0, i + 1, top));
  if (orphanRoots.length > 0) {
    rows.push({ node: null, depth: 0, hasChildren: true, collapsed: false, posInSet: top, setSize: top });
    orphanRoots.forEach((o, i) => add(o, 1, i + 1, orphanRoots.length));
  }
  return rows;
}

/** 오류 span id */
export function errorSpans(tree: TraceTree): Set<string> {
  return new Set([...tree.byId.values()].filter((n) => n.span.status_code === 'error').map((n) => n.span.span_id));
}

/** 이름·서비스에 검색어가 들어간 span id (대소문자 무시) */
export function matchSpans(tree: TraceTree, q: string): Set<string> {
  const needle = q.trim().toLowerCase();
  return new Set(
    [...tree.byId.values()]
      .filter((n) => n.span.name.toLowerCase().includes(needle) || n.span.service_name.toLowerCase().includes(needle))
      .map((n) => n.span.span_id),
  );
}
