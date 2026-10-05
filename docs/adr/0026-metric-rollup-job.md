# ADR 0026: metric 1분 rollup job — watermark, 재계산, 계정, 단일 실행

- 상태: 승인 (결정 위임)
- Owner: Data lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021~0025와 같은 위임). 근거는 §6
- 날짜: 2026-10-05 (제안·결정)
- 관련: F04, F05, E03 · D02 §07, §10, §21~22 · CLAUDE.md 계약 5·6 · ADR 0018, 0021, 0023, 0025

## 배경

D02가 정한 것은 다음과 같다.

| 절 | 내용 |
|---|---|
| §07 | window는 `[start,end)`, watermark는 최대 관측 시각 − 2분. 10분까지 늦은 point는 version을 올려 재계산하고, 그 이후는 backfill만 |
| §10 | `metric_1m`(90일)·`metric_1h`(395일). worker는 dedup 원본에서 window를 재계산하고 version 있는 완성 행으로 교체한다. query는 최대 version을 읽는다 |
| §21 | 입력이 멈춘 partition은 처리 시계 기반 idle 감지(60초)와 max_lateness(120초)로 전역 최소에서 뺀다 |
| §22 | 집계 job은 window·revision별 lease를 취하고, 일부 합산 결과를 노출하지 않는다 |

집계 의미는 ADR 0025(`internal/metricagg`)가 정했다.

정하지 않은 것은 다음과 같다.

- 재계산 대상을 고르는 방법
- rollup의 저장소 권한
- 여러 process가 동시에 돌 때의 처리
- 빈 window의 표현

## 결정

### 1. 테이블 (migration 00003)

- 키는 `metric_1m(tenant_id, metric_name, stream_id, window_start)`다. `ReplacingMergeTree(revision)`이고 TTL은 `window_start + 90일`이다.
- 값은 ADR 0025 `Aggregate` 그대로다. `has_value`·`has_increase`·`has_histogram`이 false면 해당 값이 **없다**는 뜻이다(계약 6). 품질 사유(`flags`)와 `partial`을 함께 저장한다.
- **빈 window는 행을 쓰지 않는다.** 행이 없으면 값 없음이다. 0이 아니다.
- query는 같은 키에서 revision이 가장 큰 행을 읽는다. 한 window는 항상 **통째로** 다시 쓰므로 일부만 합산된 상태가 보이지 않는다(D02 §22).

### 2. watermark와 재계산 (D02 §07, §21)

tenant마다 다음을 계산한다.

```
watermark = floor_1m(그 tenant의 최대 관측 시각 − 2분)
  최대 관측 시각이 처리 시계로 60초 넘게 늘지 않으면 → floor_1m(now − 2분)   (idle)
  now − 2분을 넘지 않는다                                                    (미래 시각 point 방어)
계산 범위 = [min(처리 완료 위치, watermark − 10분), watermark)                 (따라잡기 상한 1시간)
원본 조회 = [계산 범위 시작 − baseline 10분, watermark)
```

- **tenant별 watermark:** 최대 관측 시각은 최근 1시간 원본에서 tenant별로 구한다.
  - D02 §21은 partition watermark의 전역 최소(idle 제외)를 말한다.
  - 이 구현은 tenant 단위로 같은 규칙을 적용한다. 한 tenant의 수집이 밀려도 **그 tenant의 window만** 늦게 닫힌다. 빠른 tenant가 느린 tenant의 window를 닫지 못하고, 느린 tenant가 다른 tenant 전체를 붙잡지도 않는다(리뷰에서 발견. 처음에는 전역 최대로 구현했다).
  - Kafka partition 단위는 원본 테이블에 partition 정보가 없어 쓰지 않는다. tenant는 partition key의 앞부분이다(ADR 0020).
- **처리 완료 위치:** tenant별로 마지막으로 쓴 window의 끝을 기억한다. 재시작하면 `metric_1m`에서 복원한다.
  - watermark가 10분 넘게 뛰어도 사이 window를 빠뜨리지 않는다. 수집 지연 해소, rollup 중단 뒤 재시작, idle 전환이 이런 경우다(리뷰에서 발견).
  - **따라잡기 상한은 1시간이다.** 그보다 큰 공백은 `gap`으로 기록하고(로그, 지표 `Gaps`) 상한부터 잇는다. 남은 구간은 backfill job(후속)이 맡는다.
- **주기:** 30초다. 계산 범위를 매번 다시 계산하고 **내용이 바뀐 window만** 새 revision으로 쓴다.
  - 10분 안에 늦게 온 point는 그 window를 다시 쓰게 만든다. cumulative라면 기준점이 바뀐 다음 window도 다시 쓴다.
