# ADR 0057: Dashboard 정의와 API (S10 단계 A)

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-14 다음 단계 선택 "S10 Dashboard 저장·API")
- Owner: API lead
- 날짜: 2026-10-14 (제안·결정)
- 관련: F05, E05 · D02 §11·§14·§19, D04 §01, D05 §10~11 · 계약 1·4·6 · ADR 0016, 0027, 0049

## 배경

명세가 정한 것은 다음과 같다.
- **D02 §11:** `dashboards (id, tenant_id, spec, revision)`, 낙관적 잠금과 삭제 tombstone, RLS 예시. revision 충돌은 409 또는 If-Match 실패 412이고, 마지막 쓰기가 무조건 덮어쓰지 않는다.
- **D02 §14:** `GET/POST /dashboards`, `GET/PUT/DELETE /dashboards/{id}`, 최대 50 위젯, PUT If-Match.
- **D05 §11 Dashboard 스키마**
  - spec: `version, title, description, variables[], layout[], widgets[]`
  - 위젯: `id, kind, query, title, unit, display, time_override`
  - x/y/w/h는 24열 grid의 정수이고, 겹침과 경계 초과를 검증한다.
  - kind는 `time_series, stat, table, histogram, top_list, text`다.
  - 변수는 environment·service·team이다.
- **D04 §01:** Viewer는 dashboard 조회, Developer는 생성·편집이다.

명세가 정하지 않은 것이 있다.
- 위젯 query의 모양
- 변수가 위젯 query에 어떻게 들어가는가
- display·time_override의 값
- 크기 한도
- 목록 응답 모양

화면(편집 흐름, autosave, publish)은 단계 B다.

## 결정

### 1. 정의(DashboardSpec, `internal/dashboard`)

- **정규형**
  - 저장 전에 검증하고, 기본값을 채운 정규형을 저장한다(monitor와 같은 방식, ADR 0049).
  - 모르는 필드는 400이다. 그 종류가 쓰지 않는 display 필드도 400이다(조용히 버리지 않는다).
  - 한도
    - 위젯 50(D02 §14), 변수 10
    - 제목 200자, 설명 2,000자, text 위젯 10,000자, 본문 256KiB
    - 제어 문자 금지(제목·설명·text는 줄바꿈·탭 허용, metric 이름·label·filter 값은 모두 금지, UTF-8)
  - 개수 상한(변수·위젯·배치)은 항목 검증보다 먼저 본다. 넘으면 오류 하나로 바로 거절한다. 배치 겹침 검사(쌍)를 50개 안으로 묶기 위해서다(리뷰 P1, [PS-0013](../troubleshooting/PS-0013-dashboard-layout-unbounded-validation.md)).
- **위젯 query**
  - query-api의 구조화 식(`POST /query/metrics`의 `expression`: metric, aggregation, and/eq filter, group_by ≤ 5)과 같다(ADR 0027).
  - 별도 query 언어를 만들지 않는다(D02 §13).
  - 저장은 query를 실행하지 않는다. 화면이 보는 사람의 권한으로 query-api를 부를 때 다시 인가된다(D05 §04 "링크 접근 시 권한 재검증").
- **위젯 종류**
  - `time_series`(display: stack)
  - `stat`(group_by 없음, decimals)
  - `table`(decimals, limit 1..100, sort)
  - `top_list`(group_by 필수, decimals, limit, sort)
  - `text`: query 없음. 글자 그대로 표시하고 HTML·markdown을 해석하지 않는다(CLAUDE.md "text-only 렌더링").
