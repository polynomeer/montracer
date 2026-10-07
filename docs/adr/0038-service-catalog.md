# ADR 0038: 서비스 catalog — 자동 등록과 `GET /api/v1/services`

- 상태: 승인 (결정 위임)
- Owner: Backend lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021~0037과 같은 위임). 근거는 §6
- 날짜: 2026-10-07 (제안·결정)
- 관련: D02 §08(Service Catalog), §13(`GET /services`), D04 §01 · ADR 0016(RLS), 0034·0037(cursor), F04

## 배경

- **원본 행에는 service_id만 있다.** spans_local·logs_local의 service_id는 자연 키 (tenant, environment, service.namespace, service.name)의 결정적 UUID다(pipeline). 이름·environment는 catalog가 있어야 알 수 있다.
- **그래서 막혀 있던 것**
  - 서비스 목록(D02 §13 `GET /services`, UI S01·S02)
  - log 검색의 `service.name` 필터와 environment 제한 key의 log 검색(ADR 0037 §5)
- **D02 §08 요구**
  - catalog 필드: owner_team, repository_url, runbook_url, tier, language, first_seen, last_seen, tags
  - 자동 관측 필드와 사용자 관리 필드를 나눠, agent 갱신이 owner 정보를 덮어쓰지 않게 한다
  - 24시간 미관측은 inactive, 30일 뒤 archived(삭제하지 않음)

## 결정

### 1. 저장: 제어 DB `services` (migration postgres 00006)

- **키:** (tenant_id, service_id)이고 RLS(ENABLE+FORCE)다(ADR 0016).
  - service_id 계산(`envelope.ServiceID`)은 worker와 ingress가 같은 함수를 쓴다. pipeline에서 envelope으로 옮겼다. 그래서 catalog 행이 원본 행과 바로 이어진다.
- **이름:** 대소문자를 보존하고(`name`), 검색용 정규형(`name_normalized = lower(name)`)을 generated column으로 둔다.
- **권한으로 갈라 놓는다:** 앱 role(`montracer_rw`)은 **자동 필드만 column 단위로 INSERT하고, `last_seen`·`language`만 UPDATE한다.** 사용자 관리 필드(owner_team·repository_url·runbook_url·tier·tags)는 처음 등록할 때도, 그 뒤에도 앱 role이 쓸 수 없다(통합 시험: INSERT·UPDATE 모두 `42501`). 그래서 "agent가 owner를 덮어쓰지 않는다"가 코드 규칙이 아니라 DB 권한이다.
  - 대가: 사용자 관리 필드를 쓰는 관리 API는 별도 role(그 column에만 UPDATE)로 붙여야 한다(§4).
- **상태:** active·inactive·archived는 저장하지 않고 조회 시 `last_seen`으로 계산한다. 행은 지우지 않는다.

### 2. 등록: ingress → `internal/catalog` Registrar (비동기, best-effort)

- **언제:** **Kafka ACK 뒤**에만 알린다. durable하게 받은 데이터의 서비스만 등록한다.
- **어떤 값:** redaction·environment 범위·quota·envelope 크기를 지나 **Kafka로 보낼 record가 남은 resource**의 값이다(envelope record의 resource 순번으로 고른다). 그래서 worker가 원본에 쓰는 service_id와 같다. 언어는 `telemetry.sdk.language`다.
- **흐름**
  - 요청 경로는 queue에 넣기만 하고 막지 않는다. queue는 4,096 묶음이고, 차면 버리고 센다.
  - flusher가 5초마다 tenant별로 모아 한 번에 upsert한다.
  - 같은 서비스는 replica당 5분 안에 다시 쓰지 않는다. 그래서 `last_seen`은 최대 5분 늦다(inactive 판정은 24시간이라 충분하다).
