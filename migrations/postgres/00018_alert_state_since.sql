-- 상태 시작 시각 (ADR 0073 §3). group이 지금 상태가 된 시각: 판정 window 끝, window 없는 판정이면 평가 시각.
-- 행을 처음 넣을 때와 상태가 바뀔 때만 쓴다. 기존 행은 NULL(모름)이다 — 다음 상태 변화나 revision 변경에서 채워진다.
-- montracer_rw는 이미 표 전체 UPDATE 권한이 있다(00008). expand-only.

-- +goose Up
ALTER TABLE alert_instances ADD COLUMN state_since timestamptz;

-- +goose Down
ALTER TABLE alert_instances DROP COLUMN state_since;
