# ADR 0045: Logs 화면(S08)과 trace ↔ log 이동

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-08 "다음 단계를 진행해줘")
- Owner: FE
- 날짜: 2026-10-08 (제안·결정)
- 관련: F04, E04, D05 §03·§04·§08·§11·§12, D02 §09·§19, ADR 0022, 0037, 0039, 0041, 0043, 0044

## 배경

D05 §08은 Logs를 이렇게 정한다.

- 시간·severity·service·message 표와 attribute drawer
- live 모드는 명시적 시작/중지, 새 항목 수와 gap banner, 과거 행을 보는 동안 자동 scroll 정지
- 정제 전 데이터와 숨겨진 field는 DOM에 넣지 않는다

D05 §12는 10,000 log 행의 가상화를, D05 §11은 "서비스 → 대표 trace → span 관련 log" 이동을 요구한다. 작업계획서 Sprint S6의 데모는 "오류 trace → 관련 log (3단계 이하)"이다.

데이터는 이미 있다.
- log 검색 `POST /query/logs`(ADR 0037): `service_id`·`severity_number`·`trace_id`·`span_id`·`body` contains·`attributes.<key>` 조건
  - cursor, 누적 10,000행, 24시간 범위
  - `service.name`·environment 범위는 ADR 0039
- 서비스 catalog `GET /services`(ADR 0038)

log stream(live) API는 아직 없다.

## 결정

1. **백엔드 변경 없이** web만 바꾼다. 경로는 `/o/{org}/logs`이고 범위는 조사 context다. 24시간을 넘으면 조회하지 않고 줄이라고 안내한다.
   - context의 environment는 서비스 선택지에만 쓴다. 검색 조건에는 넣지 않고, 고르면 "아직 적용되지 않음"을 보인다(결과 절).
2. **조건과 URL**
   - 조건: 서비스(`service`, catalog에서 고름 → `service_id eq`), 최소 심각도(`sev` = `trace`…`fatal` → `severity_number gte`), trace ID(`trace`, hex 32 → `trace_id eq`), 본문 포함(`q` → `body contains`, 대소문자 무시)
   - 공유 링크에는 형식이 정해진 `service`·`sev`·`trace`만 남는다. 자유 입력인 본문 검색어 `q`는 빠진다(ADR 0041·0043과 같은 규칙).
   - 입력은 300ms 뒤 또는 Enter·조회에 반영한다(D05 §03). 잘못된 trace ID는 반영하지 않고 입력 아래에 이유를 보인다.
   - URL의 서비스가 선택지에 없으면(다른 environment, catalog 미등록, 로딩 중) "목록에 없는 서비스 xxxxxxxx…" 선택지로 보인다. 실제로 걸린 조건을 "전체"로 숨기지 않는다(리뷰 반영, S04도 같다).
   - **본문 검색어 `q`는 주소창 URL에 남는다(수용한 위험, 리뷰 지적).**
     - 뒤로 가기·새로고침으로 조건을 복원하려면(D05 §11) URL이 원천이어야 한다(ADR 0041).
     - 공유 링크에서는 빠진다. 하지만 브라우저 history와, 새로고침 시 web을 서빙하는 서버·ingress의 access log에는 남을 수 있다. log 본문 검색어에는 이메일·사용자 ID가 들어가기 쉽다.
     - 그래서 web 서빙 경로의 access log는 query string을 남기지 않아야 한다. 배포 구성(Helm·ingress)을 만들 때 확인한다(재검토 조건).
   - 최소 심각도를 고르면 "심각도가 미지정인 log는 빠집니다"를 보인다. `severity_number=0`은 OTel에서 "미지정"이라 어느 등급에도 들지 않는다.
3. **심각도 표시**
   - OTel SeverityNumber 구간을 따른다: 1~4 TRACE, 5~8 DEBUG, 9~12 INFO, 13~16 WARN, 17~20 ERROR, 21~24 FATAL.
   - 0이나 범위 밖은 이름을 지어내지 않고 "미지정"으로 쓴다(계약 6). 오류(ERROR·FATAL)는 색과 문자로 함께 보인다.
