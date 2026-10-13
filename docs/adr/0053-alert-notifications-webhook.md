# ADR 0053: 경보 알림 — notification policy와 webhook 전송

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-10 E05 단계 C 범위 선택 "C1: webhook 종단")
- Owner: API lead
- 날짜: 2026-10-10 (제안·결정)
- 관련: F06, E05 · D02 §14·§17·§20, D04 §01·§03·§11(RB04), D05 §09, D06 §05 · 계약 1·3·6 · ADR 0016, 0049, 0051

## 배경

ADR 0051로 monitor가 평가되고 경보 상태·사건(episode)·전이 event가 남는다. 이 ADR은 그 사건을 **webhook으로 알리는** 경로다. D02와 D04가 정한 것은 다음과 같다.

- D02 §17
  - 상태 전이와 notification outbox는 같은 transaction에 쓴다.
  - webhook은 HTTPS만 쓰고 timestamp·event_id·본문에 HMAC 서명한다. 5분 넘은 서명은 receiver가 거절한다.
  - 사설 IP, metadata 주소, DNS rebinding을 차단한다.
  - 전송은 at-least-once이고 event_id를 준다. 중복 억제 key는 tenant + monitor + group + episode_id다.
  - 실패하면 지수 backoff로 최대 24시간 재시도하고, 실패는 DLQ와 독립 경보로 다룬다.
- D02 §20 envelope
  - event_id, event_type, tenant_public_id, occurred_at, resource_id, revision, payload를 보낸다.
  - raw telemetry는 기본으로 첨부하지 않는다.
  - HMAC은 timestamp와 원문 body bytes를 서명하고, key_id를 둔다.
- D04 §03: webhook SSRF는 egress proxy, IP 검증, DNS 재검증으로 막는다.
- D04 §11 RB04: 평가 실패와 전송 실패를 나누고, event_id 기준으로 replay한다.
- D02 §14: monitor의 notification policy는 같은 tenant에 속하고 접근 가능한지 저장 전에 검증한다.

명세가 정하지 않은 것도 있다.
- policy 자원의 모양과 권한
- 서명 secret을 만들고 저장하는 방법
- 무엇을 알리는가
- outbox를 어떻게 소비하는가
- 재알림(30분×3)과 webhook 시험 전송은 이번 범위(C1)에서 빠진다.

## 결정

### 1. notification policy (migration 00009, `controldb.NotificationPolicyStore`, `/api/v1/notification-policies`)

- **자원:** `{name, webhook: {url}, enabled}`다. 목록·단건·생성(Idempotency-Key)·수정(If-Match, 428/412)·삭제(tombstone)는 monitor API(ADR 0049)와 같은 규약이다.
- **권한**
  - 쓰기는 `policies.write`(Tenant Admin)다. policy는 고객 데이터가 나가는 목적지와 서명 secret을 정하기 때문이다.
  - 조회는 `monitors.read`다. Developer가 monitor에 붙일 policy를 고를 수 있어야 한다.
  - environment 제한 key는 쓰기 403이다(ADR 0051 §3).
- **URL**
  - HTTPS만 받는다. host가 있어야 하고 userinfo·fragment는 없어야 한다.
  - localhost·`.local`·`.internal`, 사설·예약 IP literal, 숫자 표기 host(`127.1`, `2130706433`, `0x7f000001`)는 거절한다(400).
  - path·query에는 receiver token(Slack 형식 등)이 흔하다. 그래서 **URL 전체는 암호문으로만 저장한다**(secret과 같은 master key, AAD = 용도·tenant·policy).
  - 응답·감사에는 **scheme·host만** 보인다(`webhook_display`). salt 없는 hash도 남기지 않는다. 추측한 URL을 확인하는 데 쓰일 수 있다(리뷰 반영).
