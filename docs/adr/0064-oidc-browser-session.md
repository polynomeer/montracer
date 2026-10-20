# ADR 0064: 사람 로그인 — OIDC Authorization Code + PKCE와 서버 저장 browser session (F07 단계 A)

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-20 다음 작업 선택 "OIDC 세션·RBAC")
- Owner: Backend/Security lead
- 날짜: 2026-10-20 (제안·결정)
- 관련: F07, E01 · D04 §01(권한 모델), §02(인증 흐름), §12(JIT 금지) · D02 §12(cookie·CSRF·tenant 도출) · 계약 1 · ADR 0015, 0016, 0033, 0059, 0060 §6

## 배경

지금까지 관리·조회 API는 bearer API key만 받았다. 화면은 개발용 proxy가 seed key를 붙여서 동작했다(ADR 0042). M0 Gate Q1(보안)에는 사람 로그인이 필요하다. 명세가 정한 것은 다음과 같다.

- **D04 §02**
  - 사람은 OIDC Authorization Code + PKCE를 쓴다. state·nonce를 검증하고, redirect URI는 고정 allowlist로 둔다.
  - cookie는 Secure·HttpOnly·SameSite=Lax이고, 변경 요청에는 CSRF token이 필요하다.
  - idle 30분, 최대 12시간 뒤 재인증한다.
  - 폐기는 60초 안에 전파되고, 제어 평면이 끊기면 fail closed한다.
- **D02 §12:** 브라우저는 HttpOnly Secure session cookie와 CSRF token을 쓴다. tenant는 인증된 membership 선택에서 도출한다.
- **D04 §01:** 역할은 membership role이다(Viewer…Security Auditor). 역할 → action 표는 이미 `authz.Role.Grants`에 있다(ADR 0015).
- **D04 §12:** JIT 가입만으로 관리자 role을 부여하지 않는다.

명세가 정하지 않은 것:
- session 저장 방식(서버 저장 / 자기완결 token)
- 로그인 중 state를 둘 곳
- CSRF token 방식
- 사용자 식별자
- 여러 조직 사이 전환
- 설정 off 경로
- 로그인 경로를 둘 서비스

## 결정

### 1. session은 서버 저장(`user_sessions`)이고, cookie에는 무작위 token만

- token은 무작위 32 byte(base64url)다. cookie `__Host-mt_session`(Secure·HttpOnly·SameSite=Lax·Path=/, 만료 = 최대 12시간)에만 있다. 저장소에는 SHA-256만 둔다.
  - DB가 유출돼도 session을 재생할 수 없다.
  - `__Host-` 접두어는 브라우저가 Secure·Path=/·Domain 없음을 강제한다. 형제 하위 도메인이 cookie를 덮어쓰지 못한다.
- **서버 저장을 고른 이유:**
  - 로그아웃·멤버 제거·조직 정지가 다음 요청부터 막힌다(D04 §02 "60초 안 전파"를 cache 없이 만족한다).
  - 조직 상태가 `suspended`·`closing`·`deleted`면 그 조직의 session은 미인증이고 로그인 목록에서도 빠진다. `trial`·`active`·`past_due`·`restricted`는 받는다(D04 §12: 기존 데이터 조회는 보호).
  - JWT 같은 자기완결 token은 만료 전까지 폐기할 수 없다.
- **만료:** idle 30분은 활동마다 연장하되, 기록은 1분에 한 번만 한다(요청마다 쓰지 않게). 최대 12시간은 연장하지 않는다. 만료·폐기는 조회 때 판정한다.
- **역할은 session에 저장하지 않는다.** 요청마다 그 tenant의 membership을 다시 읽는다.
  - 역할 변경은 다음 요청부터 반영된다.
  - 멤버에서 제거되면 미인증이 된다(API key가 발급자 역할을 매번 읽는 ADR 0015 §1과 같다).
- **RLS:** `user_sessions`는 tenant RLS를 쓴다. tenant를 모르는 인증 조회는 `app.session_lookup`(정확한 hash 한 행, SELECT 전용) 정책으로만 한다(api_keys의 key_lookup과 같은 방식, ADR 0016 §4).
  - 조회 트랜잭션은 찾은 행의 tenant를 같은 트랜잭션에서 `app.tenant_id`로 두고 membership·조직 상태를 읽는다.
  - 이것은 `WithTenant`를 쓰지 않는 예외다. tenant가 요청 입력이 아니라 저장된 행에서 오기 때문이다(LookupKey와 같다).
