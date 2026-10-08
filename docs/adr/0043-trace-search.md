# ADR 0043: trace 검색 API와 Trace Explorer(S04)

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-07 "S04 Trace Explorer 진행")
- Owner: Backend · FE
- 날짜: 2026-10-07 (제안·결정)
- 관련: F02·F04, E03·E04, D02 §09·§13·§15·§19·§22, D05 §03·§06, ADR 0022, 0032, 0037, 0039, 0041, 0042

## 배경

D05 §06은 S04 Trace Explorer를 다음처럼 정한다.

- 배치: 좌측 facet, 상단 query·시간, 중앙 histogram/scatter, 하단 결과 표
- 행: root service·resource·start·duration·error·span count·completeness
- page 100, 정렬 동률은 trace_id
- 오류가 없는 trace를 성공으로 단정하지 않는다. 불완전 정보를 보존한다.

D02 §13은 `POST /query/traces`를 정한다(입력 range·filter·order·limit·cursor, 결과 trace_id·root_service·duration_ms·span_count·has_error·complete). D02 §15는 cursor 정렬 `(start_time, trace_id)`를, §19는 범위 24h·limit 1,000을 정한다.

ADR 0037은 trace 검색을 "span 검색 catalog와 함께" 미뤘고, ADR 0039 §4는 trace 검색의 `service.name`도 그때로 미뤘다. 지금 저장소에는 trace 요약 테이블이 없다. `spans_local` 정렬 키는 `(tenant_id, service_id, event_time, trace_id, span_id)`다.

명세가 정하지 않았거나 서로 다른 점:

- trace의 시작·길이 정의
- root가 없는 trace를 어떻게 표시할지
- 결과 item에 시작 시각·resource·사유를 넣을지(D05 §06·D02 §22에는 있고, D02 §13 예시에는 없음)
- `service.name`이 "그 서비스 span이 있음"인지 "root 서비스"인지
- facet·scatter 데이터를 어디서 가져올지(D02에 경로 없음)

## 결정

1. **경로**
   - `POST /api/v1/query/traces`, 그리고 `POST /api/v1/query`의 `signal=traces`다.
   - 요청 형식·오류·cursor(서명, 15분, 권한 fingerprint·query hash·수신 snapshot)·누적 10,000행·tenant 실행 slot(5+20)·service.name 풀기는 log 검색(ADR 0037·0039)과 같다.
   - 범위는 생략하면 최근 15분, 최대 24h다. limit은 기본 100, 최대 1,000이다.
2. **trace의 정의 (Tempo 검색과 같다)**
   - **찾기:** 범위 안에서 span 조건에 맞는 span이 하나라도 있는 trace를 찾는다.
   - **요약:** 그 trace의 span을 범위 ±1시간에서 읽어 요약한다. 값은 `(trace_id, span_id)`마다 최초 수신 한 행이다.
     - `start_time`: 가장 이른 span 시작
     - `duration_ms`: 가장 이른 시작부터 가장 늦은 끝까지(실수 ms)
     - `span_count`, `has_error`(status=error span이 있음)
     - root: 부모가 없는 span. `root_service_id`·`root_name`(= resource)은 그 span에서, `root_service`(이름)는 catalog로 푼다.
   - **목록에 넣는 조건:** `start_time`이 범위 안인 trace만 넣는다. 그래서 한 trace는 시작이 든 범위에서 한 번만 나오고 keyset이 안정적이다.
   - 1시간보다 긴 trace나 범위 밖으로 길게 이어진 trace는 빠진 span 때문에 `missing_parent`·`missing_root`로 보인다. 숨기지 않고 불완전으로 표시한다.
