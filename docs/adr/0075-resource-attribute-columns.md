# ADR 0075: resource 속성 열 — service.version·service.instance.id·k8s namespace·pod

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-11-02 "merge하고 다음 단계 진행해줘" — [design-rollout](../plan/design-rollout.md) D3 셋째 항목)
- Owner: Data lead (E03)
- 날짜: 2026-11-02 (제안·결정)
- 관련: F01·F03·F04, E03·E04 · D02 §05(at-least-once), §07("trace·log·metric은 … resource instance를 공유"), §09("Map은 allowlist 검색용 복제 필드, typed promoted column"), §15(filter 연산자) · D03 §01("필수 resource는 service.name, service.namespace, service.version, deployment.environment.name. service.instance.id와 k8s metadata는 agent가 보강"), §12("hostname·pod name만으로 재시작 전후를 같은 instance라 간주하지 않는다") · D05 §05(S02 instances·deployments 탭), §06(S04 facet) · 계약 2·3·4·5·6 · ADR 0019(PII 제거 — resource 포함), 0021(worker), 0022(trace 단건 `resource_attributes`), 0039, 0043·0045, 0074(집계)

## 배경

시안의 버전·Pod facet(S04·S08)과 서비스 버전·인스턴스(S02)는 resource 속성이 필요하다. 지금 저장 모델은 이렇다.

- **span:** resource 속성은 원본 payload(OTLP proto)에만 있다.
  - trace 단건 조회는 payload를 풀어 `resource_attributes`로 보인다(ADR 0022).
  - 검색·집계는 payload를 읽을 수 없다. ClickHouse가 proto를 풀지 않는다.
- **log:** payload를 저장하지 않는다. 서비스 식별(service_id)에 쓰는 속성 밖의 resource 속성은 어디에도 남지 않는다.
- **`attributes` Map:** record 속성(span·log 자체)이다. resource 속성과 섞지 않았다.

D02 §09는 "임의 key를 모두 column으로 만들지 않고, 자주 비교하는 값은 typed promoted column"으로 정한다.

정할 것:

1. 어떤 resource 속성을 어디에 저장하나
2. 없는 값·잘못된 값·redaction이 바꾼 값
3. 검색 filter와 facet, 그리고 그 의미
4. 기존 행과 rollout(at-least-once를 지키는 순서)

## 결정

### 1. `spans_local`·`logs_local`에 열 4개 (worker가 채운다)

| 열 | 속성 (OTel semantic conventions) | 타입 | 이유 |
|---|---|---|---|
| `service_version` | `service.version` | `LowCardinality(String)` | 배포 탐색·버전 facet. 서비스당 값 수가 작다 |
| `service_instance_id` | `service.instance.id` | `String` | instance 식별(D03 §12 — pod 이름이 아니라 이것으로 같은 instance를 본다) |
| `k8s_namespace_name` | `k8s.namespace.name` | `LowCardinality(String)` | pod 이름은 namespace 안에서만 유일하다 |
| `k8s_pod_name` | `k8s.pod.name` | `String` | Pod 조사. 재시작마다 바뀌어 값 수가 크다 |

- worker 정규화(ADR 0021)가 record의 resource 속성에서 읽는다. ingress가 PII를 이미 지운 뒤의 값이다(ADR 0019 — resource 속성도 같은 규칙, 계약 3).
- 정렬 키·partition·TTL은 바꾸지 않는다.
- 이 4개 밖의 resource 속성은 열로 만들지 않는다(D02 §09). span은 payload에 남고, log는 지금처럼 남지 않는다(재검토 조건).
- metric은 이미 `resource_json`이 있어 바꾸지 않는다.
- **pod 이름은 instance 식별자가 아니다**(D03 §12).
  - 같은 이름이 다른 namespace·cluster에 있을 수 있다. StatefulSet pod는 재시작해도 같은 이름이다.
  - `k8s.pod.uid`·`k8s.cluster.name`은 이번에 열로 두지 않는다(재검토 조건). 재시작 전후를 가르는 것은 `service.instance.id`다.

### 2. 저장 값: 없음·버림은 빈 문자열, API는 null

