# 디자인 시안 적용 계획

- 작성: 2026-10-22
- 대상 시안: "Montracer 디자인 시안" 캔버스, 웹 화면 S01~S14와 디자인 기초. 시안 원본은 claude.ai Design artifact이고, 이 문서에는 판단에 필요한 내용만 옮겼다.
- 상위 계획: [작업계획서](work-plan.md) §5.2 E04·E05, §5.3~5.4

## 1. 목적과 범위

**목적:** 디자인 시안을 웹 앱(`apps/web`)에 적용한다. 다만 시안은 결정이 아니라 참고안이다. 명세(D01~D06)나 ADR과 충돌하면 명세·ADR을 따르고, 시안을 고친다(§5).

**범위에 넣는 것**
- 디자인 기초: 토큰, StatusBadge, 빈 상태, 확인 대화상자, 주 메뉴.
- 이미 구현된 화면의 시각·배치 정합: S01·S02·S03·S04·S05·S08·S09·S10.
- 백엔드가 이미 있는 새 화면: 감사(Audit).

**범위에서 빼는 것과 이유**

| 항목 | 이유 | 다룰 곳 |
|---|---|---|
| iOS 앱 시안 21개 | D01에 네이티브 콘솔 앱 근거가 없다(§06은 네이티브 모바일 RUM만 후속으로 둠). 저장소에 앱 코드 없음 | 범위 결정(D01 개정 + ADR)이 먼저 필요 |
| 백엔드가 없는 화면 (Errors, JVM, Agents, 사용량, 데이터 삭제, Profiles, DBM, RUM, Synthetics, SLO·사건) | 가짜 데이터로 화면을 만들지 않는다(계약 6). 각 기능의 단계(M0~G2)에 맞춰 기능 작업으로 만든다 | §4 단계 D4 |
| 백엔드 기능이 필요한 시안 요소 (배포 표시, 1주 전 비교의 일부, 집계·facet 수, exemplar 등) | 화면 정합이 아니라 기능 작업이다 | §4 단계 D3 목록 |

## 2. 원칙

1. **결측을 정상으로 보이지 않는다.** missing·stale·partial·NO_DATA·EVALUATION_ERROR·표본은 시안보다 구현의 표현을 우선한다(계약 6).
2. **구현에 있고 시안에 없는 것은 남긴다.** 상태 계약, 접근성, 412·멱등성 처리, 잘림 안내, request_id가 그런 예다. 각 화면 비교의 "(c) 유지" 목록을 따른다.
3. **데이터가 없는 시안 요소는 만들지 않는다.** 빈 자리를 지어낸 값이나 숫자 0으로 채우지 않는다. API가 생기면 그때 붙인다.
4. **화면마다 PR 하나.** 각 PR은 시각 확인(mock server), 단위 시험, spec-reviewer를 거친다. 명세가 정하지 않은 선택은 ADR로 남긴다.

## 3. 현황

비교 결과(2026-10-22, main `b391fd1`)를 요약했다. (a)는 기존 데이터로 맞출 수 있는 차이, (b)는 API가 필요한 차이다.

| 화면 | 구현 | (a) 정합 규모 | (b)의 주요 의존 |
|---|---|---|---|
| 디자인 기초·주 메뉴 | 부분 | M | thread dump 확인 대화상자는 진단 API(F16) |
| S01 Overview | 있음 | M | SLO(F11), 변경 이벤트, monitor↔서비스 연결, `state_since` |
| S02 서비스 상세 | 있음 | L | monitor↔서비스 연결, 인스턴스·런타임·버전, 배포 |
| S03 Service Map | 있음 | M | monitor↔서비스 연결, cache·browser 노드 종류 |
| S04 Trace Explorer | 있음 | M | facet·건수 집계, duration 정렬, resource 속성 저장, 보존율 |
| S05 Trace 상세 | 있음 | M | 보존 사유, log 건수, profile·DBM |
| S08 Logs | 있음 | M | live tail, 건수·히스토그램, 수집 공백 구간, resource 속성 |
| S08 Metrics | 있음 | M (prefill 포함 시 L) | exemplar, metric 설명·bucket·원본 해상도 |
| S09 Monitor | 있음 | M | dry-run 값 시계열, 평가 값, Slack·PagerDuty·확인·사건(F11) |
| S10 Dashboard | 있음 | S~M | 다중 query 위젯, 비율 집계, SLO, 팀 폴더(F08) |
| 감사(Audit) | 자리표시 | M (새 화면) | 없음: `GET /api/v1/audit-events` 있음(ADR 0034) |
| 나머지 새 화면 | 자리표시 | — | 기능 백엔드(§4 D4) |

## 4. 단계

### D1. 디자인 기초 (먼저 하는 이유: 모든 화면이 쓴다)

