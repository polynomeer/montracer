# ADR 0076: 배포 이벤트 — `POST /deployments`·조회·저장

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-11-02 "merge하고 다음 단계 진행해줘" — [design-rollout](../plan/design-rollout.md) D3 넷째 항목)
- Owner: Backend lead (E05 — 제어 API), 화면은 E04
- 날짜: 2026-11-02 (제안·결정)
- 관련: F03(S02, M0), UC02(D01 "오류율 경보에서 서비스 상세로 이동하고 배포 이벤트와 p95 변화를 비교") · D02 §03(제어 데이터 소유·감사·outbox 한 트랜잭션), §08(서비스 자연 키 — "version과 instance는 서비스 정체성이 아니라 배포·실행 단위"), §12·§20(쓰기 POST는 Idempotency-Key 필수, 사용자 rate), §13(공통 envelope), §14("Deploy event | POST /deployments | service, env, version, timestamp") · D03 §01 표("배포 이벤트 | CI API 또는 resource version 변경 | 시각·서비스·환경·commit만 저장"), §12("deployment event는 service·environment·version·commit SHA·occurred_at·source를 포함") · D04 §01(Operator "배포 이벤트", service account에 사람 role 금지), §03(PII 최소 수집), §04(삭제·보존) · D05 §05(S02 deployments 탭, "배포 marker를 클릭하면 version 비교를 같은 시간 context에서") · 계약 1·4·5·6·7 · ADR 0015(key·role·감사 범주), 0016(controldb RLS), 0028(1시간 rollup 395일), 0034(cursor), 0037·0039(environment 제한), 0049(Idempotency-Key·판정 순서), 0064(session), 0073(서비스 키 맞추기), 0075(버전 형식)

## 배경

S02 시안은 RED 차트 위의 배포 표시선, 상단 "최근 배포" 배너, deployments 탭을 보인다(D05 §05). 계획 D3에는 "명세 확인 필요 (`deployments.write` scope만 있음)"로 남아 있었다. 확인한 명세는 이렇다.

- **D02 §14:** 쓰기 경로 `POST /deployments`(service, env, version, timestamp)를 정한다. 조회 경로는 없다.
- **D03 §12:** 내용은 "service·environment·version·commit SHA·occurred_at·source"다.
- **D03 §01:** 원천은 CI API 또는 resource version 변경이다. D03 §01 표는 "시각·서비스·환경·commit만"이라고 해 version이 빠져 있다. D02 §14(권위)와 D03 §12가 version을 포함하므로 이것을 따르고, 표는 정정 후보로 둔다.
- **D04 §01:** 쓰기는 Operator 권한이다(`deployments.write`, 이미 `internal/authz`에 있다).

정할 것:

1. 저장소·모양·삭제
2. 쓰기 계약(입력·검증·멱등·권한·한도·감사·outbox)
3. 조회 경로(명세에 없음)
4. resource version 변경 자동 감지
5. 화면(계약 5와 함께)

## 결정

### 1. 저장: 제어 DB(PostgreSQL) `deployments`

| 열 | 타입 | 뜻 |
|---|---|---|
| `tenant_id` | uuid | principal의 tenant(계약 1). RLS ENABLE+FORCE, `WithTenant`(ADR 0016) |
| `id` | uuid | |
| `service_namespace` | text | `''`이면 namespace 없음 |
| `service_name` | text | 1~255 byte(원문 대소문자 보존) |
| `service_name_normalized` | text | 소문자. 맞추기·unique용(services의 `name_normalized`와 같은 방식) |
| `environment` | text | 1~64 byte. 수집 key의 environment scope 상한(`authz` `maxEnvironmentLen`)과 같다 — 더 긴 값은 environment 제한 key로 쓰고 볼 수 없다 |
| `version` | text | 1~255 byte, 버전 글자(ADR 0075와 같다) |
| `commit` | text | git sha(hex 7~64자, 소문자). 없으면 `''` |
| `occurred_at` | timestamptz | 배포 시각 |
| `recorded_at` | timestamptz | 수신 시각 |
| `source` | text | `api`(D03 §12. 지금은 이것뿐, §4) |
| `expires_at` | timestamptz | `occurred_at` + 395일 |

- **unique:** `(tenant_id, service_namespace, service_name_normalized, environment, version, commit, occurred_at)`
  - commit 없음은 `''`라 NULL 비교 문제가 없다.
  - 대소문자만 다른 이름은 한 서비스다.
