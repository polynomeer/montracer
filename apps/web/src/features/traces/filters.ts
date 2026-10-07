// trace 검색 조건 ↔ URL query ↔ filter AST (D05 §06, F04 공유 URL, ADR 0043).
// URL에는 형식이 정해진 값만 둔다: service(UUID), errors(1), min_ms(정수), name(span 이름 일부, 200자 이하).
// 공유 링크에는 name을 빼고 나머지만 남는다(context.ts SHAREABLE) — 자유 입력이 링크로 새지 않게.
import type { FilterNode } from '../../api/types.ts';

export const MAX_TRACE_RANGE_MS = 24 * 60 * 60_000; // D02 §19
export const TRACE_PAGE = 100; // D05 §06
export const MAX_NAME_LENGTH = 200;
export const MAX_MIN_MS = 3 * 24 * 60 * 60_000; // queryplan.MaxTraceDurationMs

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

export interface TraceFilterState {
  serviceId: string | null;
  errorsOnly: boolean;
  minMs: number | null;
  name: string;
}

export const EMPTY_FILTER: TraceFilterState = { serviceId: null, errorsOnly: false, minMs: null, name: '' };

/** 잘못된 값은 무시한다(조건 없음). 화면은 입력값을 다시 보여주므로 사용자가 고칠 수 있다. */
export function parseTraceFilter(params: URLSearchParams): TraceFilterState {
  const service = params.get('service');
  const min = params.get('min_ms');
  const minMs = min !== null && /^[0-9]{1,9}$/.test(min) ? Number(min) : null;
  const name = params.get('name') ?? '';
  return {
    serviceId: service !== null && UUID.test(service) ? service : null,
    errorsOnly: params.get('errors') === '1',
    minMs: minMs !== null && minMs <= MAX_MIN_MS ? minMs : null,
    name: name.length <= MAX_NAME_LENGTH ? name : '',
  };
}

export function writeTraceFilter(params: URLSearchParams, f: TraceFilterState): URLSearchParams {
  const next = new URLSearchParams(params);
  for (const k of ['service', 'errors', 'min_ms', 'name']) next.delete(k);
  if (f.serviceId !== null) next.set('service', f.serviceId);
  if (f.errorsOnly) next.set('errors', '1');
  if (f.minMs !== null) next.set('min_ms', String(f.minMs));
  if (f.name.trim() !== '') next.set('name', f.name.trim());
  return next;
}

/** filter AST. 조건이 없으면 undefined(전체). span 조건(service_id·name)과 trace 요약 조건(has_error·duration_ms)은 서버가 나눈다. */
export function toFilterNode(f: TraceFilterState): FilterNode | undefined {
  const args: FilterNode[] = [];
  if (f.serviceId !== null) args.push({ op: 'eq', field: 'service_id', value: f.serviceId });
  if (f.name.trim() !== '') args.push({ op: 'contains', field: 'name', value: f.name.trim() });
  if (f.errorsOnly) args.push({ op: 'eq', field: 'has_error', value: true });
  if (f.minMs !== null) args.push({ op: 'gte', field: 'duration_ms', value: f.minMs });
  if (args.length === 0) return undefined;
  if (args.length === 1) return args[0];
  return { op: 'and', args };
}

/** trace 상세(S05) 링크 범위: trace 시작 1분 전 ~ 끝 1분 뒤 (단건 조회는 명시적 범위가 필요하다, ADR 0022). */
export function traceDetailWindow(startIso: string, durationMs: number): { from: string; to: string } {
  const start = Date.parse(startIso);
  return {
    from: new Date(start - 60_000).toISOString(),
    to: new Date(start + Math.ceil(durationMs) + 60_000).toISOString(),
  };
}

export const REASON_TEXT: Record<string, string> = {
  missing_root: 'root span 없음',
  missing_parent: '부모 span 누락',
  span_limit_reached: 'span 한도 도달',
};
