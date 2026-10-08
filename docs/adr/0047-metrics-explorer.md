# ADR 0047: Metrics 화면(S08) — 사전·query builder·legend

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-08 "S08 Metrics" 선택)
- Owner: FE
- 날짜: 2026-10-08 (제안·결정)
- 관련: F04, F05, E04 · D05 §03·§08·§11·§12, D02 §07·§13·§19 · 계약 5·6 · ADR 0027, 0028, 0041, 0042, 0045, 0046

## 배경

D05 §08은 Metrics를 이렇게 정한다.

- metric dictionary에서 type·unit·temporality를 먼저 보인다.
- query builder는 gauge에 무의미한 rate나 summary quantile 평균을 허용하지 않는다.
- 해상도 변경·rollup·data gap·provisional window를 legend에 표시한다.
- exemplar는 별도 선택 가능한 marker다.

조회는 `POST /query/metrics`(ADR 0027·0028)와 사전(ADR 0046)을 쓴다.

## 결정

1. **경로와 범위:** `/o/{org}/metrics`, 범위는 조사 context다. 7일(D02 §13)을 넘으면 조회하지 않고 줄이라고 안내한다.
2. **사전(왼쪽)**
   - 조사 범위의 끝에서 24시간에 관측된 metric을 이름순으로 보인다. 범위가 잘렸으면 그렇게 쓴다.
   - 이름 검색은 300ms debounce이고 서버 검색(`q`)이다.
   - 항목에는 유형(counter·up-down counter·gauge·histogram·summary)과 단위를 보인다. 조합이 섞이면 "유형·단위 충돌" 배지다.
   - 1,000개가 넘으면 검색으로 좁히라고 안내한다.
3. **선택한 metric의 머리말**
   - 조합 표(유형·단위·temporality·series 수·허용 연산)를 보인다.
   - **충돌(조합이 둘 이상)**
     - 모든 조합의 허용 연산이 같으면(단위만 다른 histogram, delta·cumulative counter) 그 연산을 고를 수 있다. 한 series에 단위가 섞이면 서버가 "단위 충돌"로 돌려준다.
     - 허용 연산이 다르면(counter와 up-down counter, gauge와 histogram) 고를 수 없고 계측 이름을 나누라고 안내한다.
     - 교집합을 쓰지 않는 이유: 서버의 유형 충돌 판정은 type·unit만 본다. counter(증가량)와 up-down counter(순변화)를 교집합 `sum`으로 합치면 섞인 값이 정상값처럼 그려진다(리뷰 P1).
   - 사전에 없는 metric(공유 링크, 24시간 밖)은 유형을 추측하지 않는다.
     - 모든 연산을 보인다. URL에 연산이 있으면(공유 링크) 그 연산으로 조회하고, 없으면 사용자가 고를 때까지 조회하지 않는다.
     - 맞지 않는 연산은 서버가 "해당 없음"으로 돌려준다. 사전 조회가 실패한 경우도 같다.
   - 사전을 받기 전에는 URL의 연산을 검증할 수 없으므로 조회하지 않는다. gauge에 rate 같은 요청을 보내지 않는다(시험에서 발견).
4. **query builder**
   - **연산:** 사전의 `aggregations`만 고를 수 있다(ADR 0046). 기본은 counter → rate, gauge → avg, histogram → p95다. 연산이 없는 유형(summary 등)은 선택을 막고 이유를 쓴다.
   - **해상도:** 기본은 자동이다. 120점 이하인 가장 작은 step을 1분~1일 선택지에서 고른다(1시간 → 1분, 4시간 → 2분, 1일 → 15분, 7일 → 2시간). 직접 고를 때는 series당 2,000점 이하만 보인다.
   - **나눠 보기:** label key 체크(최대 5개, ADR 0027)다.
   - **조건:** label = 값(and, 최대 10개)이다. 값은 300ms 뒤 또는 조회에 반영한다.
   - **environment:** 조사 context의 environment는 `deployment.environment.name` = 값 조건으로 **자동 적용**하고 그렇다고 표시한다.
     - metric은 resource 속성으로 environment를 가진다(OTel semantic conventions).
     - log·trace 화면과 달리 서버 조건으로 걸 수 있다.
5. **결과와 legend**
   - **머리말:** 연산 이름과 결과 단위를 보인다. rate는 `단위/s`, 관측 수는 "건", 무차원(`1`)은 생략한다.
   - **legend 줄:**
     - 해상도(step), 서버가 실제로 읽은 rollup(1분·1시간, ADR 0046 §4), 집계 완료 시각(`meta.watermark`)
     - 모르면 "모름"이라 쓰고 추측하지 않는다.
   - **표시 규칙:**
     - 끊긴 선 = 값 없음(0이 아님)
     - 음영 = 집계 중(pending)
     - 빈 원 = 일부만 집계(값이 바뀔 수 있는 provisional window)
     - 분위수는 bucket 병합 값(계약 5)
     - 음영·빈 원 항목은 그런 점이 있을 때만 보인다.
   - **차트:**
     - series마다 선 모양이 다르다(8가지, 색 4개와 함께). 색만으로 구별하는 쌍이 없다(D05 §12). 요약 표에 같은 선 모양 견본을 둔다.
     - 8개까지만 그리고, 넘으면 그렇다고 알린다.
     - y축은 위·가운데·아래 값을 보인다. 1만 이상은 한국어 축약(만·억), 아주 작은 값은 지수로 쓴다. 음수가 있으면 0 기준선을 그린다.
     - "표로 보기"에 step × series 값과 결측 사유를 둔다.
   - **series 요약 표:** 모든 series의 최근·최솟·최댓값, 값 있는 step 비율(completeness), 비고(단위·유형 충돌, 값 없는 step 사유)
   - **빈 결과:** "조건에 맞는 series가 없습니다(0과 다름)"
