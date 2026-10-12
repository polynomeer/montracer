-- 경보 알림 (D02 §17·§20, D04 §03, ADR 0053)
--   notification_policies       : webhook 목적지와 서명 secret. URL·secret은 암호문이다(URL path·query에 receiver token이 흔하다).
--                                  삭제는 tombstone(보낸 알림 기록이 참조한다). 살아 있는 monitor가 가리키면 지울 수 없다(409).
--   notification_deliveries     : 보낼 알림 하나(= webhook event_id)의 상태. 경보 전이와 같은 트랜잭션에 쓴다(D02 §17).
--                                  중복 억제 key = tenant + monitor + group + episode + 종류(UNIQUE). 전송은 lease를 잡은 worker 하나가 한다.
--                                  tenant를 가로지르는 scan이 이 표를 본다 — 그래서 본문(labels·값)은 여기 없다.
--   notification_delivery_bodies: 알림 본문. tenant 정책만 있다(scan으로 보이지 않는다).

-- +goose Up
CREATE TABLE notification_policies (
  tenant_id         uuid        NOT NULL REFERENCES tenants (id),
  id                uuid        NOT NULL,
  name              text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
  name_normalized   text        GENERATED ALWAYS AS (lower(name)) STORED,
  -- 화면·감사용 목적지(scheme·host만). 전체 URL은 암호문이다(HTTPS만은 저장 전에 검사한다, D02 §17).
  -- 사설 주소 차단은 보낼 때 실제 연결 주소로 다시 검사한다(DNS rebinding)
  webhook_display   text        NOT NULL CHECK (char_length(webhook_display) BETWEEN 9 AND 300),
  webhook_url_sealed bytea      NOT NULL,
  -- 서명 secret은 AES-256-GCM 암호문이다(nonce 포함). 평문은 만들 때 응답으로 한 번만 준다.
  -- secret_key_id는 receiver가 secret을 고르는 공개 id(서명 header). *_sealing_key는 각 암호문의 master key 버전이다
  -- (URL은 수정 때 현재 버전으로 다시 암호화되고 secret은 그대로라 버전이 다를 수 있다)
  url_sealing_key    text       NOT NULL CHECK (char_length(url_sealing_key) BETWEEN 1 AND 32),
  secret_key_id      text       NOT NULL CHECK (char_length(secret_key_id) BETWEEN 1 AND 64),
  secret_ciphertext  bytea      NOT NULL,
  secret_sealing_key text       NOT NULL CHECK (char_length(secret_sealing_key) BETWEEN 1 AND 32),
  enabled           boolean     NOT NULL,
  revision          bigint      NOT NULL DEFAULT 1 CHECK (revision > 0),
  created_by        text        NOT NULL CHECK (char_length(created_by) BETWEEN 1 AND 256),
  updated_by        text        NOT NULL CHECK (char_length(updated_by) BETWEEN 1 AND 256),
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  deleted_at        timestamptz,
  PRIMARY KEY (tenant_id, id)
);
CREATE INDEX notification_policies_name ON notification_policies (tenant_id, name_normalized, id) WHERE deleted_at IS NULL;
-- monitor가 가리키는 policy(spec의 notification_policy_id와 같은 값). 열로 두어 같은 tenant의 policy만 가리키게 하고(FK),
-- policy를 지울 때 가리키는 살아 있는 monitor를 찾는다(있으면 409). 이전 정의에는 policy가 없었으므로 backfill은 없다
ALTER TABLE monitors ADD COLUMN notification_policy_id uuid;
ALTER TABLE monitors ADD CONSTRAINT monitors_notification_policy_fk
  FOREIGN KEY (tenant_id, notification_policy_id) REFERENCES notification_policies (tenant_id, id);
CREATE INDEX monitors_notification_policy ON monitors (tenant_id, notification_policy_id) WHERE deleted_at IS NULL AND notification_policy_id IS NOT NULL;
GRANT UPDATE (notification_policy_id) ON monitors TO montracer_rw;

