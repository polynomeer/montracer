# ADR 0059: OpenAPI 3.1 원천과 계약 시험 — route·요청·응답·UI fixture·breaking change

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-19 다음 단계 선택 "OpenAPI 원천·계약 시험")
- Owner: Backend lead
- 날짜: 2026-10-19 (제안·결정)
- 관련: D02 §12(공통 계약, "OpenAPI 3.1 원천, breaking-change 검사와 consumer contract test를 CI에서"), §19, §20 · D06 §03(Contract 시험), §11(`make test-contract`: API·proto·UI fixture 일치) · `api/README.md` · M0 Gate Q0

## 배경

- D02 §12·§20은 API 기계 명세를 OpenAPI 3.1 원천으로 두고, breaking-change 검사와 consumer contract test를 CI에서 돌리라고 한다. 지금까지 `api/openapi/`는 비어 있었다.
- 구현된 `/api/v1` operation은 31개다(query-api 9, control-api 22). 화면(`apps/web`)은 그 응답 모양을 손으로 옮긴 type과 시험 fixture로 쓴다.
- 원천이 없어서 생긴 문제(이 작업에서 확인)
  - **UI fixture가 실제 응답과 달랐다.** 조회 `meta`의 null 필드(watermark·sampled·coverage·…)가 빠졌고, metric 응답에 `source_window_seconds`·`range`가 없었다. 서비스 fixture는 UUID가 아닌 service_id와 빈 시각을 썼다. 화면 시험은 통과했지만 실제 서버 응답과 다른 입력이었다.
  - **control-api `meta` 모양이 응답마다 달랐다.** 쓰기·일부 조회는 `warnings`가 있고, 목록·감사·timeline은 없었다.
  - **handler 시험 fixture가 실제 데이터와 달랐다.** log 행의 빈 service_id, `Content-Type` 없는 요청.
- OTLP 수집(`/v1/*`, ingress)은 OTLP proto가 원천이고 pdata로 고정한다(`api/README.md`, ADR 0001). 이 ADR의 범위가 아니다.

## 결정

### 1. 원천: 손으로 쓰는 OpenAPI 3.1 한 파일

- `api/openapi/montracer.yaml`(OpenAPI 3.1.0)이 `/api/v1` 관리·조회 API의 기계 명세 원천이다. Go는 `api/openapi`(embed)로 읽는다.
- operation마다 `x-montracer-service`(`query-api`|`control-api`)로 담당 service를 적는다. 한 파일에 두는 이유: gateway 뒤에서 client에게는 하나의 API이고, 공통 component(오류 envelope·meta·parameter)를 나눠 쓴다.
- **응답 schema는 실제 응답 그대로**다. 모르는 값은 `type: [X, 'null']`로 필수에 넣는다 — 빠짐과 null을 구분한다(D02 §12, 계약 6). 임의 key의 map(OTel 속성·label·오류 details)은 `patternProperties`로 쓴다.
- **요청 schema는 client가 지켜야 할 계약**(D02)이다. 서버가 더 관대하게 받는 값(예: `limit: 0`을 기본값으로, 선택 field의 `null`)은 적지 않는다 — 보장하지 않는 동작이다. provider 검사는 시험이 실제로 보낸 요청만 보므로, 시험 helper도 계약대로 보낸다(`Content-Type: application/json` 등).
- **검색 행(LogRow·TraceRow·SearchRow)에는 필수 field가 없다.** `projection`이 field 부분집합을 고르기 때문이다. 대가로 기본 projection에서 field가 빠져도 schema는 잡지 못한다(handler 시험의 기본 projection 단언이 맡는다). `SearchRow`(`POST /query`)는 log·trace field의 합집합이라 signal별 구분을 강제하지 않는다 — signal 전용 경로(`/query/logs`·`/query/traces`)가 정확한 schema다.
- **응답 enum과 확장:** client가 모르는 값을 원문으로 보여야 하는 필드는 enum 대신 pattern과 설명으로 둔다(예: 알림 `event_type` — 롤백한 control-api가 `alert.renotified.<n>`을 준다).
- **코드 생성은 하지 않는다(이번 단계).** D02 §12는 "원천으로 코드 생성"이라 하지만, 구현된 handler·화면 type을 생성 코드로 바꾸는 것은 이 결정의 범위가 아니다. 대신 §2·§4가 코드와 원천이 어긋나면 CI가 실패하게 한다. 생성(TS type·Go server stub)은 화면 type 정리와 함께 후속이다(§6).
- 서버 쪽 정리(원천에 맞춰 한 것)
  - control-api의 모든 응답 `meta`에 `warnings`(없으면 `[]`)를 넣었다 — additive. 한 schema(`ControlMeta`)로 쓴다.

