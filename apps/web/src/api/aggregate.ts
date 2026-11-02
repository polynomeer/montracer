// query-api 집계 (ADR 0074). 검색과 같은 range·filter로 저장된 trace·log 총 수, facet 값별 수, 시간 히스토그램을 준다.
// - trace 수는 tail sampling을 거친 표본의 수다(meta.sampled=true). 요청 수·오류율로 쓰지 않는다(계약 5).
// - facet은 그 facet이 소유한 조건을 빼고 센다(excludes_own_filter) — 여러 값을 고를 수 있게.
// - 칸의 count가 null이면 reason(future)이다. facet의 values가 null이면 reason(too_many_values·budget_exceeded·timeout)이다.
//   어느 쪽도 0으로 바꾸지 않는다(계약 6).
// - flag가 꺼진 배포는 404다(화면은 건수 없이 보인다).
import type { Schema } from '@montracer/query-schema';
import { apiRequest, type ApiResult } from './client.ts';
import type { FilterNode } from './types.ts';

export type AggregateData = Schema<'AggregateData'>;
export type AggregateFacet = Schema<'AggregateFacet'>;
export type AggregateBucket = Schema<'AggregateBucket'>;

export interface AggregateRequest {
  range: { from: string; to: string };
  filter?: FilterNode;
  facets?: { field: string; top_n?: number }[];
  histogram?: boolean;
}

export function aggregate(signal: 'traces' | 'logs', body: AggregateRequest, abort: AbortSignal): Promise<ApiResult<AggregateData>> {
  return apiRequest<AggregateData>(`/query/${signal}/aggregate`, { method: 'POST', body, signal: abort });
}