- **아직 지원하지 않는 것(422, 명세 차이 — 계약 7, 사용자에게 알림)**
  - `histogram` 위젯: bucket 분포를 돌려주는 조회 API가 없다. query-api 연산은 분위수·count·hist_sum 같은 요약만 준다. 분포 API와 함께 연다.
  - `team` 변수: 서비스 소유 팀(services.owner_team)이 아직 없어 거를 label이 없다. 팀 소유(F08)와 함께 연다.
  - D05 §11의 "6종 위젯, environment·service·team 변수" 중 둘이 빠진다. 명세(D05)를 고치지 않고 ADR로 기록한다. 해당 기능이 생기면 422를 푼다.
  - team 변수를 참조하는 filter 값(`$이름`)도 같은 422다(리뷰 반영 — 참조가 형식 오류 400으로 바뀌지 않게).
- **단계 A에 없는 것(명세 차이 — 계약 7, 사용자에게 알림)**
  - D05 §10 "마지막 published revision이 실제 공유 화면", D05 §11 "편집: autosave 초안, 명시적 publish": 서버에 draft가 없다. PUT이 곧 publish다. 초안은 단계 B 화면이 로컬(브라우저)에 둔다. 서버 draft가 필요해지면 열·표를 더한다(schema 변경).
  - D05 §11 "공유: 조직 내부 URL과 저장 revision": revision을 고정한 공유 URL이 없다(revision 이력을 두지 않는다, §2). 공유 URL은 지금 정의를 연다.
- **변수:** `{name, kind: environment|service, default}`. default가 null이면 "전체"다.
  - 위젯 filter의 eq 값이 `"$이름"`이면 그 변수다(Datadog template variable 방식). 없는 변수와 다른 종류의 field는 400이다. environment 변수는 `deployment.environment.name`에만, service 변수는 `service.name`에만 쓴다.
  - 변수를 쓰지 않는 위젯은 값을 직접 적는다(D05 §11 "전역 변수와 override 구분" — 화면이 `$` 여부로 배지를 그린다).
  - `$`로 시작하는 실제 label 값은 아직 쓸 수 없다(escape 없음).
- **time_override:** null 또는 `{range: 15m|1h|4h|1d|7d}`. web 조사 context 상대 범위와 같고, 최대 7일은 metric 조회 한도다.
- **layout:** 위젯마다 정확히 하나의 `{widget_id, x, y, w, h}`. 0 ≤ x < 24, 1 ≤ w ≤ 24, x + w ≤ 24, 0 ≤ y ≤ 1000, 1 ≤ h ≤ 48이고, 겹치면 400이다.

### 2. 저장(`dashboards`, migration 00012, `controldb.DashboardStore`)

- 열: D02 §11의 `tenant_id, id, spec, revision, deleted_at`에 다음을 더한다.
  - `title`(목록 정렬)
  - `widget_count`(목록 요약)
  - 만든·바꾼 사람과 시각
- RLS ENABLE+FORCE, `tenant_isolation`이다.
- 앱 role은 SELECT·INSERT와 정의 열 UPDATE만 갖는다. DELETE와 tenant·만든 사람 UPDATE는 없다(tombstone).
- 생성은 Idempotency-Key(monitor와 같은 `idempotency_keys`, 같은 본문 재생·다른 본문 409)다.
- 수정은 If-Match revision이 같을 때만 revision + 1이다. 다르면 412, 없으면 428이다.
- 삭제는 tombstone(revision + 1)이고, If-Match는 선택이다.
- 변경마다 operations 감사와 outbox(`dashboard.created|updated|deleted`)가 한 트랜잭션이다.
  - 감사 details에는 spec 본문(자유 입력 제목·filter 값·text)을 넣지 않는다. hash와 위젯 수만 둔다.
- **revision 이력은 두지 않는다.** monitor와 달리 평가가 특정 revision을 가리키지 않는다. 충돌 화면(D05 §10 "차이를 보여주고 duplicate 또는 재적용")은 서버 최신본과 로컬 편집본만 있으면 된다.

### 3. API(control-api)

