# ADR 0018: 분석 저장소(ClickHouse) 접근 계정·tenant row policy·migration

- 상태: 제안 (구현 PR에 적용됨, 승인 대기)
- Owner: Data lead
- 날짜: 2026-10-05 (제안)
- 관련: F02, F04, E03 · D02 §02, §09, §15 · D04 §01 · ADR 0003, 0014, 0016

## 배경

명세는 ClickHouse 접근에 대해 다음을 요구한다.

- query service만 ClickHouse에 접근하고 사용자에게 DB 자격 증명을 발급하지 않는다 (D02 §02).
- "DB 계정·row policy 또는 dedicated DB를 방어 계층으로 추가"한다 (D04 §01).
- 시간·tenant·권한 범위·expires_at을 mandatory predicate로 넣는다 (D02 §15).

로컬 stack을 점검한 결과, 앱 계정(`montracer_app`)이 CREATE·DROP·SYSTEM·S3·URL 등 사실상 모든 권한을 가진 관리자였다. 쿼리 하나에서 tenant predicate가 빠지면 격리가 바로 깨지는 상태였다. 드라이버와 migration 도구도 정해지지 않았다.

## 결정

### 1. 드라이버와 migration

| 용도 | 선택 | 고정 |
|---|---|---|
| 드라이버 | `github.com/ClickHouse/clickhouse-go/v2` (v2.48.0). native 프로토콜, 이후 수집 worker의 batch insert에도 쓴다 | `go.mod` |
| migration | goose ClickHouse dialect. 같은 `cmd/migrate` 바이너리에서 `migrate clickhouse up` 형태로 실행한다 (SQL embed) | `migrations/clickhouse/` |

- migration은 관리자 계정으로 실행한다. ClickHouse는 DDL 트랜잭션이 없으므로 `NO TRANSACTION`으로 둔다. down migration이 있고, CI에서 up → down → up 순서로 검증한다.

### 2. 계정 3개와 최소 권한

| 계정 (role) | 용도 | 권한 |
|---|---|---|
| 관리자 (`montracer_admin`) | migration, 계정·정책 관리 | 전체. 앱은 이 계정을 쓰지 않는다 |
| ingest (`montracer_ch_ingest`) | 수집 worker | 원본 테이블 INSERT만. SELECT 권한이 없다 |
| query (`montracer_ch_query`) | query service | 테이블 SELECT만. `readonly = 2`(쓰기·DDL 금지, 요청별 설정 허용), tenant row policy, 실행 예산 |
| lookup definer (`montracer_lookup_definer`) | trace lookup MV 실행 | 로그인 불가(`HOST NONE`). 원본 5개 컬럼 SELECT와 lookup INSERT만 |

- 계정은 infra가 만든다. 로컬에서는 init script가 만든다. migration은 role에 권한·정책·settings profile만 부여한다(PostgreSQL의 ADR 0016과 같은 구조).
- system DB와 information_schema는 grant가 있어야 읽을 수 있다. 이 동작은 이미지 기본값에 기대지 않고 config.d에 명시한다(`select_from_system_db_requires_grant`, `select_from_information_schema_requires_grant`). 그래서 `system.query_log`로 다른 tenant의 쿼리 내용을 볼 수 없다.
- **실행 예산 (D02 §08, §15):** query role의 settings profile에 기본값과 상한을 둔다. `readonly=2`여도 상한 위로는 올릴 수 없고, overflow 모드는 `throw`로 고정(CONST)한다.

  | 설정 | 기본값(interactive) | 상한(MAX, 비동기 job) |
  |---|---|---|
  | `max_execution_time` | 5초 | 60초 |
  | `max_bytes_to_read` | 10GB | 100GB |
  | `max_result_rows` | 10,000 | 1,000,000 |
  | `max_memory_usage` | 2GB | 8GB (명세에 없는 설계 가정) |
