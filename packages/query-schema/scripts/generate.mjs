import { writeFile } from 'node:fs/promises';
import { OUT, render } from './lib.mjs';

await writeFile(OUT, await render());
console.log(`generated ${OUT.pathname}`);
