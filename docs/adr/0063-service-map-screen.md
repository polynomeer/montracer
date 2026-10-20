# ADR 0063: S03 Service Map 화면 — 층 배치·정규화 굵기·오류 유무 색·표 대안 (S03 단계 B)

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-20 "merge하고 이어서 진행해줘", ADR 0062 후속)
- Owner: Frontend lead
- 날짜: 2026-10-20 (제안·결정)
- 관련: F03, E04 · D05 §01(전역 context·공유 URL), §02(토큰·반응형), §03(상태 계약), §07(Map 동작) · 계약 6 · ADR 0041, 0042, 0058, 0062

## 배경

ADR 0062가 `POST /service-map/query`(node·edge, 표본 span 기반, 1분 집계)를 만들었다. D05 §07이 화면에 정한 것은 다음과 같다.

- **그리기:** node는 종류(service·database·queue·external)와 상태 icon을 갖는다. edge 굵기는 requests/s, 색은 오류 상태이고, latency는 label이나 focus detail로 보인다. 범례로 "조회 범위 내 정규화"를 밝힌다.
- **상한:** 기본 200 node까지 그리고, 넘으면 숨긴 수를 보인다.
- **조작:** 선택하면 1hop을 강조하고 drawer를 연다. 두 번 클릭하면 서비스 상세다. pan/zoom, 검색, 키보드 이동, 표 보기, reset layout이 있다.
- **갱신:** 자동 refresh가 사용자가 옮긴 node 배치를 무조건 초기화하지 않는다.
- **추정:** 추정 edge에 정확한 호출 수를 붙이지 않는다.

명세가 정하지 않은 것:
- 배치 알고리즘과 graph library 사용 여부
- 오류 색의 기준
- 굵기 함수
- 갱신 주기
- 선택 상태의 URL 표현
- 상태 icon의 원천

## 결정

### 1. 배치 — 왼쪽(호출하는 쪽) → 오른쪽(불리는 쪽) 층 배치, 직접 구현

- **층:** 들어오는 edge가 없는 node가 0층이다. 각 node는 앞 node들의 가장 깊은 층 + 1이다.
  - 순환은 남은 node 중 아직 놓지 않은 앞 node가 가장 적은 하나를 먼저 놓아서 끊는다.
  - DB·큐·외부 node는 자연히 오른쪽 끝에 온다.
- **층 안 순서:** 이름순에서 시작해, 앞 층 이웃 위치의 평균(barycenter)으로 두 번 고친다.
- **결정적이다.** 같은 응답이면 같은 좌표라서 1분 자동 갱신에도 그림이 흔들리지 않는다. force layout처럼 매번 다른 그림이 나오지 않는다.
- 사용자가 끈 node 위치는 화면 상태(node id → 좌표)에 남는다. 갱신 뒤에도 유지하고, "배치 초기화"로만 되돌린다(D05 §07).
- graph library(dagre·cytoscape·d3-force)는 쓰지 않는다. 층 배치는 백 줄 남짓이고 단위 시험으로 고정할 수 있다. 의존성·번들·CSP 검토를 늘리지 않는다.

### 2. 굵기는 화면 안 정규화, 색은 "오류 있음/없음", 추정은 점선

- **굵기:** 1~6px, `1 + 5·√(요청 수 / 이 화면의 최대 요청 수)`. 범례에 "이 화면의 최대값 기준 정규화"라고 적는다(D05 §07).
- **색:** 오류가 하나라도 있으면 오류 색이다.
  - SLO·monitor가 아직 없으니 "1%면 경고" 같은 임계치를 지어내지 않는다. Overview(ADR 0048)와 같은 원칙이다.
  - 오류율 값은 drawer와 표에서 본다.
- **추정 edge:** 점선, 1px, 오류 색 없음이다. 수는 "추정 — 수 없음"으로 적는다(API가 null을 준다, 계약 6).
- 색만으로 구분하지 않는다. node는 종류 글자(서비스·DB·큐·외부)를 함께 쓰고, 외부 node는 점선 테두리다.
- **상태 icon(D05 §07):** 서비스별 monitor·SLO 상태 원천이 생기면 붙인다. 지금은 지어낸 상태를 그리지 않는다.

### 3. 선택·이동·키보드

