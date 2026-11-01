# ADR 0074: trace·log 건수·facet·히스토그램 집계

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-29 "merge하고 다음 단계 진행해줘" — [design-rollout](../plan/design-rollout.md) D3 둘째 항목)
- Owner: Backend lead (E04)
- 날짜: 2026-10-29 (제안·결정)
- 관련: F04, E04 · D02 §13(조회 API), §15(실행 예산·mandatory predicate·취소), §19(QuerySpec·meta·오류), §20(전용 경로와 generic `/query`) · D05 §03(FacetPanel, 300ms debounce), §06(S04 "좌측 facet, 중앙 histogram") · 계약 4·5·6·7 · ADR 0018(query 계정 예산), 0037(filter AST), 0039(service.name), 0043(trace 검색, 결정 8), 0045(log 검색, 후보 D)

## 배경

S04 Trace Explorer와 S08 Logs 시안은 세 가지를 보인다(D05 §06).

- 조건 패널: 값마다 건수
- 가운데: 시간 히스토그램
- 위: 총 건수

지금은 셋 다 없다.

- D02 §13·§19의 검색은 page(최대 1,000행)만 돌려준다. 총 건수도 facet 경로도 없다.
- ADR 0043 결정 8과 ADR 0045 후보 D는 facet 건수를 "집계 경로를 명세에 넣을 때"로 미뤘다.
- 화면의 scatter·히스토그램은 받은 page만 그린다. 범위 전체 분포가 아니다(ADR 0043 §6 문구).

D02는 두 가지를 정한다. 집계 경로는 둘 다 지켜야 한다.

- "별도 query 언어를 만들지 않는다"(§13)
- "필터는 AST, mandatory predicate는 AST 밖"(§15)

정할 것은 다섯 가지다.

1. 경로와 입력
2. 무엇을 세나(단위·dedup·샘플링)
3. 값을 고른 facet의 건수를 어떤 조건으로 세나
4. 히스토그램 간격과 칸의 의미
5. 실행 방식과 비용 상한

## 결정

### 1. 경로: `POST /api/v1/query/{traces,logs}/aggregate` (query-api)

- signal별 편의 경로다(D02 §13 규칙). 본문은 검색과 같은 `range`·`filter`다. 같은 catalog를 쓰고, 깊이 4·leaf 20개·`in` 100개 한도도 같다. 여기에 집계 항목을 더한다.
  - `facets`: field 최대 4개. 값마다 `top_n`(기본 10, 최대 20)
  - `histogram`: true면 시간 히스토그램
- `projection`·`order`·`limit`·`cursor`를 보내면 400이다. 범위 상한은 검색과 같은 24시간이다.
- `allow_partial`의 의미는 검색과 같다. 지금은 단일 shard라 실패하면 503이다.
- generic `POST /api/v1/query`에는 이번에 넣지 않는다. 검색처럼 signal별 편의 경로가 먼저다(D02 §20의 "같은 fixture 일치"는 generic 경로를 만들 때 시험한다).
- 응답은 공통 envelope(`data`·`meta`)이고 `next_cursor`는 없다. meta는 D02 §19의 필드를 모두 준다.
  - `resolution_seconds` = 히스토그램 `step_seconds`. 히스토그램이 없으면 null
  - `scan_bytes` = 하위 query의 합
  - `watermark` = 검색과 같다. 지금은 null(수집 watermark 미구현)
  - `coverage` = null

```json
{
  "data": {
    "total": 1834,
    "histogram": {"step_seconds": 30, "buckets": [
      {"start": "2026-10-29T00:00:00Z", "end": "2026-10-29T00:00:30Z", "count": 12, "error_count": 1}]},
    "facets": [
      {"field": "root_service_id", "values": [{"value": "…uuid…", "count": 1200}], "other_count": 600, "missing_count": 34,
       "excludes_own_filter": true}
    ]
  },
  "meta": {"request_id": "…", "schema_version": 1, "watermark": null, "partial": false, "failed_shards": [],
           "sampled": true, "coverage": null, "resolution_seconds": 30, "scan_bytes": 18000000, "warnings": []}
}
```

### 2. 무엇을 세나

- **trace:** 검색과 같은 trace 요약(ADR 0043 §2·§3)을 거친 trace 수다.
  - 세는 trace: 시작이 범위 안이고, span 조건과 요약 조건(`duration_ms`·`has_error`·`span_count`·`root_service_id`)을 모두 만족한다.
  - 같은 시각 기준이면, 검색 결과 page를 끝까지 넘긴 행 수와 같다.
