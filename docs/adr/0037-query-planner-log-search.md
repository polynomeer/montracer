# ADR 0037: query planner와 log 검색 (`POST /api/v1/query`)

- 상태: 승인 (결정 위임)
- Owner: Backend lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021~0036과 같은 위임). 근거는 §6
- 날짜: 2026-10-06 (제안·결정)
- 관련: D02 §09(log 저장), §12(공통 API), §13(`/query/logs`), §15(planner·실행 예산), §19(QuerySpec·오류) · ADR 0018(row policy), 0021(version), 0032(서버 측 parameter), 0034(서명 cursor)

## 배경

- **log 조회 경로가 없었다.** 저장된 log를 고객이 볼 수 없다. log↔trace 연결(D04 §10 probe, `make smoke`)도 수집까지만 확인했다.
- **planner가 없었다.** D02 §15의 AST·field catalog·mandatory predicate·실행 예산은 metric 조회의 작은 filter(and/eq)에만 일부 있었다.

## 결정

### 1. `internal/queryplan`: AST → SQL 조각

- **연산자:** `and`, `or`(깊이 4), `eq`, `neq`, `in`, `exists`, `gt`/`gte`/`lt`/`lte`, `contains`
- **한도:** 조건 20개, `in` 값 100개, 문자열 1,024자(D02 §15)
- **field catalog:** signal별 고정 목록이다. field마다 column 표현·값 타입·허용 연산자를 정한다. 없는 field나 허용 밖 연산자는 field 경로(`filter.args[1].op`)가 있는 400이다.
- **parameter:** 사용자 값은 모두 서버 측 parameter `{f<n>:Type}`이다(ADR 0032). SQL에 이어 붙이는 것은 catalog의 고정 column 표현뿐이다(계약 4, 인젝션 시험). map key(`attributes.<key>`)도 parameter이고 길이·문자를 검사한다.
- **mandatory predicate는 여기 없다.** tenant·시간·권한·expires_at은 저장소가 AST 밖에서 넣는다. `Compiled` 조각만으로는 query가 되지 않는다.
- **정규형(Canonical):** filter의 정규형을 cursor query hash에 쓴다. 공백과 key 순서는 결과를 바꾸지 않는다.

### 2. log catalog (logs_local, D02 §09)

| field | 타입 | 연산자 |
|---|---|---|
| `service_id` | UUID | eq, neq, in |
| `severity_number` | 0~24 정수 | eq, neq, in, gt, gte, lt, lte |
| `trace_id`, `span_id` | hex (모두 0은 거절) | eq, neq, in |
| `body` | 본문 | contains(대소문자 무시 부분 문자열) |
| `attributes.<key>` | record 속성 문자열 | eq, neq, in, contains, exists |

- regex·전문 분석은 없다(D02 §15 MVP).
- **없는 값은 비교 대상이 아니다(계약 6, 리뷰에서 발견).**
  - 없는 `attributes.<key>`는 `eq ""`에도 `neq`에도 맞지 않는다. map 비교에 `mapContains`를 함께 건다.
  - 0 byte(없는) `trace_id`·`span_id`는 `neq`에 맞지 않는다(D02 §09 "없는 값은 trace 검색에서 제외").

### 3. `telemetrystore.SearchLogs`

- **mandatory predicate:** `tenant_id`, `event_time ∈ [from, to)`(나노초, DateTime64 parameter), `expires_at > now`, 수신 snapshot을 항상 넣는다. row policy(ADR 0018)가 tenant를 한 번 더 막는다.
- **bounded dedup(최초 수신):**
  - 안쪽 subquery는 mandatory predicate만으로 고른다. 그 뒤 `ORDER BY event_id, version DESC LIMIT 1 BY event_id`로 event_id마다 최초 수신(version 최대, ADR 0021 §4) 한 행만 남긴다. FINAL 전체 scan을 하지 않는다.
  - 바깥에서 dedup된 행에 사용자 filter·keyset·정렬을 적용한다.
  - 처음 구현은 event_time 정렬 안에서 dedup했다. 그래서 같은 event_id라도 시각이 다른 재전송이면 나중에 받은 행이 이기거나 두 page에 나왔다(리뷰에서 발견).
  - 대가로 dedup이 시간 범위 안 tenant의 모든 행을 정렬한다. 24시간 범위의 큰 tenant에서 비용은 부하 시험에서 본다(재검토 조건).