3. **결과 행**
   - 필드: `trace_id, start_time, duration_ms, span_count, has_error, complete, reasons, root_service, root_service_id, root_name, last_updated_at`(projection으로 고를 수 있음).
   - D02 §13 item에 D05 §06의 시작·resource와 D02 §22의 사유·`last_updated_at`(그 trace에서 가장 늦게 수신한 span)을 더했다. 기존 필드 의미는 그대로이고 추가만 했다.
   - `complete=false`는 구조 불완전이다. `meta.partial`(실행 일부 실패)과 다르다. 사유:
     - `missing_root`
     - `missing_parent`: 부모 id 집합과 span id 집합의 hash 교집합으로 계산한다.
     - `span_limit_reached`: 요약에 쓰는 span이 10,000을 넘었다. 이때는 잘린 목록이라 부모 누락을 세지 않는다.
   - 모르는 값은 null이다: root가 없으면 root 필드 3개, catalog에 아직 없으면 `root_service`.
   - late arrival은 판정하지 않는다(ADR 0022 §4와 같다).
4. **filter는 두 단계다 (TraceQL의 span 조건·trace intrinsic 구분과 같다)**
   - **span 단계** (`queryplan.TraceSpanCatalog`): `service_id`, `service.name`(catalog로 service_id 집합, ADR 0039), `trace_id`, `name`(eq·neq·in·contains), `status`(0~2), `span_kind`(0~5), `attributes.<key>`. 의미는 "이 조건에 맞는 span이 있는 trace"다. 그래서 `service.name`은 root가 아니라 "그 서비스를 거친 trace"다.
   - **trace 요약 단계** (`queryplan.TraceSummaryCatalog`): `duration_ms`(gt·gte·lt·lte, 정수 ms), `has_error`(eq·neq, bool), `span_count`.
   - 요약 field는 최상위 `and`의 직접 조건으로만 쓴다. `or`나 하위 식 안에서 두 단계를 섞으면 400이다(span 하나인지 trace 전체인지 모호).
   - 나누기 전에 filter 전체를 두 catalog를 합친 `TraceCatalog`로 한 번 검증한다. 그래서 깊이 4·조건 20·빈 `and` 같은 한도는 단계별이 아니라 요청 전체에 걸린다.
   - 두 단계는 parameter 접두어(`s`·`t`)로 나눠 한 query에 넣는다. queryplan에 `Bool` 타입과 `ParamPrefix`를 추가했다. 사용자 값은 모두 서버 측 parameter다(ADR 0032, query_log 시험 포함).
5. **정렬**
   - `start_time desc, trace_id desc`만 받는다. 다른 order는 422다.
   - duration 정렬은 비용(전체 집계 후 정렬) 때문에 미룬다(재검토 조건).
6. **mandatory predicate**
   - 두 읽기(찾기·요약) 모두에 tenant(row policy와 이중), 시간, `expires_at`, 수신 snapshot(version), environment 범위 service_id 집합을 넣는다.
   - environment 제한 key는 catalog의 서비스 범위가 없으면 403이다. 범위 밖 span은 요약에도 들어가지 않는다. 그 자식은 missing_parent로 보인다. 범위 밖 span이 있었다는 사실은 드러내지 않는다(ADR 0022 §3과 같다).
7. **S04 화면**
   - 조건: 서비스(catalog 선택), "오류 span이 있는 trace만", 최소 duration(ms), span 이름 포함(300ms debounce·Enter).
   - URL에 `service`(UUID)·`errors`(1)·`min_ms`(정수)·`name`(200자 이하)을 둔다. 공유 링크에는 형식이 정해진 앞의 셋만 남고 자유 입력 `name`은 빠진다(ADR 0041 공유 허용 키 확장).
   - scatter: **불러온 결과**(page 누적)를 시작 × duration으로 찍는다. 오류는 모양(다이아몬드)으로도 구별한다.
     - 끌어서 구간을 고르면 조사 범위가 그 절대 구간이 되고, "범위 되돌리기"(뒤로 가기)를 제공한다.
     - 키보드 대안은 상단 시간 선택이고, 데이터 대안은 결과 표다(화면에 안내).
   - 다음 page가 실패해도 불러온 결과와 마지막 성공 시각을 유지하고, 다시 시도는 그 page를 다시 부른다(D05 §03).
   - 결과 표: 오류는 "오류 span 있음 / 없음"으로 표시한다("성공"이라 하지 않음). 불완전은 사유를 보인다. "다음 100개"는 cursor로 잇는다. trace ID는 S05로 trace 구간(시작 −1분 ~ 끝 +1분)과 함께 연결한다.
   - 보존된(샘플링된) trace만 나온다는 안내와 서비스 지표 링크를 둔다(계약 5). 24h를 넘는 범위는 조회하지 않고 이유를 보인다.
   - S02 리소스 표에 "오류 trace 보기"·"이 서비스의 trace" 링크를 단다. 서비스 → 실패 trace는 2번 선택이다(D05 §05 3클릭 기준 중 앞부분).