- **StatusBadge 변형:** PENDING, RECOVERING, NO_DATA(점선), EVALUATION_ERROR(흰 배경·실선 테두리·삼각형), MUTED 꼬리표, stale(시계), partial(파이), 표본, 추정(점선), provisional을 추가한다. 시안 규격은 높이 26px, 좌우 10px다. 지금은 "판정 없음"과 NO_DATA가 같은 점선 원으로 보이는데, 이를 분리한다.
- **EmptyState 공용 컴포넌트:** 지금 화면마다 손으로 만든 빈 상태 약 20곳을 옮긴다. 이유 종류(새 설치, 조건 불일치, 보존 만료, 미지원 계측, 준비 중인 기능, 권한 없음, 꺼짐, 원인 확인 필요)와 버튼을 갖춘다. request_id는 응답에 있는 곳에서 넘기고, 검색 화면은 결과 아래 request_id 줄을 그대로 쓴다.
- **ConfirmDialog 공용 컴포넌트:** 제목, 영향(`dl`), 사유 입력, 취소·확인 버튼을 갖춘다. monitor 삭제(MonitorDetail)와 policy 삭제(PolicyDetail)를 옮긴다.
- **주 메뉴:** 순서는 D05 §01·ADR 0041대로 둔다(Admin: Team → Access → Data Controls → Usage → Audit). 시안의 순서와 "Team · Access" 합치기는 시안 쪽을 고친다(§5). Notifications는 남긴다(ADR 0056).
- **토큰:**
  - 메뉴 선택 배경은 시안 값 `#E8EFFD`와 토큰 `#EEF3FD`가 다르다. 메뉴 전용 토큰을 두지 않고 `selectionBg`를 유지한다. 대비 검사 결과에 따라 정한다.
  - dark critical 테두리는 시안 안에서도 `#5A2A31`과 `#6B2F38`이 섞여 있다. 토큰(`#6B2F38`)을 유지한다.
- **글꼴:** 시안은 Noto Sans KR, 구현은 Pretendard 번들이다(ADR 0041). D05가 둘 다 허용하므로 바꾸지 않는다.

### D2. 구현된 화면의 시각 정합 (화면마다 PR 하나, 순서는 조사 흐름 순)

1. **S02 서비스 상세:**
   - 머리 메타 한 줄, RED 카드(큰 값과 단위, x축 눈금).
   - p95 카드에서 "이 구간 느린 trace" 링크(`min_ms`).
   - 자원 표: 총 시간 비율 막대, endpoint 필터, 행 선택.
   - 의존성 패널·탭: 이미 있는 `POST /service-map/query`를 쓰고, 표본 배지를 붙인다.
   - "1주 전 비교"는 범위를 옮긴 조회로 하되, 공용 비교 control은 D2-7에서 함께 정한다.
2. **S04 Trace Explorer:** 조건 chip 막대, scatter 축 눈금 5개와 선택 범위 표시, 촘촘한 행 보기. 완전성 문구는 이미 있다. checkbox facet(서비스 여러 개·버전·Pod)은 URL 조건 변경·resource 속성 저장이 필요해 D3로 옮겼다(ADR 0043 변경 이력).
3. **S05 Trace 상세:**
   - 머리: trace ID를 제목으로, 메타 한 줄.
   - partial·clock skew 칩.
   - 모두 펼치기, 표 보기.
   - 범례와 키보드 안내.
   - drawer의 "연결된 근거"(최대 50이면 "50+").
4. **S08 Logs:**
   - 조건 chip, checkbox facet, 수준별 색.
   - drawer: 머리, linked 배지, redaction 설명, "Trace 열기"·"전후 30초 로그" 버튼.
5. **S08 Metrics:**
   - 사전에 유형·단위·temporality를 보이고 건수 머리를 단다.
   - 정의 패널을 옮긴다.
   - "허용하지 않는 집계" 패널.
   - 그래프 안에 "데이터 없음"·"잠정" 표시를 넣는다. 범례의 마지막 값은 확정 점 기준으로 하거나 "잠정"을 붙인다.
6. **S03 Service Map:**
   - 노드 종류별 모양·아이콘·system 부제, 간선 숫자에 표본 표시.
   - 머리의 노드 수 표시, 검색 위치.
   - drawer에 RED 요약과 "이 경로의 trace" 링크.
7. **S01 Overview:**
   - 서비스 수, 소유 열, "Service Map으로 보기".
   - 30초 자동 갱신과 데이터 기준 시각.
   - monitor 상태 타일과 주의가 필요한 monitor 목록(`listMonitors`).
   - 낡은 안내("monitor가 아직 없어…")를 바꾼다.
   - 공용 "1주 전 비교" control.
8. **S09 Monitor:**
   - 세로 단계 표시와 단계별 요약.
   - 머리의 상태·revision.
   - "이렇게 동작합니다" 설명 상자, dry-run 요약(후보 구간 수·coverage).
   - 편집 화면 안에 사건 timeline.
   - 여러 단계를 한 화면에 두고 실시간 validate는 마지막에 한다.
9. **S10 Dashboard:**
   - 머리: revision·작성자, 다음 갱신 시각, 일시정지, 링크 복사.
   - `$이름 = 값` chip.
   - query pool 줄(위젯 수·query 수·병합·병렬).
   - 위젯 부제(종류·단위)와 갱신 시각, stale 실패 횟수.
   - override 배지, "bucket 병합 · 평균 아님" 표기.