- **쓰기:** 한 transaction에서 이미 있는 서비스의 자동 필드를 갱신하고(`last_seen = GREATEST(...)`, 모르는 언어는 아는 언어를 지우지 않는다), 없는 서비스만 INSERT한다. 같은 service_id는 저장소가 먼저 합친다.
- **tenant 상한 5,000 서비스:** 넘는 새 서비스는 등록하지 않고 `over_limit`으로 센다. 이미 있는 서비스의 갱신은 막지 않는다.
  - 이유: 무작위 `service.name`(pod ID가 섞인 이름 등)이 공유 제어 DB와 목록을 끝없이 키우지 않게 한다(D02 §10 cardinality 원칙과 같은 위협). 수치는 service map 기본 렌더링 상한(500 nodes, D02 §13)의 10배다 — 실제 조직의 서비스 수보다 넉넉하고, 넘으면 이름 규칙 오류일 가능성이 높다.
  - 상한은 근사다: replica 여러 개가 동시에 새 서비스를 넣으면 replica 수 × batch만큼 넘을 수 있다.
  - replica cache도 상한(100,000 항목)이 있다. 넘으면 새 항목은 cache하지 않고 쓰기만 한다.
- **넣지 않는 값:** 이름·namespace·environment가 255 byte를 넘거나, 잘못된 UTF-8이거나 NUL을 포함하면 catalog에 넣지 않는다. 그런 값 하나가 tenant batch 전체를 실패시키지 않게 한다. 수집에는 영향이 없다.
- **실패해도 수집은 계속된다.** 다음 요청이 다시 넣는다.
  - **한계:** queue 초과로 버렸거나 쓰기에 실패한 서비스는 그 뒤 다시 수집되어야 등록된다. 다시 보내지 않는 서비스(1회성 batch job 등)는 원본 행은 있어도 catalog에 빠진 채로 남는다. 원본에서 채우는 backfill은 후속이다(§4). 그때까지 복구는 그 서비스가 다시 보내는 것이다.
  - 종료 시 마지막 flush는 전체 10초 안에 끝낸다(제어 DB 장애가 종료를 끌지 않게).
  - 지표 `montracer_ingress_catalog_services_total{outcome=written|dropped|over_limit|write_error}`
  - 경보 `MontracerServiceCatalogStale`(ticket, RB01)

### 3. API: `GET /api/v1/services` (query-api, D02 §13)

- **인자:** `environment`, `owner`(owner_team), `include_archived`(기본 false), `limit`(기본 100, 최대 1,000), `cursor`
- **정렬·cursor:** `(name_normalized, environment, service_id)` keyset이다.
  - 서명 cursor(ADR 0034)에 tenant, 권한 fingerprint(environment 범위 포함), query hash를 묶는다.
  - 상태 판정 시각은 첫 page snapshot으로 고정한다.
- **environment로 제한된 key**는 허용된 environment의 서비스만 본다. 저장소의 mandatory predicate다.
- **응답:** `{data, next_cursor, meta}`. 모르는 값(언어, 사용자 필드)은 null이다.
- 권한은 `telemetry.read`이고 tenant별 동시 실행 상한(ADR 0037) 안이다.

### 4. 하지 않는 것

- **사용자 관리 필드 편집 API**(owner_team 등): control-api의 관리 API와 함께 만든다. 지금은 column만 있고 null이다.
- **이름 변경 alias**(D02 §08 "alias 관계"): 이름을 바꾸면 새 service_id다. 과거 탐색을 잇는 alias는 후속이다.
- **`service.name` 필터와 environment 제한 key의 log 검색**(ADR 0037 §5): ADR 0039로 만들었다.
- **service map**(D02 §08 edge·confidence): 후속이다.
- **version·instance:** 서비스 정체성이 아니다(D02 §08). 저장하지 않는다.
- **`instrumentation_health`**(D02 §08 catalog 필드): SDK·Collector 상태 판정 기준이 아직 없다. 계측 상태 화면(D05)과 함께 후속이다. 응답에 필드를 두지 않는다(없는 값을 정상으로 보이지 않게).
- **원본에서 catalog를 채우는 backfill:** §2의 한계를 메운다. 후속이다.
- **tenant별 상한 override:** 지금은 고정 5,000이다. 필요하면 tenant 설정으로 옮긴다.

