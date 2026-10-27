# ADR 0071: 가동 점검(F20 일부)을 G1로 앞당긴다

- 상태: 승인 (polynomeer, 2026-10-27 — 채팅 "승인할게". 단계 변경이라 결정 위임이 아니라 직접 승인을 받았다)
- Owner: Platform lead (E09)
- 날짜: 2026-10-19 (제안) / 2026-10-27 (결정)
- 관련: F20, F06, F11, E05·E09 · D01 §05~06, D02 §03·§04·§17·§18·§20, D03 §11, D04 §01·§02·§10·§13·§14·§15, D05 §01·§03·§04·§08, D06 §01~02·§05(T08)·§06·§09·§11 · 계약 1·2·3·4·6 · ADR 0049~0055, 0059

## 배경

사용자 요청(2026-10-19)은 Uptime Kuma 정도의 기본 가동 감시를 G1에 넣는 것이다. 범위는 HTTP·TCP·TLS·DNS 점검과 heartbeat(push) 점검이다. 다음 항목은 이 ADR에서 다루지 않는다.
- 공개 상태 페이지
- Slack·이메일 알림 채널 (D02 §17에 "별도 connector worker"로 이미 있다)

명세의 현재 상태는 다음과 같다.

- **D01 §06:** F20은 "API Synthetic, 사설 runner, 장애와 runner 중단 구분"이고 목표는 G2다. heartbeat 점검은 F ID가 없다.
- **D03 §11:**
  - G2 API test는 HTTP·multi-step HTTP·TLS 만료·DNS·connect·TTFB를 제공한다.
  - 대상 소유권 확인, 위험 URL denylist, metadata·loopback·사설 주소 차단을 적용한다.
  - `test_run_id`는 schedule slot·test revision·location으로 정하고, lease로 중복 실행을 막는다.
  - retry 결과와 최초 실패를 모두 남긴다. location quorum과 `runner_missing`을 `target_failure`와 분리한다.
  - 기본 interval은 1분, timeout은 60초다.
  - 고객별 격리 container와 egress proxy를 운영한다.
- **D02 §18:** synthetic_run의 키는 test·revision·slot·location·attempt이고, 저장은 PG 상태 + CH 결과다.
- **D02 §20:** `POST /synthetic-tests`는 `synthetics.write` 권한이고 location 사용권을 추가로 검증한다.
- **D04 §15 RB08:** runner 침해를 가정한다. 결과를 대상 서비스 장애로 집계하지 않는다.
- **D04 §13 과금:** synthetic run은 "시작한 test slot과 location의 run"이다. 플랫폼 실패와 내부 retry는 세지 않는다.
- **D06 §01:** G1(17~32주)은 F08~F14, G2(9~15개월)는 F15~F20·F22다. G1이 늦어져도 보안·정확성(P0)은 줄이지 않는다.
- **D06 §05 T08:** SSRF·DNS rebind·중복 slot·runner kill 시험이 Gate이고, 합격 기준은 "외부/사설 경계 유지, 이중 청구 0"이다.
- **이미 있는 것:**
  - `cmd/synthetic-runner`는 README만 있고 단계는 G2로 적혀 있다.
  - monitor 평가·상태 머신·webhook 전송은 ADR 0049~0055로 구현되어 있다.
  - 연결 직전 주소 검사는 `internal/notify/egress.go`(ADR 0053)에 있다. 이 검사는 redirect를 따라가지 않는다.

지금 정하는 이유는 두 가지다.
1. metric monitor(F06)는 계측된 서비스 안쪽만 본다. 계측하지 않은 외부 endpoint, 인증서 만료, cron·batch 작업이 죽었는지는 G2 전까지 볼 수 없다.
2. 고객은 이 공백을 별도 도구(Uptime Kuma 등)로 메운다. 그러면 경보가 두 곳으로 나뉜다.

## 결정

F20을 둘로 나눈다.
- **F20a 가동 점검:** G1.
- **F20b 고급 API Synthetic:** G2. multi-step HTTP, 사설 location, 다지역 location, 인증이 필요한 점검이 여기에 들어간다.

F20a의 범위와 동작은 다음과 같다.

### 1. 점검 종류