- **기록한 사람을 저장하지 않는다.** 행에는 D03 §12의 항목만 둔다.
  - 기록한 principal은 감사 이벤트(`deployment.recorded`, operations 범주)에만 남긴다.
  - 그래서 `telemetry.read`로 배포를 보는 사람이 감사 권한 없이 사람 식별자를 보지 않는다(ADR 0015 §4). 사람 식별자의 보존은 감사 보존을 따른다(D04 §04).
- **서비스는 자연 키(namespace·이름·환경)로 저장한다.** catalog의 service_id로 바꾸지 않는다.
  - 배포가 첫 telemetry보다 먼저 올 수 있고, 서비스 이름이 바뀌어도 기록이 남아야 한다(D02 §08).
  - 화면은 서비스 상태(ADR 0073)와 같은 규칙으로 서비스 행에 맞춘다.
- **ClickHouse가 아니다.** 양이 작은 제어 데이터이고 unique·RLS·감사·outbox와 같은 트랜잭션이 필요하다(D02 §03 — 제어 데이터는 control-api가 소유한다).
- **저장하지 않는 것:** 변경 설명·메시지·임의 속성. 자유 텍스트는 PII·secret이 섞일 수 있다(D04 §03). New Relic·Grafana는 이것들을 받지만, 이 명세는 범위를 좁게 정했다.
- **보존 395일:** metric 1시간 rollup 보존(ADR 0028)과 같다. 차트에 표시선을 그릴 수 있는 가장 긴 범위다.
  - 지난 행은 조회 predicate로 숨긴다(`expires_at > now`, 계약 4).
  - 지우기는 session과 같은 방식이다(`sessions.go`). 그 tenant의 쓰기 트랜잭션에서 만료 행을 최대 1,000개 지운다.
  - 이 방식은 tenant 순회 job과 RLS 우회 계정이 필요 없다. 쓰기가 멈춘 tenant의 만료 행은 보이지 않은 채 남다가 tenant 데이터 삭제 때 함께 지워진다.
- **삭제(D04 §04):**
  - tenant 폐쇄 삭제와 서비스·시간 selector 삭제 job(F09)의 대상에 `deployments`를 넣는다. selector는 서비스 자연 키와 `occurred_at`이다.
  - 삭제 tombstone 원장이 생기면 GET에 tombstone predicate를 더한다(다른 조회와 같다, ADR 0018 §7).
  - F09는 아직 구현 전이다. 이 의무를 작업계획 F09 항목에 남긴다.

### 2. 쓰기: `POST /api/v1/deployments` (control-api)

```json
{"service": {"name": "checkout", "namespace": "shop"}, "environment": "prod",
 "version": "1.4.2", "commit": "3f9c2ab1d4e5", "timestamp": "2026-11-02T05:10:00Z"}
```

**판정 순서:** ADR 0049와 같다.

1. 인증·권한 → 401·403
2. `Idempotency-Key` → 400
3. 본문 검증 → 400
4. 같은 key 재생 → 첫 응답, 다른 body면 409
5. 한도 → 429
6. 쓰기

**권한:**

- `deployments.write`다.
  - API key(CI가 쓴다): 이 scope를 가진 key
  - session: Operator 이상(D04 §01)
  - ingest key는 거절한다(D02 §12).
- environment 제한 principal(ADR 0039)은 자기 environment만 쓸 수 있다. 다른 environment는 403이다.
- tenant는 principal에서만 온다. body·header의 tenant는 무시한다(계약 1).
- **API key 수명(한계):** key의 실효 권한은 "scope ∩ 발급자의 현재 role"이다(ADR 0015 §1). 발급자가 Operator 아래로 바뀌거나 떠나면 CI 기록이 403이 된다.
  - D04 §01은 service account에 사람 role을 쓰지 말라고 한다. service account가 생기면 그것으로 옮긴다(재검토 조건).
  - 그때까지 CI 문서에 "배포 기록은 실패해도 배포를 막지 않는 선택 단계로 호출"을 적는다.

**검증:**

| 필드 | 규칙 |
|---|---|
| `service.name` | 1~255 byte |
| `service.namespace` | 0~255 byte |
| `environment` | 1~64 byte |
| `version` | 1~255 byte, `[A-Za-z0-9._+~-]`(ADR 0075와 같다 — 표시선·조건·버전 facet이 같은 값을 가리키게) |
| `commit` | 생략 가능, hex 7~64자 |
| `timestamp` | 생략하면 수신 시각. 수신 시각 − 30일 ~ + 5분 |