### 5. 운영

- rollout 순서는 migration 00006 → ingress(등록 시작) → query-api(목록)다.
- **기존 서비스는 다음 수집부터 등록된다.** 과거 원본에서 채우는 backfill은 하지 않는다. 24시간 안에 수집 중인 서비스는 모두 다시 보인다.

### 6. 외부 사례 근거 (2026-10-07 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| APM 수집에서 서비스를 자동 등록, 소유·연락처 metadata는 사람이 따로 관리 | Datadog Software Catalog ([Customize](https://docs.datadoghq.com/service_catalog/customize), [Discover](https://docs.datadoghq.com/service_catalog/customize/import_entries_dd)) | APM이 계측된 서비스를 자동으로 찾아 catalog를 채운다. on-call·소스·문서 같은 metadata는 UI·API·service definition 파일로 따로 더한다 | 채택: 자동 등록 + 사용자 관리 필드 분리. 그 분리를 DB 권한으로 강제한다(이 제품의 선택) |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 저장 위치 | 제어 DB(PostgreSQL) | ClickHouse에서 원본 집계: 사용자 관리 필드와 RLS·권한 분리가 어렵고, 목록 조회마다 원본을 scan한다 |
| 등록 주체 | ingress(ACK 뒤, 비동기) | worker: 제어 DB 접근 권한을 새로 줘야 한다. 동기 등록: 제어 DB 지연이 수집 지연이 된다 |
| owner 보호 | column 단위 GRANT | 애플리케이션 규칙만: 실수 한 번으로 agent가 owner를 지운다 |

## Rollback

- query-api의 `Services`를 비우면 목록은 404다. ingress 등록은 `Catalog`를 빼면 멈추고, 수집은 그대로다.
- migration 00006 down은 테이블을 지운다(사용자 관리 필드가 생긴 뒤에는 백업이 먼저다).

## 증거

- `internal/catalog`: tenant별 모음·늦은 관측 우선·언어 보존, 5분 cache와 만료 뒤 재기록, queue 초과 시 막지 않고 버림, 쓰기 실패 뒤 재시도, 종료 시 flush
- `internal/ingest`: ACK 뒤에만 sighting, service_id가 worker 값과 같음, Kafka 실패면 알리지 않음
- `internal/query`: 목록 page·cursor(상태 시각 고정)·다른 filter cursor 거절·limit 검증·environment 제한 key 전달
- `internal/controldb` 통합(PostgreSQL)
  - upsert(last_seen은 뒤로 가지 않음, 언어 보존), 대소문자 정렬, 상태(active·inactive·archived), archived 제외·포함
  - 정확히 30일 된 서비스는 archived(목록에서 제외), environment 제한 key의 정확한 결과, limit=1 keyset이 이름 정규형이 같은 경계를 넘음, tenant 분리
  - **앱 role의 owner_team UPDATE·INSERT 거부(42501)**
  - tenant 상한: 5,002개 중 2개 거절, 상한에서도 기존 서비스 갱신, 같은 호출 안 중복 service_id
- `internal/ingest`(추가): NUL이 든 서비스 이름은 빼고 나머지는 등록, envelope record가 없는 resource는 넣지 않음
- `internal/catalog`(추가): over_limit 집계와 cache, cache 크기 상한
- `tests/isolation`: B는 A의 서비스 이름·ID를 목록에서 보지 않는다
- `make smoke`(CI): checkout·payment·database가 ingress를 거쳐 active로 등록된다
- 경보 promtool 시험(`MontracerServiceCatalogStale`)

## 변경 이력

- 2026-10-07 (ADR 0042): `GET /api/v1/services/{service_id}` 단건 조회를 추가했다(D05 §05 서비스 상세 metadata).
  - 목록과 같은 항목 형식이고 archived도 포함한다.
  - 없는 서비스·다른 tenant·environment 제한 밖은 같은 404, 형식이 틀린 id는 400이다.
  - 저장소 `GetService`가 RLS와 environment 제한을 mandatory predicate로 건다.
