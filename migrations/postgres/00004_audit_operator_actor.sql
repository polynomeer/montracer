-- 감사 actor에 플랫폼 운영자(break-glass)를 더한다 (D04 §01 "모든 조회도 감사", ADR 0033).
-- 운영자 접근은 대상 tenant의 security 감사에 남는다(고객용 감사 조회 API는 후속).

-- +goose Up
-- NOT VALID로 붙이고 따로 VALIDATE한다: 검증 중 감사 insert를 막는 강한 잠금을 짧게 한다(감사 테이블은 계속 커진다).
ALTER TABLE audit_events DROP CONSTRAINT audit_events_actor_kind_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_actor_kind_check
  CHECK (actor_kind IN ('user','api_key','ingest_key','system','operator')) NOT VALID;
ALTER TABLE audit_events VALIDATE CONSTRAINT audit_events_actor_kind_check;

-- +goose Down
-- 운영자 감사 행은 지우지 않는다(감사는 append-only). 기존 행은 검사하지 않고 새 행에만 원래 제약을 건다.
ALTER TABLE audit_events DROP CONSTRAINT audit_events_actor_kind_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_actor_kind_check
  CHECK (actor_kind IN ('user','api_key','ingest_key','system')) NOT VALID;
