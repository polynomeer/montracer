# @montracer/query-schema

OpenAPI 원천(`api/openapi/montracer.yaml`)에서 생성한 API 타입이다 (ADR 0061).

- `src/schema.gen.ts`: 생성 파일. **직접 고치지 않는다.** 커밋한다.
- `src/index.ts`: `paths`·`components`·`operations`와 `Schema<'이름'>` helper
- `pnpm --filter @montracer/query-schema run generate`: 원천을 바꾼 뒤 다시 만든다.
- `pnpm --filter @montracer/query-schema test`: 원천과 생성 파일이 같은지 본다(CI js job). 다르면 실패한다.
- 생성기(openapi-typescript)는 TypeScript compiler API를 쓴다. 레포의 TypeScript 7에는 그 JS API가 없어 이 package에만 TypeScript 5.9를 생성 전용으로 둔다. 생성 결과는 화면이 TypeScript 7로 검사한다.
