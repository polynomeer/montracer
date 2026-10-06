# ADR 0024: 수집 tenant quota — tenant·signal별 token bucket, 429·413·503 구분

- 상태: 승인 (결정 위임)
- Owner: Data lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021~0023과 같은 위임). 근거는 §7
- 날짜: 2026-10-05 (제안·결정)
- 관련: F07, G1 Gate · D01 §08 · D02 §04~05, §12, §19, §21~22 · D04 §01, §08, §11 · D06 §03 Noisy tenant · ADR 0006, 0020 §7, 0023

## 배경

명세가 정한 것은 다음과 같다.

- 처리 순서는 `redaction → quota → envelope → Kafka append`다(D02 §04).
- tenant별 weighted fair queue와 byte·record token bucket을 함께 적용한다(D02 §05).
- 429는 quota다(D02 §19). 일시 과부하는 429·503과 Retry-After다(D02 §04).
- **내부 과부하 429는 가용성 실패로 센다.** 계약 초과가 확실한 요청은 분모에서 빼고, 과부하를 계약 초과로 오분류하지 않도록 quota 결정 로그를 남긴다(D01 §08).
- record별 quota 결과는 partial success로 모은다(D02 §22).

ADR 0020 §7은 quota를 G1 Gate 차단 항목으로 남겼다. 지금은 한 tenant가 cluster 전체 수집 용량을 쓸 수 있다.

정하지 않은 것은 다음과 같다.

- 한도 단위와 기본값
- 여러 ingress replica 사이의 한도 분배
- tenant별 한도의 원천
- 요청 하나가 한도보다 클 때의 응답
- 과부하와 계약 초과의 status 구분

## 결정

### 1. bucket과 판정

- **tenant × signal(traces·logs·metrics)마다 record bucket과 byte bucket 두 개**를 둔다(`internal/quota`).
  - **구현:** 두 bucket을 잠금 하나 아래에서 확인하고 차감하는 token bucket이다. `golang.org/x/time/rate`의 ReserveN/CancelAt은 쓰지 않는다. CancelAt은 뒤따른 예약분을 복원하지 않아, 동시에 거절된 요청마다 token이 사라지기 때문이다(리뷰에서 발견).
  - **시각:** 단조 증가로 다룬다. 동시 요청의 시각이 뒤섞여도 같은 구간을 두 번 적립하지 않는다.
  - **record:** redaction을 통과해 envelope로 만들 record 수다. 검증·environment·redaction에서 거절된 record는 세지 않는다.
  - **byte:** 압축 해제 후 본문 크기다. client가 통제하는 크기이고, 최대값(8MiB, ADR 0017)이 정해져 있다.
- **all-or-nothing:** 두 bucket을 모두 통과해야 하며, 하나라도 부족하면 아무것도 소비하지 않는다.
- **판정 결과**

  | 결과 | 응답 | 근거 |
  |---|---|---|
  | 통과 | Kafka append로 진행 | |
  | rate 초과 | **429 + Retry-After**(부족분 ÷ rate, 1~60초). google.rpc.Status `RESOURCE_EXHAUSTED`. 아무것도 append하지 않음 | client가 재시도하는 backpressure라 유실이 없다 |
  | 요청 하나 > burst | **413** "batch exceeds tenant burst limit; split the batch" | 기다려도 통과할 수 없다. 429로 주면 client가 영원히 재시도한다 |
  | instance 동시 처리 > `MaxInflight`(256) | **503 + Retry-After**. 인증·decode 전에 거절 | 계약 초과가 아니라 과부하다. 가용성 실패로 센다(D01 §08) |

- **위치:** D02 §04 순서 그대로 `redaction → quota → envelope → Kafka append`다. quota를 통과하지 못하면 envelope도 만들지 않는다.
  - 드물게 envelope 단계에서 1MiB 초과로 빠지는 record도 token을 쓴다. 보수적인 쪽(한도를 조금 일찍 씀)이다.
- **결정 로그 (D01 §08):** 거절된 요청의 access log에 다음을 남긴다.
  - `quota_limit`(records·bytes)과 사유별 거절 건수
  - 적용 한도: replica당 rate, burst, replica 수, override 여부
  - 이 값으로 replica 수 설정 오류 같은 **내부 원인의 429를 계약 초과와 사후에 구분**한다.
- **지표:** `montracer_ingress_records_total{outcome="rejected", reason="rate_limited"|"quota_over_burst"}`, status_class 4xx. tenant label은 두지 않는다(ADR 0023). tenant별 거절은 로그와 usage 원장으로 본다.

### 2. 기본 한도 (tenant·signal별, cluster 전체)

| 항목 | 기본값 | 사례 |
|---|---|---|
| record rate / burst | 10,000/s / 200,000 | Grafana Mimir 기본 수준(ingestion rate·burst) |
| byte rate / burst | 15MB/s / 20MB | Grafana Tempo 기본값(rate_limit_bytes 15MB, burst_size_bytes 20MB) |

