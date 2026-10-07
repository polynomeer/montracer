# @montracer/web

Montracer web UI (D05). 이 문서는 지금 들어 있는 app shell을 설명한다. 구성은 정보 구조 메뉴, 상단바, 조사 context 줄, 라우트, 테마다. 화면(S01~S14)은 아직 placeholder이며 API를 호출하지 않는다. 결정은 [ADR 0041](../../docs/adr/0041-web-app-shell.md)에 있다.

## 실행

```bash
pnpm install
pnpm --filter @montracer/web dev        # http://localhost:15173 → /o/demo/overview
pnpm --filter @montracer/web test
pnpm --filter @montracer/web typecheck
pnpm --filter @montracer/web build      # dist/
```

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
| `src/features/*` | 기능별 화면 자리(아직 비어 있음) |

## URL query 계약

| 키 | 의미 |
|---|---|
| `env` | environment(1~64 byte, 서버와 같은 규칙). 없으면 전체 환경 |
| `range` | 상대 구간 `15m`·`1h`·`4h`·`1d`·`7d` (live 조사용, 기본 `1h`) |
| `from`, `to` | 절대 구간. timezone이 명시된 ISO 8601만 받는다 (`2026-10-04T05:00:00Z`). 끝은 지금 + 5분까지, 길이는 395일까지 |
| `tz` | 표시 timezone (IANA). 없으면 브라우저 timezone |
| `filter`, `tab`, `entity` | 화면 상태. 공유 링크에 남는 유일한 비-context 키이며, ID 형식일 때만 남는다 |

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

로그인(OIDC)과 principal 기본 조직, 조직 전환 UI와 조회 cache 분리(서버 상태 library 도입 시), capabilities·권한 API에 따른 Experience·Admin 메뉴, service context, 1024~1439px icon rail, 768~1023px 단일 열과 768px 미만 읽기 모드(화면 구현 시), lint 도구.

글꼴: Pretendard (SIL Open Font License 1.1), `pretendard` 패키지에서 로컬 번들.