- **정리:** 만료된 행은 그 조직의 누가 로그인하든 같은 트랜잭션에서 지운다(표시 이름을 오래 두지 않게). 로그인이 없는 조직의 행은 정리 job(남은 것)이 지운다.
- **최소 권한:** 앱 role은 `last_seen_at`·`idle_expires_at`·`revoked_at`만 바꾼다. tenant·사용자·최대 만료·hash는 불변이다.
- **감사:** 만들기·폐기를 security 감사에 남긴다(`session.created`·`session.revoked`, 사유 `logout`·`tenant_switch`). token·hash는 남기지 않는다.

### 2. 사용자 식별자는 `oidc:<hex(SHA-256(issuer))[:12]>:<sub>`

- `sub`는 issuer 안에서만 고유하다(OIDC Core §2). issuer를 넣어 IdP 둘이 같은 sub를 주어도 섞이지 않게 한다.
- email은 쓰지 않는다. 바뀌고 재사용되기 때문이다. ID token의 email claim도 받지 않는다(PII 최소화).
- 이 값은 기존 `memberships.user_id`·API key `issued_by`·감사 actor_id와 같은 자리에 쓴다. schema를 바꾸지 않는다. sub는 230자까지 받는다(user_id 256자 상한).
- 표시 이름은 ID token의 `name` claim을 session 행에만 둔다(로그·감사 금지).

### 3. 로그인은 control-api가 한다. 로그인 state는 봉인 cookie에

- **경로**(control-api, 제어 DB 쓰기 소유):
  - `GET /api/v1/auth/login?return_to=` → IdP로 302
  - `GET /api/v1/auth/callback` → session cookie를 쓰고 303
  - `GET /api/v1/auth/session` → 사용자·조직·역할·membership 목록·CSRF token
  - `POST /api/v1/auth/logout`
  - `POST /api/v1/auth/session/tenant` → 조직 전환
- **state·nonce·PKCE verifier:** 무작위 값을 서버 key(AES-256-GCM, AAD=cookie 이름)로 봉인한 `__Host-mt_login` cookie(10분)에 둔다.
  - callback에서 state를 상수 시간으로 비교하고, cookie는 한 번 쓰면 지운다.
  - 서버에 로그인 중 상태를 저장하지 않는다(로그인 시도 수만큼 행이 생기지 않는다).
- **PKCE·nonce:** PKCE는 S256이다. ID token은 go-oidc가 서명·issuer·audience·만료를 검증하고, nonce는 봉인 cookie의 값과 상수 시간으로 비교한다.
- **redirect URI:** 설정 하나로 고정한다(`MONTRACER_OIDC_REDIRECT_URL`, query·fragment 금지).
- **return_to:** `/`로 시작하는 같은 사이트 경로만 받는다. `//`, 역슬래시, 제어 문자, 2,048자 초과는 `/`로 바꾼다(open redirect 방지).
- **JIT 가입 없음:** 로그인한 사용자에게 membership이 없으면 session을 만들지 않고 `/login?error=no_membership`으로 보낸다. membership은 provisioning(tenant 생성 때 첫 Tenant Admin)과 멤버 관리가 만든다.
- **첫 조직:** membership이 여러 개면 tenant id 순 첫 조직으로 들어가고, `POST /auth/session/tenant`로 바꾼다.
  - 바꿀 때는 새 token을 만들고 이전 session을 폐기한다(session fixation 방지).
  - 새 session의 최대 만료는 이전 session의 것을 넘지 않는다. 전환을 반복해도 IdP 재인증 없이 12시간을 늘리지 못한다.
  - 멤버 초대가 생기면 "마지막 사용 조직"이나 선택 화면을 다시 정한다. 다른 조직이 사용자를 멤버로 넣어 첫 조직이 바뀌는 것을 막기 위해서다.
  - member가 아닌 조직은 403이다. 자기 membership 목록이라 존재를 숨길 이유가 없다.
- **실패 사유:** `/login?error=state_mismatch|idp_error|token_invalid|no_membership|unavailable`. IdP가 준 오류 문구는 옮기지 않는다.
- **discovery:** 처음 로그인할 때 하고, 실패하면 다음 로그인에서 다시 한다. IdP 장애가 control-api 기동을 막지 않는다.
- **IdP 오류 분류:** token endpoint가 4xx(`invalid_grant` 등)로 거절할 때만 `token_invalid`다. 연결 오류·timeout·5xx는 `unavailable`이고 로그에 남는다(본문 없이 상태 코드만).
- **지표·경보:** 결과는 `montracer_auth_logins_total{outcome}`로 센다. `unavailable`이 10분에 3건 이상이면 `MontracerLoginUnavailable`(ticket, RB05)이다.
- library는 `github.com/coreos/go-oidc/v3`(v3.21.0)과 `golang.org/x/oauth2`(v0.37.0)다. JWT·JWKS·discovery 검증을 직접 구현하지 않는다.

