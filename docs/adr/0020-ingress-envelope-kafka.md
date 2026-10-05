# ADR 0020: OTLP ingress, Kafka envelope 형식, 신호별 식별자

- 상태: 승인
- Owner: Data lead
- 승인자: polynomeer
- 날짜: 2026-10-05 (제안) / 2026-10-05 (결정)
- 관련: F01, E02 · D02 §04, §05, §07, §09, §22 · D04 §02 · ADR 0001, 0002, 0016, 0017, 0019

## 배경

D02 §04·§05·§22가 정한 것은 다음과 같다.

- ingress 처리 순서와 ACK 경계
- envelope 필드: tenant_id, signal, schema_version, event_id, event_time, ingress_received_at, policy_version, routing_epoch, payload
- partition 기준: trace는 tenant+trace_id, metric은 tenant+stream_id, log는 tenant+source_id
- 신호별 중복 식별자
- Kafka message 1MiB 상한

정하지 않은 것은 Kafka 클라이언트, envelope 직렬화 형식, topic 이름·설정, SDK log의 source_id와 event_id, 지원할 OTLP transport다.

## 결정

### 1. 처리 순서 (D02 §04, §22)

```
인증(ingest key) → 신호 권한(ingest.<signal>) → Content-Type·압축 해제 한도·pre-scan·decode (ADR 0017)
→ record 검증 → tenant 주입(principal) · environment 범위 검사 → redaction (ADR 0019)
→ record별 envelope → Kafka append (acks=all, idempotent) → 모두 확인 후 200
```

- **environment 범위 (D04 §02):** resource의 `deployment.environment.name`이 key의 environment 범위에 없거나 아예 없으면, 그 resource의 record를 `environment_not_allowed`로 거절한다(partial success).
- **ACK 경계 (ADR 0002):** 요청 안의 유효 record가 **모두** append 확인된 뒤에만 200을 준다. 하나라도 실패하면 이미 기록된 record가 있어도 503과 Retry-After를 준다. 재전송으로 생기는 중복은 envelope event_id로 worker가 제거한다.

### 2. 응답 (OTLP/HTTP 규격, ADR 0014 §6)

| 상황 | 응답 |
|---|---|
| 성공·일부 거절 | 200 + `Export*ServiceResponse`. 거절이 있으면 `partial_success`에 건수와 사유 요약을 담는다(값 내용 없음) |
| 인증 실패 | 401 |
| 권한 없음 | 403 |
| 형식 오류 | 400 |
| 크기·복잡도 초과 | 413 |
| 미지원 Content-Type·Encoding | 415 |
| 본문 읽기 실패 (연결 끊김·읽기 timeout) | 503 + Retry-After |
| 인증 저장소·Kafka 장애 | 503 + Retry-After |

- 오류 본문은 `google.rpc.Status{code, message}`이고 요청과 같은 encoding(protobuf·JSON)으로 쓴다. 메시지는 고정 문구다.
- **크기 초과:** `Content-Length`가 한도를 넘으면 본문을 읽기 전에 413을 준다. chunked 요청은 `http.MaxBytesReader`가 한도를 넘기는 순간 413이다.
- **timeout 정합:** 본문 읽기(20초) + Kafka append 대기(10초) + 처리 여유 < 응답 쓰기(45초). append는 끝났는데 응답을 쓰지 못해 불필요한 재전송이 생기는 것을 막는다.
- **TLS:** 외부 TLS 443(D02 §04)은 앞단 load balancer나 ingress controller가 종료한다. 서비스 간 구간은 mTLS나 동등한 workload identity를 쓴다(D04 §02). 프로세스 자체는 평문 HTTP로 listen한다.
- **readiness:** broker 연결과 수집 topic 3개의 존재를 함께 확인한다. topic이 없으면 ready가 아니다.
- transport는 우선 **OTLP/HTTP**(`/v1/traces|metrics|logs`)만 제공한다. OTLP/gRPC는 후속으로 같은 처리 단계를 재사용해 추가한다.

### 3. Envelope 형식

| 부분 | 내용 |
|---|---|
| value | record **하나**만 담은 OTLP `Export*ServiceRequest` protobuf. resource와 scope를 포함하므로 envelope 하나만으로 해석할 수 있다 |
| key | tenant_id(16B) + 파티션 식별자(16B) |
| headers | `mt-tenant-id`, `mt-signal`, `mt-schema-version`(=1), `mt-event-id`, `mt-event-time-ns`, `mt-ingress-received-at-ms`, `mt-policy-version`(redaction 정책), `mt-routing-epoch` |

