# ADR 0048: Overview(S01) — 서비스별 RED 요약

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-08 "S01 Overview" 선택)
- Owner: FE
- 날짜: 2026-10-08 (제안·결정)
- 관련: F03, E04 · D05 §01·§03·§04·§05, D02 §07·§08·§13·§19 · 계약 5·6 · ADR 0027, 0038, 0042, 0047

## 배경

D05 §04는 S01을 `/o/{org}/overview`, 핵심 API "service·SLO summary, 상태→서비스"로만 정한다. D05 §01은 Observe 그룹의 역할을 "상태와 영향 파악"으로 둔다.

- SLO·monitor는 아직 없다(E05).
- 서비스 RED(요청량·오류율·p95)는 서비스 상세(S02)가 비샘플링 histogram으로 계산한다(ADR 0042). 서비스마다 7번 조회한다.
- 서비스가 수백 개면 Overview가 서비스마다 조회할 수 없다.

## 결정

1. **백엔드 변경 없이** 기존 `POST /query/metrics`의 group_by로 한 번에 받는다. 조회는 서비스 수와 무관하게 6개다.
   - 요청 수: `count` group by (`service.namespace`, `service.name`, `deployment.environment.name`, `http.response.status_code`), 범위 전체 한 점(ADR 0042)
   - p95: `p95` group by (서비스 자연 키 3개), 범위 전체 한 점. 서비스별로 서버가 bucket을 병합한 값이다(계약 5).
   - 조직 합계 카드: 요청 수 by status와 p95(group 없음), 각각 범위 전체 한 점과 차트용 step
     - 조직 p95도 모든 서비스의 bucket을 병합한 값이다. 서비스 p95들의 평균이 아니다.
   - 원천은 S02와 같은 `http.server.request.duration`, 오류는 5xx(ADR 0042)다.
   - **한도:** 서버는 결과가 group × step 10,000행을 넘으면 422 예산 초과다(ADR 0027 §2). 서비스 표는 범위 전체 한 점(step 1개)이라 서비스 × status 조합이 10,000에 가까워질 때까지 막히지 않는다. 넘으면 표 자리에 실패를 보인다.
     - D02 §19의 "series 최대 1,000"은 서버가 아직 시행하지 않는다(이 PR 이전부터의 명세·구현 차이, 사용자에게 알림).
   - **조직 p95와 bucket 경계:** 서비스(SDK)마다 histogram bucket 경계가 다르면 서버가 병합하지 않고 `bounds_mismatch`를 돌려준다. 카드는 값 대신 "bucket 경계 불일치"를 보인다(평균이나 0으로 바꾸지 않는다). 언어가 섞인 조직에서는 흔할 수 있다(재검토 조건).
2. **서비스 catalog와 합친다**(`GET /services`, 같은 environment, 1,000개)
   - 자연 키(namespace·name·environment)로 맞춘다. catalog 항목은 서비스 상세로 링크하고 수신 상태(수신 중·24시간 미수신)를 보인다.
   - catalog에 있는데 metric이 없는 서비스는 "요청 metric 없음"이다. 0 요청으로 보이지 않는다(계약 6).
   - metric에만 있는 서비스(catalog 등록 누락, ADR 0038 §2)는 "catalog 미등록" 배지와 함께 링크 없이 보인다. 숨기지 않는다.
   - **catalog를 다 받지 못했으면 "미등록"으로 단정하지 않는다**(D05 §03 "추측하지 않는다"). catalog 조회가 실패했거나 1,000개 page 한도에서 잘렸으면 찾지 못한 서비스는 "catalog 확인 안 됨"이고, 표 위에 그 이유를 쓴다.
3. **상태 판정을 하지 않는다.**
   - SLO·monitor가 없어 근거 있는 "정상/이상"이 없다. 임의 기준(예: 오류율 5%)으로 색을 칠하지 않는다.
   - 대신 기본 정렬을 오류율 큰 순으로 하고, 요청량·p95·이름으로 바꿀 수 있다. 화면에 "정상·이상 판정은 하지 않습니다"를 쓴다.
   - 정렬은 URL `sort`에 남는다(공유 허용 키). 열 머리에 `aria-sort`를 붙인다.
   - **값 없는 서비스는 정렬 방향과 무관하게 뒤**다. 0으로 보고 앞에 세우지 않는다.
4. **일부 집계(partial):** 칸에는 `*`와 screen reader용 "일부 집계"만 두고, 이유는 표 아래에 한 번 쓴다. 표 이름(caption)에도 "* 일부 집계: 이유"를 붙여 표를 탐색하는 screen reader가 이유를 듣게 한다. 이유는 S02의 규칙(watermark와 범위 끝 비교)을 따른다.
5. **조회 실패(D05 §03 실패 > empty)**
   - 서비스 표 조회가 실패하면 표 자리에 실패(request ID)와 "서비스가 없다는 뜻이 아닙니다"를 보인다. 신규 설치 empty로 보이지 않는다.
   - 카드 조회가 실패하면 값 대신 "확인할 수 없음(조회 실패)"이다. "받은 metric 없음"으로 보이지 않는다.
   - 위쪽 알림은 카드 조회의 실패만 보인다. 같은 실패를 위·아래 두 번 보이지 않는다.
