# RB02 검색 지연과 Cardinality 폭증

- 원문: D04 §11 RB02, 지표·경보: D04 §10, ADR 0023
- 관련 설계: ADR 0018(ClickHouse 계정·row policy·실행 예산), 0022(trace 조회), 0027(metric 조회), 0029·0030(cardinality 상한)
- 경보 규칙: [`deploy/prometheus/rules/montracer.rules.yml`](../../deploy/prometheus/rules/montracer.rules.yml)

## 먼저 알아둘 불변식

1. **조회는 실행 예산 안에서만 돈다.**
   - query-api는 요청마다 저장소 조회에 10초 상한을 둔다.
   - ClickHouse query 계정 profile(`montracer_query_profile`, migration clickhouse 00001)은 실행 5초, 읽기 10GB, 결과 1만 행, memory 2GB가 기본이다.
   - 예산을 넘으면 ClickHouse가 오류를 내고 query-api가 오류로 응답한다. 부분 결과를 정상처럼 돌려주지 않는다. 응답 코드는 원인별로 다르다(`internal/telemetrystore/errors.go`).

     | ClickHouse 코드 | 뜻 | 응답 |
     |---|---|---|
     | 158 `TOO_MANY_ROWS`, 160 `TOO_SLOW`, 307 `TOO_MANY_BYTES`, 396 `TOO_MANY_ROWS_OR_BYTES` | 읽기·결과 예산 초과 | **422** `QUERY_BUDGET_EXCEEDED` (4xx — 5xx 경보에 안 잡힌다) |
     | 159 `TIMEOUT_EXCEEDED` | 실행 시간 예산 초과 | 504 `QUERY_TIMEOUT` |
     | 241 `MEMORY_LIMIT_EXCEEDED`, 202 `TOO_MANY_SIMULTANEOUS_QUERIES`, 209·210 네트워크, 242 replica 읽기 전용 | 저장소 과부하·장애 | 503 `UNAVAILABLE` |
     | 그 밖(497 권한 등) | 코드·배포 결함 | 500 |
2. **cardinality 상한은 기존 series를 지킨다(ADR 0029·0030).** 상한에 걸리면 **신규** series·label 값만 거절하고, 기존 series는 계속 받는다.
3. **운영자가 tenant 데이터·metadata를 볼 때는 break-glass 기록을 남긴다(D04 §01).**
   - 아래 SQL은 tenant별 query metadata와 cardinality 등록부를 본다.
   - 실행 전에 incident 기록에 사유, 승인자, tenant scope, 시작·종료 시각(최대 30분)을 적는다.
   - **현재 공백:** 이를 강제·감사하는 break-glass 도구가 아직 없다. 그때까지 기록은 수동이다.
   - `system.query_log`의 `query` 컬럼에서 문자열 값(trace_id, metric 이름, label filter 값 등)은 `'?'`로 가려진다(ADR 0032). 같은 형태의 query는 `normalized_query_hash`로 묶는다.
   - **masking 설정(`query-masking.xml`) 배포 이전에 기록된 행의 `query` 컬럼은 조회·복사하지 않는다.** 그때는 값이 그대로 남았다. 배포 후 `TRUNCATE TABLE system.query_log`로 지우는 것이 rollout 절차다(ADR 0032 §Rollout).
   - `exception` 문구에는 값이 따옴표 없이 남을 수 있다(예: `Cannot parse uuid <값>`, ADR 0032). 원인은 `exception_code`로 판단하고 문구는 조회·복사하지 않는다.
4. **금지 사항**
   - 모든 ClickHouse replica를 한꺼번에 재시작하지 않는다(D04 §11). 진행 중 insert·merge가 함께 끊기고 RB01로 번진다.
   - 잘못 합쳐졌거나 폭증한 series를 label 자동 삭제로 "보정"하지 않는다. 서로 다른 series가 합쳐진다(D02 §10). 원본 재계산 job으로 복구한다.
   - tenant의 기존 series를 끊으려고 등록부(`metric_series`, `metric_label_values`) 행을 지우지 않는다. 지운 series는 신규로 판정되어 오히려 거절된다.

