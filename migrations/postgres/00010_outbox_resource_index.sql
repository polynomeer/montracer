-- monitor timeline의 상태 전이 조회(ADR 0054): outbox에서 resource 하나의 event를 시간 범위로 읽는다.
-- outbox는 tenant의 모든 감사·전이 event가 쌓이므로 이 index 없이는 tenant 전체를 훑는다.
-- 운영 중 쓰기를 막지 않게 CONCURRENTLY로 만든다(트랜잭션 밖).

-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS outbox_resource_time ON outbox (tenant_id, type, resource_id, occurred_at);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS outbox_resource_time;
