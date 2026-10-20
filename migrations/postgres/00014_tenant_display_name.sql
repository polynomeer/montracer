-- 조직 표시 이름 (ADR 0065): 화면의 조직 전환 목록과 URL 조직 이름(/o/{org})에 쓴다.
-- provisioning(owner)만 바꾼다 — 앱 role은 SELECT만(00001과 같다). 비면 화면은 tenant id를 쓴다.

-- +goose Up
-- UUID 모양은 받지 않는다: 이름이 없는 조직은 화면 URL에 tenant id를 쓰므로, 다른 조직의 이름과 겹치면 안 된다.
ALTER TABLE tenants ADD COLUMN display_name text
  CHECK (display_name IS NULL OR (display_name ~ '^[a-z0-9][a-z0-9-]{0,62}$'
         AND display_name !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'));
CREATE UNIQUE INDEX tenants_display_name ON tenants (display_name) WHERE display_name IS NOT NULL;

-- +goose Down
DROP INDEX tenants_display_name;
ALTER TABLE tenants DROP COLUMN display_name;