- 다음 값은 `''`로 저장한다.

  | 경우 | 지표 사유 |
  |---|---|
  | 속성이 없음 | `missing` |
  | 문자열이 아님 | `not_string` |
  | 255 byte를 넘음 | `too_long` |
  | ingress redaction 표시(`[REDACTED:<종류>]`)를 포함함 | `redacted` |

  - 지표 이름: `montracer_worker_resource_value_dropped_total{signal,attribute,reason}`
  - `missing` 비율은 D03 §01의 필수 resource(`service.version`) 누락을 보여주는 설치 품질 지표로도 쓴다.
- 자르지 않는다. 자르면 다른 pod·버전이 같은 값이 된다.
- 255 byte 근거: Kubernetes 이름의 최대 길이는 DNS subdomain 253자다. 서비스 이름(ADR 0038)과 Map key 상한(ADR 0037)도 255다.
- 조회 API는 `''`를 `null`(값 없음)로 보인다. "unknown" 같은 값을 지어내지 않는다(계약 6). 집계 facet에서는 `missing_count`로 센다(ADR 0074 §3).
  - 화면은 "정보 없음"으로 보인다. 없음과 버림을 화면에서 가르지 않는다. 지표로만 가른다(한계).
- **redaction 오탐(한계):** ADR 0019 §5에 따르면 4부분 버전(`1.4.2.0`)이 공인 IPv4로 보여 축약될 수 있다. 그러면 `2.3.4.5`와 `2.3.4.7`이 같은 값으로 저장된다.
  - PII 정책은 D04가 권위라, 이 ADR에서 규칙을 풀지 않는다.
  - 구현 PR이 실제 형태의 값이 바뀌지 않는지 시험한다. 대상: semver, `1.2.3-rc.1`, git sha, `checkout-7d9f8b6c4-x2k9p`, UUID instance ID.
  - 4부분 버전의 축약은 알려진 한계로 둔다. 풀려면 D04 개정 + ADR 0019 변경(재검토 조건).

### 3. 검색 filter와 facet

**filter catalog** (log, trace span 단계):

- field: `service.version`, `service.instance.id`, `k8s.namespace.name`, `k8s.pod.name`. 연산자는 `eq`·`neq`·`in`·`exists`다.
- **값 없는 행은 어떤 값 비교와도 맞지 않는다.** `attributes.<key>`의 `mapContains AND …` 의미와 같다.
  - `eq`·`in`: `col = v`. 사용자 값은 1~255 byte다. 빈 문자열은 400이다.
  - `neq`: `col != '' AND col != v`. 값 없는 행은 빠진다.
  - `exists`: `col != ''`
- 값 없는 행만 고르는 조건은 없다. D02 §15에 부정 연산자가 없다. 값 없음은 facet의 `missing_count`로만 보인다.
- **trace span 단계 조건의 뜻:** "그 값의 span이 하나라도 있는 trace"다(ADR 0043과 같다).
  - span 단계 조건 여럿을 `and`로 묶으면 같은 span이 모두 만족해야 한다. 예: `and(service.name=A, service.version=v)` = "서비스 A의 버전 v span이 있는 trace".
  - matched 단계가 span 행 단위로 거르기 때문이다(ADR 0043 §2).
- `attributes.<key>`는 그대로 record 속성이다. 같은 이름이 resource에 있어도 섞지 않는다.

**집계 facet** (ADR 0074에 더한다):

| signal | facet | 값 | 함께 주는 맥락 | 소유 filter field | 세는 곳 |
|---|---|---|---|---|---|
| logs | `service.version` | 버전 | `service_id` | `service.version` | 본 query(값 수가 작다) |
| logs | `k8s.pod` | pod 이름 | `k8s.namespace.name` | `k8s.pod.name`, `k8s.namespace.name` | 따로 세는 facet |
| traces | `root_service_version` | root span의 버전 | `service_id`(root 서비스) | `root_service_version` (요약 단계, **새로 추가** — `eq`·`in`) | 본 query(요약을 다시 거치지 않는다) |

- **맥락(`context`):** 같은 문자열이 다른 대상이면 값을 합치지 않는다.
  - 버전 facet은 `(service_id, version)`, pod facet은 `(namespace, pod)` 쌍으로 센다.
  - 응답의 facet 값에 `context`(문자열 map)를 더해 쌍을 보인다. 서비스 A의 `1.2.0`과 서비스 B의 `1.2.0`이 한 줄이 되지 않는다.
