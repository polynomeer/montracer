-- 멤버 관리와 초대 (ADR 0067, D04 §01 Tenant Admin "멤버", §12 JIT 금지)
--   memberships: 화면에 보일 이름(로그인 때 IdP name claim)과 마지막 로그인 시각
--   member_invitations: 한 번만 쓰는 초대 링크. token 원문은 저장하지 않는다(SHA-256만). 7일 만료.
--   초대로 줄 수 있는 역할에 tenant_admin은 없다 — 링크가 새면 관리자 권한이 새므로, 관리자는 가입 뒤 step-up한 관리자가 올린다.

-- +goose Up
ALTER TABLE memberships
  ADD COLUMN display_name  text CHECK (display_name IS NULL OR length(display_name) BETWEEN 1 AND 256),
  ADD COLUMN last_login_at timestamptz;

CREATE TABLE member_invitations (
  tenant_id     uuid        NOT NULL REFERENCES tenants (id),
  invitation_id uuid        NOT NULL,
  token_hash    bytea       NOT NULL CHECK (length(token_hash) = 32),
  role          text        NOT NULL CHECK (role IN ('viewer','developer','operator','security_auditor')),
  -- 관리자가 붙이는 구분용 메모(예: "결제팀 신규"). 선택
  label         text        CHECK (label IS NULL OR length(label) BETWEEN 1 AND 64),
  created_by    text        NOT NULL CHECK (length(created_by) BETWEEN 1 AND 256),
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  accepted_at   timestamptz,
  accepted_by   text        CHECK (accepted_by IS NULL OR length(accepted_by) BETWEEN 1 AND 256),
  revoked_at    timestamptz,
  PRIMARY KEY (tenant_id, invitation_id),
  -- 수락은 tenant를 모른 채 token hash로 찾는다(전역 고유)
  CONSTRAINT member_invitations_token_unique UNIQUE (token_hash),
  CONSTRAINT member_invitations_once CHECK (accepted_at IS NULL OR revoked_at IS NULL),
  CONSTRAINT member_invitations_accepted CHECK ((accepted_at IS NULL) = (accepted_by IS NULL))
);
-- 대기 중 초대 목록·개수(조직 상한)
CREATE INDEX member_invitations_pending ON member_invitations (tenant_id, created_at)
  WHERE accepted_at IS NULL AND revoked_at IS NULL;

ALTER TABLE member_invitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE member_invitations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON member_invitations
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());
-- 수락: tenant를 모르는 상태에서 정확한 token hash 한 행만 읽는다(session_lookup·key_lookup과 같은 방식, ADR 0016 §4). SELECT 전용.
CREATE POLICY invitation_lookup ON member_invitations FOR SELECT
  USING (token_hash = decode(nullif(current_setting('app.invitation_lookup', true), ''), 'hex'));

-- 최소 권한: 만들기, 수락·폐기 표시. 역할·만료·token hash·만든 사람은 바꾸지 않는다. 삭제하지 않는다(감사 대조)
GRANT SELECT, INSERT ON member_invitations TO montracer_rw;
GRANT UPDATE (accepted_at, accepted_by, revoked_at) ON member_invitations TO montracer_rw;

-- +goose Down
DROP TABLE member_invitations;
ALTER TABLE memberships DROP COLUMN last_login_at, DROP COLUMN display_name;
