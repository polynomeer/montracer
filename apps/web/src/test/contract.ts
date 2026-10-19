// UI fixture를 OpenAPI 원천(api/openapi/montracer.yaml)으로 검사한다 — consumer contract (D06 §11, ADR 0059 §4).
//
// 시험이 vi.stubGlobal('fetch', …)로 넣은 가짜 서버의 교환을 가로채 본다.
//   - 응답: 그 operation에 선언된 status이고, JSON 본문이 schema에 맞아야 한다. 선언 안 된 field도 거절한다
//     (서버에 없는 field에 기대는 화면을 막는다 — Go 쪽 strict 검사와 같은 기준).
//   - 요청: 가짜 서버가 받아들인(2xx) 요청의 JSON 본문과 필수 header·query parameter가 원천에 맞아야 한다
//     (서버가 거절할 요청을 화면이 보내지 않게).
// 원천에 없는 경로·외부 URL은 보지 않는다(404 fixture는 "없는 자원"이라 그대로 둔다).
import Ajv2020, { type ValidateFunction } from 'ajv/dist/2020';
import addFormats from 'ajv-formats';
import { parse } from 'yaml';
import specText from '../../../../api/openapi/montracer.yaml?raw';

type Json = null | boolean | number | string | Json[] | { [k: string]: Json };
type Obj = { [k: string]: Json };

interface Operation {
  method: string;
  path: string;
  pattern: RegExp;
  op: Obj;
}

const SPEC_ID = 'montracer';
const spec = parse(specText) as Obj;

const isObj = (v: Json | undefined): v is Obj => typeof v === 'object' && v !== null && !Array.isArray(v);

// $ref를 문서 id 기준으로 바꾸고, field 목록이 닫힌 object schema에 unevaluatedProperties: false를 넣는다.
// map(patternProperties·additionalProperties)과 allOf의 구성 요소(다른 구성 요소의 field를 모른다)는 그대로 둔다.
function strict(node: Json, inAllOf = false): Json {
  if (Array.isArray(node)) return node.map((n) => strict(n));
  if (!isObj(node)) return node;
  const out: Obj = {};
  for (const [k, v] of Object.entries(node)) {
    if (k === '$ref' && typeof v === 'string' && v.startsWith('#/')) out[k] = SPEC_ID + v;
    else if (k === 'allOf' && Array.isArray(v)) out[k] = v.map((n) => strict(n, true));
    else if (k === 'properties' && isObj(v)) out[k] = Object.fromEntries(Object.entries(v).map(([p, s]) => [p, strict(s)]));
    else out[k] = strict(v);
  }
  const objectLike = out.type === 'object' || (Array.isArray(out.type) && out.type.includes('object')) || isObj(out.properties);
  const open = 'additionalProperties' in out || 'patternProperties' in out || 'unevaluatedProperties' in out;
  if (objectLike && !open && !inAllOf) out.unevaluatedProperties = false;
  return out;
}

const ajv = new Ajv2020({ strict: false, allErrors: true });
addFormats(ajv);
ajv.addSchema({ $id: SPEC_ID, components: strict(spec.components as Json) } as object);

const operations: Operation[] = [];
for (const [path, item] of Object.entries(spec.paths as Obj)) {
  if (!isObj(item)) continue;
  const pattern = new RegExp('^' + path.replace(/\{[^}]+\}/g, '[^/]+') + '$');
  for (const [method, op] of Object.entries(item)) {
    if (isObj(op)) operations.push({ method: method.toUpperCase(), path, pattern, op });
  }
}

const compiled = new Map<string, ValidateFunction>();
function validator(key: string, schema: Json): ValidateFunction {
  let v = compiled.get(key);
  if (!v) {
    v = ajv.compile(strict(schema) as object);
    compiled.set(key, v);
  }
  return v;
}

function resolve(node: Json | undefined): Obj | undefined {
  if (!isObj(node)) return undefined;
  const ref = node.$ref;
  if (typeof ref !== 'string') return node;
  let cur: Json = spec;
  for (const part of ref.replace(/^#\//, '').split('/')) cur = isObj(cur) ? (cur[part] ?? null) : null;
  return isObj(cur) ? cur : undefined;
}

function jsonSchema(content: Json | undefined): Json | undefined {
  const c = isObj(content) ? content['application/json'] : undefined;
  return isObj(c) ? c.schema : undefined;
}

function errors(v: ValidateFunction): string {
  return (v.errors ?? [])
    .slice(0, 8)
    .map((e) => `${e.instancePath || '/'} ${e.message ?? ''}${'unevaluatedProperty' in e.params ? ` (${String(e.params.unevaluatedProperty)})` : ''}`)
    .join('; ');
}

const violations: string[] = [];

/** 기록된 위반을 꺼내고 비운다(setup의 afterEach가 시험을 실패시킨다). */
export function takeContractViolations(): string[] {
  return violations.splice(0, violations.length);
}

function find(method: string, pathname: string): Operation | undefined {
  return operations.find((o) => o.method === method && o.pattern.test(pathname));
}

/** 교환 하나를 검사한다. 위반은 모아 두었다가 그 시험이 끝날 때 보고한다. */
export async function checkExchange(input: RequestInfo | URL, init: RequestInit | undefined, res: Response): Promise<void> {
  const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, 'http://localhost');
  const method = (init?.method ?? 'GET').toUpperCase();
  const o = find(method, url.pathname);
  if (!o) return; // 원천 밖(정적 파일·외부 URL·없는 경로)
  if (res.headers.has('X-Test-Off-Contract')) return; // 시험이 일부러 원천을 벗어난 응답(fixtures.OFF_CONTRACT_HEADER)
  const where = `${method} ${url.pathname} → ${res.status}`;
  const responses = o.op.responses as Obj;
  const declared = resolve(responses[String(res.status)]);
  if (!declared) {
    violations.push(`${where}: status ${res.status} is not declared for ${o.method} ${o.path}`);
    return;
  }
  const schema = jsonSchema(declared.content);
  if (schema !== undefined && (res.headers.get('Content-Type') ?? '').includes('json')) {
    const text = await res.clone().text();
    let body: unknown;
    try {
      body = JSON.parse(text);
    } catch {
      violations.push(`${where}: response is not JSON`);
      return;
    }
    const v = validator(`res ${o.method} ${o.path} ${res.status}`, schema);
    if (!v(body)) violations.push(`${where}: response ${errors(v)}`);
  }
  if (res.status >= 300) return;
  const headers = new Headers(init?.headers);
  for (const p of (o.op.parameters as Json[] | undefined) ?? []) {
    const param = resolve(p);
    if (!param || param.required !== true || typeof param.name !== 'string') continue;
    const present = param.in === 'header' ? headers.has(param.name) : param.in === 'query' ? url.searchParams.has(param.name) : true;
    if (!present) violations.push(`${where}: request is missing required ${String(param.in)} parameter ${param.name}`);
  }
  const reqSchema = jsonSchema(resolve(o.op.requestBody as Json)?.content);
  if (reqSchema !== undefined && typeof init?.body === 'string') {
    const v = validator(`req ${o.method} ${o.path}`, reqSchema);
    if (!v(JSON.parse(init.body))) violations.push(`${where}: request ${errors(v)}`);
  }
}

/** fetch 대역을 감싸 교환을 검사한다. 감싼 함수도 vi.fn이라 시험의 mock.calls 단언은 그대로다. */
export function contractFetch<F extends (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(fetchImpl: F): F {
  return (async (input: RequestInfo | URL, init?: RequestInit) => {
    const res = await fetchImpl(input, init);
    await checkExchange(input, init, res);
    return res;
  }) as F;
}
