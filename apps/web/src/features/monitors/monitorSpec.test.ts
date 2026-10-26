import { describe, expect, it } from 'vitest';
import type { MonitorSpec } from '../../api/monitors.ts';
import { cellOf } from './MonitorDetail.tsx';
import { formatCoverage, valueCoverage } from './DryRunPreview.tsx';
import type { DryRun } from '../../api/monitors.ts';
import { describeCondition,
  exactDuration,
  explainCondition,
  explainNoData,
  stepSummaries,
  describeFilter,
  describeNoData,
  emptyForm,
  fieldForPath,
  formToSpec,
  formatValue,
  groupLabel,
  specToForm,
  stateRank,
  stateTone, aggregationChoice, evaluationChoice } from './monitorSpec.ts';

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
    const f = { ...emptyForm(), name: 'p95', kind: 'metric' as const, metric: 'http.server.request.duration', aggregation: 'p95' as const, threshold: '0.5' };
    const { spec: out } = formToSpec(f);
    expect(out).toMatchObject({ query: { kind: 'metric', metric: 'http.server.request.duration', aggregation: 'p95' }, condition: { threshold: 0.5 } });
    expect(out).not.toHaveProperty('minimum_requests');
    expect(out).toMatchObject({ no_data: { action: 'no_data' } });
  });

  it('바로 알 수 있는 오류는 보내기 전에 막는다(빈 값을 0으로 보내지 않는다)', () => {
    const bad = { ...emptyForm(), name: ' ', threshold: '', windowMinutes: '3', evaluationSeconds: 300 as const, minimumRequests: '-1' };
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

// select 값은 원천의 enum 안에서만 form에 들어간다(ADR 0061): 모르는 값은 null이라 보내지 않는다.
describe('select 값 좁히기', () => {
  it('평가 주기는 30·60·300만', () => {
    expect(evaluationChoice('60')).toBe(60);
    expect(evaluationChoice('45')).toBeNull();
    expect(evaluationChoice('')).toBeNull();
  });
  it('연산은 원천 enum만', () => {
    expect(aggregationChoice('p95')).toBe('p95');
    expect(aggregationChoice('median')).toBeNull();
  });
});

describe('시안 S09 설명·요약', () => {
  const valid = () => {
    const { spec } = formToSpec({ ...emptyForm(), name: 'x' });
    if (spec === null) throw new Error('fixture');
    return spec;
  };

  it('조건 설명: 평가 주기·window·최소 요청·for(PENDING→ALERT), for 0이면 바로 경보', () => {
    expect(explainCondition(valid())).toBe(
      '1분마다 최근 5분 window의 오류율(5xx)을 계산합니다. 요청이 100건 이상이고 값 > 2%인 상태가 2분 이상 이어지면 대기(PENDING)에서 경보(ALERT)로 바뀝니다. 그 전에 풀리면 정상(OK)으로 돌아갑니다. 요청이 100건보다 적거나 없으면 값이 없는 것으로 보고 데이터 없음 정책(다음 단계)을 따릅니다.',
    );
    expect(explainCondition({ ...valid(), for_seconds: 0, minimum_requests: 0 })).toMatch(/한 번이라도 나오면 바로 경보\(ALERT\)가 열립니다\. 요청이 없으면/);
    // 저장되는 값 그대로: 반올림하지 않는다
    expect(explainCondition({ ...valid(), condition: { operator: 'gt', threshold: 0.02005 }, window_seconds: 3900 })).toContain('최근 1시간 5분 window의 오류율(5xx)을 계산합니다. 요청이 100건 이상이고 값 > 2.005%');
  });

  it('데이터 없음 설명: 알림 여부, 평가 오류는 이전 값으로 대신하지 않음, 복구 2회', () => {
    const lines = explainNoData(valid());
    expect(lines[0]).toContain('알리지 않습니다');
    expect(lines[0]).toContain('정상(OK)으로 바꾸지 않습니다');
    expect(lines[2]).toContain('2회 연속 정상');
    expect(lines[3]).toContain('알림 policy가 없어 알림을 보내지 않습니다');
    expect(lines).toHaveLength(4);
    const edited = explainNoData({ ...valid(), notification_policy_id: 'p' }, true);
    expect(edited[3]).toContain('30분마다 최대 3번 더');
    expect(edited[4]).toContain('저장하면 지금 열린 경보는 닫히고');
    expect(explainNoData({ ...valid(), no_data: { action: 'alert', after_seconds: 600 } })[0]).toContain('10분 이어지면 경보를 엽니다');
  });

  it('단계 요약: 빈 값은 "?"·"이름 없음", 모르는 policy는 id 앞부분', () => {
    const f = { ...emptyForm(), threshold: '', service: 'checkout', groupBy: ['http.route'], policyId: '12345678-aaaa' };
    const s = stepSummaries(f, () => null);
    expect(s[0]).toBe('이름 없음 · HTTP 오류율(5xx)');
    expect(s[1]).toBe('service=checkout · 모든 환경 · group http.route');
    expect(s[2]).toBe('> ? · 5분 window · 2분 유지');
    expect(stepSummaries({ ...emptyForm(), windowMinutes: '65', forMinutes: '0' }, () => null)[2]).toBe('> 2% · 1시간 5분 window · 한 번 위반');
    expect(exactDuration(5400)).toBe('1시간 30분');
    expect(s[4]).toBe('policy 12345678…');
    expect(stepSummaries({ ...emptyForm(), policyId: 'p' }, () => 'On-call')[4]).toBe('On-call');
  });

  it('판정 비율: 반올림으로 0%·100%를 지어내지 않는다', () => {
    expect(formatCoverage(1439, 1440)).toBe('99.9%');
    expect(formatCoverage(1, 1440)).toBe('0.1%');
    expect(formatCoverage(1440, 1440)).toBe('100%');
    expect(formatCoverage(0, 1440)).toBe('0%');
    expect(formatCoverage(0, 0)).toBe('-');
  });
});

describe('dry-run 값 coverage', () => {
  const group = (evaluated: number, noData: number) => ({
    group_key: String(evaluated), labels: {}, firing_intervals: [], firing_intervals_omitted: 0, firing_seconds: 0, final_state: 'NO_DATA' as const,
    evaluated, no_data: noData, partial: 0, first_value_at: null, max_value: null, last_value: null, last_reason: '', transitions: 0,
  });
  const dr = (groups: ReturnType<typeof group>[], omitted = 0): DryRun => ({
    from: 'a', to: 'b', warmup_seconds: 0, data_until: null, step_seconds: 60, evaluations: 1440,
    // monitor 수준은 값이 없던 시점도 "평가 실행"으로 센다
    coverage: { evaluated: 1440, no_data: 0, error: 0 }, groups, groups_total: groups.length + omitted, groups_omitted: omitted, reason: '',
  });

  it('값이 한 번도 없던 group(최소 요청 미달 등)은 100%가 아니라 0%', () => {
    const c = valueCoverage(dr([group(0, 1440)]));
    expect(formatCoverage(c.evaluated, c.total)).toBe('0%');
    expect(c.worst).toBe(false);
  });

  it('group이 없으면 0, 여럿이면 가장 낮은 group', () => {
    expect(valueCoverage(dr([])).evaluated).toBe(0);
    const c = valueCoverage(dr([group(1440, 0), group(720, 720)]));
    expect(formatCoverage(c.evaluated, c.total)).toBe('50%');
    expect(c.worst).toBe(true);
    expect(valueCoverage(dr([group(1440, 0)], 3)).worst).toBe(true);
  });
});
