# ADR 0021: 수집 worker — 정규화·dedup·ClickHouse sink·offset commit

- 상태: 승인 (결정 위임)
- Owner: Data lead
- 승인자: polynomeer — 미결정 사항 3건(§4 최초 값 유지, §5 service_id, §7 poison record)을 "빅테크 서비스 사례 기준으로 결정"하도록 위임 (2026-10-05). 판단 근거는 §8
- 날짜: 2026-10-05 (제안) / 2026-10-05 (결정)
- 관련: F01, F02, E02, E03 · D02 §05, §08~10, §21~22 · D04 §04 · ADR 0002, 0003, 0018, 0020

## 배경

D02 §05·§22가 정한 worker 계약은 다음과 같다.

- partition 순서대로 batch를 읽는다.
- ClickHouse 동기 insert와 필요한 replica 확인이 끝난 뒤에만 offset을 commit한다.
- crash 전후로 같은 batch가 반복될 수 있으므로 batch token과 record key를 함께 쓴다. insert dedup window만으로 장기 replay의 정확성을 보장하지 않는다.
- 같은 span key에 다른 내용이 오면 최초 승인 값을 유지하고 충돌을 기록한다(§05, §21).
- schema 오류는 PII를 제거한 최소 payload와 오류 코드만 quarantine에 24시간 보존한다.

다음은 정하지 않았다.

- batch token을 만드는 방법
- 최초 승인 값을 저장소에서 유지하는 방법
- service catalog가 없을 때의 service_id
- 보존 기간 기준 시각
- metric 유형별로 해당 없는 컬럼의 값
- quarantine의 저장 위치와 형식
- sink 장애 시의 동작

## 결정

### 1. 처리 단계

```
poll (partition별 offset 순서) → header 검증 → 단일 record OTLP 해석·정규화
→ partition 안에서 (tenant, event_id) dedup → 테이블별 동기 insert (token)
→ 모든 batch 성공 → offset commit → rebalance 허용
```

- **consumer:** franz-go consumer group `montracer-worker-raw-v1`, 3개 원본 topic.
  - 자동 commit은 끈다.
  - 새 group은 가장 오래된 offset부터 읽는다. 최신부터 읽으면 기동 전에 append된 record를 잃기 때문이다.
  - poll부터 commit까지는 rebalance를 막는다(`BlockRebalanceOnPoll`).
- **commit 시점:** poll 한 번의 모든 partition batch가 저장된 뒤 commit한다. 하나라도 실패하면 아무것도 commit하지 않는다.

### 2. header 신뢰 경계 (ADR 0020 §3)

worker는 header만 믿는다. 다음 경우에는 record를 저장하지 않고 quarantine한다.

| 검사 | 사유 |
|---|---|
| 알 수 없는 topic | `unknown_topic` |
| 필수 header가 없거나 같은 key가 두 번 있음 | `missing_header` |
| tenant·시각·정책 버전 형식 오류, event_id 256자 초과 | `invalid_header` |
| `mt-schema-version` ≠ 1 | `unknown_schema_version` (D06 §07) |
| `mt-tenant-id` ≠ Kafka key 앞 16B | `tenant_key_mismatch` |
| topic과 `mt-signal` 불일치 | `signal_mismatch` |
| protobuf 해석 실패 | `decode_failed` |
| value에 record가 1개가 아님 | `not_single_record` |
| event_id가 payload와 맞지 않음 (trace·span ID, log uid, metric 시각) | `event_id_mismatch` |

- tenant가 어긋난 record는 어느 쪽 tenant도 믿지 않는다. 그래서 quarantine 행의 tenant는 zero UUID다.

### 3. dedup: batch token + record key (D02 §05)

- **record key = (tenant, event_id):** partition 안에서 같은 key는 한 행만 남긴다.
  - poll 단위가 아니라 partition 단위로 dedup한다. 그래야 batch 내용이 (topic, partition, offset 범위)만으로 정해진다.
  - 저장소에서는 각 테이블의 정렬 키가 tenant를 포함하므로, 다른 tenant의 행은 merge되지 않는다.
