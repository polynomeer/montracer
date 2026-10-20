# ADR 0067: 멤버 관리와 초대 링크 (F07 단계 C — API)

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-20 다음 작업 선택 "멤버 관리 API·화면")
- Owner: Backend/Security lead
- 날짜: 2026-10-20 (제안·결정)
- 관련: F07, E01 · D04 §01(Tenant Admin "정책·멤버·키·삭제 job", "step-up 후 권한과 삭제"), §02(session·폐기 전파), §12(JIT 금지) · D02 §11(감사)·§20(Idempotency-Key) · ADR 0015, 0016, 0060, 0064, 0066

## 배경

사람 로그인(ADR 0064·0065)과 MFA step-up(ADR 0066)이 생겼다. 하지만 멤버를 넣고 빼는 경로가 없다.

- 지금 membership은 provisioning·seed가 DB에 직접 넣는다.
- 로그인은 membership이 있어야 한다. JIT 가입은 없다(D04 §12).
- 그래서 고객 관리자가 동료를 들일 방법이 없다.

명세는 Tenant Admin이 "멤버"를 관리하고, 그 권한은 "step-up 후"라고만 쓴다. 다음은 정하지 않았다.

- 사람을 어떻게 들이는가(초대 방식)
- 누가 목록을 보는가
- 마지막 관리자를 어떻게 지키는가
- 멤버를 뺄 때 무엇이 함께 끝나는가

## 결정

### 1. API와 권한

| 경로 | 권한 | 비고 |
|---|---|---|
| `GET /api/v1/members` | `members.read` | user_id 순, cursor(최대 1,000) |
| `PATCH /api/v1/members/{user_id}` | `members.manage` | `{role}`. If-Match 필수(없으면 428, 다르면 412). 같은 역할이면 그대로 |
| `DELETE /api/v1/members/{user_id}` | `members.manage` | If-Match 선택. 그 사용자의 이 조직 session과 그가 만든 대기 중 초대를 함께 폐기 |
| `POST /api/v1/invitations` | `members.manage` | Idempotency-Key 필수, 링크는 한 번만 |
| `GET /api/v1/invitations` | `members.read` | 대기 중만, 최대 50 |
| `DELETE /api/v1/invitations/{id}` | `members.manage` | 대기 중만(아니면 404) |
| `POST /api/v1/auth/invitation` | 없음(로그인 전) | 초대 수락 시작 |

- **`members.read`(새 action)**: Tenant Admin과 Security Auditor다.
  - Auditor는 "누가 접근할 수 있는가"를 검토한다(D04 §01 audit·정책 metadata).
  - step-up은 필요 없다(조회).
- **`members.manage`**: Tenant Admin이고, 15분 안에 MFA step-up을 했어야 한다(ADR 0015 §2, 0066).
  - API key에는 step-up이 없으므로 API key로는 멤버를 바꿀 수 없다.
  - step-up이 없으면 403이고 `details.step_up_required: true`가 붙는다.
- 다른 조직의 멤버·초대는 없는 것과 같다(404). tenant는 principal에서만 온다(계약 1).
- **낙관적 잠금(D02 §20)**
  - `memberships.revision`을 두고, 목록의 `revision`과 PATCH 응답의 ETag로 보인다.
  - 두 관리자가 낡은 화면에서 같은 사람의 역할을 바꾸면 뒤의 것이 412다(무조건 덮어쓰지 않는다, D02 §11).
- **outbox(D02 §11·§14)**
  - 멤버·초대 변경은 감사와 같은 트랜잭션에 outbox event를 쓴다(`writeAuditAndOutbox`).
  - 대상: `member.role_changed`·`member.removed`·`member.invitation_created`·`member.invitation_revoked`·`member.invitation_accepted`.
  - 앞으로 권한 cache·stream 차단(D04 §12 "60초 이내")이 이 event로 반응한다.
  - payload에는 역할·수만 있다. 이름(PII)·token은 없다.

### 2. 마지막 Tenant Admin은 남는다