## 공통 확인 (첫 10분)

| 확인 | 방법 |
|---|---|
| 영향 route | Prometheus: `histogram_quantile(0.95, sum by (route, le) (rate(montracer_query_request_duration_seconds_bucket[10m])))`, `sum by (route, status_class) (rate(montracer_query_requests_total[10m]))` |
| 경로 전체 문제인지 | synthetic probe `trace` check(`montracer_probe_trace_visible_seconds`). probe도 느리면 저장·조회 전체, probe가 정상이면 특정 tenant·query 형태 |
| 느린·실패 query와 tenant | 아래 SQL (관리자 계정). tenant는 요청 설정 `SQL_montracer_tenant`에 있다 |
| 최근 변경 | query-api·ClickHouse release, migration(index·partition·projection) 이력 |

```sql
-- query 계정의 느린·실패 query 상위 (지난 15분). <query_user>는 MONTRACER_CH_QUERY_USER 값
SELECT event_time, query_id, Settings['SQL_montracer_tenant'] AS tenant,
       query_duration_ms, read_bytes, memory_usage, exception_code
FROM system.query_log
WHERE user = '<query_user>' AND event_time > now() - INTERVAL 15 MINUTE AND type != 'QueryStart'
ORDER BY query_duration_ms DESC LIMIT 20;

-- tenant별 scan 합계
SELECT Settings['SQL_montracer_tenant'] AS tenant, count() AS queries,
       sum(read_bytes) AS read_bytes, quantile(0.95)(query_duration_ms) AS p95_ms
FROM system.query_log
WHERE user = '<query_user>' AND event_time > now() - INTERVAL 15 MINUTE AND type = 'QueryFinish'
GROUP BY tenant ORDER BY read_bytes DESC LIMIT 10;

-- 지금 도는 query
SELECT query_id, elapsed, read_bytes, memory_usage, Settings['SQL_montracer_tenant'] AS tenant
FROM system.processes WHERE user = '<query_user>' ORDER BY elapsed DESC;
```

- `exception_code`의 뜻과 응답 코드는 위 "먼저 알아둘 불변식" 1의 표를 본다.
- 티켓·채팅에는 query_id, tenant UUID, 수치만 옮긴다.

---

### MontracerQueryLatencyHigh

- **탐지:** route별 조회 p95가 2초를 넘는 상태가 10분 지속됐다(D04 §11 RB02). 분당 1건 미만인 route는 제외한다.
  - 지연 histogram의 bucket은 1초 다음이 2.5초다. 그래서 "2초"는 bucket 사이 보간으로 계산한 근사값이다.
- **영향:** 사용자 조사 화면이 느려진다. 계속되면 예산 초과(5xx)로 이어진다. 원인이 저장 부하면 수집 freshness(RB01)도 함께 본다.
- **즉시 조치**
  1. 위 SQL로 원인 query와 tenant를 찾는다. 한 tenant·한 query 형태가 대부분이면 그 query를 취소한다.
     ```sql
     KILL QUERY WHERE query_id = '<query_id>' SYNC;
     ```
     취소된 요청은 client에 5xx로 돌아간다. 부분 결과를 정상처럼 주지 않는다.
  2. 비필수 부하(export·backfill·rollup catch-up)가 겹쳤는지 본다. `montracer_rollup_cycle_duration_seconds`가 평소보다 길면 rollup이 같은 저장소를 압박하는 중이다.
  3. 최근 query-api release 뒤에 시작됐으면 **query-api만** 직전 digest로 되돌린다. 수집·worker는 건드리지 않는다.
  4. ClickHouse 자체가 원인이면(part 폭증·merge 지연·disk) RB01 "공통 확인"의 ClickHouse 절을 따른다.
