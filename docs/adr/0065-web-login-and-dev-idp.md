# ADR 0065: 화면 로그인·조직 URL·개발 IdP (F07 단계 B)

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-20 "merge하고 이어서 진행해줘", ADR 0064 후속)
- Owner: Frontend lead · Backend/Security lead
- 날짜: 2026-10-20 (제안·결정)
- 관련: F07, E01·E04 · D05 §01(조직 전환·공유 URL), §04("401은 복귀 URL을 보존한 로그인"), D02 §12("URL tenant와 불일치하면 403"), D04 §02 · ADR 0042(dev proxy), 0064

## 배경

ADR 0064가 서버 쪽 로그인(OIDC·session·CSRF)을 만들었다. 화면에서 남은 일은 다음과 같다.

- **화면:**
  - 로그인 여부를 확인하고, 401이면 복귀 경로를 담아 로그인으로 보낸다.
  - 변경 요청에 CSRF header를 싣는다.
  - 사용자·역할을 보이고, 조직 전환과 로그아웃을 제공한다.
- **조직 URL:** 화면 URL은 `/o/{org}`인데 조직에 사람이 읽는 이름이 없다(tenants에 이름 열 없음). D02 §12는 URL 조직과 인증 조직이 다르면 403이라고 한다.
- **로컬 개발·CI:** 실제 로그인 흐름을 돌릴 IdP가 없다. 지금 화면은 dev proxy가 seed API key를 붙여 동작한다(ADR 0042).

## 결정

### 1. 조직 표시 이름 `tenants.display_name`

- 소문자 slug(`^[a-z0-9][a-z0-9-]{0,62}$`)이고 전역 고유다. provisioning(owner)만 바꾼다(migration 00014, 앱 role은 SELECT만).
- session 응답의 `tenant_name`과 `memberships[].tenant_name`으로 준다. 화면 URL 조직은 `tenant_name`, 없으면 tenant id다.
- seed는 `acme`·`globex`를 넣는다.

### 2. 화면의 session 확인 — 확인 중에는 막지 않는다

- 조직 화면은 처음 열 때 `GET /api/v1/auth/session`을 한 번 부른다.
  - **200:** session 모드다. 사용자 메뉴를 보이고, 401 처리기를 등록한다.
  - **404:** 로그인 꺼짐이다(API key proxy 모드). 지금처럼 동작한다.
  - **401:** `/api/v1/auth/login?return_to=<지금 경로>`로 보낸다(D05 §04).
- **확인 중과 확인 실패(5xx·연결 오류) 때는 화면을 그대로 그린다.** 화면의 API 호출이 같은 서버 판단(401·403·503)을 받고 각자 상태로 보인다. 그래서 첫 그림을 session 확인 왕복만큼 늦추지 않는다.
- **session으로 쓰는 중 API가 401이면**(만료·폐기) 지금 위치로 돌아오는 로그인으로 보낸다.
- **CSRF token:** session 응답의 값을 module 상태(메모리)에 두고 변경 요청마다 `X-CSRF-Token`에 싣는다. localStorage·cookie에 두지 않는다.

### 3. URL 조직 ≠ session 조직

- URL 조직이 사용자의 **다른 membership**이면 바꿀지 묻는다. 조용히 바꾸지 않는다. 공유 링크를 연 사람이 지금 조직을 모른 채 다른 조직으로 넘어가지 않게 하기 위해서다.
  - 바꾸면 `POST /auth/session/tenant`(새 token) 뒤 같은 URL을 다시 불러온다.
- **member가 아닌 조직**이면 "접근할 수 없습니다"와 내 조직 link를 보인다(D02 §12 403). 화면은 그리지 않는다.
- 루트 `/`는 session 조직으로 간다(로그인 꺼짐이면 개발 기본 조직).
- **조직 전환과 로그아웃은 페이지 전체를 다시 불러온다.** 이전 조직의 조회 결과·상태·진행 중 요청이 하나도 남지 않는다(D05 §01 "조직 전환은 cache·query·stream 취소").

### 4. 로그인 화면 `/login`

- 비밀번호를 받지 않는다. "SSO로 로그인" 버튼이 `/api/v1/auth/login?return_to=`로 보낸다(복귀 경로는 같은 사이트 경로만).
- 실패 사유(`state_mismatch`·`idp_error`·`token_invalid`·`no_membership`·`unavailable`)는 사람이 읽는 문장으로 보인다. 모르는 값은 일반 문장이다. IdP 문구는 오지 않는다.
- 로그아웃 뒤에는 `/login?logged_out=1`로 간다.

### 5. 개발 IdP `scripts/dev/devidp` — 로컬·CI 전용

- **범위:** Go 프로그램 하나다.
  - discovery·JWKS·authorize(개발 사용자 고르기 화면)·token을 제공하고, PKCE S256과 RS256 ID token을 쓴다.
  - loopback에만 bind한다(아니면 기동 거부). 서명 key는 기동마다 만든다.
  - client·secret·redirect URI는 정확히 일치해야 하고, code는 60초·한 번만 쓴다. 잘못된 요청은 redirect하지 않고 거절한다.
- **사용자:** `dev-admin`(acme 관리자·globex 조회자), `dev-viewer`(acme 조회자), `dev-outsider`(membership 없음). seed가 membership을 만든다.
- **연결:**
  - `make dev`가 함께 띄운다. control-api·query-api에 session key와 OIDC 설정이 들어간다. callback은 web dev server(`http://localhost:15173/api/v1/auth/callback`)를 거친다.
  - dev proxy는 `MONTRACER_DEV_AUTH=session`이면 key를 붙이지 않고 cookie를 넘긴다. 기본(key)에서는 `/api/v1/auth`를 404로 숨겨 지금처럼 로그인 없이 동작한다.
