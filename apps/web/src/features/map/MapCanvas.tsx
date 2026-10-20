// S03 지도 canvas (D05 §07 Map 동작).
// - 배경 끌기 = 이동, 버튼·Ctrl/⌘+휠 = 확대·축소, node 끌기 = 위치 옮김(갱신해도 유지, "배치 초기화"로 되돌림).
// - node 클릭·Enter = 선택(위·아래 1hop 강조), 서비스 node 두 번 클릭 = 서비스 상세.
// - 키보드: Tab으로 node 사이를 옮기고, →·← 는 불리는 쪽·호출하는 쪽 이웃, ↑·↓ 는 같은 열의 위·아래 node로 간다.
// - 검색은 이름이 맞는 node를 강조하고 Enter로 첫 node를 고른다. 검색어는 URL에 넣지 않는다.
import { useEffect, useMemo, useRef, useState, type Dispatch, type KeyboardEvent, type PointerEvent, type SetStateAction } from 'react';
import type { MapEdge, MapNode } from '../../api/serviceMap.ts';
import {
  KIND_LABEL,
  NODE_H,
  NODE_W,
  edgePath,
  errorRatio,
  layout,
  matchNodes,
  nodeLabel,
  oneHop,
  strokeWidth,
  type Point,
} from './mapModel.ts';

const MIN_K = 0.25;
const MAX_K = 2.5;
const DRAG_THRESHOLD = 4;
const PAD = 24;

export interface View {
  x: number;
  y: number;
  k: number;
}

/**
 * 사용자가 바꾼 배치(옮긴 node 위치)와 보기(이동·확대). 화면(ServiceMap)이 들고 있어 자동 갱신·표 보기 전환에도
 * 유지되고, "배치 초기화"로만 되돌린다(D05 §07). view가 null이면 처음 그릴 때 화면에 맞춘다.
 */
export interface MapLayoutState {
  moved: Map<string, Point>;
  view: View | null;
}

export const initialLayoutState = (): MapLayoutState => ({ moved: new Map(), view: null });

type Drag =
  | { kind: 'pan'; startX: number; startY: number; view: View; moved: boolean }
  | { kind: 'node'; id: string; startX: number; startY: number; origin: Point; moved: boolean };

function clampK(k: number): number {
  return Math.min(MAX_K, Math.max(MIN_K, k));
}

function edgeId(e: MapEdge): string {
  return `${e.source}|${e.destination}|${e.connection}|${e.confidence}`;
}

