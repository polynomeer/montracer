// 생성 파일이 원천과 같다 — 원천을 바꾸고 다시 생성하지 않으면 CI(js job)가 실패한다(ADR 0061).
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import { OUT, render } from '../scripts/lib.mjs';

test('src/schema.gen.ts is up to date with api/openapi/montracer.yaml', async () => {
  const [want, got] = [await render(), await readFile(OUT, 'utf8')];
  assert.ok(got === want, 'schema.gen.ts가 원천과 다르다: pnpm --filter @montracer/query-schema run generate');
});
