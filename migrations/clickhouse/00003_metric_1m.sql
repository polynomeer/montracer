-- metric 1분 rollup (D02 §10, §22, ADR 0025, 0026)
-- worker의 rollup job이 dedup된 metric_points에서 window를 재계산해 revision과 함께 통째로 쓴다.
-- query는 같은 (tenant, stream, window_start)에서 revision이 가장 큰 행만 읽는다(일부 합산 결과를 노출하지 않음).

-- +goose NO TRANSACTION
-- +goose Up

CREATE ROLE IF NOT EXISTS montracer_ch_rollup;

CREATE TABLE metric_1m (
  tenant_id      UUID,
  metric_name    LowCardinality(String),
  stream_id      FixedString(16),
  window_start   DateTime('UTC'),
  type           Enum8('gauge' = 1, 'sum' = 2, 'histogram' = 3, 'exponential_histogram' = 4, 'summary' = 5),
  temporality    Enum8('unspecified' = 0, 'delta' = 1, 'cumulative' = 2),
  is_monotonic   Bool,
  unit           LowCardinality(String),
  samples        UInt32,
  -- 값 통계 (gauge, non-monotonic cumulative sum). has_value = 0이면 값 없음(0이 아님)
  has_value      Bool,
  last           Float64,
  min            Float64,
  max            Float64,
  total          Float64,
  -- 증가량 (monotonic sum, non-monotonic delta sum)
  has_increase   Bool,
  increase       Float64,
  -- histogram 상태 (bucket 병합 후 percentile, 계약 5)
  has_histogram  Bool,
  count          UInt64,
  hist_sum       Float64,
  bounds         Array(Float64),
  buckets        Array(UInt64),
  resets         UInt32,
  -- 품질 사유 (missing_baseline, reset, nan_value, …)와 불완전 여부 (계약 6)
  flags          Array(LowCardinality(String)),
  partial        Bool,
  revision       UInt64,
  computed_at    DateTime64(3, 'UTC'),
  expires_at     DateTime('UTC')
) ENGINE = ReplacingMergeTree(revision)
PARTITION BY toDate(window_start)
ORDER BY (tenant_id, metric_name, stream_id, window_start)
TTL expires_at DELETE
SETTINGS non_replicated_deduplication_window = 1000;

-- rollup job: 원본 metric 읽기 + 1분 rollup 쓰기. 다른 원본(span·log)과 query 결과 테이블은 읽지 못한다.
GRANT SELECT(tenant_id, stream_id, metric_name, unit, type, temporality, is_monotonic, start_time, end_time,
             point_hash, value, count, sum, bounds, buckets, version, expires_at) ON metric_points TO montracer_ch_rollup;
GRANT INSERT ON metric_1m TO montracer_ch_rollup;
-- 재시작 시 진행 위치·최대 revision 복원용으로 세 컬럼만 읽는다(집계 값은 읽지 못한다, ADR 0026 §3).
GRANT SELECT(tenant_id, window_start, revision) ON metric_1m TO montracer_ch_rollup;
-- 전 tenant를 읽는 시스템 job임을 정책으로 명시한다(ADR 0018 §3: SELECT를 주는 role에는 같은 migration에서 정책을 만든다).
CREATE ROW POLICY rollup_all_tenants ON metric_points FOR SELECT USING 1 TO montracer_ch_rollup;
CREATE ROW POLICY rollup_all_tenants ON metric_1m FOR SELECT USING 1 TO montracer_ch_rollup;

-- query service: 읽기 + tenant row policy (ADR 0018)
GRANT SELECT ON metric_1m TO montracer_ch_query;
CREATE ROW POLICY tenant_isolation ON metric_1m FOR SELECT USING tenant_id = toUUIDOrZero(getSetting('SQL_montracer_tenant')) TO montracer_ch_query;

-- +goose Down
-- 정책보다 권한을 먼저 거둔다(정책만 지우면 users_without_row_policies_can_read_rows 설정에 따라 읽기 범위가 달라진다).
REVOKE SELECT ON metric_points FROM montracer_ch_rollup;
REVOKE SELECT, INSERT ON metric_1m FROM montracer_ch_rollup;
REVOKE SELECT ON metric_1m FROM montracer_ch_query;
DROP ROW POLICY IF EXISTS rollup_all_tenants ON metric_1m;
DROP ROW POLICY IF EXISTS tenant_isolation ON metric_1m;
DROP ROW POLICY IF EXISTS rollup_all_tenants ON metric_points;
DROP TABLE IF EXISTS metric_1m;
-- role은 다른 DB·환경과 공유될 수 있어 삭제하지 않는다.
