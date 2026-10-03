# ADR 0002: 수집 ACK 경계는 Kafka durable append

- 상태: 승인
- Owner: Data lead
- 승인자: polynomeer
- 날짜: 2026-10-03 (제안: D06 §08) / 2026-10-03 (결정)
- 관련: F01, F10, E02 · D01 §02, §08 · D02 §04~05, §21~22

## 배경

수집 ACK가 무엇을 보장하는지가 고객 재전송 동작, 데이터 손실 경계, 수집 SLO의 분모를 결정한다. DB에 직접 쓰면 저장소 장애가 곧 수집 장애가 되고 replay 경로가 없다.

## 결정

- ingress는 인증 → 압축 해제 한도 → decode → **서버 tenant 주입** → 검증 → **PII 제거** → quota → canonical envelope → **Kafka append** 순으로 처리하고, 요청 내 모든 유효 record의 append가 확인된 뒤에만 성공 ACK를 반환한다.
- Producer 설정: `acks=all`, replication factor 3, `min.insync.replicas=2`, idempotence 활성화 (production 기준).
- ACK는 **검색 가능·영구 저장·trace 보존을 의미하지 않는다.** 전체 경로는 at-least-once이며 "exactly-once"라고 부르지 않는다.
- 중복은 신호별 식별자(trace: tenant+trace_id+span_id, log: source_id+file generation+offset, metric: stream_id+start+end+point hash)와 worker checkpoint로 처리한다. worker는 sink durable write 후 offset을 commit한다.
- Kafka retention 24시간. downstream 장애가 잔여 retention 6시간 전까지 해소되지 않으면 incident를 선언한다.
- 로컬 개발은 단일 broker(RF1)를 허용하되 production SLO와 구분한다.

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A. Kafka durable append 후 ACK (채택) | ACK 의미 고정, 장애 시 replay, 저장소와 수집 분리 | Kafka 운영비, 중복 처리 구현 필수 |
| B. ClickHouse 직접 insert 후 ACK | 구성 단순 | 저장 장애가 수집 장애로 전파, replay 불가, tail sampling 상태 처리 어려움 |
| C. 메모리 수신 즉시 ACK | 지연 최소 | crash 시 ACK된 데이터 손실, 보존 SLO 정의 불가 |

## 결과

- 이점: ACK 이후 worker·저장소 장애는 replay로 회복. 단계별 회계(D02 §21) 가능.
- 비용: 3AZ Kafka 운영, dedup·충돌 격리 로직, 장애 주입 시험(append 직후 ACK 전 단절, insert 후 commit 전 crash).
- 영향 받는 계약: OTLP partial success 응답, envelope schema(`api/proto`), ACK crash 시험(`tests/`).

## Rollback

비용 절감을 위해 Kafka를 제거하면 ACK·가용성 계약 자체가 바뀌므로 새 ADR과 D01·D02 개정이 필요하다. 내부 단일 node 환경에서만 직접 쓰기를 예외로 허용한다.

## 재검토 조건

- 관리형 Kafka 비용이 신호당 원가 예산의 120%를 넘을 때 (D06 §09).
- Kafka 호환 대체 제품이 동일 내구성 보장과 라이선스 조건을 만족함이 검증될 때.

## 증거

- 설계 근거: D02 §04~05, §21~22, §23 [R1, R4], D06 §04 장애 주입 지점.
- 실측 증거: P0 Sprint 2 ACK crash 시험 결과 (작성 예정).
