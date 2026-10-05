# ADR 0031: 플랫폼 synthetic probe

- 상태: 승인 (결정 위임)
- Owner: SRE lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021~0030과 같은 위임). 근거는 §5
- 날짜: 2026-10-05 (제안·결정)
- 관련: D04 §10, §11 · D01 §08 · ADR 0018, 0019, 0023, 0026

## 배경

D04 §10은 다음을 요구한다.

- 1분마다 알려진 trace_id의 3-span trace와 연결 log·metric exemplar를 보낸다.
- 60초 안에 조회·연결·정책 적용을 검증한다.
- "synthetic 평가 실패 2회"를 경보 조건으로 둔다.
- 매 5분 synthetic monitor로 실제 알림 callback까지 확인한다.

RB01 복구 확인도 "3회 연속 synthetic 성공"을 기준으로 쓴다. 지금까지는 수동으로 trace를 보내 확인했다.

구성요소별 지표(ADR 0023)는 각자 정상인데 경로가 끊긴 장애를 놓친다. 예를 들어 worker는 commit하지만 query 계정 권한이 바뀌어 조회되지 않는 경우다.

## 결정

### 1. 별도 binary `cmd/platform-probe` (`internal/probe`)

- 고객과 **같은 공개 경로**를 지난다: OTLP/HTTP ingress → Kafka → worker → ClickHouse → query-api.
- 전용 **probe tenant**의 ingest key(environment `synthetic`)와 API key(`telemetry.read`)를 쓴다. 고객 tenant에는 쓰지 않는다.
- `cmd/synthetic-runner`(F20·F21, 고객 Synthetic 테스트)와 분리한다. 그쪽은 고객 대상 실행·SSRF 차단·과금이 핵심이고, 이 probe는 플랫폼 자체 관측이다. 실패 영역과 권한이 다르다.

### 2. 한 주기 (기본 1분)

| check | 내용 | 실패 조건 |
|---|---|---|
| `ingest_traces` | 새 trace_id로 3-span trace(root + 자식 2)를 보낸다 | 200이 아님, 또는 partial success로 일부 거절 |
| `ingest_logs` | 같은 trace의 log를 보낸다 | 위와 같음 |
| `ingest_metrics` | 그 trace를 exemplar로 단 gauge를 보낸다 | 위와 같음 |
| `trace` | `GET /api/v1/traces/{id}`를 2초 간격으로 조회 | 60초 안에 `span_count = 3`·`complete`·`reasons` 없음·`meta.partial = false`가 아님. 진행 중 요청도 60초에서 끊는다 |
| `redaction` | root span 속성에 이메일 표본(`example.com`)을 넣는다 | 응답에 원문이 있음, 또는 가림 표식 `[REDACTED:email]`이 없음(속성째 사라진 것도 정책 적용이 아니다, ADR 0019) |
| `isolation` (production 필수) | 다른 probe tenant의 API key로 같은 trace 조회 | **200**으로 보임 (ADR 0018 row policy). 404면 성공, 401·403·429·5xx·네트워크 오류는 판정 불가라 blocked |

- 주기마다 trace_id를 새로 만든다. 같은 ID를 재사용하면 이전 주기 데이터가 보여 경로 단절을 가린다.
- **signal별로 보낸다.** metric 거절(cardinality·quota)이 trace 검사를 막지 않는다. trace 수집만 성공하면 조회 검사를 계속한다.
- **평가하지 못한 검사는 `blocked`다(리뷰에서 발견).**
  - 앞 검사 때문에 평가하지 못했다는 뜻이다. trace 수집 실패 시 trace·redaction·isolation이, 조회 실패 시 redaction·isolation이 여기에 해당한다. 다른 tenant 조회가 200도 404도 아닌 경우의 isolation도 blocked다(RB03 리뷰에서 발견: key 만료가 SEV1 page로 번지지 않게).
  - blocked는 성공이 아니므로 성공으로 세지 않는다(계약 6). 실패로도 세지 않는다. 처음에는 실패로 셌는데, 그러면 단순 경로 단절이 2분 뒤 redaction·isolation page로 번진다. 그 결과 가용성 장애가 PII·격리 사고로 잘못 분류되고, 엉뚱한 rollback이 일어난다.
  - 연속 성공은 끊는다.