- 그 밖의 필드는 400이다.
- 30일 근거: 늦게 보내는 CI 재시도·backfill은 받되, 오래된 사건을 지어 넣지 못하게 한다. 5분은 시계 차이다(조사 context 미래 허용과 같다).

**멱등성:**

- `Idempotency-Key` header는 **필수**다(D02 §12·§20, ADR 0049). 없으면 400이다.
  - 24시간 보관한다. 같은 key·같은 body면 첫 응답(201)을 그대로 돌려주고, 다른 body면 409다.
  - `timestamp`를 생략한 CI 재시도도 같은 key로 한 행이 된다.
- 위 unique는 보조 수단이다. key가 달라도 같은 (서비스 키, 환경, version, commit, occurred_at)이면 새 행을 만들지 않고 200과 기존 행이다.
  - 보조 경로는 감사·outbox도 남기지 않는다. 바뀐 것이 없다.
- 같은 version을 나중에 다시 배포(rollback 뒤 재배포)하면 시각이 달라 새 행이다.

**한도:**

- 새 행만 센다. tenant마다 분당 60건이다. 넘으면 429(`Retry-After`)다.
- 셈은 DB에서 한다. 쓰기 트랜잭션에서 그 tenant의 최근 1분 `recorded_at` 행 수를 센다. 그래서 replica 수와 무관하다.
- 재생(같은 key)과 같은 행(200)은 세지 않는다.
- D02 §12의 사용자 rate(60/분·burst 20)와 조직 quota는 그것대로 따로 적용한다.
- 60은 시작값이다. CI 배포 빈도보다 넉넉하고, 실수로 반복 호출하는 것은 막는다.

**기록:**

- 감사 `deployment.recorded`(operations 범주), outbox event `deployment.recorded`를 쓰기와 같은 트랜잭션에 넣는다(D02 §03, 기존 `writeAuditAndOutbox`).
  - outbox 소비자는 아직 없다. 표시선 배너·알림 연동 같은 후속 소비자가 받을 수 있게 남긴다.
  - 감사 details와 event payload에는 서비스 키·환경·version·commit만 둔다.
- 응답은 공통 envelope의 `data`(저장한 행, 기록자 없음)다.

### 3. 조회: `GET /api/v1/deployments` (control-api)

명세(D02 §14)에는 조회 경로가 없다. 화면(D05 §05 deployments 탭·marker)에 필요해 더한다(명세 반영 후보).

- 입력:
  - `service_name`(필수, 대소문자 무시)
  - `service_namespace`: 없으면 모든 namespace, 있으면 그 값만. `service_namespace=`(빈 값)은 namespace 없는 서비스만이다.
  - `environment`
  - `from`·`to`(RFC3339, 생략하면 최근 7일, 최대 395일)
  - `limit`(기본 100, 최대 500), `cursor`
- 정렬: `occurred_at` 내림차순(동률은 id). cursor는 서명한다(ADR 0034).
- 응답: 공통 envelope(`data`·`meta`·`next_cursor`, D02 §13). 빈 결과는 `data: []`(200)다.
  - 화면은 "기록된 배포 없음"이라고 말한다. "배포 없음"이라고 하지 않는다. 배포 이벤트를 보내지 않는 조직에서는 아무 것도 모르는 것이다(계약 6).
  - 404(기능 꺼짐)와 빈 결과를 화면이 가른다.
- **권한:** `telemetry.read`다. 배포 이벤트는 telemetry를 볼 수 있는 사람이 같은 맥락에서 본다.
  - environment 제한 principal의 `environment` 파라미터:
    - 생략하면 허용 environment 행만 predicate로 건다.
    - 허용 밖 environment를 명시하면 403이다. "없음"과 "못 봄"을 섞지 않는다(ADR 0037 §3과 같은 원칙).
- **mandatory predicate(계약 4):** tenant(RLS·`WithTenant`), 시간, `expires_at`, environment 범위. 삭제 tombstone은 F09와 함께 넣는다.

### 4. resource version 변경 자동 감지는 이번에 하지 않는다

D03 §01은 두 번째 원천으로 "resource version 변경"을 든다. 이번에는 API만 한다.

- 자동 감지는 서비스·환경마다 마지막으로 본 version 상태가 필요하다(worker 상태, 늦게 온 데이터, 여러 version이 동시에 도는 canary).
- 표본 span에서 감지하면 시각이 실제 배포보다 늦거나 빗나간다(계약 5와 같은 이유).
- 자동 감지는 재검토 조건이다. `source` 열은 그때 `detected`를 더하려고 둔다.