| 종류 | 확인하는 것 | 실패 조건 |
|---|---|---|
| `http` | 상태 코드(기본 2xx·3xx), 응답 시간, 선택 사항으로 본문 문자열 포함·불포함. 단계별 시간(DNS·connect·TLS·TTFB)을 기록한다 | 코드 불일치, 문자열 조건 불일치, 확인되지 않은 도메인으로 redirect, timeout |
| `tcp` | host:port 연결 | 연결 거부, timeout |
| `tls` | 인증서 체인 검증, 남은 유효 일수 | 검증 실패, 남은 일수 < 임계값(기본 14일) |
| `dns` | 레코드 조회(A·AAAA·CNAME·MX·TXT), 선택 사항으로 기대 값 일치 | NXDOMAIN·SERVFAIL, 값 불일치, timeout |
| `heartbeat` | 고객 작업이 주기마다 보내는 신호 | 기한(period + grace) 안에 신호 없음, 또는 실패 신호 |

**G1에서 받지 않는 입력**
- 인증 header·basic auth·cookie 같은 secret은 받지 않는다. 필요하면 vault ref가 있는 F20b에서 다룬다.
- URL의 userinfo는 거절한다(422).
- query string에 secret으로 보이는 key(`token`·`key`·`secret`·`password` 등)가 있으면 경고하고 저장을 막는다(422).

**제외하는 종류와 이유**
- **ICMP ping:** runner에 `NET_RAW` 권한이 필요해 runner 격리 위험이 커진다. TCP 점검으로 대신한다.
- **WebSocket·gRPC·UDP:** G2 F20b에서 다룬다.
- **DB 직접 접속:** 고객 credential과 사설망 접근이 필요해 사설 location(F20b) 없이는 할 수 없다.
- **Docker 컨테이너 상태:** runner에 Docker socket을 mount하지 않는다(D06 §11). 컨테이너 상태는 F18 infra 수집으로 본다.

### 2. 실행 위치와 판정 (계약 6)

- **실행 위치:** G1은 Montracer 운영 지역 안의 서로 다른 AZ 2곳에서 같은 slot을 실행한다. location 단위는 AZ다. 다지역·사설 location은 F20b다.
- **quorum:** 판정하려면 두 AZ가 모두 응답해야 한다.
- **attempt:** 각 AZ에서 실패하면 같은 slot 안에서 한 번 재시도한다(attempt 2). 최초 실패와 재시도 결과를 모두 저장하고, 그 AZ의 결과는 마지막 attempt로 정한다(D03 §11).

**slot 판정**

| 두 AZ의 결과 | slot 판정 | 가동률 계산 |
|---|---|---|
| 둘 다 성공 | 성공 | 분모·분자에 넣음 |
| 둘 다 실패 | 실패 | 분모에 넣음 |
| 하나 성공, 하나 실패 | 일부 실패 | 분모·분자 모두에서 뺌 |
| 하나만 응답 | quorum 미달 | 분모·분자 모두에서 뺌 |
| 응답 없음 | `runner_missing` | 분모·분자 모두에서 뺌 |

- 뺀 slot은 종류별 개수를 가동률 옆에 함께 보여 준다. 판정 못 한 slot을 성공으로 바꾸지 않는다.
- "일부 실패는 대상 장애보다 runner 쪽 경로 문제일 가능성이 크다"는 판단은 **가정**이다. pilot에서 측정해 확인한다(재검토 조건).
- **알려진 한계:** 한 지역에서만 점검하므로, 고객 사용자 쪽 지역 네트워크 장애는 보이지 않는다. 화면과 문서에 이 사실을 적는다.

### 3. 주기·timeout·한도 (모두 가정, 실측 후 조정)

| 항목 | 값 | 근거 |
|---|---|---|
| 점검 주기 | 기본·최소 1분, 최대 1시간 | D03 §11 기본 1분 |
| timeout | 기본 30초, 상한 60초 | D03 §11 상한. 1분 주기에서 재시도까지 slot 안에 끝나도록 기본값을 낮춤 |
| heartbeat period | 1분~31일 | Healthchecks.io의 period 개념을 참고한 자체 결정 |
| heartbeat grace | 1분~24시간 | Healthchecks.io의 grace time 개념을 참고한 자체 결정 |
| tenant당 점검 수 | 200개 | 원가 가정. entitlement로 조정 |

### 4. 대상 검증 (SSRF, D03 §11·D06 T08)

