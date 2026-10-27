# ADR 0073: monitor와 서비스의 연결, 상태 시작 시각(state_since)

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-27 "승인할게 이어서 진행해줘" — [design-rollout](../plan/design-rollout.md) D3 첫 항목)
- Owner: Backend lead (E05)
- 날짜: 2026-10-27 (제안·결정)
- 관련: F06 후속, E05·E04 · D02 §17(상태 머신), §19 · D05 §03(상태 계약), §04(S01 "상태→서비스"), §05(S02 "monitor 없는 service는 초록 정상 대신 '판정 없음'"), §07(S03 node 상태 icon) · 계약 4·6 · ADR 0049(monitor 정의), 0050(평가 의미), 0051(alert-worker), 0054(S09 화면), 0062·0063(service map), 0048(S01)

## 배경

S01·S02·S03은 서비스의 상태를 보여야 한다(D05 §04·§05·§07). 그런데 monitor와 서비스를 잇는 규칙이 명세에 없다. 그래서 지금 화면은 이렇게 한다.

- S01: 서비스별 판정을 하지 않고 "monitor와 서비스의 연결이 아직 없음"이라고 안내한다(ADR 0048 변경 이력).
- S02: 머리에 경보를 보이지 않는다.
- S03: node 상태 icon이 없다(ADR 0063).

디자인 시안은 "ALERT 14:30부터"처럼 상태가 시작된 시각도 보인다. `alert_instances`에는 그 시각이 없다. `violation_since`·`nodata_since`는 판정용 시각이라 상태 시작과 다르다(예: RECOVERING은 둘 다로 알 수 없다).

정할 것은 네 가지다.

1. 어떤 monitor가 어떤 서비스에 대한 것인가
2. 여러 monitor·group이 걸린 서비스의 상태를 어떻게 하나로 보이나
3. 상태 시작 시각을 어디에 두나
4. 화면이 어떤 API로 받나

## 결정

### 1. 연결 규칙: monitor 정의에서 도출한다 (새 필드 없음)

monitor filter는 `and`·`eq`만 있다(ADR 0049). 그래서 각 group의 **유효 label** = group label(`group_by` 값) ∪ filter의 고정 `eq` 조건으로 정할 수 있다. 유효 label에 `service.name`이 있으면 그 group은 그 서비스에 대한 것이다.

- **환경:** 유효 label에 `deployment.environment.name`이 있으면 그 환경에만 걸린다. 없으면 "전체 환경 monitor"다 — 모든 환경을 함께 평가하므로 어느 환경의 서비스에서 보이든 그 사실을 함께 표시한다(§4).
- **namespace:** `service.namespace`가 있으면 맞춘다. 없으면 이름만 맞춘다(같은 이름이 여러 namespace에 있으면 모두에 걸리고, 그 사실을 표시한다).
- **걸지 않는 것:**
  - `service.name`이 유효 label에 없는 monitor(조직 전체 오류율 등)
  - 값이 빈 문자열인 `service.name` group(속성이 없던 요청이 빈 값으로 묶인 것)
  - 한 `and` 안에 `service.name`을 다른 값으로 두 번 고정한 filter(어떤 요청과도 맞지 않는다)
  - 지운 monitor
- **group이 없는 monitor**(평가 전, 처음부터 결측·실패, revision이 막 바뀜): filter에 `service.name`이 고정돼 있으면 monitor 요약 상태(평가 전·데이터 없음·평가 오류)로 그 서비스에 걸린다. `group_by service.name`만 있으면 어느 서비스인지 알 수 없다 — 이런 monitor는 응답의 `unattributed`에 따로 보인다(서비스가 "판정 없음"으로 조용히 바뀌지 않게, 계약 6).
- 연결은 매 요청 서버가 계산한다. 저장하지 않으므로 monitor 정의를 바꾸면 바로 따라간다.

### 2. 서비스 상태 = 걸린 것들의 가장 나쁜 상태

- 기여: group이 있는 monitor는 지금 revision의 group 상태, group이 없는 monitor는 monitor 요약 상태(§1). 꺼진 monitor(DISABLED)는 기여하지 않는다.
- 순서는 monitor 목록과 같다(ADR 0054): ALERT → EVALUATION_ERROR → NO_DATA → 멈춘 정상 → RECOVERING → PENDING → NOT_EVALUATED → OK. 평가 전은 정상보다 나쁘다 — 하나라도 평가 전이면 서비스는 정상이 아니다.
- **멈춤(stale):** monitor가 멈췄으면(마지막 평가가 주기의 3배, 최소 5분보다 오래됨) 그 기여에 stale을 붙이고, 정상이면 "멈춘 정상" 순서로 센다. 서비스 결과의 `stale`은 기여 중 하나라도 멈췄으면 참이다. 멈춘 정상은 초록 정상 배지로 그리지 않는다(D05 §03).
  - 한 번도 평가되지 않은 켜진 monitor도 정의(또는 revision)가 바뀐 지 같은 시간이 지나면 멈춘 것으로 본다(평가기가 집어 가지 않는 경우).
