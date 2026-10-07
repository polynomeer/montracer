// 서비스 RED 계산 (D05 §05, D02 §07, ADR 0042).
// - 원천은 비샘플링 SDK histogram `http.server.request.duration`이다(계약 5). trace 표본으로 세지 않는다.
// - 요청 수는 histogram count, p95는 서버가 bucket을 병합해 계산한다. 여기서 분위수를 평균하지 않는다.
// - 오류는 서버 응답 status 5xx다(OTel HTTP semconv: server span은 5xx만 오류, 4xx는 client 오류).
// - 오류율 = 5xx 수 / 전체 수. 서버에 metric 간 연산이 없어(ADR 0027 §5) status로 나눠 받아 여기서 나눈다.
// - 값이 없으면 null과 이유를 남긴다. 0으로 채우지 않는다(계약 6).
import type { MetricFilter, MetricPoint, MetricSeries, PointReason, ServiceItem } from '../../api/types.ts';

export const DURATION_METRIC = 'http.server.request.duration';
export const STATUS_KEY = 'http.response.status_code';
export const ROUTE_KEY = 'http.route';
export const METHOD_KEY = 'http.request.method';

/** metric 조회 최대 범위 (ADR 0027, D02 §19). */
export const MAX_METRIC_RANGE_MS = 7 * 24 * 60 * 60_000;
/** 차트 한 줄의 목표 점 수. */
export const TARGET_POINTS = 120;

const MINUTE_MS = 60_000;

export type Reason = PointReason | 'no_requests' | 'no_series';

export interface Point {
  tMs: number;
  value: number | null;
  reason: Reason | null;
  /** 값이 일부 window만으로 계산됐다(하한일 수 있다) 또는 일부 group 값이 비었다. */
  partial: boolean;
}

export interface MetricWindow {
  fromIso: string;
  toIso: string;
  rangeSeconds: number;
  chartStepSeconds: number;
}

/**
 * 조사 범위를 metric 조회 범위로 바꾼다. 시작은 분 내림, 끝은 분 올림으로 맞춘다 — 그래야 범위 전체 한 점 요약
 * (step = 범위 길이, ADR 0042)이 가능하다. 7일을 넘으면 null이다(서버 한도, 화면에 이유를 보인다).
 */
export function metricWindow(fromMs: number, toMs: number): MetricWindow | null {
  if (!(toMs > fromMs)) return null;
  const from = Math.floor(fromMs / MINUTE_MS) * MINUTE_MS;
  const to = Math.ceil(toMs / MINUTE_MS) * MINUTE_MS;
  if (to <= from || to - from > MAX_METRIC_RANGE_MS) return null;
  const rangeSeconds = (to - from) / 1000;
  // 1시간을 넘는 step은 1시간 배수로 올려 1시간 rollup을 읽게 한다(긴 범위 scan 비용, ADR 0028).
  const raw = Math.max(60, Math.ceil(rangeSeconds / TARGET_POINTS / 60) * 60);
  const chartStepSeconds = raw > 3600 ? Math.ceil(raw / 3600) * 3600 : raw;
  return { fromIso: new Date(from).toISOString(), toIso: new Date(to).toISOString(), rangeSeconds, chartStepSeconds };
}

/** 서비스의 metric을 고르는 조건: 이름·namespace·environment (catalog 자연 키, D02 §08). */
export function serviceFilter(s: Pick<ServiceItem, 'name' | 'namespace' | 'environment'>): MetricFilter {
  return {
    op: 'and',
    args: [
      { op: 'eq', field: 'service.name', value: s.name },
      { op: 'eq', field: 'service.namespace', value: s.namespace },
      { op: 'eq', field: 'deployment.environment.name', value: s.environment },
    ],
  };
}

export function isServerError(statusCode: string | undefined): boolean {
  if (statusCode === undefined || !/^\d{3}$/.test(statusCode)) return false;
  const n = Number(statusCode);
  return n >= 500 && n <= 599;
}

// 이유 우선순위: 아직 계산 중이면 pending이 가장 정확하다. 그다음 데이터 이상, 마지막이 단순 부재.
const REASON_ORDER: Reason[] = ['pending', 'bounds_mismatch', 'unit_conflict', 'type_conflict', 'missing_baseline', 'not_applicable', 'no_data'];

function pickReason(points: MetricPoint[]): Reason {
  for (const r of REASON_ORDER) {
    if (points.some((p) => p.reason === r)) return r;
  }
  return 'no_data';
}

/** 같은 step 격자의 series에서 시각 목록을 얻는다. */
function stepTimes(series: MetricSeries[]): number[] {
  const first = series[0];
  return first === undefined ? [] : first.points.map((p) => Date.parse(p.t));
}

export interface StatusCombined {
  requestsPerSecond: Point[];
  errorRate: Point[];
  requests: Point[];
  errors: Point[];
}

/**
 * status별 count series를 step마다 합친다.
 * - 어느 status에도 값이 없으면 null + 가장 정확한 이유.
 * - 일부 status만 no_data면 그 status 요청이 없었던 것이다(delta histogram은 관측이 있을 때만 보낸다). 그 외 이유로 빈 status가
 *   섞이면 합이 하한일 수 있어 partial로 표시한다.
 */