- **서명 secret**
  - 서버가 32 byte로 만든다(`whsec_…`, key id `wk_…`). **생성 응답에서 한 번만** 보인다.
  - Idempotency-Key 재생 응답에는 secret이 없고 경고 `signing_secret_not_replayed`가 붙는다. 저장하는 응답 본문에 평문을 두지 않기 위해서다.
  - 저장은 AES-256-GCM 암호문이다.
    - key: master key(`MONTRACER_NOTIFY_KEY_HEX`, 32 byte 이상, secret manager)의 SHA-256
    - AAD: tenant·policy·key id. 다른 행으로 옮긴 암호문은 풀리지 않는다.
    - master key 버전(`sealing_key`)을 같이 저장한다.
  - 앱 role은 secret 열을 바꿀 수 없다. 교체 API는 후속(C2)이다.
- **monitor 연결:** `notification_policy_id`(소문자 UUID)를 받는다. spec과 같은 값을 `monitors.notification_policy_id` 열에도 둔다(같은 tenant FK).
  - 생성·수정과 같은 트랜잭션에서 policy가 이 tenant에 살아 있는지 공유 잠금(`FOR SHARE`)으로 본다. 없거나 다른 tenant 것이면 같은 400(`notification_policy_id: not found`)이다(D02 §14).
  - **살아 있는 monitor가 가리키는 policy는 지울 수 없다(409, 리뷰 반영).** 지우면 그 monitor의 알림이 기록 없이 끊긴다. 지우기는 policy 행을 먼저 잠그므로 동시 monitor 저장과 직렬화된다.
- **변경 감사:** security 범주다. 데이터가 나가는 목적지가 바뀌기 때문이다.
- master key가 없으면 policy API는 404이고, 전송 루프는 꺼진다(rollback).

### 2. 무엇을 언제 쌓는가 (`notification_deliveries`)

- **사건 열림(`alert.opened`)과 닫힘(`alert.resolved`)만 알린다.** PENDING·RECOVERING·NO_DATA·EVALUATION_ERROR 상태 변화는 알림이 아니다(outbox `alert.state_changed`와 S09 timeline에는 남는다).
  - no_data alert 정책으로 열린 사건(episode_reason `no_data`)은 알린다.
- **같은 트랜잭션:** 열림은 `CompleteEvaluation`이 이 revision 정의의 policy로 쌓는다. 닫힘은 열림 알림을 받은 policy에만 쌓는다.
  - 열림을 못 받은 곳에 닫힘만 가지 않는다.
  - 정의에서 policy를 떼도 열린 사건의 닫힘은 간다.
- **정의 변경·끄기·삭제**로 닫힌 사건(ADR 0051 §4)도 닫힘을 알린다. `close_reason`은 `monitor_revised|monitor_disabled|monitor_deleted`이고 state는 null이다. 복구로 닫히면 `recovered`다.
- **중복 억제:** UNIQUE(tenant, policy, monitor, group_hash, episode_id, event_type)다(D02 §17의 key + 종류). 재평가·중복 scheduler가 같은 사건을 다시 써도 알림은 하나다.
- 꺼진 policy의 알림은 보낼 때 `canceled`가 된다. 낡은 revision 평가가 지워진 policy로 사건을 열면, 조용히 버리지 않고 `canceled`(`policy_deleted`) 행으로 남긴다.
- **본문은 쌓을 때 만든다.** 재시도에도 같은 bytes를 보낸다. envelope은 D02 §20을 따른다.
  - `event_id`는 알림 id다.
  - `tenant_public_id`는 tenant id다(별도 공개 id가 아직 없다).
  - `resource_id`는 monitor, `revision`은 monitor revision이다.
  - payload: monitor 이름, group_hash, labels, episode, state, value(null이면 reason, 계약 6), window_end, close_reason
  - raw telemetry는 없다.
  - 본문이 64KiB를 넘으면 labels를 빼고 `labels_truncated: true`로 보낸다. 경보 저장을 막지 않는다.
  - **본문은 별도 표(`notification_delivery_bodies`, tenant 정책만)에 둔다(리뷰 반영).** tenant를 가로지르는 scan이 보는 `notification_deliveries`에는 id·종류·시각·시도 수·결과 code만 있다.
