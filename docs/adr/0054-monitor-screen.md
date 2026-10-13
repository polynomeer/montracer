# ADR 0054: S09 Monitor 화면 — 목록·상세·편집과 상태 조회 API

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-13 다음 단계 선택 "D: S09 Monitor 화면")
- Owner: Frontend lead
- 날짜: 2026-10-13 (제안·결정)
- 관련: F06, E05 · D05 §03·§04·§09, D02 §17·§20 · 계약 6 · ADR 0041, 0042, 0049, 0050, 0051, 0052, 0053

## 배경

E05의 백엔드는 다음 ADR까지 끝났다.
- 정의(ADR 0049)
- 평가 의미(ADR 0050)
- 주기 평가(ADR 0051)
- dry-run(ADR 0052)
- webhook 알림(ADR 0053)

사용자가 쓸 화면(S09)은 아직 없다. D05 §09가 정한 것은 다음과 같다.
- **편집 흐름:** 1 신호 → 2 query·group → 3 threshold·window → 4 no-data·recovery → 5 notification → 6 preview·publish. 단계를 뒤로 옮겨도 입력이 남는다.
- **미리보기:** 마지막 24시간 dry-run은 후보 firing 구간과 데이터 coverage를 보인다. 미래 알림 횟수의 예측이라고 하지 않는다.
- **단위:** 오류율은 UI에서 %, API에서 0~1 fraction이다. 단위가 맞지 않으면 저장할 수 없다.
- **상태:** OK·PENDING·ALERT·RECOVERING·NO_DATA·EVALUATION_ERROR를 구분한다.
- **timeline:** evaluation·state transition·notification attempt를 다른 icon으로 보인다.
- **합격:** query failure가 green으로 보이지 않는다. 경보 deep link는 같은 절대시간을 연다.
  - 경보 deep link(알림 payload의 화면 URL)는 아직 없다(C2, ADR 0053 재검토 조건). 이 화면은 URL의 조사 context 범위를 그대로 쓴다. deep link가 생기면 절대 `from`·`to`를 URL에 넣어 같은 구간을 연다.

명세가 정하지 않은 것이 있다.
- 목록이 보일 "상태"
- 화면이 읽을 상태·timeline API
- 로컬 개발에서 화면이 control-api에 닿는 방법

## 결정

### 1. 상태 조회 API (control-api)

- **권한**
  - 상태·값·group labels는 monitor가 tenant 전체 telemetry를 평가한 결과다(ADR 0051 §3).
  - 그래서 `monitors.read` + `telemetry.read` + environment 제한 없음일 때만 보인다(D04 §01: key는 발급 범위보다 강해질 수 없다).
  - 그렇지 않은 principal(environment 제한 key, telemetry 권한 없는 key)
    - 목록·단건은 정의만 준다. `status`는 null이고 `meta.warnings`에 `status_forbidden`이 온다. 화면은 "상태를 볼 수 없음"이고 "평가 전"이 아니다.
    - groups·timeline은 403이다.

- **목록·단건의 `status`:** `{state, groups_by_state, last_evaluation, stale}`
  - **`state`**는 이 revision group 상태 중 가장 나쁜 것이다. 순서는 ALERT > EVALUATION_ERROR > NO_DATA > RECOVERING > PENDING > OK다. 화면도 같은 순서로 정렬한다.
    - 결측·오류는 정상보다 나쁘게 친다(계약 6).
    - 켜져 있지만 평가 기록이 없으면 `NOT_EVALUATED`, 꺼졌으면 `DISABLED`다.
    - 평가는 됐지만 group이 없으면 마지막 평가 결과로 요약한다(오류면 EVALUATION_ERROR, 아니면 NO_DATA). OK로 보이지 않게 한다.
    - 마지막 평가도 이 revision의 것만 본다. 정의를 바꾼 직후는 이전 정의의 평가로 요약하지 않고 `NOT_EVALUATED`다.
  - **`stale`:** 마지막 평가가 주기의 3배(최소 5분)보다 오래됐으면 참이다. 평가가 멈춘 monitor가 마지막 상태로 굳어 보이지 않게 한다(D05 §03 stale).
  - 쓰기 응답(생성·수정)에는 `status`가 null이다. 평가 전이기 때문이다.
