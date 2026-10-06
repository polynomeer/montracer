# ADR 0033: 운영자 break-glass key 조회·폐기

- 상태: 승인 (결정 위임)
- Owner: Security lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021~0032와 같은 위임). 근거는 §6
- 날짜: 2026-10-06 (제안·결정)
- 관련: D04 §01(관리 접근과 감사), §02(키 관리), §11 RB03 · ADR 0015, 0016

## 배경

D04 §11 RB03은 PII 유출이나 권한 침해가 의심되면 "해당 ingest key와 접근을 차단"하라고 한다. 그런데 폐기 경로가 하나도 없다.

- `KeyStore.RevokeKey`는 tenant 관리자(`keys.manage` + MFA step-up)만 부를 수 있다.
- 그 관리자가 쓸 control-api가 아직 없다.
- 운영자가 DB에서 `revoked_at`을 직접 고치면 감사와 outbox가 남지 않는다. 폐기를 증명할 수 없고, 인증 cache 무효화 event도 나가지 않는다.

D04 §01은 운영자의 cross-tenant 접근을 기본 금지하고, break-glass에 다음을 요구한다.

- 사유, 승인자, 만료 30분, 작업 scope
- 모든 조회도 감사
- 고객에게 지원 접근 이력 제공

## 결정

### 1. 권한: `authz.BreakGlassGrant` (일반 principal과 분리)

- 고객 role·key scope(`authz.Action`)와 **다른 타입**이다. 그래서 일반 `Authorize` 경로나 API middleware에 섞여 들어갈 수 없다.
- 허용 작업은 allowlist 두 개뿐이다. telemetry 조회는 break-glass로도 열지 않는다.
  - `break_glass.keys.list`: key metadata 목록. hash와 원문은 없다.
  - `break_glass.keys.revoke`: 즉시 폐기.
- 생성 조건
  - 대상 tenant 하나
  - 운영자 ≠ 승인자 (대소문자 무시, 본인 승인 금지)
  - ticket 필수
  - 사유 10~500자, 제어 문자 금지
  - 유효 기간 (0, 30분]
- **이 타입은 기록과 범위만 강제한다.** 신원 확인은 bastion/session manager가 맡는다(D04 §02).
  - CLI는 운영자 ID를 session이 넣은 `MONTRACER_OPERATOR_ID`에서만 읽는다. 인자로는 받지 않고, env가 없으면 실행하지 않는다.
  - **승인자는 검증하지 않은 입력이다.** 운영자 ≠ 승인자 검사는 본인 승인 실수를 막을 뿐, 2인 통제를 강제하지 못한다. 대조는 사후에 한다(§4, RB03).
  - **30분은 grant 객체의 수명 상한이지 승인의 수명이 아니다.** CLI는 실행마다 grant를 새로 만들므로, 승인 유효 기간은 ticket·session 기록으로 관리한다.
- **서비스 코드는 grant를 만들 수 없다(시험으로 고정).** `NewBreakGlassGrant`와 `KeyStore.BreakGlass*` 호출은 `cmd/montracer-admin`에만 있어야 한다. 다른 서비스가 부르면 단위 시험이 실패한다.

### 2. 실행: `controldb.KeyStore.BreakGlassListKeys` / `BreakGlassRevokeKey`

- **tenant 범위로만 접근한다.** `WithTenant`(RLS)를 거치고 앱 role(`montracer_rw`)을 쓴다. 운영자에게 cross-tenant 경로나 BYPASSRLS 계정을 따로 열지 않는다.
- **감사는 같은 트랜잭션에 쓴다.** 감사 쓰기가 실패하면 조회 결과를 돌려주지 않고 폐기도 적용하지 않는다(시험으로 고정).
- 감사는 대상 tenant의 `audit_events`에 security 범주, `actor_kind='operator'`(migration postgres 00004)로 남는다.
  - D04 §01 "고객에게 지원 접근 이력 제공": 고객은 `audit.read` key로 `GET /api/v1/audit-events?category=security`에서 본다(ADR 0034). UI는 아직 없다.
- key ID는 발급 형식(16자리 소문자 hex)만 받는다. 형식 밖 값은 DB와 감사에 닿기 전에 거절한다.

