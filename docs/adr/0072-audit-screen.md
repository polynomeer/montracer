# ADR 0072: 설정 > Audit 화면 — 감사 이벤트 조회 (G1 운영 준비 "감사 조회 UI")

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-22 "디자인 시안 모두 적용해줘. 작업계획이 필요하다면 미리 작성하고 그에 맞춰서 진행해줘" — [design-rollout](../plan/design-rollout.md) D4-1)
- Owner: Frontend lead
- 날짜: 2026-10-25 (제안·결정)
- 관련: E07 운영 준비("감사 조회 UI", [작업계획서](../plan/work-plan.md) G1) · D02 §14·§19 · D04 §01(운영자 접근 이력·범위 내 운영 감사), §04(보안 감사 보존 365일 제안), §12 · D05 §01(Admin), §03(상태 계약), §12 · ADR 0015 §4(범주 권한), 0033(break-glass), 0034(감사 조회 API), 0041(주 메뉴), 0064(session)

## 배경

ADR 0034로 `GET /api/v1/audit-events`가 생겼지만 화면이 없었다. D05 Admin 영역의 Audit(`settings/audit`)는 placeholder였다. 그 사이 사람 session(ADR 0064)이 생겨 control-api가 session cookie로도 이 API를 받는다(`internal/controlapi/audit.go` `authenticated`). 디자인 시안에는 Audit 화면이 없다. 정할 것은 다음이다.

- 시간 창과 이어 받기(cursor 15분 만료, 첫 page snapshot)를 화면에서 어떻게 다루는가
- 권한 차이(Operator는 운영 범주만)와 빈 결과를 어떻게 구별해 보이는가
- `details`(임의 JSON)를 어떻게 보이는가
- 메뉴를 누구에게 보이는가

## 결정

### 1. 위치와 접근

- 경로는 `/o/{org}/settings/audit`, Admin 그룹 마지막(D05 §01, ADR 0041 순서).
- 메뉴는 지금처럼 Tenant Admin·Security Auditor에게만 보인다(Admin 그룹 단위 표시). Operator는 운영 범주를 볼 권한이 있지만(ADR 0015 §4) 메뉴에는 없고 주소로 들어온다.
  - 이유: D05 §01은 Admin을 "별도 관리 권한" 그룹으로 둔다. 항목 하나만 다른 역할에 보이려면 메뉴 표시를 그룹 단위에서 항목 단위로 바꿔야 하고, 그것은 capabilities API(역할별 화면 권한)가 생길 때 함께 한다. 메뉴 숨김은 보안 수단이 아니다(D04 §12). 서버가 요청마다 범주 권한을 다시 검사한다.
  - 화면의 역할 표시("지금 권한으로는 운영 범주만")는 session 역할로 만든 안내일 뿐이다. 무엇이 보이는지는 서버가 정한다.

### 2. 조건과 시간 창

- 시간 범위는 상단 조사 context를 쓴다. 366일을 넘으면 조회하지 않는다(ADR 0034 §1).
- 범위에 365일보다 오래된 구간이 있으면 보안 감사 보존 기간(D04 §04 제안 365일)을 안내한다. 빈 결과가 "없었음"이 아닐 수 있다.
- 조건은 범주(전체·운영·보안)와 action(정확히 일치)이다. URL(`category`, `action`)에 남아 공유된다. 모르는 범주 값은 조건 없음으로 본다. 틀린 action(공백·제어 문자, 128 byte 초과)은 URL로 와도 서버에 보내지 않고 입력칸 아래에 이유를 보인다.
- 주체·대상 조건은 API가 아직 받지 않아 넣지 않는다.
- **창 고정:** 첫 page를 부를 때마다(범위·조건·새로고침) 지금 시각으로 창을 다시 고정한다. "더 보기"로 이어 받는 동안만 창을 그대로 둔다 — cursor는 query hash(from·to·범주·action)에 묶여 있다(ADR 0034 §3).
- 화면의 기준 시각은 응답 시각이 아니라 창의 끝("…까지의 기록")이다. 서버는 첫 page 시각을 snapshot으로 고정하므로 그 뒤 기록은 새로고침해야 보인다. ADR 0034 §4의 한계(늦게 커밋된 트랜잭션, 시계 차이)도 다음 조회에서 보인다.

