# ADR 0069: API key 관리 API — 발급(원문 1회)·목록·폐기(겹침 24시간), 수명 상한, 정지 조직 key 거절 (F07 — API)

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-21 다음 작업 선택 "API key 관리 API·화면")
- Owner: Backend/Security lead
- 날짜: 2026-10-21 (제안·결정)
- 관련: F07, E01 · D02 §20("Key | GET/POST /keys, DELETE /keys/{id} | 생성 시 원문 1회, 삭제는 revoke") · D04 §02(ingest key·교체 24시간·tenant 폐쇄 시 무효), §01 · ADR 0015(§1 실효 권한, §3 keys.read·keys.manage), 0016, 0033(break-glass 폐기), 0060(Idempotency-Key), 0066(step-up)

## 배경

key 형식·hash·인증·폐기 저장소는 M0 초기에 만들었다(ADR 0015, `internal/authz` `KeyHasher`, `internal/controldb` `KeyStore`). 하지만 key를 만드는 경로는 둘뿐이다.

- seed(`make seed`)
- 운영자 break-glass 폐기(ADR 0033)

고객 관리자가 수집기(ingest)나 CI(조회)용 key를 스스로 발급·폐기할 API가 없다. 또 D04 §02 "tenant 폐쇄 시 … ingest key … 무효화"는 session에만 적용돼 있었다(ADR 0064). 정지된 조직의 key는 계속 인증됐다.

D02 §20은 경로와 "원문 1회·삭제는 revoke"를 정했다. 다음은 정하지 않았다.

- 수명
- 이름
- 상한
- 교체 겹침을 주는 방법
- 응답 모양

## 결정

### 1. 경로와 권한(D02 §20, ADR 0015 §3)

| 경로 | 권한 | 비고 |
|---|---|---|
| `GET /api/v1/keys` | `keys.read`(Tenant Admin) | 최신순 1,000개. 원문·hash 없음. 폐기·만료된 key도 보인다 |
| `POST /api/v1/keys` | `keys.manage`(Tenant Admin + 15분 안의 step-up) | Idempotency-Key 필수. 201에 `token` 한 번 |
| `DELETE /api/v1/keys/{key_id}?grace_seconds=` | `keys.manage` | 그 시각부터 무효(0–86,400초) |

- API key로는 key를 관리할 수 없다. step-up이 없기 때문이다(ADR 0066).
- 다른 조직의 key는 404다. 쿼리에도 `tenant_id = app_tenant_id()`를 함께 둔다(계약 4).

### 2. 발급 요청

```json
{"kind": "ingest" | "api", "name": "…", "scopes": [...], "environments": [...], "expires_in_days": 1..365}
```

- **ingest**
  - scope는 `ingest.traces|metrics|logs`다.
  - environment는 1개 이상이다(D04 §02).
  - 발급자가 떠나도 수집이 끊기지 않는다(조직 소유, ADR 0015 §1).
- **api**
  - scope는 `authz.APIKeyScopes`다(조회·운영 action). step-up action(멤버·key 관리, 삭제)과 ingest는 줄 수 없다.
  - 발급자가 갖지 않은 scope는 403이다.
  - 쓸 때마다 발급자의 현재 역할과 교집합이다(ADR 0015 §1). 발급자가 빠지면 무효다.
- **수명은 필수이고 1–365일이다.** 만료 없는 key는 없다. 화면 기본값은 api 90일, ingest 365일로 둔다(다음 PR).
- **이름**은 선택이고 1–64자다(migration 00017 `api_keys.name`). 바꾸지 않는다 — 새 key로 교체한다.
- **environment 값**은 `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`이고 최대 20개, 중복은 없다.
- **원문**은 생성 응답에서 한 번만 준다(D02 §20, D04 §02).
  - Idempotency-Key 재생 응답에는 원문이 없다. 대신 경고 `token_not_replayed`가 붙는다(ADR 0053·0067과 같은 방식).
  - 목록은 `token_hint`(`mta_<key_id>_…`)만 준다. 어느 key인지 맞춰 보는 데 쓴다.