- 걸린 것이 없는 서비스는 응답에 없다. 화면은 "판정 없음"(D05 §05)으로 보인다. 응답이 잘렸으면(§4) "판정 없음" 대신 "확인할 수 없음"이다.
- 상태를 볼 권한이 없으면(`AuthorizeMonitorState`, tenant 전체 telemetry 읽기) 403이다. 빈 결과로 바꾸지 않는다.

### 3. 상태 시작 시각: `alert_instances.state_since`

- 열을 더한다(expand-only migration, `NULL` 허용). `montracer_rw`는 이미 표 전체 UPDATE 권한이 있다(00008).
- **쓰는 때:** 행을 처음 넣을 때(revision이 바뀌어 새로 시작한 group 포함), 또는 저장된 `state`와 다른 상태로 바뀔 때. 같은 상태가 이어지면 그대로 둔다.
- **값:** 그 판정의 window 끝. window가 없는 판정(watermark 없음·멈춤, query 실패, group 상한 초과, `no_data.after_seconds`로 벽시계 기준 경보)은 그 평가의 slot 시각이다.
- **기존 행:** NULL로 남는다. 상태가 바뀌거나 revision이 바뀌면 채워진다 — 오래 정상인 group은 오래 NULL일 수 있다. 화면은 "시작 시각 모름"으로 보이고 지어내지 않는다. outbox에서 거슬러 채우지 않는다(보존 기간 밖은 어차피 모르고, 일부만 채우면 섞여 보인다).
- **API에 노출:**
  - `MonitorGroup.state_since`
  - `MonitorStatus.state_since`: 요약 상태가 group에서 왔으면 같은 상태 group 중 가장 이른 값(하나라도 NULL이면 null). 요약이 group에서 오지 않았으면(평가 전·꺼짐·group 없는 결측·실패) null.
  - 서비스 결과의 `state_since`: 가장 나쁜 상태와 같은 기여 중 가장 이른 값, 하나라도 모르면 null.

### 4. API: `GET /api/v1/service-health` (control-api)

- 인자: `environment`(선택, 길이·문자 검증, parameter binding).
- 응답 `data`:
  - `services[]`: (`service_namespace`, `service_name`, `environment`)마다 한 행. 서버가 합친다(가장 나쁜 상태, stale은 OR, state_since는 §3 규칙).
    - `environment`가 null인 행 = 전체 환경 monitor만 걸린 경우.
    - `environment`를 주면: 그 환경 행에 전체 환경 monitor의 기여를 합치고 `includes_all_environment_monitors: true`를 붙인다. 다른 환경 행은 주지 않는다.
    - `environment`를 주지 않으면: 환경별 행과 null 행을 따로 준다. 화면이 서비스 행(환경 포함)에 맞출 때 null 행을 같은 규칙으로 합친다(시험한다).
    - 필드: `state`, `stale`, `state_since`, `monitor_count`, `name_only`(namespace 없이 이름으로만 걸린 기여가 있음), `monitors`(나쁜 순 최대 5개: id·name·state·stale·`all_environments`).
  - `unattributed[]`: §1의 서비스를 알 수 없는 monitor(나쁜 순 최대 20개, id·name·state·stale).
- **비용 상한:**
  - monitor는 (created_at, id) 순으로 1,000개까지 읽는다. 넘으면 `meta.warnings`에 `monitors_truncated`.
  - group은 SQL에서 (monitor, service.namespace, service.name, environment, state)로 묶어 읽는다(행 수 대신 묶음 수). 묶음이 20,000개를 넘으면 그만 읽고 `groups_truncated`.
  - 어느 쪽이든 잘렸으면 화면은 응답에 없는 서비스를 "판정 없음"이 아니라 "확인할 수 없음(일부 monitor만 반영)"으로 보인다.
  - 쿼리 시간 상한 5초(statement timeout). 넘으면 503.
- **권한·tenant:** `AuthorizeMonitorState`(tenant 전체 — environment 제한 key는 403), 제어 DB `WithTenant`(RLS, ADR 0016).
- **flag·지표:** `MONTRACER_SERVICE_HEALTH_ENABLED`(기본 켬). 끄면 route가 404다 — 화면은 지금처럼 "서비스별 판정을 하지 않음" 안내로 돌아간다. 지표는 control-api 공통 요청 지표(지연·오류)와 잘림 횟수(`montracer_service_health_truncated_total{reason}`).

### 5. 화면

