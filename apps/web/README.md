# @montracer/web

Montracer web UI (D05). app shell(정보 구조 메뉴, 상단바, 조사 context 줄, 라우트, 테마, [ADR 0041](../../docs/adr/0041-web-app-shell.md))과 Services 목록·S02 서비스 상세([ADR 0042](../../docs/adr/0042-service-detail-red.md)), S04 Trace Explorer([ADR 0043](../../docs/adr/0043-trace-search.md)), S05 Trace 상세([ADR 0044](../../docs/adr/0044-trace-detail.md)), S08 Logs([ADR 0045](../../docs/adr/0045-log-explorer.md))·Metrics([ADR 0047](../../docs/adr/0047-metrics-explorer.md))가 있다. 나머지 화면은 placeholder다.

## 실행

```bash
pnpm install
make up && make migrate && make dev     # query-api 127.0.0.1:18080 (다른 터미널)
make seed SCENARIO=checkout             # demo tenant·key·checkout 데이터 → .seed/demo.json
pnpm --filter @montracer/web dev        # http://localhost:15173 → /o/acme/overview
pnpm --filter @montracer/web test
pnpm --filter @montracer/web typecheck
pnpm --filter @montracer/web build      # dist/
```

### 로컬 API 연결 (dev proxy, ADR 0042)

dev server는 `/api`를 query-api로 넘기고, 서버 쪽에서 seed API key를 `Authorization`에 붙인다. key는 브라우저로 가지 않는다. 브라우저가 보낸 `Authorization`·`Cookie`는 지운다. 이 컴퓨터(loopback)에서 온 같은 사이트 요청만 넘긴다(`--host`로 열어도 다른 기기는 404). `vite preview`에는 proxy가 없다.

| 환경 변수 | 기본값 | 의미 |
|---|---|---|
| `MONTRACER_DEV_QUERY_URL` | `http://127.0.0.1:18080` | query-api 주소 |
| `MONTRACER_DEV_SEED_FILE` | 레포 루트 `.seed/demo.json` | `make seed`가 만든 상태 파일 |
| `MONTRACER_DEV_TENANT` | 첫 tenant(`acme`) | 쓸 seed tenant. 기본 조직 slug도 이 이름이 된다 |
| `MONTRACER_DEV_API_TOKEN` | — | seed 대신 쓸 API key(로컬 전용) |

key가 없으면 화면에 401 "인증이 필요합니다"가 보인다. dev server가 기동할 때 연결 대상과 key 출처(값은 아님)를 출력한다.

## 구조

| 경로 | 내용 |
|---|---|
| `src/main.tsx` | 진입점. 토큰 CSS(`@montracer/design-tokens/tokens.css`), Pretendard 글꼴, shell CSS를 불러오고 첫 그림 전에 테마를 적용 |
| `src/app/nav.ts` | 정보 구조(D05 §01)와 화면·경로 목록(D05 §04). 경로는 `/o/{org}/…` |
| `src/app/context.ts` | 조사 context ↔ URL query. 파싱·검증, 공유 링크(UTC 절대시간·허용 키만) |
| `src/app/AppShell.tsx` | sidebar·상단바·context 줄과 `<Outlet>`. 하위 화면은 `useOutletContext<InvestigationContext>()`로 context를 받는다 |
| `src/app/routes.tsx` | 라우트 표. 없는 경로는 404 |
| `src/app/theme.ts` | 시스템·light·dark 선택 → `<html data-theme>` |
| `src/app/shell.css` | 레이아웃. 색·치수는 `--mt-*` 토큰만 쓴다 |
| `src/api/` | query-api 호출(`client.ts`: 오류 envelope·Retry-After), 타입, `useRemote`(취소·늦은 응답 무시·stale 유지), `useSearchPages`(cursor page 누적·partial, S04·S08) |
| `src/features/traces/` | S04 Trace Explorer(ADR 0043): 조건 ↔ URL ↔ filter AST는 `filters.ts`, page 누적은 `useSearchPages`, 분포는 `TraceScatter`. S05 Trace 상세(ADR 0044): 트리·critical path·필터는 `waterfall.ts`(순수 함수), 가상화 waterfall은 `TraceDetail.tsx`, span 패널·log 연결은 `SpanDrawer.tsx` |
| `src/features/logs/` | S08 Logs(ADR 0045): 조건 ↔ URL ↔ filter AST·심각도 이름·S05 링크 범위는 `logFilters.ts`(순수 함수), 가상화 결과 표는 `LogExplorer.tsx`, 속성 패널은 `LogDrawer.tsx` |
| `src/features/metrics/` | S08 Metrics(ADR 0047): URL ↔ QuerySpec·자동 step·단위·값 표기는 `metricQuery.ts`(순수 함수), 여러 series 차트(색×선 모양, 끊김·집계 중·일부 집계, 표 대안)는 `SeriesChart.tsx`, 사전·builder·legend·요약 표는 `MetricsExplorer.tsx` |
| `src/features/services/` | Services 목록, S02 서비스 상세. RED 계산은 `red.ts`(순수 함수), 차트는 `MetricChart.tsx`(빈 step은 끊고 표 대안 제공) |
| `src/features/*` | 그 밖의 기능별 화면 자리(아직 비어 있음) |
| `dev-proxy.ts` | 로컬 개발 전용 API proxy(build에 들어가지 않음) |