- **보존:** 끝난 알림은 30일 뒤 지운다(본문은 CASCADE). 아직 열린 사건의 열림 기록은 남긴다(닫힘을 보낼 곳이다).
  - 전송 루프가 10분마다 지울 것이 있는 tenant를 찾아 tenant 트랜잭션에서 지운다. 알림이 끊긴 tenant도 지워진다(리뷰 반영).
  - tenant는 id 순 cursor로 100개씩 돈다(재검토 반영). 열린 사건 때문에 매번 대상이 되는 tenant가 있어도 다른 tenant가 밀리지 않는다.
  - 삭제 원장(F09)이 생기면 이 두 표를 삭제 대상에 등록한다.

### 3. 전송 (`alertworker.Notifier`, alert-worker 안의 두 번째 루프)

- **outbox dispatcher를 따로 두지 않는다.** 알림 행이 곧 전송 큐다(transactional outbox의 한 형태). 할 일 찾기는 `delivery_scan` 정책으로 tenant를 가로질러 SELECT만 한다(monitor_scan과 같은 방식, ADR 0051). 나머지는 tenant 트랜잭션이다.
- **lease:** claim마다 token을 쓴다(1분). 결과 저장은 token이 맞을 때만 한다. lease를 잃으면 버린다.
- **at-least-once:** 보낸 뒤 결과를 쓰기 전에 죽으면 lease 만료 뒤 다시 보낸다. 같은 event_id, 같은 본문이다.
  - **시도 수는 lease를 잡을 때 늘린다(리뷰 반영).** 그래서 재전송도 `Montracer-Delivery-Attempt`가 늘어난다(D04 §11 "중복 가능성 표시").
  - 종료로 끊긴 전송은(24시간 창의 마지막 시도라도) 결과를 쓰지 않는다. 포기로 기록하지 않는다.
- **결과**
  - 2xx: delivered
  - 그 밖(3xx·4xx·5xx·연결·TLS·timeout): 지수 backoff(30초에서 두 배씩, 1시간 상한)로 created로부터 24시간까지 다시 보낸다. 그 뒤에는 `failed`(DLQ)다.
  - 영구 실패는 바로 `failed`다: 사설 주소 차단, URL 형식, secret을 풀 수 없음.
  - 꺼진·지운 policy: `canceled`(시도 수에 넣지 않음)
- **서명**
  - header: `Montracer-Event-Id`, `Montracer-Timestamp`(Unix 초), `Montracer-Key-Id`, `Montracer-Signature: v1=<hex>`
  - 서명 값: HMAC-SHA256(secret, `timestamp.event_id.원문 body`)이다. D02 §17의 timestamp·event_id·본문과 §20의 원문 bytes를 모두 덮는다.
  - 시도마다 새 timestamp로 서명한다.
  - receiver는 5분 허용 범위로 검증한다. 참조 구현은 `notify.Verify`, 안내는 RB04에 있다.
- **SSRF 방어**
  - 저장할 때 URL을 검사한다(§1).
  - 보낼 때는 dialer `Control`이 **실제로 연결하는 주소**를 검사한다. 이름 해석 뒤의 주소라 DNS rebinding과 여러 주소로 풀리는 이름에도 작동한다.
  - 막는 범위:
    - loopback·사설(10/8, 172.16/12, 192.168/16, fc00::/7)
    - link-local(169.254/16, cloud metadata 포함, fe80::/10)
    - CGNAT·multicast·예약·문서용
    - NAT64·6to4, IPv4-mapped IPv6의 사설 주소
  - redirect는 따라가지 않는다(3xx는 실패). 환경 변수 proxy를 쓰지 않는다(검사를 우회한다). TLS 1.2 이상이다.
  - 응답 본문은 4KiB까지만 읽고 버린다(저장·로그하지 않는다).
  - D04 §03의 egress proxy는 배포 구성에서 추가한다. 그때도 dialer 검사는 남긴다.
