# ADR 0046: metric 사전 API — `GET /api/v1/metrics`, `/metrics/labels`

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-08 "S08 Metrics" 선택)
- Owner: API lead
- 날짜: 2026-10-08 (제안·결정)
- 관련: F04, F05, E03 · D02 §07·§10·§13·§15·§19, D05 §08 · 계약 4·6 · ADR 0018, 0025, 0026, 0027, 0028, 0032, 0034, 0047

## 배경

D05 §08: "Metrics는 metric dictionary에서 type·unit·temporality를 먼저 보여준다. query builder는 gauge에 무의미한 rate 또는 summary quantile 평균을 허용하지 않는다."

하지만 조회 API는 `POST /query/metrics`(ADR 0027)뿐이다. 이름을 알아야 부를 수 있고, 유형을 몰라 연산을 고를 수 없다. group_by·filter에 쓸 label key도 알 수 없다. D02 §13 표에는 사전 경로가 없다. `GET /capabilities`는 연산·한도의 일반 목록이지 tenant가 보낸 metric 목록이 아니다.

## 결정

### 1. `GET /api/v1/metrics?from&to[&q][&limit][&cursor]`

- **범위:** 필수이고 최대 24시간이다. 넘으면 422 `QUERY_BUDGET_EXCEEDED`(`max_range_seconds`)다.
  - 1분 rollup(`metric_1m`)을 읽는다. 원본(`metric_points`)은 query 계정이 읽지 않고 행도 많다.
  - 화면은 조사 범위의 끝에서 24시간을 쓴다(ADR 0047).
- **검색:** `q`는 이름 부분 일치이고 대소문자를 무시한다(최대 256 byte, server-side parameter).
- **page:** 이름순 keyset이다. `limit`은 기본 200, 최대 1,000(D02 §19)이다.
  - cursor는 apicursor(ADR 0034)이고, tenant·권한 fingerprint·query hash(범위·q)에 묶인다.
  - 만료 판정 시각(snapshot)을 page 사이에 고정한다.
- **항목:** `{name, variants[], conflict}`
  - variant = `{type, temporality, monotonic, unit, series, last_seen, aggregations}`
  - **한 이름에 조합이 둘 이상이면 모두 돌려주고 `conflict=true`다.** 계측 충돌(D02 §10 "unit 충돌")을 하나로 고르지 않는다.
  - `series`는 범위 안에서 관측된 stream 수다. `last_seen`은 가장 늦은 1분 window의 끝이다.
- **`aggregations`:** 그 조합에 의미가 있는 연산이다(D02 §07 Metric 정규화 계약). 화면이 이 목록으로만 연산을 고르게 한다(ADR 0047).

  | 조합 | rollup이 만드는 값(`metricagg.Compute`) | 연산 |
  |---|---|---|
  | monotonic delta·cumulative sum (counter) | 증가량 | rate, increase, sum |
  | gauge, non-monotonic cumulative sum, **unspecified sum(단조 여부 무관)** | 원시 값 | avg, min, max ("임의 rate 적용 금지") |
  | non-monotonic delta sum | 증가량(순변화) | sum (step 안 순변화) |
  | histogram | bucket 상태 | p50, p90, p95, p99, count, hist_sum |
  | summary, exponential histogram | 없음(원본 표시만) | 없음 (summary quantile은 합칠 수 없음) |

  - **이 표는 rollup이 실제로 계산하는 값과 같아야 한다.** 다르면 화면 기본 연산이 영구 `not_applicable`이 된다. 시험 `TestAllowedAggregationsMatchRollup`이 모든 (유형, temporality, monotonic) 조합을 `metricagg.Compute`로 돌려 대조한다.
    - 처음 표는 unspecified sum을 "단조면 counter, 아니면 없음"으로 적었다. rollup은 unspecified를 원시 값으로 다룬다(리뷰 P1).
  - 서버 `/query/metrics`는 지금처럼 맞지 않는 연산에 `not_applicable`을 돌려준다(ADR 0027). 이 목록은 그 앞의 안내다.

### 2. `GET /api/v1/metrics/labels?metric&from&to`

- 그 metric의 label key를 series가 많은 순으로 돌려준다: `{key, sources, series}`
  - `sources`: `attribute`(point)·`resource` 중 관측된 곳
  - metric 조회는 point 속성을 먼저, 없으면 resource를 본다(ADR 0027). 같은 key가 두 곳에 있으면 하나로 합친다.
- **stream마다 범위 안 가장 늦은 window의 label**을 쓴다. metric 조회와 같은 label 원천(`metric_1m`의 label 컬럼, ADR 0027 §4)이다.
- 200개를 넘으면 자르고 `meta.warnings`에 `label_keys_truncated`를 단다.
- **값 목록은 돌려주지 않는다.** 값 제안은 cardinality preview(D02 §10)·정책과 함께 정한다(후속).

### 3. 공통

- **mandatory predicate(D02 §15):** tenant, 시간, `expires_at`. row policy가 tenant를 한 번 더 막는다(ADR 0018). 삭제 tombstone predicate는 삭제 원장(F09)과 함께 넣는다(ADR 0018 §7, `/query/metrics`와 같다).
  - environment로 제한된 key는 허용 environment의 stream만 본다. `/query/metrics`와 같은 predicate다.
- **parameter:** 검색어·metric 이름·environment 목록은 모두 서버 측 parameter다(ADR 0032). query_log 시험에 두 조회를 더했다.
- **실행:** tenant 동시 실행 gate(ADR 0037), `Cache-Control: no-store`, 조회 시간 상한
- **설정:** query-api의 `MetricCatalog`가 없으면 두 경로는 404다. 사전 page에는 cursor key도 필요하다.