- **batch token = `<topic>/<partition>/<first>-<last>/<table>/<행 내용 해시>`:** 이 값을 ClickHouse `insert_deduplication_token`으로 보낸다.
  - crash 뒤 같은 범위를 다시 읽으면 같은 내용이라 같은 token이 나오고, insert가 무시된다.
  - **내용 해시가 필요한 이유 (2026-10-05 개정):** topic이 다시 만들어지거나 cluster가 바뀌면(rollback, DR, 로컬 Kafka 재기동 실패 복구) offset이 0부터 다시 시작한다. 그러면 새 record가 dedup window 안의 과거 batch와 같은 token을 받는다. ClickHouse는 이를 **중복으로 보고 조용히 버린다(유실).** 로컬 통합 테스트에서 실제로 metric·log가 commit 뒤 사라지는 것을 관측해 발견했다.
  - 해시 입력은 행마다 tenant, offset, event_id, payload SHA-256, version이다. 같은 offset이라도 다른 데이터면 token이 다르다.
  - 단일 node 테이블은 `non_replicated_deduplication_window = 1000`으로 이 기능을 켠다(migration 00002). Replicated 테이블은 기본 window를 쓴다.
- **범위가 달라진 replay:** token이 달라 행이 다시 들어간다. 이때는 ReplacingMergeTree merge와 query의 key 기준 dedup(`LIMIT 1 BY`, ADR 0018 §5)이 흡수한다. 즉 token은 비용 절감 수단이고, 정확성은 record key가 보장한다.

### 4. 최초 승인 값 유지 (first-write-wins)

**결정:** 같은 identity에 다른 값이 오면 **먼저 수신한 값을 남기고, 나중 값은 사유와 함께 센다.** Prometheus·Grafana Mimir 방식이다(§8 근거 1).

- **version = UInt64 최댓값 − (ingress 수신 ms << 20 | Kafka offset 하위 20비트)**
  - ReplacingMergeTree와 query는 version이 가장 큰 행을 고른다. 먼저 수신할수록 version이 커서 처리 순서와 관계없이 최초 수신 값이 남는다.
  - **같은 ms 수신은 Kafka offset으로 판정한다.** 먼저 append된 record가 이긴다. Kafka log 순서를 순서의 원천으로 삼는다.
  - offset이 같은 ms 안에서 2^20 경계를 넘는 경우에만 순서가 뒤집힌다. 1ms 안에 같은 key가 그 경계를 사이에 두고 두 번 오는 경우라 무시할 수 있다.
- **span:** 같은 key인데 payload hash가 다르면 최초 수신 값을 남기고 충돌 건수를 센다.
- **metric:** 같은 (tenant, stream, start, end)인데 point hash가 다르면 최초 수신 값만 저장한다. 나머지는 `conflicting_point_value`로 quarantine한다(D02 §05 "상충 값 격리·경고").
  - Mimir가 같은 시각의 다른 값을 버리고 `cortex_discarded_samples_total{reason}`으로 세는 것과 같다.
  - redaction이 서로 다른 series를 합친 경우(ADR 0019 §5)도 여기에 걸려 합산되지 않는다.
- **clock skew 수용:** "최초"는 ingress pod의 wall clock 기준이다. NTP 동기 오차는 ms 단위인 반면, OTLP exporter 재전송 간격은 기본 초기값 5초다(§8 근거 2). 그래서 재전송의 선후가 뒤집히지 않는다.
- **남은 한계:** 재전송에서 정렬 키의 event_time이나 service_id가 바뀌면 merge되지 않는다. D02 §09는 이 경우를 충돌로 격리하라고 한다. 이는 아래 durable state로 해결한다.
- **아직 없는 것:** batch를 넘는 충돌 검출과 `quarantined_conflict` 분리(D02 §21)는 partition owner의 durable key/hash state가 필요하다. tail sampler의 checkpoint/changelog와 같은 기반 위에 만든다.

### 5. 정규화 규칙

| 항목 | 규칙 |
|---|---|
| service_id | **결정:** 자연 키 (tenant, `deployment.environment.name`, `service.namespace`, `service.name`)의 SHA-256 앞 128비트로 만든 UUID v8 (D02 §08). 수집 경로에서 registry를 조회하지 않는다(New Relic entity GUID·Datadog unified service tagging 방식, §8 근거 3). `service.name`이 없으면 OTel 기본값 `unknown_service`. version·instance는 정체성에 넣지 않는다. 이름 변경은 새 ID가 되고, 과거 데이터 연결은 catalog alias가 맡는다 |
| span payload | envelope value(단일 span OTLP protobuf) 그대로, `payload_hash` = SHA-256 |
| span attributes | span 속성을 문자열로 복제(검색용). 타입 있는 원본과 resource는 payload에 있다 |
| log body | `Body().AsString()`. 시각이 없으면 observed 시각 |
| metric 해당 없는 값 | `value`(histogram류), `sum`(보내지 않은 경우)은 **NaN**. 0으로 바꾸지 않는다(계약 6). `count`는 UInt64라 gauge·sum에서 0이며 "해당 없음"이다. 소비자는 `type`으로 구분한다 |
| metric payload | exponential histogram, summary, min·max가 있는 histogram은 단일 point OTLP JSON을 보존한다(D02 §10) |
| stream_id·point_hash | event_id header에서 읽는다. payload의 start·end와 일치는 검사하지만 hash는 다시 계산하지 않는다. ingress가 계산한 값을 믿는 것이다(raw topic의 producer는 Kafka ACL로 ingress만 허용한다는 전제). 틀린 값이 들어와도 영향은 그 tenant 안에 머문다 |
| expires_at | event_time + 보존 기간. 기본은 D04 §04에 따라 trace·log 7일, metric 15일. tenant별 보존은 entitlement를 구현할 때 대체한다 |

