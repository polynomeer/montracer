import { apiRequest, type ApiResult } from './client.ts';
import type { ServiceItem } from './types.ts';

// GET /api/v1/services/{service_id} (ADR 0042). 없는 것과 볼 수 없는 것은 같은 404다.
export function getService(serviceId: string, signal: AbortSignal): Promise<ApiResult<ServiceItem>> {
  return apiRequest<ServiceItem>(`/services/${encodeURIComponent(serviceId)}`, { signal });
}

export interface ServiceListParams {
  environment: string | null;
  cursor: string | null;
  limit: number;
}

// GET /api/v1/services (ADR 0038)
export function listServices(params: ServiceListParams, signal: AbortSignal): Promise<ApiResult<ServiceItem[]>> {
  const q = new URLSearchParams({ limit: String(params.limit) });
  if (params.environment !== null) q.set('environment', params.environment);
  if (params.cursor !== null) q.set('cursor', params.cursor);
  return apiRequest<ServiceItem[]>(`/services?${q.toString()}`, { signal });
}
