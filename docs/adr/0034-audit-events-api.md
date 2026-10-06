# ADR 0034: 감사 조회 API와 서명 page cursor

- 상태: 승인 (결정 위임)
- Owner: Backend lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021~0033과 같은 위임). 근거는 §6
- 날짜: 2026-10-06 (제안·결정)
- 관련: D02 §12(공통 API·cursor), §14(`GET /audit-events`), D04 §01(지원 접근 이력 제공) · ADR 0014, 0015 §4, 0016, 0033

## 배경

- 감사 행(`audit_events`)은 key 발급·폐기와 운영자 break-glass(ADR 0033)에서 쌓이고 있지만, 고객이 읽을 경로가 없다. D04 §01 "고객에게 지원 접근 이력을 제공"이 ADR 0033 §4의 공백으로 남아 있다.
- D02 §14는 `GET /audit-events`를 요구한다(접근 scope 분리, cursor).
- D02 §12는 cursor를 "tenant·권한 fingerprint·query hash·snapshot 시각·만료를 포함한 서명 토큰"으로 정하고, offset pagination을 기본으로 쓰지 않게 한다.
- D02 §19는 공통 성공 응답을 `data, meta, next_cursor`로, cursor를 "만료 15분 HMAC cursor"로 정한다.
- control-api는 진입점도 없다(`cmd/control-api`에는 README만 있다).

## 결정

### 1. Endpoint: `GET /api/v1/audit-events` (`cmd/control-api`, `internal/controlapi`)

| 인자 | 규칙 |
|---|---|
| `from`, `to` | 필수, RFC3339. `[from, to)`, 범위 366일 이하(보안 audit 보존 365일 제안 D04 §04 + 경계 하루) |
| `category` | 선택, `operations` \| `security` |
| `action` | 선택, 정확히 일치, 128자 이하, 공백·제어 문자 없음 |
| `limit` | 기본 100, 1~1000. 범위 밖이면 400(D02 §12의 최대값) |
| `cursor` | 선택, 직전 응답의 `meta.next_cursor` |

- 응답은 D02 §19 공통 형태 `{data, next_cursor, meta}`다.
  - `data[]`: `id, occurred_at, category, action, actor_kind, actor_id, resource_type, resource_id, request_id(null 가능), details(객체)`
  - `next_cursor`: 최상위, 없으면 null
  - `meta`: `request_id, schema_version`
- 정렬은 `(occurred_at, id)` 내림차순, keyset pagination이다. 저장소 쿼리는 tenant·시간·범주를 항상 predicate로 넣는다(계약 4). RLS(ADR 0016)가 tenant를 한 번 더 막는다.
  - 정렬과 위치 비교는 모두 uuid `id` 기준이다. 응답용 text 변환 컬럼은 별칭(`id_text`)으로 내야 한다. 같은 이름이면 `ORDER BY id`가 text 출력으로 해석되어, collation에 따라 같은 시각 행의 page 경계가 어긋난다(리뷰에서 발견).
  - index `audit_events_keyset (tenant_id, occurred_at DESC, id DESC)`(migration postgres 00005)가 이 순서를 받친다. 기존 `audit_events_time`은 이 index가 덮으므로 지운다.
- `Cache-Control: no-store`. 감사는 보안 정보라 중간 cache에 남기지 않는다.
- 인증은 API key(`audit.read` 또는 `audit.operations.read` scope)다. 사람 session(OIDC)은 아직 없다.

### 2. 범주 권한 (ADR 0015 §4)

| principal 권한 | `category` 없음 | `category=operations` | `category=security` |
|---|---|---|---|
| `audit.read` (Tenant Admin, Security Auditor) | 두 범주 | operations | security |
| `audit.operations.read`만 (Operator) | **operations만** | operations | **403** |
| 둘 다 없음 | 403 | 403 | 403 |