### 5. 화면 (다음 PR)

- **S02 deployments 탭:** 기록된 배포 목록(시각·version·commit·환경·namespace). commit은 글자 그대로 보이고 링크를 만들지 않는다(저장소 URL을 모른다).
- **S02 RED 차트:** 범위 안의 배포 시각에 표시선을 긋는다. 선만 있고 값은 없다.
  - 선마다 키보드로 갈 수 있는 목록(표 대안)을 둔다.
  - 범위가 보존(395일)을 넘는 부분은 "배포 기록 보존 밖"이라고 말한다(UC02 "이유 표시").
- **version 비교(D05 §05 "같은 시간 context"):**
  - 이번 해석은 표시선을 고르면 배포 전후 범위로 같은 서비스 RED를 여는 것이다. RED는 비샘플링 metric이다(계약 5).
  - span·log의 version 열(ADR 0075)로 오류율·지연을 비교하지 않는다.
  - metric의 version별 나눔은 metric resource에 `service.version` label이 있고 metric 조회 group 범위(ADR 0047) 안일 때만 한다(화면 PR에서 확인). 아니면 전후 시간 비교만 한다.
- **flag:** `MONTRACER_DEPLOYMENTS_API_ENABLED`(기본 true). 끄면 두 경로가 404이고, 화면은 탭·표시선을 그리지 않는다(빈 결과와 다르다).

### 6. 운영 장치 (DoD, D06 §02)

- **지표:**
  - 요청 수·지연은 기존 control-api route 지표로 본다.
  - 따로 둔다:
    - `montracer_deployments_recorded_total{outcome}` — `created`·`existing`·`replayed`·`rate_limited`·`invalid`
    - `montracer_deployments_expired_deleted_total`
- **경보:** 두지 않는다. 쓰기 실패는 기존 control-api 5xx 경보 범위다. runbook 절은 경보를 더할 때 쓴다.

## 명세 반영 (계약 7)

D02·D03 원본(docx) 개정 후보다. 원본 개정은 문서 책임자의 일이고, 그때까지 이 ADR과 OpenAPI가 기준이다.

- D02 §14 표에 `GET /deployments`(조회), commit 필드
- D02 §11 제어 테이블 목록에 `deployments`
- D03 §01 표의 저장 항목에 version(§12·D02 §14와 맞춤)

## 외부 사례 근거 (2026-11-02 확인)

