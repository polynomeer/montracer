-- monitor 정의와 Idempotency-Key 저장 (D02 §11·§14·§20, ADR 0049)
-- monitors는 현재 정의, monitor_revisions는 revision마다 바뀌지 않는 정규형 spec이다(D02 §11 "immutable evaluation version").
-- 평가(alert-worker, ADR 0049 후속)는 (monitor_id, revision)으로 정확한 spec을 가리킨다.
-- idempotency_keys는 모든 POST mutation의 재시도 안전성이다: tenant·principal·경로·key별 request hash와 응답을 24시간 보관한다.

-- +goose Up
CREATE TABLE monitors (
  tenant_id       uuid        NOT NULL REFERENCES tenants (id),
  id              uuid        NOT NULL,
  name            text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
  name_normalized text        GENERATED ALWAYS AS (lower(name)) STORED,
  -- 정규형 MonitorSpec(internal/monitor). 기본값을 모두 채운 명시적 JSON
  spec            jsonb       NOT NULL,
  enabled         boolean     NOT NULL,
  revision        bigint      NOT NULL DEFAULT 1 CHECK (revision > 0),
  created_by      text        NOT NULL CHECK (char_length(created_by) BETWEEN 1 AND 256),
  updated_by      text        NOT NULL CHECK (char_length(updated_by) BETWEEN 1 AND 256),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  -- 삭제는 tombstone이다. 평가 이력·감사가 참조하는 정의를 지우지 않는다.
  deleted_at      timestamptz,
  PRIMARY KEY (tenant_id, id)
);
CREATE INDEX monitors_by_name ON monitors (tenant_id, name_normalized, id) WHERE deleted_at IS NULL;

CREATE TABLE monitor_revisions (
  tenant_id  uuid        NOT NULL,
  monitor_id uuid        NOT NULL,
  revision   bigint      NOT NULL CHECK (revision > 0),
  spec       jsonb       NOT NULL,
  enabled    boolean     NOT NULL,
  -- deleted면 이 revision이 삭제다(평가 중지 지점)
  deleted    boolean     NOT NULL DEFAULT false,
  created_by text        NOT NULL CHECK (char_length(created_by) BETWEEN 1 AND 256),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, monitor_id, revision),
  -- 자식 FK도 tenant_id를 포함해 cross-tenant 참조를 막는다(D02 §11)
  FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id)
);

CREATE TABLE idempotency_keys (
  tenant_id     uuid        NOT NULL REFERENCES tenants (id),
  -- principal 종류·주체(예: api_key:<key_id>). 같은 key라도 다른 principal의 요청은 별개다
  principal     text        NOT NULL CHECK (char_length(principal) BETWEEN 1 AND 300),
  method_path   text        NOT NULL CHECK (char_length(method_path) BETWEEN 1 AND 300),
  key           text        NOT NULL CHECK (char_length(key) BETWEEN 1 AND 255),
  request_hash  bytea       NOT NULL CHECK (octet_length(request_hash) = 32),
  status_code   integer     NOT NULL CHECK (status_code BETWEEN 200 AND 599),
  response_body bytea       NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, principal, method_path, key)
);
CREATE INDEX idempotency_keys_expiry ON idempotency_keys (expires_at);

ALTER TABLE monitors ENABLE ROW LEVEL SECURITY;
ALTER TABLE monitors FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON monitors
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());
ALTER TABLE monitor_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE monitor_revisions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON monitor_revisions
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON idempotency_keys
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());

-- 최소 권한: 정의는 tombstone으로만 지우고(DELETE 권한 없음), revision 이력은 추가만 한다.
GRANT SELECT, INSERT ON monitors TO montracer_rw;
GRANT UPDATE (name, spec, enabled, revision, updated_by, updated_at, deleted_at) ON monitors TO montracer_rw;
GRANT SELECT, INSERT ON monitor_revisions TO montracer_rw;
-- 만료된 idempotency 행은 같은 key를 다시 쓸 때 지운다
GRANT SELECT, INSERT, DELETE ON idempotency_keys TO montracer_rw;

-- +goose Down
DROP TABLE idempotency_keys;
DROP TABLE monitor_revisions;
DROP TABLE monitors;
