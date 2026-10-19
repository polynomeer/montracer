// query-api 응답 타입 (D02 §19 공통 응답, ADR 0027 metric, ADR 0038 catalog).
// 서버가 계산하지 않은 값은 null이다. null을 0으로 바꾸지 않는다 (계약 6).

import type { Schema } from '@montracer/query-schema';

// 응답 타입은 OpenAPI 원천에서 생성한 타입을 쓴다(@montracer/query-schema, ADR 0061). 손으로 옮기지 않는다.

/** 조회 API(query-api) meta (D02 §19). 계산하지 않는 값은 null이다. */
export type QueryMeta = Schema<'QueryMeta'>;
/** 관리 API(control-api) meta. */
export type ControlMeta = Schema<'ControlMeta'>;
/** monitor group 목록 meta(목록이 한도에서 잘렸는지). */
export type MonitorGroupsMeta = Schema<'MonitorGroupsMeta'>;
/** 응답 meta. 공통 field(request_id·schema_version·warnings) 밖은 API마다 다르다 — 아래 helper로 읽는다. */
export type Meta = QueryMeta | ControlMeta | MonitorGroupsMeta;

/** 일부 저장소가 응답하지 않았는지(조회 API만). 모르면 false가 아니라 판단하지 않는다 — 관리 API meta에는 없다. */
export function metaPartial(m: Meta | null | undefined): boolean {
  return m !== null && m !== undefined && 'partial' in m && m.partial;
}

/** 수집 watermark(조회 API만, 모르면 null). */
export function metaWatermark(m: Meta | null | undefined): string | null {
  return m !== null && m !== undefined && 'watermark' in m ? m.watermark : null;
}

/** 목록이 한도에서 잘렸는지(monitor group 목록). */
export function metaTruncated(m: Meta | null | undefined): boolean {
  return m !== null && m !== undefined && 'truncated' in m && m.truncated;
}

export type ServiceItem = Schema<'Service'>;

export type MetricSeries = Schema<'MetricSeries'>;
export type MetricPoint = MetricSeries['points'][number];
export type PointReason = NonNullable<MetricPoint['reason']>;

export type MetricAggregation = Schema<'MetricAggregation'>;

export interface MetricFilter {
  op: 'and' | 'eq';
  field?: string;
  value?: string;
  args?: MetricFilter[];
}

export interface MetricQuery {
  range: { from: string; to: string };
  step_seconds: number;
  expression: {
    metric: string;
    aggregation: MetricAggregation;
    filter?: MetricFilter;
    group_by?: string[];
  };
}

/** GET /api/v1/metrics 항목의 조합 (ADR 0046). aggregations가 비면 rollup 조회가 지원하지 않는 유형이다. */
export type MetricVariant = Schema<'MetricItem'>['variants'][number];

/** GET /api/v1/metrics 항목. conflict는 한 이름에 조합이 둘 이상(계측 충돌)이라는 뜻이다. */
export type MetricDescriptor = Schema<'MetricItem'>;

/** GET /api/v1/metrics/labels 결과 (ADR 0046). */
export interface MetricLabels {
  metric: string;
  keys: Schema<'MetricLabelItem'>[];
}

/** metric 조회 data. source_window_seconds는 실제로 읽은 rollup 해상도, range는 step 경계로 맞춘 실제 구간이다. */
export type MetricResult = Schema<'MetricQueryResponse'>['data'];

/** POST /api/v1/query/traces 결과 행(기본 projection은 모든 field, ADR 0043). complete는 구조 완결성(D02 §22)이고 meta.partial과 다르다. */
export type TraceSummary = Required<Schema<'TraceRow'>>;

/** query filter AST (D02 §15, ADR 0037). */
export interface FilterNode {
  op: 'and' | 'or' | 'eq' | 'neq' | 'in' | 'exists' | 'gt' | 'gte' | 'lt' | 'lte' | 'contains';
  field?: string;
  value?: string | number | boolean | (string | number)[];
  args?: FilterNode[];
}

export interface SearchRequest {
  range: { from: string; to: string };
  filter?: FilterNode;
  limit?: number;
  cursor?: string;
}

/** 오류 envelope(원천의 Error). code에는 client가 붙이는 NETWORK·BAD_RESPONSE·HTTP_<status>도 있다. */
export type ApiErrorBody = Omit<Schema<'Error'>, 'code'> & { code: Schema<'Error'>['code'] | ClientErrorCode };
/** 서버 응답이 아니라 화면이 만드는 오류 code(연결 실패·형식 오류·envelope 없는 HTTP 오류·호출 전 검증). */
export type ClientErrorCode = 'NETWORK' | 'BAD_RESPONSE' | 'CLIENT' | `HTTP_${string}`;

/** GET /api/v1/traces/{trace_id} span (ADR 0022). 시각은 RFC3339 UTC, 길이는 ns. */
export type TraceSpanItem = Schema<'TraceSpan'>;

export type TraceDetail = Schema<'Trace'>;

/** POST /api/v1/query/logs 행(기본 projection은 모든 field, ADR 0037). 없는 trace·span ID는 null. */
export type LogItem = Required<Schema<'LogRow'>>;
