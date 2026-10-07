import { apiRequest, type ApiResult } from './client.ts';
import type { SearchRequest, TraceSummary } from './types.ts';

// POST /api/v1/query/traces (D02 §13, ADR 0043)
export function searchTraces(body: SearchRequest, signal: AbortSignal): Promise<ApiResult<TraceSummary[]>> {
  return apiRequest<TraceSummary[]>('/query/traces', { method: 'POST', body, signal });
}
