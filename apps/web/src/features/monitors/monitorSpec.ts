// S09 monitor 편집·표시의 순수 함수 (D05 §09, ADR 0049·0050·0054).
// - 오류율은 화면에서 %로 입력하고 API에는 0~1 fraction으로 보낸다(D05 §09). 서버가 최종 검증한다.
// - 상태는 색만으로 구분하지 않는다(글자 + 모양). NO_DATA·EVALUATION_ERROR·NOT_EVALUATED는 OK와 다른 색이다(계약 6).
import type { FilterNode, GroupStateName, MonitorItem, MonitorKind, MonitorSpec, Operator, SummaryState } from '../../api/monitors.ts';
import type { BadgeIcon, BadgeVariant, Tone } from '../services/states.tsx';
import type { Schema } from '@montracer/query-schema';
import type { MetricAggregation } from '../../api/types.ts';

/** 서버가 받는 monitor 정의 입력(원천 MonitorSpecInput, ADR 0061). 편집기는 이 타입으로만 만든다. */
export type MonitorSpecInput = Schema<'MonitorSpecInput'>;

export const DEFAULT_ERROR_METRIC = 'http.server.request.duration';
export const SERVICE_KEY = 'service.name';
export const ENV_KEY = 'deployment.environment.name';

/** 화면 상태: 서버 요약 + 상태를 볼 권한이 없을 때(status null, ADR 0054 §1). */
export type ViewState = SummaryState | 'UNAVAILABLE';

export const STATE_LABEL: Record<ViewState, string> = {
  ALERT: '경보',
  PENDING: '대기',
  RECOVERING: '복구 중',
  OK: '정상',
  NO_DATA: '데이터 없음',
  EVALUATION_ERROR: '평가 오류',
  NOT_EVALUATED: '평가 전',
  DISABLED: '꺼짐',
  UNAVAILABLE: '상태를 볼 수 없음',
};

/** 상태별 배지 (디자인 기초 StatusBadge). 색·아이콘·테두리를 함께 바꿔 색만으로 구별하지 않는다. */
export interface StateBadge {
  tone: Tone;
  icon: BadgeIcon;
  variant: BadgeVariant;
}

export const STATE_BADGE: Record<ViewState, StateBadge> = {
  ALERT: { tone: 'critical', icon: 'cross', variant: 'default' },
  PENDING: { tone: 'warning', icon: 'exclaim', variant: 'default' },
  RECOVERING: { tone: 'warning', icon: 'refresh', variant: 'default' },
  OK: { tone: 'success', icon: 'check', variant: 'default' },
  // 데이터 없음은 점선, 판정 실패는 진한 실선 — 정상·판정 없음과 모양부터 다르다(계약 6).
  NO_DATA: { tone: 'neutral', icon: 'dashed', variant: 'dashed' },
  EVALUATION_ERROR: { tone: 'critical', icon: 'triangle', variant: 'emphasis' },
  NOT_EVALUATED: { tone: 'neutral', icon: 'minus', variant: 'default' },
  DISABLED: { tone: 'neutral', icon: 'minus', variant: 'default' },
  UNAVAILABLE: { tone: 'neutral', icon: 'minus', variant: 'default' },
};

export const STATE_TONE: Record<ViewState, Tone> = Object.fromEntries(
  Object.entries(STATE_BADGE).map(([k, v]) => [k, v.tone]),
) as Record<ViewState, Tone>;

/** 나쁜 순서(작을수록 먼저). group 상태는 서버 순서(controldb stateRank)와 같다. */
export const STATE_ORDER: Record<ViewState, number> = {
  ALERT: 0,
  EVALUATION_ERROR: 1,
  NO_DATA: 2,
  RECOVERING: 3,
  PENDING: 4,
  NOT_EVALUATED: 5,
  UNAVAILABLE: 6,
  OK: 7,
  DISABLED: 8,
};

/** 목록·상세의 상태. 읽기 응답의 status null은 상태 권한이 없다는 뜻이다(평가 전이 아니다). */
export function viewState(m: MonitorItem): ViewState {
  return m.status === null ? 'UNAVAILABLE' : m.status.state;
}

/** 갱신이 멈춘 monitor는 마지막 상태가 정상이어도 정상 색으로 보이지 않는다(D05 §03 stale, 계약 6). */
export function stateTone(state: ViewState, stale: boolean): Tone {
  return stateBadge(state, stale).tone;
}