### 4. CSRF는 session token의 HMAC, 변경 요청마다 확인

- CSRF token = base64url(HMAC-SHA256(csrf key, session token)). 저장하지 않는다.
  - `GET /auth/session`이 돌려주고, 화면은 변경 요청(GET·HEAD·OPTIONS 밖)마다 `X-CSRF-Token`에 싣는다.
  - 다르면 403이다(`FORBIDDEN`).
- Origin header가 있으면 화면 origin(`MONTRACER_PUBLIC_WEB_URL`)과 같아야 한다(심층 방어).
- SameSite=Lax가 다른 사이트의 POST에 cookie를 보내지 않지만, 하위 도메인·구형 브라우저에는 token이 필요하다(D04 §02).
- 조회 API의 POST(`/query` 등)도 변경 요청 규칙을 따른다. 판단을 method로만 한다(경로별 예외를 두지 않는다).
- key 하나(`MONTRACER_SESSION_KEY_HEX`, 32 byte 이상)에서 HMAC으로 CSRF key와 봉인 key를 나눈다. query-api와 control-api는 같은 key를 쓴다(CSRF token이 같아야 한다).

### 5. 두 API의 인증 순서 — bearer가 먼저, 없으면 session

- `Authorization: Bearer`가 있으면 API key로만 인증한다. 없고 session cookie가 있으면 session으로 인증한다.
- session API(`/auth/session`, logout, 전환)는 cookie로만 인증한다. API key에는 session이 없다.
- 계약 1: tenant는 session 행(로그인 때 고른 membership)에서만 온다. `X-Tenant-ID`·본문 값은 보지 않는다.

### 6. 설정 off(rollback)

- `MONTRACER_SESSION_KEY_HEX`가 없으면 session을 받지 않는다. 그러면 bearer key만 받고 `/auth/*`는 404다.
- `MONTRACER_OIDC_ISSUER`가 없으면 로그인 경로가 404다.
- 개발 환경(`make dev`)은 그대로 seed key proxy를 쓴다. 로그인 화면·개발 IdP·화면 쪽 session 사용은 단계 B다.

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| **A. 서버 저장 session + 무작위 token cookie (채택)** | 즉시 폐기·역할 반영. DB에 hash만 둔다 | 요청마다 제어 DB 조회 1회(key 인증과 같은 경로) |
| B. 자기완결 JWT session cookie | DB 조회가 없다 | 만료 전 폐기 불가(D04 §02 60초 전파와 충돌). key 회전·크기 문제 |
| C. 로그인 state를 서버 표에 | cookie 크기 걱정이 없다 | 로그인 시도마다 행이 생기고, 정리 job이 필요하다 |
| D. CSRF synchronizer token 저장 | 표준적이다 | 저장·조회가 늘어난다. HMAC 파생과 보안 성질이 같다(OWASP의 signed double-submit) |
| E. email을 사용자 키로 | 사람이 읽기 쉽다 | 변경·재사용 때문에 다른 사람이 같은 membership을 얻을 수 있다 |
| F. JIT로 viewer 자동 가입 | 온보딩이 쉽다 | 같은 IdP의 누구나 조직 데이터를 보게 된다(B2B에서 위험). D04 §12 |

## 결과

- 이점:
  - 사람이 IdP로 로그인해 역할 범위 안에서 두 API를 쓴다.
  - 폐기·역할 변경이 다음 요청부터 반영된다.
  - token 원문은 서버 어디에도 남지 않는다.
- 비용:
  - 새 의존성 3개(go-oidc, oauth2, go-jose)
  - 새 secret 2개(session key, OIDC client secret)
  - session 요청마다 제어 DB 조회
- 영향 받는 계약:
  - migration 00013(`user_sessions`, memberships `member_lookup` 정책)
  - OpenAPI `auth` 5개 operation과 `session` security scheme
  - 계약 시험이 session cookie·`X-CSRF-Token`을 공통으로 받음
