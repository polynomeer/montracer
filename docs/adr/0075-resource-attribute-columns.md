# ADR 0075: resource 속성 열 — service.version·service.instance.id·k8s namespace·pod

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-11-02 "merge하고 다음 단계 진행해줘" — [design-rollout](../plan/design-rollout.md) D3 셋째 항목)
- Owner: Data lead (E03)
- 날짜: 2026-11-02 (제안·결정)
- 관련: F01·F03·F04, E03·E04 · D02 §07("trace·log·metric은 … resource instance를 공유"), §09("Map은 allowlist 검색용 복제 필드, typed promoted column"), §15 · D03 §01("필수 resource는 service.name, service.namespace, service.version, deployment.environment.name. service.instance.id와 k8s metadata는 agent가 보강"), §348("hostname·pod name만으로 같은 instance라 간주하지 않는다") · D05 §05(S02 instances·deployments 탭), §06(S04 facet) · 계약 3·4·6 · ADR 0019(PII 제거 — resource 포함), 0021(worker), 0039(서비스 catalog), 0043·0045, 0074(집계)

## 배경

시안의 버전·Pod facet(S04·S08), 서비스 버전·인스턴스(S02)는 resource 속성이 필요하다. 지금 저장 모델은 이렇다.

- span: resource 속성은 원본 payload(OTLP proto)에만 있다. trace 단건 조회는 payload를 풀어 `resource_attributes`로 보인다(ADR 0022). 검색·집계는 payload를 읽을 수 없다(ClickHouse가 proto를 풀지 않는다).
- log: payload를 저장하지 않는다. 서비스 식별(service_id)에 쓰는 속성 밖의 resource 속성은 **어디에도 남지 않는다.**
- `attributes` Map은 record 속성(span·log 자체)이다. resource 속성과 섞지 않았다.

D02 §09는 "임의 key를 모두 column으로 만들지 않고, 자주 비교하는 값은 typed promoted column"으로 정한다. 정할 것:

1. 어떤 resource 속성을 어디에 저장하나
2. 없는 값·잘못된 값
3. 검색 filter와 facet
4. 기존 행과 rollout

## 결정

### 1. `spans_local`·`logs_local`에 열 4개 (worker가 채운다)

| 열 | 속성 (OTel semantic conventions) | 타입 | 이유 |
|---|---|---|---|
| `service_version` | `service.version` | `LowCardinality(String)` | 배포 비교·버전 facet. 서비스당 값 수가 작다 |
| `service_instance_id` | `service.instance.id` | `String` | instance 식별(D03 §348 — pod 이름이 아니라 이것으로 같은 instance를 본다) |
| `k8s_namespace_name` | `k8s.namespace.name` | `LowCardinality(String)` | pod 이름은 namespace 안에서만 유일하다 |
| `k8s_pod_name` | `k8s.pod.name` | `String` | Pod facet·조사. 재시작마다 바뀌어 값 수가 크다 |

- worker 정규화(ADR 0021)가 record의 resource 속성에서 읽는다. ingress가 PII를 이미 지운 뒤의 값이다(ADR 0019 — resource 속성도 같은 규칙, 계약 3).
- 정렬 키·partition은 바꾸지 않는다. 같은 span의 재전송은 같은 값이라 dedup이 그대로다.
- 이 4개 밖의 resource 속성은 열로 만들지 않는다(D02 §09). span은 payload에 남는다. log는 지금처럼 남지 않는다(재검토 조건).
- metric은 이미 `resource_json`이 있다(바꾸지 않는다).

### 2. 없는 값은 빈 문자열, API는 null

- 속성이 없거나, 문자열이 아니거나, 255 byte를 넘으면 `''`로 저장한다.
  - 자르지 않는다. 자르면 다른 pod·버전이 같은 값이 된다.
  - 넘은 값은 worker 지표(`montracer_worker_resource_value_dropped_total{signal,attribute,reason}`)로 센다.
- 조회 API는 `''`를 `null`(값 없음)로 보인다. "unknown" 같은 값을 지어내지 않는다(계약 6).
- 집계 facet에서는 `missing_count`로 센다(ADR 0074 §3).

