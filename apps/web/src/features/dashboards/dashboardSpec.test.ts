import { afterEach, describe, expect, it, vi } from 'vitest';
import type { DashboardSpec, Widget } from '../../api/dashboards.ts';
import {
  changeKind,
  clearDraft,
  emptySpec,
  loadDraft,
  newWidget,
  newWidgetId,
  nextPlacement,
  placementProblem,
  readingOrder,
  resolveFilter,
  resolveQuery,
  saveDraft,
  specDiff,
  specProblems,
  variableValues,
  variableValuesChecked,
  widgetRange,
  widgetStateKey,
  widgetVariables,
} from './dashboardSpec.ts';

const ts: Widget = {
  ...newWidget('w1', 'time_series'),
  title: '요청량',
  query: {
    metric: 'http.server.request.duration',
    aggregation: 'rate',
    filter: {
      op: 'and',
      args: [
        { op: 'eq', field: 'service.name', value: '$svc' },
        { op: 'eq', field: 'deployment.environment.name', value: '$env' },
        { op: 'eq', field: 'http.route', value: '/pay' },
      ],
    },
    group_by: ['http.route'],
  },
};

const T0 = Date.parse('2026-10-14T00:00:00Z');

describe('변수', () => {
  it('URL 값(빈 값 = 전체)이 기본값보다 앞선다', () => {
    const vars = [
      { name: 'svc', kind: 'service' as const, default: 'checkout' },
      { name: 'env', kind: 'environment' as const, default: 'prod' },
      { name: 'other', kind: 'service' as const, default: null },
    ];
    expect(variableValues(vars, new URLSearchParams('var-env=&var-svc=cart'))).toEqual({ svc: 'cart', env: null, other: null });
    expect(variableValues(vars, new URLSearchParams(''))).toEqual({ svc: 'checkout', env: 'prod', other: null });
  });

  it('"전체" 변수의 조건은 빼고(빈 값으로 거르지 않는다), 나머지는 값으로 바꾼다', () => {
    expect(resolveFilter(ts.query?.filter ?? null, { svc: 'cart', env: null })).toEqual({
      op: 'and',
      args: [
        { op: 'eq', field: 'service.name', value: 'cart' },
        { op: 'eq', field: 'http.route', value: '/pay' },
      ],
    });
    expect(resolveFilter({ op: 'eq', field: 'service.name', value: '$svc' }, { svc: null })).toBeUndefined();
    expect(widgetVariables(ts)).toEqual(['svc', 'env']);
  });
});

describe('조회', () => {
  it('시계열은 범위에 맞는 step, 요약 위젯은 범위 전체 한 점(분 단위로 맞춤)', () => {
    const q = resolveQuery(ts, { svc: null, env: null }, T0 - 3600_000 + 30_000, T0 - 10_000);
    expect(q?.range).toEqual({ from: '2026-10-13T23:00:00.000Z', to: '2026-10-14T00:00:00.000Z' });
    expect(q?.step_seconds).toBe(60);
    const stat: Widget = { ...newWidget('w2', 'stat'), query: { metric: 'm', aggregation: 'p95', filter: null, group_by: [] } };
    expect(resolveQuery(stat, {}, T0 - 3600_000, T0)?.step_seconds).toBe(3600);
    expect(resolveQuery(stat, {}, T0 - 8 * 86400_000, T0)).toBeNull(); // 7일 초과
    expect(resolveQuery(newWidget('t', 'text'), {}, T0 - 3600_000, T0)).toBeNull();
  });

  it('시간 덮어쓰기는 지금부터 그만큼', () => {
    expect(widgetRange({ ...ts, time_override: { range: '4h' } }, 0, 1, T0)).toEqual({ fromMs: T0 - 4 * 3600_000, toMs: T0 });
    expect(widgetRange(ts, 5, 9, T0)).toEqual({ fromMs: 5, toMs: 9 });
  });
});