### D3. 화면 정합을 막는 백엔드 항목 (기능 작업으로 넘김. 여기서는 목록만 관리)

| 항목 | 풀리는 시안 요소 | 담당 기능 |
|---|---|---|
| monitor↔서비스 연결 + `MonitorStatus.state_since` | S01 판정 열·"판정 없음" 수·시작 시각, S02 머리 경보·임계선, S03 노드 상태 | F06 후속 (가장 많이 풀림) |
| trace·log 건수·facet 집계, duration 정렬, 서비스 여러 개 조건 | S04·S08 facet 수, 총 건수, 히스토그램 | F04 |
| resource 속성 저장(`service.version`, `k8s.pod.name`) | 버전·Pod facet, 서비스 버전·인스턴스 | F01·F03 |
| 배포 이벤트 조회 | 배포 표시선·배너, 변경 목록 | 명세 확인 필요 (`deployments.write` scope만 있음) |
| dry-run 값 시계열, 평가 값·요청 수 | S09 그래프·평가 행 | F06 |
| exemplar, metric 설명·bucket | S08 Metrics | F03·F04 |
| 수집 공백 구간·지연 | S08 Logs 공백 행·배너 | F01 품질 |
| SLO·사건·확인·mute·escalation, Slack·PagerDuty | S01 SLO 표, S09 timeline, S10 예산 | F11 (G1) |
| 다중 query 위젯, 비율 집계 | S10 | F05 |

### D4. 새 화면 (기능 단계 순서. 각 기능의 API와 함께 만든다)

1. **감사(Audit):** API가 있으므로 지금 만든다. G1 운영 준비 항목 "감사 조회 UI"다.
2. **M0:**
   - 사용량: F07의 일별 수집·quota 보기. 일별 집계 저장과 API가 먼저 필요하다.
   - 설치 검증: 받음·저장·검색 가능을 구분한다.
3. **G1:**
   - Errors(F14): worker에서 묶음 표를 먼저 만든다.
   - SLO·사건(F11).
   - 데이터 삭제(F09): 계약이 무거우니 백엔드 설계를 먼저 한다.
   - Agents·JVM(F13): Java agent가 선행이다.
   - 가동 점검: ADR 0071(제안, polynomeer/montracer#59)이 승인되면 G1.
4. **G2:** Profiles, DBM, RUM, Synthetics. 그 전까지 메뉴는 지금처럼 entitlement로 숨긴다.

## 5. 시안을 고쳐야 하는 곳 (명세·ADR과 충돌)

D2를 하면서 화면은 명세를 따르고, 캔버스도 함께 고친다.

| 시안 | 충돌 | 바로잡을 방향 |
|---|---|---|
| S10 text 위젯 "markdown" | ADR 0057·0058, CLAUDE.md: 글자 그대로 렌더링 | 글자 그대로 |
| S10 histogram 위젯, `$team` 변수, "revision 기록" | ADR 0057: 422, 이력 없음 | 시안에서 빼거나 "예정"으로 표시 |
| S09 "시도 1/5" | ADR 0053: 24시간 backoff, 횟수 상한 없음 | "다음 재시도 시각 · 24시간까지" |
| S09 "http.server.errors ÷ requests" | ADR 0049: `error_ratio`는 HTTP histogram의 5xx 비율 | ADR 0049 표현 |
| S04 "정상" 범례·facet | 오류 span이 없다고 정상은 아님 | "오류 span 없음" |
| S05 "재시도 후 성공" (partial trace) | 확인할 수 없는 성공 | "성공" 삭제 |
| S03 orders-db 초록 점, "오류율 ≥ 1%" 범례 | DB에는 monitor가 없음. 임계값은 지어낸 값(ADR 0063) | "판정 없음", 오류 있음/없음 |
| S03·S02 간선 숫자 | 표본 trace 기반 | "표본" 표시 |
| S02 "계측 정상 · 11/12 pod" | stale pod이 있는데 정상이라고 함 | partial·stale 배지 |
| S08 Metrics 범례 마지막 값 | 잠정 구간의 값 | 확정 점 또는 "잠정" 표시 |
| 주 메뉴 Admin 순서·"Team · Access" 합침 | D05 §01·ADR 0041: Team → Access → Data Controls → Usage → Audit, 화면이 따로 있음 | 명세 순서·두 항목 |

## 6. 진행 기록

| 단계 | 상태 | PR |
|---|---|---|
| D1 디자인 기초 | 머지 | polynomeer/montracer#75 |
| D2-1 S02 서비스 상세 | 머지 | polynomeer/montracer#76 |
| D2-2 S04 Trace Explorer | 머지 | polynomeer/montracer#77 |
| D2-3 S05 Trace 상세 | 머지 | polynomeer/montracer#78 |
| D2-4 S08 Logs | 머지 | polynomeer/montracer#79 |
| D2-5 S08 Metrics | 머지 | polynomeer/montracer#80 |
| D2-6 S03 Service Map | 완료 (spec-reviewer 반영) | — |
| D2-7~9 | 대기 | — |
| D4-1 감사 화면 | 대기 | — |
| §5 캔버스 수정 | 대기 | — |