- **선택:**
  - node를 클릭하거나 Enter를 누르면 선택된다. 선택 node와 위·아래 1hop만 진하게 보이고 나머지는 흐려진다.
  - 옆 drawer에 들어오는 호출과 나가는 호출(요청/s·오류율·p95·큐 대기·마지막 관측)을 보인다. 서비스 상세와 이 서비스의 trace로 가는 link가 있다.
- **서비스 상세:** 서비스 node를 두 번 클릭하면 서비스 상세(S02)로 간다. S02에는 "Service Map에서 보기" link를 더한다.
- **이동·확대:**
  - 배경을 끌면 이동하고, node를 끌면 위치가 바뀐다(4px 이상 움직여야 끌기다).
  - 확대·축소는 버튼과 Ctrl/⌘ + 휠이다. 그냥 휠은 화면 스크롤로 남긴다(지도가 page scroll을 가로채지 않게).
- **키보드:**
  - Tab으로 node 사이를 옮긴다. 순서는 층 → 위에서 아래다.
  - →·←는 불리는 쪽·호출하는 쪽 이웃, ↑·↓는 같은 열의 위·아래 node로 간다.
  - Enter·Space는 선택, Esc는 해제다.
- **검색:** 이름이 맞는 node 테두리를 강조하고 맞은 수를 알린다. Enter로 첫 node를 선택한다. 검색어는 URL에 넣지 않는다(자유 입력, D05 §01).
- **표 보기**(`tab=table`): 같은 호출을 요청 많은 순으로 보인다. 열은 연결·근거(관측/추정)·요청/s·오류율·p50/p95/p99·큐 대기 p95·마지막 관측이다. 화면 낭독기와 키보드만 쓰는 사용자의 대안이다.

### 4. URL·갱신·상한·알림

- **URL:**
  - 서비스 선택은 공유 URL의 `service`(UUID)에 남긴다. S02 link와 공유 링크가 그 서비스를 열어 둔다.
  - 외부 node id(`db:…@env`)는 공유 값 형식 밖이라 화면 상태로만 둔다.
- **조사 context:** 환경과 시간 범위는 조사 context를 따른다. 7일을 넘으면 조회하지 않고 줄이라고 알린다(API 한도와 같다).
- **갱신:** 상대 범위면 1분마다 자동 갱신한다(edge 집계 단위가 1분이다). 숨은 탭에서는 멈추고, 다시 보이면 한 번 갱신한다(ADR 0058과 같은 규칙). 절대 범위면 자동 갱신하지 않는다.
- **상한:** 요청이 많은 node부터 200개를 그린다. 숨긴 수와 서버 상한으로 뺀 수(`omitted_count`)를 합쳐 알리고, 환경을 고르지 않았으면 "환경을 고르면 좁혀집니다"라고 덧붙인다.
- **알림:** API 경고를 문장으로 보인다.
  - `range_after_watermark`는 "그 시각 이후는 집계 전(0이 아니라 미계산)"으로 쓴다.
  - 환경을 넘는 호출 숨김, 읽기 상한, 경계 불일치도 알린다.
  - 항상 "표본 trace 기반 · 1분 집계" 배지를 붙인다.
- **빈 결과:** 이유를 설명한다. 서비스가 하나뿐이거나 client·server span이 없는 경우, 아직 집계 전일 수 있는 경우다.

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| **A. 결정적 층 배치(직접 구현) (채택)** | 갱신해도 같은 그림이다. 호출 방향이 왼쪽→오른쪽으로 읽힌다. 의존성이 없고 시험으로 고정된다 | node가 아주 많은 층은 세로로 길어진다(200개 상한·환경 선택으로 완화) |
| B. force-directed(d3-force 등) | 군집이 드러난다 | 갱신마다 위치가 흔들린다(D05 §07과 충돌). 시험이 어렵고 의존성이 늘어난다 |
| C. dagre·cytoscape library | 교차 최소화가 더 낫다 | 번들·의존성 검토가 늘어나고, 상태·접근성을 library 방식에 맞춰야 한다 |
| 오류율 임계치로 색(예: 1%·5%) | 한눈에 심각도가 보인다 | 근거 없는 임계치는 SLO와 다른 판정을 만든다. SLO·monitor 상태가 생기면 그것으로 칠한다 |
| 굵기를 절대 요청/s로 | 화면 사이 비교가 된다 | 작은 서비스는 모두 1px, 큰 서비스는 모두 최대로 몰린다. D05가 "범위 내 정규화"를 말한다 |