- **연결 직전 주소 검사:** `internal/notify/egress.go`(ADR 0053)의 검사를 공용 패키지로 옮겨 runner도 쓴다.
  - 이름 해석 결과가 사설·loopback·link-local·metadata·예약 주소이면 막는다. DNS rebinding도 이 단계에서 막힌다.
  - 환경 변수 proxy는 쓰지 않는다.
  - 여기에 더해 runner의 모든 egress는 egress proxy를 거친다(D03 §11, D04 §02). proxy도 같은 주소 정책을 적용한다(이중 방어).
- **소유권 확인:** 등록 도메인(Public Suffix List 기준의 eTLD+1)당 확인한다.
  - 방법: DNS TXT `_montracer-verify.<domain>` 또는 HTTP `/.well-known/montracer-verify/<token>`.
  - 확인 전에는 점검을 만들 수 없다(422).
  - 7일마다 다시 확인한다. 실패하면 그 도메인의 점검을 일시정지하고 tenant에 알린다. 도메인 소유자가 바뀌는 경우에 대비하려는 것이다.
  - IP literal 대상은 공개 주소만 허용하고, 따로 운영자 승인을 받는다.
  - 이유: 우리 runner가 남의 서버를 1분마다 두드리는 도구로 악용되지 않게 하려는 것이다.
- **redirect:** 최대 5번까지 따라간다.
  - hop마다 주소 검사를 하고, 확인된 등록 도메인 안에서만 따라간다.
  - 밖으로 나가는 redirect는 따라가지 않고 실패(`redirect_unverified`)로 판정한다.
  - 지금의 webhook 전송은 redirect를 따라가지 않는다. 이 동작은 runner에만 적용하고 webhook은 바꾸지 않는다.
- **DNS 점검의 resolver:**
  - 운영 설정의 고정 공개 recursive resolver만 쓴다. cluster resolver와 사용자 지정 resolver는 쓰지 않는다.
  - 내부 suffix(`.local`, `.internal`, `cluster.local`, 사설 대역의 `in-addr.arpa`·`ip6.arpa` 등)는 조회하기 전에 막는다.
  - 저장하는 값은 rcode와 기대 값 일치 여부뿐이다. 레코드 값 자체는 저장하지 않는다.
- **응답 처리:**
  - 응답 본문은 문자열 검사에만 쓰고 저장하지 않는다. 저장하는 것은 상태 코드, 시간, 검사 결과(일치 여부)다.
  - `tls`는 만료 시각과 검증 결과만 저장하고, 인증서 subject는 저장하지 않는다.
  - 개인정보가 처음 저장되는 길을 만들지 않으려는 것이다(계약 3).

### 5. heartbeat 수신 (계약 1·2)

- **endpoint:** ingress의 `POST /api/v1/heartbeats/{token}`. GET도 받는다(crontab의 `curl` 한 줄 지원).
- **token:** 점검 하나에만 쓸 수 있는 수집 credential이다(scope `heartbeat.write`).
  - 형식: 256-bit 무작위 값에 key_id prefix를 붙인다. keyed hash로 저장하고 constant-time으로 비교한다(D04 §02 key 관리).
  - tenant와 점검 id는 token으로 찾은 principal에서만 정한다. 요청 본문은 신뢰하지 않는다.
  - 교체하면 이전 token은 즉시 폐기한다.
- **token이 로그에 남지 않게 하기:** token이 URL path에 있으므로 ingress·LB access log와 platform trace에는 route template(`/api/v1/heartbeats/{token}`)만 남긴다. 이를 시험으로 확인한다(D04 §02 "secret은 기록하지 않는다").
- **ACK:** Kafka topic `heartbeat.v1`에 durable append한 뒤에만 2xx를 돌려준다. append에 실패하면 503이고 재시도는 호출자 몫이다.
- **받는 값:** `status`(`ok`·`fail`·`start`)와 선택 사항 `duration_ms`만 받는다. 자유 문자열은 받지 않는다(계약 3).
- **빈도 제한:** 점검당 10초에 1번 넘게 오면 429로 거절하고, ACK하지 않는다.
- **기한 판정과 플랫폼 장애 구분 (계약 6):** 기한이 지나도 다음 경우에는 ALERT가 아니라 EVALUATION_ERROR(플랫폼 사유)로 판정한다.
  - heartbeat consumer group이 기한 시점의 partition offset까지 처리하지 못했을 때(lag 기준). tenant의 heartbeat 메시지로 진행하는 watermark는 쓰지 않는다. 고객 작업이 멈춰 신호가 끊기면 그 watermark도 같이 멈춰서, 정작 ALERT가 영영 나지 않기 때문이다.
  - 기한 구간에 ingress heartbeat 경로의 가용성이 떨어졌다고 platform probe(D04 §10, `cmd/platform-probe`)가 기록했을 때. 우리 ingress가 503을 내 신호가 못 들어온 것을 고객 장애로 바꾸지 않기 위해서다.
  - 두 조건이 모두 정상이고 신호가 없으면 ALERT다.