- **`GET /api/v1/monitors/{id}/groups?limit`**(최대 1,000): `alert_instances`의 이 revision group이다. 나쁜 상태 먼저, 같은 상태는 오래된 것 먼저다. 값이 없으면 null과 이유이고, 0으로 대체하지 않는다.
- **`GET /api/v1/monitors/{id}/timeline?from&to`**(기본 24시간, 최대 7일 = 평가 기록 보존):
  - 평가(`monitor_evaluations`), 상태 전이(outbox `alert.state_changed`), 알림(`notification_deliveries`)을 시간순으로 준다.
  - outbox는 tenant의 모든 감사·전이 event가 쌓이는 표라, resource 하나를 시간 범위로 읽는 index `outbox_resource_time (tenant_id, type, resource_id, occurred_at)`를 더한다(migration 00010, 운영 중 쓰기를 막지 않게 `CONCURRENTLY`).
  - 종류마다 가장 최근 2,000개이고, 넘으면 `*_truncated`다.
  - revision으로 거르지 않는다. 이력이므로 정의를 바꾸기 전의 평가·전이·알림도 시간순으로 함께 보인다(상태 요약·groups는 지금 revision만).
- 없는 monitor, 삭제된 monitor, 다른 tenant의 monitor는 모두 같은 404다.

### 2. 화면 (`apps/web/src/features/monitors`)

- **목록**(`/o/{org}/monitors`)
  - 나쁜 상태 먼저 보인다.
  - 상태 필터는 URL(`?state=`)에 남는다.
  - 열: 상태 배지(글자 + 아이콘), group 상태별 수, 조건 한 줄, 마지막 평가(실패 이유), 알림 연결 여부
  - 목록 조회가 실패하면 "monitor가 없다는 뜻이 아닙니다"를 붙인다.
  - **갱신 멈춤(stale):** 마지막 상태가 정상이어도 정상 색(초록)을 쓰지 않는다. 지금 정상인지 모르므로 데이터 없음 다음으로 정렬하고, "갱신 멈춤" 필터로 거를 수 있다.
- **상세**(`/o/{org}/monitors/{id}`)
  - **정의 요약:** 조건, 데이터 없음 정책, 평가 주기, 범위(filter 전체를 그대로 — 화면 form이 다루지 않는 조건도), group, 알림 policy 이름, 마지막 평가, revision
  - group은 최대 200개를 보이고, 더 있으면 잘렸다고 적는다.
  - **group 상태 표:** 값이 없으면 이유를 보인다.
  - **timeline:** 조사 context 범위와 같은 절대시간이다(D05 §09 deep link). 7일을 넘으면 마지막 7일이다.
    - 평가 띠: 경보 있음·경보 없음·데이터 없음·오류. 색과 함께 막대 높이로 나누고(가득 찬 막대 = 경보·오류, 낮은 막대 = 경보 없음, 바닥 눈금 = 데이터 없음) 개수를 글로 적는다.
      - "경보 없음"은 정상 초록이 아니라 중립색이다. group이 하나도 없던 평가는 값으로 판정한 것이 아니므로 데이터 없음으로 센다(계약 6).
      - 표시 한도(2,000개)로 앞부분이 잘렸으면 그 구간을 흐리게 하고 글로 적는다.
    - 전이(▲ 열림 / ▼ 닫힘 / ◆ 변화)와 알림(✉, 전달·실패·재시도·취소, 시도 수·응답 code)을 시간 역순으로 최근 200개 보인다. 넘으면 전체 수와 함께 잘렸다고 적는다.
  - 삭제는 확인 대화와 If-Match로 한다. 대화가 열리면 확인 버튼으로 focus가 가고, Esc·취소로 닫으면 삭제 버튼으로 돌아온다.
- **편집**(`/o/{org}/monitors/new`, `/{id}/edit`): 6단계 stepper이고 form 상태 하나라 단계를 오가도 입력이 남는다.
  - 다음 단계로 갈 때 그 단계까지의 입력만 본다. 뒤로는 언제나 간다.
  - 오류율은 % 입력이다(`threshold / 100`).
  - **form이 다루지 않는 filter:** form은 서비스·환경 eq만 편집한다. 다른 label 조건, 같은 label의 두 번째 값은 "이 화면에서 바꿀 수 없는 조건"으로 보이고 저장할 때 그대로 다시 보낸다. 수정 화면을 저장했다고 범위가 몰래 넓어지지 않는다(and는 펼쳐도 의미가 같다).
  - **알림 policy:** 목록을 못 불러오면 이유를 보이고, 지금 연결된 policy는 목록에 없어도 선택지로 남아 "알림 없음"으로 바뀌지 않는다.
  - 미리보기 단계에 들어오면 `validate`를 부른다. 경고는 설명문으로 보이고 24시간 dry-run을 보인다(group별 발화 구간 막대, 발화 시간, 최대값, 데이터 없음 횟수, 끝 상태).
  - 서버 field violation은 그 필드가 있는 단계로 돌아가 필드 옆에 보인다.
  - dry-run을 못 하면 이유를 보이고 그래도 저장할 수 있다(ADR 0052 §4).
  - **저장**
    - 생성: 편집 화면마다 Idempotency-Key 하나를 쓴다. 다시 눌러도 둘이 생기지 않는다.
      - 같은 key에 다른 내용(409)이면 앞선 저장이 응답만 잃고 성공했을 수 있다. 몰래 새 key로 다시 보내지 않고, 목록 확인을 권한 뒤 사용자가 고르면 새 key로 저장한다.
    - 수정: If-Match를 쓴다. 412면 덮어쓰지 않고 "최신 내용 보기"를 준다.
