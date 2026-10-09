# ADR 0049: monitor 정의 API — MonitorSpec, validate·CRUD, Idempotency-Key, If-Match

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-09 "E05 Monitor" 선택)
- Owner: API lead
- 날짜: 2026-10-09 (제안·결정)
- 관련: F06, E05 · D02 §11·§14·§17·§19·§20, D04 §01, D05 §09 · 계약 4·6 · ADR 0014, 0015, 0016, 0027, 0034, 0042

## 배경

E05(F06 임계치 monitor)는 크다. 이 ADR은 첫 단계인 **monitor 정의를 저장하는 경로**를 정한다. 나머지는 후속 PR이다.

| 단계 | 내용 | 상태 |
|---|---|---|
| A (이 ADR) | MonitorSpec 검증·정규화, `POST /monitors/validate`, CRUD, Idempotency-Key, If-Match, 감사·outbox | 이 PR |
| B | alert-worker 평가기: scheduler lease, 상태 머신(OK·PENDING·ALERT·RECOVERING·NO_DATA·EVALUATION_ERROR), alert_instances, 24시간 dry-run | 후속 |
| C | notification policy와 webhook(HMAC·SSRF 차단·at-least-once) | 후속 |
| D | S09 화면 | 후속 |

명세가 정한 것은 다음과 같다.
- **경로(D02 §14·§20):** `POST /monitors/validate`(`monitors.write`, 200 `normalized_spec`·`warnings`·`dry_run`), `GET/POST /monitors`, `GET/PUT/DELETE /monitors/{id}`
- **생성 최소 계약 예시(D02 §14):** name, query, window·evaluation, condition, minimum_requests, for_seconds, no_data, notification_policy_id
- **변경 공통 규약(D02 §20):**
  - 모든 POST mutation은 Idempotency-Key가 필요하다. tenant·principal·경로·key별로 24시간 보관하고, 다른 body면 409다.
  - PUT은 If-Match가 없으면 428, revision이 다르면 412다.
- **제어 데이터(D02 §11):** monitors(spec, revision, status — "immutable evaluation version"), RLS, revision 충돌을 덮어쓰지 않는다.

명세가 정하지 않았거나 예시가 지금 구현과 맞지 않는 것이 있다.
- D02 §14 예시는 오류율을 counter 두 개(`http.server.errors` / `http.server.requests`)의 비율로 쓴다. 이 제품의 비샘플링 RED 원천은 HTTP 서버 histogram의 status별 count다(ADR 0042). 그리고 metric filter는 eq만 지원해 5xx 범위를 표현할 수 없다(ADR 0027).
- 428의 오류 코드가 D02 §19 코드 표에 없다.
- notification policy 저장소가 아직 없다.

## 결정

### 1. MonitorSpec과 정규형 (`internal/monitor`)

- **query.kind 두 가지**
  - `error_ratio`: HTTP 서버 histogram `http.server.request.duration`의 5xx 수 / 전체 수다. 서비스 RED 오류율과 같은 정의다(ADR 0042, 계약 5). aggregation은 받지 않는다.
    - 원천 metric은 이것만 받는다(다른 이름은 422). 5xx/전체는 HTTP 서버 histogram과 status label을 전제로 해서, counter·gauge를 지정하면 의미가 깨진다(리뷰 반영).
  - `metric`: metric 하나의 집계 값(ADR 0027의 aggregation)이다.
  - 둘 다 filter(and·eq, 깊이 4, 조건 20)와 group_by(5개)를 받는다.
  - D02 §14 예시의 `ratio_denominator`(counter 두 개의 비율)는 지금 받지 않는다. 다른 metric 간 연산이 없어서다(ADR 0027 §5). 다른 최상위 필드 이름은 예시 그대로다.
- **한도**
  - `evaluation_seconds`는 30·60·300이다(D02 §14).
  - `window_seconds`는 60~86,400이고 evaluation의 배수이며 **60의 배수**다. metric rollup이 1분 단위라서다(ADR 0026).
  - `for_seconds`는 **필수**이고 0~86,400, evaluation의 배수다. 생략을 0(한 번 위반으로 ALERT)으로 보면 노이즈 알림이 된다(D02 §17 시작값은 2분 유지). 기본값을 지어내지 않고 no_data처럼 명시하게 한다(리뷰 반영).
  - 비율 threshold는 0~1 fraction이다(UI %는 화면이 바꾼다, D05 §09). metric threshold는 유한한 수다.
  - `minimum_requests`는 error_ratio만 받고 기본 100이다(D02 §17 시작값).
    - **평가 의미(단계 B가 따른다):** window의 전체 요청 수가 minimum_requests 미만이면 위반·정상을 판정하지 않고 **NO_DATA**로 보며 no_data 정책을 적용한다(D02 §21 "쿼리는 성공했지만 최소 데이터 조건을 충족하지 않음 = NO_DATA"). OK로 보지 않는다(계약 6).