### 6. 실행 구조

- **scheduler:** alert-worker 안에서 돈다.
  - `synthetic_schedule`(PG, ADR 0051의 `monitor_schedule`과 같은 lease 방식)에서 slot을 만들어 Kafka `synthetic.jobs.v1`에 넣는다.
  - tenant를 가로지르는 schedule 조회는 ADR 0051의 `app.monitor_scan`처럼 schedule SELECT만 여는 전용 RLS 정책으로 한다. 격리 시험을 둔다.
  - PG의 slot 상태가 D02 §18의 "PG 상태"다.
- **job envelope 서명 (계약 1, RB08):**
  - scheduler는 job envelope(test_run_id·tenant·test·revision·slot·location·만료 시각)를 서명한다. 서명 key는 scheduler와 worker만 가진다.
  - runner는 envelope를 바꾸지 않고 결과에 붙여 돌려준다.
  - worker는 서명·만료를 검증하고, tenant·test·revision을 envelope에서만 가져온다. 서명이 없거나 틀린 결과, 만료된 결과, 같은 (test_run_id, attempt)의 두 번째 결과는 버린다.
  - 이렇게 하면 침해된 runner가 다른 tenant의 결과를 쓰거나 지어낼 수 없다. 자기가 받은 job의 결과를 거짓으로 보고하는 위험은 남는다. 두 AZ quorum이 이를 줄이고, RB08 절차로 대응한다.
- **runner:** `cmd/synthetic-runner`는 jobs topic을 읽고 결과를 `synthetic.results.v1`에 쓴다.
  - runner는 PG·ClickHouse credential이 없다. Kafka 두 topic의 서비스별 ACL(D04 §01)과 egress proxy 경로만 가진다.
  - G1 점검에는 고객 코드나 secret이 없으므로 tenant 공용 runner에서 실행한다. D03 §11의 고객별 격리 container는 secret·script를 다루는 F20b·F21에서 적용한다. 이 차이는 D03 개정 대상이다.
  - `test_run_id`는 slot·revision·location(AZ)으로 정하고, 결과 키는 여기에 attempt를 더한다(D02 §18).
- **저장:** worker가 결과와 heartbeat를 ClickHouse `synthetic_runs`·`heartbeat_events`에 쓴다.
  - tenant row policy·query 계정 GRANT·`expires_at`을 migration에 함께 넣는다(ADR 0018).
  - 보존 기간은 30일로 가정한다.
  - Kafka topic 3개는 `make migrate-kafka` 정의에 추가한다.
- **점검 정의:** PG `synthetic_tests`에 둔다.
  - ENABLE+FORCE RLS, `montracer_rw` 최소 권한, tombstone, revision을 적용한다.
  - API는 D02 §20·D03 §11의 `/api/v1/synthetic-tests`에 `type` 필드를 둔다(`http`·`tcp`·`tls`·`dns`·`heartbeat`). 권한은 `synthetics.write`다.
  - G1은 `/synthetic-tests/{id}/runs`(즉시 실행)를 열지 않는다. 결과 조회만 연다.
  - Idempotency-Key·If-Match는 ADR 0049·0057과 같게 쓴다.
  - 같은 PR에서 `api/openapi/montracer.yaml`에도 반영한다(ADR 0059).

### 7. 경보

- **monitor 종류 추가:** MonitorSpec(ADR 0049)에 `kind: synthetic`을 추가하고 `test_id`를 참조한다.
- **조건:**
  - 판정된 slot의 연속 실패 N회(기본 2).
  - `tls`는 남은 일수 임계값.
  - `heartbeat`는 기한 초과 또는 실패 신호.
