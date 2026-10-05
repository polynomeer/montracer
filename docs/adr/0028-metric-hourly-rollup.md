# ADR 0028: metric 1시간 rollup과 조회 해상도 자동 선택

- 상태: 승인 (결정 위임)
- Owner: Data lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021~0027과 같은 위임). 근거는 §5
- 날짜: 2026-10-05 (제안·결정)
- 관련: F04, F05, E03 · D02 §07, §10 · ADR 0025, 0026, 0027

## 배경

D02 §10은 `metric_1h`(1시간, 395일, 장기 추세)를 둔다. 지금 조회는 항상 `metric_1m`만 읽는다. 그래서 7일·1시간 step 조회도 stream마다 10,080행을 읽는다. 또 `metric_1m`(90일)보다 오래된 구간은 조회할 수 없다.

## 결정

### 1. 계산: 원본에서 1시간 window를 직접 계산

- `metric_1h`는 `metric_1m`과 같은 구조다(migration 00005). TTL은 `window_start + 395일`이고 partition은 월 단위다.
- rollup job을 **window 1시간**으로 하나 더 돌린다. 계산은 ADR 0025, 진행·watermark·revision은 ADR 0026과 같다.
  - 재계산 범위는 마지막 1시간 window다. watermark가 다음 시간 경계를 넘기 전까지는 그 시간을 다시 계산한다. 따라서 10분 안에 늦게 온 point도 반영된다.
  - 따라잡기 상한은 24시간, 주기는 2분이다. **기준점 조회 범위는 1시간**이다. 10~60분마다 보내는 cumulative stream도 직전 시간의 point를 기준점으로 쓴다.
  - 같은 worker의 `rollup` 역할 process가 1분 job과 함께 돌린다. cluster에 하나만 둔다.
- **`metric_1m`을 다시 합치지 않는다.** 원본에서 직접 계산한다.
  - 1분 window 60개를 합치려면 histogram 차분·reset·기준점 의미를 한 번 더 구현해야 한다. 원본에서 같은 함수로 계산하면 두 해상도의 의미가 정의상 같다.
  - 대가는 원본 읽기량이다. 2분마다 최근 약 1시간 10분의 원본을 읽는다.
- rollup 계정 권한과 정책은 `metric_1m`과 같다: 쓰기, 진행 위치 3컬럼 읽기, `USING 1` 정책.

### 2. 조회 해상도 자동 선택

- **`step_seconds`가 3600의 배수면 `metric_1h`, 아니면 `metric_1m`을 읽는다.** 사용자가 해상도를 고르지 않는다.
- **단, `metric_1h`가 조회 범위 시작을 덮지 못하면 `metric_1m`을 읽는다.** 덮는지 여부는 그 tenant의 가장 이른 `metric_1h` window로 판단한다(리뷰에서 발견).
  - 1시간 rollup은 배포 시점부터만 채워진다. 이 규칙이 없으면 배포 직후 7일 dashboard가 1분 rollup에는 있는 과거 시간을 `no_data`로 보인다(계약 6 위반).
  - 조회 범위는 최대 7일이고 `metric_1m`은 90일이라, 대신 읽을 수 있다.
- 결과 의미는 같다(같은 계산의 다른 window). 1시간 step 이상에서 읽는 행 수만 60배 줄어든다.
- **window 누락 판정과 watermark는 읽은 해상도 기준이다.**
  - 기대 window 수 = stream 수 × step / window. 예: 2시간 step은 1시간 window 2개다.
  - `meta.watermark`는 그 해상도 rollup의 진행 위치다.
- 90일보다 오래된 구간은 1시간 step(7일 interactive 한도 안)으로 조회한다. 장기 범위(7일 초과)는 비동기 query job(D02 §19)이 맡는다.

### 3. 운영