- **tenant의 원천은 `mt-tenant-id` header 하나뿐이다.** payload가 흉내 낸 예약 속성(`tenant_id`, `tenant.id`, `mt.*`, `montracer.*`)은 Kafka에 쓰기 전에 resource에서 지운다. worker는 header만 신뢰한다.
- 별도 envelope `.proto`를 만들지 않는다. 메타데이터는 header에 두고 payload는 표준 OTLP를 그대로 쓴다. 소비자는 schema_version header로 호환성을 판단한다(unknown major는 quarantine, D06 §07).
- 직렬화한 envelope가 1MiB를 넘으면 그 record를 `record_too_large`로 거절한다. 이것은 ADR 0017의 근사 검사를 실제 크기로 한 번 더 확인하는 단계다.
- `routing_epoch`는 Cell registry를 구현하기 전까지 설정값(기본 1)을 쓴다.

### 4. 신호별 식별자 (D02 §05, §07)

| 신호 | event_id | partition 식별자 |
|---|---|---|
| trace | hex(trace_id) + hex(span_id) | trace_id |
| log | `uid:<log.record.uid>`가 있으면 그 값, 없으면 `gen:<수신마다 새로 만든 128-bit>` | resource fingerprint (= SDK log의 source) |
| metric | `<stream_id>-<start_ns>-<end_ns>-<point_hash>` | stream_id |

- **fingerprint:** canonical(정렬·타입 포함) 직렬화의 SHA-256 앞 128비트다. **tenant를 포함한다**(D02 §07).
- **stream_id:** tenant, resource 속성과 schema URL, scope 이름·버전·속성과 schema URL, metric 이름·타입·단위·temporality·monotonic, point 속성으로 만든다.
- **dedup key = (`mt-tenant-id`, event_id):** event_id만으로는 dedup하지 않는다. trace·span ID와 `log.record.uid`는 client가 정하는 값이라, tenant를 함께 쓰지 않으면 다른 tenant의 record가 서로를 지울 수 있다. worker 계약 테스트로 고정한다.
- **redaction 후 series 병합:** redaction이 point 속성을 같은 placeholder로 바꾸면 서로 다른 원본 series가 같은 stream_id가 된다(ADR 0019 §5). 정규화 worker의 상충 값 격리(D02 §05·§21)에서 다루고, 병합 건수를 metric으로 남긴다.
- **point_hash:** 직렬화한 바이트가 아니라 값 필드(number·histogram·exponential histogram·summary)의 canonical 해시다. 속성 순서가 달라도 같은 point는 같은 ID가 된다.
- **trace·metric 재전송:** 같은 event_id와 key가 나와 worker가 중복을 제거할 수 있다. 통합 테스트로 확인했다.
- **log.record.uid가 없는 SDK log:** 수신할 때마다 ID가 새로 생기므로 client 재전송 중복을 제거할 수 없다(D02 §09에 명시된 한계). agent 수집 log의 `source_id + file generation + offset` 식별자는 node agent를 구현할 때 추가한다.

### 5. Kafka 클라이언트와 topic

- **클라이언트:** `github.com/twmb/franz-go` v1.22.1(순수 Go)과 `kadm` v1.19.0.
- **producer 설정:** `acks=all`(AllISRAcks), idempotence(기본), batch 상한 1MiB+64KiB, zstd/lz4 압축, topic 자동 생성 끔.
- **topic:** `telemetry.{traces,logs,metrics}.raw.v1`. 고객별 topic은 만들지 않는다(D02 §05).
- **topic 설정:** `cmd/migrate kafka up`이 멱등하게 만든다. **이미 있는 topic도 실제 설정을 읽어 검증하고, 하나라도 다르면 실패한다**(RF, min.insync.replicas, retention, max.message.bytes). 미리 RF 1로 만들어진 topic이 조용히 통과하면 acks=all의 내구성 전제가 깨지기 때문이다.
- RF 3 미만은 `MONTRACER_KAFKA_ALLOW_LOW_REPLICATION=1`(로컬·CI 단일 broker)일 때만 허용한다. 로컬은 `make migrate-kafka`로 분리했다.

  | 설정 | 값 |
  |---|---|
  | retention | 24시간 |
  | `min.insync.replicas` | min(2, RF) |
  | `max.message.bytes` | 1MiB+64KiB |
  | RF | production 3, 로컬 단일 broker 1 |
  | partition 수 | 기본 6 (부하 시험으로 정함) |