- **no_data는 필수이고 OK로 바꾸는 선택지가 없다**(D02 §17 "missing 정책은 monitor마다 명시", 계약 6).
  - `{"action":"no_data"}`는 NO_DATA 상태로 둔다.
  - `{"action":"alert","after_seconds":N}`(N ≥ window)은 결측이 N초 이어지면 ALERT로 둔다.
  - D02 §14 예시의 문자열 `"alert_after_600s"`도 받아 객체로 정규화한다.
- **recovery_evaluations는 2로 고정**(D02 §17)이다. 정규형에 드러내고, 다시 보낼 때 2가 아니면 422다.
- **notification_policy_id**는 정책 저장소(단계 C) 전에는 null만 받는다. 값이 있으면 422다.
- **정규형:** 기본값을 모두 채운 명시적 JSON이다. 저장된 revision만 보고 평가 의미가 정해진다. 정규형을 그대로 다시 보내면 같은 정규형이다(조회 → 수정 왕복).
- **오류:** 형식 오류는 400과 field violation, "아직 지원하지 않음"만 있으면 422, 모르는 필드는 400, 본문 64KiB 초과는 413이다(D02 §19).
- **경고**(저장은 된다): `for_seconds_zero`, `minimum_requests_zero`, `evaluation_below_rollup_resolution`(30초 평가는 1분 rollup을 다시 읽는다), `dry_run_unavailable`

### 2. 저장 (migration 00007, `controldb.MonitorStore`)

- **`monitors`:** 현재 정의, revision, enabled, tombstone(`deleted_at`)
- **`monitor_revisions`:** revision마다 바뀌지 않는 정규형 spec(+삭제 표시)이다. 평가(단계 B)가 (monitor_id, revision)으로 정확한 정의를 가리킨다(D02 §11 "immutable evaluation version").
- **`idempotency_keys`:** 결정 3
- **세 테이블 모두:** ENABLE·FORCE RLS(tenant), 자식 FK에 tenant_id 포함(D02 §11)
- **앱 role 권한:**
  - monitors: SELECT·INSERT·UPDATE(정해진 컬럼). DELETE는 없어 tombstone으로만 지운다.
  - revision 이력: SELECT·INSERT만
  - idempotency_keys: SELECT·INSERT·DELETE(만료 행)
- **한 트랜잭션:** 정의, revision 이력, 감사, outbox(`monitor.created|updated|deleted`)를 함께 쓴다(D02 §11·§14).
  - 감사 범주는 **operations**다(ADR 0015 §4, Operator도 본다).
  - 감사·outbox에는 spec 본문 대신 revision·enabled·삭제 여부·spec SHA-256만 둔다. filter 값은 자유 입력이라서다.
  - threshold 같은 내용은 revision 이력에 있다(D05 §09 "임계값 변경은 audit와 새 revision에 기록").

### 3. Idempotency-Key (모든 POST mutation의 공통 규약, D02 §20)

- 이 PR에서는 `POST /monitors`에 적용한다. validate는 상태를 바꾸지 않아 필요 없다.
- **header:** `Idempotency-Key`, 보이는 ASCII 1~255자다. 없거나 형식이 틀리면 400이다.
- **저장:** 키는 (tenant, principal 종류·주체, `METHOD path`, key)다. 요청 본문 원문의 SHA-256, 응답 status·body를 **변경과 같은 트랜잭션**에 쓰고 24시간 보관한다.
  - 같은 key·같은 본문이면 처음 응답 그대로다. `Idempotent-Replayed: true`를 붙이고 새로 만들지 않는다.
  - 같은 key·다른 본문이면 409 `CONFLICT`다(D02 §12 "같은 tenant·principal·route·key로 다른 body hash가 오면 409", §20).
  - 같은 key의 **동시 요청**은 기본 키 충돌로 하나만 commit된다. 나머지는 rollback 뒤 다시 읽어 저장된 응답을 돌려준다(최대 3번). 그래도 경합하면 503(재시도 가능)이다 — 변경은 모두 rollback됐다.
  - 만료된 key는 지우고 새 요청으로 본다.
