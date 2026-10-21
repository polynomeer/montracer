// query-api 호출 (D02 §12·§19). 인증은 dev proxy가 붙인다(ADR 0042) — 브라우저 코드는 credential을 다루지 않는다.
import { csrfHeader, notifyUnauthenticated } from './authState.ts';
import type { ApiErrorBody, Meta } from './types.ts';

export const API_BASE = '/api/v1';

export type ApiResult<T> =
  | { ok: true; status: number; data: T; meta: Meta; nextCursor: string | null }
  | { ok: false; status: number; error: ApiErrorBody; retryAfterSeconds: number | null };

function errorResult(status: number, error: ApiErrorBody, retryAfter: string | null): ApiResult<never> {
  const n = retryAfter === null ? Number.NaN : Number.parseInt(retryAfter, 10);
  return { ok: false, status, error, retryAfterSeconds: Number.isFinite(n) && n >= 0 ? n : null };
}

function isErrorBody(v: unknown): v is { error: ApiErrorBody } {
  if (typeof v !== 'object' || v === null || !('error' in v)) return false;
  const e = (v as { error: unknown }).error;
  return typeof e === 'object' && e !== null && typeof (e as ApiErrorBody).code === 'string';
}

/**
 * JSON API를 부른다. 응답은 `{data, meta}` 또는 오류 envelope이다. 취소(AbortError)는 그대로 throw한다 —
 * 호출자가 최신 요청만 반영하도록(D05 §03).
 */
export async function apiRequest<T>(
  path: string,
  init: {
    method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
    body?: unknown;
    signal?: AbortSignal;
    headers?: Record<string, string>;
    /** 성공 envelope 전체를 한 번 본다(data·meta 밖의 필드: 예를 들어 1회 표시 signing_secret). 값을 저장하지 않는다 */
    onEnvelope?: (envelope: unknown) => void;
  } = {},
): Promise<ApiResult<T>> {
  let res: Response;
  try {
    const method = init.method ?? 'GET';
    res = await fetch(`${API_BASE}${path}`, {
      method,
      headers: {
        Accept: 'application/json',
        // browser session이면 변경 요청에 CSRF token(ADR 0064 §4). API key proxy 모드면 비어 있다
        ...csrfHeader(method),
        ...(init.body === undefined ? {} : { 'Content-Type': 'application/json' }),
        ...(init.headers ?? {}),
      },
      body: init.body === undefined ? null : JSON.stringify(init.body),
      signal: init.signal ?? null,
      credentials: 'same-origin',
    });
  } catch (e) {
    if (e instanceof DOMException && e.name === 'AbortError') throw e;
    return errorResult(0, { code: 'NETWORK', message: '서버에 연결할 수 없습니다.', request_id: '', retryable: true }, null);
  }
  // 본문 없는 성공(DELETE 204): data는 null이다
  if (res.status === 204) {
    return { ok: true, status: 204, data: null as T, meta: { request_id: res.headers.get('X-Request-ID') ?? '', schema_version: 1, warnings: [] }, nextCursor: null };
  }
  let body: unknown = null;
  try {
    body = await res.json();
  } catch (e) {
    if (e instanceof DOMException && e.name === 'AbortError') throw e;
    body = null;
  }
  if (!res.ok) {
    // session 만료·폐기: 로그인으로(복귀 경로 보존). 로그인 상태 조회(/auth/session) 자체의 401은 호출자가 다룬다
    if (res.status === 401 && path !== '/auth/session') notifyUnauthenticated();
    if (isErrorBody(body)) return errorResult(res.status, body.error, res.headers.get('Retry-After'));
    return errorResult(
      res.status,
      { code: `HTTP_${String(res.status)}`, message: '서버가 오류를 돌려줬습니다.', request_id: res.headers.get('X-Request-ID') ?? '', retryable: res.status >= 500 },
      res.headers.get('Retry-After'),
    );
  }
  if (typeof body !== 'object' || body === null || !('data' in body) || !('meta' in body)) {
    return errorResult(res.status, { code: 'BAD_RESPONSE', message: '응답 형식이 올바르지 않습니다.', request_id: '', retryable: false }, null);
  }
  init.onEnvelope?.(body);
  const ok = body as { data: T; meta: Meta; next_cursor?: unknown };
  const nextCursor = typeof ok.next_cursor === 'string' ? ok.next_cursor : null;
  return { ok: true, status: res.status, data: ok.data, meta: ok.meta, nextCursor };
}
