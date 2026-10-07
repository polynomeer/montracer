import { useEffect, useRef, useState } from 'react';
import type { ApiResult } from './client.ts';
import type { ApiErrorBody, Meta } from './types.ts';

// 서버 상태 한 건 (D05 §03 상태 계약).
// - key가 바뀌면 이전 요청을 취소한다. 늦게 온 이전 응답이 최신 화면을 덮어쓰지 않는다.
// - 다시 불러오는 동안 마지막 성공 데이터를 유지한다(refreshing). 실패해도 마지막 성공 데이터와 시각을 남긴다(stale).
// - 결과는 컴포넌트 상태에만 둔다. 전역 store에 서버 원본을 복사하지 않는다(D05 §04).
// queryKey에는 조직(tenant)과 조회 조건이 모두 들어가야 한다. 조건이 바뀌면 이전 결과를 버린다
// (다른 조건의 값을 새 조건 값처럼 보이지 않게). 같은 조건을 다시 부를 때(refreshToken만 바뀜)만 이전 값을 유지한다.

export interface RemoteState<T> {
  status: 'loading' | 'success' | 'error';
  data: T | null;
  meta: Meta | null;
  nextCursor: string | null;
  error: { status: number; body: ApiErrorBody; retryAfterSeconds: number | null } | null;
  refreshing: boolean;
  /** 마지막 성공 응답 시각(ms). 오류가 나도 남는다. */
  lastSuccessMs: number | null;
}

const initial: RemoteState<never> = {
  status: 'loading',
  data: null,
  meta: null,
  nextCursor: null,
  error: null,
  refreshing: false,
  lastSuccessMs: null,
};

export function useRemote<T>(
  queryKey: string | null,
  refreshToken: number,
  fetcher: (signal: AbortSignal) => Promise<ApiResult<T>>,
): RemoteState<T> {
  const [state, setState] = useState<RemoteState<T>>(initial);
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;
  const lastKey = useRef<string | null>(null);

  useEffect(() => {
    if (queryKey === null) return;
    const ctrl = new AbortController();
    const sameQuery = lastKey.current === queryKey;
    lastKey.current = queryKey;
    setState((s) => (sameQuery && s.data !== null ? { ...s, refreshing: true } : { ...initial }));
    fetcherRef
      .current(ctrl.signal)
      .then((r) => {
        if (ctrl.signal.aborted) return;
        if (r.ok) {
          setState({
            status: 'success',
            data: r.data,
            meta: r.meta,
            nextCursor: r.nextCursor,
            error: null,
            refreshing: false,
            lastSuccessMs: Date.now(),
          });
        } else {
          setState((s) => ({
            ...s,
            status: 'error',
            error: { status: r.status, body: r.error, retryAfterSeconds: r.retryAfterSeconds },
            refreshing: false,
          }));
        }
      })
      .catch((e: unknown) => {
        if (ctrl.signal.aborted) return;
        setState((s) => ({
          ...s,
          status: 'error',
          error: {
            status: 0,
            body: { code: 'CLIENT', message: e instanceof Error ? e.message : '알 수 없는 오류', request_id: '', retryable: true },
            retryAfterSeconds: null,
          },
          refreshing: false,
        }));
      });
    return () => ctrl.abort();
  }, [queryKey, refreshToken]);

  return state;
}