/** 멈춘 정상은 시계 아이콘의 회색(stale)이다. 다른 상태는 멈춰도 그 상태 그대로 보인다. */
export function stateBadge(state: ViewState, stale: boolean): StateBadge {
  const b = STATE_BADGE[state];
  return stale && b.tone === 'success' ? { tone: 'neutral', icon: 'clock', variant: 'default' } : b;
}

/** 정렬 순서. 멈춘 정상은 데이터 없음 다음이다 — 지금 정상인지 모른다. */
export function stateRank(state: ViewState, stale: boolean): number {
  return stale && state === 'OK' ? STATE_ORDER.NO_DATA + 0.5 : STATE_ORDER[state];
}

/** 값이 없던 이유·평가 사유 (ADR 0050·0051·0052). 모르는 값은 그대로 보인다. */
export const REASON_TEXT: Record<string, string> = {
  query_failed: '조회 실패',
  group_limit_exceeded: 'group 1,000개 초과',
  invalid_spec: '정의를 해석할 수 없음',
  no_watermark: '집계된 데이터 없음',
  stale_watermark: '집계가 멈춤',
  below_minimum_requests: '최소 요청 수 미달',
  no_requests: '요청 없음',
  group_missing: '이번 window에 group 없음',
  no_data: '데이터 없음',
  missing_baseline: 'counter 기준점 없음',
  not_applicable: '이 metric 유형에 없는 값',
  bounds_mismatch: 'histogram 경계가 달라 병합 불가',
  unit_conflict: '단위 충돌',
  type_conflict: '유형 충돌',
  recovered: '복구',
  monitor_revised: '정의 변경',
  monitor_disabled: 'monitor 꺼짐',
  monitor_deleted: 'monitor 삭제',
  violation: '조건 위반',
};

export const reasonText = (r: string | null | undefined): string => (r == null || r === '' ? '' : (REASON_TEXT[r] ?? r));

export const OPERATOR_TEXT: Record<Operator, string> = { gt: '>', gte: '≥', lt: '<', lte: '≤' };

export const AGGREGATIONS = ['p95', 'p99', 'p90', 'p50', 'avg', 'min', 'max', 'sum', 'rate', 'increase', 'count', 'hist_sum'] as const;

export const EVALUATION_CHOICES = [30, 60, 300] as const;

/** select 값(문자열)을 평가 주기로. 고를 수 있는 값이 아니면 null — 원천의 enum(30·60·300) 밖 값을 보내지 않는다. */
export function evaluationChoice(v: string): MonitorSpecInput['evaluation_seconds'] | null {
  return EVALUATION_CHOICES.find((s) => String(s) === v) ?? null;
}

/** select 값(문자열)을 metric 연산으로. 원천 enum 밖이면 null. */
export function aggregationChoice(v: string): MetricAggregation | null {
  return AGGREGATIONS.find((a) => a === v) ?? null;
}

/** 편집 form 상태. 숫자 입력은 문자열로 둔다(입력 중 빈 값·부분 값을 0으로 바꾸지 않게). */
export interface MonitorForm {
  name: string;
  enabled: boolean;
  kind: MonitorKind;
  metric: string;
  aggregation: MetricAggregation;
  service: string;
  environment: string;
  groupBy: string[];
  windowMinutes: string;
  evaluationSeconds: MonitorSpecInput['evaluation_seconds'];
  operator: Operator;
  /** error_ratio는 %(0~100), metric은 값 그대로 */
  threshold: string;
  minimumRequests: string;
  forMinutes: string;
  noDataAction: 'no_data' | 'alert';
  noDataAfterMinutes: string;
  policyId: string;
  /** form 필드로 나타낼 수 없는 filter 조건(다른 label, 같은 label 두 번째 값). 편집하지 않고 그대로 다시 보낸다 —
   * 수정 화면을 저장했다고 조건이 사라지면 monitor 범위가 몰래 넓어진다. */
  extraFilters: FilterNode[];
}

export const emptyForm = (): MonitorForm => ({
  name: '',
  enabled: true,
  kind: 'error_ratio',
  metric: '',
  aggregation: 'p95',
  service: '',
  environment: '',
  groupBy: [],
  windowMinutes: '5',
  evaluationSeconds: 60,
  operator: 'gt',
  threshold: '2',
  minimumRequests: '100',
  forMinutes: '2',
  noDataAction: 'no_data',
  noDataAfterMinutes: '10',
  policyId: '',
  extraFilters: [],
});

