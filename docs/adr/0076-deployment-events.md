# ADR 0076: 배포 이벤트 — `POST /deployments`·조회·저장

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-11-02 "merge하고 다음 단계 진행해줘" — [design-rollout](../plan/design-rollout.md) D3 넷째 항목)
- Owner: Backend lead (E05)
- 날짜: 2026-11-02 (제안·결정)
- 관련: F03(S02), UC02(D01 "오류율 경보에서 서비스 상세로 이동하고 배포 이벤트와 p95 변화를 비교") · D02 §03(제어 데이터 소유), §08(서비스 자연 키 — "version과 instance는 서비스 정체성이 아니라 배포·실행 단위"), §14("Deploy event | POST /deployments | service, env, version, timestamp") · D03 §01 표("배포 이벤트 | CI API 또는 resource version 변경 | 시각·서비스·환경·commit만 저장") · D04 §01(Operator "배포 이벤트"), §03(PII 최소 수집) · D05 §05(S02 deployments 탭, "배포 marker를 클릭하면 version 비교를 같은 시간 context에서") · 계약 1·4·5·6 · ADR 0015(key·role), 0016(controldb RLS), 0049(Idempotency-Key), 0064(session), 0073(서비스 키 맞추기), 0075(resource 열)

## 배경

S02 시안은 RED 차트 위의 배포 표시선, 상단 "최근 배포" 배너, deployments 탭을 보인다(D05 §05). 계획 D3에는 "명세 확인 필요 (`deployments.write` scope만 있음)"로 남아 있었다. 명세를 확인한 결과는 이렇다.

- **D02 §14:** 쓰기 경로 `POST /deployments`와 필드(service, env, version, timestamp)를 정한다. 조회 경로는 없다.
- **D03 §01:** 원천은 CI API 또는 resource version 변경이다. 저장은 "시각·서비스·환경·commit만"이다.
- **D04 §01:** 배포 이벤트 쓰기는 Operator 권한이다(`deployments.write`, 이미 `internal/authz`에 있다).
- **D05 §05:** S02 deployments 탭과 marker가 있다.

정할 것:

1. 저장소와 모양
2. 쓰기 계약(입력·검증·멱등·권한·한도)
3. 조회 경로(명세에 없음)
4. resource version 변경으로 자동 감지할지
5. 화면에서 무엇을 보이나(계약 5와 함께)

## 결정

### 1. 저장: 제어 DB(PostgreSQL) `deployments`

| 열 | 타입 | 뜻 |
|---|---|---|
| `tenant_id` | uuid | principal의 tenant(계약 1). RLS ENABLE+FORCE, `WithTenant`(ADR 0016) |
| `id` | uuid | |
| `service_namespace` | text | `''`이면 namespace 없음 |
| `service_name` | text | 1~255 byte |
| `environment` | text | 1~255 byte(`deployment.environment.name`) |
| `version` | text | 1~255 byte, 형식은 ADR 0075 버전 글자 |
| `commit` | text NULL | git sha(hex 7~64자). 없으면 NULL |
| `occurred_at` | timestamptz | 배포 시각(요청 값, 없으면 수신 시각) |
| `recorded_at` | timestamptz | 수신 시각 |
| `source` | text | `api`(지금은 이것뿐, §4) |
| `actor_kind`, `actor_id` | text | 기록한 principal(감사와 같은 값) |
| `expires_at` | timestamptz | `occurred_at` + 395일 |

- **서비스는 자연 키(namespace·이름·환경)로 저장한다.** catalog의 service_id로 바꾸지 않는다.
  - 배포가 첫 telemetry보다 먼저 올 수 있고, 서비스 이름이 바뀌어도 기록이 남아야 한다(D02 §08).
  - 화면은 서비스 상태(ADR 0073)와 같은 규칙으로 서비스 행에 맞춘다. 이름은 대소문자를 무시한다.
