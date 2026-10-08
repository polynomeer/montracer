// Metrics 화면(S08) 조회 상태 ↔ URL ↔ QuerySpec (D05 §08, D02 §13, ADR 0027·0046·0047).
// URL 키: metric(이름), agg(연산), group(label key, 쉼표), step(초, 없으면 자동), f(key=value, 여러 개).
// 공유 링크에는 형식이 정해진 metric·agg·group·step만 남는다. filter 값(f)은 자유 입력이라 빠진다(context.ts SHAREABLE).
import type { MetricAggregation, MetricFilter, MetricQuery, MetricVariant } from '../../api/types.ts';

export const MAX_METRIC_RANGE_MS = 7 * 24 * 60 * 60_000; // D02 §13
export const DICTIONARY_RANGE_MS = 24 * 60 * 60_000; // ADR 0046 사전 범위 한도
export const MAX_GROUP_BY = 5; // ADR 0027
export const MAX_FILTERS = 10; // 서버 한도 20보다 작게(화면에서 다루기 쉽게)
export const MAX_LABEL_LENGTH = 256;
export const MAX_POINTS_PER_SERIES = 2000; // D02 §19
/** 자동 step이 노리는 점 수. 이보다 많지 않은 가장 작은 step을 고른다. */
export const TARGET_POINTS = 120;
export const STEP_CHOICES = [60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400] as const;

const AGGREGATIONS: readonly MetricAggregation[] = ['rate', 'increase', 'sum', 'avg', 'min', 'max', 'count', 'hist_sum', 'p50', 'p90', 'p95', 'p99'];
/** 처음 고를 때의 기본 연산 순서: counter는 증가율, gauge는 평균, histogram은 p95 */
const DEFAULT_ORDER: readonly MetricAggregation[] = ['rate', 'avg', 'p95', 'sum'];

export const AGG_LABEL: Record<MetricAggregation, string> = {
  rate: '초당 증가율',
  increase: 'step 증가량',
  sum: 'step 합계',
  avg: '평균',
  min: '최솟값',
  max: '최댓값',
  count: '관측 수',
  hist_sum: '관측값 합',
  p50: 'p50 (bucket 병합)',
  p90: 'p90 (bucket 병합)',
  p95: 'p95 (bucket 병합)',
  p99: 'p99 (bucket 병합)',
};

export interface LabelFilter {
  key: string;
  value: string;
}

export interface MetricState {
  metric: string | null;
  agg: MetricAggregation | null;
  groupBy: string[];
  filters: LabelFilter[];
  /** null이면 자동 */
  step: number | null;
}

// 서버 한도는 byte다(UTF-8 256 byte, ADR 0027·0046)
const utf8 = new TextEncoder();
const byteLength = (s: string) => utf8.encode(s).length;
const validLabel = (s: string) => s !== '' && byteLength(s) <= MAX_LABEL_LENGTH;

export function isAggregation(v: string | null): v is MetricAggregation {
  return v !== null && (AGGREGATIONS as readonly string[]).includes(v);
}

/** 잘못된 값은 무시한다. */
export function parseMetricState(params: URLSearchParams): MetricState {
  const metric = params.get('metric');
  const agg = params.get('agg');
  const group = (params.get('group') ?? '').split(',').filter(validLabel);
  const step = params.get('step');
  const stepNum = step !== null && /^[0-9]{1,6}$/.test(step) ? Number(step) : null;
  const filters: LabelFilter[] = [];
  for (const f of params.getAll('f')) {
    const i = f.indexOf('=');
    if (i <= 0) continue;
    const key = f.slice(0, i);
    const value = f.slice(i + 1);
    if (validLabel(key) && byteLength(value) <= MAX_LABEL_LENGTH && filters.length < MAX_FILTERS) filters.push({ key, value });
  }
  return {
    metric: metric !== null && validLabel(metric) ? metric : null,
    agg: isAggregation(agg) ? agg : null,
    groupBy: [...new Set(group)].slice(0, MAX_GROUP_BY),
    filters,
    step: stepNum !== null && (STEP_CHOICES as readonly number[]).includes(stepNum) ? stepNum : null,
  };
}

export function writeMetricState(params: URLSearchParams, s: MetricState): URLSearchParams {
  const next = new URLSearchParams(params);
  for (const k of ['metric', 'agg', 'group', 'step', 'f']) next.delete(k);
  if (s.metric !== null) next.set('metric', s.metric);
  if (s.agg !== null) next.set('agg', s.agg);
  if (s.groupBy.length > 0) next.set('group', s.groupBy.join(','));
  if (s.step !== null) next.set('step', String(s.step));
  for (const f of s.filters) next.append('f', `${f.key}=${f.value}`);
  return next;
}

