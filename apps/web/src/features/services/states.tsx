// 상태 표시 (D05 §03 상태 계약, §04 HTTP 상태별 처리).
import { useEffect, useRef, type ReactNode, type Ref } from 'react';
import type { RemoteState } from '../../api/useRemote.ts';
import { formatTimeOfDay } from './format.ts';

export type Tone = 'success' | 'warning' | 'critical' | 'info' | 'neutral';

/**
 * 배지 아이콘 (디자인 기초 StatusBadge). 상태마다 모양이 달라 색을 구별하지 못해도 읽힌다.
 * - `dashed`: 데이터 없음(NO_DATA) — 점선 원. "판정 없음"(`minus`)과 모양으로 구별한다.
 * - `triangle`: 평가 실패(EVALUATION_ERROR). `refresh`: 회복 중. `clock`: stale. `pie`: partial. `dots`: 표본.
 */
export type BadgeIcon = 'check' | 'exclaim' | 'cross' | 'info' | 'minus' | 'dashed' | 'triangle' | 'refresh' | 'clock' | 'pie' | 'dots' | 'none';

/** `dashed`: 점선 테두리(값이 없음·추정). `emphasis`: 바탕 없이 진한 실선 테두리(평가 실패처럼 판정 자체가 실패). */
export type BadgeVariant = 'default' | 'dashed' | 'emphasis';

const DEFAULT_ICON: Record<Tone, BadgeIcon> = {
  success: 'check',
  warning: 'exclaim',
  critical: 'cross',
  info: 'info',
  neutral: 'minus',
};

const CIRCLE = <circle cx="8" cy="8" r="6.5" />;

function Glyph({ icon }: { icon: BadgeIcon }) {
  switch (icon) {
    case 'check':
      return <>{CIRCLE}<path d="M5 8.2l2 2 4-4.2" /></>;
    case 'exclaim':
      return <>{CIRCLE}<path d="M8 4.5V8" /><path d="M8 11v.01" /></>;
    case 'cross':
      return <>{CIRCLE}<path d="M5.5 5.5l5 5M10.5 5.5l-5 5" /></>;
    case 'info':
      return <>{CIRCLE}<path d="M8 7v4" /><path d="M8 4.8v.01" /></>;
    case 'minus':
      return <>{CIRCLE}<path d="M5 8h6" /></>;
    case 'dashed':
      return <circle cx="8" cy="8" r="6.5" strokeDasharray="2 2" />;
    case 'triangle':
      return <><path d="M8 2l6.5 11.5h-13z" /><path d="M8 6.5v3" /><path d="M8 11.5v.01" /></>;
    case 'refresh':
      return <><path d="M13 8a5 5 0 1 1-1.5-3.6" /><path d="M13 3v2.5h-2.5" /></>;
    case 'clock':
      return <>{CIRCLE}<path d="M8 4.5V8l2.5 1.5" /></>;
    case 'pie':
      return <path d="M8 1.5a6.5 6.5 0 1 0 6.5 6.5H8z" />;
    case 'dots':
      return <><circle cx="4" cy="8" r="1.2" /><circle cx="8" cy="5" r="1.2" /><circle cx="12" cy="10" r="1.2" /></>;
    case 'none':
      return null;
  }
}