| 시도 | 감사 action | outbox |
|---|---|---|
| 목록 조회 | `break_glass.keys.listed` (`keys_returned` 수) | 없음 |
| 폐기 적용 | `key.revoked` (tenant 관리자 폐기와 같은 action) | `key.revoked`, payload는 `revoke_at`과 `actor_kind: operator`만 |
| 이미 폐기됨 | `break_glass.key.already_revoked` | 없음 |
| tenant에 없는 key(다른 tenant의 key 포함) | `break_glass.key.not_found`, resource_id는 `unknown`. 다른 tenant의 key ID가 이 tenant 고객에게 보이지 않게 한다(D04 §01). 호출자에게는 not found | 없음 |

- **사유·승인자·ticket은 감사 details에만 두고 outbox에는 싣지 않는다.** outbox는 하류 consumer로 퍼지는 event다. 그래서 폐기 event에는 폐기 사실, 시각, actor_kind만 담는다. actor_id만으로는 운영자와 tenant 사용자를 구분할 수 없어 actor_kind를 넣는다.
- 만료·범위 밖 grant, 잘못된 인자는 DB에 닿기 전에 거절하므로 감사 행이 없다. 이런 거절 시도는 CLI 구조화 로그(stderr)에 `break-glass rejected`로 남는다. 이 로그에 사유 원문은 쓰지 않는다.

### 3. 도구: `cmd/montracer-admin`

```
montracer-admin keys list   --tenant UUID --approver ID --ticket ID --reason TEXT
montracer-admin keys revoke --tenant UUID --key-id ID --approver ID --ticket ID --reason TEXT [--yes]
```

- 실행 한 번에 grant 하나(작업 하나)를 쓴다.
- revoke는 `--yes` 없이는 무엇을 할지만 출력하고 끝낸다.
- 인자 검증이 모두 끝난 뒤에만 DB에 연결한다.
- 폐기 즉시 인증이 거절된다. 지금 ingress·query-api는 인증마다 제어 DB를 직접 조회한다(`LookupKey`, 인증 cache 없음).

### 4. 하지 않는 것

- **telemetry·감사 본문 break-glass 조회:** D04 §01은 가능성을 열어 두지만, 그 접근을 감사할 query-api 경로가 아직 없다. 생기면 같은 grant 타입에 action을 더한다(별도 ADR).
- **고객 승인 단계:** Google Access Approval, Microsoft Customer Lockbox 같은 고객 승인 단계는 두지 않는다. 사고 대응에서 노출된 key를 막는 일은 고객 승인을 기다릴 수 없기 때문이다. 대신 사후에 고객이 감사로 확인한다.
- **승인 workflow 자동화:** 승인 요청·승인 기록을 받는 서버는 없다. 승인 사실은 ticket과 session manager 기록에 의존한다. 감사의 approver는 운영자가 입력한 값이므로, RB03 사후 기록에서 ticket의 승인 기록과 대조한다.
- **운영자 신원 위조 방지의 한계:** 신원은 session이 넣은 env를 믿는다. 셸에서 env를 바꾸거나 앱 DSN(`montracer_rw`)으로 직접 `UPDATE api_keys`를 하면 이 통제는 우회된다(감사 없음). 앱 DSN은 secret manager와 session 기록으로 통제한다. 서버 측 break-glass API(SSO 신원·승인 서명)가 생기면 다시 본다.
- **고객 감사 조회 UI:** ~~감사 행은 남지만 고객이 읽을 경로가 없다.~~ API는 ADR 0034(`GET /api/v1/audit-events`, `audit.read`)로 생겼다. UI는 남았다.
- **인증 cache·outbox dispatcher:** 아직 없다. 생기면 `key.revoked` event로 cache를 무효화해야 한다(D04 §02 전파 60초). 이 ADR이 `key.revoked`를 재사용하는 이유가 그것이다.

### 5. 운영 절차

RB03 "공통 대응 2. 차단"에 넣는다. 이 도구가 없을 때 있던 공백("key 폐기 경로 없음")을 지운다.

