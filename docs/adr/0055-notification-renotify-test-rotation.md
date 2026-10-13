# ADR 0055: 경보 알림 2단계 — 재알림, 시험 전송, 서명 secret 교체, deep link

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-13 다음 단계 선택 "C2: 알림 마무리")
- Owner: API lead
- 날짜: 2026-10-13 (제안·결정)
- 관련: F06, E05 · D02 §17·§20, D04 §02·§11(RB04), D05 §09 · 계약 1·6·7 · ADR 0051, 0053, 0054

## 배경

ADR 0053(C1)은 사건 열림·닫힘을 서명된 webhook으로 보낸다. 다음 넷은 C2로 미뤘다.

- **재알림:** D02 §17은 "30분, 최대 3회 후 escalation"을 정한다.
- **시험 전송:** D05 §09는 "webhook 테스트와 실제 알림 분리"를 합격 기준으로 둔다.
- **서명 secret 교체:** D02 §20은 "key rotation 동안 key_id를 명시"한다. 지금은 secret을 잃거나 유출되면 policy를 다시 만들어야 한다.
- **deep link:** D02 §20은 "payload URL은 인증 필요 deep link이며 secret을 포함하지 않는다"고 정한다. D05 §09는 "경보 deep link가 동일 절대시간을 연다"를 합격 기준으로 둔다. ADR 0053이 명세 차이로 사용자에게 알리고 미룬 항목이다.

명세가 정하지 않은 것이 있다.
- 재알림의 시작 시점, 어떤 상태에서 다시 알리는가, 3회 뒤 escalation의 대상
- 시험 전송을 누가 어떻게 보내고, 결과를 어떻게 보이는가
- 교체 겹침 기간과 겹침 동안의 서명 형식
- deep link의 주소, 시간 범위, 조직 경로

## 결정

### 1. 재알림 (`alert.renotified`)

- **조건:** 열린 사건이 평가 시점에 **ALERT**다. 열림 알림을 쌓은 시각 + 30분×k가 지났고 **마지막 알림(열림 또는 직전 재알림)에서도 30분**이 지났으면 k번째(1..3)를 쌓는다. 두 번째 조건이 Datadog `renotify_interval`("마지막 알림 뒤")과 같은 간격이다.
  - no_data 정책으로 열린 사건도 상태는 ALERT다(ADR 0050). 그래서 Datadog 기본값(Alert·No Data)과 같은 범위다.
  - RECOVERING·EVALUATION_ERROR·NO_DATA인 동안은 쌓지 않는다. 그동안은 아직 확인되지 않은 상태다.
  - ALERT로 돌아오면 다음 번호 **하나만** 쌓는다. 그다음은 다시 30분 뒤다. 놓친 번호를 몰아서 보내지 않는다(리뷰 반영: 열림 기준만 보면 복귀 직후 1분 간격으로 두 번 갔다).
- **같은 트랜잭션:** 재알림은 평가 완료(`CompleteEvaluation`) 안에서 쌓는다. 경보 상태와 같은 트랜잭션이다(D02 §17).
- **대상:** 열림 알림을 받은 policy에만 보낸다. 취소된 열림(`policy_deleted`)에는 보내지 않는다. 열림을 못 받은 곳에 재알림만 가지 않는다(닫힘과 같은 규칙, ADR 0053 §2).
- **중복 억제:** 저장 종류에 번호를 담는다(`alert.renotified.1..3`). 그래서 기존 UNIQUE(tenant, policy, monitor, group, episode, 종류)가 그대로 번호마다 하나를 보장한다. 재평가·중복 scheduler에도 k번째는 하나다.
  - webhook의 `event_type`은 `alert.renotified`이고 번호는 `payload.renotify_sequence`다. S09 timeline API도 같은 이름과 번호로 바꿔 준다.
  - 번호를 새 열로 두고 UNIQUE를 다시 만들지 않는다. 이전 binary의 `ON CONFLICT`(6열)가 맞는 제약을 잃어 경보 저장이 실패하기 때문이다. rollout 중(migration 뒤 배포 전)과 rollback 때 모두 해당한다. 확장만 하는 migration이다.
- **본문:** 열림과 같은 AlertPayload에 `renotify_sequence`(1..3)를 싣는다. 열림·닫힘에서는 null이다.
- **3회 뒤 escalation:** 더 보내지 않는다. escalation 대상(다른 policy·on-call)은 F11(G1)이다.
- S09 timeline은 재알림을 "재알림 k/3"으로 보인다.

