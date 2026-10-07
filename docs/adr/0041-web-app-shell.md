# ADR 0041: web app shell과 frontend 도구

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록")
- Owner: FE
- 날짜: 2026-10-07 (제안·결정)
- 관련: E04, F02·F03·F04, D05 §01(전역 context·정보 구조), §02(치수·반응형), §04(라우트, Frontend 구현 구조), D06 §11(`apps/web`), ADR 0013, ADR 0040

## 배경

D05 §04는 React와 TypeScript를 기본 제안 스택으로 두고 "router가 context를 파싱"하는 구조를 요구한다. 화면 경로는 `/o/{org}/…`이다. 화면 작업(E04)을 시작하려면 app shell이 먼저 있어야 한다. shell은 정보 구조 메뉴, 상단바, 조사 context 줄, 테마, 라우트를 포함한다. 그런데 bundler·router·시험 도구는 명세가 정하지 않았다.

D05 §01은 전역 context의 의미를 다음처럼 정한다.

- 조직·region은 보안 context다. environment·service·time은 조사 context다.
- environment·time을 바꿔도 화면은 유지된다.
- 공유 URL에는 from/to UTC, timezone, filter ID, 선택 tab·entity만 넣는다.
- 상대시간은 live 조사용이다. 공유는 절대시간이 기본이다.

## 결정

1. **도구**는 Vite(dev server·build), React 19, React Router(data router), TypeScript, Vitest + Testing Library + jsdom이다. 버전은 lockfile과 `package.json`에 정확히 고정하고 ADR 0013 표에 기록한다. dev server는 1xxxx 대역 관례대로 `15173`이다.
2. **조사 context는 URL query가 원천이다**. 전역 store에는 복사하지 않는다(D05 §04).
   - `env`, `range`(상대 preset 15m·1h·4h·1d·7d) 또는 `from`·`to`(timezone이 명시된 ISO 8601), `tz`를 쓴다.
   - 잘못된 값은 기본값(전체 환경·최근 1시간·브라우저 timezone)으로 대체한다. 입력 옆에 이유를 보이고 `aria-invalid`를 붙인다.
   - timezone이 없는 시각은 브라우저 local로 해석하지 않고 거절한다.
   - 끝 시각은 지금 + 5분(시계 차이)까지만 받는다. 구간 길이는 395일까지만 받는다. 395일은 가장 긴 보존 기간인 metric_1h(ADR 0028)이고, 그보다 긴 구간은 조회할 데이터가 없다.
   - `env`는 서버(`internal/authz`)와 같은 1~64 byte이고 제어 문자는 받지 않는다. 서버가 받는 값을 UI가 거절해 "전체 환경"으로 넓어지는 일이 없게 하기 위해서다.
3. **메뉴 이동**은 조사 context(`env`·`range`·`from`·`to`·`tz`)만 들고 간다. 화면별 filter·tab은 남기지 않는다.
4. **공유 링크**는 상대 구간을 지금 기준 UTC 절대 구간으로 고정한다. 허용 키(context, `filter`, `tab`, `entity`) 외의 query와 URL fragment는 버린다. 검색어 원문이나 token이 링크로 새지 않게 하기 위해서다.
   - 허용 키도 ID 형식일 때만 남긴다: `filter` `[A-Za-z0-9_-]{1,64}`, `tab` `[a-z0-9-]{1,32}`, `entity` `[A-Za-z0-9._:-]{1,128}`.
5. **정보 구조**는 D05 §01 표 그대로다. 그룹은 Observe·Investigate·Experience·Respond·Organize·Admin이다.
   - Experience(G2·G3 계약)와 Admin(Team·Access·Data Controls·Usage·Audit, 별도 관리 권한)은 해당 entitlement가 있을 때만 보인다. capabilities·권한 API를 연결하기 전까지는 빈 집합이라 숨겨진다.
   - 라우트 가드는 보안 경계가 아니다(D05 §04). 메뉴 숨김은 표시 정책일 뿐이다.
6. **없는 화면**은 존재를 드러내지 않는 404 하나로 처리한다(D05 §01).
   - **조직 전환**: shell을 조직마다 새로 mount한다(`key={org}`). 이전 조직의 화면 상태가 넘어오지 않는다. 조회 cache는 서버 상태 library를 들일 때 tenant·auth fingerprint key로 분리한다.