4. **결과 표**
   - 열은 시각·심각도·서비스·본문·trace다. 최신순(동률은 event ID, ADR 0037)이며 100개씩 cursor로 이어 붙인다.
   - 서비스 이름은 catalog에서 찾는다. catalog에 없는 service_id는 이름을 추측하지 않고 ID 앞부분을 보인다.
   - 본문은 한 줄로 줄여 보이고, 전체는 속성 패널에서 본다. 모든 값은 텍스트로만 그린다(본문에 든 HTML은 실행하지 않는다, D05 §04).
   - **가상화:** 고정 32px 행이라 스크롤 위치로 보이는 행(+앞뒤 8행)만 그린다. ADR 0044와 같이 라이브러리는 쓰지 않는다.
     - 선택 행과 키보드 focus 행은 화면 밖이어도 그린다. 그래서 focus를 잃지 않는다.
     - `role="table"`에 `aria-rowcount`, 행마다 `aria-rowindex`를 붙여 일부만 그려도 위치를 알린다. 다음 page가 있으면 전체 수를 모르므로 `aria-rowcount=-1`이다.
     - 선택 행은 본문 버튼의 `aria-current`로 알린다. table row에는 `aria-selected`를 쓸 수 없다(ARIA 1.2, 리뷰 반영).
     - catalog에 없는 서비스는 화면에 ID 앞부분만 보이고, 전체 ID는 screen reader용 텍스트로 둔다(D05 §03 "accessible name은 원문 보존").
   - **상태:**
     - 다음 page가 있으면 "N개 이상"이라고 쓴다.
     - 어느 page든 `meta.partial`이나 `failed_shards`가 있으면 "빠진 log가 있을 수 있음"을 보이고 빈 결과 문구를 쓰지 않는다(계약 6).
     - 빈 결과는 조건이 있을 때와 없을 때를 구분한다.
     - 누적 10,000행이면 조건을 좁히라고 안내한다.
     - S04도 같은 page 누적 hook(`useSearchPages`)을 쓰므로 partial 경고를 함께 얻는다.
5. **속성 패널 (D05 §03 EntityDrawer)**
   - 행의 본문 버튼으로 연다. 선택은 URL `entity`(event_id)에 남는다.
   - 화면 옆 inline 패널이라 focus를 가두지 않는다. Esc·닫기로 닫고 그 행으로 focus를 돌려준다.
   - 시각·심각도(이름과 숫자)·서비스, 본문 전체(줄바꿈 유지), trace_id·span_id·service_id·event_id, 속성을 보인다.
   - 서버가 준 field만 그린다. 정제 전 값은 수집 단계에서 제거되어 받지 않는다(D05 §08).
   - 공유 링크의 `entity`가 불러온 결과에 없으면 추측하지 않고 "불러온 결과에 이 log가 없습니다"라고 쓴다.
   - 넓은 화면(1280px 이상)에서는 조건·결과·패널을 한 줄에 둔다. 이때 trace 열은 패널에 있으므로 표에서 빼 본문 폭을 확보한다. 좁으면 패널을 아래로 내린다.
6. **trace ↔ log 이동**
   - **log → trace(S05):** trace_id가 있는 log는 S05로 간다.
     - 범위는 log 시각 1시간 전 ~ 10분 뒤다. log 시각만 알고 trace의 시작·끝은 모른다. log는 보통 span 안에서 남으므로 앞쪽을 넉넉히 둔다. 단건 조회 한도(7일, ADR 0022) 안이다.
     - 끝은 지금 + 4분을 넘지 않는다. 조사 context는 5분 넘게 미래인 끝 시각을 거절한다(커밋 전 수동 확인에서 발견). 1분 여유는 log 시각이 브라우저 시계보다 앞설 때(수집은 5분까지 허용)를 위한 것이다.
     - span_id가 있으면 그 span을 선택하고 로그 탭을 연다(`entity`·`tab`).
   - **trace → log:** S05 로그 탭에 "Logs에서 이 trace의 log 모두 보기"를 둔다. trace 조회 범위(24시간을 넘으면 앞쪽 24시간)와 `trace` 조건으로 간다.
   - **서비스 → log:** S02 리소스 카드에 "이 서비스의 오류 log"(`service` + `sev=error`)를 둔다.
7. **live 모드는 미룬다.** stream API(D02 §13)가 없어서다. 주기적 재조회로 흉내 내면 D05 §08의 gap banner·새 항목 수를 정확히 보일 수 없다. 버튼을 두지 않고 문서에 남긴다.

## 외부 사례 근거 (2026-10-08 확인)

