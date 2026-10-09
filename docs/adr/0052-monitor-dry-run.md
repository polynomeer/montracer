# ADR 0052: monitor 24시간 dry-run — 1분 bucket 재평가

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-10 "다음 단계를 진행해줘", E05 단계 B3)
- Owner: API lead
- 날짜: 2026-10-10 (제안·결정)
- 관련: F06, E05 · D02 §14·§17·§20·§21, D05 §09 · 계약 1·4·5·6 · ADR 0018, 0027, 0049, 0050, 0051

## 배경

`POST /monitors/validate`는 D02 §20에서 `normalized_spec, warnings, dry_run`을 돌려준다. ADR 0049는 평가기가 없어 `dry_run: null`과 경고 `dry_run_unavailable`로 두었다. D05 §09는 편집 흐름의 마지막 단계를 이렇게 정한다. "24시간 dry-run은 후보 firing 구간과 데이터 coverage를 보여주며 실제 미래 알림 횟수 예측이라고 주장하지 않는다."

ADR 0050은 평가 의미를 순수 함수(`internal/alerting`)로 두었고, ADR 0051은 그것을 주기 평가로 실행했다. 명세가 정하지 않은 것이 있다.
- 24시간을 어떻게 다시 평가하는가(평가 시점마다 조회하면 1분 주기에 1,440번이다)
- 누구의 권한으로 읽는가, control-api가 분석 저장소를 읽는가
- 결과 모양과 한도
- 하지 못했을 때(저장소 없음·실패·너무 큼) 무엇을 돌려주는가

## 결정

### 1. 계산 (`alerting.PlanDryRun`, `alerting.DryRun`)

- **평가 시점:** 마지막 시점은 주기 평가의 window 끝과 같다(min(지금, watermark) 분 경계, ADR 0050 §1). 그로부터 24시간 전까지 평가 주기 간격으로 둔다.
  - 1분 주기는 1,440시점, 5분 주기는 288시점이다.
  - 30초 주기는 1분 간격이다. 1분 rollup을 다시 읽는 반복 평가는 상태를 바꾸지 않는다(ADR 0050).
- **조회:** 첫 시점의 window 시작부터 마지막 시점까지 **1분 step으로 한 번** 조회한다. group_by·filter·error_ratio의 status group은 주기 평가와 같다. 한 조회의 점 수 상한(2,000)을 넘으면 기간을 나눠 조회한다(긴 window·warm-up).
  - **실행 예산(리뷰 반영):** query 계정 profile의 interactive 기본값은 결과 10,000행·5초다(ADR 0018). 1분 step 24시간은 series 하나가 약 1,440행이라, 기본값으로는 series 7개(error_ratio는 status별 series)를 넘지 못한다.
  - 그래서 dry-run 조회는 `max_result_rows` 200,000, `max_execution_time` 10초를 요청 설정으로 싣는다(`telemetrystore.Budget`). profile의 MAX(1,000,000행·60초) 안이다.
  - 예산 없이 같은 조회가 저장소에서 거절되는 것도 통합 시험으로 확인했다.
  - `max_execution_time`은 초 단위로 올림하고 [1, 60]으로 고정한다. 0은 무제한이고, 60을 넘으면 profile 위반이다.
  - warm-up이 길면 조회 범위가 최대 72시간(24시간 + warm-up 24시간 + window 24시간)이 된다. series당 행이 3배라 bucket 200,000 상한 안의 series 수가 그만큼 준다.
- **window 값:** 시점마다 window 안의 1분 bucket을 `MergeBuckets`로 합쳐 주기 평가와 같은 `Compute`·`Evaluate`를 돌린다.
  - 합(증가량·count·hist_sum·total·samples)은 더한다. min·max는 값이 있는 bucket끼리 고른다.
  - histogram bucket은 경계가 모두 같을 때만 원소별로 더한다. 다르면 `bounds_mismatch`다(계약 5).
  - **partial:** 분마다 stream 집합의 지문(`StreamSet`, stream_id hash의 XOR)을 받는다. window 안에서 집합이 다르면 어느 분에 빠진 stream이 있다는 뜻이다(pod 교체 포함).
    - 이때 합친 Streams를 하나 늘려 저장소 window 조회와 같은 규칙("stream 수 × 분 수보다 window가 적음", `metricvalue.MissingWindows`)으로 partial이 되게 한다.
    - 그래서 증가량·histogram 집계에서만 partial이고, gauge 값 통계(avg·min·max)는 저장소 조회처럼 partial이 아니다(리뷰 반영).
  - **값·사유·partial은 저장소의 window 조회와 같다.** 실제 ClickHouse에서 같은 데이터로 window 조회와 비교하는 통합 시험으로 고정했다. 대상은 다음과 같다.
    - error_ratio 서비스 20개
    - histogram p50·p95·count(stream 교체, 경계가 다른 분)
    - gauge avg·min·max(stream 둘, stream 교체)