### 2. provider 시험: handler 시험이 주고받은 교환을 원천으로 검사

- `tests/contract`가 libopenapi-validator로 교환을 검사한다.
  - **응답은 항상** 검사한다. 상태 코드가 그 operation에 선언돼 있어야 하고(`default` 응답 없음), 본문이 schema에 맞아야 한다.
  - **strict mode:** 선언 안 된 property도 위반이다(`additionalProperties`가 열려 있어도). 원천에 없는 필드를 서버가 몰래 내보내지 못한다.
  - **요청은 서버가 받아들인 경우(2xx·3xx)만** 검사한다. 거절된 요청은 negative 시험이 일부러 만든 잘못된 입력이다. 서버가 받아들인 요청이 schema에 어긋나면 schema가 너무 좁거나 서버가 너무 느슨하다.
  - **status와 error.code를 묶는다**(schema만으로는 못 묶는다): 404는 `NOT_FOUND`만, 422는 `INVALID_ARGUMENT`·`QUERY_BUDGET_EXCEEDED`만 등(`tests/contract.ErrorCodes`, 원천 `Error.description`). 화면 쪽 검사도 같은 표를 쓴다.
  - **응답 header**도 그 응답에 선언된 것만 허용한다. validator strict mode가 응답 header를 보지 않아 따로 본다(`Set-Cookie` 같은 누출을 잡는다). HTTP 기본(`Content-Type` 등)과 공통 header `X-Request-ID`·`X-Content-Type-Options`는 operation마다 적지 않는다(info.description).
  - 격리 시험이 일부러 보내는 위조 `X-Tenant-ID` 요청 header는 strict 검사에서 뺀다. 서버는 무시한다(계약 1).
- **연결:** `internal/query`·`internal/controlapi`의 요청 helper가 `contract.Serve`를 거친다. `TestMain`의 `contract.Run`이 시험 끝에 위반을 보고한다.
  - **coverage:** 그 service의 operation마다 검사를 통과한 성공 응답이 한 번 이상 있어야 한다(`-run`·`-skip`·`-short`로 일부만 돌리면 보지 않는다). 처음 연결할 때 `GET /notification-policies/{id}`에 성공 시험이 없었다 — 더했다.
  - **route parity:** handler가 등록한 route(`Handler.Routes()`)와 원천의 operation이 양쪽으로 같아야 한다. 원천 없이 route를 더하거나, 원천에만 있는 operation이 있으면 실패한다.
  - **격리 suite**(`tests/isolation`, 실제 PostgreSQL·ClickHouse)도 `contract.Wrap`으로 같은 검사를 한다. 공격 시험의 거절 응답(404·403·400)도 원천의 오류 envelope이어야 한다. 이 suite는 coverage를 보지 않는다.
- **헛돌지 않는다:** `tests/contract` 자체 시험이 필수 field 누락·선언 안 된 field·잘못된 enum·null 자리의 문자열·선언 안 된 status·원천 밖 경로의 200을 잡는지 본다.

### 3. breaking change: base revision과 비교해 막는다

- `scripts/dev/apidiff`가 base(`origin/main`, CI는 PR base)의 원천과 비교한다. 판정은 libopenapi what-changed 규칙이다.
  - 깨는 변경: path·operation·응답 field 삭제, 타입 변경, 요청 필수 field 추가 등
  - 깨지 않는 변경: 응답 field 추가, 선택 요청 field 추가, 설명 변경 (자체 시험이 둘 다 확인)
  - **선택 parameter가 필수가 되는 변경**(공유 component parameter 포함)은 what-changed 규칙이 breaking으로 보지 않아 따로 판정한다(리뷰에서 발견).
- CI의 `go` job 단계로 돈다(필수 check 이름은 그대로). base에 원천이 없으면(이 PR) 비교하지 않는다 — 판정은 git 종료 코드로 한다.
  - PR에서는 base 가지 끝과 비교한다. main push에서는 같은 원천끼리라 비교가 의미 없다(검사는 PR에서 끝난다).
