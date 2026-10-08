# ADR 0027: metric 조회 API — JSON QuerySpec, 저장소 내 step·group 집계, 결측 표현

- 상태: 승인 (결정 위임)
- Owner: API lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021~0026과 같은 위임). 근거는 §6
- 날짜: 2026-10-05 (제안·결정)
- 관련: F04, F05, E03 · D02 §10, §13, §15, §19 · CLAUDE.md 계약 4·5·6 · ADR 0018, 0022, 0025, 0026

## 배경

D02가 정한 것은 다음과 같다.

| 절 | 내용 |
|---|---|
| §13 | `POST /query/metrics`(range, expression, step_seconds → data.series, meta). 별도 query 언어를 만들지 않는다. metric은 `meta.sampled = null`이고, 누락은 data.series의 completeness·missing 구간으로 표현한다 |
| §19 | metric 범위는 최대 7일, series 최대 1,000개, series당 point 최대 2,000개. filter AST는 깊이 4·leaf 20개 |
| §10 | MVP 연산은 rate, sum, avg, min, max, histogram_quantile, group_by. PromQL과 완전히 호환되지 않음을 capability에 표시한다 |

`metric_1m`(ADR 0026)은 1분 window 단위다. 7일·1분 step이면 stream마다 10,080행이다. query 계정의 기본 결과 행 예산(10,000, ADR 0018)으로는 Go에서 합칠 수 없다. 또 `metric_1m`에는 label이 없었다. 그런데 원본(15일)보다 오래 남는 rollup(90일)은 원본 join으로 그룹화할 수 없다.

## 결정

### 1. 요청 형식 (JSON QuerySpec)

```json
{
  "range": {"from": "2026-10-05T00:00:00Z", "to": "2026-10-05T06:00:00Z"},
  "step_seconds": 300,
  "expression": {
    "metric": "http.server.request.duration",
    "aggregation": "p95",
    "filter": {"op": "and", "args": [{"field": "deployment.environment.name", "op": "eq", "value": "prod"}]},
    "group_by": ["service.name"]
  }
}
```

- **입력 이름은 D02 §13 그대로**(`range`, `expression`, `step_seconds`)다. `expression`은 query 언어가 아닌 **구조화 객체**다(D02 §13 "별도 query 언어를 만들지 않는다").
  - 모르는 필드는 400이다(PromQL 문자열 차단).
  - `sum`은 D02 §10 MVP 연산 목록의 sum이며, counter에서는 step 증가량이다. gauge의 stream별 값 합은 아직 지원하지 않는다(§5).
- **aggregation**

  | 연산 | 대상 | 의미 |
  |---|---|---|
  | `rate` | monotonic sum | 초당 증가율 = step 증가량 / step 초 |
  | `increase`, `sum` | monotonic sum | step 증가량 |
  | `avg`, `min`, `max` | gauge, non-monotonic sum | 평균(합/표본 수), 최솟값, 최댓값 |
  | `count` | histogram | 관측 수 |
  | `hist_sum` | histogram | 관측값의 합 (예: 소요시간 합계, 2026-10-07 추가, ADR 0042) |
  | `p50`, `p90`, `p95`, `p99` | histogram | **bucket 병합 뒤** 분위수(ADR 0025 §5, 계약 5) |

- **filter:** and와 eq leaf만 받는다(깊이 4, leaf 20). or·not·in 등은 문법상 유효하지만 아직 지원하지 않으므로 **422**다.
  - key는 point 속성을 먼저 보고, 없으면 resource 속성을 본다.
- **group_by:** label key 최대 5개다.
- **step:** 60의 배수다. 범위는 step 경계로 맞춘다(`[floor(from), ceil(to))`). 같은 질의를 반복하면 같은 경계가 나온다.
  - 7일 한도는 사용자가 준 원 범위로 검사한다. 정렬로 늘어난 한 step은 허용한다.
  - 예외(2026-10-07, ADR 0042): `step_seconds`가 범위 길이와 같고 `from`이 분 경계면 정렬하지 않고 범위 전체를 한 점으로 집계한다(요약 값). 시작이 시간 경계가 아니면 1분 rollup을 읽는다.

### 2. 집계 위치: 저장소 안에서 (stream, window) → (group, step)

- ClickHouse가 다음 순서로 처리한다.
  1. `(stream, window)`에서 revision이 가장 큰 행을 고른다.
  2. `(group label, step)`으로 합친다.
     - 증가량: 합
     - 값: 합, 표본 수, min·max, 최신 값
     - histogram: count·sum 합, **bucket 배열 원소별 합**, **경계 종류 수**
     - 품질 사유: 합집합. partial: 하나라도 있으면 true
- Go는 bucket마다 연산 값을 계산하고 빠진 step을 채운다.
- 결과 행은 group × step이다. 예산(결과 10,000행)을 넘으면 `QUERY_BUDGET_EXCEEDED`(422)다.
  - 사전 검사도 둔다: 7일 초과 범위와 series당 2,000 step 초과는 `QUERY_BUDGET_EXCEEDED`, step 형식 오류는 400이다.
