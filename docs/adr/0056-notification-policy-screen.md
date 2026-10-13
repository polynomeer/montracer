# ADR 0056: 알림 policy 화면과 URL 없이 수정

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-13 다음 단계 선택 "알림 policy 화면")
- Owner: Frontend lead
- 날짜: 2026-10-13 (제안·결정)
- 관련: F06, E05 · D05 §01·§03·§09, D02 §14·§20 · ADR 0041, 0053, 0054, 0055

## 배경

notification policy는 API로만 만들 수 있다(ADR 0053·0055). S09 편집의 알림 단계(ADR 0054)는 이미 있는 policy를 고르기만 한다. 그래서 사용자가 다음 일을 화면에서 끝낼 수 없다.
- webhook 목적지 만들기
- 서명 secret 받기
- 시험 전송
- secret 교체
- policy 지우기

명세가 정하지 않은 것이 있다.
- 이 화면이 정보 구조의 어디에 있는가. D05 §01 메뉴에 알림 목적지가 없고, §04 화면 목록에도 ID가 없다.
- 1회 표시 secret을 어떻게 보이는가
- 수정 화면이 전체 URL을 모르는 문제
  - URL은 암호문으로만 저장하고 응답에는 host만 있다(ADR 0053 §1).
  - 그런데 수정 API(PUT)는 `webhook.url`이 필수였다. 이름만 바꾸려 해도 전체 URL을 다시 입력해야 했다.

## 결정

### 1. 위치: Respond 그룹의 Notifications (`/o/{org}/notifications`)

- 경보 규칙(Monitors) 곁에 둔다. 화면 ID는 없다(`id: null`, D05가 정하지 않음).
- Admin 그룹이 아닌 이유는 조회 권한이 `monitors.read`(Developer 이상)라서다. Developer가 monitor에 붙일 policy를 볼 수 있어야 한다. 쓰기는 Tenant Admin이다.
- 화면은 버튼을 숨기지 않는다. 서버가 403을 주면 "Tenant Admin만 할 수 있습니다"를 보인다. 메뉴 표시는 보안 경계가 아니다(D05 §04).

### 2. 화면

- **목록:** 이름, 켜짐/꺼짐, 목적지(host), 서명 key id, "교체 중"(이전 secret이 아직 유효), 수정 시각
  - 조회 실패: "policy가 없다는 뜻이 아닙니다"
  - API 꺼짐(404): "이 배포에서는 알림을 쓸 수 없습니다"(ADR 0053 Rollback)
  - 권한 없음(403): 필요한 권한을 안내한다
- **만들기:** 이름, webhook 주소(https만), 켜기
  - 화면 하나에 Idempotency-Key 하나다.
  - 저장되면 **서명 secret을 한 번만** 보인다: 읽기 전용 입력, 복사, 검증 방법, "저장했습니다" 버튼. 버튼을 누른 뒤에야 상세로 간다.
  - 재생 응답(secret 없음)이면 "다시 볼 수 없다, 교체하라"고 안내한다.
  - 서버 field violation(`name`, `webhook.url`)은 필드 옆에 보인다.
- **상세**
  - **정의:** 이름·켜기를 고친다. 주소는 "새 주소(바꿀 때만)"이고, 지금 목적지(host)를 보인다. 비워 두면 저장된 주소를 그대로 둔다(§3).
    - If-Match이고, 412면 덮어쓰지 않는다.
    - "최신 내용 불러오기"는 form을 다시 읽은 서버 값으로 새로 시작한다(리뷰 반영). 예전 입력을 새 revision으로 보내 남의 변경(예: 끄기)을 덮지 않게 한다.
  - **시험 전송(ADR 0055 §2)**
    - 보낸 뒤 1.5초 간격으로 최대 20번 결과를 다시 읽는다.
    - 보내는 중·전달됨(응답 code)·실패(이유: 사설 주소 차단·timeout·TLS·redirect 등)·취소를 다른 배지로 보인다. 보내는 중을 성공으로 보이지 않는다.
    - 진행 중이던 시험을 돌려받으면 그렇다고 적는다. 429면 다시 할 수 있는 시간을 적는다.
    - 결과를 읽지 못하면(4xx 또는 3번 연속 실패) "확인하지 못했습니다"와 이유를 보인다. 못 읽은 것을 "보내는 중"으로 보이지 않는다(리뷰 반영). 화면을 떠나면 요청과 대기를 취소한다.
    - 시험 전송·결과 조회는 Tenant Admin(`policies.write`)이다(ADR 0055 §2). Developer는 403 안내를 본다.
  - **서명 secret:** 지금 key, 이전 key와 함께 서명하는 만료 시각
    - 교체 확인 대화에서 "24시간 함께 서명(권장)"과 "바로 버리기(유출 시)" 중 고른다.
    - 대화에서 receiver가 쉼표 목록 서명을 받아야 한다고 알린다(ADR 0055 §3). 새 secret은 한 번만 보인다.
  - **삭제:** 확인 대화와 If-Match로 한다. monitor가 쓰면(409) "monitor에서 먼저 떼어 내세요"를 보인다.
  - 확인 대화는 열리면 첫 조작으로, 닫히면 여는 버튼으로 focus가 가고, Esc로 닫힌다(ADR 0054와 같은 규칙). 화면 안에 펼쳐지는 비모달 확인 영역이라 focus를 가두지 않는다(`aria-modal` 없음).