/** filter(and/eq 트리)를 form 필드와 나머지로 나눈다. and는 결합 순서와 무관하므로 펼쳐도 의미가 같다. */
function splitFilter(f: FilterNode | null): { service: string; environment: string; extra: FilterNode[] } {
  const out = { service: '', environment: '', extra: [] as FilterNode[] };
  let hasService = false;
  let hasEnv = false;
  const walk = (n: FilterNode) => {
    if (n.op === 'and') {
      for (const a of n.args ?? []) walk(a);
      return;
    }
    if (n.op === 'eq' && n.field === SERVICE_KEY && !hasService && n.value !== undefined) {
      out.service = n.value;
      hasService = true;
    } else if (n.op === 'eq' && n.field === ENV_KEY && !hasEnv && n.value !== undefined) {
      out.environment = n.value;
      hasEnv = true;
    } else {
      out.extra.push(n);
    }
  };
  if (f !== null) walk(f);
  return out;
}

/** filter를 사람이 읽는 한 줄로. 모든 조건을 보인다(form이 다루지 않는 조건도). */
export function describeFilter(f: FilterNode): string {
  if (f.op === 'eq') return `${f.field ?? ''} = "${f.value ?? ''}"`;
  const parts = (f.args ?? []).map((a) => (a.op === 'and' ? `(${describeFilter(a)})` : describeFilter(a)));
  return parts.join(' 그리고 ');
}

/** 소수 표기를 짧게(부동소수 꼬리 없이). */
function num(v: number): string {
  return String(Number.parseFloat(v.toPrecision(12)));
}

export function specToForm(s: MonitorSpec): MonitorForm {
  const split = splitFilter(s.query.filter);
  return {
    name: s.name,
    enabled: s.enabled,
    kind: s.query.kind,
    metric: s.query.kind === 'metric' ? s.query.metric : '',
    aggregation: s.query.aggregation ?? 'p95',
    service: split.service,
    environment: split.environment,
    groupBy: [...s.query.group_by],
    windowMinutes: num(s.window_seconds / 60),
    evaluationSeconds: s.evaluation_seconds,
    operator: s.condition.operator,
    threshold: s.query.kind === 'error_ratio' ? num(s.condition.threshold * 100) : num(s.condition.threshold),
    minimumRequests: s.minimum_requests === null ? '' : String(s.minimum_requests),
    forMinutes: num(s.for_seconds / 60),
    noDataAction: s.no_data.action,
    noDataAfterMinutes: s.no_data.after_seconds === null ? '10' : num(s.no_data.after_seconds / 60),
    policyId: s.notification_policy_id ?? '',
    extraFilters: split.extra,
  };
}

export type FormErrors = Partial<Record<keyof MonitorForm, string>>;

function parseNumber(v: string): number | null {
  if (v.trim() === '') return null;
  const n = Number(v);
  return Number.isFinite(n) ? n : null;
}