- **상한**: 살아 있는(만료·폐기 전) key는 조직마다 100개까지다(설계 가정). 넘으면 409다.
  - 조직 단위 advisory lock으로 검사와 삽입을 줄 세운다.

### 3. 폐기와 교체

- `DELETE /keys/{id}`는 `revoked_at = now + grace_seconds`이다.
  - 교체 절차는 "새 key 발급 → 배포 → 이전 key를 grace로 폐기"이고, 최대 24시간 겹친다(D04 §02 "최대 24시간 두 key의 유효기간을 겹치고").
  - 이미 더 이른 시각에 폐기된 key는 그대로 둔다(멱등).
- 인증은 요청마다 제어 DB를 조회한다(cache 없음). 따라서 폐기 시각이 지나면 바로 막힌다(D04 §02 60초 목표 안).

### 4. 정지·폐쇄 조직의 key는 인증하지 않는다

- key 조회(`LookupKey`)가 조직 상태를 함께 본다.
- `trial·active·past_due·restricted`만 인증한다. session과 같은 집합이다(ADR 0064).
- `suspended·closing·deleted`면 없는 key와 같게 미인증이다(응답·시간 차이 없음).
- ingest·api 모두 해당한다. 조직을 되살리면 다시 된다.

### 5. 초대 token 접두어 정정

- 초대 token 접두어를 `mti_`에서 `mtinv_`로 바꿨다(ADR 0067 변경 이력).
- `mti_`는 ingest key 접두어(`mti_<key_id>_<secret>`)와 같았다. 유출 탐지·사고 대응에서 둘을 구분할 수 없었다.

### 6. 설정 off(rollback)

`MONTRACER_KEY_API_ENABLED=false`면 `/api/v1/keys`가 404다. 기본은 켜짐이다. seed·break-glass 경로는 영향이 없다.

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A (채택) 수명 필수 1–365일, 원문 1회, 폐기는 DELETE + grace(≤24h), 조직당 살아 있는 key 100개 | 잊힌 key가 영원히 남지 않는다. 교체 절차가 API 하나로 된다 | 1년마다 교체가 필요하다(만료 알림은 아직 없음 — 재검토 조건) |
| B 만료 없는 key 허용(선택) | 수집기 운영이 편하다 | 유출된 오래된 key가 계속 유효하다. GitHub·GitLab이 만료 필수로 바꾼 이유와 같다 |
| C 교체 전용 `POST /keys/{id}/rotate`(새 key 발급 + 이전 key 예약 폐기를 한 번에) | 한 호출이다 | D02 §20 경로 밖이다. 새 key의 scope를 바꾸려면 어차피 발급이 필요하다 |
| D 이름 수정 허용(PATCH) | 편하다 | 감사 대조가 흐려진다. 나중에 필요하면 추가한다 |

## 결과

- 이점: 고객이 수집·CI key를 스스로 발급·교체·폐기한다. 정지 조직의 key가 막힌다(D04 §02).
- 비용 / 운영 부담
  - key가 1년이면 만료된다. 만료 전 알림(경보·화면)은 아직 없다.
  - 마지막 사용 시각(`last_used_at`)도 없다. 인증 경로에 쓰기를 더하지 않았다. 교체 판단은 겹침 기간 동안의 수집 지표로 한다.
- 영향 받는 계약
  - OpenAPI: 3개 operation, `ApiKey` schema.
  - migration 00017: `api_keys.name`, UPDATE 권한은 그대로.
  - 감사·outbox: `key.created`(details에 kind·scope·environment·만료), `key.revoked`.
  - 지표는 그대로다.
