-- 수집 worker sink 지원 (D02 §05, §09, ADR 0021)
-- 1. 단일 node(non-replicated) 테이블에서도 insert_deduplication_token이 동작하도록 dedup window를 켠다.
--    production Replicated* 테이블은 replicated_deduplication_window(기본값)를 쓴다.
-- 2. envelope를 해석할 수 없을 때의 quarantine. payload 원문은 저장하지 않고 크기·해시·Kafka 위치만 둔다.
--    재처리는 같은 보존 기간(24시간) 안의 Kafka offset으로 한다 (D04 §04).

-- +goose NO TRANSACTION
-- +goose Up

ALTER TABLE spans_local   MODIFY SETTING non_replicated_deduplication_window = 1000;
ALTER TABLE logs_local    MODIFY SETTING non_replicated_deduplication_window = 1000;
ALTER TABLE metric_points MODIFY SETTING non_replicated_deduplication_window = 1000;

CREATE TABLE ingest_quarantine (
  tenant_id       UUID,                      -- header에서 해석하지 못하면 zero UUID
  signal          LowCardinality(String),
  reason          LowCardinality(String),
  topic           LowCardinality(String),
  kafka_partition Int32,
  kafka_offset    Int64,
  event_id        String,
  schema_version  String,
  payload_bytes   UInt32,
  payload_sha256  FixedString(32),
  quarantined_at  DateTime64(3, 'UTC'),
  expires_at      DateTime('UTC')
) ENGINE = ReplacingMergeTree
PARTITION BY toDate(quarantined_at)
ORDER BY (topic, kafka_partition, kafka_offset)
TTL expires_at DELETE
SETTINGS non_replicated_deduplication_window = 1000;

-- 수집 worker는 쓰기만. query 계정에는 주지 않는다(운영 조회는 관리자).
GRANT INSERT ON ingest_quarantine TO montracer_ch_ingest;

-- +goose Down
REVOKE INSERT ON ingest_quarantine FROM montracer_ch_ingest;
DROP TABLE IF EXISTS ingest_quarantine;
ALTER TABLE metric_points RESET SETTING non_replicated_deduplication_window;
ALTER TABLE logs_local    RESET SETTING non_replicated_deduplication_window;
ALTER TABLE spans_local   RESET SETTING non_replicated_deduplication_window;
