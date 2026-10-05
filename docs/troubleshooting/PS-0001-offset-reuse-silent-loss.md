# PS-0001: Kafka offset 재사용 시 ClickHouse insert dedup이 새 데이터를 조용히 버림

- 날짜: 2026-10-05 (발견·해결)
- 영역: pipeline (worker ingest 역할)
- 영향: commit된 metric·log의 **조용한 유실** — P0 (CLAUDE.md 계약 2의 내구성 의미 위반)
- 수정: `0b3307a`
- 관련: ADR 0021 §3(token 개정), D02 §05·§22

## 증상

로컬 Kafka가 재기동에 실패해 복구한 뒤, ingress가 ACK한 metric·log 일부가 ClickHouse에 없었다. worker는 오류 없이 offset을 commit했고 중복 계수만 늘었다.

## 발견 경위

로컬 stack 복구 후 수동 확인에서 저장 건수가 ACK 건수보다 적었다. 경보는 울리지 않았다. worker 입장에서는 정상 insert였기 때문이다.

## 원인

1. worker는 재시도해도 중복이 생기지 않도록 batch마다 insert dedup token을 붙였다. token은 `topic/partition/offset 범위/table`이었다.
2. ClickHouse는 dedup window 안에서 같은 token의 insert를 중복으로 보고 **성공을 돌려주며 버린다.**
3. topic을 다시 만들거나 cluster를 교체하면 offset이 0부터 다시 시작한다. 새 record가 과거 batch와 같은 offset 범위를 받으면 같은 token이 된다.
4. 결과적으로 다른 데이터가 "이미 쓴 batch"로 판정되어 사라진다. token이 위치만 식별하고 내용은 식별하지 못한 것이 원인이다.

## 해결

token에 행별 (tenant, offset, event_id, payload SHA-256, version) 해시를 붙인다. 같은 범위를 재처리하면 같은 token이 되어 dedup이 유지되고, 같은 offset이라도 내용이 다르면 다른 token이 되어 저장된다.

버린 대안: routing epoch만 token에 넣는 방법은 topic 재생성 시 epoch를 올리는 운영 절차에 의존하고, 실수 한 번이 유실로 이어진다.

## 재발 방지

- 단위 시험: `internal/pipeline/pipeline_test.go` — 재사용된 offset에 다른 내용이 오면 token이 달라야 한다. token은 table마다 달라야 한다.
- 통합 시험: `internal/pipeline/integration_test.go` — 재사용된 offset의 다른 tenant record가 실제로 1행 저장되는지 확인한다.
- ADR 0021 §3에 token 구성과 이유를 기록했다.

## 교훈

멱등 키는 **위치가 아니라 내용**을 식별해야 한다. 외부 시스템(Kafka offset)이 단조 증가를 보장하지 않는 상황(재생성, 교체, 복구)을 멱등성 설계의 입력으로 다룬다. "성공을 돌려주며 버리는" 동작이 있는 저장소에는 유실을 잡을 대사(ACK 수 대 저장 수)가 필요하다.