- **성공 응답만 저장한다.** 검증 오류·충돌·DB 장애는 저장하지 않고 재시도할 수 있다(변경도 rollback됐다).
- 만료 행 정리 job은 후속이다(재사용 시 지우고, 남은 행은 PK 조회에 영향이 없다).

### 4. If-Match와 revision

- 응답 `ETag: "<revision>"`이다. `If-Match`는 `"3"`과 `3`을 받는다. 약한 ETag·`*`·여러 값은 400이다.
- **PUT:** If-Match가 없으면 428, 다르면 412 `REVISION_MISMATCH`다. 다른 변경을 덮어쓰지 않는다(D02 §11).
  - 428은 D02 §19 코드 표에 별도 코드가 없어 `REVISION_MISMATCH` 코드에 status 428로 낸다. 새 코드를 만들지 않는다.
- **DELETE:** tombstone(204)이다. revision이 1 늘고 삭제 revision이 이력에 남는다.
  - If-Match는 선택이다(D02 §20은 PUT/PATCH만 정한다). 주면 맞아야 한다.
  - 삭제된 monitor의 조회·수정은 없는 monitor와 같은 404다.

### 5. API와 권한

- **경로**
  - `POST /api/v1/monitors/validate` → `{normalized_spec, warnings, dry_run: null}`
  - `POST /api/v1/monitors` → 201, `Location`, `ETag`
  - `GET /api/v1/monitors?limit&cursor` → 이름순(대소문자 무시, 동률 id), 100개(최대 200), apicursor(ADR 0034)
  - `GET|PUT|DELETE /api/v1/monitors/{id}`
- **권한:** 쓰기·validate는 `monitors.write`, 조회는 `monitors.read`(D04 §01: Developer 이상, Viewer는 없음)다.
  - 권한을 header(Idempotency-Key·If-Match)·본문 검증보다 먼저 본다. 권한 없는 사용자는 늘 403이고 형식 오류를 알지 못한다.
- **404:** 없는 monitor, 다른 tenant, 삭제된 monitor는 같은 404다(존재를 숨김). 형식이 틀린 id는 400이다.
- **`dry_run`은 null이고 warnings에 `dry_run_unavailable`이 있다.** 결과를 "발화 없음"처럼 보이지 않게 한다. 24시간 dry-run은 평가기(단계 B)와 함께 한다(D05 §09).

## 외부 사례 근거 (2026-10-09 확인)