- **판정 못 한 slot의 처리:**
  - `runner_missing`과 quorum 미달은 EVALUATION_ERROR(플랫폼 사유)다. monitor의 no_data 정책을 적용하지 않는다. runner fleet 중단이 고객 사건이 되지 않게 하려는 것이다(D03 §11, RB08).
  - 일부 실패만 있는 window는 위반도 복구도 아니다. 이전 상태를 유지하고 "일부 실패"를 표시한다. 연속 실패 횟수 계산에서도 건너뛴다.
- **기존 흐름 재사용:** 상태 머신(ADR 0050), 사건·재알림·webhook(ADR 0053·0055)은 그대로 쓴다.

### 8. 화면·과금·rollout

- **화면:** S12 Synthetics 중 가동 점검 부분을 G1에 노출한다. 목록, 가동률과 판정 못 한 slot 수, 응답 시간, 인증서 남은 일수, heartbeat 마지막 신호를 보여 준다.
  - runner 오류·quorum 미달은 회색 운영 문제, 실패는 test 실패로 구분한다(D05 §08).
- **trace 연결:** runner는 `http` 요청에 W3C `traceparent`를 넣는다. 대상 서비스가 Montracer로 계측되어 있으면 실패한 점검에서 그 trace로 이동할 수 있다. 계측되지 않은 외부 endpoint는 연결되지 않는다.
- **과금:**
  - 능동 점검은 slot당 1 run으로 센다. 두 AZ 실행과 재시도는 플랫폼 쪽 판정 장치이므로 세지 않는다(D04 §13 "내부 retry 제외").
  - `runner_missing`과 quorum 미달 slot은 세지 않는다(플랫폼 실패).
  - heartbeat는 run이 없으므로 "활성 heartbeat 점검 수"를 과금 후보 단위로 추가한다(D04 §13 개정 필요).
  - 이중 청구가 0인지 시험한다(T08).
- **flag:** `MONTRACER_SYNTHETIC_ENABLED`(기본 꺼짐)가 꺼지면 다음과 같이 동작한다.
  - API는 404, ingress heartbeat route도 404다.
  - scheduler가 멈춘다.
  - 기존 `kind: synthetic` monitor는 평가가 멈춘 상태(stale)로 보인다. OK로 바뀌거나 열린 사건이 닫히지 않는다(ADR 0050).
  - S12는 "가동 점검이 꺼져 있음"을 보인다.
- **kill switch:** runner에 전체 kill switch를 둔다.

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| **A (채택)** F20을 a·b로 나눠 가동 점검과 heartbeat만 G1 | 가장 흔한 공백(외부 endpoint, 인증서, cron 작업)을 G1에서 메움. 기존 monitor·알림 구현을 재사용 | G1 범위가 늘어남. runner 운영과 T08 Gate가 G1으로 당겨짐 |
| B 그대로 G2 | G1 범위·일정 보호 | 9~15개월 동안 기본 가동 감시 공백. 고객이 외부 도구를 병행해 경보가 나뉨 |
| C F20 전체를 G1 | 한 번에 끝남 | 사설 location(고객망 접근·credential 격리)과 multi-step의 보안·운영 부담이 G1 P0를 위협 |
| D 외부 도구(Blackbox exporter·Uptime Kuma) 결과를 metric으로 수집 | 구현이 가장 적음 | runner_missing·일부 실패를 구분하지 못함. 소유권 확인·SSRF 책임이 고객에게 감. 화면·과금이 따로 놂. 임시 안내로만 쓸 수 있음 |
| E heartbeat를 control-api로 받기 | 구현이 단순 | 공개 기계 트래픽이 제어 API로 들어옴. 수집 ACK 경로(계약 2)와 quota·rate limit을 다시 만들어야 함 |
| F ICMP ping 포함 | Uptime Kuma와 같은 종류 | runner에 NET_RAW 권한이 필요해 격리 약화. TCP로 대부분 대신할 수 있음 |
| G 소유권 확인 없이 rate limit만 | 시작이 쉬움 (Uptime Kuma는 자체 호스팅이라 필요 없음) | 다중 tenant SaaS runner가 제3자 공격 도구가 될 수 있음. D03 §11 위반 |
| H G1부터 고객별 격리 container | D03 §11 그대로 | secret·script 없는 점검에는 격리 이득이 작고, tenant마다 runner를 띄우는 원가가 큼. job 서명·egress proxy·주소 검사로 tenant 경계를 지킴 |