- **목록 한도:** 이름순 200개까지 읽는다. 더 있으면 "처음 200개만 보입니다"를 적는다. monitor 편집의 알림 단계도 같은 안내를 한다(리뷰 반영).
- **secret 취급:** 평문은 응답을 읽는 순간 컴포넌트 상태로만 간다(`apiRequest`의 `onEnvelope`). 전역 store·저장소·로그·URL에 두지 않는다. "저장했습니다"를 누르거나 화면을 떠나면 사라진다.
  - 표시 영역은 `role=alert`가 아니다. 화면 낭독기가 secret 값을 소리 내어 읽지 않게 한다. 나타나면 제목으로 focus가 가고, 닫으면 교체 버튼으로 돌아온다.
  - 복사한 값은 운영체제 클립보드에 남는다. 저장한 뒤 비우라고 안내한다.

### 3. API: 수정에서 `webhook`·`enabled` 생략 = 저장된 값 유지

- `PUT /api/v1/notification-policies/{id}`에서 `webhook`이 없거나 `null`이면 저장된 암호문·표시(host)를 그대로 둔다. `{"webhook":{}}`처럼 주소가 빈 값은 400이다.
- `enabled`가 없으면 저장된 값을 그대로 둔다(리뷰 반영). 빠뜨린 필드 때문에 꺼진 policy가 다시 켜지지 않게 한다. 생성에서는 없으면 켜짐이다.
- 생성(POST)은 여전히 `webhook.url` 필수다.
- 다른 field 규칙(모르는 field 400, If-Match 428/412)은 같다.
- 감사 details의 목적지·켜짐은 저장 뒤 값이다. `webhook_changed`(bool)로 같은 host 안에서 주소(receiver token)가 바뀐 것도 드러낸다. 주소 자체는 남기지 않는다.
- **하위 호환:** 지금까지 `webhook`을 빼면 400이었다. 이제 200이다. `enabled`를 빼면 켜짐이었지만 이제 유지다. 둘 다 보내는 기존 호출은 그대로다.

## 외부 사례 근거 (2026-10-13 확인 범위: 공개 제품 동작)

