# cmd/control-api

tenant·멤버·키·정책·dashboard·monitor·삭제 job 관리 API. 변경·감사·outbox를 같은 PostgreSQL 트랜잭션에 기록.

- 쓰기 소유 데이터: tenant·정책·revision·outbox (PostgreSQL, RLS)
- 동기 의존·장애 동작: PostgreSQL 불가 시 mutation 중단. 조회는 503(fail closed). ClickHouse 불가 시 validate의 dry-run만 빠진다(200, `dry_run_failed`). 기동 때 ClickHouse가 닿지 않아도 뜨고 dry-run 때 다시 연결한다(30초 간격). DSN 형식 오류·읽기 전용이 아닌 계정은 기동 거부.
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
  - 상태 조회(S09, ADR 0054): 목록·단건 `status`(가장 나쁜 group 상태·평가 전·꺼짐·stale), `GET /api/v1/monitors/{id}/groups`(group별 지금 상태), `GET /api/v1/monitors/{id}/timeline?from&to`(평가·전이·알림, 최대 7일). 상태·값은 tenant 전체 telemetry의 평가 결과라 `monitors.read` + `telemetry.read` + environment 제한 없음이 필요하다. 그렇지 않은 key는 정의만 보고(`status` null, `meta.warnings`에 `status_forbidden`) groups·timeline은 403이다. timeline의 전이 조회는 `outbox_resource_time` index(migration 00010)를 쓴다
  - flag: `MONTRACER_MONITOR_API_ENABLED=false`면 monitor 경로 404(기본 켜짐, rollback용)
  - validate 24시간 dry-run(ADR 0052): 요청자 principal로 ClickHouse query 계정(`metric_1m`)을 읽어 지난 24시간을 주기 평가와 같은 함수로 다시 평가한다. 후보 발화 구간·coverage를 준다. 못 하면 200 + `dry_run: null` + 이유 경고(`dry_run_unavailable|forbidden|failed|too_large|busy`). 10초 상한, 동시 4개, 조회 예산 결과 200,000행(interactive 기본 10,000행보다 크게)
- 구현(3차): notification policy(ADR 0053) — `GET|POST /api/v1/notification-policies`, `GET|PUT|DELETE /api/v1/notification-policies/{id}`
  - 쓰기 `policies.write`(Tenant Admin), 조회 `monitors.read`. URL은 HTTPS·공개 host만, 응답·감사에는 host만
  - 서명 secret은 서버가 만들어 생성 응답에서 한 번만 준다(재생 응답에는 없음 + `signing_secret_not_replayed`). 저장은 AES-GCM 암호문
  - monitor `notification_policy_id`는 같은 tenant에 살아 있어야 저장된다(없으면 400). 살아 있는 monitor가 가리키는 policy는 지울 수 없다(409)
  - URL 전체는 암호문으로만 저장된다(path·query의 receiver token 보호). 응답·감사에는 scheme·host만
  - `MONTRACER_NOTIFY_KEY_HEX`가 없으면 policy API는 404
- 실행 환경 변수: `MONTRACER_CONTROL_ADDR`(기본 :8081), `MONTRACER_PG_APP_DSN`, `MONTRACER_CH_QUERY_DSN`(선택, dry-run. 비우면 꺼짐), `MONTRACER_NOTIFY_KEY_HEX`·`MONTRACER_NOTIFY_KEY_VERSION`(선택, policy API·secret 암호화, alert-worker와 같은 값), `MONTRACER_KEY_PEPPER_HEX`, `MONTRACER_CURSOR_KEY_HEX`(32 byte 이상, secret manager), `MONTRACER_METRICS_ADDR`(기본 :9464)
- 운영 지표: `montracer_control_*` (route, status_class), `montracer_control_monitor_dry_run_total{outcome}`·`_duration_seconds`, 경보 `MontracerControlErrorRateHigh` (RB02)
- 아직 없는 것: 사람 session(OIDC)·CSRF, key·멤버·정책·dashboard·삭제 job API, 재알림·webhook 시험 전송·secret 교체(ADR 0053 단계 C2), 다른 POST의 Idempotency-Key, idempotency 만료 행 정리 job, rate limit, OpenAPI 원천
