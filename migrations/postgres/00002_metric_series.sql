-- metric 활성 series 등록부 (D02 §10 cardinality quota, ADR 0029)
-- 여러 ingress replica가 공유하는 "이미 받은 series" 목록이다. 한도를 넘어도 기존 series는 계속 받고 신규만 거절하려면
-- replica 사이에 같은 답이 필요하다. 활성 = 최근 1시간 안에 last_seen. 오래된 행은 정리 대상이지만 판정에는 영향이 없다.
-- 값은 stream fingerprint(128-bit)와 metric 이름뿐이다. 속성 값(PII 가능)은 저장하지 않는다.

-- +goose Up
CREATE TABLE metric_series (
  tenant_id   uuid        NOT NULL REFERENCES tenants (id),
  stream_id   bytea       NOT NULL CHECK (octet_length(stream_id) = 16),
  metric_name text        NOT NULL CHECK (char_length(metric_name) <= 255),
  first_seen  timestamptz NOT NULL,
  last_seen   timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, stream_id)
);
CREATE INDEX metric_series_active ON metric_series (tenant_id, last_seen);

ALTER TABLE metric_series ENABLE ROW LEVEL SECURITY;
ALTER TABLE metric_series FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON metric_series
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());

-- ingress: 조회·등록·last_seen 갱신, 오래된 행 정리
GRANT SELECT, INSERT, DELETE ON metric_series TO montracer_rw;
GRANT UPDATE (last_seen) ON metric_series TO montracer_rw;

-- +goose Down
DROP TABLE metric_series;