### 3. 검색 filter와 facet

**filter catalog** (log, trace span 단계):

- field: `service.version`, `service.instance.id`, `k8s.namespace.name`, `k8s.pod.name`
- 연산자: `eq`·`neq`·`in`·`exists`
  - `exists`는 값이 있음(`col != ''`)이다.
  - `eq`·`in`은 빈 값과 맞지 않는다. 사용자 값은 1자 이상이어야 한다(빈 문자열 비교를 거절한다 — 값 없음은 `exists`의 부정으로 묻는다).
- trace span 단계 조건의 뜻은 "그 값의 span이 하나라도 있는 trace"다(ADR 0043과 같다).
- `attributes.<key>`는 그대로 record 속성이다. 같은 이름이 resource에 있어도 섞지 않는다.

**집계 facet** (ADR 0074에 더한다):

| signal | facet | 값 | 소유 filter field |
|---|---|---|---|
| logs | `service.version` | 버전 | `service.version` |
| logs | `k8s.pod.name` | pod 이름 | `k8s.pod.name` |
| traces | `root_service_version` | root span의 버전 | `root_service_version` (요약 단계, 새로 추가 — `eq`·`in`) |

- 이 facet들은 값 수가 클 수 있어 속성 facet과 같은 경로로 센다. 경로는 group 상한 10만 throw → `too_many_values`(ADR 0074 §5)다.
- 요청당 상한은 "따로 세는 facet 2개"다. 속성 facet과 합쳐서 센다(ADR 0074 §5의 query 3개 상한을 지킨다).
- trace Pod facet은 두지 않는다. trace는 여러 pod를 지나고, root pod는 조사에 쓸모가 작다. filter(`k8s.pod.name` — 그 pod의 span이 있음)로 고른다.

**검색 결과:**

- log 행에 `resource`(4개 key, 없는 값은 null)를 더한다.
- trace 검색 행에 `root_service_version`을 더한다.
- trace 단건은 지금처럼 payload의 `resource_attributes`다.

### 4. migration과 기존 행

- ClickHouse migration은 `ALTER TABLE … ADD COLUMN … DEFAULT ''`다(expand-only). query 계정은 이미 표 전체 SELECT라 GRANT를 더할 일이 없다. row policy는 그대로다.
- 기존 행은 `''`(값 없음)이다.
  - span은 payload로 다시 채울 수 있지만 backfill job은 만들지 않는다. 보존 기간(span 기본 7일) 안에 자연히 채워진다.
  - 화면은 값 없음을 "버전 정보 없음"으로 보인다. 배포 직후 며칠은 값 없음 비율이 높다는 것을 rollout 안내에 적는다.
- worker와 migration 순서는 migration 먼저다.
  - 새 worker는 새 열에 쓴다. migration 전 새 worker는 INSERT가 실패하고, at-least-once라 offset을 넘기지 않는다(계약 2).
  - 옛 worker는 새 열을 쓰지 않아 DEFAULT `''`다.
- flag는 두지 않는다. 열 추가는 되돌리지 않아도 해가 없다. 조회 field는 catalog에 있어야만 쓸 수 있으므로 코드 revert로 끈다.

## 외부 사례 근거 (2026-11-02 확인)