- S01: 서비스 표에 "상태" 열(가장 나쁜 상태 배지와 시작 시각, stale·전체 환경 monitor 표시). 응답에 없으면 "판정 없음", 잘렸으면 "확인할 수 없음". 상태 조회 실패·권한 없음은 열 전체를 "확인할 수 없음"으로. `unattributed`가 있으면 표 위에 "서비스를 알 수 없는 monitor n개(평가 오류 등)"를 링크와 함께. ADR 0048의 "판정하지 않음" 안내를 바꾼다.
- S02: 머리에 상태 배지(예: "경보 · 14:30부터")와 걸린 monitor 목록 링크.
- S03: node 상태 icon(배지와 같은 모양·색). DB·queue·외부 node처럼 서비스가 아닌 node는 "판정 없음".

## 외부 사례 근거 (2026-10-27 확인)

| 사례 | 확인한 동작 | 이 결정에 쓴 부분 |
|---|---|---|
| Datadog Service Page ([docs](https://docs.datadoghq.com/tracing/services/service_page/)) | monitor는 `service:<name>` 태그가 있거나, query에 그 태그가 들어 있거나, query가 service로 group되면 그 서비스에 붙는다 | §1 정의(filter의 고정 조건·group_by)에서 도출 |
| Prometheus HTTP API ([docs](https://prometheus.io/docs/prometheus/latest/querying/api/)) | 경보마다 `activeAt`(이 상태가 된 시각)과 `state`를 준다 | §3 group마다 state_since |
| Grafana Alerting 상태 ([docs](https://grafana.com/docs/grafana/latest/alerting/fundamentals/alert-rule-evaluation/state-and-health/)) | NoData(query 성공·점 없음)와 Error(query 실패)를 Normal과 따로 둔다 | §2 결측·실패를 정상으로 합치지 않음(ADR 0050과 같음) |

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A (채택) 정의에서 도출 | 새 필드·migration 없음, 정의를 바꾸면 바로 따라감, Datadog과 같은 모델 | 이름만 같은 서비스(다른 namespace)에 함께 걸릴 수 있음(응답에 드러남) |
| B monitor에 `service_ids` 필드 | 명시적 | 사람이 고르는 값과 실제 filter가 어긋날 수 있음(다른 서비스를 보면서 이 서비스에 걸림). schema·화면 추가 |
| C 서비스 catalog에 monitor 목록 | catalog가 소유 | 같은 어긋남, monitor 삭제 시 정리 필요 |
| D group 상태 시작을 outbox 전이에서 계산 | migration 없음 | outbox 보존 기간 뒤 사라짐, 요청마다 스캔 |

## 결과

- 이점: S01 판정 열·"판정 없음" 수·시작 시각, S02 머리 경보, S03 node 상태가 풀린다(design-rollout D3 첫 항목).
- 비용: 요청마다 tenant의 monitor(≤1,000)와 group 묶음(≤20,000)을 읽는다.
- 영향 받는 계약:
  - OpenAPI: `GET /api/v1/service-health`, `MonitorGroup.state_since`, `MonitorStatus.state_since`
  - migration: `alert_instances.state_since timestamptz NULL`
- 필요한 시험:
  - 연결: 고정 filter·group_by·환경·namespace, 조직 전체 monitor, 빈 `service.name`, 서로 다른 값 두 번 고정, 지운 monitor
  - 합치기: 꺼짐, 평가 전과 정상 섞임, stale(멈춘 정상·한 번도 평가 안 됨), group 없는 결측·실패(filter 연결 → 그 상태, group_by만 → `unattributed`), revision 직후
  - 환경: 전체 환경 monitor + 환경별 monitor 합치기(`environment` 있음·없음), 화면의 null 행 합치기
  - state_since: 처음 넣은 정상, 상태 변화, 같은 상태 유지, window 없는 판정(slot 시각), revision 재시작, 기존 NULL
  - 비용: monitor 1,000 초과·묶음 20,000 초과에서 결정적 순서와 경고, 화면이 "판정 없음" 대신 "확인할 수 없음"
  - 권한·격리: environment 제한 key 403, 다른 tenant의 monitor가 섞이지 않음(`tests/isolation`)
  - flag off: route 404, 화면은 기존 안내

## 구현 순서

1. 이 ADR과 계획(이 PR)
2. 백엔드: migration, alert-worker의 state_since(ADR 0051 변경 이력), controldb 연결·합치기, control-api route·flag·지표, OpenAPI(`MonitorStatus`·`MonitorGroup` 변경은 ADR 0054 변경 이력), 위 시험(단위·통합·격리), 아키텍처 문서
3. 화면: S01 상태 열(ADR 0048 변경 이력), S02 머리, S03 node 상태(ADR 0063 변경 이력), 상태 계약 시험, 작업계획·기능 상태 등록부

## Rollback

`MONTRACER_SERVICE_HEALTH_ENABLED`를 끄면 route가 404가 되고 화면은 지금처럼 "서비스별 판정을 하지 않음"으로 돌아간다(flag off 경로를 시험한다). 코드를 되돌릴 때는 route·화면 열을 뺀다. `state_since` 열은 expand-only라 남아도 해가 없다.

## 재검토 조건

- 이름만 같은 서비스가 여러 namespace에 있어 잘못 걸리는 일이 보이면: namespace를 필수로 맞추거나 후보 B를 함께
- tenant의 monitor가 1,000개를 넘는 조직이 생기면: 연결 결과를 저장(materialize)하거나 page로 나눔
- composite monitor(F11)가 생기면: composite의 연결 규칙(참조한 monitor의 서비스)

## 증거

- 설계 검토(spec-reviewer): P0 없음. P1 5건 반영 — window 없는 판정의 state_since(slot 시각), 처음 넣은 행에도 기록, group 없는 monitor(filter 연결은 요약 상태, group_by만은 `unattributed`), 응답 행의 환경 합치기 규칙, 비용 상한(monitor·group 묶음·시간)과 잘렸을 때 "확인할 수 없음". P2 반영 — 상태 이름 통일, 평가 전 순서·한 번도 평가 안 된 monitor의 stale, ADR 0054의 멈춘 정상 순서, 빈 값·모순 filter·지운 monitor, 기존 NULL의 한계, 요약 state_since가 null인 경우, 문서·flag·지표.
- 백엔드 시험:
  - 단위(`internal/servicehealth`): 연결 규칙(고정 filter·group_by·빈 값·모순·꺼짐·조직 전체), group 없는 monitor(filter → 요약 상태, group_by만 → unattributed), 합치기(가장 나쁜 상태·시작 시각·평가 전·멈춘 정상·모르는 상태), 환경 합치기, monitor 목록 상한
  - 상태 머신(`internal/alerting`): 처음 넣은 정상, 상태 변화, 같은 상태 유지, window 없는 판정, 기존 nil, 같은 window 재평가. alert-worker 저장 행 왕복
  - handler(`internal/controlapi`): 성공 응답(OpenAPI 계약), environment 400, environment 제한 key 403, flag off 404, 잘림 지표
  - PostgreSQL 통합(`internal/controldb`): 평가 전·후, 환경, 다른 tenant, 한 번도 평가 안 된 monitor의 stale, group_by labels 경로·빈 환경, 시작 시각 모름, monitor·묶음 상한과 결정적 순서
  - 격리(`tests/isolation`): 다른 tenant·tenant header 무시·environment 제한 key 403
- 백엔드 spec-reviewer: P0 없음. P1(잘림 경계 monitor) 반영, P2(트랜잭션, 빈 group 값, 평가 시각 문구, 이름만 합치기, tenant predicate, 시험) 반영

## 변경 이력

- 2026-10-27 (구현 리뷰 반영, 백엔드 PR)
  - §3 window 없는 판정의 시각은 "slot 시각"이 아니라 평가 시각(worker가 그 평가를 시작한 벽시계)이다. `no_data.after_seconds`로 벽시계 기준 경보가 되어도 판정에 window가 있으면 그 window 끝이다.
  - §4 잘림은 행 단위로도 알린다: monitor·group 묶음을 상한에서 자르면 모든 행에 `partial: true`. 정상·진행 중 상태는 실제보다 덜 나쁠 수 있으므로 화면은 partial 행의 정상·대기·복구 중을 "확인할 수 없음(일부만 반영)"으로 보인다(경보·평가 오류·데이터 없음은 하한이라 그대로). group 묶음이 잘리면 경계(일부만 읽은) monitor와 그 뒤 monitor는 기여 계산에서 뺀다 — "group 없음"으로 잘못 분류하지 않는다.
  - §1 빈 group 값: `group_by`의 environment·namespace 값이 빈 문자열(속성이 없던 요청)이면 "조건 없음"과 다르다. API는 조건 없음만 `null`, 빈 값은 `""`(화면 "환경 미지정"). 그 행은 다른 환경 조회에 합쳐지지 않는다.
  - §4 이름만 걸린 행(`service_namespace: null`)은 같은 이름의 모든 namespace 서비스에 적용된다 — 화면이 서비스 행에 맞출 때 환경 null 행과 같은 규칙으로 합친다.
  - 상태 요약도 같은 트랜잭션·같은 5초 상한 안에서 계산한다(revision이 바뀌는 사이 섞이지 않게).
  - flag는 monitor API에도 딸린다: `MONTRACER_MONITOR_API_ENABLED=false`면 서비스 상태도 404다.