- **한도:** 요청 하나 10초, 동시 전송 8, batch 100, tick 2초다.
  - **policy 하나는 tick마다 5개까지 차례로 보낸다(리뷰 반영).** 응답 없는 endpoint 하나가 동시 전송 자리를 모두 잡아 다른 tenant의 알림을 막지 않게 한다. policy끼리는 동시에 보낸다.
  - 이 상한은 scan에서 건다(policy별 `row_number`, 재검토 반영). 한 policy에 알림이 몰려도 batch 100을 혼자 채우지 못한다.
- **master key 회전:** 새 key를 현재 버전으로 두고 이전 key를 `MONTRACER_NOTIFY_PREVIOUS_KEYS`(`버전:hex`)로 함께 준다.
  - 새로 암호화할 때는 현재 버전을 쓰고, 풀 때는 저장된 버전을 쓴다. URL은 policy를 수정하면 현재 버전으로 다시 암호화된다.
  - 이전 key를 빼먹으면 그 policy의 알림이 `secret_unavailable`로 포기된다(경보).
  - control-api는 암호화만 하므로 이전 key가 필요 없다. 풀 수 있는 key를 control-api에서 떼는 envelope/KMS 분리는 재검토 조건이다.

### 4. 운영

- **지표**
  - `montracer_notification_deliveries_total{outcome=delivered|retry|failed|canceled}`
  - `montracer_notification_delivery_duration_seconds`
  - `montracer_notification_backlog_seconds`(한 번도 시도하지 않은 가장 오래된 알림의 나이)
  - `montracer_notification_errors_total{stage=scan|deliver|prune}`
  - **master key가 없어도 전송 루프는 돈다(리뷰 반영).** 보내지 않고 backlog 지표만 낸다. 그래서 key 주입이 빠진 배포는 `MontracerNotificationBacklog`로 드러난다.
  - label에 tenant·policy·URL을 넣지 않는다.
- **경보**(RB04에 절이 있다)
  - `MontracerNotificationBacklog`(5분 넘게 미시도, page): 고객 endpoint 장애(재시도 중)는 세지 않는다.
  - `MontracerNotificationWorkerFailing`(10분째, page)
  - `MontracerNotificationDeliveryFailed`(30분 안에 포기한 알림, ticket — D02 §17 "DLQ와 독립 경보")
- **로그:** URL·본문·secret을 남기지 않는다. tenant·알림 id·결과 code만 남긴다.

## 외부 사례 근거 (2026-10-10 확인)

