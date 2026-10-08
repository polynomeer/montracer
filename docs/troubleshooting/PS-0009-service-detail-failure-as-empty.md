# PS-0009: S02 서비스 상세가 조회 실패를 "받은 metric 없음"·빈 표로 보임

- 날짜: 2026-10-08 (발견) / 2026-10-08 (해결)
- 영역: web (S02 서비스 상세)
- 영향: 상태 표현(계약 6) — 실패를 "데이터 없음"으로 읽어 잘못 판단할 수 있음, P1
- 수정: `0a8797b`, `a3345c3`(리뷰 반영)
- 관련: ADR 0042, ADR 0048(S01에서 같은 문제를 먼저 고침), D05 §03

> 번호: PS-0007·0008은 S01 Overview 작업(ADR 0048)이 쓴다. 그 작업과 따로 merge되므로 0009를 쓴다.

## 증상

RED query 7개 중 하나가 503 등으로 실패하면

- 그 카드 값이 "받은 metric 없음"(`no_series`)으로 보였다. 실패한 query의 결과가 빈 배열(`seriesOf` → `[]`)로 바뀌고 `single([])`가 `no_series`를 돌려주기 때문이다.
- 차트는 빈 series를 그려 "데이터 없음" 구간처럼 보였다.
- 리소스 표 query가 실패하면 "이 범위에 endpoint(http.route)별 요청이 없습니다"(빈 표) 또는 일부 열만 "받은 metric 없음"인 행이 보였다.

위쪽 ErrorNotice는 첫 실패 하나만 보였으므로, 어느 숫자가 실패의 결과인지 화면에서 구분할 수 없었다.

## 발견 경위

S01 Overview(ADR 0048) 리뷰에서 같은 결함(P1)을 고친 뒤, 같은 RED 코드를 쓰는 S02를 다시 살폈다. 기존 시험은 모든 query가 함께 실패하는 경우(429)만 보아 query 하나만 실패하는 경로를 덮지 못했다.

## 원인

실패와 empty를 같은 값(빈 series)으로 접었다. 조회 결과 → 값 변환 함수(`single`, `combineStatus`, `resourceRows`)는 "받은 것이 없다"만 알고 "받지 못했다"를 표현할 수 없었다.

## 해결

- `Reason`에 `query_failed`("확인할 수 없음(조회 실패)")를 추가했다.
- 화면에서 query 상태를 보고 실패한 카드 값은 `query_failed`, 차트는 "차트를 확인할 수 없음(조회 실패)"로 바꾼다. 같은 조건 새로고침만 실패해 이전 결과가 있으면 그 값을 보이고 알림이 "마지막 성공 기준"임을 말한다.
- 표 query가 하나라도 실패하면 표 대신 ErrorNotice와 "endpoint가 없다는 뜻이 아닙니다"를 보인다. 일부 열만 비운 행을 보이는 대안은 버렸다 — 그 열이 "metric 없음"처럼 읽힌다.
- 위쪽 알림은 카드 query 실패만, 표 실패는 표 자리에서만 보인다(같은 실패를 두 번 보이지 않게).
- 어느 query든 실패하면 "metric을 받지 못했습니다" 계측 안내를 보이지 않는다.

## 재발 방지

- 시험: `apps/web/src/features/services/ServiceDetail.test.tsx`
  - `카드 query $name 503` (total-count·total-p95·chart-count·chart-p95): 그 값·차트만 "확인할 수 없음", 다른 카드·표는 정상, 알림 1번
  - `표 query %s 503` (route-count·route-p95·route-hist_sum): 표 자리 실패·"없다는 뜻이 아닙니다", 빈 표·endpoint 개수 없음, 카드 정상, 알림 1번
  - `리소스 탭에서 표 query가 실패해도 metric 없음 안내로 바꾸지 않는다`
  - 위 9개는 수정 전 코드에서 모두 실패함을 확인했다.
  - `새로고침 실패로 이전 값을 보이는 카드가 있으면 … "마지막 성공 기준"을 알린다`: 대표 실패가 처음부터 실패한 query여도 이전 값의 기준 시각을 보인다(spec-reviewer P2)
  - `표 query %s가 아직 오지 않으면 … skeleton` (route-p95·route-hist_sum): 일부 열이 "받은 metric 없음"으로 보이지 않는다(spec-reviewer P2, 이 결함 이전부터 있던 같은 종류의 동작)

## 교훈

여러 query를 한 화면에 합칠 때는 query별로 실패를 그 query가 채우는 자리에 표시한다. 결과를 "빈 배열"로 정규화하는 `?? []`는 실패를 empty로 바꾸는 지점이므로, 그 앞에서 error를 먼저 본다.