### 4. `/query/metrics` 응답 보강 (ADR 0027·0028 변경)

`data`에 두 필드를 더한다. 기존 필드와 의미는 바뀌지 않는다.

- `source_window_seconds`: 실제로 읽은 rollup 해상도다(60 = `metric_1m`, 3600 = `metric_1h`).
  - 1시간 step이라도 1시간 rollup이 범위를 덮지 못하면 1분을 읽는다(ADR 0028 §2).
  - 화면 legend가 "어느 rollup인지"를 추측하지 않게 한다(D05 §08 "해상도 변경·rollup 표시").
- `range`: step 경계로 맞춘 실제 조회 범위 `[from, to)`

### 5. 하지 않는 것

- label 값 제안, description(metadata 테이블, D02 §10), exemplar
- 사전 범위 24시간 초과(1시간 rollup으로 넓히기)
- 이름 정규식·prefix 계층(namespace 탐색)

## 외부 사례 근거 (2026-10-08 확인)

| 사례 | 내용 | 반영 |
|---|---|---|
| Prometheus HTTP API ([querying/api](https://prometheus.io/docs/prometheus/latest/querying/api/)) | `/api/v1/metadata`는 metric 이름별 type·help·unit 목록(이름마다 여러 항목 가능). `/api/v1/labels`·`/label/<name>/values`는 `start`·`end`로 범위를 받는다 | 결정 1(이름별 조합 목록, 충돌을 여러 항목으로)·결정 2(범위 안 label key). 다르게: 범위 필수(전체 기간 scan 금지) |
| Grafana Prometheus query editor ([docs](https://grafana.com/docs/grafana/latest/datasources/prometheus/query-editor/)) | metrics explorer에서 metric의 이름·유형·설명 목록을 보고, builder가 고른 metric에 맞는 연산 힌트를 준다 | 결정 1의 `aggregations`(유형별 허용 연산을 서버가 알려준다) |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 사전 원천 | `metric_1m` 집계 | 별도 metric catalog 테이블(제어 DB): 동기화 경로가 하나 더 생긴다(ADR 0038의 서비스 catalog처럼). 사전은 조회용이라 rollup으로 충분하다. 규모가 커지면 재검토 |
| 범위 | 필수, 24시간 | 생략 시 전체 보존 기간: 90일 scan. 7일: 1분 rollup 행이 7배 |
| 충돌 | 조합 모두 + `conflict` | 가장 많은 조합 하나: 다른 계측을 숨긴다(계약 6 취지) |
| 허용 연산 | 서버가 계산 | 화면이 규칙을 가짐: 다른 client(dashboard·monitor 편집기)가 같은 규칙을 다시 구현해야 한다 |
| label 값 | 돌려주지 않음 | 상위 N 값: cardinality·민감값 노출 정책(D02 §10 preview)이 먼저다 |

## 결과

- 화면이 metric을 이름으로 찾고, 유형에 맞는 연산과 label key로 조회를 만든다(ADR 0047).
- **schema 변경:** 없음(기존 `metric_1m` 읽기).
- **API 변경:** 경로 2개 추가, `/query/metrics` 응답 `data`에 필드 2개 추가(호환).
- **tenant 영향:** 없음. 기존 query 계정·row policy·environment predicate를 그대로 쓴다.
- **비용:** 사전은 tenant의 24시간 `metric_1m`을 이름으로 묶는다. 정렬 key `(tenant_id, metric_name, …)`로 읽는다. 큰 tenant의 비용은 부하 시험(100k series)에서 본다.

## Rollback

query-api의 `MetricCatalog`를 비우면 두 경로는 404다. 응답 필드 추가는 되돌릴 필요가 없다(무시 가능).

## 재검토 조건

- 사전 조회 p95가 2초를 넘을 때: 이름·조합 요약 테이블(materialized view)을 둔다
- label 값 제안 요구: cardinality preview·dimension 정책(F07)과 함께
- description·instrumentation scope 표시: metadata 테이블(D02 §10)

## 증거

- `internal/query/metric_catalog_test.go`
  - 유형별 허용 연산(up-down counter·delta·summary·exponential histogram 포함)
  - page 순회·cursor binding(다른 q 거절)·snapshot 고정, 충돌 표시
  - 거절(범위 없음·거꾸로·24시간 초과·limit·위조 cursor·metric 없음)이 저장소에 닿지 않음, 401·ingest key 거절
  - environment 제한 key 전달, label key 응답과 잘림 경고, 설정 없으면 404
- `TestAllowedAggregationsMatchRollup`: 허용 연산 표 ↔ rollup이 만드는 값(증가량·원시 값·histogram) 전 조합 대조
- `tests/isolation`: B는 A의 metric 이름·label key를 보지 못하고(tenant header 무시) A의 사전 cursor는 400
- spec-reviewer: P0 없음. P1(unspecified sum 허용 연산이 rollup과 다름)과 P2(tombstone 기록, isolation 시험, 문서) 반영
- `internal/query/metrics_test.go`: `source_window_seconds`(1시간·1분 대체)와 맞춘 범위
- `internal/telemetrystore/metric_catalog_integration_test.go`(ClickHouse)
  - 이름순·조합(단위 충돌)·series 수·마지막 관측, 대소문자 무시 검색, keyset
  - 범위 밖·만료 제외, environment 제한 key, 다른 tenant 분리, SQL 조각 검색어는 이름 일치로만
  - label key: 최근 window label, attribute·resource 합침, series 순
- `querylog_integration_test.go`: 사전·label 조회의 검색어·metric 이름·environment가 query_log에 없다
- `make smoke`(CI): oracle metric 두 개의 유형·허용 연산과 histogram label key(`http.response.status_code`)
