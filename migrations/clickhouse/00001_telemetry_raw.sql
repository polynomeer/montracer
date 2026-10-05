-- 분석 저장소 원본 테이블과 접근 통제 (D02 §09~10, D04 §01, ADR 0003, 0018)
-- 로컬·단일 node용 ReplacingMergeTree다. production은 Replicated* + shard별 Distributed read table을
-- 배포 템플릿으로 만든다 (D02 §09). TTL은 즉시 차단 수단이 아니므로 query가 expires_at도 필터한다.

-- +goose NO TRANSACTION
-- +goose Up

CREATE ROLE IF NOT EXISTS montracer_ch_ingest;
CREATE ROLE IF NOT EXISTS montracer_ch_query;
-- lookup MV 전용 definer. 로그인 불가(HOST NONE), 원본 일부 컬럼 SELECT와 lookup INSERT만 갖는다.
-- 관리자 계정을 definer로 두면 MV가 전권으로 실행되고, 관리자 회전 시 모든 span insert가 실패한다.
CREATE USER IF NOT EXISTS montracer_lookup_definer IDENTIFIED WITH no_password HOST NONE;

CREATE TABLE spans_local (
  tenant_id      UUID,
  service_id     UUID,
  trace_id       FixedString(16),
  span_id        FixedString(8),
  parent_span_id FixedString(8),
  name           String,
  event_time     DateTime64(9, 'UTC'),
  duration_ns    UInt64,
  received_at    DateTime64(3, 'UTC'),
  status         UInt8,
  span_kind      UInt8,
  attributes     Map(String, String),
  payload        String,
  payload_hash   FixedString(32),
  version        UInt64,
  expires_at     DateTime('UTC')
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toDate(event_time)
ORDER BY (tenant_id, service_id, event_time, trace_id, span_id)
TTL expires_at DELETE;

CREATE TABLE logs_local (
  tenant_id   UUID,
  service_id  UUID,
  event_id    String,
  event_time  DateTime64(9, 'UTC'),
  severity    UInt8,
  trace_id    FixedString(16),
  span_id     FixedString(8),
  body        String,
  attributes  Map(String, String),
  version     UInt64,
  expires_at  DateTime('UTC')
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toDate(event_time)
ORDER BY (tenant_id, service_id, event_time, event_id)
TTL expires_at DELETE;

-- trace 단건 조회용 lookup: trace_id로 필요한 날짜·서비스만 찾는다 (D02 §09, §15).
CREATE TABLE trace_lookup (
  tenant_id  UUID,
  trace_id   FixedString(16),
  event_date Date,
  service_id UUID,
  expires_at DateTime('UTC')
) ENGINE = ReplacingMergeTree(expires_at)
PARTITION BY event_date
ORDER BY (tenant_id, trace_id, event_date, service_id)
TTL expires_at DELETE;

-- SQL SECURITY DEFINER: insert하는 ingest 계정에 원본 SELECT를 주지 않고(전 tenant 읽기 방지)
-- 전용 definer 권한으로 lookup을 채운다.
GRANT SELECT(tenant_id, trace_id, event_time, service_id, expires_at) ON spans_local TO montracer_lookup_definer;
GRANT INSERT ON trace_lookup TO montracer_lookup_definer;
CREATE MATERIALIZED VIEW trace_lookup_mv TO trace_lookup
DEFINER = montracer_lookup_definer SQL SECURITY DEFINER AS
SELECT tenant_id, trace_id, toDate(event_time) AS event_date, service_id, max(expires_at) AS expires_at
FROM spans_local
GROUP BY tenant_id, trace_id, event_date, service_id;

CREATE TABLE metric_points (
  tenant_id       UUID,
  stream_id       FixedString(16),
  metric_name     LowCardinality(String),
  unit            LowCardinality(String),
  type            Enum8('gauge' = 1, 'sum' = 2, 'histogram' = 3, 'exponential_histogram' = 4, 'summary' = 5),
  temporality     Enum8('unspecified' = 0, 'delta' = 1, 'cumulative' = 2),
  is_monotonic    Bool,
  start_time      DateTime64(9, 'UTC'),
  end_time        DateTime64(9, 'UTC'),
  point_hash      FixedString(16),
  value           Float64,
  count           UInt64,
  sum             Float64,
  bounds          Array(Float64),
  buckets         Array(UInt64),
  -- exponential histogram(scale·offset·양/음 bucket)과 summary quantile의 원본 (D02 §10 "별도 payload로 보존")
  payload         String,
  resource_json   String,
  attributes_json String,
  version         UInt64,
  expires_at      DateTime('UTC')
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toDate(end_time)
ORDER BY (tenant_id, metric_name, stream_id, end_time, point_hash)
TTL expires_at DELETE;

-- 수집 worker: 원본 테이블에 쓰기만 (lookup은 materialized view가 채운다)
GRANT INSERT ON spans_local TO montracer_ch_ingest;
GRANT INSERT ON logs_local TO montracer_ch_ingest;
GRANT INSERT ON metric_points TO montracer_ch_ingest;

-- query service: 읽기만. 사용자에게는 DB 자격 증명을 발급하지 않는다 (D02 §02).
GRANT SELECT ON spans_local TO montracer_ch_query;
GRANT SELECT ON logs_local TO montracer_ch_query;
GRANT SELECT ON trace_lookup TO montracer_ch_query;
GRANT SELECT ON metric_points TO montracer_ch_query;

-- tenant 기본값은 빈 문자열 → toUUIDOrZero('')는 zero UUID → 0행 (fail closed).
-- readonly=2: 쓰기·DDL 금지, 요청별 설정(tenant, 실행 예산)은 허용하되 예산은 MAX 제약을 넘을 수 없다.
-- 기본값은 interactive 예산, MAX는 비동기 job 예산이다 (D02 §08, §15). memory는 명세에 없는 설계 가정.
CREATE SETTINGS PROFILE IF NOT EXISTS montracer_query_profile
  SETTINGS SQL_montracer_tenant = '', readonly = 2,
           max_execution_time = 5 MAX 60,
           max_bytes_to_read = 10000000000 MAX 100000000000,
           max_result_rows = 10000 MAX 1000000,
           max_memory_usage = 2000000000 MAX 8000000000,
           timeout_overflow_mode = 'throw' CONST,
           read_overflow_mode = 'throw' CONST,
           result_overflow_mode = 'throw' CONST
  TO montracer_ch_query;

-- 방어 계층: query role은 요청의 tenant 행만 본다. query compiler의 mandatory predicate와 별개로 적용된다.
CREATE ROW POLICY tenant_isolation ON spans_local   FOR SELECT USING tenant_id = toUUIDOrZero(getSetting('SQL_montracer_tenant')) TO montracer_ch_query;
CREATE ROW POLICY tenant_isolation ON logs_local    FOR SELECT USING tenant_id = toUUIDOrZero(getSetting('SQL_montracer_tenant')) TO montracer_ch_query;
CREATE ROW POLICY tenant_isolation ON trace_lookup  FOR SELECT USING tenant_id = toUUIDOrZero(getSetting('SQL_montracer_tenant')) TO montracer_ch_query;
CREATE ROW POLICY tenant_isolation ON metric_points FOR SELECT USING tenant_id = toUUIDOrZero(getSetting('SQL_montracer_tenant')) TO montracer_ch_query;

-- +goose Down
DROP ROW POLICY IF EXISTS tenant_isolation ON metric_points;
DROP ROW POLICY IF EXISTS tenant_isolation ON trace_lookup;
DROP ROW POLICY IF EXISTS tenant_isolation ON logs_local;
DROP ROW POLICY IF EXISTS tenant_isolation ON spans_local;
DROP SETTINGS PROFILE IF EXISTS montracer_query_profile;
DROP VIEW IF EXISTS trace_lookup_mv;
DROP USER IF EXISTS montracer_lookup_definer;
DROP TABLE IF EXISTS metric_points;
DROP TABLE IF EXISTS trace_lookup;
DROP TABLE IF EXISTS logs_local;
DROP TABLE IF EXISTS spans_local;
-- role은 다른 DB·환경과 공유될 수 있어 삭제하지 않는다.