- **log:** 검색과 같은 dedup(event_id마다 최초 수신 한 행) 뒤의 log 수다.
- **mandatory predicate**는 검색과 같은 자리에 넣는다(계약 4). tenant(row policy와 SQL 둘 다)·시간·`expires_at`·environment 범위 서비스다.
  - 삭제 tombstone은 검색과 같이 삭제 원장(F09) 구현 때 함께 넣는다(ADR 0018 §7). 지금은 없다.
  - environment 범위는 AST 밖 predicate라, §3의 조건 빼기로 지워지지 않는다.
- **수신 snapshot:** 요청 시각 하나다. 모든 하위 query가 같은 값을 써서 facet 합과 `total`이 같은 시점이다. 같은 순간에 보낸 검색 첫 page와는 몇 초 어긋날 수 있다.
- **풀지 못한 `service.name`:** catalog에 없는 이름은 검색과 같이 어떤 행과도 맞지 않는다. 검색과 같은 `meta.warnings`(`service_name_unresolved`)를 준다. 그래서 `total: 0`이 "그런 서비스가 없음"인지 화면이 구별할 수 있다.
- **단위:** 저장된 trace·log의 수다. 요청 수가 아니다.
  - trace는 tail sampling을 거친 표본이라 `meta.sampled=true`다. 오류율·처리량·SLO를 이 수로 계산하지 않는다(계약 5 — 그것은 비샘플링 SDK metric).
  - tail sampling은 오류 trace를 먼저 남긴다. 그래서 `error_count / count`는 오류율이 아니다. 화면은 비율·%를 보이지 않고, "오류 trace(표본)"·"저장된 trace"라고 쓴다.
  - log의 `meta.sampled`는 검색과 같이 `null`이다(정책 추적이 아직 없다).

### 3. facet 건수: 그 facet이 소유한 조건만 빼고 센다 (disjunctive)

**facet과 소유 filter field.** facet마다 그 facet을 고르는 filter field 집합을 정한다. 화면은 facet 값을 고를 때 그 field로 filter를 만든다.

| signal | facet | 값 | 소유 filter field |
|---|---|---|---|
| traces | `root_service_id` | root span 서비스(UUID). 이름은 화면이 catalog로 붙인다 | `root_service_id` (요약 단계, **새로 추가** — `eq`·`in`, UUID) |
| traces | `has_error` | `true`·`false` | `has_error` |
| logs | `service` | 서비스 UUID | `service_id`, `service.name` |
| logs | `severity` | `unspecified`·`trace`·`debug`·`info`·`warn`·`error`·`fatal` — OTel SeverityNumber 0, 1–4, 5–8, 9–12, 13–16, 17–20, 21–24 | `severity_number` |
| logs | `attributes.<key>` | 속성 값(문자열). 요청당 2개까지. key 검증은 filter의 `attributes.<key>`와 같다 | `attributes.<key>` (같은 key) |

- trace의 서비스 조건은 두 가지이고, 뜻이 다르다.
  - span 단계 `service.name`·`service_id`: "그 서비스 span이 하나라도 있는 trace"
  - 요약 단계 `root_service_id`: "root가 그 서비스인 trace"
  - root 서비스 facet은 두 번째만 소유한다. 고른 값의 건수와 결과 수가 같게 하기 위해서다.
- 서비스 이름 facet은 두지 않는다. 행에는 service_id만 있다(ADR 0039).
- trace의 "span이 지나간 서비스" facet도 두지 않는다. root와 다른 의미라, 따로 정할 때까지 미룬다.
- `service.version`·`k8s.pod.name` facet은 resource 속성 저장(D3 셋째 항목)과 함께 한다.

**빼는 규칙.** facet F의 건수는 F의 소유 조건을 뺀 filter로 센다.

- filter 최상위가 `and`이면: 직계 child 중 **모든 leaf가 F의 소유 field인 subtree**를 뺀다. 연산자는 가리지 않는다(`eq`·`in`·범위·`neq`·`exists`, 그리고 그런 leaf만 가진 `or`).
  - 예: severity "error 또는 fatal" = `severity_number gte 17`. 이것도 뺀다.
