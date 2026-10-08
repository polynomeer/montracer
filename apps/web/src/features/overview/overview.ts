// Overview(S01) 서비스별 RED 요약 (D05 §04 "service summary, 상태→서비스", ADR 0048).
// - 원천은 서비스 상세(S02)와 같은 비샘플링 histogram `http.server.request.duration`이다(계약 5, ADR 0042).
// - 서비스마다 조회하지 않는다. 범위 전체 한 점 query를 서비스 자연 키(namespace·name·environment)로 group해 한 번에 받는다.
// - p95는 서버가 서비스별로 bucket을 병합한 값이다. 여기서 분위수를 평균하지 않는다.
// - catalog에 있는데 metric이 없는 서비스는 "요청 metric 없음"으로 남긴다(0 요청이 아니다, 계약 6).
// - catalog에 없는 metric 서비스(등록 누락)는 링크 없이 보인다.
import type { MetricPoint, MetricSeries, ServiceItem } from '../../api/types.ts';
import { STATUS_KEY, isServerError, pickReason, single, toMilliseconds, toPoints, type Point, type Reason } from '../services/red.ts';

export const NAMESPACE_KEY = 'service.namespace';
export const NAME_KEY = 'service.name';
export const ENV_KEY = 'deployment.environment.name';
/** 서비스 자연 키 (D02 §08 catalog, S02 serviceFilter와 같은 순서) */
export const SERVICE_KEYS = [NAMESPACE_KEY, NAME_KEY, ENV_KEY] as const;

export interface OverviewRow {
  key: string;
  name: string;
  namespace: string;
  environment: string;
  /** catalog의 service_id. catalog에서 찾지 못했으면 null */
  serviceId: string | null;
  /**
   * catalog 대조 결과. registered = 등록됨, unregistered = 전체 catalog를 받았는데 없음(등록 누락),
   * unknown = catalog를 받지 못했거나(실패) 잘려서(page 한도) 확인할 수 없음 — "미등록"으로 단정하지 않는다(D05 §03).
   */
  catalog: 'registered' | 'unregistered' | 'unknown';
  catalogStatus: ServiceItem['status'] | null;
  requestsPerSecond: number | null;
  errorRate: number | null;
  /** p95(ms). 값이 없으면 value null + 이유 */
  p95: Point;
  partial: boolean;
  /** 요청 수 값이 없을 때의 이유 */
  countReason: Reason | null;
}

const keyOf = (namespace: string, name: string, environment: string) => `${namespace}\u0000${name}\u0000${environment}`;
const labelsKey = (labels: Record<string, string>) => keyOf(labels[NAMESPACE_KEY] ?? '', labels[NAME_KEY] ?? '', labels[ENV_KEY] ?? '');

/** p95 Point를 ms로. 단위를 모르면 값을 버리고 이유를 남긴다(임의 변환하지 않는다). */
function p95Ms(s: MetricSeries | undefined): Point {
  const p = single(toPoints(s));
  if (p.value === null) return p;
  const ms = toMilliseconds(p.value, s?.unit ?? null);
  return ms === null ? { ...p, value: null, reason: 'unit_conflict' } : { ...p, value: ms };
}

export function overviewRows(
  catalog: ServiceItem[],
  countsByStatus: MetricSeries[],
  p95ByService: MetricSeries[],
  rangeSeconds: number,
  /** catalog 전체를 받았는지. false면 찾지 못한 서비스를 unknown으로 둔다 */
  catalogComplete = true,
): OverviewRow[] {
  type Acc = { total: number; errors: number; any: boolean; partial: boolean; points: MetricPoint[]; labels: Record<string, string> };
  const acc = new Map<string, Acc>();
  for (const s of countsByStatus) {
    const k = labelsKey(s.labels);
    const a = acc.get(k) ?? { total: 0, errors: 0, any: false, partial: false, points: [], labels: s.labels };
    const p = s.points[0];
    if (p !== undefined) a.points.push(p);
    if (p !== undefined && p.v !== null) {
      a.total += p.v;
      if (isServerError(s.labels[STATUS_KEY])) a.errors += p.v;
      a.any = true;
    }
    a.partial ||= p === undefined || p.partial || (p.v === null && p.reason !== 'no_data');
    acc.set(k, a);
  }
  const p95 = new Map(p95ByService.map((s) => [labelsKey(s.labels), s]));
  const noSeries: Point = { tMs: Number.NaN, value: null, reason: 'no_series', partial: false };

  const rows = new Map<string, OverviewRow>();
  const fromMetric = (k: string, a: Acc | undefined) => ({
    requestsPerSecond: a?.any === true ? a.total / rangeSeconds : null,
    errorRate: a?.any === true && a.total > 0 ? a.errors / a.total : null,
    p95: p95.has(k) ? p95Ms(p95.get(k)) : noSeries,
    partial: a?.partial ?? false,
    countReason: a === undefined ? ('no_series' as Reason) : a.any ? null : pickReason(a.points),
  });
  for (const s of catalog) {
    const k = keyOf(s.namespace, s.name, s.environment);
    rows.set(k, { key: k, name: s.name, namespace: s.namespace, environment: s.environment, serviceId: s.service_id, catalog: 'registered', catalogStatus: s.status, ...fromMetric(k, acc.get(k)) });
  }
  for (const [k, a] of acc) {
    if (rows.has(k)) continue;
    rows.set(k, {
      key: k,
      name: a.labels[NAME_KEY] ?? '',
      namespace: a.labels[NAMESPACE_KEY] ?? '',
      environment: a.labels[ENV_KEY] ?? '',
      serviceId: null,
      catalog: catalogComplete ? 'unregistered' : 'unknown',
      catalogStatus: null,
      ...fromMetric(k, a),
    });
  }
  return [...rows.values()];
}

export type SortKey = 'error_rate' | 'rps' | 'p95' | 'name';
export const SORT_KEYS: readonly SortKey[] = ['error_rate', 'rps', 'p95', 'name'];

export function isSortKey(v: string | null): v is SortKey {
  return v !== null && (SORT_KEYS as readonly string[]).includes(v);
}

const displayName = (r: OverviewRow) => (r.namespace === '' ? r.name : `${r.namespace}/${r.name}`);

/**
 * 정렬: 숫자 열은 큰 값부터, 값 없는 행은 항상 뒤(0으로 보지 않는다). 이름은 가나다순.
 * 동률은 요청량 큰 순 → 이름순(안정적인 순서).
 */
export function sortRows(rows: OverviewRow[], key: SortKey): OverviewRow[] {
  const val = (r: OverviewRow): number | null => (key === 'error_rate' ? r.errorRate : key === 'rps' ? r.requestsPerSecond : key === 'p95' ? r.p95.value : null);
  return [...rows].sort((a, b) => {
    if (key !== 'name') {
      const av = val(a);
      const bv = val(b);
      if (av === null && bv !== null) return 1;
      if (bv === null && av !== null) return -1;
      if (av !== null && bv !== null && av !== bv) return bv - av;
      const ar = a.requestsPerSecond ?? -1;
      const br = b.requestsPerSecond ?? -1;
      if (ar !== br) return br - ar;
    }
    return `${displayName(a)} ${a.environment}`.localeCompare(`${displayName(b)} ${b.environment}`);
  });
}

export { displayName };