### 3. 이어 받기

- 한 page는 100개, "100개 더 보기"로 아래에 붙인다. 번호 page는 keyset cursor와 맞지 않고, 무한 스크롤은 키보드 사용자가 끝(바닥글)에 닿지 못한다.
- cursor는 컴포넌트 상태에만 둔다. URL에 넣지 않는다(D05 §01, 서명돼 있어도 15분 뒤 무효).
- cursor가 거절되면(400, field `cursor`: 만료·권한 변경·조건 불일치) 이미 받은 행은 남기고 "처음부터 다시 불러오기"를 보인다. 어느 이유인지는 서버가 구분하지 않으므로 화면도 짐작하지 않는다.
- 이어 받는 중 403(강등 등, 서버는 cursor 검사 전에 범주 권한을 본다)은 감사 권한 문구로 말하고 앞의 행은 남긴다.
- 쌓을 수 있는 행 수에는 아직 상한·가상화가 없다. 10,000행(D05 §12)에 가까워지는 사용이 보이면 가상화를 붙인다(재검토 조건).

### 4. 상태(D05 §03)

| 상황 | 보이는 것 |
|---|---|
| 처음 불러오는 중 | skeleton |
| 403, 보안 범주를 골랐을 때 | "보안 범주를 볼 권한이 없습니다" + "기록이 없다는 뜻이 아닙니다" + 운영 범주 안내 |
| 403, 그 밖 | "감사 기록을 볼 권한이 없습니다"(볼 수 있는 역할) |
| 404 | "감사 조회 API에 닿지 못했습니다" + request_id. 계약에는 404가 없다(경로가 늘 등록됨) — 배포 경로 설정 오류에 대비한 분기다. "꺼짐"이라고 하지 않는다 |
| 5xx·연결 오류 | 공용 오류 알림(request_id, 다시 시도) |
| 새로고침 중·실패 | 이전 행을 남기고 "새로 불러오는 중" / 오류 알림 + "마지막 성공 기준" |
| 0개, 조건 있음 | "이 기간·조건에 맞는 감사 기록이 없습니다" |
| 0개, 조건 없음 | "이 기간에 볼 수 있는 감사 기록이 없습니다" |
| 366일 초과 | 조회하지 않고 범위를 줄이라고 안내 |

### 5. 표시

- 열: 시각, 범주, action, 주체(종류·ID), 대상(종류·ID), 자세히(접힘).
- 범주는 상태가 아니므로 경보·오류 색을 쓰지 않는다(중립 chip).
- 범주·주체 종류 이름은 API 값과 1:1이다. 서버가 새 값을 더하면 빈칸이 아니라 원문을 보인다.
- `details`와 request_id는 `<details>` 안에 접어 두고 JSON을 글자 그대로 보인다(HTML 해석 없음, CLAUDE.md "text-only 렌더링").
- 화면은 따로 가리지 않는다. `details`에 PII·secret을 넣지 않는 것은 감사를 쓰는 쪽의 책임이다(D04 §04, ADR 0034). 운영자 행의 직원 ID와 승인자는 서버가 가린다(`montracer-operator`, `details.break_glass.approver` 제거).
- 운영자 break-glass 행(보안 범주)은 사유·ticket·만료를 보인다. D04 §01이 요구하는 고객용 지원 접근 이력을 이 화면이 처음 제공한다(ADR 0033 §4가 남긴 공백).

### 6. 하지 않는 것

- 감사 화면 조회 자체를 감사하지 않는다(ADR 0034 §4. 고객 조회 감사는 F08 "조회 감사"와 함께).
- CSV·export: 넣을 때 D05 §12의 CSV formula injection 검사를 함께 한다(F26 "감사 export").
- rate limit: 관리 API 공통 작업(ADR 0034 §4).

## 외부 사례 근거 (2026-10-25 확인)