- rollup 지표에 `resolution`(1m·1h) label을 둔다.
- 정체 경보 `MontracerMetricRollupStalled`는 **해상도별로** 평가한다(`max by (resolution)`). 1분 job의 성공이 1시간 job의 정체를 가리지 않게 하기 위해서다.
- 마지막 성공 시각 series는 job을 만들 때 0으로 미리 생성한다. 기동부터 계속 실패해도 series가 없어 경보가 침묵하는 일을 막는다(리뷰에서 발견).
- **schema 유지:** `metric_1h`는 `metric_1m`의 구조를 복사했다. 이후 `metric_1m`에 컬럼을 추가하는 migration은 `metric_1h`에도 같이 적용한다.

### 4. 아직 없는 것

- 7일을 넘는 장기 범위 조회: 비동기 query job
- 5분 해상도

### 5. 외부 사례 근거 (2026-10-05 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| §1 1시간 해상도 장기 보존 | Thanos downsampling ([Exoscale 해설](https://community.exoscale.com/product/dbaas/service-specific/thanos/operation/retention-downsampling), [shoulder.dev](https://shoulder.dev/github.com/thanos-io/thanos/learn/downsampling)) | compactor가 raw를 5분·1시간 해상도로 다시 써서 긴 범위 조회를 빠르게 한다. 해상도마다 보존을 다르게 둔다 | 채택: 1시간 395일(D02 §10) |
| §2 해상도 자동 선택 | Thanos `max_source_resolution=auto` (위) | query step에 맞춰 raw·5m·1h 중 적절한 해상도를 자동 선택 | 채택: step이 1시간의 배수면 1시간 |

- **사례와 다르게 한 것:** Thanos는 downsampled block에 count·sum·min·max·counter 집계(AggrChunk)를 따로 둔다. 이 제품은 rollup 행에 ADR 0025 집계 상태를 그대로 두어 두 해상도가 같은 질의 경로를 쓴다.

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 1시간 계산 원천 | 원본 | metric_1m 재집계: 차분·reset·기준점 의미를 두 번 구현하게 된다. 1분 단계의 partial이 1시간에 섞인다 |
| 해상도 선택 | step 배수로 자동 | 사용자 지정: QuerySpec에 저장 구조가 드러난다. 범위 길이 기준: 같은 범위에서 step만 바꿔도 결과가 갈리는 경계가 생긴다 |

## 결과

- 1시간 step 이상의 dashboard 조회가 가벼워진다. 90일 이후 구간도 1시간 해상도로 395일까지 조회할 수 있다.
- **rollup process 부하가 늘어난다(원본 읽기).** 부하 시험(D06 §03, 100k series)에서 두 job의 주기 실행 시간을 함께 본다.

## Rollback

- 1시간 job을 빼는 배포로 되돌린다. 그러면 조회는 1시간 step에서도 빈 결과와 `pending`을 낸다. 그러니 query-api도 함께 되돌리거나, 해상도 선택을 1분으로 고정한다.
- migration 00005 down은 `metric_1h`와 정책을 지운다.

## 재검토 조건

- 1시간 job의 원본 읽기가 주기에 가까울 때. 이때는 `metric_1m`에서 재집계하거나 5분 해상도를 중간 단계로 도입한다.
- 7일 초과 interactive 조회 요구가 있을 때.

## 증거

- `internal/rollup`
  - 1시간 window: 닫힌 시간만 쓴다. lookback 기준점을 쓴다. 395일 TTL이다. 다음 시간 경계 전의 늦은 point는 그 시간에 반영된다.
  - 통합 테스트: 원본 → `metric_1h` 증가량 50
- `internal/query`
  - 2시간 step은 1시간 해상도 window 2개, rate 1/s, partial 아님
  - **1시간 rollup이 범위 시작을 덮지 못하면 1분 rollup을 읽는다**
- `internal/opsmetrics`: 해상도별 마지막 성공 series를 미리 만든다.
- `internal/telemetrystore` 통합 테스트: 1시간 step은 `metric_1h`만 읽는다(같은 시간의 `metric_1m` 행 무시). 1시간 watermark.
- promtool: 1h만 정체하고 1m이 진행 중이면 1h 경보만 울린다. 1h가 한 번도 성공하지 못해도 울린다.
- spec-reviewer 지적 반영: 배포 직후 1시간 해상도 공백, 경보 series 부재, 1시간 기준점 범위, 테이블 목록 단일화