- filter 최상위가 소유 subtree 하나면(최상위 `and` 없이 leaf 하나, 또는 소유 leaf만 가진 `or`): 통째로 빼서 조건 없음이 된다.
- 다른 field와 섞인 subtree는 그대로 둔다. 예: `or(service.name=a, severity_number>=17)`.
- 다 빼서 빈 `and`가 되면 조건 없음("1")이다.
- 뺀 것이 있으면 그 facet에 `excludes_own_filter: true`다. 그 facet의 합은 `total`과 다르다 — 화면이 이를 알린다.
- 그래서 서비스 A를 고른 뒤에도 B·C의 건수가 보이고, 여러 개를 고를 수 있다(checkbox).

**값과 합.** 값마다 정확한 수다. 근사하지 않는다.

- `values`는 수 내림차순이고, 수가 같으면 값 오름차순이다.
- `other_count`는 top_n 밖 값의 합이다.
- `missing_count`는 그 facet에 값이 없는 행이다(속성이 없음, root span이 없는 trace).
- 따라서 `Σvalues + other_count + missing_count` = 그 facet의 filter로 센 전체다.
- 허용 밖 facet은 400이다. 같은 facet을 두 번 요청해도 400이다.

### 4. 히스토그램

- **간격:** 서버가 고른다. 후보는 10초·30초·1분·5분·10분·30분·1시간이다. 범위 길이를 60으로 나눈 값 이상인 것 중 가장 짧은 것을 쓴다. 15분이면 30초, 24시간이면 30분이다.
- **칸 경계:** 칸의 시작은 간격의 배수(UTC)다.
  - 범위가 배수에 맞지 않으면 칸이 최대 61개다.
  - 첫 칸과 끝 칸은 범위로 잘라 `start`·`end`를 준다. 일부만 덮는 칸을 화면이 추론하지 않게 한다.
- **조건:** 히스토그램은 빼지 않은 전체 filter로 센다. 칸 합 = `total`이다.
- **나눔:**
  - trace: `count`·`error_count`. trace는 시작 시각이 든 칸에 들어간다.
  - log: `count`와 심각도 묶음별 수(`by_severity`, §3의 7개)
- **아직 오지 않은 칸:**
  - `start` ≥ 수신 snapshot(요청 시각)인 칸은 `count: null`, `reason: "future"`다.
  - snapshot 직전 60초에 걸친 칸은 수를 주되 `incomplete: true`다. 수집 지연으로 더 들어올 수 있다.
  - 60초는 시작값이다. 수집 watermark(D02 §19)를 구현하면 그 값으로 바꾼다.
- **빈 칸:** 저장된 행이 없는 지난 칸은 `count: 0`이다.
  - 수집 공백인지는 이 경로로 알 수 없다(D3 "수집 공백 구간").
  - query-api는 아직 tenant 보존 정책을 모른다. 그래서 보존이 끝난 칸도 0이다(아래 "알려진 한계").
  - 화면은 0을 "정상·0건"이 아니라 "저장된 행 없음"으로 쓴다.

### 5. 실행: 요약·dedup 한 번 + 조건부 집계, 비용 상한

**한 번 읽고 여러 facet을 센다.** 무거운 단계는 범위 전체를 한 번만 거친다.

- 무거운 단계: trace의 `matched`→`spans`→요약, log의 `LIMIT 1 BY event_id` dedup
- facet마다 §3의 "뺀 filter"를 조건 식으로 컴파일한다. 사용자 값은 parameter다(ADR 0037).
- 한 query에서 `GROUPING SETS`와 `countIf(<facet별 조건>)`으로 총 건수·히스토그램·facet을 함께 센다.
- top_n·`other_count`·`missing_count`도 같은 query 안에서 window 함수로 나눈다.

span 단계 조건은 facet마다 다를 수 없다. trace facet은 모두 요약 단계 field만 소유하므로, span 단계 `matched`는 하나를 공유한다.

**속성 facet만 따로 센다.**

- `attributes.<key>` facet은 값 수가 크다. 그래서 같은 snapshot으로 따로 센다(요청당 최대 2개).
- 서로 다른 값이 10만 개를 넘는 facet은 `values: null`, `reason: "too_many_values"`다.
  - 설정: `max_rows_to_group_by=100000`, `group_by_overflow_mode='throw'`
  - `any`·`break`는 조용히 틀린 수를 내므로 쓰지 않는다. 시험으로 설정을 고정한다.
  - ClickHouse 오류 158(TOO_MANY_ROWS)만 이 사유로 분류한다.
