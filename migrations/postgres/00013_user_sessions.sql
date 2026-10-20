-- 사람 사용자의 browser session (D04 §02 인증 흐름, D02 §12, ADR 0064)
--   OIDC 로그인 뒤 서버가 만든 session이다. 원문 token은 cookie에만 있고, 여기에는 SHA-256만 둔다.
--   tenant는 로그인 때 고른 membership이다. 역할은 저장하지 않는다 — 요청마다 memberships에서 다시 읽는다
--   (역할 변경·멤버 제거가 다음 요청부터 바로 반영된다).

-- +goose Up
CREATE TABLE user_sessions (
  id_hash         bytea       PRIMARY KEY CHECK (length(id_hash) = 32),
  tenant_id       uuid        NOT NULL REFERENCES tenants (id),
  -- membership user_id와 같은 값(oidc:<issuer hash>:<sub>, authz.OIDCUserID)
  user_id         text        NOT NULL CHECK (char_length(user_id) BETWEEN 1 AND 256),
  -- ID token의 name claim(화면 표시용). 없으면 NULL. 로그·감사에는 쓰지 않는다
  display_name    text        CHECK (display_name IS NULL OR char_length(display_name) <= 256),
  created_at      timestamptz NOT NULL DEFAULT now(),
  last_seen_at    timestamptz NOT NULL,
  -- idle 30분(활동마다 연장), 최대 12시간(연장 없음) (D04 §02)
  idle_expires_at timestamptz NOT NULL,
  expires_at      timestamptz NOT NULL,
  step_up_at      timestamptz,
  revoked_at      timestamptz,
  CHECK (idle_expires_at <= expires_at)
);
CREATE INDEX user_sessions_by_user ON user_sessions (tenant_id, user_id) WHERE revoked_at IS NULL;

ALTER TABLE user_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON user_sessions
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());
-- 인증 조회: tenant를 모르는 상태에서 정확한 token hash 한 행만 읽는다(api_keys의 key_lookup과 같은 방식, ADR 0016 §4). SELECT 전용.
CREATE POLICY session_lookup ON user_sessions FOR SELECT
  USING (id_hash = decode(nullif(current_setting('app.session_lookup', true), ''), 'hex'));
-- 로그인: tenant를 모르는 상태에서 한 사용자의 membership만 읽는다(어느 조직에 들어갈지). SELECT 전용.
CREATE POLICY member_lookup ON memberships FOR SELECT
  USING (user_id = nullif(current_setting('app.member_lookup', true), ''));

-- 최소 권한: 만들기, 활동 연장(last_seen·idle), 폐기, 만료된 행 정리. tenant·user·만료 상한·hash는 바꾸지 않는다
GRANT SELECT, INSERT, DELETE ON user_sessions TO montracer_rw;
GRANT UPDATE (last_seen_at, idle_expires_at, revoked_at) ON user_sessions TO montracer_rw;

-- +goose Down
DROP POLICY IF EXISTS member_lookup ON memberships;
DROP TABLE user_sessions;