- **의도한 breaking change**는 v2 경로로 나누는 것이 원칙이다(D02 §12). 출시 전(M0)·합의된 예외는 PR label `api-breaking`(로컬은 `MONTRACER_API_BREAKING_OK=1`)으로 통과시키되 목록은 출력한다. PR 본문에 이유와 consumer 영향을 적는다. label을 붙이거나 떼면 CI가 다시 돈다(`pull_request` types에 `labeled`·`unlabeled`).

### 4. consumer 시험: UI fixture를 원천으로 검사

- `apps/web`의 시험은 `vi.stubGlobal('fetch', …)`로 가짜 서버를 넣는다. `src/test/setup.ts`가 그 fetch를 감싸 모든 교환을 원천으로 검사한다(`src/test/contract.ts`, ajv 2020-12). 위반은 그 시험을 실패시킨다.
  - 응답: 선언된 status, schema 일치, **선언 안 된 field 거절**(닫힌 object schema에 `unevaluatedProperties: false`를 넣는다 — Go strict와 같은 기준). 화면이 서버에 없는 field에 기대지 못한다.
  - 요청: 가짜 서버가 받아들인 요청의 JSON 본문, 필수 header(Idempotency-Key·If-Match)·query parameter
  - status와 error.code를 묶는다(Go와 같은 표). JSON이 아닌 응답에 JSON schema가 선언돼 있으면 위반이다.
  - 원천 밖 경로는 보지 않는다. 모르는 값에 견디는지 보는 fixture가 필요하면 원천이 그 값을 허용해야 한다(예: 알림 `event_type`은 pattern).
  - **보지 못하는 것:** 시험이 `mockImplementation`으로 바꾼 구현의 교환, query·path parameter의 값(존재만 본다). 검사기 자체 시험(`src/test/contract.test.ts`)이 operation 찾기와 위반 검출을 확인한다.
- 공용 fixture(`src/test/fixtures.ts`: `queryMeta`·`controlMeta`·`metricData`·`serviceFixture`)로 실제 응답 모양을 한 곳에 둔다. 이번에 고친 fixture는 배경에 적었다.
- `make test-contract` = Go 계약 시험 + 화면 시험 + `apidiff`