- **세는 곳:**
  - 버전 facet은 고정 facet처럼 본 query의 `sumMapIf`에서 함께 센다. 무거운 단계는 한 번이다(ADR 0074 §5).
  - pod facet은 값 수가 커서 속성 facet과 같은 경로로 따로 센다. group 상한 10만 throw → `too_many_values`다.
  - 요청당 "따로 세는 facet"은 속성 facet과 합쳐 2개다. 그래서 ADR 0074 §5의 query 3개 상한을 지킨다.
- **trace Pod facet은 두지 않는다.** trace는 여러 pod를 지나고, root pod는 조사에 쓸모가 작다. filter(`k8s.pod.name` — 그 pod의 span이 있음)로 고른다.
- **`root_service_version`의 `missing_count`**에는 "root span 없음"과 "root에 버전 없음"이 함께 든다. ADR 0074의 root 서비스 facet 정의와 같다.

**검색 결과:**

- log 행에 `resource_attributes`(4개 key, 없는 값은 null)를 더한다. 이름은 trace 단건(ADR 0022)과 맞춘다. trace 검색 행의 `resource`(root span 이름, ADR 0043)와 다르다.
- trace 검색 행에 `root_service_version`을 더한다.
- trace 단건은 지금처럼 payload의 `resource_attributes`다.

**비목표 (계약 5):** span·log는 tail sampling을 거친 표본이다. 버전별 요청량·오류율·지연(배포 비교의 RED)은 이 열로 계산하지 않는다. 그것은 metric resource(`resource_json`·rollup label)에서 한다(S02 deployments, F03 후속). 이 열은 탐색·filter·건수(표본의 수)에만 쓴다.

### 4. migration과 rollout

- ClickHouse migration은 `ALTER TABLE … ADD COLUMN … DEFAULT ''`다(expand-only).
  - query 계정은 이미 표 전체 SELECT라 GRANT를 더할 일이 없다. row policy(행 단위)는 그대로다. lookup MV의 열 단위 GRANT도 영향이 없다.
  - 운영 배포(D02 §09: Replicated 표 + Distributed read 표)는 local 표 → Distributed 표 순으로 바꾼다(ON CLUSTER). 순서는 runbook에 둔다.
- **순서: migration 먼저, 그다음 새 worker.**
  - 옛 worker는 새 열을 쓰지 않는다. 행은 DEFAULT `''`다.
- **새 worker + 옛 스키마가 데이터를 잃지 않게 한다**(계약 2, D02 §05).
  - "열 없음" INSERT 오류는 행 하나의 결정적 거부(`RowError`)가 아니다. 그렇게 분류하면 모든 행이 quarantine으로 가고 offset이 넘어간다. quarantine은 payload를 남기지 않으므로 사실상 유실이다.
  - 그래서 이 오류는 batch 전체의 재시도 대상(transient)이다. offset을 넘기지 않는다.
  - 새 worker는 시작할 때 대상 표에 열이 있는지 확인한다. 없으면 기동하지 않는다(fail-fast).
  - Kafka 보존(24시간) 안에 migration이 끝나야 한다는 것을 runbook(RB01)에 적는다.
  - migration Down(열 삭제)을 새 worker가 도는 중에 쓰면 INSERT가 깨진다. 데이터도 지우므로 운영에서는 쓰지 않는다.
- **기존 행:** `''`(값 없음)로 남다가 보존 기간이 끝나면 만료된다. 채워지지 않는다.
  - span은 payload로 다시 채울 수 있지만 backfill job은 만들지 않는다.
  - 배포 직후 보존 기간(span 기본 7일) 동안은 "정보 없음" 비율이 높다. rollout 안내에 적는다.
- **rollout 중 dedup(한계):** 옛 worker와 새 worker가 같은 record를 같은 version으로 쓰면, ReplacingMergeTree와 조회의 `LIMIT 1 BY` 중 어느 행이 남을지 정해지지 않는다(`''` 행이 남을 수 있음). collector 보강(k8sattributes)이 재전송 사이에 달라지면 같은 span의 값이 다를 수도 있다. 둘 다 조회 의미를 깨지 않는다. 값이 비거나 다를 수 있을 뿐이다.
- flag는 두지 않는다. 열 추가는 되돌리지 않아도 해가 없다. 조회 field는 catalog에 있어야만 쓸 수 있으므로 코드 revert로 끈다.