| 사례 | 확인한 동작 | 이 결정에 쓴 부분 |
|---|---|---|
| Datadog Unified Service Tagging ([docs](https://docs.datadoghq.com/getting_started/tagging/unified_service_tagging/)) | `env`·`service`·`version` 세 예약 tag가 trace·metric·log를 잇는다. OTel `service.version` → `version`. version은 배포마다 바뀌고 배포 영향 추적·버전별 trace filter에 쓴다 | §1 `service.version`을 모든 신호의 정식 열로, §3 버전 facet |
| SigNoz logs schema ([docs](https://signoz.io/docs/userguide/logs_clickhouse_queries/)) | ClickHouse 기반. resource 속성은 JSON 열에 두고, 자주 쓰는 속성은 "selected field"로 MATERIALIZED 열을 만든다 | §1 고른 몇 개만 열로 승격(D02 §09와 같은 방향) |
| Grafana Tempo dedicated columns ([docs](https://grafana.com/docs/tempo/latest/operations/dedicated_columns/)) | 자주 쓰는 속성을 일반 key-value 목록 대신 전용 열에 둬 조회를 빠르게 한다 | §1 승격 열, 임의 key는 열로 만들지 않음 |
| OpenTelemetry semantic conventions (`service.version`, `service.instance.id`, `k8s.pod.name`, `k8s.namespace.name`) | 표준 resource 속성 이름 | §1 열 이름·원천 key |

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A (채택) 고른 4개를 typed 열로 | 검색·집계가 바로, D02 §09와 같음, 저장 비용 작음 | 다른 resource 속성은 여전히 검색 불가 |
| B resource 전체를 `resource` Map 열로 | 모든 key 검색 | 행마다 resource 전체 복제(저장 증가), Map 열 group by는 느림, 임의 key 검색은 D02 §09가 막음 |
| C resource 별도 테이블 + fingerprint(SigNoz 방식) | 저장 효율 | join·fingerprint 설계·row policy·삭제를 새로. 지금 규모에 과함 |
| D span payload에서 조회 시 풀기 | 저장 변경 없음 | ClickHouse가 proto를 못 푼다. log는 payload가 없다 |

## 결과

- 이점: 버전·Pod·instance로 검색하고 센다(S04·S08 facet, S02 instances·deployments의 기반). log의 resource가 처음으로 남는다(4개).
- 비용: 행당 열 4개. LowCardinality 2개는 작고, pod·instance는 문자열 길이만큼이다.
- 영향 받는 계약:
  - migration(ClickHouse): `spans_local`·`logs_local` 열 추가
  - worker sink 열
  - filter catalog(log·trace span·trace 요약 `root_service_version`)
  - OpenAPI(LogRow `resource`, TraceRow `root_service_version`, facet 이름)
- 필요한 시험:
  - 정규화: 4개 속성, 없음·문자열 아님·255 byte 초과 → `''`와 지표, redaction 뒤 값
  - 재전송 dedup 그대로, 기존 행 `''`
  - filter: eq·neq·in·exists, 빈 값 거절, trace span 단계 의미, 요약 `root_service_version`
  - facet: log 버전·pod, trace root 버전, missing_count, 따로 세는 facet 2개 상한
  - 응답: 없는 값 null
  - 격리: 다른 tenant의 버전·pod가 facet·filter에 안 나옴(`tests/isolation`)
  - migration 순서: 옛 worker → DEFAULT, 새 worker + 옛 스키마 → INSERT 실패·offset 유지

## 구현 순서

1. 이 ADR(이 PR)
2. 백엔드:
   - migration
   - worker 정규화·sink·지표(ADR 0021 변경 이력)
   - queryplan catalog(`exists` for 열)·요약 `root_service_version`
   - telemetrystore 검색 행·집계 facet
   - OpenAPI
   - 시험, 아키텍처 문서, `cmd/worker`·`cmd/query-api` README, ADR 0074 변경 이력
3. 화면:
   - S04 root 버전 facet·버전·pod 조건
   - S08 버전·pod facet, 행 상세의 resource
   - S02 instances 탭은 따로(F03)

## Rollback

- 열은 남겨도 해가 없다(expand-only).
- worker를 되돌리면 새 행은 `''`이 된다(값 없음 — 화면은 "정보 없음").
- 조회 field·facet은 query-api revert로 뺀다.
- migration Down은 열 삭제다. 데이터를 지우므로 운영에서는 쓰지 않는다(runbook 안내).

## 재검토 조건

- 다른 resource 속성(`host.name`·`cloud.region`·`k8s.deployment.name` 등)을 조건으로 쓰는 요구가 생기면: 승격 열을 더하거나 후보 C
- log resource 전체 보존 요구가 생기면: log payload 저장(비용·PII 재검토)
- pod 값 수 때문에 facet이 자주 `too_many_values`면: pod facet을 서비스로 좁힌 뒤에만

## 증거

- 설계 검토(spec-reviewer): 이 PR에서 받는다.

## 변경 이력

- 2026-11-02 제안·결정