### 2. 시험 전송 (`notification.test`)

- **API**
  - `POST /api/v1/notification-policies/{id}/test` → 202. `Location`에 결과 주소가 있다.
  - `GET /api/v1/notification-policies/{id}/tests/{test_id}` → 상태, 시도 수, 응답 code, 오류 code
  - 권한은 `policies.write`(Tenant Admin)다. 받는 곳으로 데이터가 나가고, 응답 code로 받는 곳 상태를 알 수 있기 때문이다. environment 제한 key는 403이다.
- **보내는 곳:** 경보 알림과 같은 알림 행이고, alert-worker 전송 루프가 보낸다.
  - control-api가 직접 보내지 않는다. SSRF 방어(dialer 검사)와 secret 복호화를 한 곳(alert-worker)에만 둔다. control-api는 지금처럼 암호화만 한다(ADR 0053 §3).
- **실제 알림과 분리(D05 §09)**
  - event_type이 `notification.test`다. payload는 `{test: true, policy_id, policy_name, message}`이고 monitor·group·state가 없다.
  - `resource_id`는 policy id, `revision`은 policy revision이다.
  - 알림 행의 monitor·group·사건은 NULL이다(CHECK로 종류와 묶는다). S09 timeline(monitor 기준 조회)에 섞이지 않는다.
- **동작**
  - 꺼진 policy에도 보낸다. 켜기 전에 확인하는 용도다. 지운 policy는 취소한다.
  - 다시 보내지 않는다. 한 번 시도한 결과(응답 code·오류 code)를 바로 보인다. 응답 본문은 저장하지 않는다(ADR 0053 §3).
  - 지표 outcome은 `test`다. 실패한 시험은 운영 경보(`MontracerNotificationDeliveryFailed`)에 세지 않는다.
- **한도:** 진행 중인 시험이 있으면 새로 만들지 않고 그것을 돌려준다(경고 `test_in_progress`). policy 하나에 1시간 20번을 넘으면 429(`Retry-After`)다. 행을 잠가 동시 요청을 줄 세운다.
- **감사:** security 범주 `notification_policy.tested`(delivery id·policy revision). policy 상태는 바꾸지 않으므로 outbox는 없다.

### 3. 서명 secret 교체

- **API:** `POST /api/v1/notification-policies/{id}/rotate-secret`
  - If-Match가 필수다(없으면 428, 다르면 412). 두 번 눌러도 두 번 바뀌지 않는다.
  - 본문은 선택이다: `{"previous_secret_expires_in_seconds": N}`(0..86400, 기본 86400).
  - 새 secret(`whsec_…`)은 이 응답에서만 보인다(`Cache-Control: no-store`). Idempotency 재생 저장이 없으므로 평문이 응답 저장소에 남지 않는다.
- **겹침**
  - N > 0이면 지금 secret을 이전 secret 열로 옮긴다. 만료 시각까지 **두 secret으로 함께 서명**한다.
  - N = 0이면 이전 secret 없이 바로 바꾼다(유출 대응).
  - **겹침 중에 다시 교체하면** 이전 secret과 그 만료는 그대로 두고 지금 secret만 바꾼다(리뷰 반영).
    - 응답을 잃어(timeout) 다시 교체해도, receiver가 가진 교체 전 secret이 바로 끊기지 않는다.
    - 그 사이에 만든 secret은 버려진다. 그것을 이미 배포한 receiver가 있으면 다시 교체한 secret으로 바꿔야 한다.
  - 상한 24시간은 D04 §02 key 교체 겹침과 Stripe 상한과 같다.
- **서명 형식**
  - `Montracer-Key-Id: wk_new,wk_old`, `Montracer-Signature: v1=<new>,v1=<old>`로 싣는다. 같은 순서이고 새 secret이 먼저다.
  - receiver는 가진 secret 하나라도 맞으면 받는다(`notify.Verify`가 쉼표 목록을 받는다).
  - 겹침이 없으면 지금과 같은 한 값이다. 겹침 중에는 **값 하나만 비교하는 receiver(C1 참조 구현 그대로)는 모든 알림을 거절한다.** 엄격한 parser에는 깨지는 변경이다. 그래서 첫 교체 전에 receiver가 쉼표 목록을 받도록 안내한다(RB04 receiver 검증).
  - 이전 secret을 풀 수 없으면(master key 누락) 지금 secret만으로 보낸다. 겹침은 편의라서 전송을 막지 않고 경고 로그만 남긴다.
