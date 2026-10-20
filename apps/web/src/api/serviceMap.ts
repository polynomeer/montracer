import { apiRequest, type ApiResult } from './client.ts';
import type { Schema } from '@montracer/query-schema';

// POST /api/v1/service-map/query (ADR 0062). 응답 타입은 원천에서 생성한 타입이다(ADR 0061).
export type ServiceMapData = Schema<'ServiceMapResponse'>['data'];
export type MapNode = Schema<'MapNode'>;
export type MapEdge = Schema<'MapEdge'>;
export type MapLatency = Schema<'MapLatency'>;
export type ServiceMapRequest = Schema<'ServiceMapRequest'>;

export function queryServiceMap(req: ServiceMapRequest, signal: AbortSignal): Promise<ApiResult<ServiceMapData>> {
  return apiRequest<ServiceMapData>('/service-map/query', { method: 'POST', body: req, signal });
}
