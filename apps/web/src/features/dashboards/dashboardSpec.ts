// S10 dashboard의 순수 함수 (D05 §10~11, ADR 0057·0058).
// - 위젯 query의 "$이름"은 변수다. 변수가 "전체"(null)면 그 조건을 뺀다 — 빈 값으로 거르지 않는다.
// - 시계열은 범위에 맞는 step, 요약(stat·table·top_list)은 범위 전체 한 점이다(ADR 0042).
// - 배치는 24열 grid이고 겹치지 않는다(서버와 같은 규칙, 저장 전에 화면에서 먼저 막는다).
import type { DashFilter, DashboardSpec, Placement, Variable, VariableKind, Widget, WidgetKind } from '../../api/dashboards.ts';
import type { MetricFilter, MetricQuery } from '../../api/types.ts';
import { metricWindow } from '../services/red.ts';

export const GRID_COLUMNS = 24;
export const MAX_WIDGETS = 50;
export const MAX_VARIABLES = 10;
export const MAX_WIDGET_HEIGHT = 48;
export const MAX_GRID_ROWS = 1000;
export const MAX_TEXT = 10_000;
/** 한 화면에서 동시에 도는 위젯 조회 수(D05 §10 "view당 병렬 6개") */
export const MAX_PARALLEL = 6;
/** 자동 갱신 간격(D05 §11 "30초, 비활성 탭 pause") */
export const REFRESH_MS = 30_000;

export const VARIABLE_FIELD: Record<VariableKind, string> = {
  environment: 'deployment.environment.name',
  service: 'service.name',
};

export const KIND_LABEL: Record<WidgetKind, string> = {
  time_series: '시계열',
  stat: '숫자 하나',
  table: '표',
  top_list: '상위 목록',
  text: '글',
};

export const TIME_OVERRIDES = ['15m', '1h', '4h', '1d', '7d'] as const;
const OVERRIDE_MS: Record<(typeof TIME_OVERRIDES)[number], number> = {
  '15m': 15 * 60_000,
  '1h': 60 * 60_000,
  '4h': 4 * 60 * 60_000,
  '1d': 24 * 60 * 60_000,
  '7d': 7 * 24 * 60 * 60_000,
};

export const emptySpec = (title = ''): DashboardSpec => ({ version: 1, title, description: '', variables: [], layout: [], widgets: [] });

/** 변수 값: 이름 → 고른 값(null이면 "전체") */
export type VariableValues = Record<string, string | null>;

const MAX_LABEL_BYTES = 256;

/** label 값 검사(서버와 같은 규칙): 1~256 byte, 제어 문자 없음 */
export function validLabelValue(v: string): boolean {
  return v !== '' && new TextEncoder().encode(v).length <= MAX_LABEL_BYTES && !/[\u0000-\u001f\u007f]/.test(v);
}

/**
 * 변수 기본값과 URL 값을 합친다. URL에 `var-이름`이 있으면 그 값("" = 전체), 없으면 정의의 기본값.
 * URL 값은 외부 입력이다: 규칙에 맞지 않으면 버리고 기본값을 쓴다(invalid에 이름을 남겨 화면이 알린다).
 */
export function variableValues(vars: Variable[], params: URLSearchParams): VariableValues {
  return variableValuesChecked(vars, params).values;
}

export function variableValuesChecked(vars: Variable[], params: URLSearchParams): { values: VariableValues; invalid: string[] } {
  const values: VariableValues = {};
  const invalid: string[] = [];
  for (const v of vars) {
    const raw = params.get(`var-${v.name}`);
    if (raw === null) values[v.name] = v.default;
    else if (raw === '') values[v.name] = null;
    else if (validLabelValue(raw)) values[v.name] = raw;
    else {
      values[v.name] = v.default;
      invalid.push(v.name);
    }
  }
  return { values, invalid };
}

/** "$이름" 참조 */
export function variableRef(value: string | undefined): string | null {
  return value !== undefined && value.startsWith('$') ? value.slice(1) : null;
}

/** 위젯이 쓰는 변수 이름(배지 표시용) */
export function widgetVariables(w: Widget): string[] {
  const out: string[] = [];
  const walk = (f: DashFilter | null | undefined) => {
    if (f == null) return;
    const ref = variableRef(f.value);
    if (f.op === 'eq' && ref !== null) out.push(ref);
    for (const a of f.args ?? []) walk(a);
  };
  walk(w.query?.filter);
  return [...new Set(out)];
}