export function combineStatus(series: MetricSeries[], stepSeconds: number): StatusCombined {
  const times = stepTimes(series);
  const out: StatusCombined = { requestsPerSecond: [], errorRate: [], requests: [], errors: [] };
  times.forEach((tMs, i) => {
    const pts = series.map((s) => s.points[i]).filter((p): p is MetricPoint => p !== undefined);
    const withValue = series.filter((s) => s.points[i]?.v !== null && s.points[i] !== undefined);
    if (withValue.length === 0) {
      const reason = pickReason(pts);
      for (const k of ['requestsPerSecond', 'errorRate', 'requests', 'errors'] as const) {
        out[k].push({ tMs, value: null, reason, partial: false });
      }
      return;
    }
    const partial = pts.some((p) => p.partial || (p.v === null && p.reason !== 'no_data'));
    let total = 0;
    let errors = 0;
    for (const s of withValue) {
      const v = s.points[i]?.v ?? 0;
      total += v;
      if (isServerError(s.labels[STATUS_KEY])) errors += v;
    }
    out.requests.push({ tMs, value: total, reason: null, partial });
    out.errors.push({ tMs, value: errors, reason: null, partial });
    out.requestsPerSecond.push({ tMs, value: total / stepSeconds, reason: null, partial });
    out.errorRate.push(
      total > 0 ? { tMs, value: errors / total, reason: null, partial } : { tMs, value: null, reason: 'no_requests', partial },
    );
  });
  return out;
}

/** group_by 없는 series 하나를 Point로 바꾼다. series가 없으면 빈 배열이다(화면이 'no_series'로 표시). */
export function toPoints(series: MetricSeries | undefined): Point[] {
  if (series === undefined) return [];
  return series.points.map((p) => ({ tMs: Date.parse(p.t), value: p.v, reason: p.v === null ? (p.reason ?? 'no_data') : null, partial: p.partial }));
}

/** 범위 전체 요약 값(한 점). series가 없으면 null + no_series. */
export function single(points: Point[]): Point {
  return points[0] ?? { tMs: Number.NaN, value: null, reason: 'no_series', partial: false };
}

export interface ResourceRow {
  route: string;
  method: string;
  requestsPerSecond: number | null;
  errorRate: number | null;
  p95: Point;
  totalSeconds: Point;
  partial: boolean;
  /** 요청 수 값이 하나도 없을 때의 이유 (요청/s·오류율이 null인 까닭). */
  countReason: Reason | null;
}

const keyOf = (labels: Record<string, string>) => `${labels[METHOD_KEY] ?? ''} ${labels[ROUTE_KEY] ?? ''}`;

/**
 * endpoint 표 (D05 §05: endpoint, requests/s, error rate, p95, total time, 기본 정렬 total time 내림차순).
 * 입력은 범위 전체 한 점 query다: count by (route, method, status), p95 by (route, method), hist_sum by (route, method).
 */
export function resourceRows(
  countByStatus: MetricSeries[],
  p95ByRoute: MetricSeries[],
  sumByRoute: MetricSeries[],
  rangeSeconds: number,
): ResourceRow[] {
  const rows = new Map<string, { route: string; method: string; total: number; errors: number; any: boolean; partial: boolean; points: MetricPoint[] }>();
  for (const s of countByStatus) {
    const k = keyOf(s.labels);
    const row = rows.get(k) ?? {
      route: s.labels[ROUTE_KEY] ?? '',
      method: s.labels[METHOD_KEY] ?? '',
      total: 0,
      errors: 0,
      any: false,
      partial: false,
      points: [],
    };
    const p = s.points[0];
    if (p !== undefined) row.points.push(p);
    if (p !== undefined && p.v !== null) {
      row.total += p.v;
      if (isServerError(s.labels[STATUS_KEY])) row.errors += p.v;
      row.any = true;
    }
    row.partial ||= p === undefined || p.partial || (p.v === null && p.reason !== 'no_data');
    rows.set(k, row);
  }
  const byKey = (list: MetricSeries[]) => new Map(list.map((s) => [keyOf(s.labels), single(toPoints(s))]));
  const p95s = byKey(p95ByRoute);
  const sums = byKey(sumByRoute);
  const missing: Point = { tMs: Number.NaN, value: null, reason: 'no_series', partial: false };
  const out: ResourceRow[] = [...rows.entries()].map(([k, r]) => ({
    route: r.route,
    method: r.method,
    requestsPerSecond: r.any ? r.total / rangeSeconds : null,
    errorRate: r.any && r.total > 0 ? r.errors / r.total : null,
    p95: p95s.get(k) ?? missing,
    totalSeconds: sums.get(k) ?? missing,
    partial: r.partial,
    countReason: r.any ? null : pickReason(r.points),
  }));
  // total time 내림차순, 값 없는 행은 뒤로. 동률은 endpoint 이름순(안정적인 순서).
  return out.sort((a, b) => {
    const av = a.totalSeconds.value;
    const bv = b.totalSeconds.value;
    if (av === null && bv !== null) return 1;
    if (bv === null && av !== null) return -1;
    if (av !== null && bv !== null && av !== bv) return bv - av;
    return `${a.method} ${a.route}`.localeCompare(`${b.method} ${b.route}`);
  });
}

/** 시간 값(unit 's' 또는 'ms')을 ms로. 모르는 단위면 null(임의 변환하지 않는다). */
export function toMilliseconds(value: number, unit: string | null): number | null {
  if (unit === 's') return value * 1000;
  if (unit === 'ms') return value;
  return null;
}
