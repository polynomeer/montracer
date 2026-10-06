# ADR 0039: log 검색의 `service.name` 필터와 environment 제한 key — 서비스 catalog로 풀기

- 상태: 승인 (결정 위임)
- Owner: Backend lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021~0038과 같은 위임). 근거는 §5
- 날짜: 2026-10-07 (제안·결정)
- 관련: D02 §08(정규화 검색 이름), §15(mandatory predicate·AST), D04 §01(environment scope), 계약 4 · ADR 0037(§3 env 403, §5), 0038(catalog) · F04·F05

## 배경

- logs_local 행에는 environment와 서비스 이름이 없고 **service_id만** 있다(ADR 0037 §3). 그래서 ADR 0037은 두 가지를 미뤘다.
  - `service.name`으로 log를 거르기
  - environment로 제한된 key의 log 검색(범위 안임을 증명할 수 없어 403)
- ADR 0038로 서비스 catalog(이름·environment ↔ service_id)가 생겼다.
- service_id는 자연 키 (tenant, environment, namespace, name)의 결정적 hash다(`envelope.ServiceID`). 그래서 **service_id가 environment E의 catalog 항목이면 그 행은 E의 것이다.** 행을 고치지 않고 catalog로 증명할 수 있다.

## 결정

### 1. `service.name` 필터

- log catalog에 `service.name`(eq·neq·in)을 더한다. 값은 1~255 byte 문자열이다.
- **대소문자를 무시한다.** catalog의 `name_normalized`(D02 §08 "검색용 정규화 필드")와 `lower(입력)`을 비교한다. `Checkout`과 `checkout`은 같은 이름으로 찾는다. 서로 다른 environment·namespace의 같은 이름도 모두 맞는다(좁히려면 environment 제한 key, 후속으로 `service.environment` 필터).
- **두 번 컴파일한다.**
  1. 첫 컴파일은 AST를 검증하고 이름만 모은다(`Compiled.Unresolved`). 저장소는 이 결과를 실행하지 않는다.
  2. query-api가 제어 DB에서 이름 → service_id를 풀고(`ResolveServiceNames`, principal의 environment 범위 안, archived 포함) 다시 컴파일한다.
- **SQL:** `has(arrayMap(x -> toUUID(x), {f<n>:Array(String)}), service_id)`. neq는 `NOT has(...)`. ID는 parameter로만 들어간다(계약 4).
- **catalog에 없는 이름은 빈 집합이다.** eq·in은 맞는 행이 없고, neq는 모든 행이 맞는다. 0이나 다른 값으로 바꾸지 않는다.
- cursor query hash는 **이름**(정규형 AST)으로 묶는다. 풀린 ID는 page마다 다시 푼다. page 사이 새로 등록된 서비스는 다음 page부터 맞을 수 있다. 수신 snapshot이 범위를 묶는다.

### 2. environment 제한 key의 log 검색

- 403 대신 **mandatory predicate**를 넣는다: 허용 environment의 catalog service_id 집합(`EnvironmentServiceIDs`). 안쪽 subquery(dedup 전)에 `has(..., service_id)`로 들어간다.
- `telemetrystore.SearchLogs`는 제한된 principal인데 집합(`EnvironmentServices`)이 없으면 여전히 403이다. **fail closed**다.
  - catalog가 없는 배포(query-api `Services` 미설정)는 그대로 403이다.
  - 허용 environment에 서비스가 없으면 빈 집합이라 아무것도 보지 않는다(NULL = 제한 없음으로 바뀌지 않는다).
- **한계: catalog에 등록되지 않은 서비스의 log는 제한 key에 보이지 않는다.** ADR 0038 §2의 등록 누락(queue 초과·쓰기 실패 뒤 다시 보내지 않은 서비스), tenant 상한 초과 서비스가 해당한다. 보안상 숨기는 쪽이 맞다(범위를 증명하지 못한 데이터). 제한 없는 key는 그대로 본다.
- 집합 상한 10,000(catalog tenant 상한 5,000의 근사 초과 여유). 넘으면 오류다.

### 3. 실행 순서

- 본문 검증·첫 컴파일 → cursor 검증 → catalog 조회(제어 DB) → 재컴파일 → tenant 실행 slot(ADR 0037 gate) → ClickHouse. 잘못된 요청은 제어 DB를 치지 않는다.

### 4. 하지 않는 것

- `service.environment`·`service.namespace` 필터, trace 검색의 `service.name`: trace·error 검색 catalog와 함께 한다.
- 이름 alias(ADR 0038 §4): 이름을 바꾼 서비스의 과거 log는 새 이름으로 찾지 못한다.
- 제어 DB 장애 시 로컬 cache로 계속 검색하기: 지금은 catalog 조회 실패가 검색 실패(503 계열)다.

### 5. 외부 사례 근거 (2026-10-07 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| 서비스·environment를 log 검색의 1급 차원으로 | Datadog Unified Service Tagging ([Tagging](https://docs.datadoghq.com/getting_started/tagging/)) | `env`·`service`·`version`을 예약 tag로 두고 metric·trace·log를 같은 값으로 잇는다. span·metric tag는 소문자로 정규화한다 | 채택: 이름 검색은 정규형(대소문자 무시). 이 제품은 행에 이름 대신 service_id를 두므로 catalog로 푼다 |
| 접근 제한을 검색마다 붙는 필터로 강제 | Datadog Logs RBAC restriction query ([Logs RBAC](https://docs.datadoghq.com/logs/guide/logs-rbac/)) | role에 restriction query를 붙여 그 query에 맞는 log만 보게 한다 | 채택: environment 범위를 사용자 filter와 별개인 mandatory predicate로. 증명할 수 없는 행은 숨긴다(fail closed, 이 제품의 선택) |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| env 증명 | catalog service_id 집합 | logs_local에 environment column 추가: migration·worker 변경, 과거 행은 비어 있어 같은 문제가 남는다(후속 최적화로는 유효) |
| 이름 비교 | 정규형(대소문자 무시) | 정확 일치: D02 §08이 검색용 정규화를 정했다 |
| 이름 풀기 위치 | query-api(검색 시) | ClickHouse dictionary: 제어 DB를 분석 저장소에 연결해야 하고 RLS·권한 경계가 흐려진다 |

## Rollback

- query-api `Services`를 비우면 `service.name`은 422, environment 제한 key는 다시 403이다(ADR 0037 동작).

## 증거

- `internal/queryplan`: 첫 컴파일은 이름만 모은다(Unresolved), 재컴파일 SQL, 없는 이름 = 빈 집합, canonical은 이름, 잘못된 값 거절
- `internal/query`: 풀린 ID로 실행, cursor 다음 page, catalog 없으면 422, 제한 key에 범위 전달, 제한 없는 key에는 범위 없음
- `internal/controldb` 통합: 대소문자 무시·archived 포함·environment 제한·다른 tenant 이름 안 풀림, environment 범위 집합(빈 environment = 빈 집합)
- `internal/telemetrystore` 통합(ClickHouse): 범위 없는 제한 key 403, 범위 집합에 맞는 행만, 빈 집합은 0행, `service.name` eq·neq·없는 이름, 풀지 않은 filter는 실행 거절
- `tests/isolation`: B가 같은 이름의 서비스를 가져도 A의 log를 못 봄, A의 staging 제한 key는 prod log를 못 봄(filter 유무 모두)
- `make smoke`(CI): `service.name = "Payment"`(대소문자 무시) + trace_id로 장애 log 1건
