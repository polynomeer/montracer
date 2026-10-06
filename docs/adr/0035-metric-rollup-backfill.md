# ADR 0035: metric rollup backfill job

- 상태: 승인 (결정 위임)
- Owner: Data lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021~0034와 같은 위임). 근거는 §5
- 날짜: 2026-10-06 (제안·결정)
- 관련: D02 §05(replay 제한), §07("10분 이후는 backfill job만 허용"), §21 · ADR 0025, 0026, 0028

## 배경

live rollup(ADR 0026·0028)은 tenant마다 `watermark − 10분`(1h는 1시간)만 다시 계산한다. 그래서 다음 구간은 원본에는 저장됐지만(ACK·저장 완료) metric 조회(rollup 테이블)에는 나오지 않는다.

- **10분 넘게 늦게 온 point:** 수집 허용 범위가 과거 24시간이라(D02 §07) 정상적으로 들어온다.
- **처음 보는 tenant의 과거 구간:** 첫 계산이 `watermark − 10분`부터다.
- **따라잡기 상한(1시간)을 넘은 gap:** rollup 정체 뒤(RB01 `MontracerMetricRollupStalled`) 남는 구간이다.

D02 §07은 "10분 이후는 backfill job만 허용"이라고 정하지만 job이 없었다. `make seed` 검증(PR #29)에서 처음 보는 tenant 구간이 비는 것으로 확인했다.

## 결정

### 1. `rollup.Backfill` (`internal/rollup/backfill.go`)

- tenant 하나의 `[from, to)`를 window 경계로 넓혀 맞추고, 원본(dedup된 `metric_points`)에서 **live job과 같은 계산**(`Job.compute`)으로 다시 쓴다.
- **설정은 live job과 같다.** `cmd/worker`의 `minuteConfig`·`hourConfig`가 단일 원천이다(Window, BaselineLookback, Retention). cumulative 기준점은 chunk 경계에서도 lookback으로 찾는다.
- **revision**은 저장된 최대값보다 크다(`max(now, MaxRevision+1)`). 이후 live job이 같은 window를 다시 쓰면 그쪽이 더 크다.
- **값 없는 window는 쓰지 않는다**(계약 6). 첫 cumulative point만 있는 window는 `missing_baseline`이다(ADR 0025 §3).
- **읽기는 tenant로 한정한다**(`ReadTenantPoints`, bound parameter). 다른 tenant의 원본을 읽지도 쓰지도 않는다.

### 2. 허용 범위

| 조건 | 이유 |
|---|---|
| `to` ≤ 현재 window 시작 | 닫히지 않은 window는 live job 몫이다. 쓰다 만 window를 확정값처럼 만들지 않는다 |
| `from` ≥ now − 원본 보존(15일, D04 §04) | 원본이 없으면 다시 만들 근거가 없다 |
| tenant는 UUID | 입력 검증 |

`--resolution all`이면 1h는 닫힌 시간까지만 계산한다(그보다 뒤는 생략하고 기록한다).

### 3. 실행 (`worker backfill`)

```
worker backfill --tenant UUID --from RFC3339 --to RFC3339 [--resolution 1m|1h|all] [--job-id ID]
```

- worker binary의 일회성 subcommand다. rollup 계정(`MONTRACER_CH_ROLLUP_DSN`)만 쓴다(ADR 0026 최소 권한).
- **chunk**(window 60개: 1m은 1시간, 1h는 60시간)마다 읽고 쓰고, chunk 사이에 200ms 쉰다. D02 §05 "live 처리 여유의 20%만" 원칙을 단순 pacing으로 지킨다. 부하 시험에서 측정해 조정한다.
- **job_id**로 시작·종료·chunk·point·window 수를 구조화 로그에 남긴다.
- **멱등이다.** 같은 범위를 다시 돌리면 같은 값을 새 revision으로 쓴다(시험으로 고정). 그래서 조회 결과는 바뀌지 않는다.
- **이미 발송된 경보는 자동 취소하지 않는다**(D02 §07). 경보 평가가 생기면 그쪽 규칙이다.

### 4. 하지 않는 것

- **자동 실행:** live job이 gap을 기록할 때(`Gaps`, 로그 `backfill required`) backfill을 자동으로 띄우지 않는다. 운영자가 범위를 확인하고 돌린다(RB01). 자동화는 job 원장(D02 §05: job_id·대상·정책 버전·예상량)과 함께 만든다.
- **10분 넘게 늦은 point 감지 지표:** 원본 행에 수신 시각을 읽어 "rollup 밖 늦은 point" 수를 세는 지표는 아직 없다. 지금은 tenant·고객 신고나 gap 로그로 판단한다.
- **처음 보는 tenant의 자동 과거 계산:** ADR 0026의 첫 계산 범위(`watermark − 10분`)는 그대로 두고 backfill로 채운다.
- **원본 보존(15일)보다 오래된 구간:** 다시 만들 수 없다. 이미 쓴 rollup(90일·395일)은 유지된다.

### 5. 외부 사례 근거 (2026-10-06 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| 집계(파생) 데이터의 과거 구간은 명시적 운영자 명령으로 다시 만든다 | Prometheus storage — Backfilling for Recording Rules ([문서](https://prometheus.io/docs/prometheus/latest/storage/)) | `promtool tsdb create-blocks-from rules --start --end`가 recording rule을 과거 구간에 대해 다시 평가해 block을 만든다. alert는 평가하지 않는다. 겹치는 범위를 다시 돌리면 중복 block이 생긴다 | 채택: 명시적 범위 명령, 경보 자동 취소 없음. 다르게: revision·내용 기반 dedup으로 재실행이 중복을 만들지 않는다 |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 실행 위치 | worker subcommand | `montracer-admin`: 제어 DB 계정만 가진 break-glass 도구에 분석 저장소 쓰기 권한을 더하게 된다. 별도 binary: rollup 설정을 두 곳에 둬야 한다 |
| live job 확장(자동) | 하지 않음 | 첫 계산 범위를 넓히면 tenant가 늘 때 첫 cycle이 커지고, 늦은 point를 매번 찾으려면 원본 전체 scan이 필요하다 |
| 계산 | live와 같은 `compute` | 별도 SQL 집계: 같은 의미를 두 구현으로 유지하게 된다(percentile·reset 처리 불일치 위험) |

## Rollback

- 실행하지 않으면 된다. 쓴 행은 이후 live job·backfill의 더 큰 revision이 덮는다.
- 잘못 쓴 범위는 같은 범위 backfill을 다시 돌려 원본 기준 값으로 되돌린다.

## 증거

- `internal/rollup` 단위
  - 3시간 전 구간을 tenant 하나만 채운다. chunk 경계 window도 기준점으로 증가량을 계산한다. 첫 window는 `missing_baseline`이다.
  - 값 없는 window는 쓰지 않는다. 1h 경계로 맞춘다.
  - 범위 밖(역순·열린 window·미래·원본 보존 밖·UUID 아님)은 쓰지 않고 거절한다. 쓰기 실패 시 멈춘다.
- `internal/rollup` 통합(ClickHouse, rollup 계정): 5개 window 증가량 합 35, 다른 tenant 행 0, 재실행 뒤 같은 결과.