**외부 사례**
- Grafana Synthetic Monitoring은 HTTP·DNS·TCP·Ping·MultiHTTP를 10~3600초 주기로, scripted·browser는 60초 이상으로 돌린다.
- Datadog API test는 HTTP·SSL·DNS·WebSocket·TCP·UDP·ICMP·gRPC로 나뉜다.
- Healthchecks.io는 heartbeat를 period(기대 간격)와 grace time(알림 전 여유)으로 정의한다.
- Uptime Kuma는 HTTP keyword·JSON query·Ping·DNS·Push 점검을 제공하고, 최소 20초 주기를 지원한다.

A는 이 가운데 다중 tenant에서 안전하게 운영할 수 있는 부분만 먼저 고른 것이다.

## 결과

- **이점:**
  - 외부 endpoint, 인증서 만료, 배치 작업 생존을 G1에서 감시한다.
  - 경보가 Montracer 한 곳에 모인다. 계측된 대상이면 실패한 점검에서 trace로 이어진다.
  - monitor·알림·OpenAPI 계약 시험을 재사용한다.
- **비용·운영 부담:**
  - G1에 runner fleet(2 AZ)과 egress proxy 운영, kill switch, 소유권 확인·재확인 흐름, job 서명 key 관리, T08 시험이 추가된다.
  - 담당은 Platform·FE다. D06 §01 인력 가정(Core 11~12명)으로는 G1 일정 연장 또는 다른 G1 P1 범위 축소가 필요할 수 있다. P0는 줄이지 않는다.
- **개정해야 할 권위 문서 (docx 원본 개정 후 재생성):**
  - D01 §05~06: F20을 F20a(G1)·F20b(G2)로 나눈다. heartbeat를 F20a에 넣는다.
  - D02:
    - §03: 점검 정의·결과의 소유 저장소.
    - §04: heartbeat 수신 protocol과 ACK.
    - §17: monitor 종류 `synthetic`, 판정 못 한 slot의 처리.
    - §18: synthetic_run 키에 location = AZ, 일부 실패·quorum 미달 결과.
    - §20: MonitorSpec 표면, `/heartbeats/{token}`, G1에서 즉시 실행 API를 열지 않음.
  - D03 §11:
    - G1 범위와 점검 종류를 적는다.
    - timeout 기본값을 30초로 바꾼다(상한 60초).
    - 두 AZ quorum 판정, attempt, heartbeat 수신·판정을 추가한다.
    - G1 공용 runner와 고객별 격리 container의 적용 범위를 나눈다.
    - DNS resolver 제한과 redirect 정책을 적는다.
  - D04:
    - §01: Kafka topic 3개의 서비스별 ACL.
    - §02: heartbeat token 형식, job 서명 key.
    - §13: 활성 heartbeat 점검 수 단위, slot당 1 run.
    - §14: G1 API runner 용량.
    - §15 RB08: G1 runner에는 location token 대신 Kafka ACL과 job 서명 key가 있다. revoke 절차를 이에 맞춘다.
  - D05:
    - §01·§08: Experience 메뉴의 Synthetics 노출을 G1 entitlement로 바꾼다.
    - §03: 일부 실패·quorum 미달·`runner_missing` 상태 계약.
    - §04: S12의 G1 범위.
  - D06:
    - §01: G1 행에 F20a를 넣는다.
    - §02: E09 선행·증거에 T08을 G1 Gate로 올린다.
    - §05 T08: 이중 청구 시험을 G1에 적용한다.
    - §06: G1 출시 Gate에 T08을 추가한다.
- **영향 받는 계약·시험:**
  - OpenAPI 원천에 `/api/v1/synthetic-tests`와 `/api/v1/heartbeats/{token}`을 추가한다(ADR 0059 contract test).
  - `tests/isolation`:
    - synthetic_tests, synthetic_runs, heartbeat_events
    - heartbeat token의 tenant 위조
    - runner 결과의 tenant 위조(서명 없음·다른 tenant envelope)
    - `synthetic_schedule` scan 정책
  - heartbeat ACK crash 시험(append 전 끊김은 2xx 없음).
  - heartbeat 판정:
    - 신호 없음 + 파이프라인 정상 → ALERT
    - consumer lag 또는 ingress 장애 구간 → ALERT 아님(EVALUATION_ERROR)
  - token이 access log·trace에 남지 않는 시험.
  - T08:
    - SSRF·DNS rebinding·redirect hop
    - 확인되지 않은 도메인으로 redirect
    - 내부 suffix DNS 조회
    - 중복 slot·runner kill
    - 이중 청구 0
  - 소유권 확인 우회·재확인 실패 시험.
  - flag off 동안 기존 monitor가 OK로 바뀌지 않는 시험.
