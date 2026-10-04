# ADR 0014: 관리·조회 API 오류 처리 설계

- 상태: 승인
- Owner: Tech lead
- 승인자: polynomeer
- 날짜: 2026-10-04 (결정)
- 관련: F07, E01, E03 · D02 §12, §19, §20 · D04 §02, §10 · CLAUDE.md 변경 불가 계약 3, 6

## 배경

D02 §12·§19는 오류 envelope 형식, 코드별 status, "SQL·stack·다른 tenant 존재를 노출하지 않는다", "안전한 재시도 여부"를 정한다. 그러나 다음은 정하지 않았다.

- 500(예상하지 못한 오류)의 코드
- 오류를 어디서 변환하고 어디서 로그로 남기는지
- panic, client 연결 끊김, timeout 처리
- retryable 판단 규칙
- request ID의 발급 주체

구현이 시작되기 전에 이 규칙을 한 곳에 고정하지 않으면 handler마다 오류 처리 방식이 달라지고, 내부 문자열이 응답에 섞이기 쉽다.

## 결정

### 1. 계층별 책임

| 계층 | 하는 일 | 하지 않는 일 |
|---|---|---|
| 도메인·저장소 패키지 (`internal/*`) | sentinel 또는 타입 오류를 반환하고 `fmt.Errorf("동작: %w", err)`로 맥락을 덧붙인다 | HTTP status 결정, 사용자 메시지 작성, 로그 기록 |
| API 경계 (`internal/httpapi`) | 오류를 `apierr.From`으로 **한 번만** 변환하고, 응답을 쓰고, **한 번만** 로그를 남긴다 | 내부 오류 문자열을 응답에 포함 |
| 오류 envelope (`internal/apierr`) | 코드 목록, status 매핑, envelope 직렬화 | 로그 기록, 요청 해석 |

- "로그 남기고 다시 반환하기"(log-and-return)는 금지한다. 같은 오류가 여러 번 기록되기 때문이다.
- 저장소 계층은 DB driver 오류를 wrap할 때 값이 들어 있는 상세(예: unique 위반의 `Key (email)=(…)`)를 포함하지 않는다. 경계 로그로 PII가 새는 것을 막기 위해서다.

### 2. 오류 코드 목록

D02 §12의 코드에 `INTERNAL`(500, retryable=false)을 추가한다. 예상하지 못한 모든 오류는 내부 문자열을 버리고 `INTERNAL`과 일반 메시지로 응답한다.

| Code | Status | 용도 |
|---|---|---|
| INVALID_ARGUMENT | 400, 의미상 미지원은 422 | 문법·범위 오류. `details.field_violations[]`에 field path를 담는다 |
| UNAUTHENTICATED | 401 | 인증 실패. 원인을 구분해 알려주지 않는다 |
| FORBIDDEN | 403 | 권한 없음, URL tenant 불일치. step-up이 필요하면 `details.step_up_required` |
| NOT_FOUND | 404 | 없음 또는 다른 tenant의 resource (존재를 숨김) |
| EXPIRED | 410 | 보존 기간 만료 |
| CONFLICT | 409 | 상태 충돌, Idempotency-Key 재사용 |
| REVISION_MISMATCH | 412 | If-Match 불일치 |
| RATE_LIMITED | 429 | Retry-After 필수 |
| UNAVAILABLE | 503 | 의존 서비스 실패, timeout |
| QUERY_TIMEOUT | 504 | query 실행 예산 초과로 인한 timeout (query planner가 명시적으로 반환) |
| INTERNAL | 500 | 예상하지 못한 오류, panic |

- 코드는 API 계약의 일부다. 코드를 추가하면 OpenAPI enum과 계약 테스트를 함께 갱신한다. v1 안에서 추가는 허용한다.
- 기존 코드의 의미를 바꾸려면 v2로 간다 (D02 §12).

### 3. 재시도 판단 (retryable)