- 실패 사유는 고정 문구다. 응답 본문·key는 로그에 남기지 않는다.

### 3. 지표와 경보

- 지표(label은 고정 enum `check`, `outcome`)
  - `montracer_probe_checks_total{check,outcome=ok|fail|blocked|skipped}`
  - `montracer_probe_consecutive_failures{check}` — 성공하면 0, blocked는 세지 않음
  - `montracer_probe_consecutive_successes{check}` — 실패·blocked면 0. RB01 복구 확인 "3회 연속 성공"(D04 §11)에 쓴다
  - `montracer_probe_trace_visible_seconds` — 전송부터 조회 성공까지
  - `montracer_probe_last_run_timestamp_seconds`, `montracer_probe_last_success_timestamp_seconds{check}`
  - check series는 기동 때 미리 만든다(rollup과 같은 이유: series 부재로 침묵하지 않게).
  - 마지막 실행 시각은 0에서 시작한다. 기동 시각을 넣으면 첫 주기 전에 죽는 crash loop가 "실행 중"으로 보인다(리뷰에서 발견).
- 경보
  - `MontracerSyntheticProbeFailing`: ~~check별 연속 실패 ≥ 2 → page~~ (아래 갱신으로 대체: `ingest_*`·`trace`만). `for` 없이 바로 울린다. 연속 2회 자체가 지속 조건이다.
  - `MontracerSyntheticProbeNotRunning`: 마지막 결과 뒤 3분 초과가 2분 지속(약 5분)되거나 지표가 없음 → ticket (감시 공백).
- ~~대응은 RB01의 해당 절이다.~~ (아래 갱신으로 대체) `redaction`·`isolation` 실패는 PII·격리 사고 후보로 다룬다.
- **갱신(RB03 작성 시):** 보안 check는 별도 경보 `MontracerSyntheticRedactionFailing`·`MontracerSyntheticIsolationFailing`(page, `security: "true"`)으로 분리해 RB03으로 보낸다. 수집 경로 경보와 대응 주체(보안 담당자 포함, SEV1 후보)가 다르기 때문이다. `MontracerSyntheticProbeFailing`은 `ingest_*`·`trace`만 본다.
- **갱신(RB03 작성 시):** `MontracerSyntheticIsolationUnmonitored`(ticket)를 더한다. isolation이 30분 동안 한 번도 성공하지 못하면 울린다. 원인은 key 미구성(skipped), 만료(blocked), 경로 장애다. isolation이 선택 구성이면 SEV1 감시가 조용히 꺼질 수 있기 때문이다. 그래서 production에서는 `MONTRACER_PROBE_OTHER_API_KEY`가 필수다.

### 4. 아직 하지 않는 것

- **metric 조회 검증:** metric 조회 API는 rollup 테이블을 읽는다. rollup watermark(ADR 0026, 2분 지연) 때문에 60초 안에 보일 수 없다. metric은 수집(ACK)까지만 검사한다. raw 조회 경로가 생기면 추가한다.
- **log 연결 조회:** log 조회 API가 아직 없다. log는 같은 trace_id로 보내되 수집까지만 검사한다.
- **5분 synthetic monitor → 알림 callback (D04 §10):** alert-worker가 생기면 추가한다.
- **probe tenant·key 자동 준비:** 지금은 운영자가 control DB에 만들고 secret manager로 주입한다. control-api가 생기면 bootstrap 절차로 옮긴다.
- **다중 지역 probe:** 단일 지역 배포에서는 한 곳에서 돈다. 지역이 늘면 지역별로 띄우고 `check` 외에 지역 label(고정 enum)을 더한다.