- 속성 facet query가 예산·시간 초과로 실패하면 그 facet만 `values: null`, `reason: "budget_exceeded"|"timeout"`이다. 다른 결과는 그대로 준다.
- 본 query(총 건수·히스토그램·나머지 facet)가 실패하면 요청 전체가 실패한다.

**예산.** 한 요청 = DB query 최대 3개(본 1 + 속성 2)다.

- 순서대로 실행하고 tenant gate 자리 하나를 쓴다(D02 §15 동시 5개를 넘지 않는다).
- 요청 전체 시간 상한은 검색과 같은 10초(`QueryTimeout`)다. 각 query의 `max_execution_time`은 남은 시간으로 준다.
- 행·byte 예산(`max_rows_to_read`·`max_bytes_to_read`)은 query 계정 profile 값(ADR 0018)이다. 속성 facet은 그 값의 절반으로 준다. 그래서 한 요청이 읽는 양은 검색 한 번의 2배를 넘지 않는다.
- 넘으면 `QUERY_BUDGET_EXCEEDED`(422)나 `QUERY_TIMEOUT`(504)이다. 화면은 "범위를 줄이세요"로 안내하고, 건수를 0으로 보이지 않는다.
- 요청 취소는 모든 하위 DB query에 전파한다(D02 §15).

**수치.** facet 4개·top_n 20·60칸·10초·10만 값·속성 2개·60초는 시작값이다. 근거는 다음과 같고, 실측(`docs/experiments`) 뒤 재검토한다.

- Kibana top 10, Loki `limit` 100
- 화면 조건 패널 폭
- 검색 `QueryTimeout`과 같은 10초
- query 계정 `max_result_rows` 기본 1만의 10배

**운영 장치.**

- flag `MONTRACER_QUERY_AGGREGATE_ENABLED`(기본 true). 끄면 route가 404이고, 화면은 지금처럼 건수 없이 보인다.
- 지표:
  - `montracer_query_aggregate_requests_total{signal,outcome}`
  - `montracer_query_aggregate_duration_seconds{signal}`
  - `montracer_query_aggregate_scan_bytes_total{signal}`
  - `montracer_query_aggregate_facet_unavailable_total{signal,reason}`
- 경보는 두지 않는다(runbook 없음).

### 6. 화면 (다음 PR)

- S04: 조건 패널에 root 서비스·오류 건수, 총 "저장된 trace n개", 범위 전체 히스토그램(scatter는 page 그대로). 비율·%는 보이지 않는다.
- S08: 서비스·심각도 건수, 총 건수, 심각도 히스토그램
- 건수 요청은 page 요청과 따로 보낸다. 실패해도 목록은 보인다. 건수 자리만 "건수를 볼 수 없음"이다.
- `null` 칸·`incomplete` 칸·`too_many_values`는 0으로 그리지 않는다.
- 선택 변경은 300ms debounce, 이전 요청은 취소한다(D05 §03).

## 알려진 한계

- **보존 경계:** query-api는 tenant 보존 정책을 모른다. 그래서 보존이 끝난 칸도 0이다. 보존 정책 조회(D04, F09)를 만들 때 그 경계 이전 칸을 `count: null`, `reason: "expired"`로 바꾼다.
- **수집 지연:** watermark가 없어 지연은 60초 시작값으로만 표시한다.

## 명세 반영 (계약 7)

D02 §13 경로 표에 집계 경로가 없다. ADR 0043 결정 8도 "집계 경로를 명세에 넣는 일과 함께"라고 했다. 그래서 D02 원본(docx) §13·§19 개정 후보로 둔다.

- §13 경로 표에 `/query/{traces,logs}/aggregate`
- §19에 집계 응답과 facet·히스토그램 의미

원본 개정은 문서 책임자의 일이다. 그때까지 이 ADR과 OpenAPI가 기준이다.

## 외부 사례 근거 (2026-10-29 확인)