오류는 `Retry` 정책을 갖는다. 값은 `Auto`(기본), `Yes`, `No` 중 하나다. 경계는 요청을 보고 `Auto`를 아래처럼 확정한다.

| Code | Auto일 때 retryable |
|---|---|
| RATE_LIMITED | true |
| UNAVAILABLE, QUERY_TIMEOUT | 재시도해도 안전한 요청이면 true: GET·HEAD·OPTIONS, 또는 `Idempotency-Key`가 있는 요청 |
| 그 외 | false |

- 부작용이 생기기 전 단계에서 난 실패(예: 인증 정보 저장소 장애)는 생성 지점에서 `Yes`로 표시한다.

### 4. 특수 오류

- **panic**: 경계에서 recover해 `INTERNAL`로 응답하고, stack을 ERROR로 기록한다. 이미 응답을 쓰기 시작했다면 로그만 남긴다.
- **client 연결 끊김** (`context.Canceled`이고 request context가 취소된 경우): 응답을 쓰지 않는다. 오류가 아니므로 DEBUG로만 기록한다.
- **`context.DeadlineExceeded`**: `UNAVAILABLE`로 응답한다. query 예산 초과는 query 계층이 `QUERY_TIMEOUT`을 명시해서 반환한다.
- **"데이터 없음"은 오류가 아니다**: 200과 빈 배열·reason으로 응답한다 (D02 §19).

### 5. Request ID와 로그

- request ID는 서버가 매 요청마다 새로 발급한다(`req_` + 128-bit 난수). 응답 header `X-Request-ID`와 envelope `request_id`에 넣는다. client가 보낸 값은 신뢰하지 않으며 그대로 쓰지 않는다.
- 로그는 경계에서 남긴다.

| 대상 | 수준 | 기록 항목 |
|---|---|---|
| 5xx | ERROR | `request_id`, `code`, `status`, `method`, `route`(path pattern, 원 URL 아님), `error`(wrap chain) |
| 401·403 | INFO | 같은 항목에서 `error` 제외. 보안 이벤트 분석용 |
| 그 외 4xx | DEBUG | 같은 항목 |

- 요청 body, query string, header, 인증 정보는 로그에 넣지 않는다 (D04 §10).

### 6. 범위 밖

- OTLP ingest의 응답은 OTLP 규격을 따른다 (partial success, gRPC status, D02 §04). 이 ADR은 `/api/v1` 관리·조회 API에만 적용한다.
- metric(code별 오류 수)은 metric 라이브러리를 선정할 때 경계에 추가한다.

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A. 경계에서 한 번 변환하고 한 번 로그 (채택) | 응답 형식과 누출 방지가 한 곳에 모임, 중복 로그 없음 | 도메인 오류를 sentinel·타입으로 설계해야 함 |
| B. handler마다 직접 응답 작성 | 유연함 | 형식 불일치, 내부 문자열 누출 위험 |
| C. RFC 9457 Problem Details | 표준 | D02 §12가 이미 envelope 형식을 정해 충돌 |

## 결과

- 이점: 누출 방지(변경 불가 계약 3·6)와 재시도 의미를 코드로 강제한다.
- 비용: 도메인 패키지가 sentinel과 타입 오류를 정의해야 한다.
- 영향 받는 계약:
  - OpenAPI 오류 schema에 `INTERNAL`과 `field_violations`를 추가해야 한다.
  - **D02 §12 표에 `INTERNAL`(500) 행을 추가하는 개정 요청이 필요하다** (원본 docx 개정).

## Rollback

경계 adapter를 쓰지 않고 handler마다 `apierr.Write`를 직접 호출하는 방식으로 되돌릴 수 있다. 오류 코드는 계약이므로 제거하지 않는다.

## 재검토 조건

- gRPC 관리 API를 추가할 때 (gRPC status 매핑 표를 추가한다).
- 오류 metric이나 trace 연동 방식을 정할 때.

## 증거

- `internal/apierr`, `internal/httpapi` 테스트: 누출, panic, 연결 끊김, 재시도 판단, request ID.
