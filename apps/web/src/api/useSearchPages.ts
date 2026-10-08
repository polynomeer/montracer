import { useCallback, useEffect, useRef, useState } from 'react';
import type { ApiResult } from './client.ts';
import type { ApiErrorBody, Meta, SearchRequest } from './types.ts';

// cursor 검색 결과 page들 (trace S04·log S08, D02 §19). "더 보기"는 다음 cursor로 이어 붙인다.
// 조건(queryKey)이 바뀌면 이전 결과를 버리고 진행 중 요청을 취소한다 — 늦은 응답이 새 조건의 화면을 덮지 않는다(D05 §03).
// queryKey에는 조직(tenant)과 조건이 모두 들어가야 한다.
export interface SearchPagesState<T> {
  status: 'idle' | 'loading' | 'success' | 'error';
  rows: T[];
  nextCursor: string | null;
  warnings: string[];
  /** 어느 page든 일부 저장소가 응답하지 않았으면 참(계약 6: 빠진 결과를 "없음"으로 보이지 않는다). */
  partial: boolean;
  requestId: string | null;
  error: { status: number; body: ApiErrorBody; retryAfterSeconds: number | null } | null;
  loadingMore: boolean;
  /** 마지막으로 page를 받은 시각(ms). 다음 page가 실패해도 남는다(D05 §03 last successful at). */
  lastSuccessMs: number | null;
}

const initial: SearchPagesState<never> = {
  status: 'idle',
  rows: [],
  nextCursor: null,
  warnings: [],
  partial: false,
  requestId: null,
  error: null,
  loadingMore: false,
  lastSuccessMs: null,
};

const isPartial = (m: Meta) => m.partial || m.failed_shards.length > 0;

export function useSearchPages<T>(
  queryKey: string | null,
  request: SearchRequest | null,
  refreshToken: number,
  fetcher: (body: SearchRequest, signal: AbortSignal) => Promise<ApiResult<T[]>>,
) {
  const [state, setState] = useState<SearchPagesState<T>>(initial);
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;
  const ctrl = useRef<AbortController | null>(null);
  const reqRef = useRef(request);
  reqRef.current = request;

  useEffect(() => {
    ctrl.current?.abort();
    const req = reqRef.current;
    if (queryKey === null || req === null) {
      setState(initial);
      return;
    }
    const c = new AbortController();
    ctrl.current = c;
    setState({ ...initial, status: 'loading' });
    fetcherRef
      .current(req, c.signal)
      .then((r) => {
        if (c.signal.aborted) return;
        setState(
          r.ok
            ? {
                ...initial,
                status: 'success',
                rows: r.data,
                nextCursor: r.nextCursor,
                warnings: r.meta.warnings,
                partial: isPartial(r.meta),
                requestId: r.meta.request_id,
                lastSuccessMs: Date.now(),
              }
            : { ...initial, status: 'error', error: { status: r.status, body: r.error, retryAfterSeconds: r.retryAfterSeconds } },
        );
      })
      .catch(() => undefined);
    return () => c.abort();
  }, [queryKey, refreshToken]);

  const loadMore = useCallback(() => {
    const req = reqRef.current;
    const cursor = state.nextCursor;
    if (req === null || cursor === null || state.loadingMore) return;
    const c = new AbortController();
    ctrl.current = c;
    setState((s) => ({ ...s, loadingMore: true }));
    fetcherRef
      .current({ ...req, cursor }, c.signal)
      .then((r) => {
        if (c.signal.aborted) return;
        setState((s) =>
          r.ok
            ? { ...s, rows: [...s.rows, ...r.data], nextCursor: r.nextCursor, warnings: r.meta.warnings, partial: s.partial || isPartial(r.meta), loadingMore: false, error: null, lastSuccessMs: Date.now() }
            : { ...s, loadingMore: false, error: { status: r.status, body: r.error, retryAfterSeconds: r.retryAfterSeconds } },
        );
      })
      .catch(() => undefined);
  }, [state.nextCursor, state.loadingMore]);

  return { state, loadMore };
}