- table function: query 계정의 `url`·`remote`·`cluster`·`file`·`s3`는 grant가 없어 거부된다. `merge()`는 허용되지만, 읽는 원본 테이블의 row policy가 그대로 적용된다(테스트로 확인).
- `telemetrystore.OpenQuery`는 접속 계정이 읽기 전용(`readonly ≥ 1`)이 아니면 기동을 거부한다(`ErrNotReadOnly`).

### 3. tenant row policy (방어 계층)

- query role에는 모든 테이블에 `tenant_id = toUUIDOrZero(getSetting('SQL_montracer_tenant'))` 정책을 건다.
- query service는 요청마다 principal의 tenant를 사용자 정의 설정 `SQL_montracer_tenant`로 보낸다. clickhouse-go에서는 `clickhouse.CustomSetting`으로 감싸야 한다.
- settings profile의 기본값은 `''`이다. 설정이 없으면 zero UUID와 비교하게 되어 **0행**이 나온다(fail closed). 잘못된 UUID 값도 0행이다.
- mandatory predicate(D02 §15)는 그대로 유지한다. row policy는 predicate가 빠진 쿼리를 막는 두 번째 방어선이다.
- row policy 조건만으로도 primary key 분석이 일어나 granule이 줄어든다. 테스트로 확인했으므로 predicate가 빠져도 full scan으로 번지지 않는다.
- **잔여 위험**
  - tenant 설정 값은 query service가 정한다. 코드가 틀린 tenant를 설정하면 막지 못한다(PostgreSQL `app.tenant_id`와 같은 신뢰 모델). 이 값은 principal에서만 가져와야 한다.
  - ClickHouse는 정책이 없는 사용자에게 전체 행을 보여준다(`users_without_row_policies_can_read_rows`). 앞으로 다른 role이나 사용자에게 SELECT를 줄 때는 같은 migration에서 그 role의 row policy도 함께 만들어야 한다.
  - **production 분산 구성:** Distributed table을 거치면 shard 쪽 쿼리가 cluster 설정의 사용자로 실행될 수 있다. 그러면 원본 테이블의 row policy가 적용되지 않을 수 있다. production 배포 템플릿은 아래를 **필수**로 한다. 이 격리 테스트가 통과해야 production에 배포한다.
    - cluster의 inter-server `<secret>`을 설정해 원래 사용자로 실행되게 한다.
    - Distributed table에도 같은 row policy를 건다.
    - 다중 shard 격리 테스트를 통과한다.

### 4. trace lookup materialized view

- lookup 테이블은 `SQL SECURITY DEFINER`로 정의한 MV가 채운다. definer는 migration 계정이다.
- ingest 계정에 원본 SELECT를 주면 모든 tenant 데이터를 읽을 수 있게 되므로 주지 않는다.

### 5. 조회 시 중복 제거

- ReplacingMergeTree의 merge는 즉시 일어나지 않는다. 그래서 query는 bounded 범위 안에서 key 기준으로 dedup한다(`LIMIT 1 BY span_id`).
- `FINAL`을 기본으로 쓰지 않는다 (D02 §05, §09).

### 6. 오류 분류 (ADR 0014)

| 분류 | 대상 | 응답 |
|---|---|---|
| 의존 서비스 장애 | 연결·네트워크 오류, 서버 오류 코드 202·209·210·241·242 | `Unavailable()` → 503 |
| 실행 시간 초과 | 159 | `QueryTimeout()` → 504 |
| 예산 초과 | 158·160·307·396, 조회 범위 초과(trace 단건 7일) | `BudgetExceeded()` → 아래 참조 |
| 입력 오류 | trace id 형식, 빈 범위 | `InvalidArgument()` → 400 + `field_violations` |
| 코드 결함 | 문법·권한 등 그 외 | 500 |