- **warm-up(리뷰 반영):** 결과 범위 앞에 for_seconds와 no_data `after_seconds` 중 큰 값만큼 시점을 더 평가한다. 이 구간에서는 상태만 만든다.
  - 처음 상태를 OK로 두면 for_seconds 동안 발화할 수 없어서 "발화 없음"처럼 보인다(for_seconds 최대 24시간).
  - warm-up 중에 열린 사건이 범위로 이어지면 구간 시작은 실제로 열린 시각이다(범위 `from`보다 이를 수 있다). 발화 시간은 범위 안만 센다.
- **watermark**
  - watermark가 없으면 평가하지 않는다(`reason: no_watermark`).
  - 10분 넘게 멈췄으면 watermark까지만 평가하고 `reason: stale_watermark`를 붙인다. 주기 평가라면 지금은 결측이다.
- 24시간 동안 series가 하나도 없으면 `reason: no_data`다. "발화 없음"과 구분한다(계약 6).
- group 값을 이어 붙인 key는 길이를 앞에 붙인다. label 값에 구분 문자가 있어도 다른 group이 섞이지 않는다.

### 2. 결과 모양 (`data.dry_run`)

```
{ from, to, warmup_seconds, data_until, step_seconds, evaluations, reason,
  coverage: { evaluated, no_data, error },                 // monitor 수준 평가 결과 분포
  groups: [ { group_key, labels, firing_intervals: [{start, end|null, reason}], firing_intervals_omitted, firing_seconds,
              final_state, evaluated, no_data, partial, first_value_at, max_value, last_value, last_reason, transitions } ],
  groups_total, groups_omitted }
```

- **후보 발화 구간:** 사건이 열린 시점부터 닫힌 시점까지다(ADR 0050 §4). 끝까지 열려 있으면 `end: null`이다. reason은 `violation` 또는 `no_data`(no_data alert 정책)다.
  - group당 50개까지 담고 나머지는 `firing_intervals_omitted`로 센다. flapping이면 수백 개가 되어 응답이 수 MB가 된다. 발화 시간은 생략한 구간도 포함한다.
- **coverage**
  - monitor 수준은 평가된 시점, 결측(window 없음), 오류(group 상한 초과)의 수다.
  - group 수준은 값이 있던 시점, 결측(최소 요청 미달 포함), partial의 수다.
  - 결측과 오류는 "발화 없음"과 섞지 않는다.
- **max_value:** 기간 중 가장 큰 값이다. 임계값을 고를 때 쓴다.
- **group 순서·상한:** 발화 시간이 긴 순서다. 최대 100개를 담고, 나머지는 `groups_omitted`로 센다.

### 3. 권한과 경로

- control-api가 **요청자 principal**로 ClickHouse query 계정을 읽는다.
  - row policy·mandatory predicate·environment 제한 경로는 query-api와 같다(ADR 0018, 계약 1·4).
  - system principal을 쓰지 않는다. 사람이 보는 결과라 요청자가 볼 수 있는 데이터만 보여야 한다.
  - environment 제한 key는 validate 자체가 403이다(ADR 0051 §3).
- `telemetry.read`가 없으면(`monitors.write`만 있는 key) dry-run을 하지 않는다.
- control-api에 `MONTRACER_CH_QUERY_DSN`(선택)을 둔다.
  - 없으면 dry-run을 하지 않는다. 이것이 rollback flag다.
  - query 계정은 읽기 전용이다(ADR 0018).
  - **ClickHouse는 선택 의존이다(리뷰 반영).** 기동 때 닿지 않아도 control-api는 뜬다. 감사·monitor API는 PostgreSQL만 쓴다.
    - dry-run을 요청할 때 다시 연결한다(시도당 2초 상한). 실패한 뒤 30초 동안은 다시 시도하지 않고 `dry_run_failed`를 준다.
    - DSN 형식 오류와 읽기 전용이 아닌 계정은 설정 오류라 기동을 거부한다.
- **CPU 보호(리뷰 반영):** 동시 dry-run은 4개다. 넘으면 기다리지 않고 `dry_run_busy`를 준다.
  - 재평가는 시간 상한(10초) context를 64시점마다 확인한다.
  - 합치기 1천만 회 상한은 실측 약 1초다.