- 마지막 관리자를 내리거나 빼면 409 CONFLICT다. 자기 자신을 내리는 경우도 같다.
- 역할 변경·제거는 트랜잭션에서 조직의 관리자 행을 `FOR UPDATE`로 먼저 잠근다(user_id 순).
  - 관리자 둘이 동시에 서로를 내려도 한 명은 남는다(통합 시험).
- 조직이 관리자 없이 남으면 고객이 스스로 복구할 수 없다. 운영자 개입(break-glass)이 필요해진다.

### 3. 빼면 session도 끝난다

- membership 삭제와 같은 트랜잭션에서 그 사용자의 이 조직 session을 `revoked_at`으로 폐기한다.
- 그 전에도 역할은 요청마다 membership에서 읽으므로, 다음 요청부터 막힌다(ADR 0064). 폐기는 그 상태를 명시적으로 남긴다.
- 그 사용자가 발급한 API key는 발급자 역할이 없어져 인증에서 무효가 된다(ADR 0015 §1).
- 그 사용자가 만든 **대기 중 초대도 폐기**한다. Tenant Admin에서 강등될 때도 같다.
  - 떠나거나 탈취된 관리자가 미리 만든 링크로 다시 들어오는 경로를 막는다(D04 §12 deprovision).
- 감사 `member.removed {role, sessions_revoked, invitations_revoked}`를 남긴다. 역할 변경은 `member.role_changed {from, to, invitations_revoked}`다.

### 4. 초대는 한 번 쓰는 링크 — 7일, 원문 미저장, 관리자 역할 없음

- **token**: `mtinv_` + 32 byte 무작위(base64url)다.
  - 저장은 SHA-256뿐이다(`member_invitations.token_hash`, 전역 고유).
  - 원문은 생성 응답의 `invitation_url`에만 있다.
  - Idempotency-Key 재생 응답에는 링크가 없다. 대신 경고 `invitation_url_not_replayed`가 붙는다. 알림 policy secret과 같은 방식이다(ADR 0053).
- **링크 모양**: `<화면 origin>/invite#mtinv_…`이다.
  - token을 fragment에 둔다. fragment는 서버로 가지 않으므로 서버 로그·proxy 로그·Referer에 남지 않는다.
  - 초대 페이지(다음 PR의 화면)가 fragment를 읽어 본문으로 보낸다.
- **유효 기간**: 7일이고, 한 번만 쓴다. 관리자가 폐기할 수 있다.
- **상한**: 대기 중 초대는 조직마다 50개까지다. 넘으면 409다.
  - 링크가 무한히 쌓이면 새는 면이 커진다.
  - 상한 검사와 삽입은 조직 단위 advisory lock으로 줄 세운다.
- **역할**: `tenant_admin`으로는 초대하지 않는다.
  - 링크는 가진 사람 누구나 쓸 수 있는 bearer다. 새면 관리자 권한이 샌다.
  - 관리자는 가입 뒤, step-up한 관리자가 역할을 올린다. "JIT로 관리자를 만들지 않는다"(D04 §12)와 같은 방향이다.
- **label**: 선택이고 1–64자다. 관리자가 구분용으로 쓴다(예: "결제팀 신규").
  - 이메일을 받지 않는다. 사람 식별 PII를 새로 저장하지 않는다.
  - 초대를 특정 계정에 묶지 않는다(아래 후보 B).

### 5. 수락 흐름

1. 초대 페이지가 `POST /api/v1/auth/invitation {"token"}`을 보낸다.
   - 로그인 전이라 session·CSRF token이 없다.
   - JSON 본문만 받는다(ADR 0060 §6 415). 따라서 다른 사이트의 form 강제 전송이 안 된다.
   - Origin이 있으면 화면 origin이어야 한다. 다른 사이트 script가 피해자를 남의 조직에 넣는 것(login CSRF)을 막는다.