- **tenant별 동시 실행 상한(ADR 0037):** query-api replica마다 tenant당 실행 5개·대기 20개다. 한 tenant의 무거운 조회가 다른 tenant의 slot을 쓰지 않는다. 넘는 요청은 429(아래 "조회 429")다.
- **아직 없는 조치:** Cell 전체 동시성 상한(D02 §15: 100), scan 추정에 따른 비동기 job 전환, 동시 실행·대기 수 지표. 그때까지 cluster 전체 감속 수단은 개별 query 취소와 release 되돌리기다.
- **복구 확인:** 15분 동안 p95 ≤ 2초가 유지되고, 영향 없는 tenant의 성공률(`status_class="2xx"` 비율)이 평시와 같다(D04 §11).

### 조회 429 (`RATE_LIMITED`)

- **뜻:** 한 tenant가 한 replica에서 조회 5개를 실행하고 20개가 대기 중이다(D02 §15). 대기도 조회 시간 상한(10초) 안에서만 한다.
- **확인:** query-api 로그의 `code=RATE_LIMITED`와 tenant를 본다. 같은 tenant의 느린 query가 slot을 오래 잡고 있는지 위 SQL로 확인한다.
- **조치:** 느린 query를 취소하거나 고객에게 범위를 줄이도록 안내한다. 상한(`MaxConcurrent`·`MaxWaiting`)을 올리기 전에 저장소 여유를 본다. 상한은 tenant 격리 장치라 cluster 부하를 줄이지는 않는다.
- **interactive 누적 10,000행:** 검색을 cursor로 계속 넘겨도 10,000행에서 멈추고 `meta.warnings`에 `interactive_row_limit_reached`가 붙는다(D02 §13). 그 이상은 export job(후속)이다.

### MontracerQueryErrorRateHigh

- **탐지:** route별 조회 5xx 비율이 1%를 넘는 상태가 10분 지속됐다.
- **이 경보가 보지 않는 것**
  - 읽기·결과 예산 초과(422)는 4xx라 여기 잡히지 않는다. 고객이 "조회가 안 된다"고 하면 `sum by (route) (rate(montracer_query_requests_total{status_class="4xx"}[10m]))`도 본다.
  - memory kill(241)은 503으로 5xx 비율에 섞인다. D04 §11 RB02의 "memory kill 발생"만 따로 보는 경보는 아직 없다. 위 SQL로 `exception_code = 241`을 센다.
- **원인별 확인**

  | 원인 | 신호 | 조치 |
  |---|---|---|
  | 실행 시간 초과(504) | `exception_code` 159 | 특정 tenant의 넓은 범위 query다. 위 "느린 query" 절차를 따른다. 예산(profile)을 올려서 해결하지 않는다 |
  | memory kill·과부하(503) | `exception_code` 241·202 | 동시에 도는 큰 query를 찾아 취소한다(`system.processes`). 비필수 부하를 줄인다 |
  | 저장소 접속 실패(503) | query-api `/readyz`가 503(응답 본문 `telemetry store unavailable`), `exception_code` 209·210·242 | ClickHouse 상태(RB01 공통 확인) |
  | 인증 저장소 장애 | 제어 DB(PostgreSQL) 접속 오류 | 제어 DB 복구. 인증을 건너뛰는 우회는 하지 않는다 |
  | 권한 오류 497 | ClickHouse `ACCESS_DENIED` | query role GRANT·row policy(migration) 누락. **row policy를 빼서 우회하지 않는다**(ADR 0018) |

- **복구 확인:** 5xx 비율이 평시로 돌아오고, probe `trace` check가 연속 성공한다.

### MontracerControlErrorRateHigh

