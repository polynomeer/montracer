# ADR 0066: MFA step-up — IdP 재인증(prompt=login·max_age=0·acr_values)으로 session에 step-up 기록 (F07·F08 일부)

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-20 다음 작업 선택 "멤버 관리 API·화면")
- Owner: Backend/Security lead
- 날짜: 2026-10-20 (제안·결정)
- 관련: F07, F08, E01 · D04 §01(Tenant Admin "step-up 후 권한과 삭제"), §02("관리자 key 발급과 데이터 삭제는 MFA step-up 대상") · ADR 0015 §2(15분 window), 0064(session), 0065(개발 IdP)

## 배경

ADR 0015 §2는 민감 action에 step-up을 요구하는 권한 검사를 만들었다.

- 대상 action은 `members.manage`, `keys.manage`, `deletion.request`다.
- 검사는 `authz.Authorize`가 한다. step-up이 없으면 `ErrStepUpRequired`를 내고, API는 403에 `details.step_up_required: true`를 담는다.
- step-up은 15분 동안 유효하다.

ADR 0064의 session에는 `step_up_at` 열이 이미 있다. 하지만 그 값을 채우는 경로가 없었다. 그래서 session으로는 위 action을 하나도 할 수 없었다. 다음 작업인 멤버 관리(`members.manage`)를 하려면 이 경로가 먼저 필요하다.

명세는 "MFA step-up 대상"이라고만 쓴다. 다음 다섯 가지는 정하지 않았다.

- 재인증을 누가 하는가
- MFA를 어떻게 확인하는가
- 재인증 결과가 지금 session의 사용자임을 어떻게 묶는가
- 응답에서 어떻게 알리는가
- 개발 환경에서 어떻게 시험하는가

## 결정

### 1. 재인증은 IdP가 한다. Montracer는 MFA를 직접 다루지 않는다

- `GET /api/v1/auth/step-up?return_to=`는 지금 session의 사용자를 IdP로 보낸다(302).
- 그 요청에는 다음 매개변수가 붙는다(OIDC Core §3.1.2.1).
  - `prompt=login`과 `max_age=0`을 보낸다. 이미 IdP에 로그인되어 있어도 다시 인증하라는 뜻이다.
  - `MONTRACER_OIDC_STEPUP_ACR`이 설정되어 있으면 `acr_values`도 보낸다.
- 로그인과 같은 봉인 cookie·PKCE·state·nonce를 쓴다.
- 로그인 state에는 **시작한 session의 token hash**(hex, 봉인 안에만 있음)를 함께 넣는다.
- 비밀번호·TOTP·WebAuthn은 Montracer가 다루지 않는다. 고객 IdP(Okta·Entra ID·Google)의 MFA 정책이 그대로 적용된다.

### 2. callback은 다섯 가지를 모두 확인할 때만 기록한다

같은 callback 경로(`/api/v1/auth/callback`)에서 state에 step-up 표시가 있으면 로그인 대신 다음을 확인한다.

1. 요청의 session cookie가 시작한 session과 **같다**.
   - 다른 탭·다른 사용자로 바뀐 session으로 돌아오면 거절한다.
2. ID token의 사용자(`oidc:<issuer hash>:<sub>`)가 session 사용자와 **같다**.
   - 재인증 화면에서 다른 계정으로 로그인하면 거절한다.
3. `auth_time`이 **있다**. 그리고 지금으로부터 5분 안이며, 1분보다 더 미래가 아니다.
   - `max_age`를 무시하는 IdP가 있어 RP가 직접 확인한다(OIDC Core §3.1.2.1 "SHOULD check the auth_time").
   - 5분은 재인증 화면에서 MFA를 입력하는 시간에 clock 차이를 더한 값이다.
4. ACR이 설정되어 있으면 ID token의 `acr`이 그 값과 **같다**.
5. session이 폐기되지 않았고, 같은 tenant 범위다.
   - `MarkStepUp`이 RLS(`WithTenant`)와 `user_id`·`revoked_at IS NULL` 조건으로 갱신한다.

모두 맞으면 다음이 일어난다.

- `user_sessions.step_up_at = now`를 기록한다.
- 감사 `session.step_up {acr}`를 남긴다.
- 303으로 `return_to`에 보낸다.

하나라도 틀리면 기록하지 않는다. 기존 session은 그대로 두고 `/login?error=step_up_rejected&return_to=…`로 보낸다.

