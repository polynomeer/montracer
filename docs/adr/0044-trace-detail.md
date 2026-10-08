# ADR 0044: Trace 상세(S05) waterfall과 span 연결

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-07 "S05 Trace 상세 진행")
- Owner: FE
- 날짜: 2026-10-07 (제안·결정)
- 관련: F02, E04, D05 §03·§06·§11, D02 §07·§22, D03 §(critical path), ADR 0022, 0037, 0041, 0043

## 배경

D05 §06·§11은 S05를 다음처럼 정한다.

- waterfall: 행 28px, 서비스 색 띠, 이름·종류·상대 시작·소요시간·상태
- 시간축 zoom·pan·검색·오류만·critical path 보기
- 선택 span 패널: attributes/events/links/logs/DB/profile 탭
- 부모가 없는 span은 orphan 그룹에 둔다
- clock skew: 원본을 고치지 않고 경고한다
- 10,000 span: 가상화와 접힌 하위 트리
- 키보드: ↑↓ 이동, → 펼침, ← 접힘, Enter 상세, Esc 닫기
- log는 정확한 ID 연결(linked)과 서비스·시간 대체(related)를 구분한다

critical path에 대해서는 D05 §11과 D03이 "async·겹침을 고려한 추정, duration 단순 합 금지"만 정하고 알고리즘은 정하지 않는다.

데이터는 이미 있다.
- trace 단건 조회 `GET /traces/{id}`(ADR 0022): span 목록·완결성·사유·`last_updated_at`
- log 검색(ADR 0037): trace_id·span_id·service_id 조건

## 결정

1. **백엔드 변경 없이** web만 바꾼다. 범위는 URL의 `from`·`to`(S04 링크가 trace 시작 −1분 ~ 끝 +1분을 넣는다)이고, 없으면 조사 context 범위다. 단건 조회 한도는 7일이다.
2. **트리**
   - `parent_span_id`로 묶고 형제는 시작 순(동률은 span_id)으로 정렬한다.
   - 부모가 이 trace에 없으면 orphan 그룹에 둔다. 순환 parent 사슬도 무한 루프 없이 orphan으로 드러낸다.
   - **clock skew:** 자식이 부모 구간 밖이면 `skew`로 표시하고 원본 시각을 고치지 않는다.
   - **시각 계산:** epoch ns는 2^53을 넘어 숫자로 정확히 담을 수 없다. 그래서 상대 시각을 "ms 차이 + ms 안의 ns 차이"로 계산한다.
     - 소수부는 떼어 따로 읽는다(엔진마다 ms 아래 자릿수 처리가 다를 수 있다).
     - offset 형식(`+09:00`)도 받는다.
3. **critical path (Jaeger UI 방식의 추정)**
   - root(여럿이면 가장 늦게 끝난 것)에서 시작한다.
   - 현재 경계(처음엔 그 span의 끝)보다 먼저 끝난 자식 중 가장 늦게 끝난 자식을 따라간다. 그 자식의 시작이 새 경계가 된다.
   - 경계를 넘어 이어지는 겹친·병렬 자식은 경로에 들지 않는다. 부모 밖으로 끝나는 자식은 부모 끝으로 잘라 본다.
   - 화면에는 "추정"이라 쓰고 duration을 합하지 않는다.
4. **보기 필터**
   - 오류만·검색(이름·서비스, 대소문자 무시)·critical path는 맞는 span과 **그 조상**을 함께 보인다(맥락 유지).
   - 접힌 span의 자손은 숨긴다.
5. **가상화**
   - 행 높이가 28px로 고정이라 스크롤 위치로 보이는 행(+앞뒤 8행)만 그린다. 라이브러리는 쓰지 않는다.
   - 키보드 focus 행은 화면 밖이어도 그린다. 그래서 `aria-activedescendant`가 늘 있는 요소를 가리킨다.
   - 행마다 `aria-posinset`·`aria-setsize`(보이는 형제 기준)를 붙여 일부만 그려도 위치를 알린다.
   - 공유 링크·뒤로 가기로 연 선택 span으로 처음 한 번 스크롤한다.
   - 시험: 3,000 span에서 60행 미만을 그린다.
6. **접근성·키보드 (WAI-ARIA tree 패턴)**
   - waterfall은 `role="tree"`이고, 행은 `treeitem`(`aria-level`·`aria-expanded`·`aria-selected`)이다. focus는 `aria-activedescendant`로 표시한다.
   - ↑↓ Home End로 focus를 옮기고, →는 펼침 또는 첫 자식, ←는 접힘 또는 부모로 간다. **Enter가 상세를 연다**(화살표만으로는 열지 않는다). Esc는 닫는다.
   - 키는 tree 요소에서만 받는다. 그래서 검색 입력 안에서는 동작하지 않는다.
   - 행의 이름에는 서비스·이름·시간·오류·skew가 들어간다(screen reader).