6. **environment context:** 모든 metric 조회에 `deployment.environment.name` 조건을 걸고 catalog도 그 environment로 부른다(ADR 0047과 같다).
7. **공통 수정:** 표의 가로 스크롤 상자(`.mt-scroll-x`)에 `position: relative`를 준다.
   - 표 안의 screen reader용 숨김 글(`position: absolute`)이 스크롤 상자에 갇히지 않아 375px에서 페이지가 가로로 늘어났다(수동 확인에서 발견, 771px).
   - S08 Metrics의 미등록 서비스 ID처럼 다른 화면의 숨김 글에도 같은 위험이 있어 공통 CSS에서 고친다. 기록은 [PS-0007](../troubleshooting/PS-0007-hidden-text-widens-mobile-page.md).

## 외부 사례 근거 (2026-10-08 확인)

| 사례 | 내용 | 반영 |
|---|---|---|
| The RED Method (Tom Wilkie, [Grafana blog](https://grafana.com/blog/2018/08/02/the-red-method-how-to-instrument-your-services/)) | Rate·Errors·Duration을 아키텍처의 모든 서비스에 같은 방식으로 본다 | 결정 1(서비스마다 같은 RED 열) |
| Datadog Software Catalog ([services list](https://docs.datadoghq.com/tracing/services/services_list/)) | Performance 보기는 environment별 latency·traffic·error rate를, Reliability 보기는 실패한 monitor·사건·배포 같은 근거로 표시한다 | 결정 3(근거(monitor) 없이 상태를 칠하지 않는다, environment 기준) |

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A. group_by 한 번 조회(채택) | 백엔드 변경 없음, 조회 수 고정 | 서비스 × status가 series 상한에 묶임 |
| B. 서비스마다 S02 조회 | 코드 재사용 | 서비스 수 × 7 조회, tenant 동시 실행 상한(5)에 막힘 |
| C. 전용 summary API·materialized view | 큰 조직에서 빠름 | 새 저장·동기화 경로. 지금 규모에 이르다(재검토) |
| D. 오류율 임계값으로 상태 색 | 한눈에 보임 | 근거 없는 수치(계약 6 취지·D05 §03 "추측하지 않는다") |

## 결과

- 조직 전체 RED와 서비스별 요약에서 서비스 상세로 1번에 간다(D05 §04 "상태→서비스").
- 영향 받는 계약: 없음(API 변경 없음). URL 공유 허용 키에 `sort`를 더했다.

## Rollback

`overview` 라우트를 placeholder로 되돌린다.

## 재검토 조건

- SLO·monitor(E05): 근거 있는 상태 열(위반 SLO·발화 monitor)과 상태 → 서비스 정렬
- 서비스 × status가 결과 행 예산에 닿을 때, 또는 D02 §19 series 상한을 서버가 시행하게 될 때: status를 2xx·4xx·5xx로 묶는 서버 집계나 summary API(후보 C)
- 조직 p95가 `bounds_mismatch`로 자주 비는 조직: bucket 경계 정규화(재bucket)나 서비스 p95 분포 표시(평균은 금지)
- 서비스 catalog가 1,000개를 넘을 때: catalog page 이어 받기

## 증거

- `apps/web/src/features/overview/overview.test.ts`: 자연 키로 status 합산·5xx 오류율·p95 ms, metric 없는 catalog 서비스(no_series), 병합 불가 p95(bounds_mismatch), 미등록 서비스, environment 구분, 모르는 단위, 정렬(값 없는 서비스는 뒤)
- `Overview.test.tsx`: 조회 6개(서비스 수 무관)·group_by·원천, 합계 카드(오류율·p95), 표 순서·링크·미등록·요청 metric 없음·수신 상태·판정 안내, 정렬 URL·aria-sort, environment 조건, metric 없음(0 요청 아님), 7일 초과
- 실패 경로(`Overview.test.tsx`): 서비스 표 조회 실패(표 자리 실패, 알림 1번), 합계 실패(확인할 수 없음), catalog 실패·잘림(확인 안 됨), 조직 p95 `bounds_mismatch`
- `scrollBox.test.ts`: `.mt-scroll-x`의 `position: relative`(PS-0007)
- 이 PR에서 함께 고친 결함: 전체 시험 반복 중 S08 Logs 입력 시험이 가끔 실패해, 조건 입력이 debounce 반영 뒤 입력 중 글자를 되돌리던 문제를 찾았다(S04·S08 공통, [PS-0008](../troubleshooting/PS-0008-debounced-input-reverted.md), `useDraft.test.ts`)
- spec-reviewer: P0 없음. P1(조회 실패가 "서비스 없음"으로 보임)과 P2(한도 문장, catalog 잘림, bounds_mismatch 기록, PS, partial 이유 연결) 반영
- 수동: mock API로 1440px(카드·표·정렬·일부 집계 안내), 375px에서 Overview·Metrics·Logs 가로 넘침 없음(결정 6), 콘솔 오류 없음
