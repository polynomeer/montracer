# ADR 0017: OTLP 수신의 복잡도 한도와 record 검증 규칙

- 상태: 제안 (구현 PR에 적용됨, 승인 대기)
- Owner: Data lead
- 날짜: 2026-10-05 (제안)
- 관련: F01, E02 · D02 §04, §05, §07 · D03 §02 · D04 §02 · ADR 0001, 0002

## 배경

명세가 정한 수신 한도는 다음과 같다.

- 압축 해제 후 8MiB (D02 §04)
- Kafka message 1MiB, 큰 record는 거절 (D02 §05)
- 시간 범위: 미래 5분, 과거 24시간 (D02 §07)
- SDK 운영 제한: span 64KiB, 속성 128개, 값 4KiB, log body 32KiB (D03 §02)

`spec-reviewer` 실측 결과, 바이트 한도만으로는 다음을 막지 못한다.

1. **증폭**: 빈 메시지를 반복한 8MiB protobuf는 decode 후 heap 약 580MiB(약 80배)를 쓴다. gzip으로 보내면 wire 크기는 수 KiB다.
2. **깊은 중첩**: kvlist를 수십만 단 중첩하면 decoder 재귀로 stack이 수백 MiB까지 늘어난다. 한도를 조금만 올려도 복구할 수 없는 stack overflow로 프로세스가 죽는다.
3. **크기 계산 누락**: span의 status message, trace state, 빈 event·link 수, resource·scope 속성, log record 전체 크기가 검사되지 않는다. 그 결과 1MiB를 넘는 envelope가 Kafka append 단계에서 실패할 수 있다.

## 결정

### 1. decode 전 pre-scan (요청 단위, 413)

본문을 decode하기 전에 한 번 훑어 아래 한도를 넘으면 `ErrTooComplex`로 거절한다. client에게는 배치를 나누라고 안내한다.

| 한도 | 시작값 | 측정 대상 |
|---|---|---|
| 요소 수 | 500,000 | protobuf: OTLP 하위 메시지 수. JSON: object·array 수 |
| protobuf 중첩 깊이 | 32 | OTLP 기본 구조는 7~8단. 남는 깊이에 AnyValue를 약 8단까지 중첩할 수 있다 |
| JSON 중첩 깊이 | 64 | JSON은 AnyValue 한 단이 약 4단이다 |

- protobuf 스캐너는 opentelemetry-proto의 하위 메시지 필드 번호만 안다. 이 번호는 pdata v1.68.0 생성 코드에서 추출했다. scalar와 bytes 필드는 건너뛴다.
- 스캐너의 재귀 깊이는 한도로 묶여 있다. JSON은 tokenizer로 훑으므로 값을 메모리에 만들지 않는다.
- 수치는 설계 가정이다. 속성 하나는 요소 2개(KeyValue와 AnyValue)이므로, 일반적인 SDK 배치(512 span)는 수만 개 수준이다. 부하 시험(D06 §03)으로 조정한다.
- 이 단계는 요청 하나의 비용만 묶는다. ingress 전체의 동시 decode 바이트 예산과 tenant별 공정성은 ingress 구현(Sprint 2)에서 정한다.

### 2. record 단위 크기 근사

- 원소마다 고정 비용 8바이트를 더한다. span·log record·data point 자체에도 고정 비용을 둔다. 빈 원소를 무한히 늘려 크기 검사를 통과하는 것을 막기 위해서다.
- span 크기에 포함하는 항목: 이름, trace state, status message, event(이름과 속성), link(trace state와 속성).
- **envelope 한도 1MiB (D02 §05)**: resource와 scope의 속성·이름·schema URL은 모든 record envelope에 복제된다. 그래서 `resource + scope + record` 근사 크기가 1MiB를 넘으면 그 record를 `record_too_large`로 거절한다. metric은 이름·설명·단위·metadata와 bucket·exemplar도 포함한다.

### 3. 속성 한도의 적용 범위

D03 §02 표의 속성 한도(128개, 값 4KiB)는 trace 행에만 적혀 있다. 이를 다음에도 같게 적용한다.

- log record 속성
- metric point 속성과 exemplar 속성
- resource와 scope 속성

이유는 위 2와 같다. 속성 한도가 없으면 envelope 크기를 묶을 수 없다. resource·scope 속성이 한도를 넘으면 그 아래 record 전체를 `resource_attributes_invalid`로 거절한다.

### 4. log 시간과 연결

- Timestamp와 ObservedTimestamp가 모두 0이면 ObservedTimestamp를 수신 시각으로 채운다. OTel 데이터 모델이 receiver에 권하는 동작이다. 이렇게 하면 envelope의 event_time이 1970년이 되지 않는다.
- span_id만 있고 trace_id가 없는 log는 거절하지 않는다. 연결만 할 수 없을 뿐 고객 log를 버리지 않는다. 연결 필드 정리는 정규화 단계에서 한다.

### 5. 요청 경계

| 경우 | 처리 |
|---|---|
| 빈 본문 | protobuf·JSON 모두 record 0건의 정상 요청 |
| 본문 읽기 실패 (연결 끊김 등) | `ErrBodyRead`로 구분한다. 경계에서의 status는 ingress 구현 시 정한다 |
| record 전부 거절 | HTTP 200 + partial_success (OTLP 규격). ingress 경계 테스트에 포함한다 |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 증폭·깊이 방어 | schema-aware pre-scan | decode 후 record 수 검사: 메모리를 이미 쓴 뒤라 늦다. 바이트 한도 축소: 비율 문제라 해결되지 않는다 |
| span 크기 | 근사 + 고정 비용 | proto 직렬화 크기: record마다 직렬화하는 비용이 크다 |
| 속성 한도 범위 | 모든 record·resource·scope | trace에만 적용: envelope 크기를 묶을 수 없다 |

## 결과

- **명세 개정 요청**:
  - D03 §02 표에 속성 한도의 적용 범위를 추가한다(log, metric, resource, scope).
  - D02 §04 또는 §05에 요청 복잡도 한도(요소 수, 깊이)를 추가한다.
- 거절 사유가 추가된다: `resource_attributes_invalid`, `record_too_large`의 적용 범위 확대. metric과 응답 message에 사용한다.
- pre-scan은 본문을 한 번 더 읽는다. CPU 비용은 부하 시험에서 측정한다.

## Rollback

`Limits`와 `Rules`의 값을 바꿔 완화할 수 있다. pre-scan을 끄면 증폭·깊이 공격에 다시 노출되므로, 끄려면 대체 방어(별도 프로세스 격리 등)를 ADR로 정해야 한다.

## 재검토 조건

- 부하 시험에서 정상 배치가 요소 한도에 걸리거나, pre-scan CPU가 ingress 예산을 넘을 때.
- OTLP proto에 새 하위 메시지 필드가 추가될 때. pdata 업그레이드 시 필드 번호를 다시 추출한다.

## 증거

- `internal/telemetry/otlp` 테스트:
  - 증폭 본문(protobuf·JSON)과 1,000단 중첩(protobuf·JSON)을 거절한다
  - 정상 중첩은 통과한다
  - span 크기의 모든 가변 필드를 검사한다
  - resource·scope 속성 위반을 처리한다
  - envelope 1MiB를 검사한다
  - 정확히 한도인 값(크기, 속성 128개, ±5분·24시간 시간 경계)은 통과한다
  - 빈 본문, 본문 읽기 실패, 기본 한도에서의 gzip bomb을 처리한다
