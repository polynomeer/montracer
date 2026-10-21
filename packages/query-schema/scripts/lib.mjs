// OpenAPI 원천 → TypeScript 타입 (ADR 0061). 생성 결과는 src/schema.gen.ts에 넣고 커밋한다.
// 생성기는 TypeScript compiler API(5.x)를 쓴다 — 이 package의 typescript 5.9는 생성 전용이고, 화면은 TypeScript 7로 결과만 읽는다.
import openapiTS, { astToString } from 'openapi-typescript';

export const SPEC = new URL('../../../api/openapi/montracer.yaml', import.meta.url);
export const OUT = new URL('../src/schema.gen.ts', import.meta.url);

const HEADER = `// 생성 파일 — 직접 고치지 않는다. 원천: api/openapi/montracer.yaml (ADR 0059·0061)
// 다시 만들기: pnpm --filter @montracer/query-schema run generate
/* eslint-disable */
`;

export async function render() {
  // 기본 설정: required가 아닌 field는 optional(빠짐), `type: [X, 'null']`은 `X | null`이다 — 둘을 구분한다(계약 6)
  const ast = await openapiTS(SPEC);
  return HEADER + astToString(ast);
}
