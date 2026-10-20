-- 조직 표시 이름 (ADR 0065): 화면의 조직 전환 목록과 URL 조직 이름(/o/{org})에 쓴다.
-- provisioning(owner)만 바꾼다 — 앱 role은 SELECT만(00001과 같다). 비면 화면은 tenant id를 쓴다.

-- +goose Up
ALTER TABLE tenants ADD COLUMN display_name text
  CHECK (display_name IS NULL OR display_name ~ '^[a-z0-9][a-z0-9-]{0,62}$');
CREATE UNIQUE INDEX tenants_display_name ON tenants (display_name) WHERE display_name IS NOT NULL;

-- +goose Down
DROP INDEX tenants_display_name;
ALTER TABLE tenants DROP COLUMN display_name;
