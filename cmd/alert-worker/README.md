# cmd/alert-worker

monitor 평가(lease + idempotency), 상태 머신(OK/PENDING/ALERT/RECOVERING/NO_DATA/EVALUATION_ERROR), notification outbox·webhook 전송.

- 쓰기 소유 데이터: 평가 일정의 lease·last_slot(`monitor_schedule`), 평가 기록(`monitor_evaluations`, 7일), group 경보 상태·사건(`alert_instances`), 전이 event(outbox `alert.state_changed`)
- 동기 의존·장애 동작
  - ClickHouse(query 계정) 조회 실패: 그 monitor의 평가 결과가 EVALUATION_ERROR(`query_failed`)다. 이전 값으로 대체하지 않는다. 열린 경보는 유지되고 새 경보는 발화하지 않는다(ADR 0050).
  - 제어 DB 실패: 할 일 찾기가 실패하면 다음 tick(5초)에 다시 한다. 저장이 실패하면 lease(1분) 만료 뒤 같은 slot(아직 지금 slot이면)이나 그 뒤 slot을 평가한다.
  - crash: lease 만료 뒤 다른 인스턴스가 같은 slot을 가져간다. 늦게 끝난 결과는 버린다(`ErrLeaseLost`).
  - 밀린 slot은 따라잡지 않는다. 지금 slot만 평가한다.
- 단계: M0 (F06), G1 (F11)
- 명세: D02 §11, §17, §21
- 구현(단계 B2, ADR 0051): `cmd/alert-worker`(조립) → `internal/alertworker`(tick·lease·평가 루프) → `internal/alerting`(평가 의미, ADR 0050) · `internal/controldb.AlertStore`(일정·lease·상태·기록·outbox) · `internal/telemetrystore`(metric 조회)
  - 할 일 찾기만 tenant를 가로지른다(`app.monitor_scan`, `monitor_schedule` SELECT만). 그 밖은 대상 tenant의 `WithTenant` 트랜잭션이다.
  - ClickHouse는 tenant마다 `authz.NewSystemPrincipal(tenant, "alert-worker", telemetry.read)`로 읽는다(row policy·mandatory predicate 경로 동일).
  - 여러 인스턴스를 띄워도 된다. slot 하나는 lease를 잡은 인스턴스 하나만 평가한다. `MONTRACER_ALERT_WORKER_ID`는 인스턴스마다 달라야 한다.
- 실행 환경 변수: `MONTRACER_PG_APP_DSN`, `MONTRACER_CH_QUERY_DSN`, `MONTRACER_ALERT_WORKER_ID`(기본 hostname-pid), `MONTRACER_METRICS_ADDR`(기본 :9464)
- 운영 지표: `montracer_alert_evaluations_total{status}`, `montracer_alert_state_transitions_total`, `montracer_alert_evaluation_duration_seconds`, `montracer_alert_lease_lost_total`, `montracer_alert_worker_errors_total{stage}`, `montracer_alert_last_scan_timestamp_seconds`(heartbeat)
  - 경보: `MontracerAlertWorkerFailing`(page), `MontracerAlertWorkerNotRunning`(page), `MontracerAlertEvaluationErrorsHigh`(ticket) — RB01
- 시간 상한: claim·저장 각 5초, 조회 15초, lease 1분. 완료 저장은 조회와 다른 context라 조회가 상한을 다 써도 EVALUATION_ERROR를 쓴다. lease는 claim마다 새 token이다(worker 이름이 겹쳐도 섞이지 않음)
- 배포 순서: migration 00008 → control-api → 경보 규칙·alert-worker. 사이에 만든 monitor는 RB01 "평가 일정이 빠진 monitor 찾기"로 채운다
- rollback: 배포를 멈추면 평가가 멈춘다. 정의 API는 그대로 동작하고, 다시 띄우면 지금 slot부터 평가한다.
- 24시간 dry-run은 control-api validate가 같은 평가 함수로 한다(ADR 0052)
- 아직 없는 것: notification policy·webhook 전송(HMAC·SSRF, 단계 C), S09 화면(단계 D), worker sharding