/** 색 + 아이콘 + 문자 (D05 §03 StatusBadge). 색만으로 상태를 전달하지 않는다. */
export function StatusBadge({
  tone,
  icon,
  variant = 'default',
  suffix,
  children,
}: {
  tone: Tone;
  /** 생략하면 tone의 기본 아이콘. */
  icon?: BadgeIcon;
  variant?: BadgeVariant;
  /** 구분선 뒤에 흐리게 붙는 보조 문구 (예: "MUTED · 16:00까지"). */
  suffix?: ReactNode;
  children: ReactNode;
}) {
  const glyph = icon ?? DEFAULT_ICON[tone];
  const cls = `mt-badge mt-badge--${tone}${variant === 'default' ? '' : ` mt-badge--${variant}`}`;
  return (
    <span className={cls}>
      {glyph !== 'none' && (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <Glyph icon={glyph} />
        </svg>
      )}
      {children}
      {suffix !== undefined && <span className="mt-badge__suffix">{suffix}</span>}
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
  timeZone,
  onRetry,
}: {
  error: NonNullable<RemoteState<unknown>['error']>;
  lastSuccessMs: number | null;
  /** 조사 context 시간대(`?tz=`). 같은 화면의 다른 시각과 같은 기준으로 마지막 성공 시각을 보인다. */
  timeZone: string;
  onRetry?: () => void;
}) {
  const { title, detail } = describeError(error);
  return (
    <div className="mt-notice mt-notice--critical" role="alert">
      <strong>{title}</strong>
      <span>{detail}</span>
      {lastSuccessMs !== null && (
        <span className="mt-label">
          마지막 성공 {formatTimeOfDay(lastSuccessMs, timeZone)} 기준 데이터를 보여주고 있습니다.
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

/** 비어 있는 이유 (디자인 기초 Empty state). 이유마다 다음 행동이 다르다 — "없음"을 한 가지로 뭉치지 않는다. */
export type EmptyReason = 'new' | 'filtered' | 'expired' | 'unsupported' | 'planned' | 'forbidden' | 'disabled' | 'unknown';

const EMPTY_KICKER: Record<EmptyReason, string> = {
  new: '아직 없음',
  filtered: '조건에 맞는 결과 없음',
  expired: '보존 기간 지남',
  unsupported: '지원하지 않는 계측',
  planned: '준비 중인 기능',
  forbidden: '권한 없음',
  disabled: '이 배포에서 꺼짐',
  unknown: '원인 확인 필요',
};

/** 비어 있는 상태. 숫자 0이나 빈 표 대신 이유·설명·다음 행동을 보인다(D05 §03, 계약 6). */
export function EmptyState({
  reason,
  title,
  children,
  action,
  requestId,
  inline = false,
}: {
  reason?: EmptyReason;
  title: ReactNode;
  children?: ReactNode;
  /** 다음 행동 (Link·button). */
  action?: ReactNode;
  /** 원인을 알 수 없을 때 지원 요청에 쓰는 request ID. */
  requestId?: string;
  /** 카드 안에 들어가는 경우 테두리 없이. */
  inline?: boolean;
}) {
  return (
    <div className={inline ? 'mt-empty mt-empty--inline' : 'mt-empty'}>
      {reason !== undefined && <span className="mt-empty__kicker">{EMPTY_KICKER[reason]}</span>}
      <strong className="mt-empty__title">{title}</strong>
      {children !== undefined && <span className="mt-empty__body">{children}</span>}
      {requestId !== undefined && requestId !== '' && <span className="mt-label mt-mono">request_id {requestId}</span>}
      {action !== undefined && <div className="mt-empty__action">{action}</div>}
    </div>
  );
}

/**
 * 되돌리기 어려운 작업의 확인 (디자인 기초 ConfirmDialog). 화면 흐름을 막지 않는 inline alertdialog다.
 * 열 때 확인 버튼(또는 호출자가 고른 요소)으로, 닫으면 연 버튼으로 focus를 옮기는 일은 호출자가 한다.
 */
export function ConfirmDialog({
  id,
  title,
  subtitle,
  facts,
  children,
  error,
  confirmLabel,
  busyLabel,
  tone = 'danger',
  busy,
  onConfirm,
  onCancel,
  confirmRef,
}: {
  id: string;
  title: ReactNode;
  /** 대상 식별자 (monospace). */
  subtitle?: ReactNode;
  /** 영향·범위 같은 사실 목록. */
  facts?: readonly { term: string; detail: ReactNode }[];
  children?: ReactNode;
  error?: string | null;
  confirmLabel: string;
  busyLabel?: string;
  tone?: 'danger' | 'primary';
  busy: boolean;
  onConfirm: () => void;
  onCancel: () => void;
  confirmRef?: Ref<HTMLButtonElement>;
}) {
  // 진행 중에는 두 버튼이 막혀 focus가 대화 밖으로 빠진다. 오류가 나면 대화로 되돌려 다시 시도·Esc가 되게 한다.
  const box = useRef<HTMLDivElement>(null);
  const hasError = error !== undefined && error !== null;
  useEffect(() => {
    if (hasError && !box.current?.contains(document.activeElement)) box.current?.focus();
  }, [hasError, error]);
  return (
    <div
      ref={box}
      tabIndex={-1}
      className="mt-confirm"
      role="alertdialog"
      aria-labelledby={`${id}-title`}
      aria-describedby={`${id}-body`}
      onKeyDown={(e) => {
        if (e.key === 'Escape' && !busy) onCancel();
      }}
    >
      <div className="mt-confirm__head">
        <strong id={`${id}-title`} className="mt-confirm__title">
          {title}
        </strong>
        {subtitle !== undefined && <span className="mt-label mt-mono">{subtitle}</span>}
      </div>
      <div id={`${id}-body`} className="mt-confirm__body">
        {facts !== undefined && facts.length > 0 && (
          <dl className="mt-confirm__facts">
            {facts.map((f) => (
              <div key={f.term}>
                <dt>{f.term}</dt>
                <dd>{f.detail}</dd>
              </div>
            ))}
          </dl>
        )}
        {children}
        {error !== undefined && error !== null && (
          <span className="mt-confirm__error" role="alert">
            {error}
          </span>
        )}
      </div>
      <div className="mt-confirm__foot">
        <button type="button" className="mt-button" onClick={onCancel} disabled={busy}>
          취소
        </button>
        <button ref={confirmRef} type="button" className={`mt-button mt-button--${tone}`} onClick={onConfirm} disabled={busy}>
          {busy && busyLabel !== undefined ? busyLabel : confirmLabel}
        </button>
      </div>
    </div>
  );
}

/** 적용된 조건 하나 (ChipBar). */
export interface ConditionChip {
  key: string;
  label: string;
  onRemove: () => void;
}

/**
 * 적용된 조건 chip 막대 (디자인 시안 query bar, S04·S08 공용). 하나씩·모두 지운다. 편집은 각 화면의 조건 패널에서 한다.
 * 조건이 없으면 무엇을 보고 있는지 글로 말한다 — 빈 막대로 두지 않는다.
 */
export function ChipBar({ chips, emptyText, onClearAll }: { chips: readonly ConditionChip[]; emptyText: string; onClearAll: () => void }) {
  // 누른 버튼이 사라져 focus가 body로 빠지지 않게 막대로 옮긴다(키보드 사용자, D05 §12)
  const bar = useRef<HTMLDivElement>(null);
  const keepFocus = (f: () => void) => () => {
    f();
    window.setTimeout(() => bar.current?.focus(), 0);
  };
  return (
    <div ref={bar} tabIndex={-1} className="mt-chipbar" role="group" aria-label="적용된 조건">
      {chips.length === 0 ? (
        <span className="mt-label">{emptyText}</span>
      ) : (
        <>
          {chips.map((c) => (
            <span key={c.key} className="mt-chip">
              <span className="mt-chip__label" title={c.label}>
                {c.label}
              </span>
              <button type="button" className="mt-chip__remove" aria-label={`조건 지우기: ${c.label}`} onClick={keepFocus(c.onRemove)}>
                ×
              </button>
            </span>
          ))}
          {chips.length > 1 && (
            <button type="button" className="mt-button mt-button--subtle" onClick={keepFocus(onClearAll)}>
              모두 지우기
            </button>
          )}
        </>
      )}
    </div>
  );
}