/**
 * 변수를 값으로 바꾼 filter. "전체"인 변수의 조건은 뺀다. 남는 조건이 없으면 undefined.
 * globalEnv(상단 조사 context의 환경, D05 §11 "공통 context")는 위젯이 환경을 직접 거르지 않을 때만 더한다:
 * 위젯의 환경 조건(값 또는 고른 환경 변수)이 앞선다. 환경 변수가 "전체"면 상단 환경을 따른다.
 */
export function resolveFilter(f: DashFilter | null, values: VariableValues, globalEnv: string | null = null): MetricFilter | undefined {
  const leaves: MetricFilter[] = [];
  const walk = (n: DashFilter) => {
    if (n.op === 'and') {
      for (const a of n.args ?? []) walk(a);
      return;
    }
    const ref = variableRef(n.value);
    if (ref === null) {
      leaves.push({ op: 'eq', field: n.field ?? '', value: n.value ?? '' });
      return;
    }
    const v = values[ref];
    if (v != null) leaves.push({ op: 'eq', field: n.field ?? '', value: v });
  };
  if (f !== null) walk(f);
  if (globalEnv !== null && !leaves.some((l) => l.field === VARIABLE_FIELD.environment)) {
    leaves.push({ op: 'eq', field: VARIABLE_FIELD.environment, value: globalEnv });
  }
  if (leaves.length === 0) return undefined;
  return leaves.length === 1 ? leaves[0] : { op: 'and', args: leaves };
}

/** 위젯의 조회 범위: 시간 덮어쓰기가 있으면 지금부터 그만큼, 아니면 dashboard 범위 */
export function widgetRange(w: Widget, fromMs: number, toMs: number, nowMs: number): { fromMs: number; toMs: number } {
  if (w.time_override === null) return { fromMs, toMs };
  return { fromMs: nowMs - OVERRIDE_MS[w.time_override.range], toMs: nowMs };
}

/** 위젯 하나의 metric 조회. text 위젯이나 범위가 7일을 넘으면 null(이유는 widgetRangeProblem). */
export function resolveQuery(w: Widget, values: VariableValues, fromMs: number, toMs: number, globalEnv: string | null = null): MetricQuery | null {
  if (w.query === null) return null;
  const win = metricWindow(fromMs, toMs);
  if (win === null) return null;
  const filter = resolveFilter(w.query.filter, values, globalEnv);
  return {
    range: { from: win.fromIso, to: win.toIso },
    // 시계열은 점 여러 개, 요약 위젯은 범위 전체 한 점(분위수를 평균하지 않는다 — 계약 5)
    step_seconds: w.kind === 'time_series' ? win.chartStepSeconds : win.rangeSeconds,
    expression: {
      metric: w.query.metric,
      aggregation: w.query.aggregation,
      ...(filter === undefined ? {} : { filter }),
      ...(w.query.group_by.length === 0 ? {} : { group_by: w.query.group_by }),
    },
  };
}

/** 같은 조회를 합치는 key(D05 §10 "같은 query는 합친다") */
export const queryKey = (q: MetricQuery): string => JSON.stringify(q);

/**
 * 위젯 화면 상태의 key: 조회 조건이 바뀔 때만 바뀐다. 상대 범위의 절대 시각(분마다 움직임)은 넣지 않는다 —
 * 그래야 자동 갱신 중에 이전 값을 유지하고, 갱신이 실패해도 마지막 값과 "갱신 실패"를 보인다(D05 §03).
 */
export function widgetStateKey(w: Widget, values: VariableValues, rangeKey: string, globalEnv: string | null): string {
  return JSON.stringify({ id: w.id, kind: w.kind, q: w.query, f: w.query === null ? null : (resolveFilter(w.query.filter, values, globalEnv) ?? null), r: w.time_override ?? rangeKey });
}

// ---- 배치 ----

export function overlaps(a: Placement, b: Placement): boolean {
  return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;
}