- byte burst 20MB는 해제 본문 상한 8MiB보다 크다. 그래서 정상 최대 요청이 byte 때문에 413을 받는 일이 없다.
- 환경 변수(`MONTRACER_QUOTA_*`)로 바꿀 수 있다. 실제 계약 값은 entitlement(D04 §10 quotas)를 구현할 때 그 원천에서 가져온다.

### 3. replica 사이 분배 (global 전략)

- rate는 **cluster 전체 값**이다. 각 ingress는 `rate / MONTRACER_INGRESS_REPLICAS`로 나눠 local bucket에 적용한다.
- **burst는 나누지 않는다.** 나누면 replica가 3개 이상일 때 byte burst(20MB/3 ≈ 6.7MB)가 최대 해제 본문(8MiB)보다 작아진다. 그러면 정상 batch가 413(영구 거절, 유실)을 받는다(리뷰에서 발견).
  - 그 대가로 cluster 전체의 순간 burst는 replica 수배까지 커질 수 있다. 지속 rate는 한도를 지킨다.
- **byte burst 하한은 해제 본문 상한(8MiB)이다.** 기본값이 그보다 작으면 기동하지 않는다. overrides가 더 낮게 잡아도 하한을 쓴다. tenant를 줄일 때는 burst가 아니라 rate를 낮춘다.
- **전제:** 부하분산기가 요청을 replica에 고르게 나눈다.
  - **OTLP/gRPC(ADR 0036):** HTTP/2 장기 연결은 한 replica에 고정되기 쉽다. 그래서 LB는 HTTP/2를 아는 L7이어야 한다. ingress는 연결 수명(2분)을 제한해 연결이 주기적으로 다시 나뉘게 한다.
- **한계:** replica 수는 지금 정적 설정이다. autoscaling 때는 값을 함께 바꿔야 한다. 건강한 replica 수를 자동으로 반영하는 것(Mimir는 ring을 쓴다)은 Cell registry(ADR 0006)를 구현할 때 함께 만든다.

### 4. tenant별 한도 (overrides 파일)

- `MONTRACER_QUOTA_OVERRIDES_FILE`(JSON)에 tenant UUID → signal → 네 값을 둔다. 없는 항목은 기본값을 쓴다.
- **10초마다** 내용 해시로 변경을 확인해 다시 읽는다. mtime과 크기는 같은 초 안의 같은 크기 편집을 놓치므로 쓰지 않는다.
  - **잘못된 파일은 적용하지 않고 직전 값을 유지한다.** 실패는 지표와 ticket 경보(`MontracerQuotaOverridesInvalid`)로 알린다.
  - 기동 시 파일이 잘못됐으면 기동하지 않는다. 조용히 기본값으로 돌지 않게 하기 위해서다.
- **엄격한 검증:** 알 수 없는 필드, 소문자가 아닌 UUID, 알 수 없는 signal, 일부 값만 지정한 한도는 거절한다. 일부 값만 쓴 항목이 0이 되어 tenant를 막는 사고를 막기 위해서다.
- 한도가 바뀌면 남은 token은 유지하고 rate·burst만 바꾼다. 10분간 쓰이지 않은 bucket은 지운다.

### 5. 아직 하지 않는 것

- **weighted fair queue (D02 §05):** cluster가 포화됐을 때 tenant 사이의 공정한 분배다. 지금은 tenant별 상한과 instance 동시 처리 상한으로 noisy neighbor를 막는다.
  - Mimir·Tempo·Loki도 수집 경로에서는 tenant별 rate limit과 instance limit을 쓴다.
  - fair queue는 D06 §03 Noisy tenant 부하 시험(한 고객 10배 15분)에서 다른 tenant SLO가 깨질 때 도입한다.
- **신규 series(cardinality) quota (D02 §10):** 신규 조합만 record 단위로 거절하는 partial success 경로다(D02 §22). 활성 series 상태가 필요해 metric 집계 worker와 함께 만든다(E03).
- **usage 원장의 tenant별 거절 집계(D04 §08)**, 그리고 **entitlement를 원천으로 하는 계약 한도.**

### 6. 회계 (D02 §21)

`valid_before_quota = quota_reject + durable_accepted`

- `quota_reject`는 `rejected{reason=rate_limited|quota_over_burst}`의 record 수다.
- `durable_accepted`는 `accepted`의 record 수다. 두 값의 단위는 같다(envelope 수).