| 사례 | 확인한 동작 | 이 결정에 쓴 부분 |
|---|---|---|
| Grafana annotations API ([docs](https://grafana.com/docs/grafana/latest/developers/http_api/annotations/)) | `POST /api/annotations`(time·timeEnd·text·tags), dashboard 없이 쓰면 조직 annotation. `GET`은 `from`·`to`·`tags`·`limit`(기본 100) | §2 시각을 받는 쓰기 경로, §3 범위·limit 조회(기본 100) |
| New Relic change tracking ([docs](https://docs.newrelic.com/docs/change-tracking/change-tracking-introduction/)) | 배포를 API·CLI·Jenkins·GitHub Actions로 기록. commit SHA·변경 설명·사용자 정보·임의 속성을 받는다. 성능 차트에 marker를 겹친다 | §2 CI가 쓰는 API key, §5 차트 표시선. 변경 설명·사용자·임의 속성은 D03 §12 범위 밖이라 받지 않는다 |
| Datadog Deployment Tracking ([docs](https://docs.datadoghq.com/tracing/services/deployment_tracking/)) | `version` tag로 버전별 요청·오류를 나누고, 범위 안의 version과 첫·마지막 관측 시각을 보인다 | §4 resource version 원천(자동 감지는 미룸), §5 version 비교 |

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A (채택) 제어 DB, 자연 키, API만 | 멱등·RLS·감사·outbox 트랜잭션, 서비스보다 먼저 와도 됨, 명세 범위 그대로 | 자동 감지 없음(배포 이벤트를 보내지 않으면 표시선 없음) |
| B ClickHouse 이벤트 표 | 차트 query와 같은 저장소 | 멱등 unique 없음(ReplacingMergeTree 지연), 감사 트랜잭션 분리, 쓰기 경로가 수집 경로와 섞임 |
| C resource version 변경 자동 감지 | 설치만으로 표시선 | worker 상태·canary·늦은 데이터, 표본 span이면 시각 오차(계약 5). 실측 근거 없음 |
| D catalog service_id로 저장 | join이 쉬움 | 서비스가 아직 없으면 거절하거나 지어내야 함, 이름 변경에 약함 |

## 결과

- 이점: UC02의 "배포 이벤트와 p95 변화 비교"와 S02 deployments 탭·표시선이 풀린다.
- 비용: 제어 DB 표 하나, 경로 둘
- 영향 받는 계약:
  - migration(PostgreSQL): `deployments`(RLS ENABLE+FORCE, `montracer_rw` 최소 GRANT, unique, 조회·만료 index)
  - OpenAPI: `POST`·`GET /api/v1/deployments`
  - 감사·outbox: `deployment.recorded`
- 필요한 시험:
  - **쓰기:**
    - 검증(필드·형식·길이·시각 범위·모르는 필드)
    - 멱등:
      - key 없음 400
      - 같은 key 재생·다른 body 409
      - key가 달라도 같은 값은 한 행(200, 감사 없음), 다른 시각은 새 행
      - timestamp 생략 + 같은 key 재시도는 한 행
      - 이름 대소문자만 다르면 한 행, commit 없음 둘은 한 행
    - 권한:
      - API key scope, Operator 미만 403, ingest key 거절
      - 발급자 강등 뒤 key 403
      - environment 제한 key의 다른 환경 403
    - 한도: 새 행만 셈, 429
    - 감사·outbox 같은 트랜잭션. 응답·행에 기록자 없음
  - **조회:**
    - 범위·정렬·cursor
    - 이름 대소문자, namespace 생략·빈 값·지정
    - environment 제한(생략 = 허용 범위만, 명시 밖 403)
    - `expires_at`, 빈 결과 200 envelope
  - **격리:** 다른 tenant 행 안 보임, body·header tenant 무시(`tests/isolation`), RLS 통합
  - **보존:** 쓰기 때 그 tenant의 만료 행만 지움(최대 1,000)
  - **flag off:** 404
  - **화면:** 404와 빈 결과 구별

## 구현 순서

1. 이 ADR(이 PR). 작업계획 F09 항목에 "deployments 삭제 대상"을 남긴다.
2. 백엔드:
   - migration, `internal/controldb` 저장·조회·만료, control-api route·flag·한도·감사·outbox·지표, OpenAPI
   - 시험(단위·통합·격리)
   - 문서: 아키텍처 문서, `cmd/control-api/README.md`, CI 호출 안내
3. 화면:
   - S02 deployments 탭·RED 표시선·전후 비교 링크
   - 상태 계약 시험(기록 없음 ≠ 배포 없음 ≠ 꺼짐)
   - 기능 상태 등록부 F03·작업계획

## Rollback

- `MONTRACER_DEPLOYMENTS_API_ENABLED=false`면 두 경로가 404이고, 화면은 탭·표시선을 그리지 않는다.
- 표는 남겨도 해가 없다(expand-only). 되돌릴 때는 control-api를 revert한다.
- CI 쪽 호출은 404를 받는다. 선택 단계로 호출하도록 안내했으므로 배포를 막지 않는다.

## 재검토 조건

- 배포 이벤트를 보내지 않는 조직이 많아 표시선이 비어 있으면: resource version 변경 자동 감지(후보 C, metric resource 기준)
- service account가 생기면: CI 기록을 그것으로 옮긴다(사람 role 의존 제거)
- 변경 설명·링크 요구가 생기면: D03 §12 저장 범위 개정 + PII 정책(D04 §03)
- 분당 60건 한도에 닿는 조직이 생기면: 한도 조정(실측)

## 증거

- 설계 검토(spec-reviewer, 2026-11-02): P0 없음.
  - P1 3건 반영:
    - Idempotency-Key 필수(D02 §12·§20)
    - 기록자는 감사에만(행·응답에서 뺌)
    - 삭제 job 대상·tombstone predicate·F09 의무 기록
  - P2 반영: D03 §12 근거·§01 표 정정 후보, environment 64 byte, unique(소문자 이름·commit `''`), outbox, 한도 셈 위치·대상·순서, API key 수명 한계·service account, environment 제한 GET·namespace 빈 값·envelope, version 비교 해석과 보존 밖 안내, 지표·만료 방식, flag off 구별, Epic 표기.

## 변경 이력

- 2026-11-02 제안·결정. 설계 검토 반영(위 증거).
