-- 제어 DB 초기 스키마: tenants, memberships, api_keys, audit_events, outbox (D02 §11, ADR 0015, 0016)
-- 모든 tenant 테이블은 ENABLE + FORCE RLS, 앱은 montracer_rw 그룹 권한으로만 접근한다.

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'montracer_rw') THEN
    CREATE ROLE montracer_rw NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  END IF;
END
$$;
-- +goose StatementEnd

-- 현재 요청의 tenant. 설정되지 않았으면 NULL → 모든 정책이 거짓(fail closed).
CREATE FUNCTION app_tenant_id() RETURNS uuid
  LANGUAGE sql STABLE
  AS $$ SELECT nullif(current_setting('app.tenant_id', true), '')::uuid $$;

CREATE TABLE tenants (
  id             uuid        PRIMARY KEY,
  region         text        NOT NULL CHECK (length(region) BETWEEN 1 AND 64),
  cell           text        NOT NULL CHECK (length(cell) BETWEEN 1 AND 64),
  -- D04 §12 고객 lifecycle
  status         text        NOT NULL CHECK (status IN ('trial','active','past_due','restricted','suspended','closing','deleted')),
  policy_version bigint      NOT NULL DEFAULT 1 CHECK (policy_version > 0),
  created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
  tenant_id  uuid        NOT NULL REFERENCES tenants (id),
  user_id    text        NOT NULL CHECK (length(user_id) BETWEEN 1 AND 256),
  role       text        NOT NULL CHECK (role IN ('viewer','developer','operator','tenant_admin','security_auditor')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, user_id)
);

CREATE TABLE api_keys (
  tenant_id    uuid        NOT NULL REFERENCES tenants (id),
  key_id       text        NOT NULL CHECK (key_id ~ '^[0-9a-f]{16}$'),
  kind         text        NOT NULL CHECK (kind IN ('ingest','api')),
  hash         bytea       NOT NULL CHECK (length(hash) = 32),
  scopes       text[]      NOT NULL CHECK (cardinality(scopes) > 0),
  environments text[]      NOT NULL DEFAULT '{}',
  -- 발급자 user_id. FK를 두지 않는다: 발급자 제거가 key 삭제로 번지면 안 되고,
  -- 제거된 발급자의 API key는 인증 시 무효가 된다 (ADR 0015 §1).
  issued_by    text        NOT NULL CHECK (length(issued_by) BETWEEN 1 AND 256),
  expires_at   timestamptz NOT NULL,
  revoked_at   timestamptz,
  -- 변경마다 증가. outbox event의 revision으로 쓴다 (D02 §14 이벤트 스키마).
  revision     bigint      NOT NULL DEFAULT 1 CHECK (revision > 0),
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, key_id),
  -- 인증은 tenant를 모른 채 key_id로 조회하므로 전역 고유여야 한다.
  CONSTRAINT api_keys_key_id_unique UNIQUE (key_id),
  -- ingest key는 environment scope 필수 (D04 §02)
  CONSTRAINT api_keys_ingest_env CHECK (kind <> 'ingest' OR cardinality(environments) > 0)
);

CREATE TABLE audit_events (
  tenant_id   uuid        NOT NULL REFERENCES tenants (id),
  id          uuid        NOT NULL,
  occurred_at timestamptz NOT NULL DEFAULT now(),
  -- ADR 0015 §4: operations는 Operator도 조회, security는 audit.read 전용
  category    text        NOT NULL CHECK (category IN ('operations','security')),
  action      text        NOT NULL CHECK (length(action) BETWEEN 1 AND 128),
  actor_kind  text        NOT NULL CHECK (actor_kind IN ('user','api_key','ingest_key','system')),
  actor_id    text        NOT NULL CHECK (length(actor_id) BETWEEN 1 AND 256),
  resource_type text      NOT NULL CHECK (length(resource_type) BETWEEN 1 AND 64),
  resource_id text        NOT NULL CHECK (length(resource_id) BETWEEN 1 AND 256),
  request_id  text        CHECK (request_id IS NULL OR length(request_id) <= 128),
  -- 본문 PII·secret 금지 (D04 §04 보안 audit)
  details     jsonb       NOT NULL DEFAULT '{}'::jsonb,
  PRIMARY KEY (tenant_id, id)
);
CREATE INDEX audit_events_time ON audit_events (tenant_id, occurred_at DESC);

CREATE TABLE outbox (
  tenant_id      uuid        NOT NULL REFERENCES tenants (id),
  event_id       uuid        NOT NULL,
  type           text        NOT NULL CHECK (length(type) BETWEEN 1 AND 128),
  resource_id    text        NOT NULL CHECK (length(resource_id) BETWEEN 1 AND 256),
  revision       bigint      NOT NULL CHECK (revision > 0),
  occurred_at    timestamptz NOT NULL DEFAULT now(),
  actor_id       text        NOT NULL,
  schema_version integer     NOT NULL CHECK (schema_version > 0),
  payload        jsonb       NOT NULL DEFAULT '{}'::jsonb,
  dispatched_at  timestamptz,
  PRIMARY KEY (tenant_id, event_id)
);
CREATE INDEX outbox_pending ON outbox (occurred_at) WHERE dispatched_at IS NULL;

ALTER TABLE tenants      ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants      FORCE ROW LEVEL SECURITY;
ALTER TABLE memberships  ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships  FORCE ROW LEVEL SECURITY;
ALTER TABLE api_keys     ENABLE ROW LEVEL SECURITY;
ALTER TABLE api_keys     FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events FORCE ROW LEVEL SECURITY;
ALTER TABLE outbox       ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox       FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON tenants
  USING (id = app_tenant_id()) WITH CHECK (id = app_tenant_id());
CREATE POLICY tenant_isolation ON memberships
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());
CREATE POLICY tenant_isolation ON api_keys
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());
-- 인증 조회: tenant를 모르는 상태에서 정확한 key_id 한 행만 읽는다 (ADR 0016 §4). SELECT 전용.
CREATE POLICY key_lookup ON api_keys FOR SELECT
  USING (key_id = nullif(current_setting('app.key_lookup', true), ''));
CREATE POLICY tenant_isolation ON audit_events
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());
CREATE POLICY tenant_isolation ON outbox
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());

GRANT EXECUTE ON FUNCTION app_tenant_id() TO montracer_rw;
-- tenant 생성·region/cell·lifecycle 변경은 provisioning workflow 전용이다 (D02 §11, D04 §12). 앱은 조회만.
GRANT SELECT ON tenants TO montracer_rw;
GRANT SELECT, INSERT, UPDATE, DELETE ON memberships TO montracer_rw;
-- key는 발급(INSERT)과 폐기(revoked_at, revision)만 바꿀 수 있다. hash·scope·발급자는 불변.
GRANT SELECT, INSERT ON api_keys TO montracer_rw;
GRANT UPDATE (revoked_at, revision) ON api_keys TO montracer_rw;
-- 감사는 append-only
GRANT SELECT, INSERT ON audit_events TO montracer_rw;
GRANT SELECT, INSERT ON outbox TO montracer_rw;

-- +goose Down
DROP TABLE outbox;
DROP TABLE audit_events;
DROP TABLE api_keys;
DROP TABLE memberships;
DROP TABLE tenants;
DROP FUNCTION app_tenant_id();
-- montracer_rw role은 다른 DB·환경과 공유될 수 있어 삭제하지 않는다.