| 사례 | 내용 | 반영 |
|---|---|---|
| Stripe Idempotent requests ([docs](https://docs.stripe.com/api/idempotent_requests)) | 첫 요청의 status·body를 저장해 같은 key 재시도에 그대로 돌려준다. key는 255자까지, 24시간 뒤 정리, 다른 parameter로 재사용하면 오류다. 검증 실패·동시 실행 충돌은 저장하지 않아 재시도할 수 있다 | 결정 3. 다르게: 실패(5xx 포함) 응답은 저장하지 않는다 — 변경이 rollback되므로 재시도가 안전하다 |
| Datadog Monitors API ([docs](https://docs.datadoghq.com/api/latest/monitors/)) | 생성·수정과 별도로 `POST /monitor/validate`(정의 검증) endpoint를 둔다 | 결정 5(validate는 저장 없이 정규형·경고) |
| Grafana alert rule no data·error 처리 ([docs](https://grafana.com/docs/grafana/latest/alerting/fundamentals/alert-rule-evaluation/nodata-and-error-states/)) | No Data의 기본은 별도 No Data 상태, Error의 기본은 Error 상태다. Normal로 바꾸는 선택지도 있다 | 결정 1. 다르게: Normal(OK)로 바꾸는 선택지는 두지 않는다(계약 6, D02 §17) |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 오류율 표현 | `error_ratio` kind(histogram 5xx/전체) | counter 두 개의 비율(D02 예시): 그런 counter를 수집하지 않고, metric 간 연산이 없다. 범용 ratio는 metric 간 연산과 함께 |
| no_data 선택지 | no_data·alert | ok(Grafana Normal): 결측을 정상으로 바꾼다(계약 6) |
| 평가 의미 보존 | revision 이력 테이블 | 현재 spec만: 평가 중 수정되면 어느 정의로 평가했는지 모른다 |
| 삭제 | tombstone | 행 삭제: 평가 이력·감사가 가리키는 정의가 사라진다 |
| Idempotency 저장 위치 | 변경과 같은 PG 트랜잭션 | 별도 cache(Redis 등): 변경과 응답 저장이 어긋날 수 있다(at-most-once 깨짐) |
| 428 코드 | REVISION_MISMATCH + 428 | 새 코드: D02 §19 코드 표 밖이다 |

## 결과

- 정의를 저장·수정·삭제하고 감사·outbox로 다른 구성 요소(단계 B의 평가기)에 알린다. 평가·알림은 아직 없다.
- **schema 변경:** migration 00007(테이블 3개). down은 세 테이블을 지운다.
- **API 변경:** control-api 경로 6개 추가
- **tenant 영향:** RLS·tenant 포함 FK·principal 기반. `tests/isolation`에 교차 공격 시험을 더했다.
- **명세와 다른 점:** 사용자에게 알린다(계약 7). D02 개정은 단계 B 이후 형태가 굳으면 함께 한다.
  - query 모양(`error_ratio`)이 D02 §14 예시와 다르다. 예시 본문을 그대로 보내면 `ratio_denominator`가 모르는 필드라 400이다.
  - D02 §14 "dry-run 검증 후 저장": 지금은 dry-run 없이 저장한다(dry-run은 단계 B).
  - D02 §20 "group 최대 1,000": 저장 시 group 수를 알 수 없다(group_by 5개 키의 조합). 평가 시 처리는 단계 B에서 정한다(재검토 조건).
  - 428의 코드를 재사용했다. DELETE If-Match는 선택이다.

## Rollback

- `MONTRACER_MONITOR_API_ENABLED=false`로 control-api를 다시 시작하면 monitor 경로는 404다(재빌드 없음, flag off 경로 시험은 `TestMonitorAPIDisabled`).
- migration 00007 down으로 테이블을 지운다(평가기가 생기기 전이라 하류 의존이 없다).

## 재검토 조건

- 단계 B(평가기): dry-run 결과 모양, evaluation·alert_instances 테이블, `monitor.*` outbox 소비, minimum_requests 미달 = NO_DATA
- group 수가 1,000(D02 §20)을 넘을 때의 평가 동작(초과 group을 버리지 않고 EVALUATION_ERROR·partial 중 무엇으로 드러낼지)
- 단계 C: notification_policy_id 검증(같은 tenant·접근 가능, D02 §14)
- 다른 metric 간 연산이 생기면: 범용 ratio query(D02 예시 형태)
- idempotency_keys 행 증가가 눈에 띄면: 만료 행 정리 job
- 다른 POST mutation(dashboard, key 발급 등)에 같은 Idempotency-Key 규약 적용

## 증거

- `internal/monitor/spec_test.go`
  - 정규형(기본값·문자열 no_data → 객체·왕복 멱등), metric kind, 경고
  - 거절 30종(한도·분 단위·배수·연산자·비율 범위·kind별 필드·error_ratio 원천·filter 형식·깊이·no_data 필수·OK 없음·for_seconds 필수·모르는 필드·크기)과 400/422 구분
- `internal/controlapi/monitors_test.go`
  - 생성(201·ETag·Location·정규형 저장·경고), Idempotency 재생·409·key 없음/형식 400
  - validate(저장 없음, dry_run null), 400/422/413, Viewer 403(header 없이도 403)·인증 401
  - If-Match 428/400/412, 수정 revision·ETag, 조회 → 수정 왕복, 삭제 412/204 → 404, 형식 오류 id 400
  - 목록 cursor·위조 400·limit, 저장소 미설정 404
- `internal/controldb/monitors_integration_test.go`(PostgreSQL)
  - 수명 주기(412·revision·tombstone, 삭제 뒤 404), revision 이력 3개(threshold 보존)·operations 감사 3·outbox 3(spec 본문 없음)
  - Idempotency(재생·다른 본문 충돌·**동시 10개 중 하나만 생성**·다른 principal 별개·만료 key 재사용)
  - tenant 격리(목록·조회·수정·삭제·같은 key), 앱 role DELETE·revision UPDATE 거부(42501), tenant context 없이 0행, Viewer 거부
- `tests/isolation`: B의 조회·수정·삭제는 없는 monitor와 같은 404(tenant header 무시), 같은 Idempotency-Key도 A 응답 재생 없음, A cursor 재사용 400
- migration 00007 up → down → up
- spec-reviewer: P0·P1 없음. P2 10건 반영(413, for_seconds 필수, minimum_requests 미달 = NO_DATA 명시, error_ratio 원천 고정, checkRole에 monitor_revisions, §12 인용, flag off, 권한 먼저, 동시성 재시도 → 503, 명세와 다른 점 보강)

## 변경 이력

- 2026-10-09 (ADR 0050): error_ratio의 group_by에 `http.response.status_code`를 넣으면 400이다(status별 group의 비율은 0 아니면 1이라 의미가 없다).
