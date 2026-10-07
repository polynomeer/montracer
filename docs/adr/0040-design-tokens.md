# ADR 0040: 디자인 토큰 패키지와 명세 밖 토큰 값

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록")
- Owner: FE/Design
- 날짜: 2026-10-07 (제안·결정)
- 관련: E04, D05 §02(색상·타이포·치수·반응형), §12(접근성), D06 §11(`packages/design-tokens`)

## 배경

D05 §02는 light/dark 기본 색(canvas·surface·text·border·action·상태 3종), 타이포 스케일, 치수, 반응형 구간을 정한다. 화면을 그리려면 명세에 없는 값도 필요하다.

- 상태 배지 테두리, 정보성 배지(info: SDK metric·sampled 표시), 중립 배지(stale·판정 없음)
- 그래프 시리즈 4색의 hex. 명세는 "blue·teal·orange·violet 순, 선 모양·marker 병행"만 정한다
- dark 테마의 시리즈·hover·선택 배경, action 위 글자색
- warning을 글자가 아닌 점·막대로 그릴 때의 색
- code 글자 12/18, body·label·code의 weight(400)

D05 §02는 "색상 대비는 구현 시 실제 조합과 disabled·hover 상태까지 자동 검사"를 요구한다. 위 값은 이 ADR로 정한다. 값의 출발점은 S01~S14 light·dark 화면 시안(2026-10-04, 레포 밖 비공개 디자인 캔버스)이고, 채택 근거는 아래 대비 시험이다. 이 ADR은 값과 함께 그 값을 코드 원천으로 옮기는 방식을 정한다.

## 결정

1. **원천은 TypeScript 객체 하나** (`packages/design-tokens/src/tokens.ts`). 값마다 출처를 주석으로 단다: `D05`(명세 표) 또는 `시안`(이 ADR로 정한 값). D05 값을 바꾸려면 명세 원본 개정과 별도 ADR이 먼저다.
2. **CSS custom property(`--mt-*`)를 생성해 커밋**한다 (`tokens.css`). 테마는 기본으로 `prefers-color-scheme`을 따르고, 사용자가 고르면 `<html data-theme="light|dark">`가 우선한다. 생성 파일이 원천과 다르면 테스트가 실패한다.
3. **대비를 단위 시험으로 강제**한다. 두 테마 모두에서 본문·보조 글자·링크(기본·hover)·action 위 글자·상태 배지 글자는 WCAG 1.4.3 4.5:1, control 경계·focus·그래프 시리즈·warning 표식은 놓일 수 있는 모든 배경(canvas·surface·subtle·선택)에서 WCAG 1.4.11 3:1. 글자 크기 12px 이상, 시리즈 순서와 선 모양이 서로 다른지도 검사한다.
   - disabled 상태 토큰은 아직 없다. D05 §02의 "disabled 상태까지 자동 검사"는 `packages/ui`에서 disabled 표현을 정할 때 토큰과 시험을 함께 추가한다.
   - focus는 D05대로 action과 같은 색이라, primary 버튼 바로 옆에서는 구별되지 않는다. focus ring은 2px 이상 `outline-offset`으로 버튼과 떨어뜨려 그 사이에 배경색이 보이게 한다(규칙은 README). 컴포넌트 시험은 `packages/ui` 단계에서 한다.
4. **외부 의존성 없이** Node 24의 TypeScript type stripping과 `node:test`로 실행한다. `src/`는 erasable 문법만 쓰고 import 경로에 `.ts`를 붙인다. `typescript`·lint 도구는 workspace에 처음 들일 때 ADR 0013에 버전을 기록하면서 `typecheck`·`lint` script를 붙인다 (`typecheck`는 2026-10-07 추가, 변경 이력 참고).
5. 명세 밖 값은 시안 값을 쓴다. 표는 `src/tokens.ts`가 원천이며, 대비 시험을 통과한 값만 쓴다.

## 외부 사례 근거