- **저장:** 이전 secret도 AES-GCM 암호문이다(AAD = tenant·policy·key id, ADR 0053 §1). 이전 secret은 key id·암호문·master key 버전·만료 시각 넷이 함께 있거나 함께 없다(CHECK).
  - 앱 role에 secret 열 UPDATE를 준다. 교체 API만 이 열을 쓴다(저장소 코드와 시험이 지킨다). 그 밖의 열 제한(tenant·만든 사람·DELETE)은 그대로다.
- **응답:** policy의 `webhook.previous_signing_key_id`·`previous_signing_key_expires_at`이다. 겹침이 끝났으면 null이다.
- **감사:** security 범주 `notification_policy.secret_rotated`(새 key id, 버린 key id, 이전 만료). secret 내용은 남기지 않는다.

### 4. deep link (`payload.links.monitor`)

- **설정:** `MONTRACER_PUBLIC_WEB_URL`(예: `https://app.example.com`). alert-worker와 control-api에 같은 값을 준다(control-api는 정의 변경으로 닫힌 사건의 알림을 쌓는다).
  - https만 받는다. http는 localhost에만 허용한다(로컬 개발).
  - path·query·fragment·자격 증명은 받지 않는다. 주소에 secret이 섞이지 않게 한다(D02 §20).
  - 비우면 `links`는 null이다. 형식 오류는 기동을 거부한다.
- **주소:** `{base}/o/{tenant_id}/monitors/{monitor_id}?from=…&to=…&tz=UTC`(S09 상세, ADR 0054)
  - 화면이 인증을 요구한다. 링크에는 권한이 없다.
  - 조직 경로는 tenant id다. 조직 slug가 아직 없다(재검토 조건).
- **절대 시간 범위(D05 §09):** 사건이 열리기 30분 전부터 이 알림 15분 뒤까지다. 언제 열어도 같은 구간이다.
  - 열림: 열림 시각 기준
  - 재알림·닫힘: 그 policy의 열림 알림 시각 기준
  - 7일(S09 timeline 한도)을 넘으면 to 앞 7일로 줄인다.
- **본문 bytes:** 본문은 쌓을 때 만든다(ADR 0053 §2). 그래서 설정을 바꿔도 이미 쌓인 알림의 링크는 그대로다.

## 외부 사례 근거 (2026-10-13 확인 범위: 공개 문서)

