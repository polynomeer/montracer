# ADR 0050: monitor 평가 의미 — window, group 값, 상태 머신

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-09 "다음 단계를 진행해줘", E05 단계 B)
- Owner: API lead
- 날짜: 2026-10-09 (제안·결정)
- 관련: F06, E05 · D02 §17·§20·§21, D05 §09, D06 §05 · 계약 5·6 · ADR 0025, 0026, 0027, 0042, 0049

## 배경

ADR 0049는 monitor 정의를 저장했다. 이 ADR은 그 정의를 **어떻게 평가하는지**를 정한다. 저장·스케줄과 무관한 순수 계산(`internal/alerting`)이고, alert-worker의 주기 평가(단계 B2)와 24시간 dry-run(단계 B3)이 같은 함수를 쓴다.

D02 §17·§21이 정한 것은 다음과 같다.
- **상태:** OK → PENDING → ALERT → RECOVERING → OK. 위반이 for_seconds 이상 유지되면 ALERT, 복구 조건이 2회 연속이면 OK다. NO_DATA와 EVALUATION_ERROR는 별도 상태다.
- watermark 이전 확정 window를 평가한다.
- NO_DATA는 "쿼리는 성공했지만 최소 데이터 조건을 충족하지 않음", query failure는 EVALUATION_ERROR이고 이전 정상값으로 대체하지 않는다.
- group은 최대 1,000이다(D02 §20).
- D06 §05의 경계 시험: "2% 초과 조건이면 정확히 2%는 발화하지 않는다."

명세가 정하지 않은 것이 있다.
- 결측·실패 동안 열린 경보(사건)는 어떻게 되는가
- 실패가 위반 연속을 끊는가
- 사라진 group은 어떻게 되는가
- group 상한을 넘으면 어떻게 하는가
- window 끝을 어떻게 정하는가

## 결정

### 1. window와 조회

- **window 끝:** min(지금, tenant rollup watermark)를 분 경계로 내린 값이다. 시작은 끝 − window_seconds다. 확정된 1분 window만 평가한다(D02 §17).
  - watermark가 없으면(rollup이 아직 이 tenant의 window를 하나도 만들지 않음) 평가할 window가 없다. 모든 group이 결측(`no_watermark`)이다.
  - **watermark가 10분 넘게 멈추면 결측(`stale_watermark`)이다(D02 §21 freshness, 리뷰 반영).** 수집·rollup 장애로 watermark가 멈췄는데 마지막 window를 계속 평가하면, 마지막 판정(OK일 수 있다)이 무기한 남는다. 10분은 늦은 데이터 재계산 한도(D02 §07)와 같다. 정상 지연은 watermark 정의(최대 관측 − 2분)와 rollup 주기로 수 분이다.
- **조회:** metric 조회(ADR 0027)를 **범위 전체 한 점**(step = window, ADR 0042)으로 하고 1분 rollup을 읽는다.
  - group_by와 filter(and·eq)는 spec 그대로 넘긴다.
  - error_ratio는 group_by 뒤에 `http.response.status_code`를 더해 `count`를 받는다.
- **값 계산:** query-api와 같은 함수(`internal/metricvalue`, `internal/query`에서 옮김)를 쓴다. 분위수는 bucket 병합 뒤 계산하고(계약 5), 결측 사유 체계도 같다.

### 2. group 값

- **error_ratio:** group마다 status별 count를 합쳐 5xx / 전체다(서비스 RED와 같은 정의, ADR 0042).
  - 전체가 0이면 `no_requests`, minimum_requests 미만이면 `below_minimum_requests`다. 둘 다 값이 없다.
  - 4xx는 오류가 아니다.
  - **일부 status bucket이 충돌·병합 불가(`unit_conflict` 등)로 빠지면 group 전체가 결측이다(리뷰 반영).** 분자·분모가 모두 하한이라 비율의 방향도 모른다. 남은 2xx만으로 "오류율 0"을 만들어 OK로 판정하지 않는다.
  - **status label이 없는 요청**(응답 없이 끊긴 요청·속성을 빼먹은 SDK)은 분모에 넣고 5xx로 세지 않는다. 오류율이 낮게 나올 수 있어 partial로 표시하고 그 수(`UnknownStatus`)를 기록에 남긴다.
  - error_ratio의 group_by에 `http.response.status_code`를 넣으면 저장 전에 거절한다(status별 group의 비율은 0 아니면 1이라 의미가 없다, ADR 0049 변경 이력).
