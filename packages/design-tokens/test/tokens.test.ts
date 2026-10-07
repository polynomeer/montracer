import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';
import { contrastRatio } from '../src/contrast.ts';
import { renderCss } from '../src/css.ts';
import { MIN_FONT_SIZE_PX, colors, typography, type ThemeName } from '../src/tokens.ts';

const THEMES: ThemeName[] = ['light', 'dark'];
const TEXT_AA = 4.5; // WCAG 1.4.3 본문
const NON_TEXT = 3; // WCAG 1.4.11 control 경계·그래프 표식

function expectAtLeast(fg: string, bg: string, min: number, label: string): void {
  const ratio = contrastRatio(fg, bg);
  assert.ok(ratio >= min, `${label}: ${fg} on ${bg} = ${ratio.toFixed(2)} < ${min}`);
}

describe('contrastRatio', () => {
  it('흑백 대비는 21:1', () => {
    assert.equal(contrastRatio('#000000', '#FFFFFF').toFixed(1), '21.0');
  });
  it('잘못된 hex는 거절', () => {
    assert.throws(() => contrastRatio('#FFF', '#000000'));
  });
});

for (const theme of THEMES) {
  const t = colors[theme];
  describe(`${theme} 대비 (D05 §02)`, () => {
    it('본문·보조 글자는 canvas·surface·subtle 위에서 4.5:1 이상', () => {
      for (const bg of [t.canvas, t.surface, t.surfaceSubtle, t.selectionBg]) {
        expectAtLeast(t.textPrimary, bg, TEXT_AA, 'text.primary');
        expectAtLeast(t.textMuted, bg, TEXT_AA, 'text.muted');
      }
    });
    it('action·hover 링크와 그 위 글자는 4.5:1 이상', () => {
      for (const bg of [t.canvas, t.surface]) {
        expectAtLeast(t.action, bg, TEXT_AA, 'action');
        expectAtLeast(t.actionHover, bg, TEXT_AA, 'actionHover');
      }
      expectAtLeast(t.onAction, t.action, TEXT_AA, 'onAction on action');
      expectAtLeast(t.onAction, t.actionHover, TEXT_AA, 'onAction on actionHover');
    });
    it('상태 배지 글자는 자기 배경 위에서 4.5:1 이상', () => {
      for (const [name, s] of Object.entries({
        success: t.success,
        warning: t.warning,
        critical: t.critical,
        info: t.info,
        neutral: t.neutral,
      })) {
        expectAtLeast(s.fg, s.bg, TEXT_AA, `${name} badge`);
      }
    });
    it('control 경계·focus·그래프 시리즈·warning 표식은 3:1 이상', () => {
      for (const bg of [t.canvas, t.surface, t.surfaceSubtle, t.selectionBg]) {
        expectAtLeast(t.borderControl, bg, NON_TEXT, 'border.control');
        expectAtLeast(t.focus, bg, NON_TEXT, 'focus');
        expectAtLeast(t.warningMark, bg, NON_TEXT, 'warning mark');
        for (const s of t.series) {
          expectAtLeast(s.color, bg, NON_TEXT, `series ${s.name}`);
        }
      }
    });
    it('시리즈는 blue·teal·orange·violet 순서이고 선 모양이 모두 다르다', () => {
      assert.deepEqual(
        t.series.map((s) => s.name),
        ['blue', 'teal', 'orange', 'violet'],
      );
      assert.equal(new Set(t.series.map((s) => s.dash)).size, t.series.length);
    });
  });
}

describe('typography', () => {
  it(`모든 글자 크기는 ${MIN_FONT_SIZE_PX}px 이상`, () => {
    for (const [name, s] of Object.entries(typography)) {
      assert.ok(s.sizePx >= MIN_FONT_SIZE_PX, `${name} ${s.sizePx}px`);
    }
  });
});

describe('tokens.css', () => {
  it('커밋된 tokens.css가 원천과 일치 (불일치 시 pnpm run generate)', () => {
    const committed = readFileSync(fileURLToPath(new URL('../tokens.css', import.meta.url)), 'utf8');
    assert.equal(committed, renderCss());
  });
  it('dark는 시스템 설정과 data-theme 강제 양쪽에 정의', () => {
    const css = renderCss();
    assert.match(css, /@media \(prefers-color-scheme: dark\)/);
    assert.match(css, /:root:not\(\[data-theme="light"\]\)/);
    assert.match(css, /:root\[data-theme="dark"\]/);
  });
});