- **ClickHouse가 아니다.** 양이 작은 제어 데이터이고 멱등성(unique)·RLS·감사와 같은 트랜잭션이 필요하다(D02 §03 제어 데이터 소유는 control-api).
- **보존 395일:** metric 1시간 rollup 보존(ADR 0028)과 같다. 차트에 표시선을 그릴 수 있는 가장 긴 범위다. 매일 지운다(`expires_at` 지난 행). 조회는 `expires_at`도 predicate로 건다(계약 4).
- **저장하지 않는 것:** 변경 설명(changelog)·배포한 사람 이름·메시지·임의 속성. D03 §01이 저장 항목을 정한다. 자유 텍스트는 PII·secret이 섞일 수 있다(D04 §03). New Relic·Grafana는 메시지·태그를 받지만, 이 명세는 범위를 좁게 정했다.

### 2. 쓰기: `POST /api/v1/deployments` (control-api)

```json
{"service": {"name": "checkout", "namespace": "shop"}, "environment": "prod",
 "version": "1.4.2", "commit": "3f9c2ab1d4e5", "timestamp": "2026-11-02T05:10:00Z"}
```

**권한:**

- `deployments.write`다.
  - API key(CI가 쓴다, ADR 0015): 이 scope를 가진 key
  - session: Operator 이상(D04 §01)
- environment 제한 principal(ADR 0039)은 자기 environment만 쓸 수 있다. 다른 environment는 403이다.
- tenant는 principal에서만 온다. body·header의 tenant는 무시한다(계약 1).

**검증:**

| 필드 | 규칙 |
|---|---|
| `service.name` | 1~255 byte |
| `service.namespace` | 0~255 byte |
| `environment` | 1~255 byte |
| `version` | 1~255 byte, 버전 글자 `[A-Za-z0-9._+~-]`(ADR 0075와 같다 — 표시선·조건·버전 facet이 같은 값을 가리키게) |
| `commit` | 생략 가능, hex 7~64자(소문자로 저장) |
| `timestamp` | 생략하면 수신 시각. 수신 시각 − 30일 ~ + 5분 |

- 그 밖의 필드는 400이다.
- 30일 근거: 늦게 보내는 CI 재시도·backfill을 받되, 오래된 사건을 지어 넣지 못하게 한다. 5분은 시계 차이다(조사 context 미래 허용과 같다).

**멱등성:**

- `Idempotency-Key` header는 선택이다. 있으면 ADR 0049와 같이 24시간 보관한다. 같은 key·같은 body면 첫 응답을 돌려주고, 다른 body면 409다.
- header가 없어도 **같은 (서비스 키, 환경, version, commit, occurred_at)은 한 행**이다(unique). 다시 보내면 200과 기존 행이다. CI의 단순 재시도가 표시선을 겹쳐 그리지 않는다.
- 같은 version을 나중에 다시 배포(rollback 뒤 재배포)하면 시각이 달라 새 행이다.

**한도와 기록:**

- 한도: tenant마다 분당 60건이다. 넘으면 429(`Retry-After`)다. CI 배포 빈도보다 넉넉하고, 실수로 반복 호출하는 것은 막는다(시작값).
- 응답은 201(새 행)이나 200(같은 행)이다. 본문은 저장한 행이다.
- 감사: `deployment.recorded`(operations 범주, ADR 0015 §4). 같은 트랜잭션에 넣는다. 감사 details에는 서비스 키·환경·version만 둔다.

### 3. 조회: `GET /api/v1/deployments` (control-api)

명세(D02 §14)에는 조회 경로가 없다. 화면(D05 §05 deployments 탭·marker)에 필요해 더한다(명세 반영 후보).