- **`QUERY_BUDGET_EXCEEDED`의 status는 422로 정한다.** D02 §12는 예시에서 코드 이름만 쓰고 status는 정하지 않았다. 이것은 ADR 0014 코드 표를 보완하는 결정이다. D02 §12 표에 행을 추가하는 개정을 함께 요청한다.
- 도메인 패키지는 apierr를 import하지 않는다. 대신 위 메서드를 구현한 오류를 반환하고, apierr가 그 메서드를 보고 분류한다.

### 7. 알려진 미충족

- **삭제 tombstone predicate (D02 §15):** 삭제 원장(F09)이 아직 없어 넣지 못했다. 삭제 원장을 구현할 때 query planner의 mandatory predicate에 추가하고, 그 predicate가 빠지면 실패하는 테스트를 같은 PR에 둔다.
- **parameter binding:** `$n` 위치 binding은 clickhouse-go가 클라이언트에서 escape하는 방식이다. 지금 입력(UUID, 검증된 hex, 시각)에서는 안전하다. 하지만 사용자 filter를 다루는 query planner(E03)는 서버 측 parameter(`{name:Type}`)를 기준으로 삼는다.

### 8. metric_points의 명세 밖 선택

D02 §10이 정하지 않은 부분은 다음처럼 정했다.

- `type`·`temporality`는 Enum8로 둔다.
- `stream_id`·`point_hash`는 FixedString(16)으로 둔다.
- `is_monotonic` 컬럼을 추가한다. OTLP Sum의 의미를 보존하기 위해서다.
- exponential histogram·summary 원본을 담는 `payload` 컬럼을 추가한다. D02 §10의 "별도 payload로 보존"을 구현한 것이다.

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 방어 계층 | row policy + 사용자 정의 설정 | tenant별 DB나 계정: tenant 수만큼 계정·DDL이 늘고 cross-tenant 운영 쿼리가 복잡해진다. dedicated Cell에서 검토한다 (ADR 0006) |
| 드라이버 | clickhouse-go v2 | HTTP 직접 호출: batch insert와 타입 처리를 다시 구현해야 한다 |
| lookup 채우기 | DEFINER MV | ingest에 SELECT 부여: 최소 권한 위반. worker가 직접 lookup insert: 쓰기 경로가 두 배가 된다 |

## 결과

- 로컬 stack의 ClickHouse 계정 구성이 바뀐다. `CLICKHOUSE_ADMIN_*`, `CLICKHOUSE_QUERY_*`, `CLICKHOUSE_INGEST_*` 변수를 쓰고 기본값은 compose에 있다. 기존 `.env`의 `CLICKHOUSE_USER/PASSWORD`는 더 쓰지 않는다.
- production의 Replicated·Distributed 테이블과 cluster 계정 관리는 배포 템플릿(helm·infra)에서 같은 role·정책 구조로 만든다.
- layout 실험 결과는 [docs/experiments/0001-clickhouse-layout.md](../experiments/0001-clickhouse-layout.md)에 둔다.

## Rollback

- migration down으로 테이블·정책·profile을 제거한다. role은 남긴다.
- row policy를 제거하면 mandatory predicate만 남는다. 이 경우 격리 테스트가 실패해야 한다.

## 재검토 조건

- 정책 평가 비용이 query SLO에 영향을 줄 때(측정 필요).
- dedicated Cell·사설망 배포에서 계정 관리 방식이 다를 때.
- `SQL_` 접두어 설정 동작이 ClickHouse 버전 업그레이드로 바뀔 때.

## 증거

- `internal/telemetrystore` 통합 테스트:
  - 같은 trace_id를 쓰는 두 tenant의 격리
  - predicate 없는 쿼리도 row policy로 격리되고, 설정이 없거나 잘못되면 0행
  - TTL 전 만료 행 비노출
  - 재전송 중복 제거
  - 입력 검증과 권한(Security Auditor 거부)
  - 계정 권한: query 계정의 INSERT·DROP·ALTER·system log·readonly 해제·url() 거부, ingest 계정의 SELECT·TRUNCATE 거부, 관리자·ingest DSN으로 OpenQuery 거부
  - primary key 사용
