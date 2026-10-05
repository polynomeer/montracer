-- metric 1시간 rollup (D02 §10 "metric_1h: 1시간 395일, 장기 추세", ADR 0028)
-- metric_1m과 같은 구조·같은 계산(ADR 0025)이며 window만 1시간이다. rollup job이 원본에서 직접 계산한다.

-- +goose NO TRANSACTION
-- +goose Up

CREATE TABLE metric_1h AS metric_1m
ENGINE = ReplacingMergeTree(revision)
PARTITION BY toYYYYMM(window_start)
ORDER BY (tenant_id, metric_name, stream_id, window_start)
TTL expires_at DELETE
SETTINGS non_replicated_deduplication_window = 1000;

-- rollup: 쓰기 + 진행 위치 3컬럼 읽기, 전 tenant 명시 정책 (ADR 0026 §3과 같다)
GRANT INSERT ON metric_1h TO montracer_ch_rollup;
GRANT SELECT(tenant_id, window_start, revision) ON metric_1h TO montracer_ch_rollup;
CREATE ROW POLICY rollup_all_tenants ON metric_1h FOR SELECT USING 1 TO montracer_ch_rollup;

-- query service: 읽기 + tenant row policy (ADR 0018)
GRANT SELECT ON metric_1h TO montracer_ch_query;
CREATE ROW POLICY tenant_isolation ON metric_1h FOR SELECT USING tenant_id = toUUIDOrZero(getSetting('SQL_montracer_tenant')) TO montracer_ch_query;

-- +goose Down
REVOKE SELECT, INSERT ON metric_1h FROM montracer_ch_rollup;
REVOKE SELECT ON metric_1h FROM montracer_ch_query;
DROP ROW POLICY IF EXISTS tenant_isolation ON metric_1h;
DROP ROW POLICY IF EXISTS rollup_all_tenants ON metric_1h;
DROP TABLE IF EXISTS metric_1h;