/** form을 API에 보낼 spec으로 바꾼다. 화면에서 바로 알 수 있는 오류만 본다(최종 검증은 서버, 400 field violation). */
export function formToSpec(f: MonitorForm): { spec: MonitorSpecInput | null; errors: FormErrors } {
  const errors: FormErrors = {};
  const name = f.name.trim();
  if (name === '') errors.name = '이름을 입력하세요';
  if (f.kind === 'metric' && f.metric.trim() === '') errors.metric = 'metric을 고르세요';
  const windowMin = parseNumber(f.windowMinutes);
  if (windowMin === null || windowMin < 1 || windowMin > 1440 || !Number.isInteger(windowMin)) errors.windowMinutes = '1~1440분(정수)';
  else if ((windowMin * 60) % f.evaluationSeconds !== 0) errors.windowMinutes = '평가 주기의 배수여야 합니다';
  const threshold = parseNumber(f.threshold);
  if (threshold === null) errors.threshold = '숫자를 입력하세요';
  else if (f.kind === 'error_ratio' && (threshold < 0 || threshold > 100)) errors.threshold = '0~100%';
  const forMin = parseNumber(f.forMinutes);
  if (forMin === null || forMin < 0 || forMin > 1440) errors.forMinutes = '0~1440분';
  let minimum: number | null = null;
  if (f.kind === 'error_ratio') {
    minimum = parseNumber(f.minimumRequests);
    if (minimum === null || minimum < 0 || !Number.isInteger(minimum)) errors.minimumRequests = '0 이상의 정수';
  }
  let after: number | null = null;
  if (f.noDataAction === 'alert') {
    const a = parseNumber(f.noDataAfterMinutes);
    if (a === null || a <= 0 || a > 1440) errors.noDataAfterMinutes = '1~1440분';
    else after = Math.round(a * 60);
  }
  // 오류가 없으면 숫자 입력은 모두 값이 있다 — 타입으로도 좁힌다(빈 값을 0으로 채우지 않는다, 계약 6)
  if (Object.keys(errors).length > 0 || windowMin === null || threshold === null || forMin === null) return { spec: null, errors };

  const leaves: FilterNode[] = [];
  if (f.service.trim() !== '') leaves.push({ op: 'eq', field: SERVICE_KEY, value: f.service.trim() });
  if (f.environment.trim() !== '') leaves.push({ op: 'eq', field: ENV_KEY, value: f.environment.trim() });
  leaves.push(...f.extraFilters);
  const query: MonitorSpecInput['query'] = {
    kind: f.kind,
    ...(f.kind === 'metric' ? { metric: f.metric.trim(), aggregation: f.aggregation } : {}),
    ...(leaves.length === 0 ? {} : { filter: { op: 'and', args: leaves } }),
    ...(f.groupBy.length === 0 ? {} : { group_by: f.groupBy }),
  };
  const spec: MonitorSpecInput = {
    name,
    enabled: f.enabled,
    query,
    window_seconds: Math.round(windowMin * 60),
    evaluation_seconds: f.evaluationSeconds,
    condition: { operator: f.operator, threshold: f.kind === 'error_ratio' ? threshold / 100 : threshold },
    for_seconds: Math.round(forMin * 60),
    no_data: f.noDataAction === 'alert' ? { action: 'alert', after_seconds: after } : { action: 'no_data' },
    notification_policy_id: f.policyId === '' ? null : f.policyId,
    ...(f.kind === 'error_ratio' ? { minimum_requests: minimum } : {}),
  };
  return { spec, errors };
}

/** 서버 field violation 경로를 form 필드로(가능한 것만). */
export function fieldForPath(path: string): keyof MonitorForm | null {
  const map: [string, keyof MonitorForm][] = [
    ['name', 'name'],
    ['query.metric', 'metric'],
    ['query.aggregation', 'aggregation'],
    ['query.group_by', 'groupBy'],
    ['query.filter', 'service'],
    ['window_seconds', 'windowMinutes'],
    ['evaluation_seconds', 'evaluationSeconds'],
    ['condition.threshold', 'threshold'],
    ['condition.operator', 'operator'],
    ['minimum_requests', 'minimumRequests'],
    ['for_seconds', 'forMinutes'],
    ['no_data.after_seconds', 'noDataAfterMinutes'],
    ['no_data', 'noDataAction'],
    ['notification_policy_id', 'policyId'],
  ];
  for (const [prefix, field] of map) if (path === prefix || path.startsWith(prefix + '.') || path.startsWith(prefix + '[')) return field;
  return null;
}

/** 값 표시: 오류율은 %, metric은 숫자 그대로(단위는 metric 사전에서). null은 호출자가 이유로 표시한다. */
export function formatValue(kind: MonitorKind, v: number): string {
  if (kind === 'error_ratio') return `${num(Math.round(v * 10000) / 100)}%`;
  const abs = Math.abs(v);
  if (abs !== 0 && (abs >= 1e6 || abs < 1e-3)) return v.toExponential(2);
  return num(Math.round(v * 1000) / 1000);
}

export function formatDuration(seconds: number): string {
  if (seconds < 60) return `${String(seconds)}초`;
  if (seconds < 3600) return `${num(seconds / 60)}분`;
  if (seconds < 86400) return `${num(Math.round((seconds / 3600) * 10) / 10)}시간`;
  return `${num(Math.round((seconds / 86400) * 10) / 10)}일`;
}

/** 조건 한 줄: "5분 오류율 > 2% · 최소 100요청 · 2분 유지". */
export function describeCondition(s: MonitorSpec): string {
  const what = s.query.kind === 'error_ratio' ? '오류율(5xx)' : `${s.query.metric} ${s.query.aggregation ?? ''}`.trim();
  const threshold = s.query.kind === 'error_ratio' ? formatValue('error_ratio', s.condition.threshold) : num(s.condition.threshold);
  const parts = [`${formatDuration(s.window_seconds)} ${what} ${OPERATOR_TEXT[s.condition.operator]} ${threshold}`];
  if (s.minimum_requests !== null) parts.push(`최소 ${s.minimum_requests.toLocaleString('ko-KR')}요청`);
  parts.push(s.for_seconds === 0 ? '한 번 위반으로 경보' : `${formatDuration(s.for_seconds)} 유지`);
  return parts.join(' · ');
}