- **metric:** 그 group의 연산 값이다. 값이 없으면 metricvalue 사유(`no_data`·`bounds_mismatch`·`unit_conflict`·`not_applicable` 등)다.
- **partial:** 일부 1분 window가 없어 계산된 값(delta histogram은 관측이 없는 분에 window를 만들지 않는다)은 평가에 쓰고 partial로 기록한다. 위의 "버린 bucket"과 다르다 — 그것은 결측이다.
- **group key:** group_by key를 정렬한 `"key"="value"` 목록이다. group_by가 없으면 group은 하나(`""`)다. series가 하나도 없으면 그 group이 `no_data`다.

### 3. 판정

- 값이 없으면 **결측(no_data)**이다. 위반도 정상도 아니다(계약 6, D02 §21).
- 값이 있으면 condition으로 위반·정상을 판정한다. **gt·lt는 엄격 비교**다. threshold와 정확히 같으면 gt 위반이 아니다(D06 §05 2% 경계).

### 4. 상태 머신 (group마다, `alerting.Next`)

- **같은 window는 한 번만 반영한다(리뷰 반영).** 30초 주기로 1분 rollup을 평가하면 연속 두 평가가 같은 window를 받는다. 이미 반영한 window(끝 ≤ 마지막 반영 window)면 아무것도 바꾸지 않는다. 같은 데이터로 복구 횟수가 두 번 올라 정상 window 하나로 사건이 닫히지 않게 한다. for_seconds도 window 끝으로 잰다.
- **결측 지속 시간은 벽시계로 잰다.** watermark가 멈춰도(window 없는 결측) no_data `after_seconds`가 흐른다.
- **window 없는 판정**(watermark 없음·멈춤, 조회 실패)은 연속 횟수를 올리지 않는다.


| 판정 | 전이 |
|---|---|
| 위반 | 연속 위반 시작(window 끝)을 둔다. 지금 window 끝 − 시작 ≥ for_seconds면 ALERT(사건 열림), 아니면 PENDING. 사건이 이미 열려 있으면(RECOVERING·NO_DATA·EVALUATION_ERROR 중이었어도) for_seconds를 다시 기다리지 않고 바로 ALERT — no_data로 연 사건이면 사유가 violation으로 바뀌지만 같은 사건이다(알림 중복 억제 key 유지) |
| 정상 | 사건이 없으면 OK(PENDING 취소). 사건이 열려 있으면 RECOVERING이고, 연속 정상이 2회(recovery_evaluations)면 OK와 사건 닫힘 |
| 결측 | 위반 연속과 복구 연속을 모두 끊는다. no_data 정책이 alert이고 결측이 after_seconds(벽시계) 이어지면 ALERT(사건 사유 no_data), 아니면 NO_DATA |
| 조회 실패 | EVALUATION_ERROR. 복구 연속은 끊고 **위반 시작·결측 시작 시각은 유지**한다 |

- **사건(episode)은 결측·실패로 닫히지 않는다.** ALERT로 연 사건은 NO_DATA·EVALUATION_ERROR 동안에도 열려 있고, 닫히는 길은 복구 2회 연속뿐이다. 데이터가 끊겼다고 경보가 조용히 풀리면 "문제 없음"으로 오해한다(계약 6).
- **조회 실패는 보수적으로 비대칭이다.**
  - 실패는 회복의 근거가 아니므로 위반 시작 시각을 지우지 않는다. 실패가 끼어도 경보가 늦춰지지 않는다.
  - 실패가 끼면 "2회 연속 정상"이 확인되지 않은 것이므로 복구 연속은 끊는다. 사건이 실패 덕에 닫히지 않는다.
- **결측은 대칭이다.** 위반도 정상도 확인되지 않아 둘 다 끊는다. 결측 자체를 경보할지는 no_data 정책이 정한다.
- **전이 기록:** Transition에 이전·다음 상태와 사건 열림·닫힘을 담는다. 알림(단계 C)과 timeline(D05 §09)이 이것을 쓴다.

### 5. group 집합

- **사라진 group:** 전에 있던 group이 이번 결과에 없으면 결측(`group_missing`)이다. 사라졌다고 정상이 되지 않는다.
- **조회 실패와 group 상한:** 조회가 실패하거나 group이 1,000을 넘으면 **monitor 전체를 EVALUATION_ERROR**로 둔다(`query_failed`·`group_limit_exceeded`).
  - 일부 group만 평가하고 나머지를 버리지 않는다. 버린 group의 경보를 숨기게 된다.
  - group_by가 없고 이전 상태도 없으면 group `""`이 오류다.
- **monitor 수준 결과(리뷰 반영):** `Evaluate`는 group과 별개로 monitor 상태(`evaluated`·`no_data`·`error`)와 사유를 돌려준다. group_by가 있는 monitor가 처음부터 실패해 group이 하나도 없어도 "평가 안 됨"이 기록된다(D05 §09 "query failure가 green으로 표시되지 않음"). 단계 B2가 이것을 평가 기록에 남긴다.