- **탐지:** 관리 API(control-api) route별 5xx 비율이 1%를 넘는 상태가 10분 지속됐다. 1차 범위는 감사 조회 `GET /api/v1/audit-events`다(ADR 0034).
- **영향:** 고객이 감사(지원 접근 이력 포함)를 읽지 못한다. 수집·조회 경로와는 실패 영역이 다르다.
- **원인별 확인**
  - 제어 DB(PostgreSQL) 장애·연결 고갈: control-api `/readyz` 503(응답 본문 `control db unavailable`), 로그 `request failed`의 `code=UNAVAILABLE`. 제어 DB를 복구한다. 인증을 건너뛰는 우회는 하지 않는다.
  - 조회 시간 초과: 넓은 범위·많은 행의 감사 조회. `audit_events_keyset` index(migration postgres 00005)가 있는지 확인한다.
  - 500: 코드·배포 결함이다. 직전 digest로 되돌린다.
- **복구 확인:** 5xx 비율이 평시로 돌아온다.

### MontracerMetricCardinalityLimited

- **탐지:** metric point가 신규 series 상한(`series_limit_exceeded`, ADR 0029) 또는 key당 label 값 상한(`label_value_limit_exceeded`, ADR 0030)으로 15분째 거절되고 있다.
- **영향:** 그 tenant의 **새** series만 저장되지 않는다. 기존 series와 다른 tenant는 정상이다. 수집 장애가 아니다.
- **tenant와 metric 찾기**
  1. ingress 로그 `otlp request`에서 `rejected.series_limit_exceeded` 또는 `rejected.label_value_limit_exceeded`가 있는 줄의 `tenant_id`를 본다.
  2. 어떤 metric이 series를 늘렸는지는 제어 DB 등록부로 본다. 운영 DB 관리자 계정으로 **읽기만** 한다. 앱 role은 RLS(FORCE)로 tenant context 없이 0행이다.
     ```sql
     -- 지난 1시간 신규 series 상위 metric (값은 저장하지 않으므로 이름과 수만 나온다)
     SELECT metric_name, count(*) AS new_series
     FROM metric_series
     WHERE tenant_id = '<tenant UUID>' AND first_seen > now() - interval '1 hour'
     GROUP BY metric_name ORDER BY new_series DESC LIMIT 10;

     -- key별 활성 값 수 (상한 100에 가까운 key가 원인 dimension)
     SELECT metric_name, label_key, count(*) AS active_values
     FROM metric_label_values
     WHERE tenant_id = '<tenant UUID>' AND last_seen > now() - interval '1 hour'
     GROUP BY metric_name, label_key ORDER BY active_values DESC LIMIT 10;
     ```
- **조치**
  - **기본은 고객에게 알리는 것이다.** 원인 metric·key(값은 없음)를 전달하고, 계측에서 해당 dimension을 빼거나 낮은 cardinality 값(route template 등)으로 바꾸도록 안내한다.
  - 계약상 상한 상향이 맞으면 quota overrides 파일의 `metrics.active_series`를 올린다(RB01 "tenant quota 조정", ADR 0024 §4). key당 값 상한의 tenant별 override는 아직 없다(ADR 0030 §3).
  - 상한을 올리기 전에 저장소 여유를 본다. 상한 상향은 rollup(ADR 0026) 비용도 함께 늘린다.
  - **상한을 일시 해제해 "받아 두는" 조치는 하지 않는다.** 한 번 활성화된 series는 1시간 동안 기존으로 판정되어 계속 들어온다.
- **복구 확인:** 고객 계측 변경 뒤 거절 증가가 멈추고, 경보가 해소된다.

---

## 사후 기록

- D04 §11 공통 체계를 따른다. 2영업일 안에 타임라인, 탐지 공백, 영향 tenant·route, 재발 방지 owner와 기한을 기록한다.
- query를 취소했다면 query_id, tenant, 사유를 기록한다(고객 문의 대응용).
- 잘못 합쳐진 series가 있었다면 영향 구간을 기록하고, 원본 재계산 job(backfill, 후속)으로 복구한다.