describe('배치', () => {
  const layout = [
    { widget_id: 'a', x: 0, y: 0, w: 12, h: 6 },
    { widget_id: 'b', x: 12, y: 0, w: 12, h: 3 },
  ];
  it('24열 안·겹침 없음', () => {
    expect(placementProblem({ widget_id: 'c', x: 0, y: 6, w: 24, h: 2 }, layout)).toBeNull();
    expect(placementProblem({ widget_id: 'c', x: 20, y: 6, w: 6, h: 2 }, layout)).toMatch(/오른쪽 끝/);
    expect(placementProblem({ widget_id: 'c', x: 10, y: 2, w: 4, h: 2 }, layout)).toMatch(/겹칩니다/);
    expect(placementProblem({ widget_id: 'a', x: 0, y: 0, w: 12, h: 7 }, layout)).toBeNull(); // 자기 자신과는 겹치지 않는다
    expect(placementProblem({ widget_id: 'c', x: 0, y: 6, w: 0, h: 2 }, layout)).not.toBeNull();
    expect(placementProblem({ widget_id: 'c', x: 0, y: 6, w: 2, h: 49 }, layout)).toMatch(/높이/);
  });
  it('새 위젯은 맨 아래, 읽는 순서는 위→아래·왼→오른쪽', () => {
    expect(nextPlacement(layout, 'c')).toEqual({ widget_id: 'c', x: 0, y: 6, w: 12, h: 6 });
    expect(readingOrder([layout[1]!, { widget_id: 'z', x: 0, y: 9, w: 1, h: 1 }, layout[0]!]).map((p) => p.widget_id)).toEqual(['a', 'b', 'z']);
    expect(newWidgetId([newWidget('w1', 'text'), newWidget('w2', 'text')])).toBe('w3');
  });
  it('종류를 바꾸면 쓰지 않는 표시 설정은 빠지고 query는 남는다', () => {
    const top = changeKind(ts, 'top_list');
    expect(top.display).toEqual({ decimals: 2, limit: 10, sort: 'desc' });
    expect(top.query?.metric).toBe('http.server.request.duration');
    expect(changeKind(ts, 'text')).toMatchObject({ query: null, text: '', unit: null });
  });
});

describe('충돌 비교', () => {
  it('위젯은 id로 맞춰 추가·변경·삭제와 위치 변경을 적는다', () => {
    const base: DashboardSpec = { ...emptySpec('A'), widgets: [ts, newWidget('w9', 'text')], layout: [{ widget_id: 'w1', x: 0, y: 0, w: 12, h: 6 }, { widget_id: 'w9', x: 12, y: 0, w: 12, h: 6 }] };
    const next: DashboardSpec = {
      ...base,
      title: 'B',
      widgets: [{ ...ts, title: '바뀜' }, newWidget('w3', 'stat')],
      layout: [{ widget_id: 'w1', x: 0, y: 0, w: 24, h: 6 }, { widget_id: 'w3', x: 0, y: 6, w: 6, h: 3 }],
    };
    expect(specDiff(base, next)).toEqual(['제목: “A” → “B”', '위젯 변경: “바뀜”', '위젯 추가: 숫자 하나 w3', '위젯 삭제: 글 w9', '위젯 1개의 위치·크기']);
    expect(specDiff(base, base)).toEqual([]);
  });
});

describe('로컬 초안', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    clearDraft('acme', 'd1');
  });
  it('저장·복원·삭제하고, 저장소를 못 쓰면 초안 없이 동작한다', () => {
    saveDraft('acme', 'd1', { baseRevision: 3, baseSpec: emptySpec('기준'), spec: emptySpec('초안'), savedAt: 5 });
    expect(loadDraft('acme', 'd1')?.spec.title).toBe('초안');
    expect(loadDraft('acme', 'd1')?.baseSpec?.title).toBe('기준');
    expect(loadDraft('other', 'd1')).toBeNull();
    clearDraft('acme', 'd1');
    expect(loadDraft('acme', 'd1')).toBeNull();
    vi.stubGlobal('localStorage', {
      getItem: () => {
        throw new Error('blocked');
      },
      setItem: () => {
        throw new Error('blocked');
      },
      removeItem: () => undefined,
    });
    expect(() => saveDraft('acme', 'd1', { baseRevision: null, baseSpec: null, spec: emptySpec(), savedAt: 1 })).not.toThrow();
    expect(loadDraft('acme', 'd1')).toBeNull();
  });
});

