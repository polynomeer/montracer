import { describe, expect, it } from 'vitest';
import type { ServiceHealthData, ServiceHealthItem } from '../../api/serviceHealth.ts';
import { healthVerdicts, verdictFor, verdictText, type Verdict } from './serviceHealth.tsx';

const item = (over: Partial<ServiceHealthItem>): ServiceHealthItem => ({
  service_namespace: 'shop',
  service_name: 'checkout',
  environment: 'prod',
  state: 'OK',
  stale: false,
  state_since: '2026-10-04T05:00:00Z',
  monitor_count: 1,
  name_only: false,
  includes_all_environment_monitors: false,
  partial: false,
  monitors: [{ id: 'm1', name: 'a', state: 'OK', stale: false, all_environments: false }],
  ...over,
});
const data = (services: ServiceHealthItem[]): ServiceHealthData => ({ services, unattributed: [] });
const key = { namespace: 'shop', name: 'checkout', environment: 'prod' };

describe('서비스 판정 맞추기 (ADR 0073)', () => {
  it('정확한 행·이름만 행·전체 환경 행을 합쳐 가장 나쁜 상태, 같은 상태의 가장 이른 시작 시각', () => {
    const v = verdictFor(key, data([
      item({}),
      item({ service_namespace: null, state: 'ALERT', state_since: '2026-10-04T05:20:00Z', monitors: [{ id: 'm2', name: 'b', state: 'ALERT', stale: false, all_environments: false }] }),
      item({ environment: null, state: 'ALERT', state_since: '2026-10-04T05:10:00Z', monitors: [{ id: 'm3', name: 'c', state: 'ALERT', stale: false, all_environments: true }] }),
      item({ service_namespace: 'other', state: 'ALERT' }), // 다른 namespace는 아니다
      item({ environment: 'stage', state: 'ALERT' }), // 다른 환경은 아니다
    ]), []);
    expect(v.kind === 'state' && v.state).toBe('ALERT');
    expect(v.kind === 'state' && v.since).toBe('2026-10-04T05:10:00Z');
    expect(v.kind === 'state' && v.allEnvironments).toBe(true);
    expect(v.kind === 'state' && v.monitors.map((m) => m.id)).toEqual(['m2', 'm3', 'm1']);
  });

  it('같은 최악 상태 중 시작 시각을 모르는 행이 있으면 시각 없음, 멈춘 정상은 진행 중보다 나쁘다', () => {
    let v = verdictFor(key, data([item({ state: 'ALERT' }), item({ environment: null, state: 'ALERT', state_since: null })]), []);
    expect(v.kind === 'state' && v.since).toBeNull();
    v = verdictFor(key, data([item({ state: 'OK', stale: true }), item({ environment: null, state: 'PENDING' })]), []);
    expect(v.kind === 'state' && v.state).toBe('OK');
    expect(v.kind === 'state' && v.stale).toBe(true);
    expect(verdictText(v, 'UTC')).toBe('정상 · 갱신 멈춤 · 05:00부터 · 전체 환경 monitor 포함');
  });

  it('걸린 것이 없으면 판정 없음, 잘렸으면 확인할 수 없음. 잘린 응답의 경보는 하한이라 그대로', () => {
    expect(verdictFor(key, data([]), []).kind).toBe('none');
    expect(verdictFor(key, data([]), ['monitors_truncated'])).toEqual({ kind: 'unknown', reason: 'partial' });
    expect(verdictFor(key, data([item({ partial: true })]), [])).toEqual({ kind: 'unknown', reason: 'partial' });
    const v = verdictFor(key, data([item({ partial: true, state: 'EVALUATION_ERROR' })]), ['groups_truncated']);
    expect(v.kind === 'state' && v.state).toBe('EVALUATION_ERROR');
  });

  it('모르는 상태는 가장 나쁘게, 글은 원문으로', () => {
    const v = verdictFor(key, data([item({ state: 'ALERT' }), item({ environment: null, state: 'MUTED_X' })]), []);
    expect(v.kind === 'state' && v.state).toBe('MUTED_X');
    expect(verdictText(v, 'UTC', true)).toBe('MUTED_X · 05:00부터');
  });

  it('빈 환경 값의 행은 그 환경 서비스에만', () => {
    expect(verdictFor({ ...key, environment: '' }, data([item({ environment: '', state: 'ALERT' })]), []).kind).toBe('state');
    expect(verdictFor(key, data([item({ environment: '', state: 'ALERT' })]), []).kind).toBe('none');
  });
});

describe('리뷰 반영: 잘림·멈춤·조회 상태', () => {
  it('잘린 응답의 하한 상태는 시작 시각 없이 "일부 monitor만 반영", 멈춘 경보도 갱신 멈춤, 여러 행이면 monitor 수는 최소값', () => {
    const v = verdictFor(key, data([item({ state: 'ALERT', partial: true, stale: true }), item({ environment: null, state: 'OK' })]), ['groups_truncated']);
    expect(v.kind === 'state' && v.since).toBeNull();
    expect(v.kind === 'state' && v.countIsMinimum).toBe(true);
    expect(verdictText(v, 'UTC')).toBe('경보 · 갱신 멈춤 · 일부 monitor만 반영 · 전체 환경 monitor 포함');
  });

  it('healthVerdicts: 불러오는 중, 404는 기능 없음, 403·연결 오류, 갱신 실패는 지난 값을 숨기고 마지막 성공 시각', () => {
    const base = { status: 'success' as const, data: null, meta: null, nextCursor: null, error: null, refreshing: false, lastSuccessMs: null };
    const err = (status: number) => ({ status, body: { code: 'UNAVAILABLE' as const, message: 'x', request_id: 'r', retryable: true }, retryAfterSeconds: null });
    expect(healthVerdicts({ ...base, status: 'loading' })?.(key)).toEqual({ kind: 'unknown', reason: 'loading' });
    expect(healthVerdicts({ ...base, status: 'error', error: err(404) })).toBeNull();
    expect(healthVerdicts({ ...base, status: 'error', error: err(403) })?.(key)).toEqual({ kind: 'unknown', reason: 'forbidden' });
    expect(healthVerdicts({ ...base, status: 'error', error: err(0) })?.(key)).toEqual({ kind: 'unknown', reason: 'failed' });
    const last = Date.parse('2026-10-04T05:58:00Z');
    const v = healthVerdicts({ ...base, status: 'error', data: data([item({ state: 'OK' })]), error: err(503), lastSuccessMs: last })?.(key);
    expect(v).toEqual({ kind: 'unknown', reason: 'failed', lastSuccessMs: last });
    expect(verdictText(v as Verdict, 'UTC')).toBe('확인할 수 없음 · 상태 조회 실패 (마지막 성공 05:58)');
  });
});
