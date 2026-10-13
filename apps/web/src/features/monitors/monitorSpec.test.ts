import { describe, expect, it } from 'vitest';
import type { MonitorSpec } from '../../api/monitors.ts';
import { cellOf } from './MonitorDetail.tsx';
import {
  describeCondition,
  describeFilter,
  describeNoData,
  emptyForm,
  fieldForPath,
  formToSpec,
  formatValue,
  groupLabel,
  specToForm,
  stateRank,
  stateTone,
} from './monitorSpec.ts';

const spec: MonitorSpec = {
  name: 'Checkout 오류율',
  enabled: true,
  query: {
    kind: 'error_ratio',
    metric: 'http.server.request.duration',
    aggregation: null,
    filter: { op: 'and', args: [{ op: 'eq', field: 'service.name', value: 'checkout' }] },
    group_by: ['http.route'],
  },
  window_seconds: 300,
  evaluation_seconds: 60,
  condition: { operator: 'gt', threshold: 0.025 },
  minimum_requests: 100,
  for_seconds: 120,
  recovery_evaluations: 2,
  no_data: { action: 'alert', after_seconds: 600 },
  notification_policy_id: '8d49d404-4115-42f8-9c59-f94ff40f7270',
};

describe('form ↔ spec', () => {
  it('오류율은 화면 %, API fraction이다(왕복)', () => {
    const f = specToForm(spec);
    expect(f.threshold).toBe('2.5');
    expect(f.service).toBe('checkout');
    expect(f.windowMinutes).toBe('5');
    expect(f.noDataAfterMinutes).toBe('10');
    const { spec: out, errors } = formToSpec(f);
    expect(errors).toEqual({});
    expect(out).toMatchObject({
      condition: { operator: 'gt', threshold: 0.025 },
      window_seconds: 300,
      for_seconds: 120,
      minimum_requests: 100,
      no_data: { action: 'alert', after_seconds: 600 },
      notification_policy_id: spec.notification_policy_id,
      query: { kind: 'error_ratio', filter: { op: 'and', args: [{ op: 'eq', field: 'service.name', value: 'checkout' }] }, group_by: ['http.route'] },
    });
  });

  it('metric은 값 그대로, 최소 요청 수 없음', () => {
    const f = { ...emptyForm(), name: 'p95', kind: 'metric' as const, metric: 'http.server.request.duration', aggregation: 'p95', threshold: '0.5' };
    const { spec: out } = formToSpec(f);
    expect(out).toMatchObject({ query: { kind: 'metric', metric: 'http.server.request.duration', aggregation: 'p95' }, condition: { threshold: 0.5 } });
    expect(out).not.toHaveProperty('minimum_requests');
    expect(out).toMatchObject({ no_data: { action: 'no_data' } });
  });

  it('바로 알 수 있는 오류는 보내기 전에 막는다(빈 값을 0으로 보내지 않는다)', () => {
    const bad = { ...emptyForm(), name: ' ', threshold: '', windowMinutes: '3', evaluationSeconds: 300, minimumRequests: '-1' };
    const { spec: out, errors } = formToSpec(bad);
    expect(out).toBeNull();
    expect(errors).toMatchObject({ name: expect.any(String), threshold: expect.any(String), windowMinutes: '평가 주기의 배수여야 합니다', minimumRequests: expect.any(String) });
    expect(formToSpec({ ...emptyForm(), name: 'x', threshold: '150' }).errors.threshold).toBe('0~100%');
    expect(formToSpec({ ...emptyForm(), name: 'x', kind: 'metric' }).errors.metric).toBeDefined();
  });

  it('서버 field violation 경로를 form 필드로', () => {
    expect(fieldForPath('condition.threshold')).toBe('threshold');
    expect(fieldForPath('query.filter.args[0].op')).toBe('service');
    expect(fieldForPath('no_data.after_seconds')).toBe('noDataAfterMinutes');
    expect(fieldForPath('notification_policy_id')).toBe('policyId');
    expect(fieldForPath('body')).toBeNull();
  });
});

