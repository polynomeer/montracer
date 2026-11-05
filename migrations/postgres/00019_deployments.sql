-- 배포 이벤트 (D02 §14, D03 §12, ADR 0076).
--   서비스는 자연 키(namespace·이름·환경)로 저장한다 — 첫 telemetry보다 먼저 올 수 있고 이름이 바뀌어도 기록이 남아야 한다.
--   기록한 사람은 저장하지 않는다(감사 deployment.recorded에만). 자유 텍스트(설명·메시지)도 없다(D03 §12, D04 §03).
--   보존 395일(metric 1시간 rollup과 같다). 조회는 expires_at을 predicate로 걸고, 지우기는 그 tenant의 쓰기 트랜잭션에서 한다.

-- +goose Up
CREATE TABLE deployments (
  tenant_id               uuid        NOT NULL REFERENCES tenants (id),
  id                      uuid        NOT NULL,
  service_namespace       text        NOT NULL DEFAULT '' CHECK (octet_length(service_namespace) <= 255),
  service_name            text        NOT NULL CHECK (octet_length(service_name) BETWEEN 1 AND 255),
  service_name_normalized text        GENERATED ALWAYS AS (lower(service_name)) STORED,
  -- 수집 key의 environment scope 상한과 같다(authz maxEnvironmentLen)
  environment             text        NOT NULL CHECK (octet_length(environment) BETWEEN 1 AND 64),
  version                 text        NOT NULL CHECK (octet_length(version) BETWEEN 1 AND 255),
  -- git sha(소문자 hex 7~64자). 없으면 ''(unique가 NULL을 서로 다르게 보지 않게)
  commit_sha              text        NOT NULL DEFAULT '' CHECK (commit_sha = '' OR commit_sha ~ '^[0-9a-f]{7,64}$'),
  occurred_at             timestamptz NOT NULL,
  recorded_at             timestamptz NOT NULL,
  source                  text        NOT NULL DEFAULT 'api' CHECK (source IN ('api')),
  expires_at              timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, id)
);
-- 같은 값은 한 행(재시도가 표시선을 겹쳐 그리지 않게). 이름은 대소문자를 무시한다.
CREATE UNIQUE INDEX deployments_natural
  ON deployments (tenant_id, service_namespace, service_name_normalized, environment, version, commit_sha, occurred_at);
CREATE INDEX deployments_by_service ON deployments (tenant_id, service_name_normalized, occurred_at DESC, id DESC);
-- 분당 한도 셈과 만료 지우기
CREATE INDEX deployments_by_recorded ON deployments (tenant_id, recorded_at);
CREATE INDEX deployments_by_expiry ON deployments (tenant_id, expires_at);

ALTER TABLE deployments ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON deployments
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());

-- 최소 권한: 행은 바꾸지 않는다. DELETE는 보존이 지난 행을 지우는 데만 쓴다.
GRANT SELECT, INSERT, DELETE ON deployments TO montracer_rw;

-- +goose Down
DROP TABLE deployments;
