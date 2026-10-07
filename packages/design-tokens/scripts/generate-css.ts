import { writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { renderCss } from '../src/css.ts';

const target = fileURLToPath(new URL('../tokens.css', import.meta.url));
writeFileSync(target, renderCss());
console.log(`wrote ${target}`);