2. 초대를 지금 쓸 수 있으면 IdP authorization URL과 **들어갈 조직 이름·역할**을 돌려준다. 봉인 로그인 state cookie에는 token의 **SHA-256만** 넣는다.
   - 초대 페이지는 "acme 조직에 조회자로 들어갑니다"를 보여 주고, 사용자가 확인한 뒤에만 IdP로 이동한다.
   - 남이 보낸 링크로 모르는 사이 다른 조직에 들어가, 그 조직에서 일하며 정보를 넣는 일을 막는다.
   - 쓸 수 없으면(없음·사용됨·폐기·만료) 404다. 이 넷을 구분하지 않는다.
3. callback이 IdP 사용자로 수락한다(`member.invitation_accepted`). 한 트랜잭션에서 두 가지를 한다.
   - 초대를 쓴다(`accepted_at`, `accepted_by`).
   - membership을 만든다(초대한 역할, IdP name).
   - 그다음 그 조직의 session을 만든다(`montracer_auth_logins_total{outcome="invitation_ok"}`).
4. 이미 그 조직의 멤버면 역할을 바꾸지 않는다. 초대만 쓰고 그 조직으로 로그인한다.
   - 초대로 권한을 내리거나 올리지 않는다.
5. 실패하면 `/login?error=invitation_invalid`다(`invitation_invalid`).
   - 해당하는 경우: 그 사이 다른 사람이 썼음, 폐기, 정지·폐쇄 조직.
- tenant는 저장된 초대 행에서 온다(요청 입력이 아님). 조회는 정확한 hash 한 행만 읽는 `invitation_lookup` 정책을 쓴다(ADR 0016 §4 방식).

### 6. 멤버 목록에 보일 이름

- `memberships.display_name`(IdP name claim)과 `last_login_at`을 둔다. 로그인·조직 전환 때 갱신한다.
- 이름이 없는 IdP면 이전 값을 둔다.
- 감사·로그에는 쓰지 않는다. 화면 표시용이다(ADR 0064의 session 이름과 같은 취급).

### 7. 설정 off(rollback)

`MONTRACER_MEMBER_API_ENABLED=false`면 멤버·초대·초대 수락 경로가 404다. 기본은 켜짐이다.

- 초대 페이지(`/invite`)는 다음 PR의 화면이다. 그 전에 만든 링크는 화면에서 열리지 않는다.
  - 그때까지는 API·smoke로만 쓴다.
  - 기본을 켜 둔 이유: API 자체는 완결돼 있고, 화면이 바로 이어서 들어온다.

### 8. 예외와 남은 위험

- **Idempotency-Key 예외**: `POST /api/v1/auth/invitation`은 Idempotency-Key를 받지 않는다(D02 §20 "모든 POST mutation").
  - 상태를 바꾸지 않는다. 초대를 쓰는 것은 callback이다.
  - 응답은 로그인 시작(봉인 cookie 설정)뿐이고 다시 보내도 같은 결과다.
  - 로그인·step-up 시작(ADR 0064·0066)과 같은 인증 경로 예외다.
- **rate limit 없음**: 로그인 전 경로라 요청마다 DB 조회 한 번이 든다.
  - token은 256 bit라 추측은 불가능하다. 다만 부하 경로가 된다.
  - 다른 공개 경로와 함께 edge(ingress) rate limit에서 다룬다. control-api README의 "아직 없는 것"(rate limit)에 있다.