- **keyset:** `(event_time, event_id) < 위치`, limit+1행으로 다음 page 유무를 안다.
- **수신 snapshot(D02 §15 snapshot_ingest_time):** logs_local에는 수신 시각 column이 없다. 하지만 version이 수신 ms를 담으므로(`MaxUint64 − (ms<<20 | offset)`) `version ≥ 하한`으로 "첫 page 시각까지 수신한 행"만 본다. 다음 page는 첫 page의 snapshot을 그대로 쓴다.
- **범위·크기:** 범위는 24시간 이하(422 `QUERY_BUDGET_EXCEEDED`, `max_range_seconds`), limit은 1~1,000(D02 §19)이다.
- **environment로 제한된 key는 403이다.** logs_local에는 environment가 없어(서비스 catalog 전, §5) 범위 안임을 증명할 수 없다. 보여주지 않는다(trace는 span resource로 거른다).

### 4. API

- **경로:** `POST /api/v1/query`(`signal` 필수)와 `POST /api/v1/query/logs`(경로가 signal을 정한다). 같은 schema·오류·예산이다(D02 §13).
  - `signal=metrics`는 422로 `/query/metrics`를 안내한다.
  - `traces`·`errors` 검색은 422(아직 없음)다.
- **요청:** `range`(생략하면 최근 15분), `filter`, `projection`, `order`, `limit`(기본 100), `cursor`, `allow_partial`
  - range를 생략하면 다음 page도 **첫 page snapshot 기준** 15분이다. query hash에는 구체 시각 대신 "default"를 넣는다. 요청마다 now로 다시 계산하면 cursor가 깨졌다(리뷰에서 발견).
  - `projection`은 결과 field 부분집합이다.
  - `order`는 `time desc`만 받는다.
  - 모르는 key는 400이다(DisallowUnknownFields).
- **응답:** `{data, next_cursor, meta}`(D02 §19)
  - 없는 trace·span ID는 null이다(0으로 채우지 않는다).
  - `meta.partial=false`, `failed_shards=[]`이다. `watermark`·`sampled`·`coverage`·`scan_bytes`는 아직 계산하지 않아 null이다(계약 6).
  - `Cache-Control: no-store`
- **cursor:** apicursor(ADR 0034), 만료 15분(D02 §19)
  - 묶는 값: tenant, 권한 fingerprint(key 종류·주체·action·environment 범위), query hash(signal·범위·정규 filter·projection)
  - page 크기는 hash에 넣지 않는다.
  - **interactive 누적 10,000행**(D02 §13): cursor가 지금까지 돌려준 행 수를 담는다. 10,000행에서 멈추고 `meta.warnings`에 `interactive_row_limit_reached`를 붙인다. 그 이상은 export job(후속)이다.
  - query-api의 `MONTRACER_CURSOR_KEY_HEX`가 없으면 **검색 경로만 404**다. trace·metric 조회는 그대로다(설정 rollback). 잘못된 key는 기동 오류다.
- **tenant별 동시 실행(D02 §15):** 실행 5개·대기 20개이고, 넘으면 429 `RATE_LIMITED` + Retry-After 1초다.
  - log 검색뿐 아니라 trace·metric 조회에도 적용한다(gateway 상한).
  - 본문이 있는 경로는 **입력 검증을 마친 뒤** slot을 잡는다. 느린 본문이나 잘못된 요청이 slot을 붙잡지 않는다.
  - 대기는 조회 시간 상한(10초) 안에서만 한다.
  - replica마다 따로 센다. Cell 전체 100, 대기 순서(FIFO 아님), 동시 실행·대기 지표는 후속이다. 대응은 RB02 "조회 429".

### 5. 하지 않는 것

- **서비스 catalog(D02 §08):** `service.name`으로 거르거나 environment key로 log를 검색하려면 service_id → 이름·environment가 필요하다.
- **trace·error 검색(`signal=traces|errors`)·`/query/traces`:** span 검색 catalog(duration·status·name)와 함께 만든다.
- **실행 예산 연결:** scan 추정·비동기 job 전환(`/query-jobs`), partial·`allow_partial` 의미(shard 실패), Cell 전체 동시성, query 취소 전파(ClickHouse query ID)
- **cache(D02 §15):** 결과 cache는 없다.
- **삭제 tombstone predicate:** 삭제 원장(F09)과 함께 넣는다(ADR 0018 §7).
- **OpenAPI 원천·contract test.**