6. **URL과 공유:** `metric`, `agg`, `group`(쉼표), `step`(없으면 자동), `f`(key=value, 여러 개)
   - 공유 링크에는 형식이 정해진 앞의 넷만 남는다. 조건 값 `f`는 자유 입력이라 빠진다(ADR 0041·0045와 같은 규칙).
7. **하지 않는 것**
   - exemplar marker: 저장된 exemplar가 없다(D02 §07 metric point의 trace_id). 수집·저장과 함께 한다.
   - 다른 metric 간 연산, 여러 query 겹쳐 그리기, brushing으로 범위 고르기, dashboard로 보내기(F05)
   - label 값 자동완성(ADR 0046 §2)

## 외부 사례 근거 (2026-10-08 확인)

| 사례 | 내용 | 반영 |
|---|---|---|
| Grafana Prometheus query editor ([docs](https://grafana.com/docs/grafana/latest/datasources/prometheus/query-editor/)) | metrics explorer에서 이름·유형·설명을 보고 고르며, builder가 고른 metric에 맞는 연산 힌트를 준다 | 결정 2·4(사전에서 고르고 유형별 연산만) |
| Grafana Prometheus query editor (ADR 0027 §6) | 범위를 step 경계에 맞춘다 | 결정 4(해상도), legend의 맞춘 범위 |
| Prometheus HTTP API ([querying/api](https://prometheus.io/docs/prometheus/latest/querying/api/)) | metadata는 이름마다 여러 항목일 수 있다 | 결정 3(충돌 조합을 모두 보이고 공통 연산만) |

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A. 사전 + 구조화 builder(채택) | 유형 규칙을 화면이 강제, query 언어 없음(D02 §13) | 다른 metric 간 연산 없음 |
| B. PromQL 텍스트 입력 | 익숙함 | D02 §13 금지, 유형 규칙 강제 불가 |
| C. series 전부 그리기 | 빠짐 없음 | 수백 선은 읽을 수 없고 색 구별 불가 → 8개 + 요약 표 |
| D. environment를 group으로만 | 단순 | context와 화면이 어긋남. log·trace는 서버 조건이 없어 안내만 하지만 metric은 걸 수 있다 |

## 결과

- 서비스 RED(S02) 밖의 metric(JVM·사용자 정의)을 조사할 수 있다. 결측·집계 중·일부 집계를 0이나 정상값으로 보이지 않는다(계약 6).
- 영향 받는 계약: 없음. URL 공유 허용 키에 `metric`·`agg`·`group`·`step`을 더했다.

## Rollback

`metrics` 라우트를 placeholder로 되돌린다.

## 재검토 조건

- exemplar 저장: 선택 가능한 marker와 trace 이동(D02 §07 미보존 사유)
- dashboard(F05): 이 builder 상태를 widget query로 저장
- series가 자주 8개를 넘을 때: 상위 N(값 기준) 고르기

## 증거

- `apps/web/src/features/metrics/metricQuery.test.ts`: URL 왕복·잘못된 값 무시·group 5개, 공유 링크(조건 값 제외), 자동·선택 step, rollup 이름(모르면 null), 사전 범위, QuerySpec(eq·and·group_by), 기본 연산, series 이름(빈 값 "(없음)"), 값 표기, 결과 단위
- `MetricsExplorer.test.tsx`
  - 사전(유형·단위·충돌 배지, 7일 → 24시간), 고르기 전 미조회
  - counter → rate 요청, legend(해상도·rollup·집계 완료·결측·집계 중·일부 집계), 요약 표(completeness·사유), 연산 선택지 제한
  - gauge에 URL rate 무시(사전 전 미조회), summary 미조회와 이유, 단위만 다른 충돌은 조회·의미가 다른 충돌은 미조회, 사전에 없는 metric의 공유 링크 연산
  - environment 자동 조건, group by·조건 → URL·요청
  - 사전에 없는 metric(연산 고를 때까지 미조회), series 8개 초과 안내·표 전체·충돌 비고, 빈 결과, 7일 초과
- spec-reviewer: P0 없음. P1(충돌 조합의 교집합이 섞인 값을 정상값으로 보임) 반영 — 허용 연산이 같을 때만. P2(공유 링크 연산 동작을 문서와 맞춤, 선 모양 8가지, label byte 한도, "볼 수 있는 모든 환경"·"계측 충돌" 문구) 반영
- 수동: mock API로 1440px(사전·머리말·builder·차트·요약 표), 충돌·summary 화면, 375px 가로 넘침 없음(머리말 표를 가로 스크롤 안에 넣음), 콘솔 오류 없음

## 변경 이력

- 2026-10-08 (PS-0008): 조건 행 입력이 debounce 반영 뒤 편집 중인 값·빈 행을 되돌리던 문제를 `useDraft`로 고쳤다.