describe('리뷰 반영', () => {
  it('상단 환경은 위젯이 환경을 직접 거르지 않을 때만 더한다(환경 변수가 "전체"면 상단을 따른다)', () => {
    const f = ts.query?.filter ?? null;
    expect(resolveFilter(f, { svc: null, env: 'staging' }, 'prod')).toEqual({
      op: 'and',
      args: [
        { op: 'eq', field: 'deployment.environment.name', value: 'staging' },
        { op: 'eq', field: 'http.route', value: '/pay' },
      ],
    });
    expect(resolveFilter(f, { svc: null, env: null }, 'prod')).toEqual({
      op: 'and',
      args: [
        { op: 'eq', field: 'http.route', value: '/pay' },
        { op: 'eq', field: 'deployment.environment.name', value: 'prod' },
      ],
    });
    expect(resolveFilter(null, {}, 'prod')).toEqual({ op: 'eq', field: 'deployment.environment.name', value: 'prod' });
  });

  it('화면 상태 key는 상대 범위의 시각이 움직여도 같고, 조건이 바뀌면 바뀐다', () => {
    const a = widgetStateKey(ts, { svc: 'cart', env: null }, '{"kind":"relative","preset":"1h"}', null);
    expect(widgetStateKey(ts, { svc: 'cart', env: null }, '{"kind":"relative","preset":"1h"}', null)).toBe(a);
    expect(widgetStateKey(ts, { svc: 'checkout', env: null }, '{"kind":"relative","preset":"1h"}', null)).not.toBe(a);
    expect(widgetStateKey(ts, { svc: 'cart', env: null }, '{"kind":"relative","preset":"1h"}', 'prod')).not.toBe(a);
  });

  it('URL 변수 값은 외부 입력이다: 제어 문자·256 byte 초과는 버리고 기본값을 쓴다', () => {
    const vars = [{ name: 'svc', kind: 'service' as const, default: 'checkout' }];
    const bad = variableValuesChecked(vars, new URLSearchParams({ 'var-svc': 'a\u0007b' }));
    expect(bad).toEqual({ values: { svc: 'checkout' }, invalid: ['svc'] });
    expect(variableValuesChecked(vars, new URLSearchParams({ 'var-svc': 'x'.repeat(257) })).invalid).toEqual(['svc']);
    expect(variableValues(vars, new URLSearchParams({ 'var-svc': 'cart' }))).toEqual({ svc: 'cart' });
  });

  it('저장 전 전체 검사: 빈 위젯, 끊긴 변수 참조, 겹친 배치, 상위 목록 group', () => {
    const spec: DashboardSpec = {
      ...emptySpec('x'),
      variables: [],
      widgets: [ts, newWidget('w2', 'stat'), { ...newWidget('w3', 'top_list'), query: { metric: 'm', aggregation: 'sum', filter: null, group_by: [] } }],
      layout: [
        { widget_id: 'w1', x: 0, y: 0, w: 12, h: 6 },
        { widget_id: 'w2', x: 6, y: 0, w: 12, h: 6 },
        { widget_id: 'w3', x: 0, y: 6, w: 6, h: 2 },
      ],
    };
    const p = specProblems(spec);
    expect(p).toContain('“요청량”: 없는 변수 $svc를 씁니다');
    expect(p).toContain('숫자 하나 w2: metric이 없습니다');
    expect(p).toContain('상위 목록 w3: 상위 목록은 group이 필요합니다');
    expect(p.some((x) => x.includes('겹칩니다'))).toBe(true);
    expect(specProblems({ ...emptySpec('ok') })).toEqual([]);
  });
});