/** 범위에 맞는 자동 step: 점이 TARGET_POINTS 이하인 가장 작은 선택지. 1시간의 배수면 1시간 rollup을 읽는다(ADR 0028). */
export function autoStep(rangeMs: number): number {
  const seconds = rangeMs / 1000;
  return STEP_CHOICES.find((s) => seconds / s <= TARGET_POINTS) ?? 86400;
}

/** 이 범위에서 쓸 수 있는 step(series당 2,000점 이하, 범위보다 짧음) */
export function stepChoices(rangeMs: number): number[] {
  const seconds = rangeMs / 1000;
  return STEP_CHOICES.filter((s) => s < seconds && seconds / s <= MAX_POINTS_PER_SERIES);
}

export function stepLabel(seconds: number): string {
  if (seconds % 3600 === 0) return `${String(seconds / 3600)}시간`;
  return `${String(seconds / 60)}분`;
}

/** 서버가 실제로 읽은 rollup 해상도(ADR 0028·0047). 모르면 null — 추측하지 않는다. */
export function rollupLabel(sourceWindowSeconds: number | undefined): string | null {
  if (sourceWindowSeconds === 3600) return '1시간 rollup';
  if (sourceWindowSeconds === 60) return '1분 rollup';
  return null;
}

/** 사전 범위: 조사 범위의 끝에서 최대 24시간 (ADR 0046) */
export function dictionaryRange(fromMs: number, toMs: number): { from: string; to: string; clipped: boolean } {
  const from = Math.max(fromMs, toMs - DICTIONARY_RANGE_MS);
  return { from: new Date(from).toISOString(), to: new Date(toMs).toISOString(), clipped: from > fromMs };
}

/** 허용 연산 중 기본값. 없으면 null(rollup 조회 불가 유형). */
export function pickAggregation(allowed: readonly MetricAggregation[]): MetricAggregation | null {
  return DEFAULT_ORDER.find((a) => allowed.includes(a)) ?? allowed[0] ?? null;
}

/** 조사 context environment를 거는 resource 속성 (OTel semantic conventions) */
export const ENV_KEY = 'deployment.environment.name';

/** 사용자에게 보이는 유형 이름 */
export function typeLabel(v: MetricVariant): string {
  if (v.type === 'sum') return v.monotonic ? 'counter' : 'up-down counter';
  return v.type;
}

/** 결과 값의 단위: rate는 초당, 관측 수는 건수 */
export function resultUnit(agg: MetricAggregation, unit: string): string {
  if (agg === 'count') return '건';
  if (agg === 'rate') return unit === '' || unit === '1' ? '/s' : `${unit}/s`;
  return unit === '1' ? '' : unit;
}

export function toMetricQuery(s: MetricState & { metric: string; agg: MetricAggregation }, fromMs: number, toMs: number, stepSeconds: number): MetricQuery {
  const args: MetricFilter[] = s.filters.filter((f) => validLabel(f.key)).map((f) => ({ op: 'eq', field: f.key, value: f.value }));
  return {
    range: { from: new Date(fromMs).toISOString(), to: new Date(toMs).toISOString() },
    step_seconds: stepSeconds,
    expression: {
      metric: s.metric,
      aggregation: s.agg,
      ...(args.length === 0 ? {} : { filter: args.length === 1 ? args[0] : { op: 'and', args } }),
      ...(s.groupBy.length === 0 ? {} : { group_by: s.groupBy }),
    },
  };
}

/** series 이름: group_by 순서의 key=value. group이 없으면 "전체". 빈 값은 "(없음)"으로 — 0이나 빈칸으로 숨기지 않는다. */
export function seriesName(labels: Record<string, string>, groupBy: string[]): string {
  if (groupBy.length === 0) return '전체';
  return groupBy.map((k) => `${k}=${labels[k] === undefined || labels[k] === '' ? '(없음)' : labels[k]}`).join(', ');
}

const fmt = new Intl.NumberFormat('ko-KR', { maximumSignificantDigits: 4 });
// 1만 이상은 한국어 축약(만·억·조)으로 줄인다. 아주 작은 값만 지수 표기
const compact = new Intl.NumberFormat('ko-KR', { notation: 'compact', maximumSignificantDigits: 3 });

export function formatValue(v: number): string {
  if (v === 0) return '0';
  const abs = Math.abs(v);
  if (abs < 1e-3) return v.toExponential(2);
  if (abs >= 1e4) return compact.format(v);
  return fmt.format(v);
}