### 5. 외부 사례 근거 (2026-10-05 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| 내부 지표와 별도의 외부 경로 검사 | Google SRE book 6장 "Monitoring Distributed Systems" ([sre.google](https://sre.google/sre-book/monitoring-distributed-systems/)) | 사용자가 보는 외부 동작을 시험하는 black-box 감시를 white-box 지표와 함께 쓴다. black-box는 "지금 실제로 안 되는" 증상을 잡는다 | 채택: 공개 경로를 probe tenant로 지난다. 구성요소 지표(ADR 0023)는 원인 분석에 쓴다 |
| 1회 실패로 page하지 않음 | Datadog Synthetic Monitoring ([경보 발생 방식](https://docs.datadoghq.com/synthetics/guide/how-synthetics-monitors-trigger-alerts), [API test](https://docs.datadoghq.com/getting_started/synthetics/api_test/)) | 재시도 횟수와 최소 실패 지속 시간(`min_failure_duration`)을 넘어야 경보한다 | 채택: D04 §10의 "2회 연속 실패". 1분 주기이므로 약 2분 지속과 같다 |
| probe 정지 감지 | 위 Google SRE 원칙(감시 공백도 증상) | — | 채택: 별도 ticket 경보 |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 실행 위치 | 별도 binary | ingress·worker 안 goroutine: 검사 대상과 실패 영역이 같아 함께 죽는다. `synthetic-runner`: 고객 기능(G2)이라 권한·과금 모델이 다르다 |
| trace_id | 주기마다 새로 | 고정 ID(명세 문구 "알려진 trace_id"): 이전 주기 데이터가 보여 단절을 가린다. "알려진" = probe가 보낸 ID를 안다는 뜻으로 해석 |
| 경보 기준 | 연속 실패 gauge ≥ 2 | 실패율 `rate()`: 1분 주기 표본이 적어 창 경계에서 흔들린다 |

## Rollout

1. 운영자가 probe tenant 둘(격리 검사용 포함)과 key를 만든다. ingest key는 environment `synthetic`, API key는 `telemetry.read`다.
2. platform-probe를 배포하고 운영 Prometheus의 scrape 대상에 넣는다.
3. 그 뒤에 `montracer-probe` 경보 group을 적용한다. 순서를 바꾸면 배포 전까지 `NotRunning` ticket이 계속 열린다(의도된 동작: 감시 공백을 드러낸다).

- Helm·compose 배포물은 Helm chart 작업(D06 §11)에서 다른 서비스와 함께 만든다. 지금은 binary와 경보까지다.

## Rollback

- platform-probe 배포를 내린다. 수집·조회 경로에는 영향이 없다(probe tenant 데이터만 생긴다).
- 경보 규칙의 `montracer-probe` group을 지운다.

## 증거

- `internal/probe` (httptest로 ingress·query 흉내)
  - 모든 검사 성공, log·exemplar가 같은 trace에 연결, 주기마다 새 trace_id
  - 60초 안에 안 보임·2 span만 보임·`meta.partial` → trace 실패, redaction·isolation은 blocked
  - 느린 조회는 deadline에서 끊고 성공으로 세지 않음
  - 원문 이메일 반환·속성 삭제 → redaction 실패(사유에 표본 없음)
  - 다른 tenant에 보임(200) → isolation 실패, 다른 key 거절(401) → blocked, other key 없으면 skipped
  - trace 수집 503·partial success → ingest_traces 실패, 의존 검사 blocked, 사유에 응답 문구 없음
  - metric 거절은 ingest_metrics만 실패, trace 검사는 계속
- `internal/opsmetrics`: 연속 실패·연속 성공, blocked는 실패로 세지 않고 연속 성공은 끊음, series 사전 생성, 마지막 실행 시각 0 시작, label 집합, 경보 규칙이 참조하는 지표·runbook 절 존재
- `deploy/prometheus/rules/montracer.rules.test.yml`: 1회 실패 무경보·2회 page·회복 해소, 정지(경계 7m 무경보·8m)·crash loop·지표 부재 ticket
- spec-reviewer 지적 반영: 의존 검사 오분류(blocked 도입), signal별 수집 검사, 조회 deadline, NotRunning 지연 문구·경계, crash loop 감지, 연속 성공 gauge, README 문구, partial 응답, rollout 순서
