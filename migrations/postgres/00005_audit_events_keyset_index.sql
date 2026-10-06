-- 감사 조회 keyset pagination용 index (ADR 0034).
-- 정렬·위치 비교가 (occurred_at, id)라서 같은 시각의 행도 index 순서로 이어 읽는다.
-- 기존 audit_events_time (tenant_id, occurred_at DESC)는 이 index가 덮으므로 지운다.

-- +goose Up
CREATE INDEX audit_events_keyset ON audit_events (tenant_id, occurred_at DESC, id DESC);
DROP INDEX audit_events_time;

-- +goose Down
CREATE INDEX audit_events_time ON audit_events (tenant_id, occurred_at DESC);
DROP INDEX audit_events_keyset;
