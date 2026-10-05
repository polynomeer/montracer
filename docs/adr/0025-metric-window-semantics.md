# ADR 0025: metric window 집계 의미 — reset, 기준점, 품질 표시, histogram 병합

- 상태: 승인 (결정 위임)
- Owner: Data lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021~0024와 같은 위임). 근거는 §5
- 날짜: 2026-10-05 (제안·결정)
- 관련: F04, F05, E03 · D02 §07, §10, §21~22 · D06 §04 metric oracle · CLAUDE.md 계약 5·6 · ADR 0018 §8, 0021 §5

## 배경

D02 §07이 정한 것은 다음과 같다.

| 유형 | 처리 |
|---|---|
| monotonic sum | cumulative 차분 또는 delta 합산. start_time 변경은 reset이다. 첫 cumulative는 기준점만 설정한다 |
| non-monotonic sum | 원시 값. 임의 rate를 적용하지 않는다 |
| gauge | 결측을 0으로 채우지 않는다 |
| histogram | 동일 경계로 통합한 뒤 percentile을 계산한다 |
| summary | 원본만 표시하고 quantile을 평균하지 않는다 |

D02 §10은 count·sum·min·max·histogram state를 보관하고 p95를 평균하지 않으라고 한다. 음수 delta, count와 bucket 합 불일치, 단위 충돌은 격리 metric으로 다룬다.

D06 §04는 oracle을 요구한다: cumulative reset, out-of-order, duplicate delta, missing baseline, histogram 경계 차이, 단위 변경, NaN/Inf, 빈 bucket, 알려진 분포의 p95, percentile 평균이면 실패하는 fixture.

정하지 않은 것은 다음과 같다.

- 값이 줄었을 때 reset으로 볼지
- reset 뒤 값을 어떻게 셀지
- delta point를 어느 window에 넣을지
- NaN·Inf 처리
- window 안에서 histogram 경계가 바뀔 때의 처리
- percentile 보간 방식

## 결정

`internal/metricagg`의 순수 계산(저장소·시계 무관)으로 다음 의미를 고정한다. rollup 저장(`metric_1m`·`metric_1h`)과 watermark는 이 계산을 쓰는 후속 작업이다.

### 1. window와 point 배정

- window는 `[start, end)`이고 point는 **관측 시각(end_time)**으로 window에 속한다. delta의 구간(start~end)을 window에 나눠 배분하지 않는다.
- 도착 순서와 무관하게 관측 시각으로 정렬한다(out-of-order).
- 같은 관측 시각의 point는 하나로 줄인다(duplicate delta).
  - 값이 다르면 먼저 온 것을 쓰고 `duplicate_timestamp`로 표시한다.
  - worker가 batch 안의 상충 값은 이미 quarantine한다(ADR 0021 §4). 이것은 batch를 넘는 경우를 막는 방어선이다.

### 2. 유형별 계산

| 유형 | 결과 |
|---|---|
| gauge, non-monotonic cumulative sum | last·min·max·합·표본 수 (평균 = 합/표본 수). 차분·rate 없음 |
| monotonic delta sum | 증가량 = delta 합. 음수 delta는 빼고 `negative_delta` |
| non-monotonic delta sum | 순변화 = delta 합 (음수 정상) |
| monotonic cumulative sum | 증가량 = 연속 point 차분 합. 아래 reset·기준점 규칙 |
| histogram (delta) | bucket·count·sum 합 |
| histogram (cumulative) | 연속 point의 bucket·count·sum 차분 합 |
| summary, exponential histogram | window 집계 안 함. `unsupported_type`, 원본 표시만 |

- **exponential histogram:** scale이 다른 point를 병합하려면 scale을 낮추는 변환이 필요하다. 이 변환은 후속 결정이다. 원본은 `metric_points.payload`에 보존되어 있다(ADR 0018 §8).

### 3. reset과 기준점 (monotonic cumulative sum, cumulative histogram)

- **reset 조건**
  - sum: **start_time이 바뀌었거나**(D02 §07) **값이 줄었을 때**
  - histogram: start_time 변경, count 감소, 어떤 bucket이든 감소
- **reset 뒤 처리:** 새 point의 값을 **0부터 쌓인 것**으로 보고 전체를 더한다. reset 횟수를 세고 `reset`으로 표시한다.
- **기준점:** window 시작 전 마지막 point(baseline)로 첫 차분을 계산한다.
  - 기준점이 없으면 첫 point는 **기준점으로만** 쓴다(D02 §07). window를 `partial`과 `missing_baseline`으로 표시한다. 누적값 전체를 증가량으로 세지 않는다.
  - 기준점이 없고 point가 하나뿐이면 증가량을 **모른다**(`HasIncrease=false`). 0이 아니다.
- **histogram 경계 변경:** 차분할 수 없다. 그 point를 새 기준점으로 삼고 `bounds_changed`·`partial`로 표시한다.

### 4. 품질 표시와 결측 (계약 6)

- 해당 유형에 값이 없으면 `Has*`가 false다. 빈 window는 값 없음이다.
- NaN·±Inf 값과 sum은 계산에서 빼고 `nan_value`·`inf_value`로 표시한다. 보내지 않은 histogram sum은 NaN이다.
- count와 bucket 합이 다르거나 bucket 수가 경계 수 + 1이 아니면 그 point를 빼고 `count_bucket_mismatch`로 표시한다.
- 사유는 고정 문자열이다. rollup 행과 조회 응답(`meta`·series completeness)에 그대로 실어 UI가 불완전을 표시하게 한다.

