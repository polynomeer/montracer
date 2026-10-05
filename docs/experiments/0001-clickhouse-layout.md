# 실험 0001: ClickHouse layout과 multi-tenant 조회 지연

- 상태: **완료 (layout·지연 질문). 실제 OTLP payload 크기 측정은 Sprint 2 후속**
- 관련: 작업계획서 §5.1 기술 검증 #3 "ClickHouse multi-tenant query latency", ADR 0003·0018, D01 §08(검색 p95 2초), D02 §09
- 실험 프로그램: [`tests/load/chlayout`](../../tests/load/chlayout/main.go)

## 질문

D02 §09의 원본 테이블 layout으로 아래 두 가지가 가능한가?

1. MVP 부하에서 검색 p95 2초를 달성할 수 있는가? 부하는 tenant 10개, 서비스 100개, 평균 10k spans/s (D01 §02).
2. tenant row policy를 적용한 상태에서도 정렬 키가 granule을 걸러내는가?

## 방법

- **데이터 생성**
  - 관리자 계정이 `INSERT … SELECT FROM numbers()`로 서버 안에서 합성 span을 만든다.
  - trace당 10 span, 오류 1%, 속성 6개, payload 약 400B다.
  - tenant와 서비스는 trace 단위로 고르게 나눈다.
- **측정**
  - query 계정이 row policy를 적용한 채 쿼리를 반복 실행한다.
  - 지연은 클라이언트에서 결과 수신까지 잰다.
  - 읽은 행·바이트는 `system.query_log`, 저장 크기는 `system.parts`·`system.parts_columns`에서 읽는다.
- **쿼리**

  | 쿼리 | 조건 |
  |---|---|
  | trace search | 한 서비스, 최근 1시간, 최신 100건 (D01 §08 측정 조건) |
  | error trace search | tenant 전체, 최근 1시간 |
  | service RED | 한 서비스, 최근 1시간, 1분 버킷 (참고용) |
  | trace by id | lookup 테이블을 거친 원본 조회 |

## 결과 1: 로컬 smoke (2026-10-05)

- **환경:** MacBook(Apple Silicon), Docker Desktop VM 7.65GiB를 다른 로컬 프로젝트와 공유했다. ClickHouse 26.8.15.10 단일 node였고, 서버 메모리 한도는 약 1.4GiB였다.
- **규모:** 100만 span, 1시간 분포 (평균 약 278 spans/s 밀도로, 목표의 약 1/36), 쿼리별 10회.
- **주의:** 10회 표본이라 아래 "p95"는 사실상 최댓값이다. 분포 판단에는 쓰지 않는다.

| 쿼리 | p50 | p95 | 평균 읽은 행 | 평균 읽은 바이트 |
|---|---|---|---|---|
| trace search: 1 service, 최근 1h, 최신 100 | 6.8 ms | 26.9 ms | 34,581 | 1.7 MiB |
| error trace search: tenant 전체, 최근 1h | 5.8 ms | 9.7 ms | 73,552 | 2.8 MiB |
| service RED: 1 service, 최근 1h, 1분 버킷 | 8.5 ms | 17.6 ms | 33,481 | 1.5 MiB |
| trace by id (lookup → 원본) | 11.6 ms | 21.0 ms | 86,612 | 3.4 MiB |

전체 100만 행 중 단일 서비스 쿼리는 약 3.5%를 읽었다. tenant(1/10)와 서비스(1/100)로 걸러지지만, granule(8,192행) 단위라 그 이상 걸러내지는 못했다.

**질문 2(row policy만으로 pruning되는가):** 통합 테스트 `TestRowPolicyAloneUsesPrimaryKey`에서 확인했다. 3개 tenant × 1만 span을 넣고 쿼리에 tenant predicate 없이 row policy만 적용했다. `EXPLAIN indexes = 1`의 PrimaryKey 단계에서 선택된 granule이 전체보다 적었다.

**저장 크기:** 압축 86.9MiB, 비압축 806MiB(9.3배), span당 압축 91B.

| 컬럼 | 압축 MiB | 비압축 MiB | 압축률 |
|---|---|---|---|
| payload_hash | 29.8 | 29.7 | 1.0x |
| payload | 24.1 | 389.3 | 16.2x |
| span_id | 7.5 | 7.4 | 1.0x |
| parent_span_id | 7.1 | 7.4 | 1.0x |
| attributes | 6.6 | 243.7 | 36.9x |
| duration_ns | 5.6 | 7.4 | 1.3x |
| trace_id | 2.0 | 14.8 | 7.6x |
| event_time | 0.9 | 7.4 | 8.0x |

## 결과 2: 목표 밀도 (2026-10-05, GitHub runner)