## 외부 사례 근거 (2026-10-09 확인)

| 사례 | 내용 | 반영 |
|---|---|---|
| Prometheus alerting rules ([docs](https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/)) | `for` 동안 active지만 firing 전인 요소는 pending이다. `keep_firing_for`는 조건이 마지막으로 충족된 뒤에도 firing을 유지해 flapping과 거짓 해제를 막는다 | 결정 4: PENDING과 for_seconds, 복구 2회 연속(거짓 해제 방지), 결측·실패로 사건을 닫지 않음 |
| Grafana alert rule state ([docs](https://grafana.com/docs/grafana/latest/alerting/fundamentals/alert-rule-evaluation/state-and-health/)) | Normal·Pending·Alerting 외에 No Data(쿼리 성공, 데이터 없음)와 Error(쿼리 실패)를 별도 상태로 둔다 | 결정 3·4: NO_DATA와 EVALUATION_ERROR 구분(D02 §17·§21과 같음) |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 결측·실패 중 사건 | 열린 채 유지 | 닫음(Prometheus 기본 비활성): 데이터가 끊기면 경보가 풀려 정상처럼 보인다(계약 6) |
| 실패와 위반 연속 | 유지 | 끊음: 실패가 반복되면 경보가 계속 미뤄진다 |
| 실패와 복구 연속 | 끊음 | 유지: 실패가 끼어도 사건이 닫힐 수 있다 |
| group 상한 초과 | monitor 전체 EVALUATION_ERROR | 상위 1,000개만: 나머지 group의 경보를 숨긴다 |
| window 끝 | min(지금, watermark) 분 경계 | 지금: 확정되지 않은 window를 평가해 늦게 온 데이터로 값이 바뀐다 |
| 값 계산 | query-api와 같은 함수(metricvalue) | 따로 구현: 화면과 경보의 숫자가 달라질 수 있다 |

## 결과

- 평가 의미가 정해지고 시험으로 고정된다. 아직 실행 경로(alert-worker)·저장(alert_instances)·dry-run은 없다(단계 B2·B3).
- **query-api 변경:** 값 계산을 `internal/metricvalue`로 옮겼다. 동작은 같다(기존 시험 통과).
- 영향 받는 계약·schema·API: 없음

## Rollback

순수 패키지 추가다. 되돌려도 실행 경로에 영향이 없다. metricvalue 이전은 query 시험이 동작 동일을 보장한다.

## 재검토 조건

- 단계 B2(alert-worker): 평가 슬롯 lease·idempotency, alert_instances 저장, 전이 outbox, system principal
- 단계 B3: 24시간 dry-run 결과 모양(D05 §09 "후보 firing 구간과 데이터 coverage")
- watermark 멈춤 한도(10분)가 실제 지연 분포와 맞지 않을 때
- composite monitor(F11, Kleene 3값)는 이 상태를 입력으로 쓴다

## 증거

- `internal/alerting/alerting_test.go`
  - window(watermark 없음 → 없음, min(지금, watermark) 분 경계), 조회(status group 추가, step = window, 1분 rollup, filter)
  - error_ratio(정확히 2%는 gt 위반 아님, 최소 요청 미달 → NO_DATA, 4xx 제외, partial), metric(bounds_mismatch), series 없음 → no_data, group 상한
  - 상태 머신
    - for_seconds 지나야 ALERT, 복구 중 위반은 같은 사건, 결측 중 사건 유지
    - 실패가 끼면 복구 연속 끊김·위반 시작 유지
    - 결측은 위반 연속 끊음, 정상이 PENDING 취소, for 0, no_data alert after_seconds
  - group 집합: 사라진 group 결측, 조회 실패·상한 초과는 전체 오류
- 리뷰 반영 뒤 추가: 같은 window 반복(복구·for_seconds), watermark 멈춤(결측, 벽시계로 no_data 발화), 버린 5xx bucket(결측), status 없는 요청(partial), RECOVERING 중 결측, 실패 중 결측 시작 유지, no_data 사건 중 위반·정상 2회로 닫힘, 첫 평가 실패의 monitor 수준 결과, Violates 경계 8종
- spec-reviewer: P1 4건(같은 window 이중 반영, 멈춘 watermark, 버린 bucket으로 OK, 첫 실패 무기록)·P2 반영. 남김: metric kind에서 group 하나가 bucket 여러 개를 받는 경우(현재 저장소는 범위 시작 정렬로 한 점) — 단계 B2 통합 시험에서 고정
- `internal/query` 기존 시험: metricvalue 이전 뒤 동작 동일