/** 배치 하나가 grid 안이고 다른 위젯과 겹치지 않는지. 문제가 있으면 이유. */
export function placementProblem(p: Placement, layout: Placement[]): string | null {
  if (![p.x, p.y, p.w, p.h].every(Number.isInteger)) return '위치와 크기는 정수여야 합니다';
  if (p.x < 0 || p.x >= GRID_COLUMNS) return `가로 위치는 0~${String(GRID_COLUMNS - 1)}입니다`;
  if (p.w < 1 || p.x + p.w > GRID_COLUMNS) return `폭은 1 이상이고 오른쪽 끝(${String(GRID_COLUMNS)}열)을 넘을 수 없습니다`;
  if (p.y < 0 || p.y > MAX_GRID_ROWS) return `세로 위치는 0~${String(MAX_GRID_ROWS)}입니다`;
  if (p.h < 1 || p.h > MAX_WIDGET_HEIGHT) return `높이는 1~${String(MAX_WIDGET_HEIGHT)}입니다`;
  const other = layout.find((o) => o.widget_id !== p.widget_id && overlaps(o, p));
  return other === undefined ? null : `다른 위젯(${other.widget_id})과 겹칩니다`;
}

/** 새 위젯을 놓을 자리: 맨 아래 왼쪽 */
/** 새 위젯을 더할 자리가 grid 안에 있는지 */
export function canPlace(layout: Placement[], h = 6): boolean {
  return layout.reduce((m, p) => Math.max(m, p.y + p.h), 0) + h <= MAX_GRID_ROWS;
}

export function nextPlacement(layout: Placement[], widgetId: string, w = 12, h = 6): Placement {
  const bottom = layout.reduce((m, p) => Math.max(m, p.y + p.h), 0);
  return { widget_id: widgetId, x: 0, y: bottom, w, h };
}

/** grid 위치를 CSS 변수로 넘긴다(좁은 화면은 CSS가 이를 무시하고 읽는 순서로 쌓는다). */
export function cellStyle(p: Placement): Record<string, string> {
  return { '--dash-col': `${String(p.x + 1)} / span ${String(p.w)}`, '--dash-row': `${String(p.y + 1)} / span ${String(p.h)}` };
}

/** 읽는 순서(위→아래, 왼→오른쪽). 좁은 화면은 이 순서로 한 줄씩 쌓는다. */
export function readingOrder(layout: Placement[]): Placement[] {
  return [...layout].sort((a, b) => a.y - b.y || a.x - b.x);
}

export function newWidgetId(widgets: Widget[]): string {
  const ids = new Set(widgets.map((w) => w.id));
  for (let i = widgets.length + 1; ; i++) {
    if (!ids.has(`w${String(i)}`)) return `w${String(i)}`;
  }
}

/** 새 위젯 기본값(종류별 표시 기본값은 서버와 같다) */
export function newWidget(id: string, kind: WidgetKind): Widget {
  const base = { id, kind, title: '', unit: null, time_override: null } as const;
  switch (kind) {
    case 'text':
      return { ...base, display: {}, query: null, text: '' };
    case 'time_series':
      return { ...base, display: { stack: false }, query: { metric: '', aggregation: 'rate', filter: null, group_by: [] }, text: null };
    case 'stat':
      return { ...base, display: { decimals: 2 }, query: { metric: '', aggregation: 'p95', filter: null, group_by: [] }, text: null };
    case 'table':
    case 'top_list':
      return { ...base, display: { decimals: 2, limit: 10, sort: 'desc' }, query: { metric: '', aggregation: 'sum', filter: null, group_by: kind === 'top_list' ? ['service.name'] : [] }, text: null };
  }
}

/** 위젯 종류를 바꾼다: 쓸 수 있는 설정은 남기고, 그 종류가 쓰지 않는 표시 설정은 뺀다(서버가 거절한다). */
export function changeKind(w: Widget, kind: WidgetKind): Widget {
  const fresh = newWidget(w.id, kind);
  return { ...fresh, title: w.title, unit: kind === 'text' ? null : w.unit, time_override: kind === 'text' ? null : w.time_override, query: fresh.query === null ? null : (w.query ?? fresh.query), text: kind === 'text' ? (w.text ?? '') : null };
}

// ---- 저장 전 전체 검사(서버가 최종 검증한다) ----

