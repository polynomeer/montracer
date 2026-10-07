// 조사 context(environment·시간 범위·timezone)를 URL과 주고받는다 (D05 §01 전역 context).
// - environment·time 변경은 현재 화면을 유지한다. 조직은 경로(/o/{org})에 있고 여기서 다루지 않는다.
// - 상대시간은 live 조사용이다. 공유 링크는 절대시간(UTC)으로 고정하고,
//   context·filter ID·선택 tab·entity 외의 query 값은 공유 URL에 넣지 않는다.

export const RELATIVE_PRESETS = {
  '15m': 15 * 60_000,
  '1h': 60 * 60_000,
  '4h': 4 * 60 * 60_000,
  '1d': 24 * 60 * 60_000,
  '7d': 7 * 24 * 60 * 60_000,
} as const;

export type RelativePreset = keyof typeof RELATIVE_PRESETS;

export const DEFAULT_PRESET: RelativePreset = '1h';

export type TimeRange =
  | { kind: 'relative'; preset: RelativePreset }
  | { kind: 'absolute'; fromMs: number; toMs: number };

export interface InvestigationContext {
  environment: string | null;
  range: TimeRange;
  timeZone: string;
}

export type ContextField = 'env' | 'range' | 'tz';

export interface ContextError {
  field: ContextField;
  message: string;
}

export interface ParsedContext {
  context: InvestigationContext;
  /** 잘못된 값은 기본값으로 대체하고 이유를 남긴다. 화면은 이 오류를 입력 옆에 표시한다. */
  errors: ContextError[];
}

// 서버 규칙과 같다: 1..64 byte (internal/authz maxEnvironmentLen). 제어 문자는 받지 않는다.
const MAX_ENVIRONMENT_BYTES = 64;
const CONTROL_CHARS = /[\u0000-\u001f\u007f]/;

function isValidEnvironment(v: string): boolean {
  const bytes = new TextEncoder().encode(v).length;
  return bytes >= 1 && bytes <= MAX_ENVIRONMENT_BYTES && !CONTROL_CHARS.test(v);
}

// 끝 시각은 지금보다 5분까지만 앞설 수 있다 (시계 차이 허용).
export const MAX_FUTURE_SKEW_MS = 5 * 60_000;
// 가장 긴 보존 기간(metric_1h 395일, ADR 0028)보다 긴 구간은 조회할 데이터가 없다.
export const MAX_SPAN_MS = 395 * 24 * 60 * 60_000;

// 공유 URL에 남길 수 있는 화면 상태 (D05 §01: filter ID, 선택 tab·entity).
// ID 형식만 받는다. 검색어 원문 같은 자유 입력은 이 키에 들어와도 공유 링크로 나가지 않는다.
const SHAREABLE: Record<'filter' | 'tab' | 'entity', RegExp> = {
  filter: /^[A-Za-z0-9_-]{1,64}$/,
  tab: /^[a-z0-9-]{1,32}$/,
  entity: /^[A-Za-z0-9._:-]{1,128}$/,
};

function isPreset(v: string): v is RelativePreset {
  return Object.hasOwn(RELATIVE_PRESETS, v);
}

// 명시적 timezone(Z 또는 ±hh:mm)이 있는 ISO 8601만 받는다. 브라우저 local 해석을 막는다.
const ISO_WITH_ZONE = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d{1,3})?)?(Z|[+-]\d{2}:\d{2})$/;

function parseInstantMs(v: string): number | null {
  if (!ISO_WITH_ZONE.test(v)) return null;
  const ms = Date.parse(v);
  return Number.isFinite(ms) ? ms : null;
}

export function isValidTimeZone(tz: string): boolean {
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: tz });
    return true;
  } catch {
    return false;
  }
}

export function browserTimeZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone;
}

