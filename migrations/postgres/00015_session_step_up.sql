-- session의 MFA step-up 기록 (D04 §02 "관리자 key 발급과 데이터 삭제는 MFA step-up 대상", ADR 0015 §2, ADR 0066)
-- 앱 role이 step_up_at을 바꿀 수 있게 한다(재인증 callback). 다른 열 권한은 00013 그대로다.

-- +goose Up
GRANT UPDATE (step_up_at) ON user_sessions TO montracer_rw;

-- +goose Down
REVOKE UPDATE (step_up_at) ON user_sessions FROM montracer_rw;