- 지표는 `montracer_auth_logins_total{outcome="step_up_ok"|"step_up_rejected"}`다.
- app role은 `step_up_at` 열만 UPDATE할 수 있다(migration 00015, 열 단위 GRANT).

### 3. 유효 시간과 표시

- 유효 시간은 ADR 0015 §2를 그대로 따른다(`step_up_at`부터 15분).
- session 응답(`GET /auth/session`, 조직 전환 응답)에 `step_up_expires_at`을 넣는다.
  - step-up이 없거나 지났으면 `null`이다.
  - 화면은 민감 작업 전에 이 값을 본다. 또는 403 `step_up_required`를 받으면 재인증으로 보낸다.
  - 이 필드는 optional로 추가했다(apidiff 호환).
- 조직을 전환하면 새 session을 만든다(ADR 0064). 따라서 step-up은 **이어지지 않는다**. 조직이 바뀌면 다시 재인증한다.

### 4. ACR 설정

- `MONTRACER_OIDC_STEPUP_ACR`이 비어 있으면 IdP에서 "다시 로그인"만 확인한다. 이때 MFA는 IdP의 기본 정책에 맡긴다.
- 운영에서는 IdP의 MFA ACR 값을 넣는다. 예는 IdP마다 다르다.
  - Okta는 `urn:okta:loa:2fa:any`다.
  - Auth0는 `http://schemas.openid.net/pape/policies/2007/06/multi-factor`다.
- 값은 IdP 문서에서 확인한다. 이 ADR은 특정 IdP 값을 고정하지 않는다.

### 5. 개발 IdP

- 개발 IdP(ADR 0065)는 받은 `acr_values`의 첫 값을 `acr` claim으로 돌려준다. `auth_time`은 지금이다.
- 사용자 고르기를 "그 수준의 인증"으로 본다(MFA 흉내).
- smoke 5c는 두 경우를 확인한다.
  - 다른 사용자로 재인증하면 거절된다.
  - 같은 사용자로 재인증하면 `step_up_expires_at`이 생긴다.

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A (채택) IdP 재인증(`prompt=login`·`max_age=0`·`acr_values`) + RP가 sub·auth_time·acr·session 확인 | MFA를 고객 IdP 정책에 맡긴다. 표준 매개변수만 쓴다. 비밀을 새로 저장하지 않는다 | ACR 값이 IdP마다 다르다(설정). `max_age`를 무시하는 IdP가 있다(auth_time을 직접 확인해 막음) |
| B Montracer 자체 TOTP/WebAuthn | IdP와 무관하게 MFA를 보장한다 | 비밀(TOTP seed)·credential 저장, 분실 복구, 지원 부담. 고객 SSO 정책과 이중이다 |
| C step-up 없이 로그인 시점 MFA만 | 단순하다 | D04 §02 위반. 탈취된 session으로 12시간 동안 key 발급·삭제가 가능하다 |
| D 비밀번호 재입력 화면(Montracer) | 단순하다 | 사람 비밀번호를 받지 않는 구조(OIDC)와 맞지 않는다 |
| E 재인증 결과를 새 session으로 발급(로그인과 같게) | 경로가 하나다 | 다른 사용자로 재인증하면 session이 바뀐다. 진행 중인 화면 상태와 CSRF token이 깨진다 |

## 결과

- 이점: session으로 민감 action(멤버·key·삭제)을 할 수 있다. 그 전에 IdP에서 최근에 다시 인증해야 한다.
- 비용 / 운영 부담:
  - 운영자는 IdP의 MFA ACR 값을 설정해야 한다.
  - 비어 있으면 "다시 로그인"만 보장한다(runbook RB05에 적음).
- 영향 받는 계약:
  - OpenAPI: `GET /api/v1/auth/step-up` 추가. `SessionResponse.data.step_up_expires_at` 추가(optional, nullable).
  - schema: migration 00015(`GRANT UPDATE (step_up_at)`). 테이블 변경은 없다.
  - 지표: `montracer_auth_logins_total`의 outcome에 `step_up_ok`·`step_up_rejected`를 추가했다.
  - 감사: `session.step_up`.
  - 시험이 확인하는 것은 다음과 같다.
    - 거절 다섯 가지(다른 사용자·오래된/없는/미래 auth_time·ACR 불일치·다른 session·폐기된 session)
    - 다른 tenant 범위 기록 거절
    - 15분 경계
    - 평소 로그인은 재인증을 요구하지 않음

## 기능 상태