CREATE TABLE notification_deliveries (
  tenant_id        uuid        NOT NULL,
  id               uuid        NOT NULL,
  policy_id        uuid        NOT NULL,
  monitor_id       uuid        NOT NULL,
  group_hash       text        NOT NULL CHECK (char_length(group_hash) = 64),
  episode_id       uuid        NOT NULL,
  event_type       text        NOT NULL CHECK (event_type IN ('alert.opened', 'alert.resolved')),
  status           text        NOT NULL CHECK (status IN ('pending', 'delivered', 'failed', 'canceled')),
  -- 전송 시도 수. lease를 잡을 때 늘린다(보낸 뒤 결과를 쓰기 전에 죽어도 다음 시도의 번호가 늘어 재전송이 드러난다)
  attempts         integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  next_attempt_at  timestamptz NOT NULL,
  lease_token      uuid,
  lease_until      timestamptz,
  last_status_code integer,
  last_error       text        CHECK (last_error IS NULL OR char_length(last_error) <= 64),
  created_at       timestamptz NOT NULL,
  finished_at      timestamptz,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, policy_id, monitor_id, group_hash, episode_id, event_type),
  FOREIGN KEY (tenant_id, policy_id) REFERENCES notification_policies (tenant_id, id),
  FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id),
  CHECK ((status = 'pending') = (finished_at IS NULL))
);
CREATE INDEX notification_deliveries_due ON notification_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX notification_deliveries_episode ON notification_deliveries (tenant_id, monitor_id, group_hash, episode_id);
CREATE INDEX notification_deliveries_policy ON notification_deliveries (tenant_id, policy_id);
CREATE INDEX notification_deliveries_finished ON notification_deliveries (finished_at) WHERE finished_at IS NOT NULL;

CREATE TABLE notification_delivery_bodies (
  tenant_id   uuid  NOT NULL,
  delivery_id uuid  NOT NULL,
  -- 보낼 envelope bytes. 재시도에도 같은 본문을 보낸다(서명은 시도마다 새 timestamp)
  body        bytea NOT NULL CHECK (octet_length(body) <= 65536),
  PRIMARY KEY (tenant_id, delivery_id),
  FOREIGN KEY (tenant_id, delivery_id) REFERENCES notification_deliveries (tenant_id, id) ON DELETE CASCADE
);

ALTER TABLE notification_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notification_policies
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());

ALTER TABLE notification_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notification_deliveries
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());
-- 보낼 알림 찾기: tenant 없이 SELECT만(monitor_scan과 같은 방식, ADR 0051). 이 표에는 id·종류·시각·시도 수·결과 code만 있다
-- (본문은 notification_delivery_bodies, URL·secret은 policy의 암호문). 상태 변경은 tenant 정책(위)만 허용한다.
CREATE POLICY delivery_scan ON notification_deliveries FOR SELECT
  USING (current_setting('app.delivery_scan', true) = 'on');

ALTER TABLE notification_delivery_bodies ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_delivery_bodies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notification_delivery_bodies
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());

GRANT SELECT, INSERT ON notification_policies TO montracer_rw;
-- secret 열은 바꿀 수 없다(교체는 후속 단계). 삭제는 tombstone
GRANT UPDATE (name, webhook_display, webhook_url_sealed, url_sealing_key, enabled, revision, updated_by, updated_at, deleted_at) ON notification_policies TO montracer_rw;
GRANT SELECT, INSERT, DELETE ON notification_deliveries TO montracer_rw;
GRANT UPDATE (status, attempts, next_attempt_at, lease_token, lease_until, last_status_code, last_error, finished_at) ON notification_deliveries TO montracer_rw;
-- 본문은 쓰고 읽기만(지우기는 알림 행 삭제의 CASCADE)
GRANT SELECT, INSERT ON notification_delivery_bodies TO montracer_rw;

-- +goose Down
DROP INDEX monitors_notification_policy;
ALTER TABLE monitors DROP CONSTRAINT monitors_notification_policy_fk;
ALTER TABLE monitors DROP COLUMN notification_policy_id;
DROP TABLE notification_delivery_bodies;
DROP TABLE notification_deliveries;
DROP TABLE notification_policies;