- **PII 보존(D04 §03·§04)**
  - `memberships.display_name`은 membership과 함께 지워진다. 그 사용자를 빼면 사라진다.
  - `member_invitations.created_by`·`accepted_by`는 사람 이름이 아니라 user_id(issuer hash + sub)다.
  - 초대 행은 감사 대조를 위해 남기고, 조직 삭제 job(D04 §04)에서 tenant 데이터와 함께 지운다(재검토 조건).

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A (채택) 한 번 쓰는 초대 링크(7일, hash 저장, fragment, 관리자 역할 없음) + 수락 시 IdP 로그인 | 이메일 발송 기반 없이 동작한다. PII를 새로 저장하지 않는다. 고객 IdP 인증을 그대로 쓴다 | 링크가 새면 그 역할로 누구나 들어올 수 있다(7일·한 번·폐기·관리자 불가로 줄임) |
| B 이메일 주소에 묶은 초대(수락 시 ID token `email`·`email_verified` 대조) | 링크가 새도 다른 계정이 못 쓴다 | 이메일(PII) 저장. IdP마다 email claim 신뢰도가 다르다(검증 안 된 email). 발송 기반 필요. G1 SCIM(F08)에서 IdP가 계정을 넣는 쪽이 맞다 |
| C 관리자가 user_id(`oidc:…:sub`)를 직접 입력해 추가 | 링크가 없다 | 관리자가 상대의 IdP sub를 알아야 한다(현실적이지 않음). 오타가 다른 사람을 넣는다 |
| D 이메일 도메인으로 자동 가입(JIT) | 마찰이 없다 | D04 §12 위반(JIT 금지). 도메인 공유 조직에서 넘친다 |
| E 초대로 tenant_admin 허용 | 한 단계 줄어든다 | 링크 유출 = 관리자 권한 유출. step-up 없이 관리자가 생긴다 |

## 결과

- 이점: 고객 관리자가 스스로 멤버를 들이고, 역할을 바꾸고, 뺄 수 있다. 모든 변경은 step-up과 감사를 거친다.
- 비용 / 운영 부담:
  - 초대 링크 전달은 고객 몫이다(메신저·메일). 이메일 발송은 하지 않는다.
  - 대기 중 초대는 50개까지다.
- 영향 받는 계약:
  - OpenAPI: 위 7개 operation과 schema `Member`·`MemberRole`·`Invitation`이 추가된다. apidiff는 호환이다.
  - migration 00016
    - `memberships.display_name`·`last_login_at`
    - `member_invitations`(RLS+FORCE, `tenant_isolation`·`invitation_lookup`, INSERT·SELECT와 수락·폐기 열만 UPDATE)
  - authz: `members.read`.
  - 지표 outcome: `invitation_ok`·`invitation_invalid`.
  - 감사: `member.role_changed`·`member.removed`·`member.invitation_created`·`member.invitation_revoked`·`member.invitation_accepted`.
- tenant 영향:
  - 모든 변경은 `WithTenant`다.
  - 수락만 hash 조회 정책으로 tenant를 찾는다. 찾은 tenant는 저장 행에서 온다.
  - 격리 suite가 다른 조직 멤버·초대의 조회·변경 404를 확인한다.
- retention 영향:
  - 초대 행은 수락·폐기 뒤에도 남는다(감사 대조).
  - 정리 job은 조직 삭제(F07 삭제 job)와 함께 정한다(재검토 조건).

## 기능 상태

- F07: 멤버 관리 API는 이 ADR이다. 화면(설정 > 팀, 초대 페이지, 403 step-up 안내)은 다음 PR이다.
- 조직 정지 때 API key 막기는 남았다.

## Rollback

- `MONTRACER_MEMBER_API_ENABLED=false` 또는 이전 control-api 이미지로 되돌린다.
  - 이미 만든 membership은 남는다(정상 멤버다).
  - 대기 중 초대는 쓸 수 없게 된다.
- migration 00016 down은 `member_invitations`와 새 열을 지운다. 감사 이벤트는 남는다.
- **순서가 중요하다.** 로그인(`CreateSession`)이 flag와 무관하게 `memberships.display_name`·`last_login_at`을 갱신한다.
  - 배포는 migration 00016 → 새 이미지 순서다.
  - 되돌릴 때는 이전 이미지 → migration down 순서다.
  - 거꾸로 하면 모든 로그인이 실패한다.

## 재검토 조건

- SCIM·SAML(F08)이 들어오면, IdP가 계정을 넣고 빼는 쪽으로 옮길지 정한다. 그때 초대는 IdP 없는 조직용으로 남길지도 정한다.
- 초대 링크 유출 사고가 생기거나, 고객이 계정 고정 초대를 요구하는 경우(후보 B).
- 수락·폐기된 초대 행의 보존 기간(조직 삭제 job, D04 §04).
- 대기 중 50개·7일이 운영에서 모자란다는 근거가 생긴 경우.

## 근거 (빅테크 사례·표준)