- tenant 영향
  - 발급은 principal의 tenant로만 한다(`ValidateKeyIssuance`, 저장소가 principal과 issuance를 대조).
  - 폐기·목록은 `WithTenant` + tenant predicate다.
  - 격리 suite `TestKeysAcrossTenants`가 위조 X-Tenant-ID로 남의 key를 보거나 폐기하지 못함, 정지 조직 key 401을 확인한다.
- retention 영향: key 행은 폐기·만료 뒤에도 남는다(감사 대조). 조직 삭제 job에서 함께 지운다.

## 기능 상태

- F07: key 관리 API는 이 ADR이다. 설정 > Access 화면은 다음 PR이다.
- 조직 정지 때 key 막기도 마쳤다.

## Rollback

- `MONTRACER_KEY_API_ENABLED=false` 또는 이전 이미지로 되돌린다. 이미 발급한 key는 그대로 쓰인다.
- **정지 조직 key 거절은 flag로 끄지 않는다.** 보안 계약(D04 §02)이다. 되돌리려면 이미지 rollback이다.
- migration 00017 down은 이름 열을 지운다. 이미지를 먼저 되돌린다(새 코드는 name을 읽는다).

## 재검토 조건

- 만료 전 알림(30·7일, 화면과 경보)이 필요할 때 — 1년 수명이 운영에서 문제가 되면 먼저 한다.
- 마지막 사용 시각이 교체 판단에 필요할 때(인증 경로의 비동기 기록).
- 조직당 100개가 모자랄 때(대규모 조직의 서비스별 ingest key).
- SCIM·service account(D04 §01 "service account에는 사람의 관리자 role을 사용하지 않는다")가 들어오면 API key의 발급자 결속을 다시 본다.

## 근거 (빅테크 사례·표준)

- **GitHub fine-grained personal access token**
  - 만료가 필수다(최대 1년, 조직이 더 짧게 강제 가능). token은 만든 직후 한 번만 보인다. 권한을 골라 준다.
  - 근거: GitHub Docs "Managing your personal access tokens".
- **GitLab personal·project access token**
  - 만료 필수(GitLab 16.0부터), 기본 최대 365일이다. 폐기는 즉시다.
  - 근거: GitLab Docs "Personal access tokens".
- **AWS IAM access key 교체**
  - "새 key 만들기 → 애플리케이션 갱신 → 이전 key 비활성화 → 삭제"로 겹쳐서 교체한다. secret은 만들 때 한 번만 볼 수 있다.
  - 근거: AWS IAM User Guide "Rotate access keys".
- **GitHub token 접두어(ghp_, gho_ …)**
  - 종류마다 다른 접두어를 두어 secret scanning이 종류를 구분하고 오탐을 줄인다.
  - 근거: GitHub Blog "Behind GitHub's new authentication token formats", 2021.
  - 초대 token 접두어를 분리한 근거다.
- **D04 §02**: tenant 폐쇄 시 ingest key 무효화, 교체 최대 24시간 겹침, 원문 1회.

## 증거

- `internal/controlapi/keys_test.go` `TestKeysAPI`
  - 조회 권한, step-up 없는 발급 403
  - 원문 1회와 재생에 없음, 목록에 원문 없음, ingest 접두어
  - 검증 11가지, Idempotency-Key 필수, 상한 409
  - 폐기 grace·즉시·범위 밖·잘못된 id·없는 key·CSRF, flag off
- `internal/controldb/keys_api_integration_test.go` `TestKeyAPIStore`
  - 이름, 멱등, 다른 조직 principal 거절·폐기 404
  - 정지·폐쇄·삭제 조직 key 미인증(restricted는 인증)
  - 동시 발급 상한, 폐기 뒤 자리, 이름 UPDATE 불가
- `internal/authz/keyscopes_test.go`: 발급 가능 scope 목록이 발급 검증과 맞음(step-up action 없음).
- `tests/isolation` `TestKeysAcrossTenants`.
- smoke 5c: step-up한 관리자가 조회 key 발급 → 그 key로 query-api 200 → 폐기 → 401.
