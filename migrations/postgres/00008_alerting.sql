-- monitor 평가 실행 경로 (D02 §11·§17, ADR 0051)
--   monitor_schedule   : 평가 대상·주기·마지막 slot·lease. alert-worker가 tenant를 가로질러 "할 일"을 찾는 유일한 표면이다.
--                        spec 본문은 없다(id·주기·active·revision·마지막 slot·lease뿐). 가로 조회는 app.monitor_scan='on'일 때 SELECT만 열린다(key_lookup과 같은 방식).
--   monitor_evaluations: (tenant, monitor, slot)마다 평가 기록. 같은 slot을 두 번 평가하지 않는다(PK = idempotency key).
--   alert_instances    : group별 경보 상태. 상태 전이는 같은 트랜잭션의 outbox(alert.state_changed)와 함께 쓴다.

-- +goose Up
CREATE TABLE monitor_schedule (
  tenant_id          uuid        NOT NULL,
  monitor_id         uuid        NOT NULL,
  evaluation_seconds integer     NOT NULL CHECK (evaluation_seconds IN (30, 60, 300)),
  -- enabled이고 삭제되지 않았을 때만 평가한다
  active             boolean     NOT NULL,
  revision           bigint      NOT NULL CHECK (revision > 0),
  -- 마지막으로 끝낸 평가 slot의 시작(slot = floor(epoch / evaluation_seconds) × evaluation_seconds)
  last_slot          timestamptz,
  -- lease: 평가 중인 claim의 token(claim마다 새 UUID)과 만료 시각. 만료되면 다른 worker가 가져간다(crash 복구).
  -- 완료는 같은 token일 때만 된다(fencing). worker 이름은 monitor_evaluations.worker에 남긴다
  lease_token        uuid,
  lease_until        timestamptz,
  PRIMARY KEY (tenant_id, monitor_id),
  FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id)
);
CREATE INDEX monitor_schedule_due ON monitor_schedule (last_slot NULLS FIRST) WHERE active;

CREATE TABLE monitor_evaluations (
  tenant_id     uuid        NOT NULL,
  monitor_id    uuid        NOT NULL,
  slot_start    timestamptz NOT NULL,
  revision      bigint      NOT NULL CHECK (revision > 0),
  -- evaluated | no_data(확정 window 없음) | error(조회 실패·group 상한)
  status        text        NOT NULL CHECK (status IN ('evaluated', 'no_data', 'error')),
  reason        text        CHECK (reason IS NULL OR char_length(reason) <= 64),
  window_start  timestamptz,
  window_end    timestamptz,
  groups        integer     NOT NULL CHECK (groups >= 0),
  alerting      integer     NOT NULL CHECK (alerting >= 0),
  worker        text        NOT NULL CHECK (char_length(worker) BETWEEN 1 AND 200),
  duration_ms   integer     NOT NULL CHECK (duration_ms >= 0),
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, monitor_id, slot_start),
  FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id)
);

CREATE TABLE alert_instances (
  tenant_id       uuid             NOT NULL,
  monitor_id      uuid             NOT NULL,
  -- group key의 SHA-256 hex(D02 §11 group_hash). key 원문은 labels로 남긴다
  group_hash      text             NOT NULL CHECK (char_length(group_hash) = 64),
  labels          jsonb            NOT NULL,
  -- 이 상태를 만든 monitor revision. revision이 바뀌면 상태를 새로 시작한다(ADR 0051)
  revision        bigint           NOT NULL CHECK (revision > 0),
  state           text             NOT NULL CHECK (state IN ('OK', 'PENDING', 'ALERT', 'RECOVERING', 'NO_DATA', 'EVALUATION_ERROR')),
  -- 열린 사건(episode). 닫히면 NULL. 알림 중복 억제 key(tenant+monitor+group+episode_id, D02 §17)
  episode_id      uuid,
  episode_reason  text             CHECK (episode_reason IS NULL OR episode_reason IN ('violation', 'no_data')),
  violation_since timestamptz,
  nodata_since    timestamptz,
  ok_streak       integer          NOT NULL DEFAULT 0 CHECK (ok_streak >= 0),
  last_window_end timestamptz,
  last_value      double precision,
  last_reason     text             CHECK (last_reason IS NULL OR char_length(last_reason) <= 64),
  version         bigint           NOT NULL DEFAULT 1 CHECK (version > 0),
  updated_at      timestamptz      NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, monitor_id, group_hash),
  FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id),
  CHECK ((episode_id IS NULL) = (episode_reason IS NULL))
);

ALTER TABLE monitor_schedule ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON monitor_schedule
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());
-- 평가 대상 찾기: tenant 없이 SELECT만. 수정은 tenant 트랜잭션에서만 된다(위 정책).
CREATE POLICY monitor_scan ON monitor_schedule FOR SELECT
  USING (current_setting('app.monitor_scan', true) = 'on');

ALTER TABLE monitor_evaluations ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON monitor_evaluations
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());

ALTER TABLE alert_instances ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON alert_instances
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());

GRANT SELECT, INSERT ON monitor_schedule TO montracer_rw;
GRANT UPDATE (evaluation_seconds, active, revision, last_slot, lease_token, lease_until) ON monitor_schedule TO montracer_rw;
-- 평가 기록은 추가와 보존 기간(7일) 정리만
GRANT SELECT, INSERT, DELETE ON monitor_evaluations TO montracer_rw;
GRANT SELECT, INSERT, UPDATE, DELETE ON alert_instances TO montracer_rw;

-- 이미 있는 monitor의 schedule (00007 이후 만든 정의).
-- migration은 owner 계정으로 돈다(ADR 0016). monitors는 FORCE RLS라 owner도 tenant 없이는 0행을 본다 → 이 문장 동안만 FORCE를 푼다.
-- 새 표의 FORCE도 backfill 뒤에 건다. 빠진 monitor가 있으면 migration을 실패시킨다(조용히 평가되지 않는 정의가 남지 않게).
ALTER TABLE monitors NO FORCE ROW LEVEL SECURITY;
INSERT INTO monitor_schedule (tenant_id, monitor_id, evaluation_seconds, active, revision)
SELECT tenant_id, id, (spec->>'evaluation_seconds')::integer, enabled AND deleted_at IS NULL, revision FROM monitors;
-- +goose StatementBegin
DO $$
BEGIN
  IF (SELECT count(*) FROM monitors) <> (SELECT count(*) FROM monitor_schedule) THEN
    RAISE EXCEPTION 'monitor_schedule backfill incomplete';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE monitors FORCE ROW LEVEL SECURITY;
ALTER TABLE monitor_schedule FORCE ROW LEVEL SECURITY;
ALTER TABLE monitor_evaluations FORCE ROW LEVEL SECURITY;
ALTER TABLE alert_instances FORCE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE alert_instances;
DROP TABLE monitor_evaluations;
DROP TABLE monitor_schedule;