### 5. 외부 사례 근거 (2026-10-19 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| API 설명 문서를 원천으로 두고 계약 시험에 쓴다 | GitHub REST API description ([github/rest-api-description](https://github.com/github/rest-api-description)) | OpenAPI 3.0과 3.1(`descriptions-next`) 설명을 공개하고, 그 문서가 GitHub API 요청 검증과 contract test에 쓰이는 설명과 같게 유지된다고 밝힌다 | 채택: 손으로 쓴 3.1 원천을 요청·응답 검증과 계약 시험에 그대로 쓴다 |
| REST 명세를 한 원천 저장소에 둔다 | Azure REST API specs ([Azure/azure-rest-api-specs](https://github.com/Azure/azure-rest-api-specs)) | Azure REST API 명세(OpenAPI·TypeSpec)의 공식(canonical) 원천 저장소 | 채택: 명세를 코드 옆 한 파일로 두고 PR에서 함께 바꾼다 |
| 검증 도구 | pb33f libopenapi-validator ([pkg.go.dev](https://pkg.go.dev/github.com/pb33f/libopenapi-validator)) | OpenAPI 3.1 문서 검증, `http.Request`·`http.Response` 검증, 선언 안 된 property를 잡는 strict mode | 채택(§2) |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 원천 방식 | 손으로 쓴 OpenAPI 한 파일 + 시험으로 코드와 묶기 | Go 코드에서 생성(swaggo 등): 문서가 구현을 따라가 "원천"이 아니고 D02 §12와 반대다. TypeSpec: 도구 사슬이 하나 더 는다 |
| 일치 확인 | 기존 handler 시험의 교환을 검사 | 전용 contract 시험을 따로 작성: 같은 경로를 두 번 쓰고 빠지기 쉽다. 생성 server stub: 지금 handler를 다시 써야 한다 |
| 검증 라이브러리 | libopenapi(-validator) | kin-openapi + oasdiff: 널리 쓰이지만 선언 안 된 property를 잡는 strict 검사를 따로 만들어야 하고, 검증과 diff가 다른 문서 모델을 쓴다. libopenapi는 3.1 검증·strict·what-changed diff를 한 모델로 준다 |
| 응답 field 엄격도 | strict(선언 안 된 field 거절) | 느슨하게: 문서에 없는 field가 생기고 화면이 거기에 기댄다(이번에 실제로 발견) |
| consumer 검사 위치 | 화면 시험의 fetch 대역 | Pact 같은 broker 기반 CDC: 같은 레포·같은 팀이라 broker 운영 비용만 는다 |

## 원천이 드러낸 현재 동작과 D02의 차이

원천은 **지금 구현된 동작**을 적었다. 아래는 D02와 달랐고, 계약 7에 따라 사용자에게 알린 뒤 **ADR 0060이 결정했다**(1~4·7은 D02에 맞춤, 5는 문서화, 6은 session 인증과 함께).

1. `POST /notification-policies/{id}/test`·`/rotate-secret`은 Idempotency-Key를 요구하지 않는다(D02 §20 "모든 POST mutation"). test는 진행 중인 test 재사용과 rate limit, rotate는 If-Match로 중복을 막는다.
2. monitor·policy·dashboard 목록 `limit`은 최대 200이다(D02 §12 "최대 1,000"). 상한 수치는 ADR 0049·0053·0057의 결정이다.
3. 428(If-Match 없음)의 오류 code가 `REVISION_MISMATCH`라 412와 code로 구별되지 않는다. 413의 code는 `INVALID_ARGUMENT`다.
4. `rotate-secret`만 본문 초과가 413이 아니라 400이다.
5. 같은 Idempotency-Key 재생 응답의 `meta.request_id`는 원래 요청의 값이다(응답 header `X-Request-ID`와 다르다).
6. 서버는 `Content-Type` 없는(또는 json이 아닌) POST·PUT 본문도 받는다. 원천은 `application/json`만 선언한다. session cookie가 들어오면 CSRF 방어(D02 §12)와 함께 415로 막을지 정한다.
7. `POST /query`의 `signal=metrics`는 422로 `/query/metrics`를 안내한다(ADR 0037 §4의 결정). D02 §19는 `/query`가 metrics도 받는다고 적는다.

## Rollback

- 시험만 바뀐 부분: `contract.Serve` 호출을 `ServeHTTP`로 되돌리고 `TestMain`을 지우면 이전 동작이다. CI 단계 `OpenAPI breaking change`는 지워도 다른 check에 영향이 없다.
- 서버 변경은 control-api `meta.warnings` 추가뿐이고 additive다(되돌리면 원천의 `ControlMeta`도 같이 되돌린다).

## 증거

- `tests/contract`: 원천이 올바른 3.1(operationId 고유·service 표시), 어긋남 6종·status와 code 불일치·선언 안 된 응답 header를 잡음, route 차이를 양쪽으로 보고, breaking 5종(필수가 된 parameter 2종 포함)·호환 2종 판정
- `internal/query`(9 operation)·`internal/controlapi`(22 operation): 모든 교환이 strict 검사를 통과하고 operation마다 성공 응답이 검사됨, route parity
- `tests/isolation`(CI 통합 job): 실제 저장소를 거친 응답·공격 시험 거절 응답도 원천과 일치(처음 CI에서 helper의 Content-Type 누락과 위조 `X-Tenant-ID`로 24건 위반 → 고침)
- `apps/web`: 모든 시험의 fetch 교환이 원천과 일치(처음 연결 때 83개 시험이 fixture 불일치로 실패 → fixture를 실제 응답 모양으로 고침, PS-0014). 검사기 자체 시험 4개
- `scripts/dev/apidiff`: base에 원천이 없으면 건너뜀, 있으면 breaking 목록과 실패

## 변경 이력

- 2026-10-19: 최초 작성.
- 2026-10-19: spec-reviewer 반영 — 필수가 된 parameter 판정, status↔code와 응답 header 검사, coverage 예외(-skip·-short), 요청 schema 원칙, 검색 행 한계, D02 차이 6·7, apidiff 종료 코드 판정·label 재실행, 화면 검사기 보강과 자체 시험, PS-0014.
- 2026-10-19: '현재 동작과 D02의 차이' 7건을 ADR 0060이 결정했다. 계약 시험의 428 code를 `INVALID_ARGUMENT`로 바꿨다.
