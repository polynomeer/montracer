-- dashboard 정의 (D02 §11·§14, D05 §10~11, ADR 0057)
--   dashboards: 현재 정의(정규형 DashboardSpec). 낙관적 잠금(revision, If-Match)과 삭제 tombstone(D02 §11 "optimistic locking, 삭제 tombstone").
--   query는 저장만 한다 — 실행은 보는 사람의 권한으로 query-api가 한다.

-- +goose Up
CREATE TABLE dashboards (
  tenant_id        uuid        NOT NULL REFERENCES tenants (id),
  id               uuid        NOT NULL,
  title            text        NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
  title_normalized text        GENERATED ALWAYS AS (lower(title)) STORED,
  -- 정규형 DashboardSpec(internal/dashboard). 위젯 수는 목록에 보이려고 따로 둔다(spec과 같은 트랜잭션에서 쓴다)
  spec             jsonb       NOT NULL,
  widget_count     integer     NOT NULL CHECK (widget_count BETWEEN 0 AND 50),
  revision         bigint      NOT NULL DEFAULT 1 CHECK (revision > 0),
  created_by       text        NOT NULL CHECK (char_length(created_by) BETWEEN 1 AND 256),
  updated_by       text        NOT NULL CHECK (char_length(updated_by) BETWEEN 1 AND 256),
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  deleted_at       timestamptz,
  PRIMARY KEY (tenant_id, id)
);
CREATE INDEX dashboards_by_title ON dashboards (tenant_id, title_normalized, id) WHERE deleted_at IS NULL;

ALTER TABLE dashboards ENABLE ROW LEVEL SECURITY;
ALTER TABLE dashboards FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON dashboards
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());

-- 최소 권한: 지우기는 tombstone(DELETE 권한 없음), tenant·id·만든 사람은 바꾸지 않는다
GRANT SELECT, INSERT ON dashboards TO montracer_rw;
GRANT UPDATE (title, spec, widget_count, revision, updated_by, updated_at, deleted_at) ON dashboards TO montracer_rw;

-- +goose Down
DROP TABLE dashboards;