| 사례 | 내용 | 반영 |
|---|---|---|
| Stripe webhooks ([docs](https://docs.stripe.com/webhooks)) | `timestamp.payload`를 HMAC-SHA256으로 서명한다. 기본 허용 5분이고, 재시도마다 새 timestamp·서명을 만든다. HTTPS만 받고, 3xx는 실패다. 지수 backoff로 최대 3일 재시도한다. receiver는 event id로 중복을 거른다. 서명 secret은 `whsec_`이고 회전 시 24시간 겹친다 | §3 서명(event_id를 더함)·허용 5분·redirect 실패·재시도(D02 §17의 24시간)·event_id 중복 제거, §1 `whsec_` 형식. 회전은 C2 |
| Standard Webhooks ([site](https://www.standardwebhooks.com/)) | 모든 webhook 구현은 SSRF·위조·재전송 공격을 막아야 한다 | §3 SSRF, 서명, timestamp 허용 범위 |
| OWASP SSRF Prevention Cheat Sheet ([link](https://cheatsheetseries.owasp.org/cheatsheets/Server_Side_Request_Forgery_Prevention_Cheat_Sheet.html)) | 이름 해석 뒤 주소를 검사하고, redirect를 따라가지 않고, 내부·metadata 주소를 막는다 | §3 dialer Control 검사·redirect 금지·차단 범위 |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 큐 | 알림 행 = 전송 큐(경보와 같은 트랜잭션) | 범용 outbox dispatcher + Kafka: 아직 dispatcher가 없고, tenant별 policy 조회·lease를 결국 같은 DB에서 한다 |
| 알림 대상 | 사건 열림·닫힘 | 모든 상태 전이: PENDING·NO_DATA 깜빡임이 알림 폭주가 된다(D02 §17 재알림 정책과도 맞지 않음) |
| secret 보관 | 서버 생성, 1회 표시, AES-GCM(AAD) | 평문 저장: DB 유출이 곧 위조다. 사용자가 secret 지정: 약한 secret, 재사용 |
| secret 재표시 | 하지 않음(교체로 새로) | Stripe처럼 다시 보기: master key로 풀어 보여주는 경로가 생긴다. 회전 API(C2)로 대신한다 |
| SSRF | 연결 직전 주소 검사 + redirect 금지 + proxy 무시 | 저장 때 DNS 검사만: rebinding에 뚫린다. allowlist만: 고객 endpoint를 미리 알 수 없다 |
| 쓰기 권한 | Tenant Admin(policies.write) | Developer(monitors.write): 누구나 데이터 목적지를 바꿀 수 있다 |
| 닫힘 대상 | 열림을 받은 policy | 현재 정의의 policy: 정의를 바꾸면 열림·닫힘이 다른 곳으로 간다 |
| 쓰이는 policy 삭제 | 409 | 허용하고 새 알림을 버림: 경보가 기록 없이 사라진다. 허용하고 취소 행만 남김: 사람이 볼 때는 이미 늦다 |
| scan 범위 | 상태 표만(본문 분리) | 한 표에 본문까지: scan 설정 하나로 모든 tenant의 labels·값이 보인다 |
| URL 저장 | 암호문 + 표시용 host | 평문: 백업·복제본에 receiver token이 남는다 |

## 결과

- 경보 사건이 서명된 webhook으로 나간다. 재알림(30분×3·escalation), 시험 전송, secret 교체는 단계 C2다. Slack·이메일 connector(D02 §17 별도 worker)와 S09 화면은 후속이다.
- **schema:** PostgreSQL migration 00009
  - 표: `notification_policies`, `notification_deliveries`, `notification_delivery_bodies`
  - `monitors.notification_policy_id` 열(같은 tenant FK)
  - RLS ENABLE+FORCE, `delivery_scan` 정책(상태 표만)
  - 최소 GRANT: secret 열 UPDATE 없음, policy DELETE 없음, 본문 UPDATE·DELETE 없음(CASCADE로만 지움)
- **API:** `/api/v1/notification-policies` CRUD. monitor `notification_policy_id`는 422(미지원)에서 검증 후 저장으로 바뀐다.
- **설정:** control-api와 alert-worker에 `MONTRACER_NOTIFY_KEY_HEX`·`MONTRACER_NOTIFY_KEY_VERSION`(같은 값)을 둔다. 회전 중에는 alert-worker에 `MONTRACER_NOTIFY_PREVIOUS_KEYS`를 둔다.
- **D02 §20 deep link(명세 차이, 계약 7):** envelope에 "인증 필요 deep link"가 아직 없다. 공개 base URL 설정과 S09 화면 경로(단계 D)가 정해진 뒤 C2에서 `payload.links`로 더한다. 사용자에게 알린 미룬 항목이다.
- **tenant 영향:** 할 일 찾기만 tenant를 가로지르고 id·시각만 읽는다. secret 암호문은 tenant·policy에 묶인다.
- **retention 영향:** 끝난 알림은 30일 보존한다(열린 사건의 열림 기록은 예외).

## Rollout

1. migration 00009
2. master key를 secret manager에 넣고 control-api·alert-worker에 같은 key·버전을 준다.
3. control-api(policy API), alert-worker(전송 루프), 경보 규칙을 배포한다.
   - key 없이 alert-worker를 배포하면 알림이 쌓이기만 하고 `MontracerNotificationBacklog`가 울린다.

## Rollback

- 전송만 끄기: alert-worker의 `MONTRACER_NOTIFY_KEY_HEX`를 비운다. 알림은 쌓이고, 다시 켜면 보낸다(24시간 창 밖이면 한 번 시도 뒤 포기).
- policy API 끄기: control-api의 key를 비운다(404). 정의에 붙은 policy id는 남는다.
- migration 00009 down은 세 표와 monitors 열을 지운다.
- **이전 binary로 되돌릴 때(리뷰 반영):** 이전 `monitor.Normalize`는 `notification_policy_id`를 미지원(422)으로 거절한다. 그래서 policy가 붙은 monitor는 평가 때 `invalid_spec`(EVALUATION_ERROR)가 된다.
  - 되돌리기 전에 모든 monitor에서 policy를 뗀다(PUT, `notification_policy_id: null`).
  - 또는 전송만 끄는 위 방법을 쓴다.

## 재검토 조건

- 단계 C2: 재알림(D02 §17 30분, 최대 3회 후 escalation), webhook 시험 전송(D05 §09 "실제 알림과 분리"), secret 교체(겹침 기간), mute 배지
- master key 회전 뒤 남은 이전 버전 암호문을 일괄 다시 암호화하는 절차(지금은 policy 수정 때 URL만 다시 암호화)
- control-api에서 풀 수 있는 key 떼기(D04 §02 role별 decrypt scope): envelope 암호화/KMS
- 알림 행 시각(`next_attempt_at` 최초값은 DB 시각, lease 비교는 worker 시각) 혼용: 시계 차이가 lease보다 크면 일찍 가로챌 수 있다(at-least-once는 유지)
- egress proxy 도입(D04 §03): 배포 구성
- 알림 수가 늘어 전송 처리량이 부족할 때: tenant별 공정성(한 tenant의 느린 endpoint가 다른 tenant 전송을 늦추지 않게)
- tenant 공개 id가 생기면 `tenant_public_id`를 바꾼다(schema_version 올림)

## 증거

- `internal/notify/notify_test.go`
  - 공개 주소 판정 31종: metadata, IPv4-mapped, NAT64, 6to4, IPv4-compatible, site-local, Teredo 포함(IPv6는 2000::/3 허용 목록)
  - 숫자 표기 host 거절, URL 암호문(용도·tenant 바인딩), 이전 key로 풀기(회전)
  - URL 검사(https·host·자격 증명·fragment·localhost·사설 literal), URL 가리기
  - 암호화: tenant·policy·key id·버전·다른 master key로는 풀리지 않음
  - 서명·검증(본문·event id·key·5분 초과·timestamp 형식)
  - backoff·24시간 포기
  - TLS 서버로 보내 receiver 서명 검증, loopback 연결 차단(영구), redirect를 따라가지 않음, 503, timeout, 신뢰하지 않는 인증서
- `internal/controlapi/notifications_test.go`
  - secret은 생성 응답에만 있고 저장 암호문과 같다. 재생은 secret 없음·경고, 저장된 재생 본문에 평문이 없다. URL은 host만이다.
  - 형식 400 7종, 권한(Developer 쓰기 403·조회 200, Viewer 조회 403)
  - If-Match 428/412, 수정에도 secret 유지, 삭제 404, key 없으면 404
- `internal/controlapi/monitors_test.go` `TestMonitorNotificationPolicy`: 없는 policy 400, 있으면 정규형에 남는다.
- `internal/alertworker/notifier_test.go`
  - delivered(풀린 URL·서명 key·event id·claim이 늘린 시도 번호)
  - 재시도 30초, 24시간 포기, 사설 주소 영구 실패
  - policy 꺼짐 → canceled(전송 없음), 풀 수 없는 secret → failed(전송 없음)
  - 다른 worker가 잡음, lease 잃음, 종료 중 결과를 쓰지 않음(24시간 창의 마지막 시도 포함)
  - master key 없음 → 보내지 않고 backlog만, policy당 tick 상한·차례 전송, 주기 정리
- `internal/controldb/notifications_integration_test.go`(PostgreSQL)
  - policy CRUD·재생·암호문(secret·URL)은 그 tenant에서만 풀림·URL token이 평문으로 없음·security 감사 3건(URL token 없음)·Developer 쓰기 403·다른 tenant 404
  - 앱 role은 secret 열 UPDATE·tenant_id UPDATE·policy DELETE·본문 UPDATE·DELETE 불가(42501)
  - monitor가 가리키는 policy 삭제 → `ErrPolicyInUse`(tombstone 안 남음), 낡은 revision이 지운 policy로 연 사건 → canceled 행
  - 알림 쌓기
    - 없는 policy를 가리키면 저장 거절
    - 열림 envelope 내용, 같은 사건 중복 없음, 복구 → resolved(recovered)
    - 정의 변경 → resolved(monitor_revised, state null, policy를 뗀 뒤에도)
    - 지운 policy는 새 알림 없음
  - 큐
    - 다른 tenant 이름으로 claim·완료 불가, token이 다르면 쓰지 않음, 재시도 시각 전 다시 잡히지 않음
    - scan 정책은 상태 표 SELECT만(본문·policy·다른 표 안 보임, UPDATE 0행)
    - 시도 수는 claim마다 1씩 는다
    - 주기 정리: 30일 지난 기록 삭제(열린 사건 예외), 본문 CASCADE
    - scan 공정성: 한 policy에 30건이 몰려도 policy당 5개만 골라 다른 policy 알림이 함께 나온다
- `internal/alertworker/integration_test.go` `TestNotificationEndToEnd`(PostgreSQL): 정의 → 사건 열림 → 같은 트랜잭션의 알림 → 전송 루프가 암호문을 풀어 서명해 보낸다. 503 → backoff 전 재전송 없음 → 같은 event_id·본문으로 재전송 → 성공 뒤 재전송 없음.
- `tests/isolation` `TestNotificationPoliciesAcrossTenants`: B는 A의 policy를 볼 수도 바꿀 수도 없다(404, header 무시). B의 monitor가 A의 policy를 가리키면 없는 것과 같은 400이다.
- `deploy/prometheus/rules/montracer.rules.test.yml`: backlog·worker 실패·포기 경보와 반례
- PostgreSQL migration up → down → up
- spec-reviewer: P1 2건(key 없을 때 backlog 경보가 울리지 않음, 지운 policy로 알림이 조용히 끊김)·P2 11건 반영, 재검토 P1 1건(scan이 공정성 없이 batch를 한 policy로 채움)·P2 1건(정리 tenant 목록 굶주림) 반영. F06 registry 상태는 Gate 증거 전이라 `planned` 유지

## 변경 이력

- 2026-10-13 (ADR 0055, 단계 C2): 재알림(`alert.renotified`, 30분×3), 시험 전송(`notification.test`, `POST …/{id}/test`), secret 교체(`POST …/{id}/rotate-secret`, 겹침 동안 두 서명), 본문 `payload.links`(deep link)가 더해졌다.
  - §1 "앱 role은 secret 열을 바꿀 수 없다"는 바뀌었다. 교체 API만 secret 열을 쓴다.
  - §3 서명 header는 겹침 기간에 쉼표 목록이다.
  - §결과의 D02 §20 deep link 명세 차이는 해소됐다.
- 2026-10-13 (ADR 0056): PUT에서 `webhook`을 빼면 저장된 URL(암호문·host)을 그대로 둔다(이전에는 400). `enabled`를 빼면 저장된 값을 그대로 둔다(이전에는 켜짐). 감사 details에 `webhook_changed`가 더해졌다. 알림 policy 화면(Respond > Notifications)이 생겼다.