### 6. 로컬 안전장치

- Kafka를 쓰는 make target(`migrate-kafka`, `test-integration`)은 먼저 확인한다. `KAFKA_PORT`가 montracer compose project의 Kafka 서비스가 실제로 연 host 포트와 다르면 중단한다. 이 검사는 compose label로 컨테이너를 찾는다.
- 이유: 같은 머신의 다른 프로젝트 Kafka에 topic을 만들거나 데이터를 쓰는 사고가 실제로 날 뻔했다. 낡은 `.env`가 19092를 가리키고 있었고, 그 포트는 다른 프로젝트 Kafka가 쓰고 있었다.

### 7. 아직 없는 것

다음 항목은 별도 작업으로 남긴다. 이 중 **metric, runbook(RB01: Kafka 장애 시 ingress 503, topic 설정 불일치), quota**는 G1 출시 Gate(Q3·Q5)를 막는 항목으로 추적한다.

- 수집 metric: 승인·사유별 거절 건수, produce 지연·실패, 503 비율. 지금은 구조화 access log에만 건수가 남는다.
- decode·redaction·envelope 단계의 CPU 시간 상한. 지금은 요청 크기·복잡도 한도(ADR 0017)로만 묶는다.

- tenant별 quota·weighted fair queue (D02 §05, D04 §08)
- OTLP/gRPC
- 사유별 수집 metric
- Cell routing registry
- agent log source_id
- 인증 결과 cache: key 폐기 전파 60초(D04 §02)를 지키면서 PG 조회를 줄이는 cache

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| Kafka 클라이언트 | franz-go | confluent-kafka-go: cgo·librdkafka 의존. sarama: 유지 상태와 API가 franz-go보다 불리함 |
| envelope 형식 | header + 단일 record OTLP | envelope `.proto`: codegen 도구 체인이 필요하고 OTLP를 다시 감싸게 된다. JSON: 크기·비용이 크다 |
| record 단위 | span·log record·point | 요청 batch 그대로: partition 기준(trace_id·stream_id)을 만족할 수 없다 |

## 결과

- 이제 수집 경로가 Kafka까지 이어진다. 다음은 worker(정규화·dedup·ClickHouse sink)다(Sprint 2~3).
- record 단위로 나누는 비용(resource·scope 복사, record당 직렬화)이 든다. 부하 시험에서 측정한다.
- 로컬 Collector는 아직 debug exporter다. ingress로 연결하는 것은 `make dev` 구현 때 한다.

## Rollback

ingress 배포를 되돌린다. topic·schema는 v1을 유지한다. 형식을 바꾸려면 v2 topic과 dual-read로 전환한다.

## 재검토 조건

- record 단위 분할 비용이 수집 처리량 목표(D01 §02)를 막을 때.
- OTLP/gRPC 사용 비율이 높을 때.
- Kafka 대체 제품을 검토할 때 (ADR 0002 재검토 조건).

## 증거

- `internal/ingest` 단위 테스트
  - tenant는 key에서만 온다(payload 속성·`X-Tenant-ID` 무시)
  - Kafka 쓰기 전에 redaction이 적용된다
  - environment 범위에 따른 partial success
  - 401·403·415·400·413, 인증 저장소 장애 503
  - **produce 실패·일부만 append된 뒤 실패·append 대기 timeout일 때 200을 주지 않음**(503 + Retry-After, 내부 오류 비노출)
  - 8MiB를 넘는 일반 본문 413(Content-Length 경로와 chunked 경로 모두), 본문 읽기 실패 503
  - 오류 본문 encoding이 요청을 따름, `/v1/metrics` 경로, record_too_large, 예약 속성 제거
  - 로그에 payload·key 없음
- `internal/telemetry/envelope` 테스트: 재전송 시 같은 ID, tenant 포함 key, 속성 순서와 무관한 metric ID, 5개 metric 유형 왕복, 1MiB 초과 거절
- `migrate kafka up` 실측: RF 3 미만 거부, 설정 불일치 topic 검출
- 통합 테스트 (PostgreSQL + Kafka)
  - 발급한 key로 수집한 뒤 Kafka에서 다시 읽기: key 앞 16B가 인증된 tenant, 같은 trace는 같은 partition, 재전송 시 같은 event_id
  - 폐기된 key는 401
  - broker에 닿지 않으면 503
