-- metric label 활성 값 등록부 (D02 §10 "key당 활성 값 100개", ADR 0030)
-- (tenant, metric, label key)별로 최근 1시간 안에 본 값의 해시만 둔다. 값 자체는 저장하지 않는다.
-- 값은 redaction(ADR 0019)을 거친 뒤의 값이다.

-- +goose Up
CREATE TABLE metric_label_values (
  tenant_id   uuid        NOT NULL REFERENCES tenants (id),
  metric_name text        NOT NULL CHECK (char_length(metric_name) <= 255),
  label_key   text        NOT NULL CHECK (char_length(label_key) <= 255),
  value_hash  bytea       NOT NULL CHECK (octet_length(value_hash) = 16),
  first_seen  timestamptz NOT NULL,
  last_seen   timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, metric_name, label_key, value_hash)
);
CREATE INDEX metric_label_values_active ON metric_label_values (tenant_id, metric_name, label_key, last_seen);

ALTER TABLE metric_label_values ENABLE ROW LEVEL SECURITY;
ALTER TABLE metric_label_values FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON metric_label_values
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());

GRANT SELECT, INSERT, DELETE ON metric_label_values TO montracer_rw;
GRANT UPDATE (last_seen) ON metric_label_values TO montracer_rw;

-- +goose Down
DROP TABLE metric_label_values;