| 사례 | 확인한 동작 | 이 결정에 쓴 부분 |
|---|---|---|
| Datadog Log Explorer facets ([docs](https://docs.datadoghq.com/logs/explorer/facets/)) | facet은 값의 top list와 값마다 맞는 log 수를 보인다. 요약은 "현재 query의 범위"다 | §3 값마다 정확한 수, 현재 범위·filter 기준 |
| Algolia faceting ([docs](https://www.algolia.com/doc/guides/managing-results/refine-results/faceting/)) | 여러 값을 고르는 refinement list는 disjunctive다. InstantSearch는 결과 요청과 facet 요청을 따로 보낸다 | §3 소유 조건만 빼고 셈, §6 page와 건수 요청 분리 |
| Kibana Discover ([docs](https://www.elastic.co/docs/explore-analyze/discover/discover-get-started)) | field의 top 10 값과 계산에 쓴 record 수를 함께 보인다 | §3 top_n 기본 10, 무엇의 합인지 함께 준다(`other_count`·`missing_count`) |
| Grafana Loki volume API ([docs](https://grafana.com/docs/loki/latest/reference/loki-http-api/)) | `index/volume`·`volume_range`는 같은 matcher로 label별 양을 따로 준다. `limit` 기본 100, `step`으로 구간 | §1 검색과 같은 filter의 별도 집계 경로, §4 서버가 간격을 정함 |
| Grafana Tempo search API ([docs](https://grafana.com/docs/tempo/latest/api_docs/)) | tag 값 목록에 건수가 없고, 검색은 첫 N개만 준다(총 수 없음) | 버린 후보 C(값 목록만) — 시안의 건수를 풀지 못한다 |
| Datadog Trace Explorer ([docs](https://docs.datadoghq.com/tracing/trace_explorer/)) | 15분 넘는 범위의 시계열은 retention filter로 보존한 span만 센다 | §2 표본의 수임을 밝히고 요청 수·비율로 쓰지 않음 |

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A (채택) signal별 `/aggregate`, 요약·dedup 한 번 + `GROUPING SETS`·조건부 집계로 disjunctive, 속성 facet만 따로 | 검색과 같은 filter·predicate, 다중 선택, 정확한 수, 무거운 단계 1회(속성 facet 있을 때 최대 3회) | SQL이 복잡하다(조건 식 생성기·시험이 필요) |
| B 검색 응답에 총 건수·facet을 함께 | 요청 하나 | page마다 전체를 다시 셈(cursor 다음 page에도), 검색 지연이 집계 비용에 묶임 |
| C 값 목록만(건수 없음, Tempo 방식) | 싸다 | 시안의 건수·히스토그램을 풀지 못한다 |
| D 근사 수(`uniq`·`topK`·표본) | 빠르다 | 근사임을 화면이 계속 알려야 하고, 작은 값이 0이나 빠짐으로 보일 수 있다(계약 6). 실측 근거 없음 |
| E facet마다 별도 query(disjunctive) | 단순한 SQL | dedup·요약(범위 전체 정렬·집계)을 facet 수만큼 반복한다. gate·예산을 그만큼 더 쓴다 |
| F 현재 filter 그대로(conjunctive) | 조건 식 하나 | 값을 하나 고르면 다른 값이 모두 0 — 다중 선택이 안 된다 |

## 결과

- 이점: S04·S08의 facet 건수, 총 건수, 범위 전체 히스토그램이 풀린다(design-rollout D3 둘째 항목). 서비스 여러 개 조건도 된다.
  - log: `service_id in`
  - trace root 서비스: 새 `root_service_id in`
  - trace "지나간 서비스": 기존 `service.name in`
- 비용: 요청마다 범위 전체를 읽는다. trace는 요약 단계까지 간다. page 검색보다 비싸고, 읽는 양은 검색의 최대 2배다.
- 영향 받는 계약:
  - OpenAPI: `POST /api/v1/query/traces/aggregate`, `POST /api/v1/query/logs/aggregate`, `AggregateRequest`·`AggregateResult` schema
  - trace filter catalog: `root_service_id`(요약 단계) 추가 — 검색에서도 쓸 수 있다
  - migration 없음
- 필요한 시험:
  - **집계 의미:** dedup된 수, 요약 조건(`duration_ms`·`root_service_id`)이 수에 들어감, 검색 page를 모두 넘긴 수와 같음(통합, 같은 fixture)
  - **조건 빼기(단위):** 최상위 `and`의 소유 leaf·소유 `or`·범위 조건 제거, 섞인 `or`는 유지, 최상위 leaf 하나, 다 빼서 조건 없음, 서비스 facet의 `service_id`·`service.name` 둘 다, environment predicate는 남음
  - **facet:** top_n·`other_count`·`missing_count` 합, 같은 수일 때 순서, 허용 밖·중복 facet 400, 속성 facet 2개 상한, 속성 key 검증이 filter와 같음
  - **히스토그램:** 간격 고르기(15분·1시간·24시간), 배수가 아닌 범위의 61칸과 잘린 경계 칸, 빈 칸 0, future·incomplete 칸, 심각도 묶음 경계(0, 4/5, 12/13, 20/21, 24), 칸 합 = `total`
  - **비용:** `group_by_overflow_mode='throw'` 설정 고정, 값 10만 초과 → 그 facet만 `too_many_values`, 속성 facet 예산·시간 초과 → 그 facet만, 본 query 초과 422·504, gate 자리 하나, 취소 전파, 남은 시간 배분
  - **권한·격리:** environment 제한 key는 서비스 범위로만, 모든 하위 query에 tenant·환경 predicate(SQL 시험), 다른 tenant 행이 섞이지 않음(`tests/isolation`), tenant header 무시
  - **service.name을 못 풂:** warning이 붙고, 0은 그 이름의 결과
  - **flag off:** 404, 화면은 건수 없이
  - **OpenAPI 계약 시험:** 성공 응답·오류 status

## 구현 순서

1. 이 ADR(이 PR). ADR 0043 결정 8·ADR 0045 후보 D의 변경 이력에 "ADR 0074로 결정"을 적는다.
2. 백엔드:
   - `internal/queryplan`: 조건 빼기, `root_service_id`
   - `internal/telemetrystore`: 집계 query
   - `internal/query`: route·flag·지표
   - OpenAPI
   - 위 시험(단위·통합·격리)
   - 문서: 아키텍처 문서, `cmd/query-api/README.md`, design-rollout D3 행
3. 화면:
   - S04·S08 건수·히스토그램(ADR 0043·0045 변경 이력)
   - 서비스 여러 개 URL 조건
   - 상태 계약 시험(0·비율을 그리지 않음)
   - 작업계획·기능 상태 등록부

## Rollback

`MONTRACER_QUERY_AGGREGATE_ENABLED=false`면 route가 404이고, 화면은 건수 없는 지금 모습으로 돌아간다(flag off 경로를 시험한다). 저장소 변경이 없으므로 코드 revert만으로 되돌린다. `root_service_id` filter field는 검색에서도 쓰이므로, 되돌릴 때 함께 뺀다.

## 재검토 조건

- 실측에서 24시간 trace 집계가 예산을 자주 넘으면: trace 요약 테이블·MV(ADR 0043 후보 C), 또는 근사 수(후보 D)를 "근사" 표시와 함께
- 보존 정책 조회가 생기면: 보존 경계 이전 칸을 `expired`로
- 수집 watermark가 생기면: `incomplete`를 watermark 기준으로
- resource 속성을 저장하면: `service.version`·`k8s.pod.name` facet 추가
- "span이 지나간 서비스" facet이 필요해지면: 의미(span 수인지 trace 수인지)를 따로 정한다
- generic `/query`에 집계를 넣을 때: D02 §20의 같은 fixture 일치 시험

## 증거

- 백엔드 시험:
  - 단위(`internal/queryplan`): 조건 빼기(최상위 and의 소유 leaf·소유 or·범위 조건, 섞인 or 유지, 최상위 leaf 하나, 다 빼면 조건 없음, 속성 key별), `root_service_id` 요약 단계 field
  - 단위(`internal/telemetrystore`): 간격 고르기, 61칸·잘린 경계 칸·빈 칸 0·future·incomplete·심각도 묶음, top_n·other·missing, 설정(속성 facet만 group 상한 throw·읽기 절반, 남은 시간), 실패 분류(group 상한만 too_many_values, 같은 158의 읽기 상한은 budget_exceeded, 연결·취소는 요청 실패), facet 한도
  - handler(`internal/query`): 성공 응답(OpenAPI 계약, future 칸·셀 수 없는 facet의 null), 소유 조건을 뺀 facet filter, trace의 span 단계 공유와 `meta.sampled=true`, 풀지 못한 service.name 경고, 거절(projection·cursor·모르는·다른 signal facet·속성 key·중복·top_n·5개·속성 3개·signal·filter), flag off 404, environment 제한 key
  - ClickHouse 통합(`internal/telemetrystore`): dedup·만료·다른 tenant 제외, facet별 조건 빼기, 속성 missing, 히스토그램 합 = total, 검색 결과 수와 같음, group 상한 초과 facet만 too_many_values, trace root 없음 missing·요약 조건·span 조건·environment 범위
  - 격리(`tests/isolation`): B가 A의 trace_id로 세도(위조 tenant header 포함) 0, A의 서비스가 facet에 없음
  - handler: 본 query 예산·시간 초과 → 422·504(건수 없음). gate 자리 하나·취소 전파는 검색과 같은 `gate`·요청 `context`를 쓴다(검색 시험이 본다).
- 백엔드 spec-reviewer: P0 없음. P1 2건 반영(PS-0018). P2 반영 — root 없는 trace의 `root_service_id`, `sumMapIf` 이탈 기록, 절반 예산 범위, parameter 접두어 구분자(`fa<i>_`), 속성 facet의 environment 범위·심각도 경계·trace 검색 수 = total 시험.
- 설계 검토(spec-reviewer, 2026-10-29): P0 없음.
  - P1 5건 반영:
    - facet과 filter field 불일치 → 소유 field 집합, 요약 단계 `root_service_id`, subtree 제거 규칙
    - gate·예산 우회 → 순차·gate 자리 하나·남은 시간·속성 facet 절반 예산
    - overflow 판정 → `throw`·오류 158만
    - 0이 아닌 칸 → future·incomplete, 보존 경계는 한계로 명시
    - 후보 E 근거 → 조건부 집계로 무거운 단계 1회 채택
  - P2 반영: D02 개정 후보, meta 필드, 히스토그램 경계·전체 filter, 오류 비율 금지, tombstone, unresolved, 지표·시험, 수치 근거, ADR 0043·0045 변경 이력.

## 변경 이력

- 2026-10-29 제안·결정. 설계 검토 반영(위 증거).
- 2026-11-01 (구현, 백엔드 PR)
  - §5 group 상한(`max_rows_to_group_by`·`throw`)은 속성 facet query에만 건다. 본 query의 trace 요약 `GROUP BY trace_id`는 trace 수만큼 group을 만들어, 상한을 걸면 10만 trace 넘는 범위가 통째로 실패한다. 고정 facet은 `sumMapIf`라 GROUP BY가 아니다.
  - §5 "오류 158만 too_many_values": 읽기 상한(`max_rows_to_read`)도 같은 158이다. 오류 문구의 `max_rows_to_group_by`로 가른다.
  - §5 각 query의 실행 상한은 interactive 기본 5초와 요청에 남은 시간 중 작은 값이다.
  - §1 `meta.scan_bytes`는 검색과 같이 null이다(실행 통계 미수집). 수집하면 하위 query 합으로 채운다.
  - §5 지표: 요청 수·지연은 기존 route 지표(`montracer_query_requests_total`·`montracer_query_request_duration_seconds`, route별)로 본다. 따로 두는 것은 `montracer_query_aggregate_facet_unavailable_total{signal,reason}` 하나다. scan bytes 지표는 실행 통계를 수집할 때 둔다.
  - §4 snapshot 이후 칸(future)은 null이다. 그 칸에 든 행(시계 차이로 미래 시각인 행)은 `total`에는 들어가므로, null 칸이 있으면 칸 합이 `total`보다 작을 수 있다.
  - §3 trace `root_service_id` 조건의 column은 `if(roots > 0, root_service_id, NULL)`이다. root 없는 trace의 요약 기본값(모두 0인 UUID)이 그 값의 조건과 맞지 않게 한다.
  - §5 고정 facet(서비스·심각도·root 서비스·오류)은 `GROUPING SETS`·window 대신 `sumMapIf`(값→수 map) 하나씩으로 같은 행에서 함께 세고, top_n·other·missing은 Go에서 나눈다. 값 수가 작고(서비스 catalog 상한·7개·2개) query가 한 행이라 단순하다. window(`sum() OVER ()`)는 속성 facet query에만 쓴다.
  - §5 "절반 예산"은 읽기 byte(`max_bytes_to_read` 5GB)만이다 — query 계정 profile에 행 읽기 상한이 없다. profile 값을 바꾸면 함께 바꾼다.
  - 백엔드 리뷰 P1 2건(심각도 히스토그램 key 타입, group 상한 오류 문구)은 [PS-0018](../troubleshooting/PS-0018-clickhouse-type-and-error-text-assumptions.md).
