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
- 실행 환경 변수: `MONTRACER_CONTROL_ADDR`(기본 :8081), `MONTRACER_PG_APP_DSN`, `MONTRACER_KEY_PEPPER_HEX`, `MONTRACER_CURSOR_KEY_HEX`(32 byte 이상, secret manager), `MONTRACER_METRICS_ADDR`(기본 :9464)
- 운영 지표: `montracer_control_*` (route, status_class), 경보 `MontracerControlErrorRateHigh` (RB02)
- 아직 없는 것: 사람 session(OIDC)·CSRF, key·멤버·정책·dashboard·monitor·삭제 job API, Idempotency-Key 저장, rate limit, OpenAPI 원천