describe('표시', () => {
  it('조건·결측 정책 한 줄', () => {
    expect(describeCondition(spec)).toBe('5분 오류율(5xx) > 2.5% · 최소 100요청 · 2분 유지');
    expect(describeCondition({ ...spec, for_seconds: 0 })).toContain('한 번 위반으로 경보');
    expect(describeNoData(spec)).toBe('데이터가 10분 없으면 경보');
    expect(describeNoData({ ...spec, no_data: { action: 'no_data', after_seconds: null } })).toContain('알림 없음');
  });

  it('값과 group 이름', () => {
    expect(formatValue('error_ratio', 0.0234)).toBe('2.34%');
    expect(formatValue('metric', 0.4567)).toBe('0.457');
    expect(formatValue('metric', 12345678)).toBe('1.23e+7');
    expect(groupLabel({})).toBe('(전체)');
    expect(groupLabel({ 'service.name': 'checkout', 'http.route': '/cart' })).toBe('http.route=/cart, service.name=checkout');
  });
});

describe('form이 다루지 않는 filter', () => {
  it('다른 label·같은 label 두 번째 값·중첩 and를 잃지 않는다(spec → form → spec)', () => {
    const filter = {
      op: 'and' as const,
      args: [
        { op: 'eq' as const, field: 'service.name', value: 'checkout' },
        { op: 'and' as const, args: [{ op: 'eq' as const, field: 'http.route', value: '/pay' }, { op: 'eq' as const, field: 'service.name', value: 'cart' }] },
        { op: 'eq' as const, field: 'deployment.environment.name', value: 'prod' },
      ],
    };
    const form = specToForm({ ...spec, query: { ...spec.query, filter } });
    expect(form.service).toBe('checkout');
    expect(form.environment).toBe('prod');
    expect(form.extraFilters.map(describeFilter)).toEqual(['http.route = "/pay"', 'service.name = "cart"']);
    const out = formToSpec(form).spec as { query: { filter: { args: unknown[] } } };
    expect(out.query.filter.args).toEqual([
      { op: 'eq', field: 'service.name', value: 'checkout' },
      { op: 'eq', field: 'deployment.environment.name', value: 'prod' },
      { op: 'eq', field: 'http.route', value: '/pay' },
      { op: 'eq', field: 'service.name', value: 'cart' },
    ]);
    expect(describeFilter(filter)).toBe('service.name = "checkout" 그리고 (http.route = "/pay" 그리고 service.name = "cart") 그리고 deployment.environment.name = "prod"');
  });
});

describe('상태 표현', () => {
  it('갱신이 멈춘 정상은 정상 색이 아니고, 데이터 없음 다음으로 정렬된다', () => {
    expect(stateTone('OK', false)).toBe('success');
    expect(stateTone('OK', true)).toBe('neutral');
    expect(stateTone('ALERT', true)).toBe('critical');
    expect(stateRank('OK', true)).toBeGreaterThan(stateRank('NO_DATA', false));
    expect(stateRank('OK', true)).toBeLessThan(stateRank('RECOVERING', false));
    expect(stateRank('UNAVAILABLE', false)).toBeLessThan(stateRank('OK', false));
  });

  it('평가 띠: group이 없던 평가는 데이터 없음, 경보 없는 평가는 정상(초록)이 아니라 "경보 없음"', () => {
    const e = { slot: 's', status: 'evaluated' as const, reason: null, window_start: null, window_end: null, groups: 0, alerting: 0, duration_ms: 1 };
    expect(cellOf(e)).toBe('nodata');
    expect(cellOf({ ...e, groups: 2 })).toBe('quiet');
    expect(cellOf({ ...e, groups: 2, alerting: 1 })).toBe('alert');
    expect(cellOf({ ...e, status: 'error' })).toBe('error');
  });
});