- **실행:** `experiment-chlayout` workflow [run 37254176615](https://github.com/polynomeer/montracer/actions/runs/37254176615) (main `ee0ef8e`)
- **환경:** GitHub ubuntu runner (4 vCPU, 15GiB RAM), ClickHouse 26.8.15.10 단일 node, 로컬 compose와 같은 config·계정·row policy
- **규모:** 3,600만 span을 최근 1시간에 분포시켰다. MVP 평균 10k spans/s × 1시간이다 (D01 §02). tenant 10개 × 서비스 100개, active part 17개, 쿼리별 50회.
- **생성:** 3,600만 span에 1분 18초가 걸렸다(서버 내부 생성이라 수집 경로 처리량과는 무관하다).

| 쿼리 | p50 | p95 | p99 | 평균 읽은 행 | 평균 읽은 바이트 |
|---|---|---|---|---|---|
| trace search: 1 service, 최근 1h, 최신 100 | 18.3 ms | **24.1 ms** | 53.0 ms | 171,773 | 9.8 MiB |
| error trace search: tenant 전체, 최근 1h | 46.4 ms | 49.6 ms | 50.9 ms | 4,432,066 | 137.7 MiB |
| service RED: 1 service, 최근 1h, 1분 버킷 | 17.9 ms | 21.9 ms | 39.8 ms | 174,360 | 8.3 MiB |
| trace by id (lookup → 원본) | 22.7 ms | 26.2 ms | 28.0 ms | 188,416 | 7.3 MiB |

**저장 크기:** 압축 3,062MiB, 비압축 29,015MiB(9.5배), span당 압축 89B. smoke와 거의 같다.

| 컬럼 | 압축 MiB | 비압축 MiB | 압축률 |
|---|---|---|---|
| payload_hash | 1103.1 | 1098.6 | 1.0x |
| payload | 862.0 | 14402.1 | 16.7x |
| span_id | 275.8 | 274.7 | 1.0x |
| parent_span_id | 263.7 | 274.7 | 1.0x |
| attributes | 208.2 | 9015.7 | 43.3x |
| duration_ns | 206.6 | 274.7 | 1.3x |
| trace_id | 72.3 | 549.3 | 7.6x |
| event_time | 35.1 | 274.7 | 7.8x |

### 판정

- **질문 1: 가능하다.** D01 §08 측정 조건(최근 1시간, 단일 서비스, 제한 내 hot query)의 검색 p95는 24ms, p99는 53ms였다. 목표 p95 2초·p99 5초보다 약 80배 낮다. 서비스를 지정하지 않은 tenant 단위 검색도 p95 50ms다.
- **질문 2: 그렇다.** row policy만으로 granule이 줄어드는 것을 통합 테스트로 확인했다(결과 1 참조). 단일 서비스 쿼리는 tenant의 1시간 데이터 360만 행 중 약 17만 행(약 4.8%)만 읽었다.
- **ADR 0003 재검토 조건** "10 tenant·10k spans/s 조건에서 p95 2초 미달"은 **해당하지 않는다.** ClickHouse 통합 저장 결정을 유지한다.

## 해석과 한계

- **smoke 결과는 정확성 확인용이다.** layout, row policy, 쿼리 형태가 동작하고 primary key가 쓰인다는 점을 확인했다. 지연 판단은 결과 2를 쓴다.
- **결과 2의 여유는 조건부다.** 동시 쿼리(D01 §02: 20개), 동시 insert·merge 경합, 3배 burst, 7일 보존 누적, replica·분산 구성이 없는 상태에서 잰 값이다. 이 조건은 D06 §03 부하 시험(Dashboard 동시 20명, Burst, Soak)에서 다시 확인한다.
- **합성 payload는 실제보다 훨씬 잘 압축된다**(반복 문자열). 실제 span byte와 압축률은 수집 worker가 OTLP payload를 저장하기 시작한 뒤(Sprint 2) 다시 측정해 D04 §06 산식에 넣는다(D06 §03 "용량 확정").
- 단일 node에 replica·merge 경합이 없다. production 3AZ 구성 수치가 아니다 (D01 §02).

## 관찰과 후속 제안

| 관찰 | 제안 | 결정 방법 |
|---|---|---|
| `payload_hash`(32B)가 압축 크기의 34%로 가장 크다. 압축되지 않는 hash다 | 충돌 감지(D02 §21)는 partition owner의 changelog에 두고, 원본 테이블에서는 빼거나 16B로 줄이는 방안을 검토한다 | dedup 구현(Sprint 2) 후 ADR |
| `span_id`·`parent_span_id`는 무작위라 압축되지 않는다(span당 16B) | 정렬 키상 피할 수 없다. 그대로 둔다 | — |
| 서비스를 지정하지 않은 tenant 단위 쿼리는 서비스 단일 쿼리보다 2배 많이 읽는다 | trace_summary(D02 §10)·projection 도입 시 비교한다 | 목표 밀도 결과 확인 후 |
| `event_time`·`duration_ns`는 기본 LZ4에서 압축률이 낮다 | `Delta, ZSTD` 등 codec을 비교한다 | 목표 밀도 실행에 codec 변형을 추가 |

결론: D02 §09 layout과 ClickHouse 통합 저장(ADR 0003)은 MVP 단일 쿼리 지연 목표를 큰 여유로 만족한다. 남은 검증은 두 가지다.
- 실제 OTLP payload의 크기와 압축률. 수집 worker 구현 후 D04 §06 용량 산식에 반영한다.
- 동시성·혼합 부하 시험 (D06 §03).