## 결과

- 이점:
  - 지도와 표가 같은 내용이다(키보드·낭독기).
  - 공유 링크와 S02 link가 서비스를 선택해 연다.
  - 미계산·환경 밖·상한으로 빠진 것을 숨기지 않는다.
- 비용:
  - 층 배치는 교차를 완전히 줄이지 못한다(barycenter 2회).
  - 200 node를 넘으면 환경 선택이 필요하다.
- 영향 받는 계약:
  - 경로 `/o/{org}/map`(D05 표)
  - URL `tab=table`·`service`
  - 화면 시험(consumer contract로 응답 모양 검사)
- 남은 것:
  - node 상태 icon(monitor·SLO 원천 후)
  - team·namespace 필터(D05 §07, catalog의 team 소유 후)
  - 군집(cluster) 묶음 표시
  - 1024px 미만 icon rail·읽기 모드
  - 선택 서비스 중심으로 다시 조회(`service_ids`, API는 있음)
  - edge 선택과 edge drawer

## Rollback

- 화면은 route 하나다. route를 placeholder로 되돌리면 된다(API·데이터와 독립이다).
- API flag `MONTRACER_SERVICE_MAP_API_ENABLED=false`면 화면은 404 오류 안내를 보인다.

## 재검토 조건

- 사용성 시험에서 교차가 많아 읽기 어렵다는 결과가 나오면 library 배치(C)를 검토한다.
- monitor·SLO와 서비스가 연결되면 node 상태 icon과 edge 색을 그 판정으로 바꾼다.
- 환경을 골라도 200 node를 자주 넘으면 군집 표시를 한다.

## 근거 (빅테크 사례)

- **AWS X-Ray service map / CloudWatch ServiceLens** — https://docs.aws.amazon.com/xray/latest/devguide/xray-console-servicemap.html
  - client에서 downstream으로 왼쪽→오른쪽으로 그린다. node를 고르면 옆 패널에 요청·오류·지연 분포를 보인다.
  - 추정한(inferred) node를 따로 표시한다.
  - §1 방향, §3 drawer, §2 추정 표시의 근거다.
- **Datadog Service Map** — https://docs.datadoghq.com/tracing/services/services_map/
  - node를 고르면 이웃만 강조하고 나머지를 흐리게 한다. 검색으로 서비스를 찾는다. 환경별로 map을 나눈다.
  - node 색은 monitor 상태다(지어낸 임계치가 아님).
  - §2 색 원칙, §3 강조·검색, §4 환경의 근거다.
- **Grafana node graph panel** — https://grafana.com/docs/grafana/latest/panels-visualizations/visualizations/node-graph/
  - 그래프와 같은 데이터를 표(nodes·edges)로도 보인다. 많은 node는 상한을 두고 "더 보기"를 한다. 확대·이동을 지원한다.
  - §3 표 보기, §4 상한의 근거다.
- **Kiali (Istio) graph** — https://kiali.io/docs/features/topology/
  - 계층 배치(dagre)를 기본으로 하고, 사용자가 옮긴 배치를 유지한다.
  - edge 굵기·label은 요청량, 색은 오류 응답 유무·비율을 쓴다.
  - §1 층 배치, 배치 유지의 근거다.

## 증거

- 단위: `apps/web/src/features/map/mapModel.test.ts`
  - 층·순환
  - 결정적 좌표와 barycenter
  - 상한(추정은 0)
  - 값 null(계약 6), 굵기, 1hop, 숨긴 이름
- 화면: `apps/web/src/features/map/ServiceMap.test.tsx`
  - 조회 조건, 종류·색·점선, 선택·1hop·URL·Esc, 공유 링크
  - 키보드 이동, 검색, 표 보기
  - 경고 알림(미계산·환경·상한), 200 node 상한, 빈 결과·오류·7일 초과
  - 배치 초기화
  - 응답 fixture는 OpenAPI 원천으로 검사된다(ADR 0059).
