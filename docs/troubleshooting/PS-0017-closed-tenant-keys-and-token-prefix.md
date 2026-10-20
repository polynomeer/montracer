# PS-0017: 정지·폐쇄된 조직의 key가 계속 인증됐고, 초대 token 접두어가 ingest key와 같았다

- 날짜: 2026-10-21 (발견) / 2026-10-21 (해결)
- 영역: controldb·authz (key 인증), control-api (초대)
- 영향: tenant 경계(정지 조직의 수집·조회 지속), 사고 대응 혼선 — 심각도 P1(계약 위반, D04 §02). 배포 전 발견
- 수정: PR polynomeer/montracer#71
- 관련: ADR 0069 §4·§5, ADR 0064(session은 이미 막음), ADR 0067(초대), D04 §02 "tenant 폐쇄 시 신규 token 발급, active session, ingest key, stream, export URL을 모두 무효화한다"

## 증상

1. **정지 조직 key**
   - `tenants.status`를 `suspended`·`closing`·`deleted`로 바꾸면 그 조직의 session은 다음 요청부터 막혔다(ADR 0064).
   - 하지만 ingest·api key는 계속 인증됐다. 정지된 조직의 수집기가 데이터를 계속 보내고, CI가 조회를 계속할 수 있었다.
2. **접두어 충돌**
   - 초대 token(`mti_` + 43자)과 ingest key(`mti_<key_id>_<secret>`)의 접두어가 같았다.
   - 유출 탐지(secret scanning)나 사고 대응 때 문자열만 보고 무엇인지 구분할 수 없었다.

## 발견 경위

- (1) API key 관리 기능(ADR 0069)을 고르며 작업계획서의 남은 항목 "조직 정지 때 API key 막기"를 확인했다. session을 만들 때(ADR 0064) key 경로는 범위 밖으로 남겨 두었다.
- (2) 같은 작업에서 key token 형식(`internal/authz/apikey.go` 주석)을 읽다가 발견했다. 초대(ADR 0067)는 key 형식을 보지 않고 접두어를 골랐다.

## 원인

1. key 조회(`controldb.KeyStore.LookupKey`)가 key 행과 발급자 역할만 읽고 조직 상태를 보지 않았다.
   - session 쪽은 상태 집합(`sessionStatuses`)을 따로 두어 막았다. 같은 계약(D04 §02)의 다른 인증 경로가 같은 규칙을 공유하지 않았다.
2. token 접두어의 등록부가 없었다. 각 기능이 접두어를 따로 정했다.

## 해결

1. **key 조회가 조직 상태를 `KeyRecord.TenantOpen`으로 함께 돌려준다.**
   - `authz.Authenticate`는 hash가 맞은 **뒤에** 이 값을 보고 거절한다.
   - 처음 고친 안은 조회 단계에서 "없는 key"로 바로 돌려주는 것이었다. 리뷰가 지적한 문제: key_id만 알면(비밀이 아님) 조회 단계 수, 곧 응답 시간으로 조직이 정지됐는지를 알 수 있었다.
   - 지금은 상태와 무관하게 같은 조회를 다 하고, 판정은 secret을 가진 사람만 볼 수 있는 단계에서 한다.
   - 기본값(false)이 거절이라, 상태를 채우지 않는 조회 대역은 인증되지 않는다(fail closed).
2. **초대 token 접두어를 `mtinv_`로 바꿨다.**
   - 접두어는 `mti_`(ingest key), `mta_`(API key), `mtinv_`(초대)다.

## 재발 방지

- 시험: `internal/authz/apikey_test.go` `TestAuthenticateClosedTenant`
  - 닫힌 조직의 ingest·api key 거절, 열린 조직은 인증
- 시험: `internal/controldb/keys_api_integration_test.go` `TestKeyAPIStore`
  - suspended·closing·deleted에서 ingest·api 모두 미인증, restricted는 인증
- 시험: `tests/isolation` `TestKeysAcrossTenants`
  - 정지 조직 key로 query-api 401
- 시험: 초대 시험(`members_test.go`·`InvitePage.test.tsx`)이 `mtinv_`를 쓴다
- 규칙: 인증 경로를 새로 만들면 조직 상태 규칙(`sessionStatuses`)을 같은 집합으로 쓰는지 확인한다. 새 token 종류는 기존 접두어와 겹치지 않는지 ADR에 적는다.

## 교훈

- 같은 계약(D04 §02 "폐쇄 시 모두 무효화")을 경로마다 따로 구현하면 빠지는 경로가 생긴다. 계약을 경로 목록(session·ingest key·api key·stream·export)으로 펼쳐 시험한다.
- 거절 판정은 비밀을 확인한 뒤에 한다. 그래야 판정 이유가 시간으로 새지 않는다.
