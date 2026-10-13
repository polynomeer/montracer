-- 경보 알림 2단계 (D02 §17·§20, D05 §09, ADR 0055)
--   재알림: 열린 사건이 ALERT로 남아 있으면 30분마다, 최대 3번 다시 알린다. 저장 종류는 번호를 담아 alert.renotified.1..3이다
--           (webhook의 event_type은 alert.renotified, 번호는 payload.renotify_sequence). 종류에 번호가 있으므로 기존 중복 억제 key
--           (tenant + policy + monitor + group + episode + 종류)를 그대로 쓴다 — 이전 binary의 ON CONFLICT도 그대로 맞는다(확장만 하는 변경).
--   시험 전송: policy 하나에 보내는 notification.test. 경보와 무관하므로 monitor·group·사건이 없다(NULL — UNIQUE에서 서로 겹치지 않는다).
--   secret 교체: 새 secret으로 바꾸고 이전 secret을 정해진 시각까지 함께 서명한다(겹침 기간). 이전 secret도 암호문이다.

-- +goose Up
ALTER TABLE notification_deliveries
  ALTER COLUMN monitor_id DROP NOT NULL,
  ALTER COLUMN group_hash DROP NOT NULL,
  ALTER COLUMN episode_id DROP NOT NULL;
ALTER TABLE notification_deliveries DROP CONSTRAINT notification_deliveries_event_type_check;
ALTER TABLE notification_deliveries ADD CONSTRAINT notification_deliveries_event_type_check
  CHECK (event_type IN ('alert.opened', 'alert.resolved', 'alert.renotified.1', 'alert.renotified.2', 'alert.renotified.3', 'notification.test'));
-- 시험 전송만 monitor·group·사건이 없다
ALTER TABLE notification_deliveries ADD CONSTRAINT notification_deliveries_kind CHECK (
  (event_type = 'notification.test') = (monitor_id IS NULL)
  AND (monitor_id IS NULL) = (group_hash IS NULL)
  AND (monitor_id IS NULL) = (episode_id IS NULL));

-- 이전 서명 secret(교체 뒤 겹침 기간). 넷 다 있거나 넷 다 없다
ALTER TABLE notification_policies
  ADD COLUMN previous_secret_key_id      text CHECK (previous_secret_key_id IS NULL OR char_length(previous_secret_key_id) BETWEEN 1 AND 64),
  ADD COLUMN previous_secret_ciphertext  bytea,
  ADD COLUMN previous_secret_sealing_key text CHECK (previous_secret_sealing_key IS NULL OR char_length(previous_secret_sealing_key) BETWEEN 1 AND 32),
  ADD COLUMN previous_secret_expires_at  timestamptz;
ALTER TABLE notification_policies ADD CONSTRAINT notification_policies_previous_secret CHECK (
  (previous_secret_key_id IS NULL) = (previous_secret_ciphertext IS NULL)
  AND (previous_secret_key_id IS NULL) = (previous_secret_sealing_key IS NULL)
  AND (previous_secret_key_id IS NULL) = (previous_secret_expires_at IS NULL));
-- 교체 API가 secret 열을 바꾼다(그 밖의 경로는 secret 열을 쓰지 않는다 — 저장소 코드와 시험으로 지킨다)
GRANT UPDATE (secret_key_id, secret_ciphertext, secret_sealing_key,
  previous_secret_key_id, previous_secret_ciphertext, previous_secret_sealing_key, previous_secret_expires_at) ON notification_policies TO montracer_rw;

-- +goose Down
REVOKE UPDATE (secret_key_id, secret_ciphertext, secret_sealing_key,
  previous_secret_key_id, previous_secret_ciphertext, previous_secret_sealing_key, previous_secret_expires_at) ON notification_policies FROM montracer_rw;
ALTER TABLE notification_policies DROP CONSTRAINT notification_policies_previous_secret;
ALTER TABLE notification_policies
  DROP COLUMN previous_secret_key_id,
  DROP COLUMN previous_secret_ciphertext,
  DROP COLUMN previous_secret_sealing_key,
  DROP COLUMN previous_secret_expires_at;
-- 이전 schema에 없는 종류(재알림·시험)는 지운다(본문은 CASCADE). FORCE RLS라 owner도 tenant 설정 없이는 0행이 보이므로
-- 지우는 동안만 FORCE를 푼다(superuser·BYPASSRLS가 아닌 owner 계정으로 migrate해도 down이 되게, PS-0010과 같은 함정)
ALTER TABLE notification_deliveries NO FORCE ROW LEVEL SECURITY;
ALTER TABLE notification_delivery_bodies NO FORCE ROW LEVEL SECURITY;
DELETE FROM notification_deliveries WHERE event_type NOT IN ('alert.opened', 'alert.resolved');
ALTER TABLE notification_delivery_bodies FORCE ROW LEVEL SECURITY;
ALTER TABLE notification_deliveries FORCE ROW LEVEL SECURITY;
ALTER TABLE notification_deliveries DROP CONSTRAINT notification_deliveries_kind;
ALTER TABLE notification_deliveries DROP CONSTRAINT notification_deliveries_event_type_check;
ALTER TABLE notification_deliveries ADD CONSTRAINT notification_deliveries_event_type_check
  CHECK (event_type IN ('alert.opened', 'alert.resolved'));
ALTER TABLE notification_deliveries
  ALTER COLUMN monitor_id SET NOT NULL,
  ALTER COLUMN group_hash SET NOT NULL,
  ALTER COLUMN episode_id SET NOT NULL;