- **smoke 5c:** 개발 IdP로 실제 로그인을 지난다.
  - session(acme 관리자) → 조회 API(GET 성공, CSRF 없는 POST 403) → globex 전환(조회자) → 로그아웃 → 미인증
  - membership 없는 사용자는 거절된다.
  - CI 통합 job이 실행한다.

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| **A. 확인 중에도 화면 그리기 (채택)** | 첫 그림이 빠르다. 서버가 같은 판단을 하므로 보안 차이가 없다 | 로그인 필요할 때 화면이 잠깐 보였다 넘어간다 |
| B. 확인이 끝날 때까지 막기 | 깜빡임이 없다 | 모든 화면이 왕복 하나만큼 늦다. 확인 실패 시 화면 전체가 막힌다 |
| **URL 조직이 다르면 묻기 (채택)** | 공유 링크로 조용히 조직이 바뀌지 않는다 | 클릭이 하나 늘어난다 |
| URL 조직으로 자동 전환 | 클릭이 없다 | 링크 하나가 사용자의 조직 문맥을 바꾼다(혼동·오조작) |
| **개발 IdP 직접 구현 (채택)** | 새 container 이미지·버전 고정이 없다. CI에서 함께 돈다 | 표준 전체를 구현하지 않는다(개발 전용, production 금지) |
| Dex·Keycloak container | 실제 제품이다 | 이미지·설정·기동 시간·메모리가 는다(로컬 Docker 메모리 공유 문제). CI 시간도 는다 |
| URL에 tenant id 사용 | 이름 열이 필요 없다 | 사람이 읽지 못한다. 기존 `/o/acme` 링크가 깨진다 |

## 결과

- 이점:
  - 사람이 IdP로 로그인해 화면을 쓴다. 조직 전환·로그아웃·만료 처리가 있다.
  - 로컬과 CI에서 실제 로그인 흐름을 확인한다.
- 비용:
  - 개발 전용 프로그램 하나
  - tenants 열 하나
  - session 확인 요청 하나(조직 화면을 처음 열 때)
- 영향 받는 계약:
  - OpenAPI session 응답에 `tenant_name`(additive)
  - migration 00014
  - 화면 route `/login`
  - dev proxy 경로 `/api/v1/auth`
- 남은 것:
  - 화면 권한(capabilities)에 따른 메뉴·버튼(403 안내는 지금도 서버가 준다)
  - 조직 전환 때 전체 다시 불러오기 대신 조회 cache 분리
  - Safari 등에서 localhost Secure cookie가 동작하는지 개발 문서에 확인 추가
  - Content-Type 415

## Rollback

- 화면: session 확인이 404면 지금처럼 동작한다. 서버에서 로그인을 끄면(ADR 0064 §6) 화면도 꺼진다.
- migration 00014 down은 열을 지운다. 화면은 tenant id URL로 돌아간다.
- dev: `MONTRACER_DEV_AUTH`를 빼면 key 모드다.

## 근거 (빅테크 사례)

- **Grafana — 로그인·조직 전환** — https://grafana.com/docs/grafana/latest/administration/organization-management/
  - 사용자 메뉴에서 조직을 바꾸면 페이지를 다시 불러온다.
  - 다른 조직의 dashboard link를 열면 조직을 바꿀지 묻는다(`?orgId=`).
  - §3의 근거다.
- **GitHub·Google Workspace — 계정·조직 전환** — 링크의 조직이 지금 계정에 없으면 접근할 수 없다는 안내와 다른 계정으로 전환하기를 보인다. 조용히 바꾸지 않는다. §3의 근거다.
- **Auth0 SPA SDK·OWASP CSRF Cheat Sheet**
  - https://auth0.com/docs/libraries/auth0-single-page-app-sdk
  - https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html
  - 401이면 복귀 경로(`appState`/`returnTo`)를 담아 로그인으로 보낸다. token은 메모리에 두고, 변경 요청에는 custom header CSRF token을 쓴다.
  - §2의 근거다.
- **Kubernetes·Argo CD의 로컬 개발용 Dex 설정**
  - 로컬·CI는 개발 IdP를 쓰고 production IdP와 분리한다. 개발 사용자는 고정이다.
  - 우리는 container 대신 작은 Go 프로그램으로 같은 역할을 한다(이미지·메모리 비용 때문, 위 후보 표).
  - §5의 근거다.

## 증거

- **화면 시험** `apps/web/src/app/SessionGate.test.tsx`:
  - 사용자 메뉴·전환·로그아웃(CSRF header, 전환 뒤 새 token)
  - 401 → 복귀 경로 로그인, API 401 → 로그인
  - URL 조직 불일치(전환 묻기·접근 불가), 로그인 꺼짐, 루트
  - 로그인 화면 문장·복귀 경로 거르기
- **dev proxy:** `dev-proxy.test.ts`의 session·key 모드
- **개발 IdP:** `scripts/dev/devidp/main_test.go`. 실제 `internal/session` client로 PKCE·code 1회·redirect·PKCE 강제를 검사한다.
- **통합:** `tenants.display_name` 규칙·membership 이름(`sessions_integration_test.go`)
- **smoke 5c:** CI 통합 job