- 경로는 D02 §14와 같다.
- 권한
  - 조회 `dashboards.read`(Viewer 이상), 쓰기 `dashboards.write`(Developer 이상). Security Auditor는 볼 수 없다(D04 §01).
  - environment 제한 key도 다룰 수 있다. 정의에는 telemetry가 없고, query는 보는 사람의 권한·environment 범위로 실행되기 때문이다(monitor 쓰기와 다르다, ADR 0051 §3).
    - 받아들인 위험(무결성): staging 제한 key가 조직 전체가 보는 production dashboard의 filter·변수 기본값을 바꿀 수 있다. MVP 권한은 조직 단위 role이라(D04 §01) 위반은 아니다. 감사(operations)로 누가 바꿨는지 남는다. resource별 environment·team scope(GA, F08)에서 다시 본다(재검토 조건).
- 응답
  - 목록은 요약(제목·위젯 수·revision·시각)이고 제목순, 서명 cursor, 한 page 기본 100·최대 200이다. monitor·policy 목록과 같은 한도다(ADR 0049). D02 §12의 일반 상한(최대 1,000)보다 작게 둔다 — 관리 목록 화면은 한 page를 모두 그린다.
  - 단건·쓰기 응답은 정규형 spec과 ETag다.
- 없는 dashboard, 다른 tenant의 dashboard, 삭제된 dashboard는 같은 404다.
- flag `MONTRACER_DASHBOARD_API_ENABLED=false`면 경로가 404다(rollback, 기본 켜짐).

## 외부 사례 근거 (2026-10-14 확인 범위: 공개 문서)

