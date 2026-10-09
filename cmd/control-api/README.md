# cmd/control-api

tenant·멤버·키·정책·dashboard·monitor·삭제 job 관리 API. 변경·감사·outbox를 같은 PostgreSQL 트랜잭션에 기록.

- 쓰기 소유 데이터: tenant·정책·revision·outbox (PostgreSQL, RLS)
- 동기 의존·장애 동작: PostgreSQL 불가 시 mutation 중단. 조회는 503(fail closed).
- 단계: M0 (F05~F07)
- 명세: D02 §11, §14, §20 · D04 §01, §12
- 구현(1차): `GET /api/v1/audit-events`(ADR 0034) — `cmd/control-api`(진입점, `/healthz`·`/readyz`) → `internal/controlapi`(인증·입력 검증·서명 cursor) → `internal/controldb.AuditStore`(RLS, 범주 권한 ADR 0015 §4)
  - 범주: `audit.read`는 operations·security, `audit.operations.read`만 있으면 operations. security를 명시 요청했는데 권한이 없으면 403
  - 응답: `{data, next_cursor, meta}`(D02 §19). 운영자 break-glass 행은 운영자·승인자 ID를 가린다
  - cursor: `internal/apicursor` 서명 토큰(tenant·권한 fingerprint·query hash·snapshot·만료 15분)
- 구현(2차): monitor 정의(ADR 0049) — `POST /api/v1/monitors/validate`, `GET|POST /api/v1/monitors`, `GET|PUT|DELETE /api/v1/monitors/{id}`
  - spec 검증·정규형은 `internal/monitor`(alert-worker와 공유), 저장은 `internal/controldb.MonitorStore`(정의·revision 이력·operations 감사·outbox 한 트랜잭션, tombstone)
  - POST는 `Idempotency-Key` 필수(같은 트랜잭션에 응답 저장, 24시간, 다른 본문 409), PUT은 `If-Match` 필수(없으면 428, 다르면 412)
  - 권한: 쓰기·validate `monitors.write`, 조회 `monitors.read`(Developer 이상). environment 제한 API key는 쓰기·validate가 403이다(평가가 tenant 전체를 읽는다, ADR 0051)
  - 정의 생성·수정·삭제는 같은 트랜잭션에서 평가 일정(`monitor_schedule`)을 바꾸고, 수정·끄기·삭제는 경보 상태를 끝낸다(열린 사건은 닫힘 event, ADR 0051)
  - flag: `MONTRACER_MONITOR_API_ENABLED=false`면 monitor 경로 404(기본 켜짐, rollback용)
- 실행 환경 변수: `MONTRACER_CONTROL_ADDR`(기본 :8081), `MONTRACER_PG_APP_DSN`, `MONTRACER_KEY_PEPPER_HEX`, `MONTRACER_CURSOR_KEY_HEX`(32 byte 이상, secret manager), `MONTRACER_METRICS_ADDR`(기본 :9464)
- 운영 지표: `montracer_control_*` (route, status_class), 경보 `MontracerControlErrorRateHigh` (RB02)
- 아직 없는 것: 사람 session(OIDC)·CSRF, key·멤버·정책·dashboard·삭제 job API, notification policy·webhook(ADR 0049 단계 C), 다른 POST의 Idempotency-Key, idempotency 만료 행 정리 job, rate limit, OpenAPI 원천
