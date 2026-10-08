// log 검색 조건 ↔ URL query ↔ filter AST (D05 §08, F04 공유 URL, ADR 0037·0045).
// URL에는 형식이 정해진 값만 공유한다: service(UUID), sev(최소 심각도 이름), trace(hex 32).
// 본문 검색어(q)는 자유 입력이라 공유 링크에서 빠진다(context.ts SHAREABLE).
import type { FilterNode, LogItem } from '../../api/types.ts';
import { MAX_FUTURE_SKEW_MS } from '../../app/context.ts';

export const MAX_LOG_RANGE_MS = 24 * 60 * 60_000; // ADR 0037 §3
export const LOG_PAGE = 100; // ADR 0037 기본 limit
export const MAX_QUERY_LENGTH = 200; // 서버 한도 1,024자보다 작게(입력 실수 방지)

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const TRACE_ID = /^[0-9a-f]{32}$/;
const ZERO_TRACE = '0'.repeat(32);

// OTel log data model SeverityNumber 구간: 1~4 TRACE, 5~8 DEBUG, 9~12 INFO, 13~16 WARN, 17~20 ERROR, 21~24 FATAL. 0은 미지정.
export const SEVERITY_LEVELS = [
  { key: 'trace', label: 'TRACE', min: 1 },
  { key: 'debug', label: 'DEBUG', min: 5 },
  { key: 'info', label: 'INFO', min: 9 },
  { key: 'warn', label: 'WARN', min: 13 },
  { key: 'error', label: 'ERROR', min: 17 },
  { key: 'fatal', label: 'FATAL', min: 21 },
] as const;

export type SeverityKey = (typeof SEVERITY_LEVELS)[number]['key'];

/** severity_number → 이름. 0(미지정)이나 범위 밖은 이름을 지어내지 않는다. */
export function severityLabel(n: number): string {
  if (!Number.isInteger(n) || n < 1 || n > 24) return '미지정';
  let label = 'TRACE';
  for (const l of SEVERITY_LEVELS) if (n >= l.min) label = l.label;
  return label;
}

export function isErrorSeverity(n: number): boolean {
  return n >= 17 && n <= 24;
}

export interface LogFilterState {
  serviceId: string | null;
  minSeverity: SeverityKey | null;
  traceId: string | null;
  query: string;
}

export const EMPTY_LOG_FILTER: LogFilterState = { serviceId: null, minSeverity: null, traceId: null, query: '' };

export function isTraceId(v: string): boolean {
  return TRACE_ID.test(v) && v !== ZERO_TRACE;
}

/** 잘못된 값은 무시한다(조건 없음). */
export function parseLogFilter(params: URLSearchParams): LogFilterState {
  const service = params.get('service');
  const sev = params.get('sev');
  const trace = params.get('trace')?.toLowerCase() ?? null;
  const q = params.get('q') ?? '';
  return {
    serviceId: service !== null && UUID.test(service) ? service : null,
    minSeverity: SEVERITY_LEVELS.find((l) => l.key === sev)?.key ?? null,
    traceId: trace !== null && isTraceId(trace) ? trace : null,
    query: q.length <= MAX_QUERY_LENGTH ? q : '',
  };
}

export function writeLogFilter(params: URLSearchParams, f: LogFilterState): URLSearchParams {
  const next = new URLSearchParams(params);
  for (const k of ['service', 'sev', 'trace', 'q', 'entity']) next.delete(k);
  if (f.serviceId !== null) next.set('service', f.serviceId);
  if (f.minSeverity !== null) next.set('sev', f.minSeverity);
  if (f.traceId !== null) next.set('trace', f.traceId);
  if (f.query.trim() !== '') next.set('q', f.query.trim());
  return next;
}

/** filter AST(ADR 0037 log catalog). 조건이 없으면 undefined(전체). */
export function toLogFilterNode(f: LogFilterState): FilterNode | undefined {
  const args: FilterNode[] = [];
  if (f.serviceId !== null) args.push({ op: 'eq', field: 'service_id', value: f.serviceId });
  if (f.minSeverity !== null) {
    const min = SEVERITY_LEVELS.find((l) => l.key === f.minSeverity)?.min ?? 1;
    args.push({ op: 'gte', field: 'severity_number', value: min });
  }
  if (f.traceId !== null) args.push({ op: 'eq', field: 'trace_id', value: f.traceId });
  if (f.query.trim() !== '') args.push({ op: 'contains', field: 'body', value: f.query.trim() });
  if (args.length === 0) return undefined;
  if (args.length === 1) return args[0];
  return { op: 'and', args };
}

/**
 * log에서 trace 상세(S05)로 갈 때의 조회 범위. log 시각만 알고 trace 시작·끝은 모른다.
 * log는 보통 span 안에서 남으므로 앞쪽을 넉넉히(1시간), 뒤쪽은 10분 둔다. 단건 조회 한도(7일, ADR 0022) 안이다.
 * 끝은 지금 + 4분을 넘지 않는다. 조사 context는 5분 넘게 미래인 끝 시각을 거절하고(MAX_FUTURE_SKEW_MS),
 * 수집은 5분 넘게 미래인 log를 거절하므로 log 시각이 브라우저 시계보다 조금 앞서도 링크가 유효하다.
 */
const TRACE_LINK_FUTURE_MARGIN_MS = MAX_FUTURE_SKEW_MS - 60_000;
export const TRACE_FROM_LOG_BEFORE_MS = 60 * 60_000;
export const TRACE_FROM_LOG_AFTER_MS = 10 * 60_000;

export function traceWindowFromLog(timeIso: string, nowMs: number): { from: string; to: string } {
  const t = Date.parse(timeIso);
  const to = Math.min(t + TRACE_FROM_LOG_AFTER_MS, nowMs + TRACE_LINK_FUTURE_MARGIN_MS);
  return { from: new Date(t - TRACE_FROM_LOG_BEFORE_MS).toISOString(), to: new Date(to).toISOString() };
}

/** S05 링크 query: 조사 context + log 시각 기준 범위. span이 있으면 그 span의 로그 탭을 연다. */
export function traceLinkSearch(contextSearch: string, log: LogItem, nowMs: number): string {
  const q = new URLSearchParams(contextSearch);
  const w = traceWindowFromLog(log.time, nowMs);
  q.delete('range');
  q.set('from', w.from);
  q.set('to', w.to);
  if (log.span_id !== null) {
    q.set('entity', log.span_id);
    q.set('tab', 'logs');
  }
  return `?${q.toString()}`;
}

export interface ServiceOption {
  service_id: string;
  name: string;
  namespace: string;
  environment: string;
}

export function serviceLabel(s: ServiceOption): string {
  return s.namespace === '' ? s.name : `${s.namespace}/${s.name}`;
}
