// 시험 fixture 공용 조각. 서버 응답과 같은 모양이어야 한다 — setup의 consumer contract 검사가 원천과 맞춘다(ADR 0059 §4).

/** 조회 API meta(D02 §19). 계산하지 않는 값은 서버처럼 null이다. */
export function queryMeta(over: Partial<QueryMetaFixture> = {}): QueryMetaFixture {
  return {
    request_id: 'req-1',
    schema_version: 1,
    partial: false,
    failed_shards: [],
    watermark: null,
    sampled: null,
    coverage: null,
    resolution_seconds: null,
    scan_bytes: null,
    warnings: [],
    ...over,
  };
}

export interface QueryMetaFixture {
  request_id: string;
  schema_version: number;
  partial: boolean;
  failed_shards: string[];
  watermark: string | null;
  sampled: boolean | null;
  coverage: number | null;
  resolution_seconds: number | null;
  scan_bytes: number | null;
  warnings: string[];
}

/** metric 조회 data(D02 §13). range는 서버가 step에 맞춘 실제 구간을 돌려준다 — 시험은 고정 값이면 된다. */
export function metricData(series: unknown[], sourceWindowSeconds: 60 | 3600 = 60) {
  return {
    series,
    source_window_seconds: sourceWindowSeconds,
    range: { from: '2026-10-04T05:00:00Z', to: '2026-10-04T06:00:00Z' },
  };
}

/** 관리 API meta(조회·쓰기 공통, ADR 0059 §2). */
export function controlMeta(over: { request_id?: string; warnings?: string[] } = {}) {
  return { request_id: 'req-c', schema_version: 1, warnings: [] as string[], ...over };
}

/** 일부러 원천을 벗어난 응답(모르는 enum 값에 견디는지 보는 시험 등)에 단다 — consumer contract 검사가 건너뛴다. */
export const OFF_CONTRACT_HEADER = 'X-Test-Off-Contract';

/** 서비스 catalog 항목(D02 §08). service_id는 UUID, 시각은 RFC3339다. */
export function serviceFixture(over: { name: string; service_id?: string; environment?: string; namespace?: string } & Record<string, unknown>) {
  return {
    service_id: 'aaaaaaaa-0000-4000-8000-000000000001',
    namespace: '',
    environment: 'prod',
    language: null,
    status: 'active',
    first_seen: '2026-10-01T00:00:00Z',
    last_seen: '2026-10-04T05:59:00Z',
    owner_team: null,
    repository_url: null,
    runbook_url: null,
    tier: null,
    tags: [] as string[],
    ...over,
  };
}