| 사례 | 확인한 동작 | 이 결정에 쓴 부분 |
|---|---|---|
| GitHub 조직 audit log ([docs](https://docs.github.com/en/organizations/keeping-your-organization-secure/managing-security-settings-for-your-organization/reviewing-the-audit-log-for-your-organization)) | 조직 owner만 본다. 180일 보존, 기본은 최근 3개월이고 더 오래된 것은 `created` 범위로. 자유 검색 없이 `action:`(정확한 이벤트 또는 범주)·`actor:`·`created:` 조건 | 정확히 일치하는 action 조건, 기간을 먼저 고르는 조회, 보존 기간 안내 |
| Google Cloud Audit Logs ([docs](https://docs.cloud.google.com/logging/docs/audit)) | Admin Activity는 Logs Viewer, Data Access는 Private Logs Viewer — 종류마다 볼 권한이 다르다 | 범주별 권한을 화면이 숨기지 않고 403을 "기록 없음"과 구별(ADR 0015 §4·0034 §2와 같은 모델) |
| Datadog Audit Trail ([docs](https://docs.datadoghq.com/account_management/audit_trail/)) | Audit Trail Read 권한, 보존 기간 설정(최대 90일), 이벤트를 고르면 속성(JSON) 패널 | `details`를 원문 JSON으로 펼쳐 보기, 보존 기간 안내 |

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A (채택) "더 보기" + cursor는 상태에만 | keyset·snapshot과 맞음, 키보드로 끝까지 감, 공유 URL에 만료되는 값이 없음 | 행이 계속 쌓임(상한·가상화는 재검토) |
| B 번호 page | 익숙함 | keyset cursor로는 임의 page로 갈 수 없음. offset은 API에 없음 |
| C 무한 스크롤 | 손이 덜 감 | 바닥글·다음 영역에 닿기 어려움(D05 §12), 실패 위치가 안 보임 |
| D Operator에게 Audit 메뉴 노출 | 운영 범주를 찾기 쉬움 | 메뉴 표시 단위를 바꿔야 함 — capabilities API와 함께(§1) |

## 결과

- 이점: 고객이 설정 변경·key 폐기·운영자 접근을 직접 확인한다. G1 운영 준비 항목 "감사 조회 UI"가 채워진다.
- 비용: 행 상한이 없다. Operator는 주소로 들어와야 한다.
- 영향 받는 계약: 새 API·schema 없음. web dev proxy가 `/api/v1/audit-events`를 control-api로 보낸다(운영 배포의 경로 배치는 control-api의 다른 관리 API와 같다).

## Rollback

route와 dev proxy 항목을 되돌리면 placeholder로 돌아간다. 저장하는 것이 없어 되돌릴 수 없는 부분은 없다.

## 재검토 조건

- 한 번에 수천 행을 넘게 펼치는 사용이 보이면: 가상화 또는 쌓는 행 상한
- capabilities API가 생기면: Operator에게 Audit 메뉴(운영 범주) 보이기
- API가 주체·대상 조건이나 404(기능 끔)를 내면: 조건 추가, 404 문구
- 보안 감사 보존 기간이 확정되면(D04 §04 제안 365일): 안내 문구의 수치

## 증거

- `apps/web/src/features/audit/Audit.test.tsx`
  - 표(시간 역순, details 글자 그대로, 운영자 행), 한 page 100·7일 창·category 없음
  - 조건이 URL에 남고 서버에 감, 틀린 action은 보내지 않음(입력·URL 모두)
  - 더 보기 이어 붙이기, cursor 거절 → 앞 행 유지·처음부터, 더 보기 403·5xx
  - 403(보안 범주)·조건 없는/있는 빈 결과·5xx, Operator 기본 조회(category 없음)
  - 새로고침 실패 시 이전 행·마지막 성공 기준, 조건 변경 시 창 다시 고정
  - 365일 보존 안내, 366일 초과는 조회하지 않음, 모르는 범주·주체 종류는 원문
- `apps/web/dev-proxy.test.ts`: audit-events가 control-api로
- spec-reviewer: P0 없음. P1(조건을 바꿔도 처음 창을 써서 그 뒤 기록이 빠짐, 기준 시각이 응답 시각) 반영. P2(새로고침 중 행 유지, 모르는 enum, URL action 검사, 더 보기 403 문구, 404 문구, 빈 결과 이유·보존 안내, fixture의 운영자 범주, 실패 경로 시험) 반영. Operator 메뉴는 §1로 결정
- 수동: mock API로 표·details 펼침·범주 chip 확인
