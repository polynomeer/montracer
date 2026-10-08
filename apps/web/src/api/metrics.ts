import { apiRequest, type ApiResult } from './client.ts';
import type { MetricDescriptor, MetricLabels, MetricQuery, MetricResult } from './types.ts';

// POST /api/v1/query/metrics (ADR 0027, 0028, 0042)
export function queryMetrics(query: MetricQuery, signal: AbortSignal): Promise<ApiResult<MetricResult>> {
  return apiRequest<MetricResult>('/query/metrics', { method: 'POST', body: query, signal });
}

// GET /api/v1/metrics?from&to&q&limit&cursor (metric 사전, ADR 0046). 범위는 24시간까지.
export function listMetrics(
  params: { from: string; to: string; q: string; limit: number; cursor: string | null },
  signal: AbortSignal,
): Promise<ApiResult<MetricDescriptor[]>> {
  const q = new URLSearchParams({ from: params.from, to: params.to, limit: String(params.limit) });
  if (params.q !== '') q.set('q', params.q);
  if (params.cursor !== null) q.set('cursor', params.cursor);
  return apiRequest<MetricDescriptor[]>(`/metrics?${q.toString()}`, { signal });
}

// GET /api/v1/metrics/labels?metric&from&to (ADR 0046). key만 온다(값 목록 없음).
export function listMetricLabels(metric: string, from: string, to: string, signal: AbortSignal): Promise<ApiResult<MetricLabels>> {
  const q = new URLSearchParams({ metric, from, to });
  return apiRequest<MetricLabels>(`/metrics/labels?${q.toString()}`, { signal });
}