### 4. 하지 못했을 때

validate는 **200**이다. spec은 유효하므로 정의 검증 결과는 그대로 준다. `dry_run`은 `null`이고, 이유가 warnings에 온다.

| 경고 | 뜻 |
|---|---|
| `dry_run_unavailable` | 이 배포에 분석 저장소 조회가 없다 |
| `dry_run_forbidden` | 요청자에게 `telemetry.read`가 없다 |
| `dry_run_failed` | 조회 실패·시간 초과(10초)·ClickHouse 연결 안 됨 |
| `dry_run_too_large` | 1분 bucket 200,000개 초과(저장소가 `max_result_rows`로 거절하거나 받은 수 합계), 합치기 1천만 회 초과, 또는 다른 저장소 예산 초과 |
| `dry_run_busy` | 동시 dry-run 4개 초과. 잠시 뒤 다시 validate |

- 지표: `montracer_control_monitor_dry_run_total{outcome=ok|unavailable|forbidden|failed|too_large|busy}`, `montracer_control_monitor_dry_run_duration_seconds`. 실패는 로그 `monitor dry-run failed`(단계, 오류)다. 경보는 두지 않는다. dry-run 실패는 정의 저장·평가를 막지 않는다.

- `dry_run_unavailable`은 이제 `monitor.Normalize`가 늘 붙이는 경고가 아니다. validate가 dry-run을 못 했을 때만 붙는다. 생성(`POST /monitors`)은 dry-run을 하지 않으므로 dry-run 경고가 없다.
- warnings는 경고가 없어도 `null`이 아니라 빈 목록이다.

## 외부 사례 근거 (2026-10-10 확인)