export function describeNoData(s: MonitorSpec): string {
  return s.no_data.action === 'alert' && s.no_data.after_seconds !== null
    ? `데이터가 ${formatDuration(s.no_data.after_seconds)} 없으면 경보`
    : '데이터가 없으면 "데이터 없음" 상태(알림 없음)';
}

export function groupLabel(labels: Record<string, string>): string {
  const keys = Object.keys(labels).sort();
  return keys.length === 0 ? '(전체)' : keys.map((k) => `${k}=${labels[k] ?? ''}`).join(', ');
}

/** 경고 코드 설명 (monitor.Warn*, ADR 0052). */
export const WARNING_TEXT: Record<string, string> = {
  for_seconds_zero: '한 번 위반으로 바로 경보가 열립니다. 잠깐 튀는 값에도 알림이 갈 수 있습니다.',
  minimum_requests_zero: '최소 요청 수가 0이라 요청이 아주 적을 때도 오류율로 판정합니다(저트래픽 노이즈).',
  evaluation_below_rollup_resolution: '30초 평가는 1분 집계를 다시 읽습니다. 판정은 1분마다 바뀝니다.',
  dry_run_unavailable: '이 배포에서는 24시간 미리보기를 할 수 없습니다.',
  dry_run_forbidden: '미리보기에 필요한 telemetry 조회 권한이 없습니다.',
  dry_run_failed: '미리보기 조회가 실패했습니다. 잠시 뒤 다시 시도하세요.',
  dry_run_too_large: 'series·window가 커서 미리보기를 할 수 없습니다. group을 줄여 보세요.',
  dry_run_busy: '미리보기 요청이 많습니다. 잠시 뒤 다시 시도하세요.',
};

export const isGroupState = (s: string): s is GroupStateName => s in STATE_ORDER && s !== 'NOT_EVALUATED' && s !== 'DISABLED' && s !== 'UNAVAILABLE';

/** 입력한 길이를 반올림 없이(1시간 5분처럼). 사용자가 넣은 값을 다른 값으로 보이지 않게 한다. */
export function exactDuration(seconds: number): string {
  if (seconds < 60) return `${num(seconds)}초`;
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  return [h > 0 ? `${String(h)}시간` : '', m > 0 ? `${String(m)}분` : '', s > 0 ? `${num(s)}초` : ''].filter((x) => x !== '').join(' ');
}

const minutesText = (v: string): string => {
  const n = Number(v);
  return v.trim() === '' || !Number.isFinite(n) || n < 0 ? '?분' : n === 0 ? '0분' : exactDuration(Math.round(n * 60));
};

/** 임계값을 저장되는 값 그대로(오류율은 % 로, 반올림 없이). */
const exactThreshold = (ratio: boolean, v: number): string => (ratio ? `${num(v * 100)}%` : num(v));

/**
 * 단계 목록의 요약 한 줄(디자인 시안 S09). 입력 중인 form을 그대로 읽는다 — 검증 전 값이라 비었거나 틀리면 "?"로 둔다.
 * 알림 단계는 policy 이름을 모르면 id 앞부분을 보인다(몰래 "알림 없음"으로 보이지 않게).
 */
export function stepSummaries(f: MonitorForm, policyName: (id: string) => string | null): string[] {
  const ratio = f.kind === 'error_ratio';
  const signal = ratio ? 'HTTP 오류율(5xx)' : f.metric.trim() === '' ? 'metric 미선택' : `${f.metric} ${f.aggregation}`;
  const scope = [f.service.trim() === '' ? '모든 서비스' : `service=${f.service.trim()}`, f.environment.trim() === '' ? '모든 환경' : `env=${f.environment.trim()}`];
  if (f.groupBy.length > 0) scope.push(`group ${f.groupBy.join(', ')}`);
  if (f.extraFilters.length > 0) scope.push(`조건 ${String(f.extraFilters.length)}개 더`);
  const threshold = f.threshold.trim() === '' ? '?' : `${f.threshold.trim()}${ratio ? '%' : ''}`;
  const hold = f.forMinutes.trim() === '0' ? '한 번 위반' : `${minutesText(f.forMinutes)} 유지`;
  const policy = f.policyId === '' ? '알림 없음' : (policyName(f.policyId) ?? `policy ${f.policyId.slice(0, 8)}…`);
  return [
    `${f.name.trim() === '' ? '이름 없음' : f.name.trim()} · ${signal}`,
    scope.join(' · '),
    `${OPERATOR_TEXT[f.operator]} ${threshold} · ${minutesText(f.windowMinutes)} window · ${hold}`,
    f.noDataAction === 'alert' ? `${minutesText(f.noDataAfterMinutes)} 없으면 경보` : '데이터 없음 상태(알림 없음)',
    policy,
    '검증과 지난 24시간 미리보기',
  ];
}

