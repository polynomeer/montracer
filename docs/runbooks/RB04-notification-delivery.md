# RB04 알림 누락

- 원문: D04 §11 RB04(알림 누락과 지역 장애). 이 문서는 webhook 알림 전송까지 다룬다. 지역 장애 절차는 D04 §05 구성 이후에 쓴다.
- 관련 설계: ADR 0051(평가 실행), ADR 0053(알림 정책·전송), D02 §17·§20
- 경보 규칙: [`deploy/prometheus/rules/montracer.rules.yml`](../../deploy/prometheus/rules/montracer.rules.yml) `montracer-notifications` 그룹

## 먼저 알아둘 것

1. **평가 실패와 전송 실패를 나눈다(D04 §11).**
   - 평가가 멈추면 알림이 쌓이지 않는다. RB01 `MontracerAlertWorkerFailing`·`MontracerAlertWorkerNotRunning`을 따른다.
   - 알림이 쌓였는데 나가지 않으면 이 runbook이다.
2. **알림은 경보 전이와 같은 트랜잭션에 쌓인다.** 사건(episode)이 열리거나 닫힐 때 `notification_deliveries` 행이 생긴다.
   - 행이 없으면 평가 쪽 문제다(또는 monitor에 policy가 없다).
   - 행이 `pending`이면 전송 쪽 문제다.
3. **전송은 at-least-once다.** 같은 알림을 다시 보내면 같은 `event_id`(`Montracer-Event-Id`)와 같은 본문이 간다.
   - receiver는 `event_id`로 중복을 거른다.
   - 다시 보낸 시도는 `Montracer-Delivery-Attempt`가 1보다 크다.
4. **운영자가 tenant의 알림 행을 보는 것은 tenant 데이터 접근이다.**
   - 본문(labels·값)은 고객 데이터다.
   - 아래 조회는 tenant UUID·알림 id·상태·시각만 고른다. incident 기록에 사유와 범위를 남긴다(RB03 원칙 5).
5. **URL과 secret을 옮기지 않는다.**
   - webhook URL의 path·query에는 receiver token이 흔하다. 화면·감사에는 host만 보인다.
   - 서명 secret 평문은 서버에 없다. 생성 응답에서 한 번만 보였다.

## 경보별 대응

### MontracerNotificationBacklog

- **탐지:** 한 번도 시도하지 않은 알림 중 가장 오래된 것이 5분 넘게 기다린다(`montracer_notification_backlog_seconds`).
- **영향:** 경보는 열렸지만 사람이 받지 못한다. 고객에게 보이지 않는 장애라 page다.
- **원인과 조치**
  1. alert-worker 로그 `notification delivery disabled: MONTRACER_NOTIFY_KEY_HEX is empty`가 있으면 보내지 않고 쌓이기만 하는 상태다(backlog 지표만 낸다). secret manager의 master key 주입을 확인한다. control-api와 같은 key·버전이어야 한다.
  2. alert-worker process 상태를 본다(`MontracerAlertWorkerNotRunning`). 전송 루프는 같은 process에 있다.
  3. `MontracerNotificationWorkerFailing`이 함께 울리면 제어 DB 문제다. 아래 절을 따른다.
  4. 처리량 부족: `montracer_notification_delivery_duration_seconds`가 상한(10초) 근처면 느린 endpoint가 많다.
     - policy 하나는 tick마다 5개까지 차례로만 보내므로, endpoint 하나가 전체를 막지는 않는다.
     - 느린 policy가 많으면 alert-worker 인스턴스를 늘린다(lease로 나눠 보낸다).
- **복구 확인:** backlog가 0으로 내려가고 `montracer_notification_deliveries_total{outcome="delivered"}`가 는다.

### MontracerNotificationWorkerFailing

- **탐지:** 전송 루프의 보낼 알림 찾기(`scan`)나 lease·결과 저장(`evaluate`)이 10분째 실패한다.
- **영향:** 알림이 나가지 않거나, 나간 결과가 저장되지 않아 lease 만료 뒤 다시 나간다(중복).
- **단계:** `scan`(보낼 알림 찾기), `deliver`(lease 잡기·결과 저장), `prune`(30일 정리)
- **조치:** alert-worker 로그 `notification tick failed`·`notification delivery failed`·`notification prune failed`를 본다.
  - 제어 DB 연결·지연을 확인한다.
  - migration 00009가 적용됐는지 확인한다.
  - `42501`이면 `montracer_rw` GRANT를 확인한다.
- **복구 확인:** 오류 지표가 멈추고 backlog가 줄어든다.

### MontracerNotificationDeliveryFailed

- **탐지:** 30분 안에 전송을 포기한 알림(`failed`, DLQ)이 있다. 포기하는 경우는 셋이다.
  - 24시간 동안 지수 backoff로 다시 보냈지만 모두 실패했다(30초 → 1시간 상한).
  - 영구 실패: 목적지가 사설·예약 주소로 풀렸다(`destination_blocked`). DNS rebinding이 의심될 때도 이 사유다.
  - secret을 풀 수 없다(`secret_unavailable`): master key 버전이 맞지 않는다.
