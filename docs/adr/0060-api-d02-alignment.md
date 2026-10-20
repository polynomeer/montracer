# ADR 0060: API 동작을 D02에 맞춘다 — Idempotency-Key·428·413·limit·재생 응답·Content-Type·`/query` metrics

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-19 "권장사항대로 선택해서 결정해줘")
- Owner: Backend lead
- 날짜: 2026-10-19 (제안·결정)
- 관련: D02 §12(공통 계약·limit), §19(오류 status·`/query`), §20(Idempotency-Key·If-Match), 계약 7 · ADR 0037, 0049, 0053, 0055, 0057, 0059

## 배경

ADR 0059가 OpenAPI 원천을 만들며 구현과 D02가 다른 곳 7개를 찾았다. 계약 7에 따라 임의로 고르지 않고 사용자에게 알렸고, 사용자가 권장안대로 결정하라고 위임했다. 원칙은 **D02(권위 문서)에 맞춘다**이고, D02가 정하지 않은 부분은 외부 사례로 정한다.

## 결정

### 1. 시험 전송·secret 교체도 Idempotency-Key 필수 (D02 §20 "모든 POST mutation")

- `POST /notification-policies/{id}/test`·`/rotate-secret`은 Idempotency-Key가 없으면 400이다. 같은 key 재요청은 부작용 없이 저장된 응답을 그대로 돌려주고(`Idempotent-Replayed: true`), 같은 key의 다른 요청은 409다.
- **부작용과 응답 저장은 한 트랜잭션이다**(`controldb.runIdempotent`). 실패한 요청(낡은 If-Match, 한도 초과 등)은 key를 남기지 않아 같은 key로 고쳐 다시 보낼 수 있다.
- **같은 key의 동시 요청은 줄 세운다**(transaction advisory lock, tenant·principal·경로·key의 hash). 줄 세우지 않으면 동시 secret 교체의 뒤 요청이 412를 받고(앞 요청이 revision을 올림), 응답을 잃은 client가 다시 교체해 두 번 바뀔 수 있다(리뷰에서 발견, 통합 시험 `TestIdempotentRotateConcurrent`).
- key는 경로(policy id 포함)에 묶는다. 본문이 없는 시험 전송도 policy마다 따로다. 교체의 request hash는 해석한 If-Match revision과 본문이다(`"2"`와 `2`는 같은 요청).
- **secret 교체 재생 응답에는 새 secret이 없다**(`signing_secret_not_replayed` 경고). policy 생성과 같다 — secret을 재생용으로 저장하지 않는다. 응답을 잃은 client는 다시 교체한다(다른 key, 새 If-Match).
- 화면은 누를 때마다 새 key를 보낸다.
- 이전 동작(ADR 0055): 시험은 진행 중 재사용·rate limit, 교체는 If-Match로 중복을 막았다. 둘은 그대로 남는다. 같은 클릭의 재전송을 key로 합치는 것만 더한다.

### 2. 관리 API 목록 limit 최대 1,000 (D02 §12)

- monitor·policy·dashboard 목록은 기본 100, 최대 1,000이다(이전 200, ADR 0049·0053·0057). group 목록(기본 200, 최대 1,000)은 이미 맞다.
- **monitor 목록은 응답 크기도 묶는다.** 행에 정의(spec, 최대 64KB)가 실려 1,000행이면 최악 64MB다. 정의 합이 4 MiB를 넘기 전에 끊고 `next_cursor`로 잇는다(limit은 상한이고 서버는 더 적게 줄 수 있다 — 첫 행은 항상 싣는다). policy·dashboard 목록은 요약이라(정의 없음) 행이 작다.

### 3. If-Match가 없는 428의 code는 `INVALID_ARGUMENT` (field `If-Match`)

- 빠진 필수 header는 시스템 상태와 무관한 요청 오류다. revision 불일치(412 `REVISION_MISMATCH`)와 code로 구별된다. D02 §12 표에 428은 없고 status는 D02 §20을 따른다.