## URL query 계약

| 키 | 의미 |
|---|---|
| `env` | environment(1~64 byte, 서버와 같은 규칙). 없으면 전체 환경 |
| `range` | 상대 구간 `15m`·`1h`·`4h`·`1d`·`7d` (live 조사용, 기본 `1h`) |
| `from`, `to` | 절대 구간. timezone이 명시된 ISO 8601만 받는다 (`2026-10-04T05:00:00Z`). 끝은 지금 + 5분까지, 길이는 395일까지 |
| `tz` | 표시 timezone (IANA). 없으면 브라우저 timezone |
| `filter`, `tab`, `entity` | 화면 상태. ID 형식일 때만 공유 링크에 남는다 |
| `service`, `errors`, `min_ms`, `name` | trace 검색 조건(S04). 앞의 셋(UUID·`1`·정수)만 공유 링크에 남고 자유 입력 `name`은 빠진다 |
| `metric`, `agg`, `group`, `step`, `f` | metric 조회(S08 Metrics). 이름·연산·group label key(쉼표)·step만 공유 링크에 남고 조건 값 `f`(`key=value`)는 빠진다 |
| `service`, `sev`, `trace`, `q` | log 검색 조건(S08). 서비스 UUID·최소 심각도(`trace`~`fatal`)·trace ID(hex 32)만 공유 링크에 남고 본문 검색어 `q`는 빠진다 |

- 잘못된 값은 기본값으로 대체하고 context 줄에 이유를 보인다.
- 메뉴 이동은 context 키만 들고 간다.
- "절대시간 링크 복사"는 상대 구간을 지금 기준 UTC로 고정하고, 위 표 밖의 query(검색어 원문 등)와 fragment를 버린다.
- 조직이 바뀌면 shell을 새로 mount한다(`OrgShell`의 `key={org}`).

## 규칙 (D05 §04, apps/README)

- 서버 상태는 tenant·auth fingerprint를 포함한 key로 관리하고 전역 store에 서버 원본을 복사하지 않는다.
- 라우트 가드와 메뉴 숨김은 보안 경계가 아니다. 서버 재인가를 전제로 한다.
- log·stack·SQL은 text-only 렌더링. raw HTML 금지.
- 색·치수는 토큰 변수만 쓴다. 상태색은 상태 표시에만 쓴다 (ADR 0040).

## 아직 없는 것

S02의 의존성·인스턴스·오류·배포 탭과 endpoint(route)별 trace 이동, S04 facet 개수·duration 정렬, S05 profile 탭(수집 전), S08 live 모드(stream API 전)·속성 facet, Metrics exemplar·label 값 자동완성·다른 metric 간 연산, brushing·baseline 비교, 로그인(OIDC)과 principal 기본 조직, 조직 전환 UI와 조회 cache 분리(서버 상태 library 도입 시), capabilities·권한 API에 따른 Experience·Admin 메뉴, service context, 1024~1439px icon rail, 768~1023px 단일 열과 768px 미만 읽기 모드(화면 구현 시), lint 도구.

글꼴: Pretendard (SIL Open Font License 1.1), `pretendard` 패키지에서 로컬 번들.
