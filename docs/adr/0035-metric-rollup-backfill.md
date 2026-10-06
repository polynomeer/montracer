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
- **설정은 live job과 같다.** `cmd/worker`의 `minuteConfig`·`hourConfig`가 단일 원천이다(Window, MaxLateness, Recompute, BaselineLookback, Retention).
- **cumulative 기준점은 window 앞 `BaselineLookback` 안의 마지막 point만 쓴다(리뷰에서 발견, live job도 함께 고침).**
  - 이전 `compute`는 읽기 시작점만 lookback으로 정했다. 그래서 계산 범위 뒤쪽 window는 더 오래된 point를 기준점으로 썼다.
  - 결과적으로 같은 window 값이 계산 범위의 시작 위치(live cycle·backfill chunk)에 따라 달랐다.
  - 이제 `Config.BaselineLookback` 문서와 ADR 0025 §3대로, 더 드문 stream은 `missing_baseline`이다. chunk 크기가 달라도 같은 값이다(시험).
- **revision**은 저장된 최대값보다 크다(`max(now, MaxRevision+1)`).
  - backfill 범위는 live 재계산 구간과 겹치지 않는다(§2). 그래서 같은 window를 두 process가 다투지 않는다.
  - 두 process의 시계 차이나 live job의 내용 해시 캐시(`j.last`)에 결과가 기대지 않는다.
- **값 없는 window는 쓰지 않는다**(계약 6). 첫 cumulative point만 있는 window는 `missing_baseline`이다(ADR 0025 §3).
- **읽기는 tenant로 한정한다**(`ReadTenantPoints`). 다른 tenant의 원본은 읽지도 쓰지도 않는다.
  - tenant는 UUID로 검증하고 정규 문자열로 바꾼다. 대문자 UUID가 조용히 0 window로 끝나지 않게 한다.
  - tenant 조건은 별도 고정 SQL 조각(`tenant_id = toUUID($3)`)이고 값은 bound parameter다. 잘못된 값이 zero UUID나 전체 읽기로 바뀌지 않는다.

### 2. 허용 범위

| 조건 | 이유 |
|---|---|
| `to` ≤ `floor(now − MaxLateness) − Recompute` (1m: 약 12분 전, 1h: 직전 닫힌 시간의 1시간 전) | 그 뒤는 live job이 다시 계산한다. 겹치면 두 process가 같은 window를 다투고, 아직 도착하지 않은 point가 빠진 값을 확정값처럼 쓸 수 있다(리뷰에서 발견) |
| `from` ≥ now − 원본 보존 + `BaselineLookback` + 1시간 | 보존 경계에서는 기준점 lookback의 원본이 이미 만료돼 있어, live가 정상 계산한 window를 더 나쁜 값으로 덮을 수 있다. 보존 기간은 수집 쪽 단일 원천(`pipeline.DefaultRetention.Metrics`, 15일)을 쓴다 |
| tenant는 UUID | 입력 검증 |

- **해상도 처리:** 실행 전에 모든 해상도의 범위를 검증한다. 1m을 쓴 뒤 1h에서 실패하지 않는다.
  - `--resolution all`이면 1h는 live 1h 재계산 구간 앞까지로 줄인다.
  - 닫힌 시간이 없으면 1h를 건너뛰고 기록한다.
  - 1m은 줄이지 않고 오류를 낸다.

### 3. 실행 (`worker backfill`)

```
worker backfill --tenant UUID --from RFC3339 --to RFC3339 [--resolution 1m|1h|all] [--job-id ID]
```