export function parseContext(params: URLSearchParams, defaultTimeZone: string, nowMs: number): ParsedContext {
  const errors: ContextError[] = [];

  let environment: string | null = null;
  const env = params.get('env');
  if (env !== null) {
    if (isValidEnvironment(env)) {
      environment = env;
    } else {
      errors.push({ field: 'env', message: '환경 이름은 1~64 byte여야 합니다.' });
    }
  }

  let range: TimeRange = { kind: 'relative', preset: DEFAULT_PRESET };
  const from = params.get('from');
  const to = params.get('to');
  if (from !== null || to !== null) {
    const fromMs = from === null ? null : parseInstantMs(from);
    const toMs = to === null ? null : parseInstantMs(to);
    if (fromMs === null || toMs === null) {
      errors.push({ field: 'range', message: '시작·끝 시각은 timezone이 있는 ISO 8601이어야 합니다 (예: 2026-10-04T05:00:00Z).' });
    } else if (fromMs >= toMs) {
      errors.push({ field: 'range', message: '시작 시각이 끝 시각보다 앞서야 합니다.' });
    } else if (toMs > nowMs + MAX_FUTURE_SKEW_MS) {
      errors.push({ field: 'range', message: '끝 시각이 미래입니다.' });
    } else if (toMs - fromMs > MAX_SPAN_MS) {
      errors.push({ field: 'range', message: '구간은 395일(가장 긴 보존 기간) 이내여야 합니다.' });
    } else {
      range = { kind: 'absolute', fromMs, toMs };
    }
  } else {
    const preset = params.get('range');
    if (preset !== null) {
      if (isPreset(preset)) {
        range = { kind: 'relative', preset };
      } else {
        errors.push({ field: 'range', message: `알 수 없는 시간 범위입니다: ${preset.slice(0, 20)}` });
      }
    }
  }

  let timeZone = defaultTimeZone;
  const tz = params.get('tz');
  if (tz !== null) {
    if (isValidTimeZone(tz)) {
      timeZone = tz;
    } else {
      errors.push({ field: 'tz', message: '알 수 없는 timezone입니다.' });
    }
  }

  return { context: { environment, range, timeZone }, errors };
}

export function resolveRange(range: TimeRange, nowMs: number): { fromMs: number; toMs: number } {
  if (range.kind === 'absolute') return { fromMs: range.fromMs, toMs: range.toMs };
  return { fromMs: nowMs - RELATIVE_PRESETS[range.preset], toMs: nowMs };
}

/** 현재 query를 유지한 채 context 값만 바꾼다 (화면 이동 없음). */
export function writeContext(params: URLSearchParams, ctx: InvestigationContext): URLSearchParams {
  const next = new URLSearchParams(params);
  for (const k of ['env', 'range', 'from', 'to', 'tz']) next.delete(k);
  if (ctx.environment !== null) next.set('env', ctx.environment);
  if (ctx.range.kind === 'relative') {
    next.set('range', ctx.range.preset);
  } else {
    next.set('from', new Date(ctx.range.fromMs).toISOString());
    next.set('to', new Date(ctx.range.toMs).toISOString());
  }
  next.set('tz', ctx.timeZone);
  return next;
}

/** 공유 링크용 query. 상대시간은 지금 기준 절대시간(UTC)으로 고정하고, 허용된 키만 남긴다. */
export function shareSearchParams(params: URLSearchParams, ctx: InvestigationContext, nowMs: number): URLSearchParams {
  const { fromMs, toMs } = resolveRange(ctx.range, nowMs);
  const out = writeContext(new URLSearchParams(), { ...ctx, range: { kind: 'absolute', fromMs, toMs } });
  for (const [k, pattern] of Object.entries(SHAREABLE)) {
    const v = params.get(k);
    if (v !== null && pattern.test(v)) out.set(k, v);
  }
  return out;
}

export function formatRange(range: TimeRange, nowMs: number, timeZone: string): string {
  const { fromMs, toMs } = resolveRange(range, nowMs);
  const day = new Intl.DateTimeFormat('ko-KR', { timeZone, month: 'long', day: 'numeric' });
  const time = new Intl.DateTimeFormat('ko-KR', { timeZone, hour: '2-digit', minute: '2-digit', hourCycle: 'h23' });
  const zone =
    new Intl.DateTimeFormat('en-US', { timeZone, timeZoneName: 'short' })
      .formatToParts(toMs)
      .find((p) => p.type === 'timeZoneName')?.value ?? timeZone;
  const sameDay = day.format(fromMs) === day.format(toMs);
  const start = `${day.format(fromMs)} ${time.format(fromMs)}`;
  const end = sameDay ? time.format(toMs) : `${day.format(toMs)} ${time.format(toMs)}`;
  return `${start}–${end} ${zone}`;
}