### 6. quarantine (D02 §05, D04 §04)

- 테이블은 `ingest_quarantine`이고 24시간 TTL을 둔다. ingest 계정은 INSERT만 할 수 있고 query 계정에는 권한이 없다.
- **payload 원문을 저장하지 않는다.** 크기와 SHA-256, Kafka 위치(topic·partition·offset)만 남긴다.
  - envelope는 이미 redaction을 거쳤지만, 해석에 실패한 record는 redaction이 정상이었는지 확신할 수 없다.
  - 재처리는 같은 24시간 보존 안에 있는 Kafka offset으로 한다.

### 7. 장애 동작

- **sink 실패:** 같은 batch를 같은 token으로 재시도한다. backoff는 200ms에서 시작해 최대 5초이고, 총 60초 예산 안에서만 재시도한다.
  - 예산을 넘으면 commit 없이 프로세스가 종료된다. 재시작하면 마지막 commit 이후부터 다시 읽는다.
  - 오래 멈춰 rebalance를 막지 않도록, rebalance timeout은 재시도 예산 + 30초로 둔다.
- **commit 실패:** 프로세스를 종료한다. 다음 소유자가 같은 범위를 다시 쓰고 token과 record key가 중복을 흡수한다.
- **최소 권한:** worker는 기동할 때 ingest 계정이 원본을 **읽을 수 없는지** 확인한다(권한 오류 497). 읽을 수 있으면 기동을 거부한다.
- **로그:** ClickHouse 예외는 코드만 남긴다. 드라이버의 행 변환 오류는 고정 문구로 바꾼다. 두 경우 모두 문구에 입력 값 일부가 실릴 수 있기 때문이다.
- **poison record (결정):** 실패를 두 종류로 나눈다. Kafka Connect `errors.tolerance` + DLQ, Uber 재처리·DLQ topic과 같은 구분이다(§8 근거 4).
  - **record 단위의 결정적 실패:** 드라이버가 특정 행을 변환하지 못한 경우다. 그 행만 `sink_rejected`로 quarantine(DLQ)에 돌리고 나머지를 즉시 다시 저장한다. partition은 멈추지 않는다.
  - **의존 서비스 실패:** 연결 오류, 서버 오류 등이다. 위의 재시도 예산과 종료·재시작으로 처리한다. 데이터를 건너뛰지 않는다. OTel Collector exporter도 일시 오류는 재시도하고 영구 오류는 버린다.
  - **quarantine 행 자체가 거부되면** 돌릴 곳이 없으므로 partition을 멈춘다. 이것은 코드 결함 신호다.
  - **남은 경로:** 서버가 데이터 때문에 거부하는 결정적 오류는 아직 일시 오류와 구분하지 않는다. 그래서 재시작 반복으로 나타난다. 오류 코드 분류와 batch 이분 탐색은 runbook과 함께 G1 전에 만든다.
- **replica 확인:** `MONTRACER_CH_INSERT_QUORUM`이 0보다 크면 `insert_quorum`을 보낸다. production은 2로 둔다.

### 8. 외부 사례 근거 (2026-10-05 확인)

결정 기준은 같은 문제를 운영 규모에서 다룬 빅테크 관측·데이터 서비스의 공개 문서다. 사례끼리 다를 때는 권위 명세(D02)와 맞는 쪽을 택하고, 다른 쪽은 기각 사유와 함께 남긴다.