7. **반응형**: 1440px 이상은 sidebar를 고정한다. 그 아래는 메뉴 버튼으로 여는 overlay drawer다.
   - drawer는 modal이다. 열면 첫 메뉴로 focus가 가고, 본문은 `inert`라 focus가 메뉴 안에 머문다. Esc·scrim으로 닫으면 메뉴 버튼으로 focus가 돌아온다. 넓은 화면으로 바뀌면 자동으로 풀린다.
   - 아직 없는 것: 1024~1439px "접힌 sidebar(64px icon rail)"는 메뉴 icon이 정해진 뒤 추가한다. 768~1023px 단일 상세 열과 768px 미만 읽기 모드(incident·service summary 중심)는 각 화면을 구현할 때 함께 만든다.
8. **테마**: 기본은 시스템 설정을 따른다. 사용자가 고르면 `<html data-theme>`로 강제한다(ADR 0040). 선택은 브라우저 저장소에만 둔다. 저장소를 쓸 수 없으면 그 화면에만 적용한다.
9. **글꼴**: `pretendard` 패키지의 variable dynamic subset을 앱에 번들한다. 외부 CDN은 쓰지 않는다(D05 §02 "로컬 번들"). 이것으로 ADR 0040의 남은 일 하나를 닫는다.
10. **개발용 기본 조직**: 인증 연동 전에는 `/`를 `/o/demo/overview`로 보낸다. 로그인을 연결하면 principal의 기본 조직으로 바꾼다.

## 외부 사례 근거

| 사례 | 내용 | 반영 |
|---|---|---|
| Grafana ([github.com/grafana/grafana](https://github.com/grafana/grafana), [dashboards 사용 문서](https://grafana.com/docs/grafana/latest/dashboards/use-dashboards/)) | React + TypeScript, 시간 범위·변수를 URL query(`from`·`to`·`var-*`)로 두어 링크로 상태를 공유 | 결정 2·4 |
| Datadog 공유 링크 ([docs.datadoghq.com/dashboards/guide/custom_time_frames](https://docs.datadoghq.com/dashboards/guide/custom_time_frames/)) | 상대 시간과 고정 시간을 구분해 링크로 공유 | 결정 4 |
| React 공식 문서 "Creating a React App" ([react.dev/learn/creating-a-react-app](https://react.dev/learn/creating-a-react-app)) | framework 없이 시작할 때 Vite 같은 build 도구 사용을 안내 | 결정 1 |
| Vitest ([vitest.dev](https://vitest.dev/)) | Vite 설정을 그대로 쓰는 test runner | 결정 1 |

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A. Vite + React Router SPA (채택) | 정적 파일로 배포, API는 Go 서비스가 담당. 설정 최소 | SSR 없음. 조사 도구라 SEO 불필요 |
| B. Next.js 등 full-stack framework | SSR·서버 라우트 | Node 서버가 하나 더 생김. API·인가는 Go에 있어 이점이 작음 |
| C. Jest + babel | 사례가 많음 | Vite 설정과 변환 설정이 이중화됨 |
| D. 전역 store(Redux 등)에 context 보관 | 접근이 쉬움 | D05 §04 위반 소지, URL과 이중 원천 |

## 결과

- 이점: 화면 작업이 라우트·context·테마를 바로 쓸 수 있다. context 규칙(공유 절대시간, 허용 키)이 단위 시험으로 고정된다.
- 비용: 의존성 갱신은 ADR 0013 표와 함께 한다. lint 도구는 아직 없다. root `lint` script는 `--if-present`라 건너뛴다.
- 영향 받는 계약: 없음(API·schema 변경 없음). URL query 이름(`env`, `range`, `from`, `to`, `tz`, `filter`, `tab`, `entity`)이 화면 간 계약이 된다.

## Rollback

`apps/web`을 지우면 된다. 배포된 적이 없다.

## 재검토 조건

- 로그인(OIDC)·capabilities API 연결 시: 결정 5·10을 바꾼다
- 메뉴 icon이 정해질 때: 결정 7의 1024~1439px icon rail
- 서버 상태 library를 도입할 때(D05 §04 tenant·auth fingerprint key): 별도 ADR
- lint 도구 도입 시: ADR 0013 갱신

## 증거

- `apps/web/src/app/context.test.ts`, `AppShell.test.tsx` — 27개 시험(조직 전환 시험은 `key`를 빼면 실패함을 확인)
- 로컬 확인: 1440px 고정 sidebar, 1024px drawer, light·dark 전환, 메뉴 이동 시 context 유지 (dev server `:15173`)
