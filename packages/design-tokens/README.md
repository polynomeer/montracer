# @montracer/design-tokens

light/dark 색상·타이포·치수 토큰 (D05 §02, ADR 0040). `apps/web`과 `packages/ui`가 이 패키지를 원천으로 쓴다.

| 파일 | 내용 |
|---|---|
| `src/tokens.ts` | 토큰 원천. 값마다 출처를 주석으로 표시한다 (`D05` = 명세 표, `시안` = 명세가 정하지 않아 디자인 시안에서 정한 값) |
| `tokens.css` | `src/tokens.ts`에서 생성한 CSS custom property (`--mt-*`). **직접 고치지 않는다** |
| `src/contrast.ts` | WCAG 대비 계산 |
| `test/tokens.test.ts` | 대비·글자 크기·시리즈 규칙과 `tokens.css` 최신 여부 검사 |

## 사용

```css
@import '@montracer/design-tokens/tokens.css';

body {
  background: var(--mt-color-canvas);
  color: var(--mt-color-text-primary);
  font: var(--mt-font-size-body) / var(--mt-line-height-body) var(--mt-font-sans);
}
```

- 테마: 기본은 시스템 설정(`prefers-color-scheme`)을 따른다. 사용자가 고르면 `<html data-theme="light|dark">`로 강제한다.
- 그래프처럼 JS에서 값이 필요하면 `import { colors } from '@montracer/design-tokens'`. 시리즈는 색과 함께 `dash`(선 모양)를 legend·선에 같이 쓴다.
- 상태색(success·warning·critical)은 상태 표시에만 쓴다. 빨강은 오류 상태와 profile diff 범례에만 쓴다 (D05 §02).
- focus ring은 `--mt-color-focus`로 2px 이상, `outline-offset` 2px 이상을 둔다. focus가 action과 같은 색이라, 떨어뜨리지 않으면 primary 버튼 위에서 보이지 않는다.
- 글꼴 파일은 이 패키지에 없다. `apps/web`이 `pretendard` variable font를 번들하고, `--mt-font-sans`가 그 이름(`'Pretendard Variable'`)을 먼저 찾는다.

## 변경

1. `src/tokens.ts`를 고친다. D05 표의 값을 바꾸려면 명세 원본(docx) 개정과 ADR이 먼저다.
2. `pnpm --filter @montracer/design-tokens run generate`로 `tokens.css`를 다시 만든다.
3. `pnpm --filter @montracer/design-tokens test` — 대비가 기준(본문 4.5:1, control·그래프 3:1) 아래로 내려가면 실패한다.

빌드 없이 Node 24의 TypeScript type stripping과 `node:test`로 실행한다. 그래서 `src/`는 erasable 문법만 쓰고 import 경로에 `.ts`를 붙인다(`tsconfig.json`의 `allowImportingTsExtensions`·`erasableSyntaxOnly`). `pnpm --filter @montracer/design-tokens typecheck`로 타입을 검사한다. lint 도구는 아직 없다.