### 6. 외부 사례 근거 (2026-10-06 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| 사유(justification)·ticket 기록, 고객이 보는 접근 기록 | Google Cloud Access Transparency ([개요](https://cloud.google.com/security/products/access-transparency)), Access Approval ([승인](https://cloud.google.com/cloud-provider-access-management/access-approval/docs/approve-requests)) | 지원 인력 접근마다 business justification과 지원 ticket을 기록한다. 고객은 그 기록을 본다. 승인된 접근은 승인 만료나 case 종료 때 끝난다 | 채택: 사유·ticket 필수, 대상 tenant 감사에 기록(`audit.read`) |
| 시간 제한, 자동 만료 | Microsoft Purview Customer Lockbox ([문서](https://learn.microsoft.com/purview/customer-lockbox-requests)) | 엔지니어 접근은 요청한 기간만 허용되고 지나면 자동 회수된다 | 채택: grant 유효 30분(D04 §01). 고객 승인 단계는 §4대로 두지 않는다 |
| 성공·실패 시도 모두 감사, 승인자 | AWS Well-Architected SEC03-BP03 ([emergency access](https://docs.aws.amazon.com/wellarchitected/latest/framework/sec_permissions_emergency_process.html)) | 긴급 접근의 성공·실패 시도를 모두 상세 감사하고 incident와 연결한다. 승인 절차와 대리 승인자를 둔다 | 채택: 없는 key·이미 폐기된 key 시도도 감사, ticket으로 incident와 연결, 승인자 ≠ 운영자 |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 권한 표현 | 별도 grant 타입 | 운영자용 Principal kind: 일반 `Authorize`와 API 경로에 섞일 위험이 있다. 실수 한 번으로 cross-tenant 권한이 된다 |
| DB 접근 | 앱 role + RLS | 관리자(BYPASSRLS) 계정: 범위 실수가 다른 tenant로 번진다. D04 §01 "cross-tenant 기본 금지"에 어긋난다 |
| 감사 위치 | 대상 tenant의 audit_events | 별도 운영자 감사 테이블: 고객이 지원 접근 이력을 볼 수 없다(D04 §01) |
| 폐기 감사 action | `key.revoked` 재사용 | 별도 action: 앞으로 만들 인증 cache 무효화 consumer(§4)가 두 event를 모두 알아야 한다 |

## 결과

- RB03의 key 차단 단계를 실행할 수 있다. 폐기가 tenant 관리자 폐기와 같은 event로 하류에 전달된다.
- 운영자 접근이 tenant 감사에 남는다. 고객 조회 API가 생기면 고객에게 보이므로, 사유 문구는 고객이 읽는다는 전제로 쓴다.

## Rollback

- `cmd/montracer-admin` 배포를 거둔다.
- migration 00004 down은 actor_kind 제약을 원래 값으로 되돌린다. 기존 운영자 감사 행은 지우지 않고 `NOT VALID`로 남긴다. 감사는 append-only다.

## 증거

- `internal/authz`
  - 본인 승인, 30분 초과, 빈 ticket·짧은 사유·제어 문자, 알 수 없는 action, 범위 밖 action, 만료를 거절한다.
  - key ID 형식 검사, break-glass 사용처를 운영자 도구로 한정(아키텍처 시험).
- `internal/controldb` 통합 테스트(PostgreSQL)
  - 폐기하면 즉시 인증이 거절된다. 감사(operator, ticket·승인자·사유)가 남고, outbox에는 `revoke_at`만 있다.
  - 재폐기는 no-op로 감사되고 outbox는 추가되지 않는다.
  - 다른 tenant의 key ID는 not found다. 그 key는 그대로이고, 감사는 grant tenant에만 `unknown`으로 남는다(key ID 미기록).
  - 형식 밖 key ID는 감사에 닿지 않는다. outbox payload에 `actor_kind: operator`가 있다.
  - 목록은 그 tenant의 key만 보이고 조회가 감사된다.
  - 범위 밖·만료 grant는 감사 없이 거절된다.
  - 감사 실패 시 폐기가 롤백된다.
- `cmd/montracer-admin`
  - `--yes` 없이는 DB에 연결하지 않는다.
  - `--operator` 인자가 없고, session env가 없으면 실행하지 않는다.
  - 잘못된 인자는 연결 전에 거절하고 거절을 로그에 남긴다.
  - 로그에 사유 원문이 없다.
- migration 00004는 `NOT VALID` 후 `VALIDATE`로 제약을 바꾼다(검증 중 강한 잠금을 짧게).
- spec-reviewer 지적 반영: 운영자 인자 제거, 승인자·30분 의미와 위조 한계 명시, 없는 cache·consumer·고객 조회 API를 공백으로 정정, not_found 감사의 key ID 제거, 사용처 시험, 거절 로그, outbox actor_kind, migration 잠금