export function MapCanvas({
  nodes,
  edges,
  selectedId,
  onSelect,
  onOpen,
  layoutState,
  setLayoutState,
}: {
  nodes: readonly MapNode[];
  edges: readonly MapEdge[];
  selectedId: string | null;
  onSelect: (n: MapNode | null) => void;
  onOpen: (n: MapNode) => void;
  layoutState: MapLayoutState;
  setLayoutState: Dispatch<SetStateAction<MapLayoutState>>;
}) {
  const base = useMemo(() => layout(nodes, edges), [nodes, edges]);
  const moved = layoutState.moved;
  const view = layoutState.view ?? { x: PAD, y: PAD, k: 1 };
  const setMoved = (f: (m: Map<string, Point>) => Map<string, Point>) => setLayoutState((s) => ({ ...s, moved: f(s.moved) }));
  const setView = (v: View | ((v: View) => View)) =>
    setLayoutState((s) => ({ ...s, view: typeof v === 'function' ? v(s.view ?? { x: PAD, y: PAD, k: 1 }) : v }));
  const [query, setQuery] = useState('');
  const drag = useRef<Drag | null>(null);
  const svgRef = useRef<SVGSVGElement>(null);
  const nodeRefs = useRef(new Map<string, SVGGElement>());
  const pos = (id: string): Point => moved.get(id) ?? base.get(id) ?? { x: 0, y: 0 };
  const byId = useMemo(() => new Map(nodes.map((n) => [n.node_id, n])), [nodes]);
  const maxRequests = useMemo(() => Math.max(0, ...edges.map((e) => e.requests ?? 0)), [edges]);
  const focusSet = selectedId === null ? null : oneHop(selectedId, edges);
  const matches = useMemo(() => new Set(matchNodes(nodes, query).map((n) => n.node_id)), [nodes, query]);

  const focusNode = (id: string | undefined) => {
    if (id !== undefined) nodeRefs.current.get(id)?.focus();
  };
  // 선택을 닫으면(drawer의 Esc·✕) focus가 사라지지 않게 그 node로 돌려준다
  const lastSelected = useRef<string | null>(selectedId);
  useEffect(() => {
    const prev = lastSelected.current;
    lastSelected.current = selectedId;
    if (selectedId === null && prev !== null && (document.activeElement === null || document.activeElement === document.body)) {
      nodeRefs.current.get(prev)?.focus();
    }
  }, [selectedId]);

  const onPointerDown = (e: PointerEvent<SVGSVGElement>) => {
    if (e.button !== 0) return;
    const target = (e.target as Element).closest('[data-node-id]');
    const id = target?.getAttribute('data-node-id') ?? null;
    drag.current =
      id === null
        ? { kind: 'pan', startX: e.clientX, startY: e.clientY, view, moved: false }
        : { kind: 'node', id, startX: e.clientX, startY: e.clientY, origin: pos(id), moved: false };
  };
  const onPointerMove = (e: PointerEvent<SVGSVGElement>) => {
    const d = drag.current;
    if (d === null) return;
    const dx = e.clientX - d.startX;
    const dy = e.clientY - d.startY;
    if (!d.moved && Math.hypot(dx, dy) < DRAG_THRESHOLD) return;
    if (!d.moved) {
      // 끌기가 시작된 뒤에만 capture한다. 누르자마자 잡으면 click·dblclick 대상이 svg가 되어
      // node 두 번 클릭(서비스 상세)이 오지 않는다(브라우저에서 확인).
      e.currentTarget.setPointerCapture?.(e.pointerId);
    }
    d.moved = true;
    if (d.kind === 'pan') {
      setView({ ...d.view, x: d.view.x + dx, y: d.view.y + dy });
    } else {
      const next = { x: d.origin.x + dx / view.k, y: d.origin.y + dy / view.k };
      setMoved((m) => new Map(m).set(d.id, next));
    }
  };
  const onPointerUp = () => {
    const d = drag.current;
    drag.current = null;
    if (d === null || d.moved) return;
    if (d.kind === 'node') onSelect(byId.get(d.id) ?? null);
    else onSelect(null);
  };
  const zoomBy = (f: number) => setView((v) => ({ ...v, k: clampK(v.k * f) }));
  // 그냥 휠은 화면 스크롤이다. Ctrl/⌘ + 휠만 확대·축소한다. React의 onWheel은 passive라 preventDefault가
  // 듣지 않아 브라우저 확대도 함께 일어난다 — native listener를 passive:false로 단다.
  useEffect(() => {
    const el = svgRef.current;
    if (el === null) return;
    const onWheel = (e: globalThis.WheelEvent) => {
      if (!e.ctrlKey && !e.metaKey) return;
      e.preventDefault();
      const f = e.deltaY < 0 ? 1.15 : 1 / 1.15;
      setLayoutState((s) => {
        const v = s.view ?? { x: PAD, y: PAD, k: 1 };
        return { ...s, view: { ...v, k: clampK(v.k * f) } };
      });
    };
    el.addEventListener('wheel', onWheel, { passive: false });
    return () => el.removeEventListener('wheel', onWheel);
  }, [setLayoutState]);
  // 화면에 맞춤: 그림 전체가 보이게 하되 1배보다 키우지 않는다(글자가 원래 크기보다 커지지 않게)
  const fit = () => {
    const el = svgRef.current;
    if (el === null || el.clientWidth === 0) return;
    const all = [...base.values()];
    const w = Math.max(...all.map((p) => p.x), 0) + NODE_W + PAD * 2;
    const h = Math.max(...all.map((p) => p.y), 0) + NODE_H + PAD * 2;
    const k = clampK(Math.min(1, el.clientWidth / w, el.clientHeight / h));
    setView({ k, x: Math.max(PAD, (el.clientWidth - (w - PAD * 2) * k) / 2), y: Math.max(PAD, (el.clientHeight - (h - PAD * 2) * k) / 2) });
  };
  // 보기가 아직 없을 때(처음) 한 번만 맞춘다. 자동 갱신·node 이동·표 보기 전환은 사용자의 보기를 바꾸지 않는다(D05 §07)
  useEffect(() => {
    if (layoutState.view === null) fit();
  }, []);
  const reset = () => {
    setMoved(() => new Map());
    fit();
  };

  const onNodeKey = (e: KeyboardEvent<SVGGElement>, n: MapNode) => {
    const here = pos(n.node_id);
    const sameColumn = nodes.filter((m) => pos(m.node_id).x === here.x).sort((a, b) => pos(a.node_id).y - pos(b.node_id).y);
    const idx = sameColumn.findIndex((m) => m.node_id === n.node_id);
    switch (e.key) {
      case 'Enter':
      case ' ':
        e.preventDefault();
        onSelect(n);
        return;
      case 'Escape':
        e.preventDefault();
        onSelect(null);
        return;
      case 'ArrowRight':
        e.preventDefault();
        focusNode(edges.find((x) => x.source === n.node_id && byId.has(x.destination))?.destination);
        return;
      case 'ArrowLeft':
        e.preventDefault();
        focusNode(edges.find((x) => x.destination === n.node_id && byId.has(x.source))?.source);
        return;
      case 'ArrowDown':
        e.preventDefault();
        focusNode(sameColumn[idx + 1]?.node_id);
        return;
      case 'ArrowUp':
        e.preventDefault();
        focusNode(sameColumn[idx - 1]?.node_id);
        return;
      default:
    }
  };

  // 화면 낭독기 순서: 열(층) → 위에서 아래
  const ordered = [...nodes].sort((a, b) => pos(a.node_id).x - pos(b.node_id).x || pos(a.node_id).y - pos(b.node_id).y);

  return (
    <div className="mt-card mt-map-canvas">
      <div className="mt-map-toolbar">
        <label className="mt-map-search">
          <span className="mt-label">node 찾기</span>
          <input
            className="mt-input"
            type="search"
            value={query}
            placeholder="서비스·DB 이름"
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => {
              if (e.key !== 'Enter') return;
              const first = ordered.find((n) => matches.has(n.node_id));
              if (first !== undefined) {
                onSelect(first);
                focusNode(first.node_id);
              }
            }}
          />
        </label>
        {query.trim() !== '' && (
          <span className="mt-label" role="status">
            {matches.size === 0 ? '맞는 node 없음' : `${String(matches.size)}개 맞음`}
          </span>
        )}
        <div className="mt-map-toolbar__zoom" role="group" aria-label="확대·축소">
          <button type="button" className="mt-icon-button" aria-label="확대" onClick={() => zoomBy(1.25)}>
            +
          </button>
          <button type="button" className="mt-icon-button" aria-label="축소" onClick={() => zoomBy(1 / 1.25)}>
            −
          </button>
          <button type="button" className="mt-button" onClick={fit}>
            화면에 맞춤
          </button>
          <button type="button" className="mt-button" onClick={reset}>
            배치 초기화
          </button>
        </div>
      </div>
      <svg
        className="mt-map-svg"
        role="group"
        aria-label={`서비스 지도: node ${String(nodes.length)}개, 호출 ${String(edges.length)}개`}
        ref={svgRef}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={() => (drag.current = null)}
      >
        <defs>
          {/* 화살표 크기는 선 굵기와 무관하게 고정한다(굵은 선에서 화살표가 커지지 않게) */}
          <marker
            id="mt-map-arrow"
            viewBox="0 0 10 10"
            refX="10"
            refY="5"
            markerWidth="9"
            markerHeight="9"
            markerUnits="userSpaceOnUse"
            orient="auto-start-reverse"
          >
            <path d="M 0 0 L 10 5 L 0 10 z" className="mt-map-arrow" />
          </marker>
        </defs>
        <g transform={`translate(${String(view.x)} ${String(view.y)}) scale(${String(view.k)})`}>
          {edges.map((e) => {
            const s = byId.get(e.source);
            const d = byId.get(e.destination);
            if (s === undefined || d === undefined) return null;
            const dim = focusSet !== null && !(e.source === selectedId || e.destination === selectedId);
            const errored = (errorRatio(e) ?? 0) > 0;
            const cls = [
              'mt-map-edge',
              e.confidence === 'estimated' ? 'mt-map-edge--estimated' : '',
              errored ? 'mt-map-edge--error' : '',
              dim ? 'mt-map-dim' : '',
            ]
              .filter((c) => c !== '')
              .join(' ');
            return (
              <path
                key={edgeId(e)}
                className={cls}
                data-edge={edgeId(e)}
                d={edgePath(pos(e.source), pos(e.destination))}
                strokeWidth={strokeWidth(e, maxRequests)}
                markerEnd="url(#mt-map-arrow)"
              >
                <title>{`${nodeLabel(s)} → ${nodeLabel(d)}`}</title>
              </path>
            );
          })}
          {ordered.map((n) => {
            const p = pos(n.node_id);
            const dim = focusSet !== null && !focusSet.has(n.node_id);
            const cls = [
              'mt-map-node',
              `mt-map-node--${n.kind}`,
              n.node_id === selectedId ? 'mt-map-node--selected' : '',
              matches.has(n.node_id) ? 'mt-map-node--match' : '',
              dim ? 'mt-map-dim' : '',
            ]
              .filter((c) => c !== '')
              .join(' ');
            const label = nodeLabel(n);
            return (
              <g
                key={n.node_id}
                ref={(el) => {
                  if (el === null) nodeRefs.current.delete(n.node_id);
                  else nodeRefs.current.set(n.node_id, el);
                }}
                className={cls}
                data-node-id={n.node_id}
                transform={`translate(${String(p.x)} ${String(p.y)})`}
                tabIndex={0}
                role="button"
                aria-pressed={n.node_id === selectedId}
                aria-label={`${label}, ${KIND_LABEL[n.kind]}`}
                onKeyDown={(e) => onNodeKey(e, n)}
                onDoubleClick={() => onOpen(n)}
              >
                <rect width={NODE_W} height={NODE_H} rx={6} />
                <text className="mt-map-node__kind" x={10} y={17}>
                  {KIND_LABEL[n.kind]}
                </text>
                <text className="mt-map-node__name" x={10} y={34}>
                  {label.length > 22 ? `${label.slice(0, 21)}…` : label}
                </text>
                <title>{label}</title>
              </g>
            );
          })}
        </g>
      </svg>
      <MapLegend />
    </div>
  );
}

function MapLegend() {
  return (
    <div className="mt-map-legend" aria-label="범례">
      <span>
        <svg width="36" height="10" aria-hidden="true">
          <line x1="0" y1="5" x2="36" y2="5" className="mt-map-edge" strokeWidth={4} />
        </svg>
        굵기 = 요청량(이 화면의 최대값 기준 정규화)
      </span>
      <span>
        <svg width="36" height="10" aria-hidden="true">
          <line x1="0" y1="5" x2="36" y2="5" className="mt-map-edge mt-map-edge--error" strokeWidth={2} />
        </svg>
        오류가 있는 호출
      </span>
      <span>
        <svg width="36" height="10" aria-hidden="true">
          <line x1="0" y1="5" x2="36" y2="5" className="mt-map-edge mt-map-edge--estimated" strokeWidth={1} />
        </svg>
        추정(이름만 맞춤, 수 없음)
      </span>
    </div>
  );
}
