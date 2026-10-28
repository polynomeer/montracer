// control-api 서비스 상태 (ADR 0073). monitor 정의에서 서비스 연결을 도출해 서비스마다 가장 나쁜 상태를 준다.
// - 걸린 monitor가 없는 서비스는 응답에 없다. meta.warnings에 잘림(monitors_truncated·groups_truncated)이 있으면 일부만 반영했다.
// - flag가 꺼졌거나 monitor API가 꺼진 배포는 404다.
import type { Schema } from '@montracer/query-schema';
import { apiRequest, type ApiResult } from './client.ts';

export type ServiceHealthItem = Schema<'ServiceHealth'>;
export type ServiceHealthMonitor = Schema<'ServiceHealthMonitor'>;
export interface ServiceHealthData {
  services: ServiceHealthItem[];
  unattributed: ServiceHealthMonitor[];
}

export function getServiceHealth(environment: string | null, signal: AbortSignal): Promise<ApiResult<ServiceHealthData>> {
  const q = environment === null ? '' : `?environment=${encodeURIComponent(environment)}`;
  return apiRequest<ServiceHealthData>(`/service-health${q}`, { signal });
}