| # | 결정 | 사례 | 내용 | 채택 여부 |
|---|---|---|---|---|
| 1 | 최초 값 유지 | Prometheus TSDB ([PromLabs](https://promlabs.com/blog/2022/12/15/understanding-duplicate-samples-and-out-of-order-timestamp-errors-in-prometheus/)) | 같은 series·timestamp에 다른 값이 오면 나중 sample을 거절하고 거절 건수를 metric으로 노출 | 채택: 최초 값 유지 + 사유별 계수 |
| 1 | 〃 | Grafana Mimir / Grafana Cloud ([ingestion errors](https://grafana.com/docs/grafana-cloud/observe-and-act/send-data/metrics/metrics-prometheus/ingestion-errors/), [v2.16 release notes](https://grafana.com/docs/mimir/next/release-notes/v2.16/)) | 같은 timestamp의 중복 sample을 버리고 `cortex_discarded_samples_total{reason="sample_duplicate_timestamp"}`로 계수 | 채택: quarantine 사유 `conflicting_point_value` |
| 1 | 〃 | Datadog Metrics ([metrics](https://docs.datadoghq.com/metrics/), [historical metrics](https://docs.datadoghq.com/metrics/custom_metrics/historical_metrics/)) | 같은 timestamp면 마지막 값이 덮어씀 | 기각: D02 §05·§21이 최초 승인 값 유지를 요구 |
| 1 | 〃 | Grafana Tempo ([architecture](https://grafana.com/docs/tempo/latest/operations/architecture/)) | 같은 span은 trace ID 기준 sharding과 compaction에서 중복 제거. 내용 상충 정책은 정하지 않음 | 참고: key 기준 dedup만 같다 |
| 2 | clock skew 수용 | OpenTelemetry Collector exporterhelper ([README](https://github.com/open-telemetry/opentelemetry-collector/blob/v0.98.0/exporter/exporterhelper/README.md), [resiliency](https://opentelemetry.io/docs/collector/resiliency/)) | `retry_on_failure` 기본 initial_interval 5초, max_interval 30초, max_elapsed_time 300초 | 근거: 재전송 간격(초)이 NTP 오차(ms)보다 커서 수신 순서가 뒤집히지 않음 |
| 3 | service_id | New Relic entity GUID ([guid spec](https://github.com/newrelic/entity-definitions/blob/main/docs/entities/guid_spec.md)) | `account\|domain\|type\|identifier`를 base64로 인코딩한 결정적 ID. registry 발급 없이 같은 입력이면 같은 ID | 채택: 자연 키 결정적 ID |
| 3 | 〃 | Datadog unified service tagging ([docs](https://docs.datadoghq.com/getting_started/tagging/unified_service_tagging/)) | 서비스 정체성 = 예약 태그 `service`·`env`. `version`은 배포마다 바뀌는 차원이며 정체성이 아님 | 채택: version·instance 제외 |
| 4 | poison record | Uber Engineering ([Reliable Reprocessing and DLQ with Kafka](https://www.uber.com/blog/reliable-reprocessing/)) | 실패 message를 재시도 topic과 DLQ로 분리해 실시간 처리를 막지 않음 | 채택: 행 단위 DLQ |
| 4 | 〃 | Kafka Connect sink DLQ ([MongoDB Kafka sink 오류 처리](https://www.mongodb.com/docs/kafka-connector/upcoming/sink-connector/fundamentals/error-handling-strategies/)) | `errors.tolerance=all` + `errors.deadletterqueue.topic.name`. 실패 message를 DLQ로 보내고 topic·offset 등 문맥을 남김 | 채택: 위치·사유·해시만 남김(원문 없음, §6) |
| 4 | 〃 | OpenTelemetry Collector exporterhelper (위와 같음) | 일시 오류만 재시도, 영구 오류는 drop | 채택: 일시 오류와 record 오류 구분 |

- **원문을 남기지 않는 점은 사례와 다르다.** Kafka Connect DLQ와 Uber DLQ는 원문 message를 보관한다. 이 제품은 PII 계약(CLAUDE.md 3, D02 §05)이 우선이라 원문을 남기지 않는다. 재처리는 Kafka 24시간 보존 안에서 offset으로 한다.

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| batch token 범위 | partition 단위 offset 범위 | poll 단위: poll 구성이 실행마다 달라 token이 재현되지 않는다. token 없음: 짧은 crash 재처리마다 raw 행이 두 배가 된다 |
| 최초 값 유지 | 역순 version | 수신 시각 version: 마지막 재전송이 이긴다(D02 §21 위반). 별도 state 조회: 매 record마다 저장소 조회 비용이 든다. state 기반은 충돌 격리 구현 때 추가한다 |
| service_id | 자연 키 결정적 UUID | catalog 조회 후 발급: catalog가 아직 없고, 수집 경로에 동기 의존이 생긴다 |
| quarantine 내용 | 위치·해시만 | 원문 보존: 해석 실패 record의 redaction 상태를 보장할 수 없다 |
| sink 장애 | 예산 후 종료·재시작 | 무한 재시도: rebalance가 막혀 group 전체가 멈춘다. 실패 batch를 건너뛰고 commit: 유실이 생긴다 |
| 상충 값 | 최초 값 유지 + 사유별 계수 (Prometheus·Mimir) | 마지막 값 유지(Datadog metric 방식): D02 §05·§21의 "최초 승인 값 유지"와 충돌한다. 두 값 모두 저장: delta·sum 집계가 이중 계수된다 |
| 같은 ms 판정 | Kafka offset | 무작위(이전안): 같은 입력에서 결과가 실행마다 달라질 수 있다. 수신 ns 정밀도: ingress pod 간 clock 비교라 ms보다 나을 근거가 없다 |
| poison record | 행 단위 DLQ(quarantine) + 일시 오류는 정지 | partition 정지(이전안): 결정적 오류 하나가 그 partition의 모든 tenant 수집을 막는다. 전부 DLQ: 저장소 장애 때 대량 데이터가 quarantine으로 빠지고 원문이 없어 복구가 어렵다 |

## 결과

- 수집 경로가 ClickHouse까지 이어진다: OTLP → ingress → Kafka → worker → 원본 테이블 → `telemetrystore`.
- migration 00002가 추가된다: dedup window와 quarantine 테이블.
- 다음 작업은 G1 Gate에서 추적한다.
  - worker metric: consumer lag, batch 지연, 중복·충돌·quarantine 건수(사유별). 지금은 구조화 로그에만 남는다.
  - runbook: sink 장애로 인한 재시작 반복, poison batch, quarantine 급증
- metric window 집계(rollup·watermark·reset)는 이 worker 뒤에 별도 단계로 만든다(D02 §10, §22).

## Rollback

- worker 배포를 되돌리면 offset이 멈춘 자리에서 lag만 쌓인다(Kafka 24시간 보존).
- migration 00002 down은 quarantine 테이블을 지우고 dedup window 설정을 되돌린다.

## 재검토 조건

- partition 단위 insert가 너무 작은 part를 많이 만들어 merge 부하가 커질 때. 이때는 partition을 묶는 token 체계를 검토한다.
- 충돌 격리 state를 구현할 때. 그때 version 규칙과의 관계를 다시 본다.
- service catalog를 도입할 때.

## 증거

- `internal/pipeline` 단위 테스트
  - **(tenant, event_id) 계약:** 두 tenant가 같은 trace·span ID를 써도 각각 저장되고 service_id도 다르다.
  - 재전송 dedup: 처리 순서와 관계없이 최초 수신 값이 남고 충돌 건수가 기록된다.
  - metric: 같은 stream·시각에 다른 값이 오면 최초 수신 값만 남고 나중 값은 `conflicting_point_value`로 quarantine된다. offset상 나중 수신 값이 먼저 와도 결과가 같다. 다른 tenant는 충돌이 아니다.
  - 같은 ms 수신은 offset이 작은 record가 남는다.
  - poison record: sink가 거부한 행만 `sink_rejected`로 quarantine되고 나머지 행은 저장된다. quarantine 행은 거부 대상이 아니다.
  - batch가 결정적이고 token 형식이 고정돼 있다.
  - header 위조·누락·중복, schema 버전, signal, event_id 불일치, 손상 value, 다중 record value는 quarantine되고 원문이 없다.
  - log uid·gen 형식, 5개 metric 유형의 컬럼 매핑(sum 없는 histogram은 NaN), service_id 자연 키 규칙, 만료 시각 범위 자르기.
  - sink 재시도 성공과 예산 초과 종료.
- 통합 테스트 (Kafka + ClickHouse)
  - **insert 성공 → commit 전 crash → 재시작:** crash 뒤 offset이 그대로이고, 재처리 결과 span 20개(재전송 5개 포함 입력), metric 5개, log 1개가 논리 중복 없이 남는다. 재전송 값이 최초 값을 덮지 않는다.
  - tenant B 분리, 위조 record quarantine, lookup MV가 채워짐.
  - 같은 batch 3회 쓰기 → raw 1행(token dedup). 같은 offset 범위에 다른 내용(topic 재생성 모사) → 버려지지 않고 저장.
  - 관리자 DSN으로 sink 기동 거부.
- CI migration up → down → up (00002 포함).
