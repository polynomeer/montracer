# ADR 0015: API key 권한 수명, step-up 유효 시간, key 조회 권한, Operator 운영 감사

- 상태: 승인
- Owner: Security
- 승인자: polynomeer
- 날짜: 2026-10-04 (결정)
- 관련: F07, F08, E01 · D04 §01, §02, §12 · PR #2 리뷰의 미정 항목 2~5

## 배경

D04 §01~02와 §12는 다음을 요구한다.

- "API key는 발급자의 권한보다 강해질 수 없다"
- "관리자 key 발급과 데이터 삭제는 MFA step-up 대상"
- Operator는 "범위 내 운영 감사"를 볼 수 있다
- SCIM deprovision 후 60초 안에 접근을 차단한다

다만 아래 네 가지는 정해져 있지 않다.

1. 발급 뒤 발급자의 권한이 줄어들면 key는 어떻게 되는가
2. step-up은 얼마 동안 유효한가
3. key 목록 조회에도 step-up이 필요한가
4. "운영 감사"의 범위는 무엇인가

## 결정

### 1. API key는 사용할 때마다 발급자의 현재 권한으로 제한한다

- **API key**의 실효 권한은 `저장된 scope ∩ 발급자의 현재 role이 허용하는 action`이다. 저장소는 key를 조회할 때 발급자의 현재 membership role을 함께 돌려준다(`KeyRecord.IssuerRole`).
- 발급자가 조직에서 제거되었거나 deprovision되면(`IssuerRole`이 비어 있음) key 인증을 거절한다. 그러면 SCIM deprovision의 60초 차단(D04 §12)이 별도 revoke job 없이 인증 cache의 전파 한도 안에서 key에도 적용된다.
- **ingest key**는 사람이 아니라 조직이 소유한다. 발급자의 권한 변화와 무관하다. 담당자가 퇴사했다고 production 수집이 끊기면 안 되기 때문이다. ingest key는 명시적 revoke와 만료로만 관리한다.

### 2. step-up 유효 시간은 15분

- 사람 principal은 마지막 MFA step-up 시각(`stepUpAt`)을 받는다. 다음 조건을 모두 만족할 때만 step-up 상태로 본다.
  - `0 ≤ now − stepUpAt ≤ 15분`
  - 미래 시각이 아님 (clock 오류 방어)
- 15분은 설계 가정이다. 세션의 idle 30분보다 짧게 잡아, 민감 작업은 최근 인증을 다시 요구하게 한다.

### 3. key 조회와 관리를 나눈다

| action | 대상 | 허용 role | step-up |
|---|---|---|---|
| `keys.read` | `GET /keys`, key metadata 조회 (원문·hash 없음) | Tenant Admin | 불필요 |
| `keys.manage` | `POST /keys`, `DELETE /keys/{id}` | Tenant Admin | 필요 |

### 4. Operator의 "범위 내 운영 감사" = `audit.operations.read`

운영 감사는 조직 운영 설정의 변경 이력만 포함한다.

| 구분 | 감사 이벤트 범주 |
|---|---|
| 포함 | monitor, silence, dashboard, 배포 이벤트, 수집 정책 제안·적용, notification 정책 |
| 제외 (보안 감사, `audit.read`만) | 인증·로그인, 멤버·role 변경, key 발급·폐기, 삭제 job, 데이터 조회 감사, break-glass, export |

| action | 허용 role |
|---|---|
| `audit.operations.read` | Operator, Tenant Admin, Security Auditor |
| `audit.read` (전체) | Tenant Admin, Security Auditor |

- 감사 이벤트의 범주는 audit 기능을 구현할 때 이벤트 type에 고정한다.
- query 계층은 `audit.read`가 없는 principal에게 운영 범주만 필터해서 보여준다.

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| key 권한 수명 | 사용 시점 교집합 | 강등 시 key 일괄 revoke: 이벤트 누락에 취약하고, 강등을 되돌려도 key가 복구되지 않음 |
| step-up | 15분 window | 세션 전체 유지: 탈취된 세션으로 민감 작업이 가능해짐 |
| key 조회 | 분리 | 조회에도 step-up: 일상 점검을 방해함 |
| 운영 감사 | 범주 한정 action | 미부여: D04 §01 표와 어긋남 |

## 결과

- 저장소의 key 조회는 발급자의 현재 role을 함께 반환해야 한다(JOIN `memberships`).
- 발급자 role이 바뀌면 인증 cache(최대 60초) 안에서 반영된다.
- audit 이벤트 schema에 범주(`operations` | `security`)를 둔다.

## Rollback

- 교집합 규칙을 끄면 발급 시점 검사로 돌아간다. 이 경우 강등 시 revoke job이 필요하다.
- step-up window는 설정값으로 바꿀 수 있다.

## 재검토 조건

- G1 team·environment scope(F08)를 도입할 때 교집합에 scope도 포함한다.
- 고객이 서비스 계정 key의 발급자 독립을 요구할 때. 이때는 service account principal을 도입한다.

## 증거

- `internal/authz` 테스트:
  - 강등된 발급자의 key는 실효 권한이 줄어든다
  - 발급자가 제거되면 key 인증을 거절한다
  - ingest key는 발급자와 무관하다
  - step-up은 15분 경계와 미래 시각에서 거절된다
  - `keys.read`와 `keys.manage`를 분리한다
  - Operator는 `audit.operations.read`만 가진다
