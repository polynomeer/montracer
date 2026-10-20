# RB05 로그인과 browser session

- 원문: D04 §02(인증 흐름·폐기 전파), D04 §11(장애 대응 원칙). 사람 로그인(OIDC)과 browser session을 다룬다. API key는 RB03(break-glass 폐기)을 따른다.
- 관련 설계: ADR 0064, ADR 0016(RLS 조회 정책)
- 경보 규칙: [`deploy/prometheus/rules/montracer.rules.yml`](../../deploy/prometheus/rules/montracer.rules.yml) `montracer-auth` 그룹

## 먼저 알아둘 것

1. **로그인 장애와 session 장애를 나눈다.**
   - 로그인(control-api `/api/v1/auth/login`·`/callback`)은 IdP와 제어 DB에 의존한다.
   - 이미 로그인한 session의 요청은 제어 DB만 본다. IdP 장애 중에도 기존 session(최대 12시간)과 API key는 동작한다.
2. **session은 제어 DB의 `user_sessions` 행이다.**
   - 폐기(`revoked_at`)·멤버 제거·조직 정지(`suspended`·`closing`·`deleted`)는 다음 요청부터 막힌다. cache가 없다.
   - 제어 DB 장애 중 session 요청은 503이다(fail closed).
3. **token 원문은 어디에도 없다.** 저장소에는 SHA-256만 있다. 사용자 cookie 값을 받아 옮기지 않는다.
4. **실패 사유는 `/login?error=…`와 지표 `montracer_auth_logins_total{outcome}`에 남는다.**
   - `state_mismatch`: 로그인 cookie 없음·만료(10분)·위조. 다른 탭·오래된 IdP 화면에서 돌아온 경우가 흔하다.
   - `idp_error`: IdP가 거절했다(사용자 취소 포함).
   - `token_invalid`: ID token 검증 실패(서명·audience·nonce·만료) 또는 IdP의 4xx.
   - `no_membership`: 그 사용자에게 들어갈 조직이 없다(가입 JIT 없음).
   - `unavailable`: IdP·제어 DB에 닿지 않았다.

## 경보별 대응

### MontracerLoginUnavailable

- **탐지:** 10분에 3건 이상 로그인이 `unavailable`로 끝났다.
- **영향:** 새 로그인이 안 된다. 기존 session과 API key는 영향이 없다.
- **즉시 조치**
  1. control-api 로그에서 `oidc discovery failed`·`oidc exchange failed`·`login membership lookup failed`를 찾는다. 로그에는 원인 문구만 있고 code·token은 없다.
  2. **discovery·exchange 실패**
     - IdP 상태 페이지와 `MONTRACER_OIDC_ISSUER`의 `/.well-known/openid-configuration` 응답을 control-api가 도는 네트워크에서 확인한다. egress·DNS·TLS 인증서 만료가 흔한 원인이다.
     - discovery는 성공할 때까지 로그인마다 다시 시도한다. IdP가 돌아오면 재시작 없이 복구된다.
  3. **membership 조회 실패:** 제어 DB 장애다. RB01 공통 확인을 따른다.
- **복구 확인:** `montracer_auth_logins_total{outcome="ok"}`가 다시 증가한다.

## 절차

### 사용자 한 명의 session 끊기(계정 탈취 의심)

1. IdP에서 사용자를 비활성화한다. 그러면 새 로그인이 막힌다.
2. 조직 membership을 지우거나 역할을 낮춘다. 다음 요청부터 그 사용자의 모든 session이 미인증·축소된다.
3. membership을 남겨야 하면 그 사용자의 session 행을 폐기한다. 대상 tenant 범위 트랜잭션에서 실행하고 RB03 원칙대로 사유를 남긴다.
   ```sql
   BEGIN;
   SELECT set_config('app.tenant_id', '<tenant UUID>', true);
   UPDATE user_sessions SET revoked_at = now() WHERE user_id = '<oidc:…>' AND revoked_at IS NULL;
   COMMIT;
   ```

### session key 회전(`MONTRACER_SESSION_KEY_HEX`)

- key가 바뀌면 CSRF token과 로그인 state 봉인이 바뀐다.
  - 기존 session은 GET이 계속 되지만, 변경 요청은 화면이 `GET /api/v1/auth/session`으로 새 CSRF token을 받을 때까지 403이다.
  - 로그인 중이던 사용자(10분)는 `state_mismatch`로 다시 시작한다.
- 절차: control-api와 query-api에 같은 새 key를 **동시에** 배포한다. 둘이 다르면 화면의 CSRF token이 한쪽에서만 맞는다.
- key 유출이 의심되면 모든 session을 폐기한다(tenant마다 위 UPDATE를 `user_id` 조건 없이). 사유를 security incident로 기록한다.

### 조직 정지·폐쇄

- provisioning이 `tenants.status`를 `suspended`·`closing`·`deleted`로 바꾸면, 그 조직의 session은 다음 요청부터 미인증이고 새 로그인 목록에서도 빠진다(ADR 0064).
- API key도 함께 폐기한다(RB03).

## 사후 기록

- 장애 시간, 실패 건수(`outcome`별), 원인(IdP·네트워크·DB), 영향 받은 조직 수를 incident 기록에 남긴다. 사용자 식별자·token은 남기지 않는다.