### 6. 외부 사례 근거 (2026-10-06 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| 기본 범위 15분, page 1,000, 이전 응답의 cursor로 다음 page | Datadog Logs Search API ([logs API](https://docs.datadoghq.com/api/latest/logs/), [pagination guide](https://docs.datadoghq.com/logs/guide/collect-multiple-logs-with-pagination/)) | `POST /api/v2/logs/events/search`에서 `filter.from/to`의 기본은 now-15m~now, `page.limit` 최대 1,000, 다음 page는 응답의 `meta.page.after`를 `page.cursor`로 보낸다 | 채택: 기본 15분, limit 최대 1,000, `next_cursor`(D02 §19와 같다). 다르게: query 문자열 대신 구조화 AST(D02 §15 "별도 query 언어를 만들지 않는다") |
| 불투명 cursor, 다른 인자 재사용 거절, page 크기 변경 허용 | Google AIP-158 (ADR 0034 §6) | — | 채택(ADR 0034와 같다) |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| filter 표현 | JSON AST + catalog | 문자열 query 언어(Datadog·Loki 식): parser·escaping이 새 공격면이고 D02 §15가 AST를 정했다 |
| dedup | `LIMIT 1 BY event_id` | FINAL: 전체 scan(D02 E03 "FINAL 전체 scan 금지"). dedup 없음: 재전송 log가 두 번 보인다 |
| 수신 snapshot | version 하한 | 수신 시각 column 추가: migration·worker 변경이 필요하고, version이 같은 정보를 이미 담는다 |
| environment 제한 key | 403 | 전부 보여주기: 범위 밖 데이터 노출. 빈 결과: "없음"과 "못 봄"이 섞인다 |

## Rollback

- query-api의 `MONTRACER_CURSOR_KEY_HEX`를 비우면 검색 경로만 404다.
- 동시 실행 상한은 `MaxConcurrent`·`MaxWaiting`으로 조정한다.

## 증거

- `internal/queryplan`
  - 컴파일 결과 SQL과 parameter, 사용자 값(인젝션 시도 포함)이 SQL에 없음
  - 한도 거절: 깊이·조건 수·in 수·문자열 길이·타입·범위·hex·UUID·map key·빈 contains·없는 field·허용 밖 연산자. 각 거절의 field 경로를 확인한다
  - 정규형 동일성
- `internal/query`
  - cursor로 전 page 순회, 첫 page snapshot 고정, filter가 컴파일된 SQL·parameter로만 저장소에 감
  - 응답 형식(projection, null ID, no-store)
  - 거절 10종(모르는 body key, signal, path 불일치, filter, order, projection, limit, cursor)이 저장소에 닿지 않음
  - cursor binding(다른 key·filter 거절, page 크기 변경 허용), environment 제한 key 403, 동시 실행 5+20 초과 429
- `internal/telemetrystore` 통합(ClickHouse)
  - 정렬·dedup(최초 수신 본문), filter 조합(severity·body 대소문자 무시·trace_id·attribute), keyset 두 page
  - 수신 snapshot, 만료 제외, tenant 분리, 25시간 거절, environment 제한 key 403
- `tests/isolation`: B가 A의 trace_id로 log 검색하면 빈 결과, A의 cursor 재사용 400
- `make smoke`(CI): 장애 trace의 log 2건(error 1건)을 trace_id로 찾는다(log↔trace 연결)
- 리뷰 반영 뒤 추가
  - 기본 range cursor가 시계가 흘러도 쓰인다(P1 회귀)
  - 누적 10,000행에서 멈춤, gate 대기 상한
  - 없는 값 비교 제외(단위)
  - ClickHouse 통합: 컴파일된 SQL 형태 전부(in 4종·neq·exists·service_id), 없는 값 제외, 수신 snapshot 경계, 시각이 다른 재전송의 최초 수신 dedup
  - query_log에 log 검색 값(본문 needle·attribute·trace_id)이 없다(ADR 0032 시험 확장)
- spec-reviewer 지적 반영: 기본 range cursor(P1), dedup 순서, SQL 형태 통합 시험, query_log, fingerprint env, gate(검증 뒤·대기 상한·RB02), 설정 rollback, 없는 값 의미, 누적 10,000행