| 사례 | 내용 | 반영 |
|---|---|---|
| Datadog monitor 옵션 ([Monitor API Options](https://docs.datadoghq.com/monitors/guide/monitor_api_options)) | `renotify_interval`: 해결되지 않았으면 마지막 알림 뒤 N분마다 다시 알린다. `renotify_statuses` 기본은 Alert·No Data다. `renotify_occurrences`로 횟수를 제한하고, `escalation_message`는 재알림에 붙는다 | §1 ALERT(no_data 사건 포함)에서만, 3회 상한. escalation은 F11 |
| Stripe webhook 서명([Checking Webhook Signatures](https://stripe.com/docs/webhooks/signatures)) | endpoint secret을 굴릴 때 이전 secret을 최대 24시간 유지할 수 있다. 그동안 secret마다 서명을 하나씩 만들어 header에 `v1=`를 여럿 싣는다. receiver 라이브러리는 하나라도 맞으면 받는다. 허용 5분 | §3 겹침 0..24시간, `v1=` 여러 값, `Verify`가 목록을 받음 |
| 교체 후 검증 절차(2차 자료: [webhook signing key rotation](https://hackmd.io/@dmytro-nasyrov/webhook-signing-key-rotation-verification-trail)) | 교체 → 두 secret을 받는 receiver 배포 → 새 secret으로 전환 → 겹침 뒤 이전 secret 제거. header를 key 하나로 접는 parser는 정상 전송을 거절한다 | RB04 교체 절차, 쉼표 목록 안내 |
| Kit webhook API ([Rotate a webhook endpoint secret](https://developers.kit.com/api-reference/webhooks/rotate-a-webhook-endpoint-secret)) | secret 교체를 endpoint 자원의 별도 동작(API)으로 둔다 | §3 `rotate-secret` 하위 동작 |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 재알림 생성 위치 | 평가 완료 트랜잭션 | 전송 루프가 타이머로 생성: 경보 상태와 다른 트랜잭션이라 닫힌 사건에 재알림이 갈 수 있다(D02 §17 같은 트랜잭션) |
| 재알림 상태 | ALERT만 | 열린 사건 전체(RECOVERING 포함): 복구 중에 "아직 경보"라고 알린다 |
| 놓친 번호 | 다음 번호 하나 | 몰아서: 복귀 순간 여러 통이 한꺼번에 간다 |
| 시험 전송 주체 | 알림 행 + alert-worker | control-api가 동기 전송: SSRF dialer와 secret 복호화 key가 control-api에도 필요하다(ADR 0053 재검토 조건과 반대 방향) |
| 시험 재시도 | 없음 | 24시간 재시도: 결과를 바로 볼 수 없고, 실패가 운영 경보가 된다 |
| 겹침 서명 | 두 서명을 한 header에 | 이전 secret만 / 새 secret만: 교체 중인 receiver 한쪽이 거절한다. 두 번 전송: 중복이 는다 |
| 교체 중복 방지 | If-Match | Idempotency-Key: 재생 응답에 새 secret 평문을 저장해야 한다(ADR 0053 §1과 충돌) |
| 재알림 번호 저장 | 저장 종류에 번호(`alert.renotified.k`) | `sequence` 열 + UNIQUE 재생성: 이전 binary의 `ON CONFLICT`가 깨진다(배포 순서에 따라 경보 저장 실패) |
| deep link 조직 경로 | tenant id | slug: 아직 없다. 링크 없음: D02 §20 미충족이 계속된다 |

## 결과

- **API**
  - 새 경로: `POST /notification-policies/{id}/test`, `GET …/tests/{test_id}`, `POST …/rotate-secret`
  - policy 응답 `webhook`에 `previous_signing_key_id`·`previous_signing_key_expires_at`이 더해진다.
  - monitor timeline 알림에 `renotify_sequence`가 더해진다.
- **webhook:** event_type `alert.renotified`·`notification.test`가 새로 생긴다. AlertPayload에 `renotify_sequence`·`links`가 더해진다(기존 필드는 같다). 겹침 기간에는 `Montracer-Key-Id`·`Montracer-Signature`가 쉼표 목록이다.
- **schema:** PostgreSQL migration 00011
  - `notification_deliveries`
    - monitor·group·episode를 NULL 허용으로 바꾼다(시험).
    - 종류 CHECK에 `alert.renotified.1..3`·`notification.test`를 더하고, 시험과 NULL 열을 묶는 CHECK를 더한다.
    - UNIQUE는 그대로다(확장만, 이전 binary와 함께 돈다).
  - `notification_policies`: 이전 secret 네 열과 CHECK, secret 열 UPDATE GRANT
  - CHECK 추가는 표를 한 번 읽는다. 끝난 알림은 30일 보존이라 표가 작다.
- **tenant 영향:** 새 동작은 모두 `WithTenant`(RLS) 안이고, Tenant Admin 권한이다. 시험 결과 조회는 같은 policy의 시험만 본다(다른 policy·tenant·경보 알림 id는 404).
- **retention 영향:** 재알림·시험 행은 다른 알림과 같이 끝난 뒤 30일 보존한다. 열린 사건의 재알림 행은 사건이 닫힐 때까지 남는다(최대 3개).
- **설정:** `MONTRACER_PUBLIC_WEB_URL`(선택, alert-worker·control-api 같은 값). 로컬 `make dev`는 `http://localhost:15173`이다. `MONTRACER_NOTIFY_RENOTIFY_ENABLED`(alert-worker, 기본 true).

## Rollout

1. migration 00011
2. control-api·alert-worker 배포(`MONTRACER_PUBLIC_WEB_URL` 설정)
3. receiver 안내(RB04): 쉼표 목록 서명 검증(첫 secret 교체 전에 필수), `alert.renotified`·`notification.test` 처리, `links`

## Rollback

- **binary만 되돌리기(migration 유지):** schema가 확장만 했으므로 이전 binary가 그대로 돈다(같은 UNIQUE·ON CONFLICT).
  - 재알림을 더 쌓지 않는다. 이미 쌓인 재알림·시험은 이전 binary도 보낸다(같은 전송 경로). 다만 시험 실패를 24시간 재시도하고 지표 outcome이 `test`가 아니다.
  - 이전 binary는 이전 secret을 모른다. 겹침 중에 되돌리면 지금 secret만으로 서명한다(교체 전 secret을 쓰는 receiver는 거절한다).
  - 이전 control-api의 timeline은 재알림 종류를 그대로(`alert.renotified.k`) 보인다.
- **재알림만 끄기:** alert-worker의 `MONTRACER_NOTIFY_RENOTIFY_ENABLED=false`. 열림·닫힘 알림은 그대로다. 이미 쌓인 재알림은 보낸다.
- 시험 전송·교체만 끄기: policy API 전체 flag(ADR 0053, master key를 비우면 404)를 따른다. 따로 flag를 두지 않는다. 둘 다 Tenant Admin이 직접 부르는 동작이고, 저장 데이터에 남는 위험이 없다.
- deep link 끄기: `MONTRACER_PUBLIC_WEB_URL`을 비운다.
- migration 00011 down은 재알림·시험 행을 지운다(이전 schema에 없는 종류). 그 뒤 이전 secret 열을 지우고 종류 CHECK·NOT NULL을 되돌린다. 이전 secret으로 서명하던 겹침은 끝난다.
  - 지우는 동안만 FORCE RLS를 푼다. superuser·BYPASSRLS가 아닌 owner 계정은 tenant 설정 없이 FORCE RLS 표에서 0행을 본다(PS-0010과 같은 함정). 그러면 뒤의 NOT NULL 되돌리기가 실패한다.

## 재검토 조건

- escalation(F11): 3번 뒤 다른 policy·on-call로 넘기기, mute·silence와 재알림의 관계
- 조직 slug가 생기면 deep link 경로를 slug로 바꾼다(tenant id 주소는 redirect로 유지)
- 재알림 간격·횟수를 monitor마다 정하기(Datadog `renotify_interval`·`renotify_occurrences`처럼): 지금은 D02 §17 시작값 고정
- 시험 결과 목록(policy별 최근 시험) 화면: notification policy 화면과 함께
- master key 회전 뒤 이전 secret 암호문도 다시 암호화하는 절차(ADR 0053 재검토 조건과 같음)

## 증거

- `internal/notify/notify_test.go`: 겹침 기간 두 서명(key id·서명 순서, 각 secret으로 검증, 무관한 secret 거절), deep link(형식 거절 6종, 빈 값, localhost http, 절대 시각·초 절삭, 7일 줄임), 본문 종류(시험 payload에 경보 필드 없음, 재알림 번호·links null)
- `internal/alertworker/notifier_test.go`: 시험 전송(재시도 없음, 꺼진 policy에도 보냄, 지운 policy 취소, outcome test), 이전 secret 함께 서명·풀 수 없으면 지금 secret만
- `internal/controlapi/notification_ops_test.go`: 시험(Developer 403, 202·Location, 진행 중 재사용 경고, 결과 조회, 다른 policy 404, 형식 400, 한도 429·Retry-After), 교체(428·403·형식 400 3종, 새 secret·이전 key id·만료·기본 24시간·no-store·ETag, 낡은 If-Match 412, 0이면 이전 없음)
- `internal/controldb/notification_ops_integration_test.go`(PostgreSQL)
  - 재알림: 30분 전 없음, 31분 1번째(같은 번호 하나), RECOVERING 동안 없음, 복귀 때 하나·바로 다음 평가에는 없음(직전 재알림 30분), 30분 뒤 3번째, 3번 상한. 본문 번호(저장 종류와 같음)·상태·값·policy, 열림 deep link
  - 재알림 끄기: 때가 지나도 열림 알림만
  - 시험: Developer 403·다른 tenant 404, 진행 중 재사용, 시험 종류 claim·실패 결과 조회, 꺼진 policy, 1시간 20번 한도, security 감사 수
  - 교체: Developer 403, 낡은 revision, 새·이전 secret이 claim에서 함께 읽히고 각각 풀림, 겹침이 지나면 이전 없음, 겹침 중 재교체는 이전 secret·만료 유지, policy 수정(PUT)은 secret 열을 바꾸지 않음, 0이면 바로 바꿈, 감사에 key id만(평문 없음), 24시간 초과 거절
- `internal/controldb/notifications_integration_test.go`: 앱 role은 만든 사람·tenant 수정, policy·본문 삭제 불가(42501)
- `apps/web/src/features/monitors/Monitors.test.tsx`: timeline 재알림 "재알림 2/3", 모르는 종류(이전 API의 저장 종류)는 닫힘으로 짐작하지 않고 원문
- PostgreSQL migration up → down → up(로컬, 재알림·시험 행이 있는 상태에서 down)
- spec-reviewer: P1 1건(복귀 뒤 재알림이 1분 간격으로 몰림)·P2 5건(owner 계정 down, 겹침 중 재교체가 receiver secret을 끊음, "하위 호환" 표현, 모르는 종류를 닫힘으로 표시, 재알림 flag·PUT secret 보존 시험) 반영
