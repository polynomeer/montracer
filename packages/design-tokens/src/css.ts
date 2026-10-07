// 토큰을 CSS custom property로 렌더링한다. 결과는 tokens.css로 커밋하고,
// 테스트가 원천(tokens.ts)과 일치하는지 확인한다. 재생성: pnpm --filter @montracer/design-tokens run generate

import {
  breakpointPx,
  colors,
  dashboardGrid,
  fontFamily,
  layoutPx,
  radiusPx,
  shadow,
  spacePx,
  typography,
  type ThemeColors,
  type ThemeName,
} from './tokens.ts';

function kebab(s: string): string {
  return s.replace(/[A-Z]/g, (c) => `-${c.toLowerCase()}`);
}

function colorVars(theme: ThemeName): string[] {
  const t: ThemeColors = colors[theme];
  const out: string[] = [];
  for (const [key, value] of Object.entries(t)) {
    if (typeof value === 'string') {
      out.push(`--mt-color-${kebab(key)}: ${value};`);
    } else if (Array.isArray(value)) {
      continue;
    } else {
      const s = value as { fg: string; bg: string; border: string };
      out.push(`--mt-color-${kebab(key)}-fg: ${s.fg};`);
      out.push(`--mt-color-${kebab(key)}-bg: ${s.bg};`);
      out.push(`--mt-color-${kebab(key)}-border: ${s.border};`);
    }
  }
  t.series.forEach((s, i) => {
    out.push(`--mt-series-${i + 1}: ${s.color};`);
  });
  out.push(`--mt-shadow-overlay: ${shadow.overlay[theme]};`);
  out.push(`color-scheme: ${theme};`);
  return out;
}

function staticVars(): string[] {
  const out: string[] = [
    `--mt-font-sans: ${fontFamily.sans};`,
    `--mt-font-mono: ${fontFamily.mono};`,
  ];
  for (const [name, t] of Object.entries(typography)) {
    out.push(`--mt-font-size-${name}: ${t.sizePx}px;`);
    out.push(`--mt-line-height-${name}: ${t.lineHeightPx}px;`);
    out.push(`--mt-font-weight-${name}: ${t.weight};`);
  }
  for (const v of spacePx) {
    out.push(`--mt-space-${v}: ${v}px;`);
  }
  for (const [name, v] of Object.entries(radiusPx)) {
    out.push(`--mt-radius-${kebab(name)}: ${v}px;`);
  }
  for (const [name, v] of Object.entries(layoutPx)) {
    out.push(`--mt-size-${kebab(name)}: ${v}px;`);
  }
  out.push(`--mt-dashboard-columns: ${dashboardGrid.columns};`);
  out.push(`--mt-dashboard-gutter: ${dashboardGrid.gutterPx}px;`);
  out.push(`--mt-dashboard-row-unit: ${dashboardGrid.rowUnitPx}px;`);
  return out;
}

function block(selector: string, lines: string[], indent = ''): string {
  return `${indent}${selector} {\n${lines.map((l) => `${indent}  ${l}`).join('\n')}\n${indent}}\n`;
}

export function renderCss(): string {
  const bp = Object.entries(breakpointPx)
    .map(([k, v]) => `${k} ${v}px`)
    .join(', ');
  return [
    '/* 생성 파일 — 직접 고치지 말 것. 원천: src/tokens.ts (D05 §02) */',
    `/* breakpoints: ${bp} (CSS 변수는 media query에 쓸 수 없어 주석으로 둔다) */`,
    '',
    block(':root', [...staticVars(), ...colorVars('light')]),
    '@media (prefers-color-scheme: dark) {',
    block(':root:not([data-theme="light"])', colorVars('dark'), '  ').trimEnd(),
    '}',
    '',
    block(':root[data-theme="dark"]', colorVars('dark')),
  ].join('\n');
}