- worker binary의 일회성 subcommand다. rollup 계정(`MONTRACER_CH_ROLLUP_DSN`)만 쓴다(ADR 0026 최소 권한).
- **chunk 크기:** 1m은 window 60개(1시간), 1h는 window 1개(원본 2시간 분량)다.
- **point 상한:** chunk 하나에서 읽는 원본 point를 200만 개로 제한한다. 넘으면 쓰지 않고 멈춘다(`ErrBackfillTooLarge`, 범위를 나눈다).
- **pacing:** chunk 사이에 200ms 쉰다. **이것으로 D02 §05 "live 처리 여유의 20%만"을 보장하지는 못한다.** chunk 읽기 자체가 수 초 걸릴 수 있다. 부하 시험에서 측정해 조정한다(§4).
- **job_id**로 시작·chunk별 진행(`done_to`, point·window 수)·종료를 구조화 로그에 남긴다. 실패하면 오류에 마지막 완료 지점이 있어 그 지점부터 다시 돌린다.
- **멱등이다.** 원본이 그대로면 같은 범위 재실행은 같은 값을 새 revision으로 쓴다. chunk 크기와 무관하다(시험).
- **이미 발송된 경보는 자동 취소하지 않는다**(D02 §07). 경보 평가가 생기면 그쪽 규칙이다.

### 4. 하지 않는 것

- **자동 실행:** live job이 gap을 기록할 때(`Gaps`, 로그 `backfill required`) backfill을 자동으로 띄우지 않는다. 운영자가 범위를 확인하고 돌린다(RB01). 자동화는 job 원장(D02 §05: job_id·대상·정책 버전·예상량)과 함께 만든다.
- **10분 넘게 늦은 point 감지 지표:** 원본 행에 수신 시각을 읽어 "rollup 밖 늦은 point" 수를 세는 지표는 아직 없다. 지금은 tenant·고객 신고나 gap 로그로 판단한다.
- **처음 보는 tenant의 자동 과거 계산:** ADR 0026의 첫 계산 범위(`watermark − 10분`)는 그대로 두고 backfill로 채운다.
- **원본 보존(15일)보다 오래된 구간:** 다시 만들 수 없다. 이미 쓴 rollup(90일·395일)은 유지된다.
- **운영 지표:** backfill 전용 지표는 없고 로그만 있다. 자동 실행을 만들 때 함께 만든다.
- **delete epoch·tombstone 미적용:** D02 §18은 "backfill은 quota와 delete epoch를 적용"이라고 한다. 삭제 원장(F09)이 아직 없어 지금 위반은 아니다. 다만 삭제 job이 생기면 그 전에 backfill이 삭제 epoch를 적용해야 한다. 그때까지 **삭제 대상 tenant·범위에는 backfill을 돌리지 않는다**(RB01).
- **큰 tenant의 1h backfill:** 1h window 하나(원본 2시간)가 point 상한을 넘으면 돌릴 수 없다. 원본을 stream 단위로 나눠 읽는 방식(streaming)이 필요하다.
- **`LoadProgress` 비용:** 해상도마다 rollup 테이블의 최대 revision을 한 번 읽는다. 큰 테이블에서의 비용은 부하 시험에서 본다.

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
  - 3시간 전 구간을 tenant 하나만 채운다. chunk 경계 window도 lookback 안 기준점으로 증가량을 계산한다. 첫 window는 `missing_baseline`이다.
  - **chunk 7과 60의 결과가 같다**(15분 주기 cumulative stream, lookback 밖 기준점 미사용).
  - 값 없는 window는 쓰지 않는다. 1h 경계로 맞춘다. 대문자 UUID도 정규화해 쓴다.
  - 범위 밖은 쓰지 않고 거절한다: 역순, 열린 window, 미래, live 재계산 구간, 원본 보존 여유 밖, UUID 아님.
  - point 상한을 넘으면 쓰지 않는다. 쓰기 실패 시 멈춘다. live 재계산 시작 시각(1m·1h).
- `cmd/worker` 단위: 인자 오류, all의 1h clamp·skip, 1m 범위 오류 시 아무 해상도도 실행하지 않음.
- live rollup 기존 시험은 기준점 나이 제한 뒤에도 통과한다.
- spec-reviewer 지적 반영: 기준점 나이 제한(live 포함), live 구간과 겹침 금지, chunk 메모리 상한, tenant 정규화·검증, 보존 단일 원천·경계 여유, 실행 전 전체 검증, pacing 문구·진행 로그, delete epoch 공백, 근거 문구
- `internal/rollup` 통합(ClickHouse, rollup 계정): 5개 window 증가량 합 35, 다른 tenant 행 0, 재실행 뒤 같은 결과.
