import { apiRequest, type ApiResult } from './client.ts';
import type { LogItem, SearchRequest, TraceDetail, TraceSummary } from './types.ts';

// POST /api/v1/query/traces (D02 §13, ADR 0043)
export function searchTraces(body: SearchRequest, signal: AbortSignal): Promise<ApiResult<TraceSummary[]>> {
  return apiRequest<TraceSummary[]>('/query/traces', { method: 'POST', body, signal });
}

// GET /api/v1/traces/{trace_id}?from&to (ADR 0022). 범위는 필수, 최대 7일. 보이는 span이 없으면 404다.
export function getTrace(traceId: string, from: string, to: string, signal: AbortSignal): Promise<ApiResult<TraceDetail>> {
  const q = new URLSearchParams({ from, to });
  return apiRequest<TraceDetail>(`/traces/${encodeURIComponent(traceId)}?${q.toString()}`, { signal });
}

// POST /api/v1/query/logs (ADR 0037)
export function searchLogs(body: SearchRequest, signal: AbortSignal): Promise<ApiResult<LogItem[]>> {
  return apiRequest<LogItem[]>('/query/logs', { method: 'POST', body, signal });
}