### 7. 외부 사례 근거 (2026-10-05 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| §1 tenant별 token bucket, 초과 시 429 | Grafana Mimir distributor ([docs](https://grafana.com/docs/mimir/latest/references/architecture/components/distributor/)) | tenant별 ingestion rate·burst. 초과하면 요청을 버리고 HTTP 429 | 채택 |
| §1·§2 byte 한도 | Grafana Tempo ([Manage trace ingestion](https://grafana.com/docs/tempo/latest/operations/manage-trace-ingestion/)) | tenant별 `rate_limit_bytes` 15MB/s, `burst_size_bytes` 20MB. 초과 시 `RATE_LIMITED` | 채택: byte 기본값 |
| §3 replica 분배 | Grafana Mimir distributor (위) | 각 distributor의 local limiter를 limit / N(건강한 distributor 수)으로 설정 | rate에 채택(정적 N). burst는 최대 요청 보장을 위해 나누지 않음(이 제품의 선택). 자동 N은 보류 |
| §4 overrides reload | Grafana Mimir runtime configuration ([docs](https://grafana.com/docs/mimir/latest/configure/about-runtime-configuration/)) | tenant별 limits overrides 파일, 기본 10초마다 reload | 채택 |
| §1 429와 Retry-After | OTLP 규격 ([OTLP Specification](https://opentelemetry.io/docs/specs/otlp/), [Exporter](https://opentelemetry.io/docs/specs/otel/protocol/exporter/)) | 서버는 throttling을 재시도 가능 응답으로 알리고 Retry-After로 대기를 지시. exporter는 이를 따라 같은 batch를 재전송 | 채택 |

- **사례와 다르게 한 것:** 요청 하나가 burst보다 크면 413을 준다. Mimir·Tempo는 이 경우도 rate limit 응답(429)으로 처리해 client가 재시도를 반복할 수 있다. 이 제품은 통과할 수 없는 요청을 영구 오류로 알린다. 이것은 D01 §08이 SLO 분모에서 빼는 "계약 초과가 확실한 요청"이다.

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 초과 응답 | 요청 전체 429 | record별 partial success 거절: client가 재시도하지 않아 rate 초과분이 영구 유실된다. 순간 spike를 유실로 만들 이유가 없다 |
| byte 단위 | 해제 본문 | envelope 합계(canonical byte): resource 복사로 요청보다 수십 배 커질 수 있어 정상 batch가 413이 된다. 과금용 canonical byte는 usage 원장에서 따로 센다(D04 §09) |
| 분배 | 정적 replica 수로 나눔 | 중앙 rate limiter(Redis 등): 수집 hot path에 동기 의존이 생긴다. local 그대로: replica 수만큼 한도가 커진다 |
| 한도 원천 | overrides 파일 + 기본값 | 제어 DB 조회: entitlement 모델 전이라 schema가 없다. 요청마다 조회하면 수집 경로가 PG에 묶인다 |

## 결과

- G1 Gate 차단 항목 중 "quota"가 1차로 해소된다(rate 상한). fair queue와 신규 series quota는 위 조건에서 이어간다.
- 운영
  - `MONTRACER_INGRESS_REPLICAS`를 배포 replica 수와 맞춘다.
  - tenant 조정 절차는 RB01 "tenant quota 조정"에 있다.
  - 429가 늘어도 가용성 경보(5xx)는 울리지 않는다. 용량 부족은 503과 증설로 다룬다.

## Rollback

- 한도를 크게 올리거나(환경 변수), quota를 끄는 배포로 되돌린다. 저장 상태는 없다.

## 재검토 조건

- D06 §03 Noisy tenant 시험에서 다른 tenant의 SLO가 깨질 때. 이때는 fair queue를 도입한다.
- autoscaling으로 replica 수가 자주 바뀔 때. 이때는 replica 수를 자동으로 반영한다.
- entitlement를 구현할 때. 이때는 한도 원천을 옮긴다.

## 증거

- `internal/quota` 테스트
  - burst 뒤 rate 제한과 정확한 대기 시간
  - all-or-nothing(거절이 token을 쓰지 않음)
  - **동시 거절 50건 뒤에도 token 손실 없음**
  - **시각 역행 시 이중 적립 없음**
  - **rate만 replica로 나눔**
  - **byte burst 하한**
  - burst 초과는 영구
  - tenant·signal 격리(noisy neighbor)
  - replica 분배
  - overrides 적용·변경·제거, idle bucket 제거
  - overrides 파일 검증 6종, 잘못된 편집 뒤 직전 값 유지, 주기 reload, 기동 시 파일 없음 실패
- spec-reviewer 지적 반영
  - x/time/rate CancelAt의 token 손실
  - burst를 나눠 생기는 정상 batch 413
  - quota 위치의 D02 §04 불일치(순서를 명세대로 수정)
  - 시각 역행
  - 결정 로그 부족
  - overrides 변경 감지
- `internal/ingest` 테스트
  - 429와 Retry-After 1초, RESOURCE_EXHAUSTED, append 0건, 결정 로그
  - burst 초과 413, 다른 tenant 통과
  - 동시 처리 초과 시 인증 전 503
- promtool: `MontracerQuotaOverridesInvalid` 시나리오