- **남은 것:**
  - 단계 B: 화면 로그인·조직 전환·CSRF header, 개발 IdP와 seed membership, dev proxy 대체
  - 멤버 관리 API(초대·역할 변경·제거)
  - MFA step-up(`step_up_at` 열만 있음, ADR 0015 §2)
  - 만료 session 정리 job(지금은 그 조직의 로그인 때 지운다)
  - ~~Content-Type 415 강제~~ — 2026-10-20 실행(ADR 0060 §6 변경 이력)
  - 조직 정지 때 API key도 막기(지금 key 인증은 조직 상태를 보지 않는다 — 기존 공백, RB05·RB03은 함께 폐기)
  - SAML·SCIM(G1)
  - URL 조직과 session tenant의 불일치 403(D02 §12, 화면 조직 slug와 함께)

## 기능 상태

- requirements-registry의 F07은 `planned`로 둔다. 상태는 release·Gate 증거로 바꾼다(F01~F06과 같다).
- 진행은 작업계획서 E01에 적는다.

## 변경 이력

- 2026-10-20 (spec-reviewer 반영, PR polynomeer/montracer#65)
  - 조직 전환이 최대 만료를 늘리지 않게(P1)
  - 정지·폐쇄 조직 차단
  - IdP 4xx만 token_invalid
  - 로그인 지표·경보·RB05
  - 격리 suite에 session
  - 만료 행 조직 단위 정리
  - `WithTenant` 예외와 첫 조직 선택 재검토 조건 명시

## Rollback

- `MONTRACER_SESSION_KEY_HEX`·`MONTRACER_OIDC_ISSUER`를 비운다. bearer key 동작은 그대로다.
- migration down은 `user_sessions`와 `member_lookup` 정책을 지운다. 그러면 모든 browser session이 끝난다.

## 재검토 조건

- session 조회가 제어 DB 부하의 주된 원인이 되면 짧은 cache를 검토한다. 이때 폐기 전파 60초 상한을 지킨다.
- IdP가 여럿(조직별 SSO)이 되면 issuer 선택·조직별 IdP 설정으로 넓힌다(G1 SAML·OIDC federation).

## 근거 (빅테크 사례·표준)

- **OAuth 2.0 Security BCP (RFC 9700) · RFC 7636 PKCE** — https://www.rfc-editor.org/rfc/rfc9700
  - confidential client에도 PKCE를 권장하고, redirect URI는 정확히 일치시킨다. state로 CSRF를 막는다.
  - §3의 근거다.
- **Google OpenID Connect** — https://developers.google.com/identity/openid-connect/openid-connect
  - state·nonce 검증, ID token 서명·aud·exp 검증, 식별자로 `sub`를 쓰고 email을 쓰지 말라는 권고.
  - §2·§3의 근거다.
- **OWASP Session Management·CSRF Prevention Cheat Sheet**
  - https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html
  - https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html
  - 서버 측 session과 무작위 id, `__Host-` 접두어·HttpOnly·Secure·SameSite, idle·absolute timeout, 권한 변경 시 재발급, HMAC 기반 CSRF token, Origin 검사.
  - §1·§3·§4의 근거다.
- **Kubernetes(kube-apiserver)의 OIDC 인증** — https://kubernetes.io/docs/reference/access-authn-authz/authentication/#openid-connect-tokens
  - go-oidc로 ID token을 검증하고, 사용자를 issuer + claim으로 식별한다(prefix로 issuer를 구분).
  - §2의 issuer 구분, §3의 library 선택 근거다.
- **GitHub Enterprise·Grafana의 SSO 로그인**
  - Grafana 문서: https://grafana.com/docs/grafana/latest/setup-grafana/configure-security/configure-authentication/generic-oauth/
  - JIT 사용자 생성은 기본 역할을 관리자가 정하고, 조직 소유자 권한은 자동 부여하지 않는다.
  - Grafana는 org 전환 시 사용자 세션의 org 문맥을 바꾼다.
  - §3의 JIT 금지, 조직 전환 근거다.

## 증거

- 단위:
  - `internal/session`: 발급·인증, CSRF·Origin, idle·최대 만료·연장 상한, 폐기, 멤버 제거, 저장소 장애, 봉인 state 위조·만료·불일치, return_to, env
  - 가짜 IdP로 실제 PKCE S256·ID token 서명·nonce·audience 검증
- control-api: 로그인 흐름 전체, 조직 전환(이전 session 무효), 로그아웃, JIT 거절, 위조 state, IdP 오류·장애, 설정 off
- query-api: cookie GET, POST의 CSRF
- 계약: 응답·요청이 OpenAPI 원천과 같다
- 통합(PostgreSQL, CI):
  - 조회 정책(한 hash·한 사용자), tenant 범위 연장·폐기, 연장 상한
  - 역할 변경·제거 반영, 감사
  - 열 권한(최대 만료 변경 불가)