| 사례 | 내용 | 반영 |
|---|---|---|
| Datadog template variables(D01 [B03](https://docs.datadoghq.com/dashboards/template_variables/)) | dashboard 변수를 위젯 query 안에서 `$이름`으로 참조하고, 값을 고르면 참조한 위젯만 걸러진다 | §1 `"$이름"` 참조, 변수 미참조 위젯은 영향 없음 |
| Grafana dashboard JSON model([docs](https://grafana.com/docs/grafana/latest/dashboards/build-dashboards/view-dashboard-json-model/)) | dashboard 정의는 하나의 JSON이다. 위젯(panel)마다 `gridPos {x, y, w, h}`를 24열 grid에 둔다. 저장할 때 version으로 충돌을 검출한다 | §1 layout 24열, §2 revision·If-Match |
| Datadog dashboard API([docs](https://docs.datadoghq.com/api/latest/dashboards/)) | 위젯 정의에 layout(x·y·width·height)을 함께 둔다. 위젯 종류마다 정해진 정의 필드만 받는다 | §1 종류별 display 검증 |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 위젯 query | query-api 구조화 식 | 별도 query 문자열: D02 §13 "별도 query 언어 금지", AST + binding(계약 4)을 다시 만들어야 한다 |
| 변수 적용 | `"$이름"` 명시 참조 | 모든 위젯에 자동 적용: 다른 서비스를 비교하는 위젯이 몰래 걸러진다 |
| histogram·team | 422로 미룸 | 저장만 허용: 화면이 그릴 수 없는 위젯·거를 수 없는 변수가 쌓인다 |
| revision 이력 | 두지 않음 | monitor처럼 이력 표: 평가가 revision을 가리키지 않는 dashboard에는 쓰임이 없다(복원은 재검토 조건) |
| 목록 | 요약만 | 목록에 spec: 위젯 50개 × 200개를 매번 보낸다 |

## 결과

- **API:** `/api/v1/dashboards` CRUD가 생긴다. 화면(S10)은 단계 B다.
- **schema:** PostgreSQL migration 00012 `dashboards`(RLS, 최소 GRANT)
- **tenant 영향:** 모두 `WithTenant`이고 cross-tenant 404다(tests/isolation).
- **retention 영향:** 삭제는 tombstone이다. 삭제 원장(F09)이 생기면 대상에 등록한다.
- **requirements-registry:** F05는 `planned`로 둔다. registry 상태는 출시 성숙도(planned → experimental → beta → GA)이고, 화면(단계 B)과 Gate 증거 전이다(ADR 0049·0053과 같은 기준).
- **설정:** `MONTRACER_DASHBOARD_API_ENABLED`(control-api, 기본 true)

## Rollback

- `MONTRACER_DASHBOARD_API_ENABLED=false`로 API만 끈다. 정의는 남는다.
- migration 00012 down은 표를 지운다. 저장된 dashboard가 사라지므로, 쓰기 시작한 뒤에는 flag로 끄는 쪽을 쓴다.

## 재검토 조건

- 단계 B(S10 화면): 보기·편집 분리, autosave(로컬 draft), 충돌 비교, 변수 옵션(무권한 값 제거, D05 §11), 동일 query 합치기·병렬 6
- histogram 위젯(분포 조회 API), team 변수(F08 팀 소유) — 422 해제
- CSV export(별도 권한·10k행·formula injection, D05 §11), 서버 draft, revision 복원
- 다른 telemetry(log·trace) 위젯 query
- resource별 environment·team 쓰기 scope(F08): environment 제한 key의 dashboard 쓰기 범위
- `$`로 시작하는 실제 label 값 escape

## 증거

- `internal/dashboard/spec_test.go`
  - 정규형(기본값·공백 제거·text 원문 보존·정규형 왕복·빈 dashboard)
  - 거절 23종: 모르는 필드, 제목 없음·제어 문자, version, 종류, query 없음, 연산, 종류 밖 display, time_override, text+query, stat group_by, top_list group_by 없음, 없는 변수, 다른 종류 field의 변수, 변수 중복·이름 형식, 배치 누락·없는 위젯 배치·24열 초과·폭 0, 위젯 id 중복, 겹침, filter 깊이
  - 50 위젯 상한(50은 됨), 개수 상한을 넘는 본문은 오류 하나로 바로 거절(배치 5,000개, 변수 11개 — PS-0013), 겹침 오류 수 상한
  - metric 이름·group_by·filter field·값의 제어 문자 거절, team 변수 참조는 422
  - 아직 지원하지 않음만 있으면 422, 섞이면 400
  - 본문 크기
- `internal/controlapi/dashboards_test.go`
  - 생성(Idempotency-Key 필수, 정규형·위젯 수·ETag·Location, 재생)
  - 목록 요약(spec 없음), 단건 spec, 404, id 형식 400
  - 권한(Viewer 쓰기 403, Security Auditor 조회 403)
  - 수정 428/412, 삭제 412/204, 삭제 뒤 404
  - 400·422·413, flag off 404
  - cursor(다른 principal 400)
- `internal/controldb/dashboards_integration_test.go`(PostgreSQL)
  - Viewer 생성 403, 생성·재생·Idempotency 충돌, 제목순 목록·keyset page, Security Auditor 403, 단건
  - 낡은 revision 거절, 수정, Viewer 수정 403
  - 다른 tenant 조회·수정·삭제 404·목록 없음
  - 낡은 삭제 거절, tombstone, 이후 404
  - operations 감사·outbox 4건(감사에 spec 본문 없음)
  - 앱 role DELETE·만든 사람·tenant 변경 불가(42501)
- `tests/isolation` `TestDashboardsAcrossTenants`: B의 조회·수정·삭제는 없는 것과 같은 404(X-Tenant-ID 무시), 목록·응답에 A의 제목 없음
- PostgreSQL migration up → down → up(로컬)
- spec-reviewer: P1 1건(배치 수 상한 없이 겹침 O(n²) — PS-0013)·P2 7건(label 제어 문자, draft·revision 공유 명세 차이 기록, team 참조 422, environment 제한 key 쓰기 위험 수용, 목록 한도 근거, README, registry 상태) 반영

## 변경 이력

- 2026-10-19: 목록 limit 최대 200 → 1,000(D02 §12, ADR 0060 §2). If-Match가 없는 428의 code는 `INVALID_ARGUMENT`(field `If-Match`, ADR 0060 §3).