### 4. secret 교체의 본문 초과는 413 (D02 §19)

- 다른 쓰기 경로와 같다(이전 400).

### 5. 재생 응답 본문은 처음 응답 그대로 (변경 없음, 문서화)

- `meta.request_id`는 처음 요청의 값이고, header `X-Request-ID`는 이 요청의 값이다. 재생은 `Idempotent-Replayed: true`로 알린다. D02는 재생의 request_id를 정하지 않는다.

### 6. `Content-Type` 강제(415)는 사람 session 인증과 함께 정한다 (2026-10-20 실행 — 아래 변경 이력)

- 지금 인증은 bearer key뿐이라 브라우저 form CSRF 경로가 없다. session cookie(OIDC, D04 §02)를 도입할 때 CSRF token과 함께 `application/json` 아닌 쓰기를 415로 막을지 정한다. 원천은 `application/json`만 선언하고, 시험 helper도 그렇게 보낸다.

### 7. `POST /query`가 `signal=metrics`를 실행한다 (D02 §19·§20)

- 본문은 metric 전용 확장(`expression`·`step_seconds`)이고 `/query/metrics`와 같은 query·같은 응답이다(시험이 둘을 비교한다, D02 §20 "generic query와 전용 경로 결과는 같은 fixture에서 일치").
- 검색 field(`filter`·`projection` 등)를 섞으면 400이다. signal 경로(`/query/logs`)에 `metrics`를 주면 경로와 달라 400이다.
- 이전 동작(ADR 0037 §4): 422로 `/query/metrics`를 안내했다.

