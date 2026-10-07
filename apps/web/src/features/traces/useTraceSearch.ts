import { useCallback, useEffect, useRef, useState } from 'react';
import type { ApiErrorBody, SearchRequest, TraceSummary } from '../../api/types.ts';
import { searchTraces } from '../../api/traces.ts';

// trace 검색 결과 page들 (D05 §06: page 100, "더 보기"는 cursor). 조건(queryKey)이 바뀌면 이전 결과를 버리고
// 진행 중 요청을 취소한다 — 늦은 응답이 새 조건의 화면을 덮지 않는다(D05 §03).
export interface TraceSearchState {
  status: 'idle' | 'loading' | 'success' | 'error';
  rows: TraceSummary[];
  nextCursor: string | null;
  warnings: string[];
  requestId: string | null;
  error: { status: number; body: ApiErrorBody; retryAfterSeconds: number | null } | null;
  loadingMore: boolean;
  /** 마지막으로 page를 받은 시각(ms). 다음 page가 실패해도 남는다(D05 §03 last successful at). */
  lastSuccessMs: number | null;
}

const initial: TraceSearchState = {
  status: 'idle',
  rows: [],
  nextCursor: null,
  warnings: [],
  requestId: null,
  error: null,
  loadingMore: false,
  lastSuccessMs: null,
};

export function useTraceSearch(queryKey: string | null, request: SearchRequest | null, refreshToken: number) {
  const [state, setState] = useState<TraceSearchState>(initial);
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
    searchTraces(req, c.signal)
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
    searchTraces({ ...req, cursor }, c.signal)
      .then((r) => {
        if (c.signal.aborted) return;
        setState((s) =>
          r.ok
            ? { ...s, rows: [...s.rows, ...r.data], nextCursor: r.nextCursor, warnings: r.meta.warnings, loadingMore: false, error: null, lastSuccessMs: Date.now() }
            : { ...s, loadingMore: false, error: { status: r.status, body: r.error, retryAfterSeconds: r.retryAfterSeconds } },
        );
      })
      .catch(() => undefined);
  }, [state.nextCursor, state.loadingMore]);

  return { state, loadMore };
}