8. **facet 개수는 이번에 하지 않는다.** D02에 facet/top-N 경로가 없다. 그래서 조건 패널은 개수 없는 선택지로 둔다. 개수는 집계 경로를 명세에 넣는 일과 함께 한다(재검토 조건).

## 외부 사례 근거

| 사례 | 내용 | 반영 |
|---|---|---|
| Grafana Tempo search API ([grafana.com/docs/tempo/latest/api_docs](https://grafana.com/docs/tempo/latest/api_docs/)) | 검색 결과가 trace 요약(`traceID`, `rootServiceName`, `rootTraceName`, `startTimeUnixNano`, `durationMs`)이다. root가 없으면 root 필드가 비어 있다 | 결정 2·3 |
| Grafana TraceQL ([grafana.com/docs/tempo/latest/traceql](https://grafana.com/docs/tempo/latest/traceql/)) | span 조건(spanset)과 trace 수준 intrinsic(trace duration, root service)을 구분한다 | 결정 4 |
| Jaeger query API ([jaegertracing.io/docs/latest/apis](https://www.jaegertracing.io/docs/latest/apis/)) | service·operation·tags·min/max duration으로 trace를 찾는다. 조건에 맞는 span이 있는 trace를 돌려준다 | 결정 2·4·7 |
| Datadog Trace Explorer ([docs.datadoghq.com/tracing/trace_explorer](https://docs.datadoghq.com/tracing/trace_explorer/)) | 보존된(indexed) span·trace만 검색되고, 지표는 비샘플링 metric에서 본다 | 결정 7 안내 문구 |

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A. span 조건으로 찾고 범위 ±1h에서 요약 (채택) | 새 테이블 없음. 범위 경계 trace도 전체로 요약됨. 사례와 같은 의미 | 24h·대형 tenant에서 scan이 크다(재검토 조건). 1h 넘는 trace는 불완전으로 보임 |
| B. 범위 안 span만으로 요약 | 단순, scan 작음 | 경계의 trace 대부분이 불완전·잘린 duration으로 보임 |
| C. trace 요약 테이블·MV 먼저 | 검색이 빠름, duration 정렬 가능 | 늦게 온 span 재계산·dedup·삭제 설계가 필요. 지금 측정 근거 없음 |
| D. root span 조건만 | root 기준 의미가 단순 | "그 서비스를 거친 trace" 검색이 안 됨(조사 흐름의 핵심) |

## 결과

- 이점
  - S04가 실제 검색을 한다. S02 → 실패 trace가 이어진다.
  - 불완전·root 없음·오류 없음이 "정상"과 구별된다.
  - log 검색과 같은 cursor·budget·격리 규칙을 쓴다.
- 비용
  - 찾기 단계가 범위 전체를 scan한다. 요약 단계는 찾은 trace 전부를 집계한 뒤 시작 시각으로 page를 자른다. 그래서 큰 tenant의 24h·조건 없는 검색은 query 계정 예산(5초, 10GB, 2GB 메모리)에 닿을 수 있다.
    - 시간 초과는 504 `QUERY_TIMEOUT`이다.
    - scan·결과·query 메모리 한도 초과는 422 `QUERY_BUDGET_EXCEEDED`다. query 하나의 메모리 한도(ClickHouse 241 "for query")는 다시 해도 같으므로 503(재시도)이 아니다. 서버 전체 메모리는 503이다.
  - scatter는 불러온 page만 보인다. 범위 전체 분포가 아니다(화면에 "불러온 N개"로 표시).
- 영향 받는 계약
  - 새 경로 `POST /query/traces`
  - `/query`의 `signal=traces`가 422에서 200으로 바뀐다.
  - queryplan `Bool`·`ParamPrefix`·`Split`
  - catalog `ServiceNames`
  - 공유 허용 키 `service`·`errors`·`min_ms`

## Rollback

- 경로를 막으면 된다(`Config.Traces` nil → 404). S04는 오류 안내를 보인다.
- 저장 형식 변경은 없다.

## 재검토 조건

- 실측에서 trace 검색이 예산(5초·10GB)을 넘을 때: trace 요약 테이블·MV(후보 C), duration 정렬, 범위 전체 분포 histogram
- facet 개수·top-N 경로를 D02에 넣을 때(결정 8): 조건 패널의 개수, "더보기"
- late arrival·sampled parent 누락 판정이 정해질 때(ADR 005 tail decision_wait): 사유 추가
- export·취소 전파(`/query-jobs`)를 할 때: 긴 query의 cancel·export(D05 §06)
- S05 Trace 상세를 구현할 때: 링크 대상 화면

## 증거

- `internal/queryplan/traces_test.go`: 단계 나누기, 섞인 or 거절, Bool·접두어, 거절 입력
- `internal/telemetrystore/errors_test.go`: query 메모리 한도는 예산 초과, 서버 전체 메모리는 503
- `TestSearchTracesSpanLimit`(ClickHouse): span 10,001개 trace는 span_limit_reached, 부모 누락 오판 없음
- handler 시험: 단계에 걸친 조건 21개·깊이 5·빈 and는 400이고 저장소에 닿지 않는다
- `internal/telemetrystore/trace_search_integration_test.go` (실제 ClickHouse)
  - 요약 값(재전송 dedup, 오류, root, 부모 누락)
  - span·요약 filter와 and
  - keyset, 범위 시작 전 trace는 자기 범위에서 한 번만
  - tenant 분리(같은 trace_id), environment 범위(svcB span 제외)
  - 입력 거절(25h, limit, 풀지 않은 service.name, 위조 keyset)
- `internal/telemetrystore/querylog_integration_test.go`: trace 검색의 trace_id 값이 query_log에 없다(ADR 0032)
- `internal/query/trace_search_test.go`: 행(null·사유·root 이름), 저장소에 간 두 단계 조건, cursor·snapshot, 거절 시 저장소 미호출, environment 범위 전달
- `internal/controldb/services_integration_test.go`: `ServiceNames` 격리
- `tests/isolation`: B가 A의 trace_id로 검색하면 빈 결과(위조 header 포함), A의 cursor는 400, 같은 trace_id면 자기 span만 요약
- smoke: checkout 오류 trace 20개(오류 요청 수와 같음), 모두 3 span·완전·root 이름
- web
  - `filters.test.ts`: URL·AST·공유 허용 키
  - `TraceExplorer.test.tsx`: 행 표시, 요청 본문, cursor 이어 붙이기, 상세 링크, 24h 거절, 빈 결과 이유, 입력 오류
  - S02 링크 시험
- 수동: mock API로 dev server 확인(scatter, 끌어서 구간 선택·되돌리기, S02 → 오류 trace)

## 변경 이력

- 2026-10-07 (ADR 0044): S04 결과의 trace ID 링크 대상인 S05 Trace 상세를 구현했다(재검토 조건 "S05 구현" 해소).
