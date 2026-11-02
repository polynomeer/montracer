-- resource 속성 열 (ADR 0075 §1). worker가 PII 제거 뒤의 resource 속성에서 채운다. 없는 값은 ''이고 조회 API는 null로 보인다.
-- expand-only: 기존 행은 '' 그대로 만료된다(backfill 없음). query 계정은 표 전체 SELECT라 GRANT를 더하지 않는다.
-- 순서: 이 migration 먼저, 그다음 새 worker(열이 없으면 새 worker는 기동하지 않는다, ADR 0075 §4).
-- 운영(Replicated + Distributed)은 local 표 → Distributed 표 순서로 ON CLUSTER 적용한다(RB01).

-- +goose NO TRANSACTION
-- +goose Up

ALTER TABLE spans_local
  ADD COLUMN IF NOT EXISTS service_version     LowCardinality(String) DEFAULT '',
  ADD COLUMN IF NOT EXISTS service_instance_id String DEFAULT '',
  ADD COLUMN IF NOT EXISTS k8s_namespace_name  LowCardinality(String) DEFAULT '',
  ADD COLUMN IF NOT EXISTS k8s_pod_name        String DEFAULT '';

ALTER TABLE logs_local
  ADD COLUMN IF NOT EXISTS service_version     LowCardinality(String) DEFAULT '',
  ADD COLUMN IF NOT EXISTS service_instance_id String DEFAULT '',
  ADD COLUMN IF NOT EXISTS k8s_namespace_name  LowCardinality(String) DEFAULT '',
  ADD COLUMN IF NOT EXISTS k8s_pod_name        String DEFAULT '';

-- +goose Down
-- 열 삭제는 데이터를 지우고, 실행 중인 새 worker의 insert를 깨뜨린다. 운영에서는 쓰지 않는다(RB01).
ALTER TABLE logs_local
  DROP COLUMN IF EXISTS k8s_pod_name,
  DROP COLUMN IF EXISTS k8s_namespace_name,
  DROP COLUMN IF EXISTS service_instance_id,
  DROP COLUMN IF EXISTS service_version;

ALTER TABLE spans_local
  DROP COLUMN IF EXISTS k8s_pod_name,
  DROP COLUMN IF EXISTS k8s_namespace_name,
  DROP COLUMN IF EXISTS service_instance_id,
  DROP COLUMN IF EXISTS service_version;