## 명세 반영 (계약 7)

D02 원본(docx) 개정 후보다. 원본 개정은 문서 책임자의 일이고, 그때까지 이 ADR과 OpenAPI가 기준이다.

- §09 DDL에 열 4개
- §13·§19에 LogRow `resource_attributes`, TraceRow `root_service_version`, facet 이름·`context`

## 외부 사례 근거 (2026-11-02 확인)

| 사례 | 확인한 동작 | 이 결정에 쓴 부분 |
|---|---|---|
| Datadog Unified Service Tagging ([docs](https://docs.datadoghq.com/getting_started/tagging/unified_service_tagging/)) | `env`·`service`·`version` 세 예약 tag가 trace·metric·log를 잇는다. OTel `service.version` → `version`. version은 배포마다 바뀌고, 배포 영향 추적과 버전별 trace filter에 쓴다 | §1 `service.version`을 모든 신호의 정식 열로, §3 버전 facet·filter |
| SigNoz logs schema ([docs](https://signoz.io/docs/userguide/logs_clickhouse_queries/)) | ClickHouse 기반. resource 속성은 JSON 열에 두고, 자주 쓰는 속성은 "selected field"로 MATERIALIZED 열을 만든다 | §1 고른 몇 개만 열로 승격(D02 §09와 같은 방향) |
| Grafana Tempo dedicated columns ([docs](https://grafana.com/docs/tempo/latest/operations/dedicated_columns/)) | 자주 쓰는 속성을 일반 key-value 목록 대신 전용 열에 둬 조회를 빠르게 한다 | §1 승격 열. 임의 key는 열로 만들지 않음 |
| OpenTelemetry semantic conventions (`service.version`, `service.instance.id`, `k8s.pod.name`, `k8s.namespace.name`) | 표준 resource 속성 이름 | §1 열 이름·원천 key |

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A (채택) 고른 4개를 typed 열로 | 검색·집계가 바로, D02 §09와 같음, 저장 비용 작음 | 다른 resource 속성은 여전히 검색 불가 |
| B resource 전체를 `resource` Map 열로 | 모든 key 검색 | 행마다 resource 전체 복제(저장 증가), Map 열 group by는 느림, 임의 key 검색은 D02 §09가 막음 |
| C resource 별도 테이블 + fingerprint(SigNoz 방식) | 저장 효율 | join·fingerprint 설계·row policy·삭제를 새로 만들어야 함. 지금 규모에 과함 |
| D span payload에서 조회 시 풀기 | 저장 변경 없음 | ClickHouse가 proto를 못 푼다. log는 payload가 없다 |

## 결과

- 이점: 버전·Pod·instance로 검색하고 센다(S04·S08 facet). log의 resource가 처음으로 남는다(4개).
- 비용: 행당 열 4개. LowCardinality 2개는 작고, pod·instance는 문자열 길이만큼이다.
- 영향 받는 계약:
  - migration(ClickHouse): `spans_local`·`logs_local` 열 추가
  - worker sink 열, 기동 시 열 확인
  - filter catalog: log·trace span 단계, trace 요약 `root_service_version`
  - OpenAPI: LogRow `resource_attributes`, TraceRow `root_service_version`, facet 이름·`context`
  - ADR 0074: facet 표·따로 세는 facet 상한(변경 이력)
- 필요한 시험:
  - **정규화:**
    - 4개 속성
    - 없음·문자열 아님·255 byte 초과·redaction 표시 → `''`와 사유별 지표
    - 실제 형태의 버전·pod·UUID가 redaction 뒤에도 그대로
  - **재전송·기존 행:** 재전송 dedup 그대로, 기존 행 `''`
  - **filter:**
    - eq·neq(값 없는 행 제외)·in·exists, 빈 값 400
    - trace span 단계의 같은 span 의미, 요약 `root_service_version`
  - **facet:**
    - log 버전(서비스별로 갈림)·pod(namespace별로 갈림)·`context`
    - trace root 버전(본 query, 요약 1회)
    - `missing_count`, 따로 세는 facet 2개 상한
  - **응답:** 없는 값 null
  - **집계 SQL:** 하위 query 모두 mandatory predicate(tenant·시간·expires_at·환경 범위)
  - **격리:** 다른 tenant의 버전·pod가 facet·filter에 안 나옴(`tests/isolation`)
  - **migration 순서:**
    - 옛 worker → DEFAULT
    - 새 worker + 옛 스키마 → 기동 거부. 실행 중이면 transient 재시도·quarantine 0건·offset 유지

## 구현 순서

1. 이 ADR(이 PR). ADR 0074 변경 이력·design-rollout D3 행을 같이 고친다.
2. 백엔드:
   - migration
   - worker 정규화·sink·기동 확인·지표(ADR 0021 변경 이력)
   - queryplan catalog(열 연산자 의미)·요약 `root_service_version`
   - telemetrystore 검색 행·집계 facet(`context`)
   - OpenAPI
   - 시험
   - 문서: 아키텍처 문서, `cmd/worker`·`cmd/query-api` README, RB01(migration 시간·순서·Down 경고), ADR 0019 변경 이력(오탐 시험 결과)
3. 화면:
   - S04: root 버전 facet, 버전·pod 조건
   - S08: 버전·pod facet, 행 상세의 resource
   - S02 instances·deployments 탭은 따로(F03, RED는 metric)

## Rollback

- 열은 남겨도 해가 없다(expand-only).
- worker를 되돌리면 새 행은 `''`이 된다. 화면은 "정보 없음"이다.
- 조회 field·facet은 query-api revert로 뺀다.
- migration Down은 열 삭제다. 데이터를 지우고 새 worker의 INSERT를 깨므로 운영에서는 쓰지 않는다(runbook).

## 재검토 조건

- 다른 resource 속성(`host.name`·`cloud.region`·`k8s.deployment.name`·`k8s.pod.uid`·`k8s.cluster.name` 등)을 조건이나 식별에 쓰는 요구가 생기면: 승격 열을 더하거나 후보 C
- 4부분 버전의 redaction 축약이 실제 고객에게 문제가 되면: D04 개정 + ADR 0019에서 `service.version` key의 IP 규칙 예외
- log resource 전체를 보존하라는 요구가 생기면: log payload 저장(비용·PII 재검토)
- pod 값 수 때문에 facet이 자주 `too_many_values`면: 서비스로 좁힌 뒤에만 pod facet을 허용

## 증거

- 백엔드 시험:
  - 단위(`internal/pipeline`): 열 값·사유(없음·빈 값·문자열 아님·255/256 byte·redaction 표시), 실제 형태의 값이 redaction 뒤 그대로, span·log 행과 batch 사유 수(재전송은 한 번)
  - 단위(`internal/queryplan`): eq·neq(값 없는 행 제외)·in·exists SQL, 빈 값·256 byte·contains·빈 원소 거절, trace span 단계 catalog, 요약 `root_service_version`
  - 단위(`internal/telemetrystore`): 맥락 쌍 facet(서비스별·값 없음·top_n), 따로 세는 facet 상한
  - handler(`internal/query`): 버전·pod facet의 소유 조건 빼기(pod는 namespace 조건까지), `context` 응답(OpenAPI 계약), 따로 세는 facet 3개 400, trace pod facet 400, root 버전 facet
  - ClickHouse 통합: sink가 열을 저장(없는 값 `''`), 옛 스키마에 기동 거부·쓰기 실패가 `RowError`가 아님, log 검색 행·neq·exists·다른 tenant 값 안 보임, 버전 facet 서비스별·pod facet namespace별·missing, trace 같은 span 의미·root 버전 facet·조건·pod 조건·검색 행 root 버전
  - `tests/isolation`의 HTTP 수준 시험은 ingress가 resource를 그대로 넘기는 경로라 telemetrystore 통합의 tenant 경계 시험으로 대신했다(다른 tenant의 버전·pod가 filter·검색에 안 나옴)
- 설계 검토(spec-reviewer, 2026-11-02): P0 없음.
  - P1 반영:
    - redaction 오탐 → 표시 값은 `''`·사유, 시험, 4부분 버전은 한계와 재검토 조건
    - pod 이름 식별 → (namespace, pod) 쌍, "식별자가 아님", uid·cluster는 재검토
    - 없는 값을 묻는 연산자 → 없음, `missing_count`만
    - `neq` 의미 → 값 없는 행 제외
    - 계약 5 → 비목표
    - migration 순서의 유실 경로 → transient·fail-fast·runbook
  - P2 반영: 버전 facet을 본 query로, 서비스별로 가름(`context`), span 단계 같은 span 의미, `missing_count`의 두 뜻, 없음·버림 구별은 지표로(한계), rollout 중 dedup 한계, 기존 행 문구, 운영 ON CLUSTER·Down 경고, 255 근거, `resource_attributes` 이름, 명세 반영 절, ADR 0074 변경 이력·design-rollout(이 PR), 구현 PR의 시험·문서 목록.

## 변경 이력

- 2026-11-02 제안·결정. 설계 검토 반영(위 증거).
- 2026-11-02 (구현, 백엔드 PR)
  - §3 facet 이름: log pod facet은 `k8s.pod`(값은 pod 이름, `context`에 namespace). 소유 field는 `k8s.pod.name`·`k8s.namespace.name`.
  - §2 빈 문자열 속성도 `missing`이다.
  - §4 기동 확인은 insert 문을 준비하고 보내지 않는 방식이다(ingest 계정은 표를 읽을 수 없어 `system.columns` 대신). 열 없음 코드 16·47.
  - 백엔드 리뷰(spec-reviewer: P0·P1 없음) 반영: pod facet의 namespace가 없으면 `context` key를 생략한다(빈 문자열을 값으로 보이지 않음). 시험의 임시 GRANT 회수, 지표 label 시험. §4 rollout 한계에 더한다 — 옛 worker가 insert 뒤 commit 전에 죽으면 새 worker의 같은 batch는 insert token으로 무시되어 그 행은 값 없음으로 남는다(유실 아님). 배포 순서는 migration → worker·query-api(RB01).
- 2026-11-02 (구현, 화면 PR)
  - S08: "버전별 log 수"(줄마다 서비스), "Pod별 log 수"(namespace/pod, namespace가 없으면 "namespace 정보 없음") checkbox, URL `ver`·`pod`, 조건 chip, log 상세의 "Resource"(없는 값 "정보 없음").
  - S04: "Root 버전" checkbox(줄마다 root 서비스), URL `rver`(요약 단계 `root_service_version`).
  - **조건은 문자열 기준이다.** 버전 줄을 고르면 그 버전 문자열로 거른다 — 다른 서비스의 같은 버전도 맞고, 그 줄들도 함께 고른 상태로 보인다(서비스 조건으로 좁힌다). pod도 이름으로 거른다(다른 namespace의 같은 이름 포함). facet 안내에 이를 적었다. (서비스·namespace 쌍으로 거르면 facet의 조건 빼기가 소유 field 밖의 서비스 조건과 섞여 여러 개 고르기가 깨진다.)
  - URL·공유 링크 값은 형식이 정해진 글자만 받는다: 버전 `[A-Za-z0-9._+~-]{1,255}`(저장 상한과 같은 길이), pod Kubernetes 이름. 그 밖의 값은 checkbox를 막고 이유를 보이는 글로 알린다. 화면 조건은 20개까지, 공유 링크 목록은 filter in 상한(100)까지 받아 넘는 목록이 통째로 빠지지 않게 한다(ADR 0074와 같다). 형식이 틀린 원소가 섞인 목록은 공유 링크에서 빠진다(자유 입력이 새지 않게).
  - 공유 URL 허용 키 확장(D05 §03 "filter ID, tab·entity"를 넘어선다 — ADR 0043의 ADR 0041 확장과 같은 방식): `ver`·`rver`·`pod`. 근거: 형식이 정해진 식별 값이고, ingress redaction 뒤의 resource 값이라 원문 PII가 아니다(ADR 0019).
  - 화면 리뷰(spec-reviewer: P0·P1 없음) 반영: 시험 안정화(다시 세는 동안 기다림), 공유 정규식을 화면 형식과 같은 원천에서, 막은 이유를 보이는 글·`aria-describedby`로, 맥락 없는 버전 줄은 "서비스 정보 없음", Root 버전 값 없음에 root 없는 trace도 든다는 안내, S04 Root 버전·공유 링크·debounce·옛 응답 drawer 시험.
  - S04 span 단계 버전·pod 조건과 S02 instances·deployments 탭은 아직 없다.