| 사례 | 내용 | 반영 |
|---|---|---|
| Datadog monitor 설정 — Preview graphs ([docs](https://docs.datadoghq.com/monitors/configuration/)) | "Evaluated Data" 탭은 지금의 query·임계값으로 과거 데이터를 어떻게 평가했을지와 과거 상태 전이(OK → ALERT)를 보여준다 | 결정 1·2: 같은 평가 함수로 과거를 다시 평가하고 사건 구간·전이 수를 보여준다 |
| Grafana alert rule 만들기 ([docs](https://grafana.com/docs/grafana/latest/alerting/alerting-rules/create-grafana-managed-rule/)) | 저장 전 Preview로 query·조건을 확인한다 | 결정 4: 미리보기가 실패해도 정의 검증과 분리한다(validate 200) |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 조회 방식 | 1분 bucket 한 번 + window마다 합침 | 시점마다 window 조회: 1,440번. offset별 step=window 조회: window 길이만큼(24시간 window는 1,440번) |
| 평가 함수 | 주기 평가와 같은 Compute·Evaluate | dry-run 전용 근사: 미리보기와 실제 경보가 달라진다 |
| 권한 | 요청자 principal | system principal: 요청자가 못 보는 데이터가 결과에 섞일 수 있다 |
| 실패 응답 | validate 200, dry_run null + 이유 | 5xx: 유효한 정의의 검증까지 막는다. 빈 결과: "발화 없음"처럼 보인다(계약 6) |
| 실행 위치 | control-api(endpoint가 있는 곳) | query-api 내부 호출: 서비스 간 인증·경로가 새로 생긴다 |

## 결과

- validate가 24시간 dry-run을 돌려준다. D02 §14 "dry-run 검증 후 저장"은 화면 흐름(S09, 단계 D)이 validate → 생성 순서로 지킨다. 생성 API 자체는 dry-run을 요구하지 않는다(ADR 0049와 같다).
- **API 변화:** `data.dry_run`이 객체가 될 수 있다. warnings는 빈 목록이 될 수 있고, `dry_run_unavailable`이 늘 오지는 않는다. 생성 응답의 meta.warnings에 더는 `dry_run_unavailable`이 없다.
- **control-api 의존성:** ClickHouse query 계정(선택)
- schema·tenant 데이터 변경 없음. retention 영향 없음.

## Rollback

control-api의 `MONTRACER_CH_QUERY_DSN`을 비우면 dry-run이 꺼진다(`dry_run: null`, `dry_run_unavailable`). 코드를 되돌려도 저장 데이터에 영향이 없다.

## 재검토 조건

- ADR 0050 §4에서 남긴 "metric kind에서 group 하나가 bucket 여러 개" 경우는 dry-run에서 생기지 않는다. 합친 뒤 group마다 bucket이 하나이기 때문이다. 주기 평가도 같은 합치기 경로로 옮겨 두 경로를 하나로 만들지는 운영 데이터를 보고 정한다.
- 실제 tenant의 series 수에서 응답 시간(10초 한도)이나 예산(bucket 200,000 — error_ratio 기준 status 포함 series 약 130개)이 자주 막힐 때(`too_large`·`failed` 지표): 조회를 group 상위 N개로 좁히거나 1시간 rollup을 쓴다.
- error_ratio의 status별 series가 예산을 자주 넘으면: 저장소에서 status를 5xx·기타·없음으로 접어 series를 줄인다.
- S09 화면(단계 D)이 결과 모양을 쓰면서 바뀌는 필드

## 증거

- `internal/alerting/dryrun_test.go`
  - 계획
    - 마지막 시점 = min(지금, watermark) 분 경계, 1,440·288시점, 30초 → 1분
    - warm-up 시점(for·no_data after 중 큰 값), 조회에 예산을 싣는다
    - 긴 window·warm-up은 빈틈 없이 나눠 조회한다
    - watermark 없음·멈춤
  - 합치기
    - 합·min·max·last·histogram 원소, 입력은 바뀌지 않는다
    - 경계가 다르면 bounds_mismatch, 빠진 분과 stream 집합 변화는 partial
    - group key는 섞이지 않는다
  - 24시간 중 한 시간 5%
    - 사건 하나다. 시작은 window 오류율이 2%를 넘은 시점 + for 120초, 끝은 2회 연속 정상이다.
    - 정확히 2%인 group은 발화하지 않는다.
    - 최소 요청 미달 group은 결측 1,440이고, 끊긴 뒤에는 group_missing이다.
  - warm-up
    - for 1시간 계속 위반이면 범위 첫 시점부터 발화한다.
    - warm-up 중에 열린 사건은 시작이 범위 앞이고, 발화 시간은 범위 안만 센다.
  - flapping 구간 50개 상한·생략 수·발화 시간 포함, 취소된 context, watermark 없음 → no_watermark, series 없음 → no_data, group 100개 상한
- `internal/controlapi/dryrun_test.go`
  - 성공: 요청자 principal로 조회, 열린 사건, dry-run 경고 없음
  - 저장소 없음·조회 실패·예산·telemetry.read 없음 → null + 각 경고
  - 동시 상한 → `dry_run_busy`, outcome별 계수
- `cmd/control-api/lazyquery_test.go`: ClickHouse가 닿지 않으면 오류를 돌려주고 30초 안에는 다시 시도하지 않으며, 그 뒤 다시 시도한다. 읽기 전용이 아닌 계정은 오류다.
- `internal/alertworker/integration_test.go` `TestDryRunMatchesWindowQuery`(실제 ClickHouse)
  - 1분 bucket을 합친 결과와 window 조회의 값·사유·partial이 같다
    - error_ratio 서비스 20개(12,000행)
    - histogram p50·p95·count: stream 교체 → 양쪽 모두 partial, 경계가 다른 분 → 양쪽 모두 bounds_mismatch
    - gauge avg·min·max
  - 마지막 dry-run 시점이 주기 평가 window와 같다. 2% 이하 서비스는 발화하지 않는다.
  - 예산 없는 같은 조회는 저장소가 거절한다.
- `tests/isolation` `TestMonitorDryRunAcrossTenants`: 같은 서비스·시간대에 A는 오류 10%, B는 0%다. B의 dry-run은 max_value 0이고 발화하지 않는다(tenant header 무시). A는 같은 정의로 발화한다(대조군).
- 계산 시간 실측(로컬 개발 기기, 1회, ClickHouse 조회 제외)
  - 1분 bucket 173,280개(series 120, 5분 window, 합치기 86만): 0.6초
  - series 40·1시간 window(합치기 346만): 0.34초
  - bucket 398,544개: 1.5초
  - 예산(bucket 200,000·합치기 1천만)은 이 측정으로 정했다. 실제 조회 시간은 `montracer_control_monitor_dry_run_duration_seconds`로 본다.
- spec-reviewer
  - P1 2건 반영: interactive 결과 행 예산으로 series 7개 초과 시 늘 실패, ClickHouse 장애 시 control-api 기동 실패
  - P2 7건 반영: warm-up, stream 교체 partial, group key 충돌, CPU·응답 크기, 비교 시험 범위, 지표, 문서
  - 재검토 P2 3건 반영: gauge 교체를 partial로 표시, 1초 미만 실행 예산이 무제한이 됨, 연결 시도가 lock을 오래 잡음