| 사례 | 내용 | 반영 |
|---|---|---|
| Grafana Alerting contact points([test contact point](https://archive.grafana.com/docs/grafana/v9.2/alerting/contact-points/test-contact-point/), [8.2 소개](https://grafana.com/blog/new-in-grafana-8-2-test-contact-points-for-alerts-before-they-fire)) | 알림 목적지(contact point)를 Alerting 메뉴에 경보 규칙과 함께 둔다. 목적지 편집 화면의 Test로 시험 알림을 보내고, 성공·실패(오류 포함)를 바로 보인다 | §1 Respond 그룹, §2 시험 전송과 결과 표시 |
| Stripe webhook endpoint 화면 | signing secret은 endpoint 화면에서 드러내 보고, "Roll secret"으로 교체하며 이전 secret 만료(최대 24시간)를 고른다 | §2 교체 대화의 겹침 선택. 다시 보기는 하지 않는다(ADR 0053 §1) |
| GitLab webhook API 결함([#384326](https://gitlab.com/gitlab-org/gitlab/-/issues/384326)) | 숨겨진 secret token을 다시 보내지 않고 webhook을 수정하면 token이 지워졌다(결함으로 보고). 다시 보이지 않는 값은 생략하면 유지해야 한다는 반례다 | §3 생략하면 유지 |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 위치 | Respond > Notifications | Admin: Developer가 조회할 수 없다(조회는 `monitors.read`). Organize > Integrations: 설치 화면(S13)과 섞인다 |
| URL 수정 | 비우면 유지(PUT 생략) | 매번 전체 URL 재입력: 이름만 바꿔도 receiver token을 다시 찾아야 한다. 전체 URL 다시 보이기: ADR 0053 §1(host만 보임)과 충돌한다 |
| 시험 결과 | 짧은 간격으로 다시 읽기(최대 30초) | 실시간 stream: 한 번 보내는 결과에 비해 무겁다 |
| secret 표시 | 확인 버튼을 누를 때까지 표시 | 자동으로 사라짐: 복사 전에 사라질 수 있다 |

## 결과

- **web:** Notifications 목록·만들기·상세 세 화면이 생긴다(`apps/web/src/features/notifications`). 메뉴 Respond에 Notifications가 더해진다.
  - `api/notificationPolicies.ts`는 policy 호출을 모은다. `api/monitors.ts`의 policy 타입·목록 함수는 여기서 다시 내보낸다.
- **API:** PUT에서 `webhook`·`enabled` 생략은 저장된 값 유지다(§3). schema 변경은 없다. policy API는 OpenAPI 원천(`api/openapi`)이 아직 없어 ADR·README가 계약이다. OpenAPI 도입(작업계획서 §5.2)과 함께 옮긴다.
- **tenant 영향:** 없다(같은 저장소·권한).
- **retention 영향:** 없다.

## Rollback

- web 배포를 되돌리면 메뉴·화면이 사라진다. API로는 여전히 다룰 수 있다.
- PUT `webhook` 생략을 되돌리면 생략한 요청이 다시 400이 된다. 저장 데이터에는 영향이 없다.

## 재검토 조건

- 조직 role을 화면이 알게 되면(OIDC session, G1) Tenant Admin이 아닌 사용자에게 쓰기 버튼을 처음부터 숨기거나 비활성으로 보인다.
- policy를 쓰는 monitor 목록(지우기 전 확인), 최근 전송·시험 기록 목록
- Slack·이메일 connector가 생기면 목적지 종류를 고르는 단계

## 증거

- `internal/controlapi/notifications_test.go`
  - `TestPolicyUpdateKeepsURL`: webhook 없이 수정하면 암호문·host가 그대로이고 이름·enabled만 바뀐다. 생성에서 webhook 생략은 400이다.
  - `TestPolicyUpdateKeepsEnabled`: 꺼진 policy를 enabled 없이(`"webhook": null`) 수정해도 꺼진 채다.
- `internal/controldb/notification_ops_integration_test.go`(PostgreSQL): URL·enabled 없이 수정한 뒤에도 claim에서 풀리는 URL이 직전 주소이고 enabled가 그대로다. 감사 `webhook_changed`는 주소를 바꾼 수정만 참이다.
- `apps/web/src/features/notifications/Notifications.test.tsx`
  - 입력 검사, 시험 결과 문장(보내는 중≠성공, 모르는 code 원문)
  - 목록(켜짐·host·교체 중, API 꺼짐 404·조회 실패는 "없음"이 아님)
  - 만들기: 필수 입력 전 요청 없음, Idempotency-Key·본문, secret 1회 표시 뒤 상세로 이동하면 secret 없음, 재생 응답 안내, 400 필드 오류·403
  - 상세
    - 주소 비우면 webhook 없는 PUT·If-Match, 412는 덮어쓰지 않음, 새 주소는 webhook으로
    - 시험: 보내는 중 → 전달됨·응답 code, 실패 이유, 429 다시 할 시간
    - 교체: 대화 focus·Esc 복귀, 바로 버리기 = 0초·If-Match, 새 secret 1회, 닫으면 사라짐
    - 삭제 409 안내 뒤 성공하면 목록으로, 404 화면
    - 412 뒤 최신 내용을 불러오면 form이 서버 값(이름·꺼짐)으로 바뀐다
    - 시험 결과 조회 404면 "확인하지 못했습니다"(보내는 중 아님)
    - 새 secret 제목으로 focus, 닫으면 교체 버튼으로
  - 목록 200개 초과 안내
- 화면 확인(mock API, 브라우저)
  - 목록, 상세의 시험 실패 결과, 교체 뒤 secret 표시가 렌더링됐다.
  - secret 영역이 가운데 정렬·좁은 입력이던 것을 왼쪽 정렬·넓은 입력으로 고쳤다.
  - 375px에서 세 화면 모두 가로 스크롤이 없다(scrollWidth = clientWidth).
- spec-reviewer: P1 1건(412 뒤 불러와도 form이 예전 값이라 남의 변경을 덮음)·P2 8건(PUT enabled 생략이 다시 켬, 감사에 주소 변경 표시 없음, `webhook: null` 의미, 결과 조회 실패를 보내는 중으로 보임·취소 없음, 200개 초과 표시, secret 영역 `role=alert`·focus, 이전 key 만료 null 표시, 문서 열) 반영. F06 registry 상태는 Gate 증거 전이라 `planned` 유지
