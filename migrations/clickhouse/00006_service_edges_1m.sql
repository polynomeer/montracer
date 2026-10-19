-- service map edge 1분 집계 (D02 §08 Map 산출 규칙, §10 "service_edges_1m: 30일, service map", ADR 0062)
-- worker의 servicemap job이 dedup된 span 쌍에서 window를 재계산해 (tenant, window)마다 같은 revision으로 통째로 쓴다.
-- 늦은 span이 짝을 찾으면 같은 호출의 key(destination)가 바뀌므로, query는 (tenant, window)의 최대 revision 행만 읽는다.

-- +goose NO TRANSACTION
-- +goose Up

CREATE ROLE IF NOT EXISTS montracer_ch_servicemap;

CREATE TABLE service_edges_1m (
  tenant_id         UUID,
  window_start      DateTime('UTC'),
  source_service_id UUID,
  -- 상대 서비스 service_id(소문자 UUID) 또는 "db:<system>[/<namespace>]", "queue:<system>", "peer:<이름>", "host:<등록 domain>|ip"
  destination       String,
  destination_kind  Enum8('service' = 1, 'database' = 2, 'queue' = 3, 'external' = 4, 'peer' = 5),
  connection        Enum8('rpc' = 1, 'messaging' = 2),
  requests          UInt64,
  errors            UInt64,
  -- latency histogram(ms). bucket은 경계별 수, 마지막은 +Inf (percentile은 bucket 병합, 계약 5)
  bounds            Array(Float64),
  buckets           Array(UInt64),
  -- messaging 큐 대기(consumer 시작 − producer 끝) histogram. 같은 경계
  wait_count        UInt64,
  wait_buckets      Array(UInt64),
  last_seen         DateTime64(3, 'UTC'),
  revision          UInt64,
  computed_at       DateTime64(3, 'UTC'),
  expires_at        DateTime('UTC')
) ENGINE = ReplacingMergeTree(revision)
PARTITION BY toDate(window_start)
ORDER BY (tenant_id, window_start, source_service_id, destination, connection)
TTL expires_at DELETE
SETTINGS non_replicated_deduplication_window = 1000;

-- servicemap job: span 짝 계산에 필요한 컬럼만 읽는다(payload·name·received_at 없음). log·metric 원본은 읽지 못한다.
-- 전 tenant를 읽는 시스템 job임을 정책으로 명시한다(ADR 0018 §3).
GRANT SELECT(tenant_id, service_id, trace_id, span_id, parent_span_id, event_time, duration_ns, status, span_kind, attributes, expires_at)
  ON spans_local TO montracer_ch_servicemap;
CREATE ROW POLICY servicemap_all_tenants ON spans_local FOR SELECT USING 1 TO montracer_ch_servicemap;
GRANT INSERT ON service_edges_1m TO montracer_ch_servicemap;
-- 재시작 시 진행 위치·최대 revision 복원용으로 세 컬럼만 읽는다(집계 값은 읽지 못한다).
GRANT SELECT(tenant_id, window_start, revision) ON service_edges_1m TO montracer_ch_servicemap;
CREATE ROW POLICY servicemap_all_tenants ON service_edges_1m FOR SELECT USING 1 TO montracer_ch_servicemap;

-- query service: 읽기 + tenant row policy (ADR 0018)
GRANT SELECT ON service_edges_1m TO montracer_ch_query;
CREATE ROW POLICY tenant_isolation ON service_edges_1m FOR SELECT USING tenant_id = toUUIDOrZero(getSetting('SQL_montracer_tenant')) TO montracer_ch_query;

-- +goose Down
-- 정책보다 권한을 먼저 거둔다(정책만 지우면 users_without_row_policies_can_read_rows 설정에 따라 읽기 범위가 달라진다).
REVOKE SELECT ON spans_local FROM montracer_ch_servicemap;
REVOKE SELECT, INSERT ON service_edges_1m FROM montracer_ch_servicemap;
REVOKE SELECT ON service_edges_1m FROM montracer_ch_query;
DROP ROW POLICY IF EXISTS tenant_isolation ON service_edges_1m;
DROP ROW POLICY IF EXISTS servicemap_all_tenants ON service_edges_1m;
DROP ROW POLICY IF EXISTS servicemap_all_tenants ON spans_local;
DROP TABLE IF EXISTS service_edges_1m;
-- role은 다른 DB·환경과 공유될 수 있어 삭제하지 않는다.