- F07: 멤버 관리 API·화면은 남아 있다(다음 PR). step-up을 요구하는 action이 쓰는 것은 그 PR부터다.
- F08: step-up의 기반(재인증·기록·표시)만 이 ADR에서 했다. 팀·환경 scope, SAML·SCIM, 조회 감사는 남아 있다.

## Rollback

- control-api를 이전 이미지로 되돌리면 `/auth/step-up`이 사라진다.
  - session 응답의 `step_up_expires_at`도 빠진다(optional이라 화면이 깨지지 않는다).
  - 민감 action은 다시 session으로 할 수 없게 된다. API key 경로는 그대로다.
- migration 00015 down은 GRANT를 회수한다. 이미 기록된 `step_up_at` 값은 15분 뒤 효력이 없어진다.

## 재검토 조건

- 고객 IdP가 `prompt=login`이나 `max_age`를 지원하지 않아 재인증이 성립하지 않는 경우.
- step-up 유효 시간(15분, ADR 0015)이 업무 흐름(대량 멤버 정리)에 비해 짧다는 근거가 생긴 경우.
- ACR 대신 `amr` claim(예: `mfa`, `hwk`)으로 판단해야 하는 IdP가 주 고객이 되는 경우.
- RFC 9470(Step-Up Authentication Challenge)을 API key·machine 경로에도 써야 하는 경우.

## 근거 (빅테크 사례·표준)

- **GitHub sudo mode**
  - 저장소 삭제·SSH key 추가 같은 민감 작업 전에 다시 인증을 요구한다.
  - 그 뒤 일정 시간은 다시 묻지 않는다.
  - 근거: docs.github.com "Sudo mode".
  - 채택한 것: "로그인 상태와 별도로 최근 재인증 시각을 두고, 민감 작업은 그 시각으로 판단"하는 구조.
- **AWS IAM `aws:MultiFactorAuthAge`**
  - 정책이 "MFA가 N초 안에 있었는가"를 조건으로 건다.
  - 근거: AWS IAM 문서 "AWS global condition context keys".
  - 채택한 것: 민감 action을 역할과 별도의 MFA 시각 조건으로 막는 방식(ADR 0015 §2의 window).
- **Google 재인증**
  - Google Account와 Google Cloud console은 보안 설정 변경 전에 비밀번호·2단계 인증을 다시 묻는다.
  - 근거: Google Account Help "Verify it's you", Google Cloud "Reauthentication".
  - 채택한 것: 재인증을 IdP 쪽(여기서는 고객 IdP)에 맡기는 방식.
- **OIDC Core 1.0**
  - §3.1.2.1은 `prompt=login`·`max_age`·`acr_values`를 정의한다.
  - §2는 `auth_time`·`acr` claim을 정의한다.
  - `max_age`를 보냈다면 RP가 `auth_time`을 확인해야 한다.
- **RFC 9470 (OAuth 2.0 Step-Up Authentication Challenge Protocol)**
  - resource server가 `insufficient_user_authentication`으로 `acr_values`·`max_age`를 요구한다.
  - client는 그 값으로 재인증한다.
  - 이 ADR은 같은 구조를 따르되, 브라우저 session이므로 403 `details.step_up_required`로 알린다.
- **Okta·Auth0 step-up 문서**
  - `acr_values`로 MFA 수준을 요청하고, ID token `acr`로 확인한다.
  - ACR 값을 IdP별 설정으로 둔 이유다.

## 증거

- `internal/controlapi/auth_test.go` `TestStepUpFlow`
  - 재인증 매개변수
  - 거절: 다른 사용자, 오래된/없는 auth_time, ACR, 다른 session
  - 성공 뒤 `step_up_expires_at`
  - 15분 만료
- `internal/session/session_test.go` `TestCompleteStepUp`
  - 거절 여섯 가지
  - step-up 전 `members.manage`는 `ErrStepUpRequired`, 후는 허용, 15분 뒤 다시 필요
- `internal/session/oidc_test.go` `TestOIDCReauthentication`
  - 실제 서명 ID token의 `auth_time`·`acr` 해석
  - 평소 로그인 URL에는 재인증 매개변수가 없음
- `internal/controldb/sessions_integration_test.go`
  - 다른 tenant 범위·다른 사용자·폐기된 session 거절
  - 감사 순서
- `scripts/dev/devidp/main_test.go`: 개발 IdP의 `acr`·`auth_time`.
- smoke 5c: 개발 IdP로 재인증(다른 사용자 거절, 같은 사용자 성공).