- **GitHub organization invitations**
  - 초대는 7일 뒤 만료되고, 관리자가 대기 중 초대를 보고 취소할 수 있다.
  - 조직에는 owner가 한 명 이상 있어야 한다(마지막 owner는 뺄 수 없다).
  - 근거: GitHub Docs "Inviting users to join your organization", "Maintaining ownership continuity for your organization".
  - 채택한 것: 7일, 대기 목록·폐기, 마지막 관리자 보호.
- **Slack 초대 링크**
  - 링크로 workspace에 들이고, 관리자가 링크를 비활성화할 수 있다.
  - 근거: Slack Help Center "Invite new members to your workspace".
  - 채택한 것: 이메일 없는 링크 방식과 폐기.
- **Grafana organization users**
  - 마지막 organization admin은 제거할 수 없고, 초대는 대기 목록에서 취소한다.
  - 근거: Grafana Docs "Manage users in an organization".
- **OWASP Forgot Password Cheat Sheet**(일회성 token 지침)
  - token은 충분히 무작위이고, 한 번만 쓰고, 짧게 유효하며, 저장은 hash로 한다.
  - 채택한 것: 초대 token 처리.
- **OWASP "Information exposure through query strings in URL"**
  - URL query의 비밀은 서버·proxy 로그와 Referer로 샌다.
  - 채택한 것: token을 fragment에 두고 본문으로 보내는 방식.
- **D04 §12 JIT 금지**
  - 이 결정의 내부 근거다. 그래서 초대로 관리자를 만들지 않는다.

## 변경 이력

- 2026-10-21 (PR — API key 관리, ADR 0069): 초대 token 접두어를 `mti_`에서 `mtinv_`로 바꿨다. `mti_`는 ingest key 접두어와 같아
  비밀 탐지(secret scanning)·사고 대응에서 둘을 구분할 수 없었다. 배포 전이라 기존 링크는 없다.

- 2026-10-20 (spec-reviewer 반영, PR polynomeer/montracer#69)
  - PATCH If-Match·`memberships.revision`(P1)
  - 멤버·초대 변경 outbox event(P1)
  - 관리자 강등·제거 시 그가 만든 초대 폐기(P1)
  - 초대 수락 시작이 들어갈 조직·역할을 돌려줌(확인 화면)
  - 동시 수락·동시 생성 상한 시험
  - Idempotency-Key 예외·rate limit·PII 보존·배포 순서 명시

## 증거

- `internal/controlapi/members_test.go`
  - `TestMembersAPI`
    - 목록 쪽 나누기와 is_self, 조회자 403
    - step-up 없는 변경 403(`step_up_required`), 역할 변경·검증·다른 조직 404, 마지막 관리자 409, CSRF
    - 초대 Idempotency-Key·tenant_admin 거절·링크 한 번(재생에는 없음)
    - 수락(Origin·잘못된 token 404·IdP 로그인 뒤 developer session·재사용 404), 시작 뒤 폐기된 초대 거절
    - 빼면 session 미인증, step-up 만료
  - `TestInvitationLimitAndDisabled`: 상한 409, 설정 off 404
- `internal/controldb/members_integration_test.go`
  - 권한·step-up, If-Match(낡은 revision 412), 마지막 관리자(동시 강등 포함), 빼면 session 폐기, outbox
  - 관리자 강등·제거 시 그가 만든 초대 무효, 같은 초대 동시 수락은 한 명만, 동시 생성도 상한 50
  - 로그인이 이름·마지막 로그인 기록
  - 초대 멱등·조직 범위·한 번·폐기·만료·정지 조직·상한, 이미 멤버면 역할 유지
  - 열 단위 GRANT, 감사
- `tests/isolation` `TestMembersAcrossTenants`: step-up한 A 관리자 session이 X-Tenant-ID로 B를 지정해도 B의 멤버·초대를 보지도 바꾸지도 못함.
- smoke 5c: 개발 IdP로 dev-outsider 초대 → 수락(acme 조회자 session) → 재사용 거절 → 제거 → session 끝.
