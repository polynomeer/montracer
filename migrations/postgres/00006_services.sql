-- 서비스 catalog (D02 §08 Service Catalog, ADR 0038)
-- 자연 키 (tenant, environment, service.namespace, service.name). service_id는 worker가 원본 행에 쓰는 결정적 UUID와 같다
-- (pipeline.ServiceID) — spans_local·logs_local의 service_id와 바로 이어진다.
-- 자동 관측 필드(ingress가 쓴다)와 사용자 관리 필드(owner_team 등, 관리 API가 쓴다)를 나눈다.
-- 앱 role은 자동 필드만 INSERT·UPDATE할 수 있다 — agent 관측이 owner 정보를 쓰거나 덮어쓰지 않는다(D02 §08)를 권한으로 강제한다.
-- 사용자 관리 필드를 쓰는 관리 API는 별도 role로 붙인다(ADR 0038 §1).

-- +goose Up
CREATE TABLE services (
  tenant_id       uuid        NOT NULL REFERENCES tenants (id),
  service_id      uuid        NOT NULL,
  environment     text        NOT NULL CHECK (char_length(environment) <= 255),
  namespace       text        NOT NULL CHECK (char_length(namespace) <= 255),
  name            text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 255),
  -- 검색용 정규화(대소문자 보존 원본은 name)
  name_normalized text        GENERATED ALWAYS AS (lower(name)) STORED,
  language        text        CHECK (language IS NULL OR char_length(language) <= 64),
  first_seen      timestamptz NOT NULL,
  last_seen       timestamptz NOT NULL,
  -- 사용자 관리 필드 (관리 API 후속, 앱 role UPDATE 불가)
  owner_team      text        CHECK (owner_team IS NULL OR char_length(owner_team) <= 255),
  repository_url  text        CHECK (repository_url IS NULL OR char_length(repository_url) <= 2048),
  runbook_url     text        CHECK (runbook_url IS NULL OR char_length(runbook_url) <= 2048),
  tier            text        CHECK (tier IS NULL OR char_length(tier) <= 32),
  tags            text[]      NOT NULL DEFAULT '{}',
  PRIMARY KEY (tenant_id, service_id)
);
CREATE INDEX services_by_name ON services (tenant_id, name_normalized, environment, service_id);

ALTER TABLE services ENABLE ROW LEVEL SECURITY;
ALTER TABLE services FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON services
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());

-- ingress: 등록·자동 필드 갱신, query-api: 조회
GRANT SELECT ON services TO montracer_rw;
GRANT INSERT (tenant_id, service_id, environment, namespace, name, language, first_seen, last_seen) ON services TO montracer_rw;
GRANT UPDATE (last_seen, language) ON services TO montracer_rw;

-- +goose Down
DROP TABLE services;