/** 정의 전체의 문제(사람이 읽는 문장). 위젯 편집을 취소해 빈 위젯이 남았거나, 변수를 지워 참조가 끊긴 경우를 저장 전에 잡는다. */
export function specProblems(spec: DashboardSpec): string[] {
  const out: string[] = [];
  if (spec.title.trim() === '') out.push('제목을 입력하세요');
  const names = new Map<string, Variable>();
  spec.variables.forEach((v, i) => {
    if (!/^[a-z][a-z0-9_]{0,31}$/.test(v.name)) out.push(`변수 ${String(i + 1)}: 이름 형식(영문 소문자로 시작, 소문자·숫자·_ 32자)`);
    else if (names.has(v.name)) out.push(`변수 $${v.name}: 이름이 겹칩니다`);
    names.set(v.name, v);
    if (v.default !== null && !validLabelValue(v.default)) out.push(`변수 $${v.name}: 기본값은 1~256 byte, 제어 문자 없음`);
  });
  for (const w of spec.widgets) {
    const label = w.title === '' ? `${KIND_LABEL[w.kind]} ${w.id}` : `“${w.title}”`;
    if (w.kind === 'text') {
      if ((w.text ?? '').trim() === '') out.push(`${label}: 글이 비었습니다`);
      continue;
    }
    if ((w.query?.metric ?? '').trim() === '') out.push(`${label}: metric이 없습니다`);
    if (w.kind === 'stat' && (w.query?.group_by.length ?? 0) > 0) out.push(`${label}: 숫자 하나 위젯은 group으로 나누지 않습니다`);
    if (w.kind === 'top_list' && (w.query?.group_by.length ?? 0) === 0) out.push(`${label}: 상위 목록은 group이 필요합니다`);
    const walk = (f: DashFilter | null | undefined) => {
      if (f == null) return;
      const ref = variableRef(f.value);
      if (f.op === 'eq' && ref !== null) {
        const v = names.get(ref);
        if (v === undefined) out.push(`${label}: 없는 변수 $${ref}를 씁니다`);
        else if (VARIABLE_FIELD[v.kind] !== f.field) out.push(`${label}: $${ref}는 ${VARIABLE_FIELD[v.kind]}에만 쓸 수 있습니다`);
      }
      for (const a of f.args ?? []) walk(a);
    };
    walk(w.query?.filter);
  }
  for (const p of spec.layout) {
    const problem = placementProblem(p, spec.layout);
    if (problem !== null) out.push(`위젯 ${p.widget_id} 배치: ${problem}`);
  }
  return out;
}

// ---- 충돌 비교 (D05 §10 "concurrent revision 충돌은 차이를 보여준다") ----

const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

/** 두 정의의 차이를 사람이 읽는 줄로. 위젯은 id로 맞춘다. */
export function specDiff(from: DashboardSpec, to: DashboardSpec): string[] {
  const out: string[] = [];
  if (from.title !== to.title) out.push(`제목: “${from.title}” → “${to.title}”`);
  if (from.description !== to.description) out.push('설명이 바뀌었습니다');
  if (!same(from.variables, to.variables)) out.push(`변수: ${from.variables.map((v) => v.name).join(', ') || '없음'} → ${to.variables.map((v) => v.name).join(', ') || '없음'}`);
  const a = new Map(from.widgets.map((w) => [w.id, w]));
  const b = new Map(to.widgets.map((w) => [w.id, w]));
  const name = (w: Widget) => (w.title === '' ? `${KIND_LABEL[w.kind]} ${w.id}` : `“${w.title}”`);
  for (const [id, w] of b) {
    const before = a.get(id);
    if (before === undefined) out.push(`위젯 추가: ${name(w)}`);
    else if (!same(before, w)) out.push(`위젯 변경: ${name(w)}`);
  }
  for (const [id, w] of a) if (!b.has(id)) out.push(`위젯 삭제: ${name(w)}`);
  const pa = new Map(from.layout.map((p) => [p.widget_id, p]));
  const moved = to.layout.filter((p) => pa.has(p.widget_id) && !same(pa.get(p.widget_id), p)).length;
  if (moved > 0) out.push(`위젯 ${String(moved)}개의 위치·크기`);
  return out;
}

// ---- 편집 초안(브라우저에만, D05 §10 "autosave는 로컬 draft") ----

export interface Draft {
  baseRevision: number | null;
  /** 초안을 시작한 서버 정의. 복원 뒤 충돌이 나면 "그 사이 바뀐 것"을 이 기준으로 비교한다 */
  baseSpec: DashboardSpec | null;
  spec: DashboardSpec;
  savedAt: number;
}

const draftKey = (org: string, id: string) => `montracer.dashboard-draft.${org}.${id}`;

/** 저장소를 쓸 수 없으면(사생활 보호 창 등) null — 초안 없이 동작한다. */
export function loadDraft(org: string, id: string): Draft | null {
  try {
    const raw = globalThis.localStorage.getItem(draftKey(org, id));
    if (raw === null) return null;
    const d = JSON.parse(raw) as Partial<Draft>;
    if (typeof d.savedAt !== 'number' || typeof d.spec?.title !== 'string' || !Array.isArray(d.spec.widgets)) return null;
    return { baseRevision: d.baseRevision ?? null, baseSpec: d.baseSpec ?? null, spec: d.spec, savedAt: d.savedAt };
  } catch {
    return null;
  }
}