- **API 호출:** `apiRequest`가 PUT·DELETE·추가 header를 받는다. DELETE 204는 data null이다.

### 3. 로컬 개발

- dev proxy는 `^/api/v1/(monitors|notification-policies)` 경로를 control-api(`MONTRACER_DEV_CONTROL_URL`, 기본 18081)로 넘긴다. 나머지 `/api`는 query-api다. seed key를 서버 쪽에서 붙이는 방식은 같다(ADR 0042).
- seed(demo) API key에 `monitors.read`·`monitors.write`를 더한다. 이전 key가 이 권한을 갖지 않으면 재실행 때 새로 발급한다.

## 외부 사례 근거 (2026-10-13 확인 범위: 공개 제품 동작)

| 사례 | 내용 | 반영 |
|---|---|---|
| Datadog monitor 상태 화면 | monitor 하나의 상태 history(그래프와 상태 띠)와 event 목록(상태 전이·알림)을 함께 보인다. 목록은 상태(Alert·Warn·No Data·OK)로 거른다 | §2 상세 timeline·목록 상태 필터 |
| Grafana alert rule 목록 | rule을 상태(Firing·Pending·Normal)별로 나누고 instance(label 조합)별 상태를 보인다. No Data·Error를 Normal과 구분한다 | §1 group 상태별 수, §2 group 상태 표 |
| Datadog monitor 편집 Preview(ADR 0052 근거) | 지금 조건으로 과거 데이터를 어떻게 평가했을지 보인다 | §2 미리보기 단계 |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 목록 상태 | 가장 나쁜 group 상태 + 평가 전·꺼짐·stale | 경보 group 수만: 오류·데이터 없음 monitor가 정상처럼 보인다(계약 6) |
| 상태 API | 목록·단건 응답에 `status` 포함 + groups·timeline 별도 | 화면이 monitor마다 따로 조회: 목록이 N+1 요청이 된다 |
| 제한 key의 상태 | 정의는 주고 `status` null + 경고, groups·timeline 403 | 목록 전체 403: 정의 조회 권한(ADR 0049)까지 빼앗는다. environment로 걸러 보이기: 평가가 tenant 전체 telemetry라 group·값을 environment로 나눌 수 없다 |
| form 밖 filter | 보여 주고 그대로 보존 | 저장 금지: 다른 필드를 고칠 수 없다. 버림: 범위가 몰래 넓어진다 |
| timeline 원천 | 평가 기록·outbox·알림 행을 읽음 | 별도 timeline 표: 같은 사실을 두 번 저장한다 |
| 편집 | 한 form + 단계 표시(stepper) | 단계마다 따로 저장: 중간 상태 정의가 생긴다. 한 화면 긴 form: D05 §09 단계 순서를 지키지 않는다 |
| 오류율 입력 | % | fraction: 2를 입력하면 200%가 된다(흔한 실수) |

## 결과

- **API 변화:** monitor 목록·단건 응답에 `status`가 더해지고 목록 `meta`에 `warnings`가 더해진다(기존 필드는 같다). `GET /monitors/{id}/groups`, `/timeline`이 새로 생긴다.
- **schema:** PostgreSQL migration 00010이 outbox index 하나를 더한다(`CREATE INDEX CONCURRENTLY`, 트랜잭션 밖). 데이터는 바뀌지 않는다.
- **tenant 영향:** 없다. 모두 `WithTenant`(RLS) 안에서 읽는다. 상태는 `monitors.read` + `telemetry.read` + environment 제한 없음일 때만 보인다.
- **retention 영향:** 없다(평가 기록 7일 보존을 그대로 읽는다).
- **web:** S09 세 화면이 생긴다. Monitors 메뉴가 placeholder에서 실제 화면이 된다.
- **로컬:** seed key 권한이 늘었다(재실행 시 재발급). dev proxy가 control-api에 닿는다.

## Rollback