- 범주를 지정하지 않으면 볼 수 있는 범주만 돌려준다(ADR 0015 §4 "운영 범주만 필터").
- **명시적으로 security를 요청했는데 권한이 없으면 403이다.** 조용히 빈 결과를 주면 "보안 이벤트가 없다"로 오해할 수 있다(계약 6: 빈 결과와 권한 없음을 구분).
- 운영자 break-glass 행(`actor_kind=operator`)은 security 범주다. 그래서 `audit.read`만 본다.
- **고객 응답에서 직원 개인 식별자는 가린다(리뷰에서 발견).**
  - `actor_id`는 `montracer-operator`로 바꾸고, `details.break_glass.approver`는 뺀다.
  - 사유·ticket·발급·만료 시각·폐기 시각은 그대로 보인다.
  - 원본은 제어 DB에 남아 내부 조사(RB03 사후 대조)에 쓴다.
  - 사유는 자유 입력이므로 고객이 읽는다는 전제로 쓴다(ADR 0033).

### 3. 서명 cursor (`internal/apicursor`)

```
c1.<base64url(JSON {t, f, q, s, e, p})>.<base64url(HMAC-SHA256)>
```

| 필드 | 내용 |
|---|---|
| `t` tenant | principal tenant |
| `f` 권한 fingerprint | `hash(kind, subject, 볼 수 있는 범주)`. 강등되면 이전 cursor는 거절된다 |
| `q` query hash | `hash(from, to, category, action)`. **page 크기는 넣지 않는다**(page마다 바꿀 수 있다) |
| `s` snapshot | 첫 page 요청 시각. 이후 page는 `min(to, snapshot)`까지만 읽는다 |
| `e` 만료 | 발급 + 15분 (D02 §19) |
| `p` 위치 | 마지막 행의 `(occurred_at, id)`. 응답에 이미 있는 값만 담는다 |

- 서명 key는 `MONTRACER_CURSOR_KEY_HEX`(32 byte 이상, secret manager에서 주입)다. 서명만 하고 암호화하지 않는다.
- 위조·손상·만료·다른 tenant·권한·query는 모두 400 `INVALID_ARGUMENT`(field `cursor`)다. 어느 검사에서 실패했는지 구분하지 않는다. 그 차이가 다른 tenant·권한의 단서가 되지 않게 한다.
- key를 바꾸면 발급된 cursor가 모두 무효가 된다. 클라이언트는 첫 page부터 다시 읽는다.
- 이 패키지는 이후 다른 목록 API(keys, monitors 등)에도 쓴다.

### 4. 하지 않는 것

- **사람 session(OIDC)·UI:** 인증은 API key뿐이다. 감사 화면은 UI(F05) 작업에서 만든다.
- **조회 자체의 감사:** 감사 조회를 다시 감사하지는 않는다. D04 §01 "모든 조회도 감사"는 운영자 break-glass에 대한 요구다. 고객 자신의 감사 조회는 데이터 조회 감사 기능과 함께 다시 본다.
- **rate limit(D02 §12 사용자 60 req/min):** 관리 API 공통으로 나중에 만든다.
- **OpenAPI 원천:** 조회 API와 함께 `api/openapi`로 옮긴다.
- **snapshot의 한계:** 다음 두 경우 가장 최근 행이 그 page 묶음에서 빠질 수 있다. 둘 다 다음 조회에서는 보인다.
  - **늦게 커밋되는 트랜잭션:** `occurred_at`은 트랜잭션 시작 시각(`now()`)이다. 첫 page 직전에 시작해 뒤늦게 커밋된 변경이 해당한다.
  - **시계 차이:** snapshot은 앱 시계로 잡는데 `occurred_at`은 DB 시계다. DB 시계가 앱보다 앞서면 빠진다.

### 5. 배치

- `cmd/control-api`는 이 API의 첫 진입점이다. D02 §03은 M0에서 control-api와 query-api를 같은 binary로 돌려도 된다고 하지만, 여기서는 별도 binary로 둔다. 제어 DB만 쓰고, 조회 저장소(ClickHouse)와 실패 영역이 다르기 때문이다.
- 지표는 `montracer_control_requests_total`, `montracer_control_request_duration_seconds`(route, status_class)다.
- 경보는 `MontracerControlErrorRateHigh`(route별 5xx > 1% 10분, warning)이고, 대응은 RB02 해당 절이다.

