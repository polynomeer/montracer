// query-api 호출 (D02 §12·§19). 인증은 dev proxy가 붙인다(ADR 0042) — 브라우저 코드는 credential을 다루지 않는다.
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
  init: { method?: 'GET' | 'POST'; body?: unknown; signal?: AbortSignal } = {},
): Promise<ApiResult<T>> {
  let res: Response;
  try {
    res = await fetch(`${API_BASE}${path}`, {
      method: init.method ?? 'GET',
      headers: init.body === undefined ? { Accept: 'application/json' } : { Accept: 'application/json', 'Content-Type': 'application/json' },
      body: init.body === undefined ? null : JSON.stringify(init.body),
      signal: init.signal ?? null,
      credentials: 'same-origin',
    });
  } catch (e) {
    if (e instanceof DOMException && e.name === 'AbortError') throw e;
    return errorResult(0, { code: 'NETWORK', message: '서버에 연결할 수 없습니다.', request_id: '', retryable: true }, null);
  }
  let body: unknown = null;
  try {
    body = await res.json();
  } catch (e) {
    if (e instanceof DOMException && e.name === 'AbortError') throw e;
    body = null;
  }
  if (!res.ok) {
    if (isErrorBody(body)) return errorResult(res.status, body.error, res.headers.get('Retry-After'));
    return errorResult(
      res.status,
      { code: 'HTTP_' + String(res.status), message: '서버가 오류를 돌려줬습니다.', request_id: res.headers.get('X-Request-ID') ?? '', retryable: res.status >= 500 },
      res.headers.get('Retry-After'),
    );
  }
  if (typeof body !== 'object' || body === null || !('data' in body) || !('meta' in body)) {
    return errorResult(res.status, { code: 'BAD_RESPONSE', message: '응답 형식이 올바르지 않습니다.', request_id: '', retryable: false }, null);
  }
  const ok = body as { data: T; meta: Meta; next_cursor?: unknown };
  const nextCursor = typeof ok.next_cursor === 'string' ? ok.next_cursor : null;
  return { ok: true, status: res.status, data: ok.data, meta: ok.meta, nextCursor };
}
