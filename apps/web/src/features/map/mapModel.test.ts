import { describe, expect, it } from 'vitest';
import type { MapEdge, MapNode } from '../../api/serviceMap.ts';
import { errorRatio, layers, layout, limitNodes, matchNodes, nodeLabel, oneHop, requestsPerSecond, strokeWidth } from './mapModel.ts';

const svc = (id: string, name = id): MapNode => ({ node_id: id, kind: 'service', name, service_id: id, namespace: '', environment: 'prod' });
const ext = (id: string, name: string, kind: MapNode['kind'] = 'database'): MapNode => ({
  node_id: id,
  kind,
  name,
  service_id: null,
  namespace: null,
  environment: 'prod',
});
const edge = (source: string, destination: string, requests: number | null = 10, errors: number | null = 0): MapEdge => ({
  source,
  destination,
  connection: 'rpc',
  confidence: requests === null ? 'estimated' : 'observed',
  requests,
  errors: requests === null ? null : errors,
  latency: null,
  queue_wait: null,
  last_seen: '2026-10-04T05:59:00Z',
});

describe('layers', () => {
  it('호출하는 쪽이 앞 층, 가장 깊은 경로를 따른다', () => {
    const nodes = [svc('a'), svc('b'), svc('c'), ext('db', 'postgresql')];
    const l = layers(nodes, [edge('a', 'b'), edge('b', 'c'), edge('a', 'c'), edge('c', 'db')]);
    expect(Object.fromEntries(l)).toEqual({ a: 0, b: 1, c: 2, db: 3 });
  });

  it('순환이 있어도 모든 node에 층을 주고 결정적이다', () => {
    const nodes = [svc('x'), svc('y'), svc('z')];
    const edges = [edge('x', 'y'), edge('y', 'z'), edge('z', 'x')];
    const a = Object.fromEntries(layers(nodes, edges));
    const b = Object.fromEntries(layers([...nodes].reverse(), [...edges].reverse()));
    expect(Object.keys(a).sort()).toEqual(['x', 'y', 'z']);
    expect(a).toEqual(b);
  });
});

describe('layout', () => {
  it('같은 입력이면 같은 좌표(자동 갱신에 흔들리지 않음), 층마다 x가 오른쪽으로', () => {
    const nodes = [svc('a', 'checkout'), svc('b', 'payment'), ext('db', 'postgresql')];
    const edges = [edge('a', 'b'), edge('b', 'db')];
    const p1 = layout(nodes, edges);
    const p2 = layout([...nodes].reverse(), [...edges].reverse());
    expect([...p1.entries()].sort()).toEqual([...p2.entries()].sort());
    expect(p1.get('a')!.x).toBeLessThan(p1.get('b')!.x);
    expect(p1.get('b')!.x).toBeLessThan(p1.get('db')!.x);
  });

  it('층 안 순서는 앞 층 이웃 위치를 따른다(선이 덜 꼬이게)', () => {
    const nodes = [svc('r1', 'a-root'), svc('r2', 'b-root'), svc('c1', 'z-child-of-a'), svc('c2', 'a-child-of-b')];
    const p = layout(nodes, [edge('r1', 'c1'), edge('r2', 'c2')]);
    expect(p.get('c1')!.y).toBeLessThan(p.get('c2')!.y);
  });
});

describe('limitNodes', () => {
  it('요청이 많은 node부터 남기고 남은 node 사이 edge만 둔다', () => {
    const nodes = [svc('a'), svc('b'), svc('c'), svc('d')];
    const edges = [edge('a', 'b', 100), edge('a', 'c', 5), edge('c', 'd', 1)];
    const r = limitNodes(nodes, edges, 3);
    expect(r.nodes.map((n) => n.node_id).sort()).toEqual(['a', 'b', 'c']);
    expect(r.edges).toHaveLength(2);
    expect(r.hidden).toBe(1);
  });

  it('추정 edge는 요청 수 0으로 센다', () => {
    const r = limitNodes([svc('a'), svc('b'), svc('c')], [edge('a', 'b', null), edge('a', 'c', 1)], 2);
    expect(r.nodes.map((n) => n.node_id).sort()).toEqual(['a', 'c']);
  });
});

describe('값', () => {
  it('추정 edge는 요청/s·오류율이 없다(0이 아님)', () => {
    expect(requestsPerSecond(edge('a', 'b', null), 60)).toBeNull();
    expect(errorRatio(edge('a', 'b', null))).toBeNull();
    expect(errorRatio(edge('a', 'b', 0))).toBeNull();
    expect(errorRatio(edge('a', 'b', 50, 5))).toBe(0.1);
    expect(requestsPerSecond(edge('a', 'b', 120), 60)).toBe(2);
  });

  it('굵기는 화면 최대값 기준 1~6px, 추정은 1px', () => {
    expect(strokeWidth(edge('a', 'b', 100), 100)).toBe(6);
    expect(strokeWidth(edge('a', 'b', 25), 100)).toBe(3.5);
    expect(strokeWidth(edge('a', 'b', null), 100)).toBe(1);
  });

  it('1hop은 자기와 위·아래 이웃', () => {
    expect([...oneHop('b', [edge('a', 'b'), edge('b', 'c'), edge('c', 'd')])].sort()).toEqual(['a', 'b', 'c']);
  });

  it('이름: namespace/name, 숨긴 host는 이름 대신 설명', () => {
    expect(nodeLabel({ ...svc('a', 'checkout'), namespace: 'shop' })).toBe('shop/checkout');
    expect(nodeLabel(ext('host:internal@prod', 'internal', 'external'))).toBe('내부 host (이름 숨김)');
    expect(nodeLabel(ext('host:ip@prod', 'ip', 'external'))).toBe('IP 주소 (숨김)');
    expect(matchNodes([svc('a', 'Checkout'), svc('b', 'payment')], 'CHECK').map((n) => n.node_id)).toEqual(['a']);
    expect(matchNodes([svc('a')], '  ')).toEqual([]);
  });
});
