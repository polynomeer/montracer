import type { Reason } from './red.ts';

// 값이 없는 이유를 사람이 읽는 말로 (D05 §03: empty 사유 구분, 0으로 보이지 않게).
export const REASON_LABEL: Record<Reason, string> = {
  no_data: '데이터 없음',
  pending: '집계 중',
  no_requests: '요청 없음',
  no_series: '받은 metric 없음',
  bounds_mismatch: 'bucket 경계 불일치',
  unit_conflict: '단위 충돌',
  type_conflict: '유형 충돌',
  not_applicable: '해당 없음',
  missing_baseline: '기준점 없음',
};

const num = (digits: number) =>
  new Intl.NumberFormat('ko-KR', { maximumFractionDigits: digits, minimumFractionDigits: 0 });

export function formatRate(perSecond: number): string {
  if (perSecond === 0) return '0';
  if (perSecond < 0.01) return '<0.01';
  return num(perSecond < 10 ? 2 : 1).format(perSecond);
}

export function formatPercent(ratio: number): string {
  if (ratio === 0) return '0';
  const pct = ratio * 100;
  if (pct < 0.01) return '<0.01';
  return num(2).format(pct);
}

export function formatMilliseconds(ms: number): string {
  if (ms >= 10_000) return `${num(1).format(ms / 1000)} s`;
  if (ms >= 1) return `${num(ms < 10 ? 1 : 0).format(ms)} ms`;
  return `${num(2).format(ms)} ms`;
}

export function formatSeconds(seconds: number): string {
  if (seconds >= 3600) return `${num(1).format(seconds / 3600)} h`;
  if (seconds >= 60) return `${num(1).format(seconds / 60)} min`;
  return `${num(seconds < 10 ? 2 : 1).format(seconds)} s`;
}

export function formatClock(tMs: number, timeZone: string, withDate = false): string {
  return new Intl.DateTimeFormat('ko-KR', {
    timeZone,
    hour: '2-digit',
    minute: '2-digit',
    hourCycle: 'h23',
    ...(withDate ? { month: 'numeric', day: 'numeric' } : {}),
  }).format(tMs);
}

export function formatDateTime(iso: string, timeZone: string): string {
  return new Intl.DateTimeFormat('ko-KR', {
    timeZone,
    month: 'long',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hourCycle: 'h23',
  }).format(Date.parse(iso));
}