7. **상세 패널**
   - 화면 옆 inline 패널이라 focus를 가두지 않는다. 닫으면 waterfall로 focus를 돌려준다.
   - 선택 span과 탭은 URL `entity`·`tab`에 남는다(뒤로 가기·공유 링크로 복원, ADR 0041 허용 키).
   - 값은 모두 텍스트로만 그린다(속성에 든 HTML은 실행하지 않는다, D05 §04).
   - **로그 탭**
     - **linked**: trace_id·span_id가 같은 log. ID가 정확히 같으면 시각이 어긋나도(비동기 log, host 시계 차이) 찾도록 **trace 조회 범위 전체**에서 찾는다.
     - **related**: 같은 서비스(service_id)의 log에서 linked를 뺀 것. 범위는 기본 **span 시작 ±30초**(D02 §07)다. 긴 span은 "범위 넓히기"로 시작 −30초 ~ 끝 +30초(log 범위 한도 24h 안)까지 넓힌다.
     - 두 목록을 나눠 보이고, related에는 "ID로 연결되지 않음"을 쓴다.
     - 각 목록은 최근 50개까지다.
       - 다음 page가 있거나 `meta.partial`이면 "더 있음/빠졌을 수 있음"을 알린다. 잘린 결과를 "log 없음"이라 하지 않는다(계약 6).
       - 실패하면 다시 시도할 수 있다.
   - **DB 탭:** `db.system(.name)`이 있을 때만 DB 속성과 쿼리 텍스트를 보인다. SQL literal은 수집 단계에서 제거되고 화면에서 복원하지 않는다.
   - **Profile 탭:** 수집 전(Phase G1)이라 이유를 보인다.
8. **trace 응답의 `meta.partial`**이 참이면 경고 배지를 보인다(지금 서버는 늘 false, ADR 0022). trace가 불완전하면 오류가 없어도 "받은 span 중 오류 없음"이라고 쓴다(받지 못한 span을 단정하지 않는다).
9. **찾지 못한 trace(404)**: 이유를 추측하지 않는다. "범위 밖·샘플링 미보존·보존 만료·권한 밖일 수 있음"과 request ID, 범위 넓히기 안내를 보인다(D05 §03 "확인할 수 없음"). 형식이 틀린 trace ID는 조회하지 않는다.

## 외부 사례 근거

| 사례 | 내용 | 반영 |
|---|---|---|
| Jaeger UI critical path ([jaegertracing/jaeger-ui#1582](https://github.com/jaegertracing/jaeger-ui/pull/1582)) | 가장 늦게 끝난 자식을 따라 거슬러 가는 critical path 계산, 겹친 자식은 늦은 쪽만 | 결정 3 |
| Jaeger UI ([jaegertracing.io/docs/latest/frontend-ui](https://www.jaegertracing.io/docs/latest/frontend-ui/)) | trace timeline(waterfall), span 접기, span 상세의 tags·process·logs | 결정 2·4·7 |
| WAI-ARIA APG Tree View ([w3.org/WAI/ARIA/apg/patterns/treeview](https://www.w3.org/WAI/ARIA/apg/patterns/treeview/)) | tree·treeitem 역할, 화살표·Home·End·Enter 동작, `aria-activedescendant` | 결정 6 |
| Grafana trace to logs ([grafana.com/docs/grafana/latest/explore/trace-integration](https://grafana.com/docs/grafana/latest/explore/trace-integration/)) | span에서 trace/span ID와 시간 범위(앞뒤 여유)로 log를 찾는다 | 결정 7 로그 |

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A. DOM 가상화 + ARIA tree (채택) | 같은 DOM이 접근성 트리를 겸함, 의존성 없음 | 행 높이 고정 필요(D05가 28px로 정함) |
| B. Canvas waterfall + 별도 표 대안 | 매우 큰 trace에서 빠름 | 대안 화면을 따로 만들어야 함(D05 §06 조건) |
| C. critical path = self time 큰 순 | 단순 | 병렬·async에서 틀림, D05 §11 위반 |
| D. 가상화 라이브러리 도입 | 검증된 구현 | 의존성·ADR 0013 갱신, 고정 높이라 이점 작음 |

## 결과

- 이점: S04 → S05 → span log까지 이어진다. 서비스(S02)에서 실패 trace와 그 log까지 3번 선택이다(D05 §05 합격 기준: S02 "오류 trace 보기" → S04 trace 선택 → S05 span 선택·로그 탭).
- 비용
  - 단건 조회는 span을 최대 10,000까지 한 번에 받는다(ADR 0022). 그보다 큰 trace는 `span_limit_reached`로 보인다.
  - related log는 서비스·시간 대체라 같은 시간대의 다른 요청 log도 섞인다. 화면에 "ID로 연결되지 않음"을 밝힌다.
- 영향 받는 계약: 없음(API 변경 없음). URL `entity`(span_id)·`tab`을 S05가 쓴다.

## Rollback

S05 라우트를 placeholder로 되돌리면 된다.

## 재검토 조건

- 10,000 span을 넘는 trace의 page 조회가 생길 때: 하위 트리 지연 로드
- profile 수집(Phase G1): Profile 탭의 정확 연결·시간 겹침 구분(D05 §06)
- DBM 화면: DB 탭에서 query digest로 이동
- span link가 다른 trace를 가리킬 때 그 trace의 시간을 알 수 있게 되면: 링크 이동

## 증거

- `apps/web/src/features/traces/waterfall.test.ts`: ns 정밀도(소수부 분리·offset 형식), posinset·setsize, 트리·orphan·skew(원본 유지)·순환, critical path(병렬은 늦은 쪽만, 순차는 모두), 접기, 오류만·검색(조상 포함, orphan도)
- `TraceDetail.test.tsx`
  - URL 범위로 조회, 헤더(불완전 사유·오류 수·skew)
  - log 잘림 안내("없음" 대신 "더 있음")·재시도, related 범위 넓히기(시작 ±30초 → 끝 +30초)
  - 트리 순서·orphan 그룹·보기 전환
  - 키보드(↓ 이동, ← 접기, Enter 상세, Esc 닫기, URL 복원)
  - 속성 HTML 미실행, 이벤트, DB, linked·related log 분리와 요청 조건(span 앞뒤 30초)
  - 404 안내, 형식 오류 미조회, 3,000 span 가상화
- 수동: mock API로 S04 → S05 이동, waterfall·skew·orphan·오류 bar, DB 탭, linked·related log 확인(콘솔 오류 없음)
