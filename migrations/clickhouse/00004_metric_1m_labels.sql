-- metric_1m에 stream label을 함께 저장한다 (ADR 0027).
-- 원본 metric_points는 15일, metric_1m은 90일 보존이라(D02 §10) 원본 join으로는 오래된 rollup을 그룹화할 수 없다.

-- +goose NO TRANSACTION
-- +goose Up

ALTER TABLE metric_1m ADD COLUMN IF NOT EXISTS resource_json String DEFAULT '' AFTER unit;
ALTER TABLE metric_1m ADD COLUMN IF NOT EXISTS attributes_json String DEFAULT '' AFTER resource_json;
GRANT SELECT(resource_json, attributes_json) ON metric_points TO montracer_ch_rollup;

-- +goose Down
REVOKE SELECT(resource_json, attributes_json) ON metric_points FROM montracer_ch_rollup;
ALTER TABLE metric_1m DROP COLUMN IF EXISTS attributes_json;
ALTER TABLE metric_1m DROP COLUMN IF EXISTS resource_json;