- **mandatory predicate:** tenant·시간·expires_at이다(D02 §15). query 계정의 row policy도 함께 걸린다(ADR 0018).
- **바인딩:** label key·value·environment 목록은 **모두 parameter binding**이다. SQL에는 고정 template만 쓴다(계약 4). SQL 조각을 담은 필터가 아무것도 일치시키지 않는 것을 통합 테스트로 확인한다.
- **environment 제한:** environment가 제한된 API key는 허용 environment의 stream만 집계한다. environment가 없는 stream은 볼 수 없다(ADR 0022 §3과 같은 원칙).

### 3. 결측과 품질 표현 (D02 §13, 계약 6)

- 모든 step에 point를 둔다. 값이 없으면 `"v": null`과 사유를 둔다.

  | 사유 | 의미 |
  |---|---|
  | `no_data` | 그 step에 window가 없다, 또는 histogram 관측이 0건이다 |
  | `missing_baseline` | counter 기준점이 없어 증가량을 모른다 |
  | `not_applicable` | 그 유형에 이 연산 값이 없다. 예: gauge에 rate(D02 §07 "임의 rate 금지"), NaN |
  | `bounds_mismatch` | 경계가 다른 histogram이 섞여 병합할 수 없다 |
  | `unit_conflict` | 단위가 다른 stream이 한 group에 섞였다. step마다 단위가 달라도 series 전체가 이 사유다 |
  | `type_conflict` | 유형이 다른 stream이 한 group·step에 섞였다 |
  | `pending` | rollup watermark 이후라 아직 계산되지 않았다(`no_data`와 구분) |

- `partial`이 true인 경우는 세 가지다.
  - 집계한 window 중 불완전한 것이 있다(ADR 0025 §4).
  - **합산형 값(증가량·histogram)에서 step 안의 1분 window가 빠졌다.** 기대 window 수는 stream 수 × step/60이다. 빠진 window는 0으로 더해진 셈이라 값이 하한일 뿐이다.
  - step이 **rollup watermark 이후**다(늦은 데이터로 바뀔 수 있다).
- series별 `completeness`(값 있는 step / 전체 step)와 `missing`(연속 결측 구간 `[from, to)`)을 둔다.
- **meta**
  - `resolution_seconds = step`
  - `sampled = null`(metric)
  - `watermark` = 그 tenant의 rollup이 계산을 마친 시각(가장 늦은 `metric_1m` window의 끝). rollup은 tenant별로 진행한다(ADR 0026 §2).
  - `coverage·scan_bytes = null`(아직 계산하지 않음)
  - 같은 metric 이름에 다른 유형이 섞이면 `warnings: ["type_conflict"]`
- NaN·Inf는 JSON 값으로 내보내지 않는다(`not_applicable`).

### 4. metric_1m에 label 저장 (migration 00004)

- `resource_json`·`attributes_json` 컬럼을 추가한다. rollup은 stream의 최신 point label을 함께 쓴다.
- 원본 join 없이 90일 rollup을 group·filter할 수 있다.
- rollup 계정에는 원본의 두 label 컬럼 SELECT를 추가한다.

### 5. 아직 없는 것

- `metric_1h`(1시간 rollup)와 해상도 자동 선택. 지금은 항상 `metric_1m`을 읽으며, 7일·1시간 step이면 7×24 = 168 step이다.
- or·in 필터, label 정규식, `GET /capabilities`의 연산 목록 노출(D02 §10)
- 다른 metric 간 연산(비율 등)과 sum over gauges(stream별 값의 합)
- 조회 결과 cache, rate limit(D02 §12)

