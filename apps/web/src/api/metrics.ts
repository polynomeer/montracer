import { apiRequest, type ApiResult } from './client.ts';
import type { MetricQuery, MetricResult } from './types.ts';

// POST /api/v1/query/metrics (ADR 0027, 0028, 0042)
export function queryMetrics(query: MetricQuery, signal: AbortSignal): Promise<ApiResult<MetricResult>> {
  return apiRequest<MetricResult>('/query/metrics', { method: 'POST', body: query, signal });
}
