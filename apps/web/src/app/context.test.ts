import { describe, expect, it } from 'vitest';
import {
  DEFAULT_PRESET,
  formatRange,
  parseContext,
  resolveRange,
  shareSearchParams,
  writeContext,
  type InvestigationContext,
} from './context.ts';

const TZ = 'Asia/Seoul';
const NOW_MS = Date.parse('2026-10-04T06:00:00Z'); // 15:00 KST

const parse = (q: string) => parseContext(new URLSearchParams(q), TZ, NOW_MS);

describe('parseContext', () => {
  it('값이 없으면 전체 환경·최근 1시간·기본 timezone', () => {
    const { context, errors } = parse('');
    expect(errors).toEqual([]);
    expect(context).toEqual({ environment: null, range: { kind: 'relative', preset: DEFAULT_PRESET }, timeZone: TZ });
  });

  it('절대 구간과 environment·timezone을 읽는다', () => {
    const { context, errors } = parse('env=prod&from=2026-10-04T05:00:00Z&to=2026-10-04T06:00:00Z&tz=UTC');
    expect(errors).toEqual([]);
    expect(context.environment).toBe('prod');
    expect(context.range).toEqual({ kind: 'absolute', fromMs: NOW_MS - 3_600_000, toMs: NOW_MS });
    expect(context.timeZone).toBe('UTC');
  });

  it('시작이 끝보다 늦으면 오류와 함께 기본 구간', () => {
    const { context, errors } = parse('from=2026-10-04T06:00:00Z&to=2026-10-04T05:00:00Z');
    expect(errors.map((e) => e.field)).toEqual(['range']);
    expect(context.range).toEqual({ kind: 'relative', preset: DEFAULT_PRESET });
  });

  it('timezone 없는 시각은 브라우저 local로 해석하지 않고 거절', () => {
    const { errors } = parse('from=2026-10-04T05:00:00&to=2026-10-04T06:00:00');
    expect(errors.map((e) => e.field)).toEqual(['range']);
  });

  it('한쪽만 있는 구간은 거절', () => {
    expect(parse('from=2026-10-04T05:00:00Z').errors.map((e) => e.field)).toEqual(['range']);
  });

  it('끝 시각이 미래(5분 초과)면 거절, 5분 이내 시계 차이는 허용', () => {
    expect(parse('from=2026-10-04T05:00:00Z&to=2026-10-04T06:10:00Z').errors.map((e) => e.field)).toEqual(['range']);
    expect(parse('from=2026-10-04T05:00:00Z&to=2026-10-04T06:04:00Z').errors).toEqual([]);
  });

  it('가장 긴 보존 기간(395일)보다 긴 구간은 거절', () => {
    expect(parse('from=0001-01-01T00:00:00Z&to=2026-10-04T06:00:00Z').errors.map((e) => e.field)).toEqual(['range']);
    expect(parse('from=2025-10-05T06:00:00Z&to=2026-10-04T06:00:00Z').errors).toEqual([]);
  });

  it('environment는 서버와 같은 1..64 byte, 공백·한글 허용, 제어 문자 거절', () => {
    expect(parse('env=pro%20d').context.environment).toBe('pro d');
    expect(parse('env=%ED%94%84%EB%A1%9C%EB%93%9C').context.environment).toBe('프로드');
    expect(parse(`env=${'a'.repeat(64)}`).errors).toEqual([]);
    expect(parse(`env=${'a'.repeat(65)}`).errors.map((e) => e.field)).toEqual(['env']);
    expect(parse(`env=${'가'.repeat(22)}`).errors.map((e) => e.field)).toEqual(['env']); // 66 byte
    expect(parse('env=a%0Ab').errors.map((e) => e.field)).toEqual(['env']);
  });

  it('알 수 없는 preset·environment·timezone은 각각 오류', () => {
    const { context, errors } = parse('range=99y&env=&tz=Mars/Base');
    expect(errors.map((e) => e.field).sort()).toEqual(['env', 'range', 'tz']);
    expect(context).toEqual({ environment: null, range: { kind: 'relative', preset: DEFAULT_PRESET }, timeZone: TZ });
  });

  it('오류 메시지에 긴 입력을 그대로 싣지 않는다', () => {
    const { errors } = parse(`range=${'x'.repeat(500)}`);
    expect(errors[0]?.message.length).toBeLessThan(80);
  });
});

describe('resolveRange', () => {
  it('상대 구간은 now 기준', () => {
    expect(resolveRange({ kind: 'relative', preset: '15m' }, NOW_MS)).toEqual({ fromMs: NOW_MS - 900_000, toMs: NOW_MS });
  });
});

describe('writeContext', () => {
  it('context 값만 바꾸고 화면의 다른 query는 유지', () => {
    const ctx: InvestigationContext = { environment: 'staging', range: { kind: 'relative', preset: '4h' }, timeZone: TZ };
    const next = writeContext(new URLSearchParams('tab=resources&from=x&to=y'), ctx);
    expect(next.get('tab')).toBe('resources');
    expect(next.get('env')).toBe('staging');
    expect(next.get('range')).toBe('4h');
    expect(next.has('from')).toBe(false);
  });
});

describe('shareSearchParams', () => {
  it('상대 구간을 지금 기준 UTC 절대 구간으로 고정', () => {
    const ctx = parse('env=prod&range=1h').context;
    const shared = shareSearchParams(new URLSearchParams('env=prod&range=1h'), ctx, NOW_MS);
    expect(shared.get('from')).toBe('2026-10-04T05:00:00.000Z');
    expect(shared.get('to')).toBe('2026-10-04T06:00:00.000Z');
    expect(shared.has('range')).toBe(false);
    expect(shared.get('tz')).toBe(TZ);
  });

  it('filter ID·tab·entity 외의 query는 공유 URL에 넣지 않는다', () => {
    const params = new URLSearchParams('filter=f_123&tab=spans&entity=svc_1&q=user%40example.com&token=abc');
    const shared = shareSearchParams(params, parse('').context, NOW_MS);
    expect([...shared.keys()].sort()).toEqual(['entity', 'filter', 'from', 'tab', 'to', 'tz']);
  });

  it('ID 형식이 아닌 filter·tab·entity는 공유 링크에서 뺀다', () => {
    const params = new URLSearchParams();
    params.set('filter', 'service:checkout AND user@example.com');
    params.set('tab', 'Spans<script>');
    params.set('entity', 'x'.repeat(200));
    const shared = shareSearchParams(params, parse('').context, NOW_MS);
    expect([...shared.keys()].sort()).toEqual(['from', 'to', 'tz']);
  });
});

describe('formatRange', () => {
  it('같은 날은 끝 시각만, timezone 약어 포함', () => {
    expect(formatRange({ kind: 'relative', preset: '1h' }, NOW_MS, 'UTC')).toBe('10월 4일 05:00–06:00 UTC');
  });
});