### 6. 외부 사례 근거 (2026-10-05 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| §1 JSON QuerySpec (언어 없음) | Honeycomb Query Specification ([docs](https://docs.honeycomb.io/api/query-specification/)) | JSON으로 `calculations`(op: COUNT, AVG, MAX, MIN, P50…), `filters`(column·op·value), `breakdowns`(group), `granularity`(초)를 정의. group 결과 수 상한 | 채택: aggregation·filter·group_by·step_seconds |
| §1 step 경계 정렬 | Grafana Prometheus data source ([Query editor](https://grafana.com/docs/grafana/v13.0/datasources/prometheus/query-editor/)) | 질의 범위를 step에 맞춰 정렬(Unix 시각이 step으로 나누어떨어지게)해 일관된 시각화와 결과 cache를 지원 | 채택 |
| §1 rate·분위수 의미 | Prometheus `rate()`·`histogram_quantile()` (ADR 0025 §6) | 초당 증가율. 분위수는 bucket을 합친 뒤 계산 | 채택 |
| §3 결측 null + 사유 | D02 §13 (명세) | completeness·missing 구간으로 누락 표현 | 채택. Prometheus처럼 결측 sample을 생략하지 않는다(사유를 잃는다) |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| query 형식 | JSON QuerySpec | PromQL 부분 구현: D02 §13이 별도 언어를 금지했다. 호환되지 않는 부분을 사용자가 예측하기 어렵다 |
| 집계 위치 | ClickHouse 안 (group, step) | Go에서 window 행을 합침: 결과 행 예산 10,000을 바로 넘는다(7일 × 1분 = stream당 10,080행) |
| label 원천 | metric_1m 컬럼 | 원본 join: 원본 15일 이후 rollup을 그룹화할 수 없다. 별도 stream catalog: 테이블과 동기화가 하나 더 필요하다(cardinality quota와 함께 검토) |
| 결측 | null + 사유 + completeness | 생략(Prometheus): 왜 없는지 모른다. 0: 계약 6 위반 |
| 미지원 filter | 422 | 무시: 사용자가 의도와 다른 결과를 본다 |

## 결과

- 수집한 metric을 dashboard·monitor가 조회할 수 있다: OTLP → ingress → Kafka → worker → metric_points → rollup → metric_1m → `POST /api/v1/query/metrics`.
- **schema 변경:** migration 00004(컬럼 추가).
  - migration 이전에 쓴 `metric_1m` 행은 label이 빈 값으로 남는다. rollup은 최근 10분만 다시 계산하므로 오래된 행은 채워지지 않는다.
  - `metric_1m`은 바로 직전 PR(#17)에서 생겨 운영 데이터가 없다. 그래서 backfill 없이 진행한다. 운영 데이터가 생긴 뒤의 schema 변경은 backfill job과 함께 한다.

## Rollback

- query-api 배포를 되돌린다.
- migration 00004 down은 label 컬럼과 권한을 지운다.

## 재검토 조건

- 7일 조회가 느릴 때. 이때는 `metric_1h`와 해상도 자동 선택을 도입한다.
- group 수가 결과 예산을 자주 넘을 때. 이때는 상위 N과 나머지 묶음을 도입한다.
- or·in 필터 요구가 생길 때. 이때는 query planner(E03)의 filter AST로 옮긴다.

## 증거

- spec-reviewer 지적 반영
  - window가 빠진 step을 정상값으로 내던 문제
  - watermark 이후 step을 no_data로 표시하던 문제
  - 7일 경계 정렬
  - 유형·단위 충돌을 point 단위로 표시
  - D02 §13 `expression` 이름
  - label 컬럼 기존 행 설명
- `internal/query` 테스트
  - step 경계 정렬, 빈 step은 null+`no_data`, 연속 결측 구간 병합, completeness 0.4
  - rate = 증가량 / 60s, meta `resolution_seconds`와 `sampled = null`
  - `missing_baseline`은 0이 아니다.
  - 병합 p95 ≤ 10ms(instance 평균이면 ~500). 경계가 섞이면 `bounds_mismatch`.
  - gauge의 avg·min·max, gauge에 rate는 `not_applicable`, 단위 혼합은 `unit_conflict`, NaN은 값이 아니다.
  - watermark 이후 step은 `pending`·partial이고 `meta.watermark`를 채운다.
  - window가 빠진 step은 partial, 유형이 섞이면 `type_conflict`, step마다 단위가 다르면 series 전체가 `unit_conflict`
  - 8일 원 범위는 422, 최상위 `metric` 필드는 400
  - 오류: 모르는 연산·필드(PromQL 문자열 포함)·범위 없음·비문자열 eq·깊은 필터는 400, or 필터는 422, 권한 없음은 403
- `internal/telemetrystore` 테스트
  - 검증 7종, 8일 범위와 2,001 step은 예산 초과
  - 통합(ClickHouse)
    - 큰 revision만 사용, label group_by·filter(point 속성 → resource 속성)
    - 다른 tenant 행 제외
    - environment 제한 key는 prod만
    - SQL 조각을 담은 filter가 아무것도 일치시키지 않음
    - histogram bucket 원소별 합과 경계 혼합 감지
    - tenant별 rollup watermark

## 변경 이력

- 2026-10-07 (ADR 0042): 서비스 상세(S02) 요약 값과 endpoint 표를 위해 두 가지를 추가했다.
  - 집계 `hist_sum`
  - 범위 전체 한 점 규칙(step = 범위 길이, 분 경계 시작)
  - 기존 요청의 결과는 바뀌지 않는다. 범위 = step인 요청만 이전의 두 점 대신 한 점을 받는다.
- 2026-10-08 (ADR 0046): 응답 `data`에 `source_window_seconds`(실제로 읽은 rollup, 60·3600)와 `range`(step 경계로 맞춘 범위)를 더했다. 기존 필드의 의미는 바뀌지 않는다. metric 사전(`GET /api/v1/metrics`, `/metrics/labels`)이 연산 선택지(유형별 허용 연산)를 알려준다.