- 입력: `service_name`(필수), `service_namespace`, `environment`, `from`·`to`(RFC3339, 생략하면 최근 7일, 최대 395일), `limit`(기본 100, 최대 500), `cursor`
- 정렬: `occurred_at` 내림차순(동률은 id). cursor는 서명한다(ADR 0034).
- 권한: `telemetry.read`다. 배포 이벤트는 telemetry를 볼 수 있는 사람이 같은 맥락에서 본다.
  - environment 제한 principal은 자기 environment 행만 본다(predicate).
  - 서비스 이름은 대소문자를 무시하고 맞춘다.
- `namespace` 생략: 이름이 같은 모든 namespace를 준다. 행마다 `service.namespace`가 있어 화면이 가른다.
- 빈 결과는 200 `[]`다.
  - 화면은 "기록된 배포 없음"이라고 말한다. "배포 없음"이라고 하지 않는다.
  - 배포 이벤트를 보내지 않는 조직에서는 아무 것도 모르는 것이다(계약 6).

### 4. resource version 변경 자동 감지는 이번에 하지 않는다

D03 §01은 두 번째 원천으로 "resource version 변경"을 든다. 이번에는 API만 한다.

- 자동 감지는 서비스·환경마다 마지막으로 본 version 상태가 필요하다(worker 상태, 늦게 온 데이터, 여러 version이 동시에 도는 canary).
- 표본 span에서 감지하면 시각이 실제 배포보다 늦거나 빗나간다(계약 5와 같은 이유).
- 대신 S02는 metric resource의 version으로 "이 범위에서 본 version"을 따로 보일 수 있다(다음 단계). 자동 감지는 재검토 조건이다.
- `source` 열은 그때 `detected`를 더하려고 둔다.

### 5. 화면 (다음 PR)

- **S02 deployments 탭:** 기록된 배포 목록(시각·version·commit·환경·namespace). commit은 글자 그대로 보이고 링크를 만들지 않는다(저장소 URL을 모른다).
- **S02 RED 차트:** 범위 안의 배포 시각에 표시선을 긋는다. 선만 있고 값은 없다. 선마다 키보드로 갈 수 있는 목록(표 대안)을 둔다.
- **version 비교(D05 §05 "같은 시간 context"):** 표시선을 고르면 배포 전후 범위로 같은 서비스 RED를 연다. RED는 비샘플링 metric이다(계약 5).
  - span·log의 version 열(ADR 0075)로 오류율·지연을 비교하지 않는다.
  - metric resource의 version별 비교는 metric 조회의 group label 범위 안에서만 한다(ADR 0047).
- **flag:** `MONTRACER_DEPLOYMENTS_API_ENABLED`(기본 true). 끄면 두 경로가 404이고 화면은 탭·표시선을 그리지 않는다.

## 명세 반영 (계약 7)

D02 원본(docx) 개정 후보다. 원본 개정은 문서 책임자의 일이고, 그때까지 이 ADR과 OpenAPI가 기준이다.

- §14 표에 `GET /deployments`(조회), commit 필드
- §11 제어 테이블 목록에 `deployments`

## 외부 사례 근거 (2026-11-02 확인)