export function saveDraft(org: string, id: string, d: Draft): void {
  try {
    globalThis.localStorage.setItem(draftKey(org, id), JSON.stringify(d));
  } catch {
    // 저장 못 해도 편집은 계속된다(초안은 편의다)
  }
}

export function clearDraft(org: string, id: string): void {
  try {
    globalThis.localStorage.removeItem(draftKey(org, id));
  } catch {
    // 무시
  }
}

// ---- 보기 화면 표시(디자인 시안 S10) ----

const PERCENTILES = new Set(['p50', 'p90', 'p95', 'p99']);

/** 분위수 위젯: 서버가 histogram bucket을 합친 뒤 계산한다(평균의 평균 아님, 계약 5). */
export const isPercentile = (w: Widget): boolean => w.query !== null && PERCENTILES.has(w.query.aggregation);

/** 위젯 부제: 종류 · 연산 · 단위 */
export function widgetSubtitle(w: Widget): string {
  const parts = [KIND_LABEL[w.kind]];
  if (w.query !== null) parts.push(w.query.aggregation);
  if (w.unit != null && w.unit !== '') parts.push(w.unit);
  return parts.join(' · ');
}

/**
 * 위젯이 dashboard 변수나 상단 환경을 무시하게 되는 조건(resolveFilter의 우선순위와 같다).
 * - 고정 값: service.name = payment-gateway면 $service를 골라도 이 위젯은 바뀌지 않는다. 같은 field의 변수를 위젯이 함께 쓰면
 *   두 조건이 다 걸리므로(좁아질 뿐) 그 변수는 무시가 아니다.
 * - 환경: 위젯의 환경 조건(고정 값, 또는 값이 골라진 환경 변수)이 상단 환경보다 앞선다.
 */
export function widgetOverrides(
  w: Widget,
  variables: Variable[],
  globalEnv: string | null,
  values: VariableValues = {},
): { field: string; value: string; ignores: string[] }[] {
  const fixed: { field: string; value: string }[] = [];
  const walk = (f: DashFilter | null | undefined) => {
    if (f == null) return;
    if (f.op === 'eq' && variableRef(f.value) === null && f.field !== undefined && f.value !== undefined) fixed.push({ field: f.field, value: f.value });
    for (const a of f.args ?? []) walk(a);
  };
  walk(w.query?.filter);
  const used = new Set(widgetVariables(w));
  const out: { field: string; value: string; ignores: string[] }[] = [];
  const seen = new Set<string>();
  for (const f of fixed) {
    const k = `${f.field}=${f.value}`;
    if (seen.has(k)) continue;
    seen.add(k);
    const ignores = variables.filter((v) => VARIABLE_FIELD[v.kind] === f.field && !used.has(v.name)).map((v) => `$${v.name}`);
    if (f.field === VARIABLE_FIELD.environment && globalEnv !== null) ignores.push('상단 환경');
    if (ignores.length > 0) out.push({ ...f, ignores });
  }
  // 환경 변수에 값이 골라져 있으면 상단 환경 대신 그 값으로 거른다(고정 환경이 이미 있으면 위에서 말함)
  if (globalEnv !== null && !fixed.some((f) => f.field === VARIABLE_FIELD.environment)) {
    for (const v of variables) {
      const value = values[v.name];
      if (v.kind === 'environment' && used.has(v.name) && value != null) out.push({ field: VARIABLE_FIELD.environment, value: `$${v.name} = ${value}`, ignores: ['상단 환경'] });
    }
  }
  return out;
}

/** query 묶음 요약: 위젯 수, 조회하는 위젯 수, 같은 query를 합친 뒤 실제 요청 수. */
export function poolSummary(widgets: Widget[], values: VariableValues, fromMs: number, toMs: number, nowMs: number, globalEnv: string | null) {
  const keys: string[] = [];
  for (const w of widgets) {
    const r = widgetRange(w, fromMs, toMs, nowMs);
    const q = resolveQuery(w, values, r.fromMs, r.toMs, globalEnv);
    if (q !== null) keys.push(queryKey(q));
  }
  const unique = new Set(keys).size;
  return { widgets: widgets.length, queries: keys.length, requests: unique, merged: keys.length - unique };
}
