# cmd/alert-worker

monitor 평가(lease + idempotency), 상태 머신(OK/PENDING/ALERT/RECOVERING/NO_DATA/EVALUATION_ERROR), notification outbox·webhook 전송.

- 쓰기 소유 데이터: 평가 일정의 lease·last_slot(`monitor_schedule`), 평가 기록(`monitor_evaluations`, 7일), group 경보 상태·사건·상태 시작 시각(`alert_instances`, `state_since`는 처음 만들 때와 상태가 바뀔 때만, ADR 0073), 전이 event(outbox `alert.state_changed`), 알림(`notification_deliveries`, 같은 트랜잭션에 쌓고 전송 루프가 상태를 쓴다, 30일)
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
- 알림 전송(ADR 0053): 사건 열림·닫힘 알림을 webhook으로 보낸다.
  - lease token, at-least-once(같은 event_id), HMAC(`timestamp.event_id.body`)
  - 2xx 아니면 30초~1시간 backoff로 24시간 재시도한 뒤 포기(DLQ)
  - SSRF: 연결 직전 주소 검사, redirect·proxy 금지
  - `MONTRACER_NOTIFY_KEY_HEX`가 없으면 보내지 않고 backlog 지표만 낸다(알림은 쌓이고 `MontracerNotificationBacklog`가 울린다)
  - policy 하나는 tick마다 5개까지 차례로 보낸다. 시도 수는 lease를 잡을 때 늘린다. 10분마다 30일 지난 알림을 지운다
- 알림 2단계(ADR 0055)
  - 재알림: 열린 사건이 ALERT로 남아 있으면 열림 알림 30분×k 뒤 k번째(최대 3번)를 평가 완료 트랜잭션에서 쌓는다(`alert.renotified`, `renotify_sequence`)
  - 시험 전송(`notification.test`): 한 번만 보낸다(재시도 없음), 꺼진 policy에도 보낸다, 지표 outcome `test`
  - secret 교체 겹침 기간에는 두 secret으로 서명한다(`Montracer-Key-Id`·`Montracer-Signature` 쉼표 목록, 새 secret 먼저)
  - 본문 `payload.links.monitor`: S09 상세 주소(절대 from·to). `MONTRACER_PUBLIC_WEB_URL`이 없으면 null
- 실행 환경 변수: `MONTRACER_PG_APP_DSN`, `MONTRACER_CH_QUERY_DSN`, `MONTRACER_ALERT_WORKER_ID`(기본 hostname-pid), `MONTRACER_METRICS_ADDR`(기본 :9464), `MONTRACER_NOTIFY_KEY_HEX`·`MONTRACER_NOTIFY_KEY_VERSION`(control-api와 같은 값, secret manager), `MONTRACER_NOTIFY_PREVIOUS_KEYS`(회전 중 이전 key `버전:hex,…`), `MONTRACER_PUBLIC_WEB_URL`(선택, 알림 deep link의 web 주소, https — localhost만 http), `MONTRACER_NOTIFY_RENOTIFY_ENABLED`(기본 true, false면 재알림만 끔)
- 운영 지표: `montracer_alert_evaluations_total{status}`, `montracer_alert_state_transitions_total`, `montracer_alert_evaluation_duration_seconds`, `montracer_alert_lease_lost_total`, `montracer_alert_worker_errors_total{stage}`, `montracer_alert_last_scan_timestamp_seconds`(heartbeat)
  - 경보: `MontracerAlertWorkerFailing`(page), `MontracerAlertWorkerNotRunning`(page), `MontracerAlertEvaluationErrorsHigh`(ticket) — RB01
  - 알림: `montracer_notification_*`, 경보 `MontracerNotificationBacklog`(page), `MontracerNotificationWorkerFailing`(page), `MontracerNotificationDeliveryFailed`(ticket) — RB04
- 시간 상한: claim·저장 각 5초, 조회 15초, lease 1분. 완료 저장은 조회와 다른 context라 조회가 상한을 다 써도 EVALUATION_ERROR를 쓴다. lease는 claim마다 새 token이다(worker 이름이 겹쳐도 섞이지 않음)
- 배포 순서: migration 00008 → control-api → 경보 규칙·alert-worker. 사이에 만든 monitor는 RB01 "평가 일정이 빠진 monitor 찾기"로 채운다
- rollback: 배포를 멈추면 평가가 멈춘다. 정의 API는 그대로 동작하고, 다시 띄우면 지금 slot부터 평가한다.
- 24시간 dry-run은 control-api validate가 같은 평가 함수로 한다(ADR 0052)
- 아직 없는 것: 재알림(30분×3)·webhook 시험 전송·secret 교체(단계 C2), Slack·이메일 connector, S09 화면(단계 D), worker sharding
