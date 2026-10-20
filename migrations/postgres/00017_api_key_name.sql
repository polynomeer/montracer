-- API key 이름 (ADR 0069). 관리자가 key를 구분하는 이름(예: "checkout-prod collector"). 선택, 1–64자.
-- 기존 key는 NULL이다. 이름은 바꾸지 않는다(UPDATE 권한 없음) — 바꾸려면 새 key를 발급하고 이전 key를 폐기한다.

-- +goose Up
ALTER TABLE api_keys ADD COLUMN name text CHECK (name IS NULL OR length(name) BETWEEN 1 AND 64);

-- +goose Down
ALTER TABLE api_keys DROP COLUMN name;