### 8. 외부 사례 근거 (2026-10-19 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| §1·§5 모든 POST에 key, 재생은 처음 응답 그대로, 다른 요청은 오류, 실패는 저장 안 함 | Stripe Idempotent requests ([docs.stripe.com/api/idempotent_requests](https://docs.stripe.com/api/idempotent_requests)) | v1은 모든 POST가 key를 받는다. 처음 요청의 status·본문을 저장해 재시도에 그대로 돌려준다. 원래 요청과 parameter가 다르면 오류다. 검증 실패·동시 충돌은 저장하지 않는다 | 채택. D02 §20대로 key를 필수로 둔다(Stripe는 선택) |
| §3 빠진 필수 입력은 INVALID_ARGUMENT | Google `google.rpc.Code` ([code.proto](https://github.com/googleapis/googleapis/blob/master/google/rpc/code.proto)) | INVALID_ARGUMENT는 시스템 상태와 무관하게 잘못된 입력이다. 상태 때문에 거절할 때는 FAILED_PRECONDITION·ABORTED다 | 채택. 빠진 If-Match는 INVALID_ARGUMENT, 낡은 revision은 412 REVISION_MISMATCH |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| §1 | D02대로 key 필수 | D02를 고쳐 예외 인정: If-Match·rate limit이 중복을 대부분 막지만, 같은 클릭의 재전송(시험 두 번)은 막지 못한다 |
| §2 | 1,000(+ monitor는 4 MiB) | 200 유지 + D02 개정: 목록은 keyset이고, 정의가 실리는 monitor 목록은 byte 상한으로 묶으면 1,000도 안전하다. 근거 없이 D02와 다른 수치를 둘 이유가 없다. monitor 목록에서 정의를 빼기: 화면이 목록에서 정의를 쓰고 필드 삭제라 호환이 깨진다 |
| §3 | INVALID_ARGUMENT | 새 code `PRECONDITION_REQUIRED`: 오류 code enum이 늘고 client 분기만 는다 |
| §6 | session과 함께 | 지금 415: bearer key client에 이득 없이 호환만 깬다 |
| §7 | 실행 | D02 개정(`/query`는 log·trace만): D02 §19·§20이 명시한 generic query 계약을 줄인다 |

## 호환 영향 (consumer)

base(main)에는 아직 원천이 없어 `apidiff`가 이 변경을 breaking으로 비교하지 않는다(ADR 0059 §3). 실제 동작 기준으로 client가 알아야 할 것:
- 시험 전송·secret 교체에 Idempotency-Key가 없으면 400이 된다(레포 안 consumer는 화면뿐이고 함께 고쳤다).
- If-Match 없는 428의 code가 `REVISION_MISMATCH` → `INVALID_ARGUMENT`. 레포 안 화면은 428 code로 분기하지 않는다.
- `POST /query`의 `signal=metrics`가 422 → 실행. 목록 limit 상한 확대는 호환된다.

## Rollback

- §1: handler가 key 없이도 받게 되돌리면 이전 동작이다. 저장된 key는 24시간 뒤 만료된다.
- §2·§3·§4·§7: 상수·오류 생성·분기만 되돌린다. 원천(`montracer.yaml`)도 함께 되돌린다.

## 증거

- `internal/controldb` 통합: 같은 key 재요청은 새 시험을 쌓지 않고 두 번 바꾸지 않음(revision 그대로), 다른 요청은 충돌, 실패한 요청 뒤 같은 key 재사용 가능, 같은 key 동시 교체 4개 중 하나만 바꾸고 나머지는 재생
- `internal/controlapi`: key 없음 400, 재생(같은 본문·`Idempotent-Replayed`·Location·ETag, 교체는 secret 없이 경고), 다른 요청 409, 교체 본문 초과 413, 428 code·field, limit 1,000 허용·1,001 거절
- `internal/query`: `/query` metrics와 `/query/metrics`의 query·응답 일치, 검색 field 섞으면 400
- `internal/controlapi`: monitor 목록 byte 상한(최소 1행)
- 계약 시험(ADR 0059): status↔code 표의 428을 `INVALID_ARGUMENT`로, 원천의 두 operation에 Idempotency-Key·409·413·재생 header, `/query`의 oneOf 요청·응답
- `tests/isolation`: 다른 tenant의 policy에 시험·교체(key 포함)는 없는 것과 같은 404

## 변경 이력

- 2026-10-20 (§6 실행, browser session 도입 후 — ADR 0064·0065)
  - **규칙:** 본문이 있는 POST·PUT·PATCH는 Content-Type이 `application/json`이어야 한다. charset이 있으면 utf-8만 받는다. 아니면 415다.
    - code는 `INVALID_ARGUMENT`이고 field_violations에 `Content-Type`이 들어간다(413과 같은 code 규칙).
    - 본문 없는 요청(로그아웃 등)과 읽기 요청은 보지 않는다.
    - 두 API 모두 경계 middleware(`httpapi.RequireJSONBody`)에서 route 처리 전에 본다.
  - **이유:** session cookie가 생기면서 HTML form·`text/plain`으로 API를 부르는 browser CSRF 경로가 생겼다. JSON만 받으면 그런 요청은 CORS preflight 없이는 만들 수 없다.
    - CSRF token(ADR 0064 §4)과 함께 두는 심층 방어다(OWASP CSRF Cheat Sheet "content type 제한").
    - GitHub REST API도 지원하지 않는 media type을 415로 거절한다.
  - **호환:** 레포 안 client(화면 `client.ts`, smoke, 격리 suite, probe)는 이미 `application/json`을 보낸다.
    - OpenAPI는 쓰기 operation 16개에 415 응답을 더했다(additive, apidiff 통과).
    - Content-Type 없이 JSON을 보내던 외부 client는 415를 받는다. machine client 문서에 적는다.
- 2026-10-19: spec-reviewer 반영 — 같은 key 동시 요청 advisory lock, monitor 목록 4 MiB 상한, 교체 hash에 해석한 revision, `/query` 첫 JSON 값으로 signal 판별, 호환 영향 절.