| 사례 | 내용 | 반영 |
|---|---|---|
| GitHub Primer primitives ([github.com/primer/primitives](https://github.com/primer/primitives)) | 토큰 원천에서 테마별 CSS 변수를 생성해 배포하고, 화면은 변수만 참조 | 결정 1·2 |
| IBM Carbon themes ([carbondesignsystem.com/elements/themes/overview](https://carbondesignsystem.com/elements/themes/overview/)) | 역할 이름(semantic) 토큰으로 테마를 교체, 같은 역할 이름이 light·dark에서 다른 값을 가짐 | 결정 1의 역할 이름(`textMuted`, `borderControl`) |
| WCAG 2.2 1.4.3 / 1.4.11 ([w3.org/TR/WCAG22](https://www.w3.org/TR/WCAG22/#contrast-minimum)) | 본문 4.5:1, UI 구성 요소·그래픽 3:1 | 결정 3의 기준값 |
| Node.js TypeScript 지원 ([nodejs.org/api/typescript.html](https://nodejs.org/api/typescript.html)), `node:test` ([nodejs.org/api/test.html](https://nodejs.org/api/test.html)) | Node 24에서 erasable TypeScript를 빌드 없이 실행, 내장 test runner | 결정 4 |

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A. TS 원천 + 생성 CSS 커밋 + 내장 test (채택) | 의존성 0, 빌드 단계 없음, CSS만 쓰는 소비자도 바로 사용 | import에 `.ts` 확장자, typecheck는 typescript 도입 후 |
| B. Style Dictionary 같은 토큰 빌드 도구 | 플랫폼별 출력(iOS·Android) | 지금은 web 하나뿐. 의존성·버전 고정 비용 |
| C. CSS만 손으로 작성 | 가장 단순 | 그래프 등 JS에서 쓸 값이 따로 놀고, 대비 시험이 CSS 파싱에 의존 |
| D. 명세 밖 값을 정하지 않고 명세 개정까지 대기 | 권위 문서와 일치 | E04 화면 작업이 막힘. 시안 값은 D05 값과 충돌하지 않음 |

## 결과

- 이점: 화면·컴포넌트가 hex 대신 역할 이름을 쓴다. 대비가 기준 아래로 내려가는 변경은 CI(`pnpm run test`)에서 막힌다.
- 비용: `tokens.css` 재생성을 잊으면 시험이 실패한다(의도된 동작). D05 원본에 시안 값을 반영할지는 디자인 리뷰에서 정한다.
- ~~남은 일: 글꼴 로컬 번들~~ — 2026-10-07 `apps/web`에서 해결 (변경 이력)
- 남은 일: disabled 토큰과 focus ring 시험 (결정 3).
- 영향 받는 계약: 없음(API·schema 변경 없음). `apps/web`과 `packages/ui`는 `@montracer/design-tokens`를 원천으로 쓴다.

## Rollback

패키지를 지우면 된다. 아직 이 패키지를 쓰는 화면이 없다.

## 재검토 조건

- 디자인 리뷰나 D05 개정으로 시안 값이 바뀔 때 (이 ADR 변경 이력에 기록)
- `typescript`·lint 도구를 workspace에 들일 때 (결정 4)
- web 외 플랫폼(모바일 등)이 같은 토큰을 써야 할 때 (후보 B 재검토)

## 증거

- `packages/design-tokens/test/tokens.test.ts` — 15개 시험(대비, 글자 크기, 시리즈, `tokens.css` 최신 여부)
- 값의 출발점: S01~S14 light·dark 화면 시안(2026-10-04). 레포 밖 비공개 디자인 캔버스이므로 증거는 위 시험이다

## 변경 이력

- 2026-10-07 (ADR 0041): `apps/web` shell과 함께 반영.
  - `typescript`(devDependency)와 `tsconfig.json`(`erasableSyntaxOnly`)을 추가하고 `typecheck` script를 붙였다(결정 4).
  - 글꼴 로컬 번들은 `apps/web`이 `pretendard` variable font로 해결했다(남은 일 하나 종료).
  - font stack 맨 앞을 번들된 이름 `'Pretendard Variable'`로 바꿨다.
  - focus ring 규칙(offset 2px)은 `apps/web` shell CSS에 적용했다. 컴포넌트 시험은 여전히 `packages/ui` 단계다.