- **영향:** 해당 사건의 알림이 receiver에 닿지 않았다. 경보 상태 자체는 정상 기록이다.
- **조사:** 아래 조회를 `app.tenant_id`를 건 tenant 트랜잭션에서 한다. 운영자 접근이므로 기록을 남긴다.

  ```sql
  BEGIN;
  SELECT set_config('app.tenant_id', '<tenant uuid>', true);
  -- 본문(labels·값)은 notification_delivery_bodies에 있다. 조사에 필요하지 않으면 읽지 않는다
  SELECT id, policy_id, event_type, attempts, last_status_code, last_error, created_at, finished_at
  FROM notification_deliveries WHERE status = 'failed' AND finished_at > now() - interval '1 day'
  ORDER BY finished_at DESC LIMIT 50;
  COMMIT;
  ```

- **사유별 조치**
  - `http_status`·`connect_failed`·`timeout`·`tls_failed`: 고객 endpoint 문제다. 고객에게 policy host와 마지막 상태 코드를 알린다. URL 전체는 우리 쪽에서 보이지 않는다.
  - `destination_blocked`: 고객이 사설 주소를 가리키게 했거나 DNS가 바뀌었다. 공개 HTTPS endpoint로 policy를 고치도록 안내한다. 반복되면 보안 담당자에게 알린다(SSRF 시도 가능성).
  - `secret_unavailable`: master key 회전 절차가 어긋났다. 저장된 암호문의 버전(`secret_sealing_key`·`url_sealing_key`) key가 alert-worker에 없다.
    1. 현재 key는 `MONTRACER_NOTIFY_KEY_HEX`·`_VERSION`에 둔다.
    2. 이전 key는 alert-worker의 `MONTRACER_NOTIFY_PREVIOUS_KEYS`(`버전:hex,…`)에 넣는다.
    3. 고친 뒤 아래 "다시 보내기"를 한다.
  - `redirect`: receiver가 3xx를 줬다. redirect는 따라가지 않는다(SSRF 방어). 최종 URL로 policy를 고치도록 안내한다.

## 다시 보내기 (replay, D04 §11 "event_id 기준 replay, 중복 가능성 표시")

원인을 고친 뒤 포기한 알림을 다시 보낸다.
- 같은 `event_id`·같은 본문이 가므로 receiver가 중복을 거를 수 있다.
- `Montracer-Delivery-Attempt`는 이어서 증가한다(중복 가능성 표시). 시도 수는 lease를 잡을 때 늘므로, 보낸 뒤 결과를 못 쓴 재전송도 번호가 다르다.
- 24시간 재시도 창은 `created_at` 기준이라, 하루가 지난 알림은 한 번만 시도하고 다시 실패하면 바로 포기한다.

```sql
BEGIN;
SELECT set_config('app.tenant_id', '<tenant uuid>', true);
UPDATE notification_deliveries SET status = 'pending', next_attempt_at = now(), finished_at = NULL, lease_token = NULL, lease_until = NULL
WHERE status = 'failed' AND id = ANY('{<id1>,<id2>}'::uuid[]);
COMMIT;
```

## 고객 문의: "경보 알림이 안 왔다"

1. monitor에 policy가 붙어 있는지 본다(`notification_policy_id`).
   - policy가 꺼졌으면 알림은 `canceled`(`policy_disabled`)다.
   - 살아 있는 monitor가 가리키는 policy는 지울 수 없다(409). 그래도 낡은 정의 평가가 지운 policy로 사건을 열었으면 `canceled`(`policy_deleted`)다.
2. 그 시각에 사건이 열렸는지 본다(`alert_instances`, outbox `alert.state_changed`).
   - PENDING·NO_DATA·EVALUATION_ERROR 상태 변화는 알림이 아니다. 사건 열림·닫힘만 알린다(ADR 0053).
3. 알림 행의 상태(`pending`·`delivered`·`failed`·`canceled`)와 `last_error`로 위 절차를 따른다.
4. `delivered`인데 못 받았다면 receiver 쪽 서명 검증 실패를 의심한다. 아래 "receiver 검증"을 안내한다.

## receiver 검증 (고객 안내)

- **header**
  - `Montracer-Event-Id`
  - `Montracer-Timestamp`(Unix 초)
  - `Montracer-Key-Id`(서명 secret id, `wk_…`)
  - `Montracer-Signature`(`v1=<hex>`)
  - `Montracer-Delivery-Attempt`
- **서명:** HMAC-SHA256(secret, `<timestamp>.<event_id>.<원문 body bytes>`)의 hex다. 본문은 다시 직렬화하지 말고 받은 bytes 그대로 쓴다.
- **시간:** timestamp가 지금과 5분 넘게 다르면 거절한다(D02 §17). 재전송 공격을 막는다.
- **중복:** `event_id`로 거른다(at-least-once).
- secret(`whsec_…`)은 policy를 만들 때 한 번만 보인다. 잃어버렸으면 policy를 다시 만든다. secret 교체 API는 후속 단계다.