| 사례 | 확인한 동작 | 이 결정에 쓴 부분 |
|---|---|---|
| Grafana annotations API ([docs](https://grafana.com/docs/grafana/latest/developers/http_api/annotations/)) | `POST /api/annotations`(time·timeEnd·text·tags), dashboard 없이 쓰면 조직 annotation. `GET`은 `from`·`to`·`tags`·`limit`(기본 100) | §2 시각을 받는 쓰기 경로, §3 범위·limit 조회(기본 100) |
| New Relic change tracking ([docs](https://docs.newrelic.com/docs/change-tracking/change-tracking-introduction/)) | 배포를 API·CLI·Jenkins·GitHub Actions로 기록. commit SHA·변경 설명·사용자 정보·임의 속성을 받는다. 성능 차트에 marker를 겹친다 | §2 CI가 쓰는 API key, §5 차트 표시선. 변경 설명·사용자·임의 속성은 D03 §01 저장 범위 밖이라 받지 않는다 |
| Datadog Deployment Tracking ([docs](https://docs.datadoghq.com/tracing/services/deployment_tracking/)) | `version` tag로 버전별 요청·오류를 나누고, 범위 안의 version과 첫·마지막 관측 시각을 보인다 | §4 resource version 원천(자동 감지는 미룸), §5 version 비교 |

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A (채택) 제어 DB, 자연 키, API만 | 멱등·RLS·감사 트랜잭션, 서비스보다 먼저 와도 됨, 명세 범위 그대로 | 자동 감지 없음(배포 이벤트를 보내지 않으면 표시선 없음) |
| B ClickHouse 이벤트 표 | 차트 query와 같은 저장소 | 멱등 unique 없음(ReplacingMergeTree 지연), 감사 트랜잭션 분리, 쓰기 경로가 수집 경로와 섞임 |
| C resource version 변경 자동 감지 | 설치만으로 표시선 | worker 상태·canary·늦은 데이터, 표본 span이면 시각 오차(계약 5). 실측 근거 없음 |
| D catalog service_id로 저장 | join이 쉬움 | 서비스가 아직 없으면 거절하거나 지어내야 함, 이름 변경에 약함 |

## 결과

- 이점: UC02의 "배포 이벤트와 p95 변화 비교"와 S02 deployments 탭·표시선이 풀린다.
- 비용: 제어 DB 표 하나, 매일 지우는 job, 경로 둘
- 영향 받는 계약:
  - migration(PostgreSQL): `deployments`(RLS, `montracer_rw` 최소 GRANT, 만료 index)
  - OpenAPI: `POST`·`GET /api/v1/deployments`
  - 감사 action `deployment.recorded`
- 필요한 시험:
  - **쓰기:**
    - 검증(필드·형식·시각 범위·모르는 필드)
    - 멱등(Idempotency-Key 재생·다른 body 409·header 없는 같은 값 한 행·다른 시각 새 행)
    - 권한(api key scope·Operator 미만 403·environment 제한 key의 다른 환경 403)
    - 한도 429, 감사 같은 트랜잭션
  - **조회:** 범위·정렬·cursor·이름 대소문자·namespace 생략·environment 제한 predicate·`expires_at`·빈 결과 200
  - **격리:** 다른 tenant 행 안 보임, body·header tenant 무시(`tests/isolation`), RLS 통합
  - **보존:** 만료 job이 395일 지난 행만 지움
  - **flag off:** 404

## 구현 순서

1. 이 ADR(이 PR)
2. 백엔드:
   - migration, `internal/controldb` 저장·조회·만료, control-api route·flag·한도·감사, OpenAPI
   - 시험(단위·통합·격리)
   - 문서: 아키텍처 문서, `cmd/control-api/README.md`, 운영(만료 job 실패는 기존 경보 범위인지 확인)
3. 화면:
   - S02 deployments 탭·RED 표시선·전후 비교 링크
   - 상태 계약 시험(기록 없음 ≠ 배포 없음)

## Rollback

`MONTRACER_DEPLOYMENTS_API_ENABLED=false`면 두 경로가 404이고 화면은 탭·표시선을 그리지 않는다. 표는 남겨도 해가 없다(expand-only). 되돌릴 때는 control-api를 revert한다. CI 쪽 호출은 404를 받는다 — CI가 배포를 실패로 보지 않도록 문서에 "선택 단계로 호출"을 적는다.

## 재검토 조건

- 배포 이벤트를 보내지 않는 조직이 많아 표시선이 비어 있으면: resource version 변경 자동 감지(후보 C, metric resource 기준)
- 변경 설명·링크 요구가 생기면: D03 §01 저장 범위 개정 + PII 정책(D04 §03)
- 분당 60건 한도에 닿는 조직이 생기면: 한도 조정(실측)

## 증거

- 설계 검토(spec-reviewer): 이 PR에서 받는다.

## 변경 이력

- 2026-11-02 제안·결정
