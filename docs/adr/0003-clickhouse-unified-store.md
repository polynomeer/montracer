# ADR 0003: ClickHouse에 trace·log·metric 통합 저장

- 상태: 승인
- Owner: Data lead
- 승인자: polynomeer
- 날짜: 2026-10-03 (제안: D06 §08) / 2026-10-03 (결정)
- 관련: F02~F06, E03 · D02 §01, §09~10, §15 · ADR 0010(예정)

## 배경

MVP는 세 신호의 correlation(trace→log, exemplar→trace)과 운영 단순성이 중요하다. 신호별 전용 엔진(Tempo, Loki, Mimir 등)을 각각 운영하면 삭제·DR·비용 원장·권한 필터를 엔진마다 구현해야 한다.

## 결정

- trace·log·metric 원본과 rollup을 **ClickHouse**에 저장한다. 제어 데이터는 PostgreSQL(RLS + outbox).
- 원본 테이블은 `ReplacingMergeTree(version)`, `ORDER BY (tenant_id, …, event_time, …)`, `expires_at` TTL. production은 2 replica 이상 `ReplicatedReplacingMergeTree` + shard별 Distributed read table (D02 §09).
- background merge를 uniqueness 보장으로 간주하지 않는다. query는 bounded 범위에서 key 기준 dedup하며 `FINAL` 전체 scan을 기본으로 쓰지 않는다. TTL은 즉시 접근 차단 수단이 아니므로 query가 `expires_at`을 필터링한다.
- metric은 MVP 연산(rate, sum, avg, min, max, histogram_quantile, group_by)만 지원하고 PromQL 비호환을 `GET /capabilities`에 명시한다.
- query service만 ClickHouse에 접근한다. 사용자에게 DB 자격증명을 발급하지 않는다.

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A. ClickHouse 통합 (채택) | 단일 SQL 집계·correlation, 운영 엔진 1개, 압축 효율 | temporality·histogram 병합·검색 DSL 직접 구현, 전문 검색·PromQL 미제공 |
| B. Tempo + Loki + Mimir | 각 신호 표준 쿼리(PromQL 등) | 엔진 3개 운영, 삭제·DR·권한을 엔진별로 구현 |
| C. Elasticsearch/OpenSearch 중심 | 전문 검색 강함 | metric 비용·cardinality 취약, 운영비 |

## 결과

- 이점: 초기 운영과 correlation 단순화.
- 비용: metric 정규화·rollup·cardinality 제어 직접 구현, merge·part 운영 runbook 필요.
- 영향 받는 계약: `migrations/clickhouse`, query planner의 mandatory predicate, P0 기술 검증(multi-tenant query latency, histogram 저장 byte).

## Rollback

PromQL 필수 또는 고카디널리티 부하가 Core query SLO를 침해하면 metric만 전용 TSDB로 분리한다(stream identity·단위·결과 계약 유지). 추가 엔진은 삭제·DR·비용 원장을 함께 구현해야 한다 (ADR 0010).

## 재검토 조건

- P0 기술 검증에서 10 tenant·10k spans/s 조건 검색 p95 2초를 만족하지 못할 때.
- PromQL·full-text(형태소·ranking) 요구가 확정되고 운영비가 분리 비용을 넘을 때.

## 증거

- 설계 근거: D02 §01, §09~10, §15, §23 [R5, R8], D06 §08~09.
- 실측 증거: P0 ClickHouse 부하 PoC 보고서 (작성 예정).