- web 화면은 배포를 되돌리면 placeholder로 돌아간다.
- 상태 API는 읽기 전용이다. 되돌려도 저장 데이터에 영향이 없다.
- migration 00010의 down은 index만 지운다(`DROP INDEX CONCURRENTLY`). 지워도 timeline이 느려질 뿐 결과는 같다.
- `CREATE INDEX CONCURRENTLY`가 중간에 실패하면 INVALID index가 남고, 재실행은 `IF NOT EXISTS`로 지나간다. 실패했으면 `SELECT indisvalid FROM pg_index WHERE indexrelid = 'outbox_resource_time'::regclass`로 확인하고, false면 `DROP INDEX CONCURRENTLY outbox_resource_time` 뒤 migration의 `CREATE INDEX CONCURRENTLY` 문을 그대로 다시 실행한다(goose 기록은 그대로 둔다).
- control-api의 `MONTRACER_MONITOR_API_ENABLED=false`면 monitor 경로가 404다.
  - 목록은 "목록을 불러오지 못했습니다. monitor가 없다는 뜻이 아닙니다"를 보인다.
  - 상세는 "찾을 수 없습니다(삭제·권한·기능 꺼짐)"를 보인다.

## 재검토 조건

- monitor·group이 많아 목록 상태 요약이 느릴 때: 요약을 평가 완료 때 일정 표에 함께 쓰는 방식
- notification policy 관리 화면(Settings), mute·silence 배지(F11), SLO·Incident 화면
- D05 §09 "template와 직접 query 변경 충돌 시 diff": template 기능과 함께
- metric 단위를 사전(ADR 0046)에서 받아 임계값 입력에 단위를 붙이기(지금은 안내문)

## 증거

- `internal/controlapi/monitor_status_test.go`
  - 목록·단건 `status`, 상태가 없으면 NOT_EVALUATED
  - environment 제한 key·telemetry.read 없는 key: 정의는 200, `status` null + `status_forbidden`, groups·timeline 403. 제한 없는 key·Developer는 상태를 본다
  - groups(값 없으면 null과 이유)
  - timeline(기본 24시간, 7일 초과·역순·형식 오류는 400, 없는 monitor는 404, Viewer는 403)
- `internal/controldb/monitor_status_integration_test.go`(PostgreSQL)
  - 평가 전 NOT_EVALUATED, 꺼짐 DISABLED, ALERT+NO_DATA → ALERT, 10분 뒤 stale
  - group 순서·limit, timeline(평가·열림 전이·알림), 8일 거절
  - 정의 변경 뒤 이전 revision group·마지막 평가 제외(NOT_EVALUATED), 다른 tenant는 없음·404
  - migration 00010 up → down → up(로컬 PostgreSQL)
- `internal/controldb/monitor_status_test.go`: 상태 권한(Developer·제한 없는 key 허용, environment 제한·telemetry.read 없음·monitors.read 없음 거절)
- `apps/web/src/features/monitors/monitorSpec.test.ts`
  - % ↔ fraction 왕복, 입력 오류(빈 값을 0으로 보내지 않음), 서버 경로 → 필드, 조건·결측 정책 문장, 값 표기
  - form 밖 filter(다른 label·같은 label 두 번째 값·중첩 and) spec → form → spec 보존, filter 문장
  - stale 정상은 정상 색 아님·정렬 위치, 평가 띠 칸(group 0 → 데이터 없음, 경보 없음은 초록 아님)
- `apps/web/src/features/monitors/Monitors.test.tsx`
  - 목록: 나쁜 상태 순, 평가 오류·갱신 멈춤, 상태 필터 URL, 조회 실패·API 꺼짐(404)은 "없음"이 아님, 빈 목록 안내, 상태 권한 없음 → "상태를 볼 수 없음", 멈춘 정상이 정상보다 먼저·"갱신 멈춤" 필터
  - 상세: 정의·policy 이름, group 값·이유, 평가 띠 개수, 전이·알림 문장, 삭제 If-Match·이동, 삭제 대화 focus·Esc, 상태 권한 없으면 groups·timeline을 부르지 않음, 잘린 group·이벤트·평가 띠 표시, 404
  - 편집
    - 이름 없으면 다음으로 가지 않는다.
    - 2.5% → 0.025, filter·policy를 validate로 보낸다.
    - 미리보기(경고 설명·예측 아님·발화 막대), Idempotency-Key로 저장하고 이동한다.
    - 서버 violation이면 해당 단계로 돌아간다.
    - 412는 덮어쓰지 않는다.
    - dry-run을 못 해도 저장할 수 있다.
    - form 밖 filter를 보이고 PUT에 그대로 보낸다.
    - policy 목록 실패에도 연결된 policy가 유지된다.
    - 409(같은 key 다른 내용)는 다시 보내지 않고, 확인 뒤 새 key로 저장한다.
- `apps/web/dev-proxy.test.ts`: monitor·policy 경로는 control-api로(먼저 맞음), 비슷한 경로(`/monitorsx`)는 query-api로
- 화면 확인(mock API, 브라우저)
  - 목록·상세·편집 미리보기가 렌더링됐다.
  - 375px에서 세 화면 모두 가로 스크롤이 없다(scrollWidth = clientWidth).
  - 끝까지 열린 발화 구간이 막대 밖으로 밀려 안 보이던 것을 고쳤다.