### 5. 여러 stream 병합과 percentile

- **병합:** histogram은 **bucket을 먼저 더한 뒤** percentile을 구한다(계약 5).
  - 경계가 다르면 `ErrBoundsMismatch`를 낸다. 임의로 재분배하거나 평균하지 않는다.
  - 단위가 다르면 `ErrUnitMismatch`를 낸다. 단위 변경은 다른 stream이다(D02 §07 identity).
- **percentile:** bucket 안에서 선형 보간한다.
  - 첫 bucket의 하한은 첫 경계가 양수면 0이다.
  - +Inf bucket에 걸리면 가장 큰 유한 경계를 쓴다.
  - 관측이 없으면 값이 없다(0이 아니다).

### 6. 외부 사례 근거 (2026-10-05 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| §3 값 감소 = reset, 0부터 다시 셈 | Prometheus `rate()`/`increase()` ([PagerTree 해설](https://pagertree.com/learn/prometheus/promql/counter-rates-and-increases), [MetricFire](https://medium.com/@MetricFire/how-the-prometheus-rate-function-works-cc63fe90ef19)) | counter 값 감소를 reset으로 보고, reset 뒤 값은 0에서 증가한 것으로 보정 | 채택(start_time 변경과 함께) |
| §3 reset 뒤 값 전체 | New Relic cumulative metrics ([docs](https://docs.newrelic.com/docs/data-apis/understand-data/metric-data/cumulative-metrics/)) | 값이 갑자기 줄면 reset으로 보고 새 측정을 0이 앞선 것처럼 delta로 냄 | 채택 |
| §3 첫 point는 기준점만 | OTel Collector `cumulativetodelta` ([README](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/processor/cumulativetodeltaprocessor/README.md)) | 처음 본 series는 이전 값이 없어 유효한 delta를 만들 수 없다. 첫 sample을 버리는 `drop` 선택지가 있음 | 채택(명세와 같음). start_time으로 새 counter를 판정해 첫 값을 쓰는 `auto` 방식은 명세 개정이 필요해 보류 |
| §5 bucket 안 선형 보간 | Prometheus `histogram_quantile` ([quantile.go](https://gitverse.ru/germanubis/prometheus/content/main/promql/quantile.go), [ClickHouse quantilePrometheusHistogram](https://clickhouse.com/docs/fr/reference/functions/aggregate-functions/quantilePrometheusHistogram)) | bucket 안 분포를 선형으로 가정해 보간, 추정치임 | 채택 |
| §5 bucket 병합 후 percentile | Prometheus Histograms and Summaries (D02 §23 R8) | 여러 instance의 quantile은 평균할 수 없고 histogram bucket을 합쳐야 함 | 채택 (계약 5) |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 값 감소 처리 | reset | 음수 증가량으로 반영: counter에는 의미가 없다. 무시: reset 뒤 증가분을 잃는다 |
| 첫 cumulative | 기준점만 + partial | 누적값 전체를 증가량으로: process 시작 전 누적이 한 window에 몰려 spike가 된다(명세 위반) |
| delta 배정 | 관측 시각 | 구간 비례 배분: delta가 window보다 길 때만 의미가 있고, 1m window·15s 수집에서는 오차보다 복잡도가 크다 |
| 경계가 다른 병합 | 오류 | 재분배(rebucketing): 분포를 가정해 숨은 오차를 만든다. 사용자에게 경계 통일을 요구한다 |

## 결과

- Sprint 2 "metric reset oracle"(D06 §04)이 충족된다.
- **다음 단계 (같은 의미를 그대로 씀)**
  - `metric_1m`·`metric_1h` rollup 테이블과 rollup 계정·row policy
  - watermark(최대 관측 − 2분)와 10분 늦은 도착 재계산(version 증가)
  - idle partition 처리(D02 §21)
  - metric 조회 API
- **D02 §10 "격리 metric":** worker 지표와 함께 rollup job에서 사유별로 센다.

## Rollback

순수 계산 라이브러리다. 아직 저장 경로에 쓰이지 않는다. 되돌릴 저장 상태가 없다.

## 재검토 조건

- exponential histogram을 집계할 때(scale 변환 규칙).
- OTel `auto` 방식처럼 start_time으로 새 counter를 판정해 첫 값을 쓰도록 명세를 바꿀 때.
- delta가 window보다 긴 stream이 흔할 때(구간 배분).

## 증거

- `internal/metricagg/oracle_test.go`: D06 §04 항목 전부
  - cumulative reset(start_time 변경과 값 감소)
  - out-of-order(거짓 reset 없음)
  - duplicate delta(한 번만 셈, 상충 값 표시)
  - missing baseline(누적값을 증가량으로 세지 않음, point 하나면 값 없음)
  - histogram 경계 차이(stream 안에서는 새 기준점, 병합은 오류)
  - 단위 변경 오류
  - NaN/Inf 제외와 빈 gauge window에 값 없음, `[start,end)` 경계
  - 빈 bucket과 빈 histogram에서 값 없음, +Inf bucket
  - 알려진 균등 분포의 p95 = 95±1
  - **percentile 평균 fixture:** 병합 p95 ≤ 10ms인데 평균하면 ~500ms. 평균으로 구현하면 실패한다.
  - 음수 delta(monotonic만), count·bucket 불일치, non-monotonic cumulative는 값, 미지원 유형
- **mutation 확인:** reset 판정에서 값 감소를 빼면 `TestCumulativeReset`이 실패한다(증가량 −92).