- **revision:** `max(계산 시각 ns, 저장된 최대 revision + 1)`이다. 재시작하거나 노드가 바뀌어 시계가 뒤로 가도 새 계산이 이전 행을 이긴다(리뷰에서 발견).
- **같은 관측 시각의 상충 값:** 원본을 version 순(먼저 수신한 것부터)으로 정렬해 metricagg가 첫 값을 쓰게 한다. 이렇게 batch를 넘는 상충 값도 합산하지 않고 `duplicate_timestamp`로 표시한다(ADR 0021 §4·0025 §1).
- **insert token:** 행 내용으로 만든다. 같은 내용을 재시도하면 한 번만 들어간다.

### 3. 계정과 권한 (ADR 0018 확장)

| 계정 (role) | 권한 |
|---|---|
| rollup (`montracer_ch_rollup`) | `metric_points` 필요 컬럼 SELECT, `metric_1m` INSERT, `metric_1m`의 `tenant_id`·`window_start`·`revision` SELECT(진행 위치·revision 복원용). 집계 값과 그 밖의 원본(span·log)은 읽지 못한다 |
| query (`montracer_ch_query`) | `metric_1m` SELECT + tenant row policy |

- **rollup은 전 tenant metric을 읽는 시스템 job이다.** 이 사실을 두 테이블의 `USING 1` row policy(`rollup_all_tenants`)로 **명시**한다(ADR 0018 §3 "SELECT를 주는 role에는 같은 migration에서 정책을 만든다").
- **migration down:** 정책보다 권한을 먼저 거둔다(REVOKE). 정책만 지우면 `users_without_row_policies_can_read_rows` 설정에 따라 읽기 범위가 달라지기 때문이다.
- rollup job은 기동할 때 계정이 span을 **읽을 수 없는지** 확인한다. 읽을 수 있으면 기동하지 않는다(`ErrRollupTooPrivileged`).
- 로컬·CI 계정은 init script가 만든다(`CLICKHOUSE_ROLLUP_USER`, 기본 `montracer_rollup`).

### 4. 실행: worker의 rollup 역할, cluster에 하나

- `cmd/worker`에 역할을 둔다: `MONTRACER_WORKER_ROLES=ingest,rollup`(기본 `ingest`). rollup은 **별도 단일 replica 배포**로 켠다.
- **단일 실행을 배포 템플릿에서 강제한다.** `replicas: 1`과 `strategy: Recreate`로, rolling update 중에도 두 process가 겹치지 않게 한다.
- **lease는 아직 없다.** 두 process가 같은 window를 동시에 쓰면 내용은 같고 revision만 다르다. 원본을 읽은 시점이 다르면 늦게 쓴 쪽의 오래된 snapshot이 이길 수 있다. 그래서 단일 실행을 배포 규칙으로 강제하고 runbook에 명시한다.
  - D02 §22의 window lease는 rollup을 여러 process로 나눠야 할 때 함께 도입한다(tenant 범위 분할).
- **운영 지표:**
  - `montracer_rollup_cycles_total{outcome}`
  - `montracer_rollup_windows_written_total`
  - `montracer_rollup_window_flags_total{flag}` (D02 §10 격리 metric: missing_baseline, negative_delta, count_bucket_mismatch 등)
  - `montracer_rollup_cycle_duration_seconds`
  - `montracer_rollup_last_success_timestamp_seconds`
- **경보:** `MontracerMetricRollupStalled`(5분 무성공, page). 대응은 RB01 해당 절에 있다.

### 5. 아직 없는 것

- `metric_1h`(1분 rollup에서 계산)
- **tenant별 보존:** `metric_1m`은 D02 §10 기본 90일이다. entitlement 보존(D04 §10)은 entitlement를 구현할 때 반영한다.
- **삭제 tombstone(F09):** 원본 삭제 원장이 생기면 rollup도 같은 selector로 재계산하거나 삭제한다(D04 §04). 원본 point가 사라진 window의 기존 행은 다시 써지지 않으므로, 삭제 job이 rollup 행도 지워야 한다.
- metric 조회 API(`POST /query/metrics`)
- backfill job(10분 초과 지연)
- window lease
- partition·tenant별 watermark
- 신규 series(cardinality) quota(ADR 0024 §5)