| 사례 | 내용 | 반영 |
|---|---|---|
| OpenTelemetry Logs Data Model ([SeverityNumber](https://opentelemetry.io/docs/specs/otel/logs/data-model/#field-severitynumber)) | 1~4 TRACE … 21~24 FATAL, 0은 미지정 값 | 결정 2·3 |
| Datadog Log Explorer side panel ([docs](https://docs.datadoghq.com/logs/explorer/side_panel/)) | 행을 열면 메시지와 구조화 속성, trace ID가 있으면 trace로 이동하고 그 ID로 log를 거른다. trace가 샘플링으로 없을 수 있다고 밝힌다 | 결정 5·6 |
| Grafana Explore logs ([logs integration](https://grafana.com/docs/grafana/latest/explore/logs-integration/)) | log 행을 누르면 field·link 상세(inline 또는 sidebar), level 값으로 색을 정하고 level이 없으면 표시하지 않는다 | 결정 3·5 |
| Datadog Live Tail ([docs](https://docs.datadoghq.com/logs/explorer/live_tail/)) | live는 시간 범위 선택의 별도 옵션이고, 많으면 표본만 보인다 | 결정 7(stream API와 함께 별도 모드로) |

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A. 고정 높이 DOM 가상화 + ARIA table (채택) | ADR 0044와 같은 방식, 의존성 없음 | 본문을 한 줄로 줄여야 함(전체는 패널) |
| B. 가변 높이 행(본문 전체 표시) | 한눈에 여러 줄 본문 | 측정·재배치 필요, 10,000행에서 느림 |
| C. 폴링으로 live 흉내 | 바로 만들 수 있음 | gap·새 항목 수가 부정확(D05 §08 위반 위험) |
| D. 속성 facet(top-N) | D05 §03 FacetPanel | 집계 API가 없음(D02 facet 경로와 함께) |

## 결과

- 이점
  - 서비스(S02) → 오류 log → trace(S05)의 span 로그 탭까지 3번 선택으로 간다.
  - 반대로 trace(S05) → 그 trace의 log 전체(S08)도 1번이다.
- 비용
  - log 행에 environment가 없고 `service.environment` 필터도 아직 없다(ADR 0039 §4). 그래서 조사 context의 environment는 서비스 선택지(catalog)에만 쓰이고 검색 조건에는 들어가지 않는다(S04와 같다). environment를 고르면 화면에 그렇게 안내한다. environment로 제한된 key는 서버가 mandatory predicate로 막는다(ADR 0039 §2).
  - log → trace 범위는 추정이다. log보다 1시간 넘게 먼저 시작한 trace는 S05에서 404이고, S05가 범위를 넓히라고 안내한다(ADR 0044 결정 9).
- 영향 받는 계약: 없음(API 변경 없음). URL 키 `sev`·`trace`를 공유 허용 키에 더했다.

## Rollback

`logs` 라우트를 placeholder로 되돌리고 S02·S05의 링크를 뺀다.

## 재검토 조건

- log stream API: live 모드(명시적 시작/중지, gap banner, "새 로그 N개")
- facet 집계 경로(D02): 서비스·심각도·속성 top-N FacetPanel
- 저장 검색(F04): 조건 저장·공유
- `service.environment` 필터(ADR 0039 §4): environment context를 서버 조건으로
- web 배포 구성(Helm·ingress): access log에 query string(`q`)을 남기지 않는지 확인

## 증거

- `apps/web/src/features/logs/logFilters.test.ts`: 심각도 구간·미지정, URL 왕복·잘못된 값 무시·trace ID 소문자화, filter AST, 공유 링크(본문 검색어 제외), S05 링크 범위(지금으로 자름)·span 선택
- `LogExplorer.test.tsx`
  - URL 조건 → 요청 본문, 행 표시(모르는 서비스는 ID)
  - 본문 HTML 미실행, trace 링크(범위·span·로그 탭)
  - 속성 패널(URL entity, Esc로 닫고 행으로 focus), 결과에 없는 entity
  - cursor 이어 붙이기, 빈 결과(조건 유무), partial(빈 결과 문구 없음), 24시간 초과 미조회
  - 입력(잘못된 trace ID 미반영, 조회 즉시 반영, 심각도 안내)
  - 3,000행 가상화(60행 미만), 화면 밖 선택 행, 스크롤
- `TraceDetail.test.tsx`: S05 → S08 링크(trace 범위·trace 조건)
- `ServiceDetail.test.tsx`: S02 → S08 링크(서비스·ERROR 이상)
- spec-reviewer: P0·P1 없음. P2 7건 중 6건 반영(table row의 aria-selected, 행 수 모름, 줄인 서비스 ID의 accessible name, 선택지에 없는 서비스, log 시각이 앞설 때 끝 시각, `q`의 URL 위험 문서화). 미래 끝 시각 버그는 커밋 전에 고쳐 계약 결함이 아니므로 PS를 쓰지 않았다(재발 방지 시험은 `logFilters.test.ts`).
- 수동: mock API로 S08 표·속성 패널(1440px 한 줄 배치), S08 → S05(span 로그 탭) → S08 이동 확인