### 6. 외부 사례 근거 (2026-10-06 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| 불투명 page token, 다른 인자로 재사용하면 거절, page 크기는 변경 가능, 만료 | Google AIP-158 Pagination ([aip.dev/158](https://google.aip.dev/158)) | page token은 불투명해야 하고, 다른 인자가 바뀌면 `INVALID_ARGUMENT`다. page_size는 바꿀 수 있다. 만료할 수 있고 3일을 기본으로 제안한다 | 채택: query hash에서 limit 제외, 불일치는 400. 다르게: 만료는 D02 §19의 15분이다(권위 문서). 최대 초과 page 크기는 coerce하지 않고 400이다(D02 §12 최대값을 명시적 오류로) |
| 감사 범주별 열람 권한 분리 | Google Cloud Audit Logs ([개요](https://cloud.google.com/logging/docs/audit)) | `roles/logging.viewer`는 Admin Activity 감사를 읽는다. Data Access 감사는 `roles/logging.privateLogViewer`가 따로 필요하다 | 채택: operations는 `audit.operations.read`, security는 `audit.read`(ADR 0015 §4) |
| 지원 인력 접근을 고객 감사에서 확인, 직원 신원은 비공개 | Google Access Transparency ([개요](https://cloud.google.com/security/products/access-transparency), ADR 0033 §6) | 지원 인력 접근 기록과 사유(justification)를 고객이 본다. 개별 직원 신원 대신 접근자 유형·위치 같은 속성을 보여 준다 | 채택: break-glass 행을 security 범주로 노출하되 운영자·승인자 ID는 가리고 사유·ticket은 보인다 |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| pagination | keyset `(occurred_at, id)` | offset: D02 §12 금지, 새 행이 끼면 중복·누락이 생긴다 |
| cursor 무결성 | HMAC 서명 | 서버 저장 cursor: 저장소와 만료 청소가 필요하다. 평문 위치: 다른 query·권한에 재사용된다 |
| 권한 없는 범주 요청 | 403 | 빈 결과: "없음"과 "못 봄"이 섞인다 |
| 배치 | 별도 binary | query-api에 합침: ClickHouse 장애가 감사 조회까지 내린다 |

## Rollback

- control-api 배포를 거둔다. 데이터 변경은 없다(읽기 전용).
- 서명 key를 바꾸면 발급된 cursor가 모두 무효가 된다.

## 증거

- `internal/apicursor`
  - 왕복, 다른 key·변조·다른 tenant·권한·query, 접두어 없음, 길이 초과, 만료를 거절한다.
  - fingerprint 경계 모호성이 없다.
- `internal/authz`: 범주 권한(Admin·Auditor 두 범주, Operator는 operations, Viewer·Developer는 403).
- `internal/controlapi` (fake 저장소)
  - role별 범주, Operator의 security 요청은 403, 인증 없음은 401
  - cursor로 전체 page 순회, snapshot 고정
  - 다른 권한·query·변조 cursor는 400, limit 변경은 허용
  - 입력 검증은 저장소 호출 전에 한다
  - 응답 형식(최상위 next_cursor, details 객체, null), `no-store`, 저장소 장애는 503
  - 운영자 행의 운영자·승인자 ID를 가리고 사유·ticket은 남긴다
- `internal/controldb` 통합 테스트(PostgreSQL)
  - tenant 분리, 범주 제한, Operator의 security 요청 거절
  - keyset 두 page, action 필터와 `[from, to)` 경계, 권한 없는 role 거절
  - 같은 시각 행 6개를 limit 1로 끝까지 순회해도 누락·중복이 없다
- 경보 promtool 시험(`MontracerControlErrorRateHigh`)
- spec-reviewer 지적 반영: next_cursor 위치(D02 §19), cursor 만료 15분(D02 §19), ORDER BY 별칭 충돌과 keyset index, 운영자 신원 비공개와 근거 정정, 시계 차이 한계, 범위 상한 근거, 경보·runbook