### 6. 외부 사례 근거 (2026-10-05 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| §2 지연 허용 watermark + idle | Apache Flink ([Generating Watermarks](https://ci.apache.org/projects/flink/flink-docs-stable/docs/dev/datastream/event-time/generating_watermarks/)) | `forBoundedOutOfOrderness`(최대 지연만큼 기다림)와 `withIdleness`(일정 시간 입력이 없으면 idle로 보고 watermark 최소에서 제외) | 채택: 관측 − 2분, idle 60초 (D02 §07·§21과 같음) |
| §2 늦은 sample을 기다린 뒤 계산 | Prometheus recording rules ([docs](https://prometheus.io/docs/prometheus/3.5/configuration/recording_rules/)) | `query_offset`·`rule_query_offset`으로 평가 시각을 과거로 밀어 늦게 도착한 sample이 저장된 뒤 계산 | 채택: 최대 지연 뒤 window를 닫음 |
| §4 단일 실행 | Thanos compactor ([docs](https://thanos.io/tip/components/compact.md/)) | 동시 실행에 안전하지 않아 bucket당 singleton으로 배포해야 하며, 나누려면 서로 다른 block stream을 맡겨야 함 | 채택: rollup 단일 replica. 분할이 필요하면 tenant 범위 분할 + lease |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 재계산 방식 | 최근 10분을 매 주기 전체 재계산 + 바뀐 window만 씀 | 변경 감지(수신 시각 index): 정렬 키에 수신 시각이 없어 효율이 나지 않는다. ClickHouse MV로 insert 시 합산: 중복 delta를 더한다(D02 §05 금지) |
| watermark | tenant별 관측 시각 + idle 시 처리 시계 | 처리 시계만: 수집이 밀리면 window를 일찍 닫아 매번 다시 쓴다. 관측 시각만: 입력이 멈추면 window가 영원히 열려 있다. 전역 최대: 빠른 tenant가 느린 tenant의 window를 일찍 닫는다. 전역 최소: 느린 tenant 하나가 전체를 붙잡는다 |
| 진행 위치 | 저장(metric_1m에서 복원) | 매번 고정 범위만: watermark가 뛰면 window가 영구히 빠진다 |
| 실행 단위 | worker 역할(별도 배포) | 별도 binary: 배포 단위만 늘고 코드는 같다 |
| 빈 window | 행 없음 | 0 행: 계약 6 위반 |

## 결과

- 수집한 metric이 1분 rollup으로 이어진다. dashboard·monitor·SLO(F05·F11)가 쓸 저장 원천이 생긴다.
- **비용:** 매 30초 최근 약 20분의 원본을 읽는다. 기준 부하(D06 §03: 100k active series)에서 시간·메모리를 측정해 재검토한다.

## Rollback

- rollup 역할을 끄면 `metric_1m` 갱신만 멈춘다. 원본은 영향받지 않는다.
- migration 00003 down은 `metric_1m`과 정책을 지운다. role은 남긴다.

## 재검토 조건

- 주기 실행 시간이 주기(30초)에 가까워질 때. 이때는 tenant 범위로 분할하고 window lease를 도입한다.
- 특정 tenant의 수집 지연으로 재계산이 잦을 때. 이때는 tenant별 watermark를 도입한다.
- `metric_1h`와 조회 API를 추가할 때. 이때는 같은 revision 규칙을 따른다.

## 증거

- spec-reviewer 지적 반영
  - watermark가 뛸 때 window가 빠지는 문제
  - 전역 최대 watermark
  - 벽시계 revision
  - batch를 넘는 상충 값 순서
  - migration down 권한
- `internal/rollup` 단위 테스트
  - **처리 완료 위치부터 따라잡기:** watermark−10분 이전의 미처리 window도 계산한다. mutation 확인: 이 로직을 빼면 실패한다.
  - **1시간 초과 공백은 gap 기록**
  - **재시작 뒤 revision > 저장된 최대값**
  - **tenant별 watermark:** 느린 tenant의 window를 닫지 않는다.
  - **batch를 넘는 상충 값:** 먼저 수신한 값을 쓰고 표시한다.
  - 닫힌 window만 계산하고, 빈 window는 행이 없다.
  - lookback으로 기준점을 찾는다.
  - 늦은 point가 오면 그 window와 기준점이 바뀐 다음 window만 새 revision으로 다시 쓴다. 바뀌지 않았으면 쓰지 않는다.
  - 쓰기 실패 뒤 재시도한다.
  - tenant를 분리한다.
  - 재계산 범위 밖 window는 쓰지 않는다.
  - **watermark:** 수집이 밀리면 늦게 닫고, idle 60초 뒤에는 처리 시계로 닫으며, 미래 시각 point가 window를 미리 닫지 못한다.
  - 내용 token이 안정적이다.
- 통합 테스트(ClickHouse rollup 계정)
  - 실제 원본에서 counter 증가량 70, delta histogram 병합 [2,3], 기준점 없는 tenant B는 `missing_baseline`·partial(누적 5를 세지 않음)
  - 재실행 시 행 수가 그대로다.
  - query 계정은 자기 tenant 행만 본다.
  - 관리자 DSN으로는 기동하지 않는다.
  - rollup 계정은 `metric_1m` 집계 값을 읽거나 `logs_local`에 쓸 수 없다. 진행 위치 컬럼만 읽는다.
- promtool: `MontracerMetricRollupStalled`가 울릴 때와 안 울릴 때.