- **문서:** 승인되면 같은 PR에서 갱신한다.
  - `docs/plan/requirements-registry.md`(F20a 행), `docs/plan/work-plan.md`
  - `docs/architecture/README.md`, `cmd/synthetic-runner/README.md`(단계 G1)
  - runbook(runner 중단·egress 차단·job 서명 key 교체)

## Rollback

- **기능 끄기:** `MONTRACER_SYNTHETIC_ENABLED`를 끄면 API·scheduler·heartbeat route가 멈춘다. runner kill switch는 실행 중인 job도 버린다.
- **남는 것:** 저장된 결과는 보존 기간까지 남는다. migration은 expand-only라 되돌리지 않는다.
- **G2로 되돌리기:** 범위 결정을 G2로 되돌리면 F20a를 F20b에 다시 합치고 D01·D06 개정을 되돌린다. 이미 고객에게 열었다면 entitlement 종료를 공지한 뒤 해야 한다.
- **되돌릴 수 없는 것:** 발급된 heartbeat token. 폐기만 할 수 있고, 고객 crontab에 남는다.

## 재검토 조건

- runner 원가가 신호당 원가 예산의 120%를 넘을 때(D06 §09 위험 등록부의 원가 한도). 이때 주기 하한·tenant 한도를 다시 정한다.
- 고객이 1분보다 짧은 주기(Uptime Kuma 최소 20초, Grafana 최소 10초)를 반복해서 요구할 때. 원가 측정 후 entitlement로 연다.
- pilot에서 일부 실패의 원인이 runner 경로가 아니라 대상 쪽인 경우가 많을 때. 일부 실패를 판정에서 빼는 규칙을 다시 정한다.
- 한 지역 판정의 거짓 경보가 pilot에서 의미 있게 나올 때. 다지역 location을 앞당긴다.
- 소유권 확인이 도입을 크게 막는다는 pilot 근거가 나올 때. 확인 방법을 늘리고, 확인 원칙은 유지한다.
- G1 일정이 이 범위 때문에 P0 위험을 만들 때. 후보 B로 되돌린다.

## 증거

- 아직 없음(제안). 승인 후 다음을 추가한다:
  - runner 원가 실측(`docs/experiments/`)
  - T08 시험 결과
  - heartbeat ACK crash 시험
  - 격리 시험 링크
- 외부 사례:
  - Grafana Synthetic Monitoring check types — https://grafana.com/docs/grafana-cloud/testing/synthetic-monitoring/create-checks/checks/
  - Datadog API tests — https://docs.datadoghq.com/synthetics/api_tests/
  - Healthchecks.io period·grace — https://healthchecks.io/docs/configuring_checks/
  - Uptime Kuma 기능 목록 — https://github.com/louislam/uptime-kuma

## 변경 이력

- 2026-10-27 승인.
  - 같은 PR에서 갱신: 기능 상태 등록부(F20 → F20a G1·F20b G2), 작업계획서(G1 표에 가동 점검 행, G2 표의 F20b), `cmd/synthetic-runner/README.md`·`cmd/README.md`(단계), 아키텍처 문서(아직 없는 서비스), 디자인 적용 계획(D4 G1).
  - **남은 일 — 권위 문서 개정:** 위 "개정해야 할 권위 문서"(D01·D02·D03·D04·D05·D06)는 docx 원본을 고친 뒤 `scripts/docs/convert_specs.py`로 다시 만들어야 한다(CLAUDE.md: Markdown 파생본은 직접 고치지 않는다). 원본 개정 전까지 이 ADR이 F20a의 범위·단계 근거다. 구현 PR은 개정된 명세 절을 인용한다.
  - runbook(runner 중단·egress 차단·job 서명 key 교체)은 runner를 만드는 PR에서 쓴다(경보와 함께, 문서화 규칙).