/** "이렇게 동작합니다" — 조건 단계의 상태 전이 설명(ADR 0050). spec이 유효할 때만 만든다. */
export function explainCondition(s: MonitorSpecInput): string {
  const ratio = s.query.kind === 'error_ratio';
  const what = ratio ? '오류율(5xx)' : `${s.query.metric} ${s.query.aggregation ?? ''}`.trim();
  const threshold = exactThreshold(ratio, s.condition.threshold);
  const groups = (s.query.group_by ?? []).length > 0 ? ' group마다 따로' : '';
  const min = ratio && s.minimum_requests !== null && s.minimum_requests !== undefined && s.minimum_requests > 0 ? s.minimum_requests : null;
  const head = `${exactDuration(s.evaluation_seconds)}마다${groups} 최근 ${exactDuration(s.window_seconds)} window의 ${what}을 계산합니다.`;
  const cond = `${min !== null ? `요청이 ${min.toLocaleString('ko-KR')}건 이상이고 ` : ''}값 ${OPERATOR_TEXT[s.condition.operator]} ${threshold}인 상태가`;
  const fire =
    s.for_seconds === 0
      ? `${cond} 한 번이라도 나오면 바로 경보(ALERT)가 열립니다.`
      : `${cond} ${exactDuration(s.for_seconds)} 이상 이어지면 대기(PENDING)에서 경보(ALERT)로 바뀝니다. 그 전에 풀리면 정상(OK)으로 돌아갑니다.`;
  const low = ratio
    ? ` 요청이 ${min !== null ? `${min.toLocaleString('ko-KR')}건보다 적거나 ` : ''}없으면 값이 없는 것으로 보고 데이터 없음 정책(다음 단계)을 따릅니다.`
    : ' 값이 없으면 데이터 없음 정책(다음 단계)을 따릅니다.';
  return `${head} ${fire}${low}`;
}

/**
 * 데이터 없음·복구 단계 설명(ADR 0050·0051·0053·0055). 복구 횟수는 spec 값(지금은 2로 고정)을 쓴다.
 * editing이면 저장이 열린 경보를 닫고 새 revision을 빈 상태부터 평가한다는 것도 말한다(ADR 0051 monitor_revised).
 */
export function explainNoData(s: MonitorSpecInput, editing = false): string[] {
  const recovery = s.recovery_evaluations ?? 2;
  const nd = typeof s.no_data === 'object' ? s.no_data : null;
  const after = nd !== null && nd.action === 'alert' ? (nd.after_seconds ?? null) : null;
  return [
    after !== null
      ? `query는 성공했지만 데이터가 없으면 데이터 없음(NO_DATA)이고, ${exactDuration(after)} 이어지면 경보를 엽니다.`
      : 'query는 성공했지만 데이터가 없으면 데이터 없음(NO_DATA)으로 표시만 하고 알리지 않습니다. 정상(OK)으로 바꾸지 않습니다.',
    'query가 실패하면 평가 오류(EVALUATION_ERROR)이고 이전 값으로 대신하지 않습니다. 결측·실패로 열린 경보를 닫지 않습니다.',
    `경보 중 조건이 풀리면 복구 중(RECOVERING)이 되고, ${String(recovery)}회 연속 정상이면 정상(OK)으로 닫힙니다.`,
    s.notification_policy_id === null || s.notification_policy_id === undefined || s.notification_policy_id === ''
      ? '알림 policy가 없어 알림을 보내지 않습니다. 상태 변화는 timeline에만 남습니다.'
      : '알림은 경보가 열릴 때, 경보가 이어지면 30분마다 최대 3번 더, 닫힐 때 갑니다. 대기·복구 중·데이터 없음·평가 오류로 바뀌는 것은 timeline에만 남습니다.',
    ...(editing ? ['저장하면 지금 열린 경보는 닫히고(닫힘 알림), 새 revision은 빈 상태부터 다시 평가합니다.'] : []),
  ];
}
