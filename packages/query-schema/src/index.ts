// OpenAPI 원천에서 생성한 API 타입 (ADR 0061). 화면은 손으로 쓴 응답 타입 대신 이것을 쓴다.
export type { components, operations, paths } from './schema.gen';
import type { components } from './schema.gen';

/** 원천의 schema(components.schemas)다. 예: `Schema<'QueryMeta'>` */
export type Schema<K extends keyof components['schemas']> = components['schemas'][K];
