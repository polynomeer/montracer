// 상태 표시 (D05 §03 상태 계약, §04 HTTP 상태별 처리).
import type { ReactNode } from 'react';
import type { RemoteState } from '../../api/useRemote.ts';

export type Tone = 'success' | 'warning' | 'critical' | 'info' | 'neutral';

const ICON: Record<Tone, ReactNode> = {
  success: <path d="M5 8.2l2 2 4-4.2" />,
  warning: (
    <>
      <path d="M8 4.5V8.5" />
      <path d="M8 11v.01" />
    </>
  ),
  critical: <path d="M5.5 5.5l5 5M10.5 5.5l-5 5" />,
  info: (
    <>
      <path d="M8 7v4" />
      <path d="M8 4.8v.01" />
    </>
  ),
  neutral: <path d="M5 8h6" />,
};

/** 색 + 아이콘 + 문자 (D05 §03 StatusBadge). 색만으로 상태를 전달하지 않는다. */
export function StatusBadge({ tone, children }: { tone: Tone; children: ReactNode }) {
  return (
    <span className={`mt-badge mt-badge--${tone}`}>
      <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6" aria-hidden="true">
        <circle cx="8" cy="8" r="6.5" strokeDasharray={tone === 'neutral' ? '2 2' : undefined} />
        {ICON[tone]}
      </svg>
      {children}
    </span>
  );
}

export function SourceBadge({ children }: { children: ReactNode }) {
  return <span className="mt-source-badge">{children}</span>;
}

/** API 오류를 D05 §04 규칙의 문장으로. request ID를 함께 보인다(지원 요청용). */
export function describeError(e: NonNullable<RemoteState<unknown>['error']>): { title: string; detail: string } {
  switch (e.status) {
    case 401:
      return {
        title: '인증이 필요합니다',
        detail: '로그인 연동 전입니다. 로컬에서는 make seed로 key를 만든 뒤 web dev server를 다시 시작하세요.',
      };
    case 403:
      return { title: '이 정보를 볼 권한이 없습니다', detail: '조직 관리자에게 telemetry 조회 권한을 요청하세요.' };
    case 404:
      return { title: '찾을 수 없습니다', detail: '주소가 바뀌었거나 볼 수 있는 권한이 없습니다.' };
    case 422:
      return { title: '조회 범위를 줄이세요', detail: e.body.message };
    case 429:
      return {
        title: '요청이 많습니다',
        detail: e.retryAfterSeconds === null ? '잠시 후 다시 시도하세요.' : `${String(e.retryAfterSeconds)}초 후 다시 시도하세요.`,
      };
    case 0:
      return { title: '서버에 연결할 수 없습니다', detail: 'query-api가 실행 중인지 확인하세요 (make dev).' };
    default:
      return e.status >= 500
        ? { title: '일시적으로 조회할 수 없습니다', detail: '잠시 후 다시 시도하세요.' }
        : { title: '요청을 처리하지 못했습니다', detail: e.body.message };
  }
}

export function ErrorNotice({
  error,
  lastSuccessMs,
  onRetry,
}: {
  error: NonNullable<RemoteState<unknown>['error']>;
  lastSuccessMs: number | null;
  onRetry?: () => void;
}) {
  const { title, detail } = describeError(error);
  return (
    <div className="mt-notice mt-notice--critical" role="alert">
      <strong>{title}</strong>
      <span>{detail}</span>
      {lastSuccessMs !== null && (
        <span className="mt-label">
          마지막 성공 {new Date(lastSuccessMs).toLocaleTimeString('ko-KR', { hourCycle: 'h23' })} 기준 데이터를 보여주고 있습니다.
        </span>
      )}
      {error.body.request_id !== '' && <span className="mt-label mt-mono">request_id {error.body.request_id}</span>}
      {onRetry !== undefined && error.status !== 401 && error.status !== 403 && (
        <button type="button" className="mt-button" onClick={onRetry}>
          다시 시도
        </button>
      )}
    </div>
  );
}

export function Skeleton({ height = 120, label }: { height?: number; label: string }) {
  return <div className="mt-skeleton" style={{ height }} role="status" aria-label={label} />;
}
