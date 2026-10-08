// PS-0007: 가로 스크롤 상자 안의 visually-hidden(position: absolute) 글이 상자에 갇혀야 한다.
// jsdom은 layout을 계산하지 않아 넘침을 직접 잴 수 없다. 대신 규칙이 지워지지 않게 CSS 원문을 고정한다.
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

describe('.mt-scroll-x', () => {
  it('positioning context를 만든다(안의 absolute 숨김 글이 페이지를 넓히지 않게)', () => {
    const css = readFileSync(resolve(process.cwd(), 'src/features/services/services.css'), 'utf8') // vitest root = apps/web;
    const rule = /\.mt-scroll-x\s*\{([^}]*)\}/.exec(css)?.[1] ?? '';
    expect(rule).toMatch(/overflow-x:\s*auto/);
    expect(rule).toMatch(/position:\s*relative/);
  });
});
