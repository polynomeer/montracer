# ADR 0021: 수집 worker — 정규화·dedup·ClickHouse sink·offset commit

- 상태: 제안
- Owner: Data lead
- 승인자: (미정)
- 날짜: 2026-10-05 (제안)
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
- **batch token = `<topic>/<partition>/<first>-<last>/<table>`:** 이 값을 ClickHouse `insert_deduplication_token`으로 보낸다.
  - crash 뒤 같은 범위를 다시 읽으면 같은 token이 나와 insert가 무시된다.
  - 단일 node 테이블은 `non_replicated_deduplication_window = 1000`으로 이 기능을 켠다(migration 00002). Replicated 테이블은 기본 window를 쓴다.
- **범위가 달라진 replay:** token이 달라 행이 다시 들어간다. 이때는 ReplacingMergeTree merge와 query의 key 기준 dedup(`LIMIT 1 BY`, ADR 0018 §5)이 흡수한다. 즉 token은 비용 절감 수단이고, 정확성은 record key가 보장한다.

### 4. 최초 승인 값 유지: version = UInt64 최댓값 − ingress 수신 ms

- ReplacingMergeTree는 version이 가장 큰 행을 남긴다. query도 version이 가장 큰 행을 고른다.
- 먼저 수신한 record일수록 version을 크게 두면, 처리 순서와 관계없이 최초 수신 값이 남는다. 수신 시각은 header의 ingress 수신 시각이다.
- **같은 batch 안의 충돌**은 건수로 세고 로그로 경고한다.
  - span: 같은 key인데 payload hash가 다르면 최초 수신 값을 남긴다.
  - metric: 같은 (tenant, stream, start, end)인데 point hash가 다르면 event_id가 달라 두 행이 모두 저장된다(D02 §05 "상충 값 격리·경고"). redaction이 서로 다른 series를 합친 경우(ADR 0019 §5)도 여기에 걸린다. rollup은 이 상태의 window를 자동으로 합산하면 안 된다(rollup 구현 때 격리한다).
- **이 규칙의 한계** (충돌 격리 state를 구현할 때 함께 해결한다)
  - **같은 ms 수신:** version이 같아진다. ReplacingMergeTree는 나중에 insert된 행을 남기고, query `LIMIT 1 BY`는 둘 중 아무 행이나 고른다. 대부분 같은 내용의 재전송이지만, 내용이 다르면 최초 값이 보장되지 않는다.
  - **정렬 키의 event_time·service_id:** 재전송에서 시작 시각이나 서비스가 바뀌면 merge되지 않는다(D02 §09는 이 경우를 충돌로 격리하라고 한다). 조회 범위에 따라 나중 행이 보일 수 있다.
  - **수신 시각의 원천:** "최초"는 각 ingress pod의 wall clock 기준이라 pod 간 clock skew의 영향을 받는다.
- **아직 없는 것:** batch를 넘는 충돌 검출과 `quarantined_conflict` 분리(D02 §21)는 partition owner의 durable key/hash state가 필요하다. tail sampler의 checkpoint/changelog와 같은 기반 위에 만든다.

### 5. 정규화 규칙

| 항목 | 규칙 |
|---|---|
| service_id | 자연 키 (tenant, `deployment.environment.name`, `service.namespace`, `service.name`)의 SHA-256 앞 128비트로 만든 UUID v8 (D02 §08). `service.name`이 없으면 OTel 기본값 `unknown_service`. catalog는 같은 함수로 ID를 만들거나 alias로 연결한다 |
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
- **poison batch:** sink가 같은 batch에서 결정적으로 실패하면(예: 변환 오류), 그 partition은 재시작을 반복하며 멈춘다. 지금은 유실보다 정지를 택한다. record 단위로 quarantine으로 돌리는 경로와 runbook은 G1 전에 만든다.
- **replica 확인:** `MONTRACER_CH_INSERT_QUORUM`이 0보다 크면 `insert_quorum`을 보낸다. production은 2로 둔다.

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| batch token 범위 | partition 단위 offset 범위 | poll 단위: poll 구성이 실행마다 달라 token이 재현되지 않는다. token 없음: 짧은 crash 재처리마다 raw 행이 두 배가 된다 |
| 최초 값 유지 | 역순 version | 수신 시각 version: 마지막 재전송이 이긴다(D02 §21 위반). 별도 state 조회: 매 record마다 저장소 조회 비용이 든다. state 기반은 충돌 격리 구현 때 추가한다 |
| service_id | 자연 키 결정적 UUID | catalog 조회 후 발급: catalog가 아직 없고, 수집 경로에 동기 의존이 생긴다 |
| quarantine 내용 | 위치·해시만 | 원문 보존: 해석 실패 record의 redaction 상태를 보장할 수 없다 |
| sink 장애 | 예산 후 종료·재시작 | 무한 재시도: rebalance가 막혀 group 전체가 멈춘다. 실패 batch를 건너뛰고 commit: 유실이 생긴다 |

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
  - metric: 같은 stream·시각에 다른 값이 오면 충돌로 센다. 다른 tenant는 충돌이 아니다.
  - batch가 결정적이고 token 형식이 고정돼 있다.
  - header 위조·누락·중복, schema 버전, signal, event_id 불일치, 손상 value, 다중 record value는 quarantine되고 원문이 없다.
  - log uid·gen 형식, 5개 metric 유형의 컬럼 매핑(sum 없는 histogram은 NaN), service_id 자연 키 규칙, 만료 시각 범위 자르기.
  - sink 재시도 성공과 예산 초과 종료.
- 통합 테스트 (Kafka + ClickHouse)
  - **insert 성공 → commit 전 crash → 재시작:** crash 뒤 offset이 그대로이고, 재처리 결과 span 20개(재전송 5개 포함 입력), metric 5개, log 1개가 논리 중복 없이 남는다. 재전송 값이 최초 값을 덮지 않는다.
  - tenant B 분리, 위조 record quarantine, lookup MV가 채워짐.
  - 같은 batch 3회 쓰기 → raw 1행(token dedup).
  - 관리자 DSN으로 sink 기동 거부.
- CI migration up → down → up (00002 포함).
