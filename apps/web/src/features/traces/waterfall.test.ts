import { describe, expect, it } from 'vitest';
import type { TraceSpanItem } from '../../api/types.ts';
import { buildTree, criticalPath, errorSpans, matchSpans, nsBetween, visibleRows } from './waterfall.ts';

const T0 = Date.parse('2026-10-04T05:00:00Z');

function span(id: string, parent: string | null, startMs: number, durMs: number, extra: Partial<TraceSpanItem> = {}): TraceSpanItem {
  const start = new Date(T0 + startMs).toISOString();
  return {
    trace_id: 't'.repeat(32),
    span_id: id,
    parent_span_id: parent,
    service_id: 'svc',
    service_name: 'checkout',
    environment: 'prod',
    name: `op-${id}`,
    kind: 'internal',
    status_code: 'unset',
    status_message: '',
    start_time: start,
    end_time: new Date(T0 + startMs + durMs).toISOString(),
    duration_ns: durMs * 1e6,
    attributes: {},
    resource_attributes: {},
    events: [],
    links: [],
    ...extra,
  };
}

const ids = (rows: ReturnType<typeof visibleRows>) => rows.map((r) => (r.node === null ? '[orphans]' : `${'  '.repeat(r.depth)}${r.node.span.span_id}`));

describe('nsBetween', () => {
  it('RFC3339 nano 차이를 ns 정밀도로(epoch ns는 number로 정확하지 않다)', () => {
    expect(nsBetween('2026-10-04T05:00:00Z', '2026-10-04T05:00:00.000000123Z')).toBe(123);
    expect(nsBetween('2026-10-04T05:00:00Z', '2026-10-04T05:00:00.5Z')).toBe(5e8);
    expect(nsBetween('2026-10-04T05:00:01.000000001Z', '2026-10-04T05:00:00.999999999Z')).toBe(-2);
    // ms 아래 자릿수를 엔진에 맡기지 않는다(반올림하면 1ms가 이중으로 더해진다), offset 형식도 같은 순간
    expect(nsBetween('2026-10-04T05:00:00Z', '2026-10-04T05:00:00.123999999Z')).toBe(123_999_999);
    expect(nsBetween('2026-10-04T05:00:00Z', '2026-10-04T14:00:00.000000007+09:00')).toBe(7);
  });
});

describe('buildTree', () => {
  it('트리·상대 시각·서비스 순서, 부모가 없는 span은 orphan, 자식이 부모 밖이면 skew(시각은 그대로)', () => {
    const t = buildTree([
      span('a', null, 0, 100),
      span('c', 'a', 50, 70, { service_name: 'payment' }), // 부모(0~100) 밖으로 끝남 → skew
      span('b', 'a', 10, 20),
      span('x', 'zzz', 30, 5), // 부모 미수신
    ]);
    expect(t.roots.map((n) => n.span.span_id)).toEqual(['a']);
    expect(t.roots[0]?.children.map((n) => n.span.span_id)).toEqual(['b', 'c']);
    expect(t.orphans.map((n) => n.span.span_id)).toEqual(['x']);
    expect(t.byId.get('c')).toMatchObject({ startNs: 50e6, endNs: 120e6, skew: true });
    expect(t.skewCount).toBe(1);
    expect(t.durationNs).toBe(120e6);
    expect(t.services).toEqual(['checkout', 'payment']);
  });

  it('순환 parent 사슬은 무한 루프 없이 orphan으로 드러낸다', () => {
    const t = buildTree([span('p', 'q', 0, 10), span('q', 'p', 1, 5)]);
    expect(t.roots).toHaveLength(0);
    expect(t.byId.size).toBe(2);
    expect(visibleRows(t, { collapsed: new Set(), only: null }).length).toBeGreaterThanOrEqual(2);
  });
});

describe('criticalPath', () => {
  it('가장 늦게 끝난 자식을 따라가고, 겹친(병렬) 자식은 늦은 쪽만', () => {
    // a(0~100): b(0~60)와 c(10~90)가 병렬, c 안의 d(20~80), c 끝난 뒤 e(92~99)
    const t = buildTree([
      span('a', null, 0, 100),
      span('b', 'a', 0, 60),
      span('c', 'a', 10, 80),
      span('d', 'c', 20, 60),
      span('e', 'a', 92, 7),
    ]);
    expect([...criticalPath(t)].sort()).toEqual(['a', 'c', 'd', 'e']);
  });

  it('겹치지 않는 순차 자식은 모두 경로에 든다', () => {
    const t = buildTree([span('a', null, 0, 100), span('b', 'a', 0, 40), span('c', 'a', 50, 40)]);
    expect([...criticalPath(t)].sort()).toEqual(['a', 'b', 'c']);
  });
});

describe('visibleRows', () => {
  const tree = () =>
    buildTree([
      span('a', null, 0, 100),
      span('b', 'a', 10, 20),
      span('b1', 'b', 12, 5, { status_code: 'error', name: 'SELECT orders' }),
      span('c', 'a', 50, 40),
      span('x', 'zzz', 30, 5),
    ]);

  it('깊이 우선, 그다음 orphan 그룹, 형제 위치·수(aria-posinset·setsize)', () => {
    const rows = visibleRows(tree(), { collapsed: new Set(), only: null });
    expect(ids(rows)).toEqual(['a', '  b', '    b1', '  c', '[orphans]', '  x']);
    expect(rows.map((r) => `${String(r.posInSet)}/${String(r.setSize)}`)).toEqual(['1/2', '1/2', '1/1', '2/2', '2/2', '1/1']);
  });

  it('접으면 자손을 뺀다', () => {
    const rows = visibleRows(tree(), { collapsed: new Set(['b']), only: null });
    expect(ids(rows)).toEqual(['a', '  b', '  c', '[orphans]', '  x']);
    expect(rows[1]).toMatchObject({ hasChildren: true, collapsed: true });
  });

  it('오류만: 오류 span과 그 조상만(맥락 유지)', () => {
    const t = tree();
    expect(ids(visibleRows(t, { collapsed: new Set(), only: errorSpans(t) }))).toEqual(['a', '  b', '    b1']);
  });

  it('검색: 이름·서비스, 대소문자 무시, orphan도 찾는다', () => {
    const t = tree();
    expect(ids(visibleRows(t, { collapsed: new Set(), only: matchSpans(t, 'select') }))).toEqual(['a', '  b', '    b1']);
    expect(ids(visibleRows(t, { collapsed: new Set(), only: matchSpans(t, 'op-x') }))).toEqual(['[orphans]', '  x']);
  });
});
