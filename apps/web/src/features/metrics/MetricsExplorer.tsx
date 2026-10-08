// S08 Metrics (D05 §08, F04·F05, ADR 0046·0047).
// 좌측 metric 사전(유형·단위·temporality를 먼저), 가운데 query builder와 차트, 아래 series 요약 표.
// - 연산은 유형이 허용하는 것만 고를 수 있다(gauge에 rate, summary quantile 평균 금지, D02 §07).
// - 해상도(step)·읽은 rollup·집계 완료 시각(watermark)·집계 중(pending)·일부 집계(partial)·결측을 legend에 밝힌다.
// - 값이 없는 step은 0이 아니라 끊긴 선과 사유다(계약 6). percentile은 bucket 병합 값이다(계약 5).
import { useEffect, useMemo, useState } from 'react';
import { useOutletContext, useParams, useSearchParams } from 'react-router';
import { listMetricLabels, listMetrics, queryMetrics } from '../../api/metrics.ts';
import type { MetricAggregation, MetricDescriptor, MetricQuery, MetricSeries, MetricVariant } from '../../api/types.ts';
import { useRemote } from '../../api/useRemote.ts';
import { formatRange, resolveRange, type InvestigationContext } from '../../app/context.ts';
import { REASON_LABEL, formatClock, formatPercent } from '../services/format.ts';
import { toPoints } from '../services/red.ts';
import { ErrorNotice, Skeleton, StatusBadge } from '../services/states.tsx';
import '../services/services.css';
import '../traces/traces.css';
import {
  AGG_LABEL,
  MAX_FILTERS,
  MAX_GROUP_BY,
  MAX_LABEL_LENGTH,
  MAX_METRIC_RANGE_MS,
  ENV_KEY,
  autoStep,
  dictionaryRange,
  formatValue,
  parseMetricState,
  pickAggregation,
  resultUnit,
  rollupLabel,
  seriesName,
  stepChoices,
  stepLabel,
  toMetricQuery,
  typeLabel,
  writeMetricState,
  type LabelFilter,
  type MetricState,
} from './metricQuery.ts';
import { MAX_DRAWN, SeriesChart, seriesStyle } from './SeriesChart.tsx';
import './metrics.css';

const DICT_LIMIT = 1000;
const ALL_AGGS: MetricAggregation[] = ['rate', 'increase', 'sum', 'avg', 'min', 'max', 'count', 'hist_sum', 'p50', 'p90', 'p95', 'p99'];

/**
 * 이름의 조합들이 함께 쓸 수 있는 연산. 모든 조합의 허용 연산이 **같을 때만** 그 목록이다(예: 단위만 다른 histogram,
 * delta·cumulative counter). 의미가 다른 조합(counter와 up-down counter, gauge와 histogram)이 섞이면 빈 목록이다 —
 * 교집합으로 고르면 서버가 유형 충돌로 잡지 못하는 섞인 값(증가량 + 순변화)이 정상값처럼 그려진다(ADR 0047, 리뷰 P1).
 */
export function allowedAggregations(d: MetricDescriptor): MetricAggregation[] {
  const [first, ...rest] = d.variants;
  if (first === undefined) return [];
  const key = (v: MetricVariant) => [...v.aggregations].sort().join(',');
  return rest.every((v) => key(v) === key(first)) ? first.aggregations : [];
}

export function MetricsExplorer() {
  const { org = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const [params, setParams] = useSearchParams();
  const state = parseMetricState(params);
  const [refreshTick, setRefreshTick] = useState(0);
  const rangeKey = JSON.stringify(ctx.range);
  const nowMs = useMemo(() => Date.now(), [rangeKey, refreshTick]);
  const { fromMs, toMs } = resolveRange(ctx.range, nowMs);
  const tooLong = toMs - fromMs > MAX_METRIC_RANGE_MS;
  const setState = (next: MetricState) => setParams(writeMetricState(params, next));

  // 사전: 조사 범위 끝에서 최대 24시간
  const [search, setSearch] = useState('');
  const [debounced, setDebounced] = useState('');
  useEffect(() => {
    const t = window.setTimeout(() => setDebounced(search.trim()), 300);
    return () => window.clearTimeout(t);
  }, [search]);
  const dict = dictionaryRange(fromMs, toMs);
  const dictionary = useRemote(`${org}|metrics|${dict.from}|${dict.to}|${debounced}`, refreshTick, (signal) =>
    listMetrics({ from: dict.from, to: dict.to, q: debounced, limit: DICT_LIMIT, cursor: null }, signal),
  );
  // 선택한 metric의 설명: 검색 결과에 없을 수 있으므로 이름으로 따로 찾는다
  const selectedDict = useRemote(state.metric === null ? null : `${org}|metric|${state.metric}|${dict.from}|${dict.to}`, refreshTick, (signal) =>
    listMetrics({ from: dict.from, to: dict.to, q: state.metric ?? '', limit: DICT_LIMIT, cursor: null }, signal),
  );
  const descriptor = state.metric === null ? null : (selectedDict.data?.find((d) => d.name === state.metric) ?? null);
  const descriptorKnown = selectedDict.status === 'success' && selectedDict.nextCursor === null;

  // 유형을 모르면(사전에 없음) 모든 연산을 보이되 직접 고를 때까지 조회하지 않는다
  const allowed = descriptor === null ? ALL_AGGS : allowedAggregations(descriptor);
  // 사전을 받기 전에는 URL의 연산을 검증할 수 없으므로 조회하지 않는다(gauge에 rate 같은 요청을 보내지 않게)
  const dictPending = selectedDict.data === null && selectedDict.error === null;
  const agg: MetricAggregation | null = dictPending
    ? null
    : state.agg !== null && allowed.includes(state.agg)
      ? state.agg
      : descriptor === null
        ? null
        : pickAggregation(allowed);

  const labels = useRemote(state.metric === null ? null : `${org}|metric-labels|${state.metric}|${dict.from}|${dict.to}`, refreshTick, (signal) =>
    listMetricLabels(state.metric ?? '', dict.from, dict.to, signal),
  );

  const rangeMs = toMs - fromMs;
  const choices = stepChoices(rangeMs);
  const step = state.step !== null && choices.includes(state.step) ? state.step : autoStep(rangeMs);
  // 조사 context의 environment는 resource 속성 조건으로 자동 적용한다(OTel semconv deployment.environment.name)
  const envFilters: LabelFilter[] = ctx.environment === null ? [] : [{ key: ENV_KEY, value: ctx.environment }];
  const request =
    tooLong || state.metric === null || agg === null
      ? null
      : toMetricQuery({ ...state, metric: state.metric, agg, filters: [...envFilters, ...state.filters] }, fromMs, toMs, step);
  const result = useRemote(request === null ? null : `${org}|metric-query|${JSON.stringify(request)}`, refreshTick, (signal) =>
    queryMetrics(request as MetricQuery, signal),
  );

  return (
    <section className="mt-page" aria-labelledby="metrics-title">
      <header className="mt-page__header">
        <h1 id="metrics-title" className="mt-title">
          Metrics
        </h1>
        <span className="mt-label">{formatRange(ctx.range, nowMs, ctx.timeZone)}</span>
        <button type="button" className="mt-button mt-page__action" onClick={() => setRefreshTick((n) => n + 1)}>
          새로고침
        </button>
      </header>
      <div className="mt-explorer mt-metrics">
        <Dictionary
          state={state}
          search={search}
          onSearch={setSearch}
          dictionary={dictionary}
          clipped={dict.clipped}
          timeZone={ctx.timeZone}
          onSelect={(name) => setState({ metric: name, agg: null, groupBy: [], filters: [], step: state.step })}
        />
        <div className="mt-explorer__main">
          {tooLong && (
            <div className="mt-notice mt-notice--warning" role="alert">
              metric은 7일까지 조회할 수 있습니다. 시간 범위를 줄이세요.
            </div>
          )}
          {state.metric === null ? (
            <div className="mt-card mt-empty">
              <strong>metric을 고르세요</strong>
              <span>왼쪽 사전에서 고르면 유형에 맞는 연산으로 그립니다.</span>
            </div>
          ) : (
            <>
              <MetricHeader name={state.metric} descriptor={descriptor} known={descriptorKnown} loading={selectedDict.status === 'loading'} />
              <Builder
                state={state}
                agg={agg}
                allowed={allowed}
                step={step}
                choices={choices}
                autoStepSeconds={autoStep(rangeMs)}
                labelKeys={labels.data?.keys.map((k) => k.key) ?? []}
                labelsTruncated={labels.meta?.warnings.includes('label_keys_truncated') === true}
                environment={ctx.environment}
                onChange={setState}
              />
              {request !== null && (
                <Result
                  state={result}
                  groupBy={state.groupBy}
                  agg={agg ?? 'avg'}
                  unit={descriptor?.variants.length === 1 ? (descriptor.variants[0]?.unit ?? '') : ''}
                  step={step}
                  fromMs={fromMs}
                  toMs={toMs}
                  timeZone={ctx.timeZone}
                  onRetry={() => setRefreshTick((n) => n + 1)}
                />
              )}
            </>
          )}
        </div>
      </div>
    </section>
  );
}

function Dictionary({
  state,
  search,
  onSearch,
  dictionary,
  clipped,
  timeZone,
  onSelect,
}: {
  state: MetricState;
  search: string;
  onSearch: (v: string) => void;
  dictionary: ReturnType<typeof useRemote<MetricDescriptor[]>>;
  clipped: boolean;
  timeZone: string;
  onSelect: (name: string) => void;
}) {
  const rows = dictionary.data ?? [];
  return (
    <aside className="mt-card mt-explorer__filters mt-metric-dict" aria-labelledby="dict-title">
      <h2 id="dict-title" className="mt-card__title">
        Metric 사전
      </h2>
      <div className="mt-field">
        <label htmlFor="md-search">이름 검색</label>
        <input id="md-search" className="mt-input" type="search" maxLength={MAX_LABEL_LENGTH} value={search} onChange={(e) => onSearch(e.target.value)} />
        <span className="mt-label">{clipped ? '조사 범위의 마지막 24시간에 관측된 metric(볼 수 있는 모든 환경)' : '조사 범위에 관측된 metric(볼 수 있는 모든 환경)'}</span>
      </div>
      {dictionary.error !== null ? (
        <ErrorNotice error={dictionary.error} lastSuccessMs={dictionary.lastSuccessMs} timeZone={timeZone} />
      ) : dictionary.status === 'loading' ? (
        <Skeleton height={160} label="metric 사전을 불러오는 중" />
      ) : rows.length === 0 ? (
        <p className="mt-muted">{search.trim() === '' ? '이 범위에 관측된 metric이 없습니다.' : '이름이 맞는 metric이 없습니다.'}</p>
      ) : (
        <ul className="mt-metric-list" aria-label="metric 목록">
          {rows.map((d) => (
            <li key={d.name}>
              <button type="button" className="mt-metric-item" aria-current={d.name === state.metric ? 'true' : undefined} onClick={() => onSelect(d.name)}>
                <span className="mt-mono mt-metric-item__name">{d.name}</span>
                <span className="mt-label">
                  {d.conflict ? (
                    <StatusBadge tone="warning">계측 충돌</StatusBadge>
                  ) : (
                    d.variants.map((v) => `${typeLabel(v)}${v.unit === '' ? '' : ` · ${v.unit}`}`).join('')
                  )}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
      {dictionary.nextCursor !== null && (
        <p className="mt-label" role="note">
          {DICT_LIMIT.toLocaleString('ko-KR')}개까지 보입니다. 이름으로 검색해 좁히세요.
        </p>
      )}
    </aside>
  );
}

function MetricHeader({ name, descriptor, known, loading }: { name: string; descriptor: MetricDescriptor | null; known: boolean; loading: boolean }) {
  return (
    <section className="mt-card" aria-labelledby="metric-name">
      <h2 id="metric-name" className="mt-card__title mt-mono">
        {name}
      </h2>
      {loading ? null : descriptor === null ? (
        <p className="mt-label" role="note">
          {known ? '사전 범위(최근 24시간 이내)에서 관측되지 않은 metric입니다.' : '사전에서 이 metric을 확인하지 못했습니다.'} 유형을 알 수 없어 모든 연산을 보입니다. 맞지 않는 연산은
          "해당 없음"으로 나옵니다.
        </p>
      ) : (
        <>
          {descriptor.conflict && (
            <div className="mt-notice mt-notice--warning" role="note">
              {allowedAggregations(descriptor).length === 0
                ? '같은 이름에 의미가 다른 계측(예: counter와 up-down counter)이 섞여 있어 합쳐 그릴 수 없습니다. 계측 이름을 나누세요.'
                : '같은 이름에 단위·temporality가 다른 계측이 섞여 있습니다. 한 series에 단위가 섞이면 값 대신 "단위 충돌"로 나옵니다.'}
            </div>
          )}
          <div className="mt-scroll-x">
            <table className="mt-table mt-table--dense">
              <thead>
                <tr>
                  <th scope="col">유형</th>
                  <th scope="col">단위</th>
                  <th scope="col">temporality</th>
                  <th scope="col" className="mt-num">
                    series
                  </th>
                  <th scope="col">허용 연산</th>
                </tr>
              </thead>
              <tbody>
                {descriptor.variants.map((v) => (
                  <VariantRow key={`${v.type}|${v.unit}|${v.temporality}|${String(v.monotonic)}`} v={v} />
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </section>
  );
}

function VariantRow({ v }: { v: MetricVariant }) {
  return (
    <tr>
      <td>{typeLabel(v)}</td>
      <td className="mt-mono">{v.unit === '' ? <span className="mt-muted">없음</span> : v.unit}</td>
      <td>{v.temporality}</td>
      <td className="mt-num">{v.series.toLocaleString('ko-KR')}</td>
      <td>
        {v.aggregations.length === 0 ? (
          <span className="mt-muted">조회 미지원 ({v.type === 'summary' ? 'summary quantile은 합칠 수 없음' : '원본 표시만'})</span>
        ) : (
          v.aggregations.join(', ')
        )}
      </td>
    </tr>
  );
}

function Builder({
  state,
  agg,
  allowed,
  step,
  choices,
  autoStepSeconds,
  labelKeys,
  labelsTruncated,
  environment,
  onChange,
}: {
  state: MetricState;
  agg: MetricAggregation | null;
  allowed: MetricAggregation[];
  step: number;
  choices: number[];
  autoStepSeconds: number;
  labelKeys: string[];
  labelsTruncated: boolean;
  environment: string | null;
  onChange: (s: MetricState) => void;
}) {
  // filter 값은 입력 중 URL을 바로 바꾸지 않는다: 300ms 뒤 또는 Enter에 반영(D05 §03)
  const [draft, setDraft] = useState<LabelFilter[]>(state.filters);
  const filtersKey = JSON.stringify(state.filters);
  useEffect(() => setDraft(state.filters), [filtersKey]);
  useEffect(() => {
    if (JSON.stringify(draft) === filtersKey) return;
    const t = window.setTimeout(() => onChange({ ...state, filters: draft.filter((f) => f.key !== '') }), 300);
    return () => window.clearTimeout(t);
  }, [draft]);
  const keyOptions = [...new Set([...labelKeys, ...state.groupBy, ...draft.map((f) => f.key)])].filter((k) => k !== '');

  return (
    <section className="mt-card" aria-label="조회 조건">
      <form
        className="mt-metric-builder"
        onSubmit={(e) => {
          e.preventDefault();
          onChange({ ...state, filters: draft.filter((f) => f.key !== '') });
        }}
      >
        <div className="mt-field">
          <label htmlFor="mb-agg">연산</label>
          <select
            id="mb-agg"
            className="mt-select"
            value={agg ?? ''}
            disabled={allowed.length === 0}
            onChange={(e) => onChange({ ...state, agg: e.target.value as MetricAggregation })}
          >
                {agg === null && <option value="">{allowed.length === 0 ? '선택할 수 있는 연산 없음' : '연산을 고르세요'}</option>}
            {allowed.map((a) => (
              <option key={a} value={a}>
                {a} · {AGG_LABEL[a]}
              </option>
            ))}
          </select>
        </div>
        <div className="mt-field">
          <label htmlFor="mb-step">해상도(step)</label>
          <select
            id="mb-step"
            className="mt-select"
            value={state.step !== null && choices.includes(state.step) ? String(state.step) : ''}
            onChange={(e) => onChange({ ...state, step: e.target.value === '' ? null : Number(e.target.value) })}
          >
            <option value="">자동 ({stepLabel(autoStepSeconds)})</option>
            {choices.map((c) => (
              <option key={c} value={c}>
                {stepLabel(c)}
              </option>
            ))}
          </select>
          {state.step !== null && state.step !== step && <span className="mt-label">이 범위에 맞지 않아 자동 해상도를 씁니다.</span>}
        </div>
        <fieldset className="mt-field mt-metric-group">
          <legend>
            나눠 보기 (group by, 최대 {MAX_GROUP_BY}개){labelsTruncated ? ' · series가 많은 label 200개까지' : ''}
          </legend>
          {keyOptions.length === 0 ? (
            <span className="mt-muted">label이 없습니다.</span>
          ) : (
            <div className="mt-metric-keys">
              {keyOptions.map((k) => {
                const on = state.groupBy.includes(k);
                return (
                  <label key={k} className="mt-field--check mt-mono">
                    <input
                      type="checkbox"
                      checked={on}
                      disabled={!on && state.groupBy.length >= MAX_GROUP_BY}
                      onChange={(e) => onChange({ ...state, groupBy: e.target.checked ? [...state.groupBy, k] : state.groupBy.filter((g) => g !== k) })}
                    />
                    {k}
                  </label>
                );
              })}
            </div>
          )}
        </fieldset>
        <fieldset className="mt-field mt-metric-filters">
          <legend>조건 (모두 일치)</legend>
          {environment !== null && (
            <p className="mt-label mt-mono">
              {ENV_KEY} = {environment} <span className="mt-muted">(상단 환경 선택에서 자동 적용)</span>
            </p>
          )}
          {draft.map((f, i) => (
            <div key={i} className="mt-metric-filter">
              <label className="mt-visually-hidden" htmlFor={`mb-fk-${String(i)}`}>
                조건 {i + 1} label
              </label>
              <select
                id={`mb-fk-${String(i)}`}
                className="mt-select mt-mono"
                value={f.key}
                onChange={(e) => setDraft(draft.map((d, j) => (j === i ? { ...d, key: e.target.value } : d)))}
              >
                <option value="">label 선택</option>
                {keyOptions.map((k) => (
                  <option key={k} value={k}>
                    {k}
                  </option>
                ))}
              </select>
              <span aria-hidden="true">=</span>
              <label className="mt-visually-hidden" htmlFor={`mb-fv-${String(i)}`}>
                조건 {i + 1} 값
              </label>
              <input
                id={`mb-fv-${String(i)}`}
                className="mt-input mt-mono"
                maxLength={MAX_LABEL_LENGTH}
                value={f.value}
                onChange={(e) => setDraft(draft.map((d, j) => (j === i ? { ...d, value: e.target.value } : d)))}
              />
              <button type="button" className="mt-button" aria-label={`조건 ${String(i + 1)} 빼기`} onClick={() => setDraft(draft.filter((_, j) => j !== i))}>
                빼기
              </button>
            </div>
          ))}
          <div className="mt-metric-filter-actions">
            <button type="button" className="mt-button" disabled={draft.length >= MAX_FILTERS} onClick={() => setDraft([...draft, { key: '', value: '' }])}>
              조건 더하기
            </button>
            <button type="submit" className="mt-button">
              조회
            </button>
            <span className="mt-label">조건 값은 공유 링크에 들어가지 않습니다.</span>
          </div>
        </fieldset>
      </form>
    </section>
  );
}

function Result({
  state,
  groupBy,
  agg,
  unit,
  step,
  fromMs,
  toMs,
  timeZone,
  onRetry,
}: {
  state: ReturnType<typeof useRemote<{ series: MetricSeries[]; source_window_seconds?: number; range?: { from: string; to: string } }>>;
  groupBy: string[];
  agg: MetricAggregation;
  unit: string;
  step: number;
  fromMs: number;
  toMs: number;
  timeZone: string;
  onRetry: () => void;
}) {
  if (state.error !== null && state.data === null) return <ErrorNotice error={state.error} lastSuccessMs={state.lastSuccessMs} timeZone={timeZone} onRetry={onRetry} />;
  if (state.status === 'loading' || state.data === null) return <Skeleton height={240} label="metric을 불러오는 중" />;
  const data = state.data;
  const series = data.series;
  const shownUnit = resultUnit(agg, series.find((s) => s.unit !== null)?.unit ?? unit);
  const chartFrom = data.range === undefined ? fromMs : Date.parse(data.range.from);
  const chartTo = data.range === undefined ? toMs : Date.parse(data.range.to);
  const rollup = rollupLabel(data.source_window_seconds);
  const watermark = state.meta?.watermark ?? null;
  const hasPending = series.some((s) => s.points.some((p) => p.v === null && p.reason === 'pending'));
  const hasPartial = series.some((s) => s.points.some((p) => p.v !== null && p.partial));
  const chartSeries = series.map((s) => ({ name: seriesName(s.labels, groupBy), points: toPoints(s) }));
  return (
    <section className="mt-card" aria-labelledby="metric-result-title">
      {state.error !== null && <ErrorNotice error={state.error} lastSuccessMs={state.lastSuccessMs} timeZone={timeZone} onRetry={onRetry} />}
      <div className="mt-card__head mt-card__head--inset">
        <h2 id="metric-result-title" className="mt-card__title">
          {AGG_LABEL[agg]}
          {shownUnit === '' ? '' : ` (${shownUnit})`}
        </h2>
        <span className="mt-label">
          해상도 {stepLabel(step)}
          {rollup === null ? '' : ` · ${rollup}`}
          {watermark === null ? ' · 집계 완료 시각 모름' : ` · ${formatClock(Date.parse(watermark), timeZone, true)}까지 집계 완료`}
        </span>
      </div>
      <ul className="mt-metric-legend" aria-label="차트 표시 규칙">
        <li>
          <span className="mt-metric-legend__gap" aria-hidden="true" />
          끊긴 선: 값 없음(0이 아님, 사유는 표)
        </li>
        {hasPending && (
          <li>
            <span className="mt-metric-legend__pending" aria-hidden="true" />
            음영: 집계 중(아직 계산 전)
          </li>
        )}
        {hasPartial && (
          <li>
            <span className="mt-metric-legend__partial" aria-hidden="true" />빈 원: 일부만 집계(값이 바뀔 수 있음)
          </li>
        )}
        {agg.startsWith('p') && <li>분위수는 bucket을 합친 뒤 계산합니다(평균하지 않음).</li>}
      </ul>
      {series.length === 0 ? (
        <div className="mt-empty mt-empty--inline">
          <strong>조건에 맞는 series가 없습니다</strong>
          <span>조건을 줄이거나 시간 범위를 넓혀 보세요. 값이 0인 것과 다릅니다.</span>
        </div>
      ) : (
        <>
          {series.length > MAX_DRAWN && (
            <p className="mt-label" role="note">
              series {series.length.toLocaleString('ko-KR')}개 중 {MAX_DRAWN}개만 그렸습니다. 아래 표에는 모두 있습니다. 조건이나 나눠 보기를 줄이세요.
            </p>
          )}
          <SeriesChart title={AGG_LABEL[agg]} series={chartSeries} format={formatValue} unit={shownUnit} fromMs={chartFrom} toMs={chartTo} timeZone={timeZone} />
          <SeriesTable series={series} names={chartSeries.map((s) => s.name)} />
        </>
      )}
      <div className="mt-card__footer">{state.meta?.request_id !== undefined && <span className="mt-label mt-mono">request_id {state.meta.request_id}</span>}</div>
    </section>
  );
}

const FLAG_LABEL: Record<string, string> = { unit_conflict: '단위 충돌', type_conflict: '유형 충돌' };

function SeriesTable({ series, names }: { series: MetricSeries[]; names: string[] }) {
  return (
    <div className="mt-scroll-x">
      <table className="mt-table mt-table--dense">
        <caption className="mt-visually-hidden">series 요약</caption>
        <thead>
          <tr>
            <th scope="col">series</th>
            <th scope="col" className="mt-num">
              최근 값
            </th>
            <th scope="col" className="mt-num">
              최솟값
            </th>
            <th scope="col" className="mt-num">
              최댓값
            </th>
            <th scope="col" className="mt-num">
              값 있는 step
            </th>
            <th scope="col">비고</th>
          </tr>
        </thead>
        <tbody>
          {series.map((s, i) => {
            const vals = s.points.flatMap((p) => (p.v === null ? [] : [p.v]));
            const last = [...s.points].reverse().find((p) => p.v !== null);
            const reasons = [...new Set(s.points.flatMap((p) => (p.v === null && p.reason !== undefined ? [p.reason] : [])))];
            const style = i < MAX_DRAWN ? seriesStyle(i) : null;
            return (
              <tr key={names[i]}>
                <th scope="row" className="mt-mono mt-series-name">
                  {style !== null && (
                    <svg width="18" height="8" aria-label={style.dashLabel} role="img" className="mt-series-swatch" style={{ color: style.color }}>
                      <line x1="0" x2="18" y1="4" y2="4" stroke="currentColor" strokeWidth="2.5" strokeDasharray={style.dash} />
                    </svg>
                  )}
                  {names[i]}
                </th>
                <td className="mt-num">{last?.v == null ? '—' : formatValue(last.v)}</td>
                <td className="mt-num">{vals.length === 0 ? '—' : formatValue(Math.min(...vals))}</td>
                <td className="mt-num">{vals.length === 0 ? '—' : formatValue(Math.max(...vals))}</td>
                <td className="mt-num">{formatPercent(s.completeness)}%</td>
                <td>
                  {[...s.flags.map((f) => FLAG_LABEL[f] ?? f), ...(reasons.length === 0 ? [] : [`값 없는 step: ${reasons.map((r) => REASON_LABEL[r]).join('·')}`])].join(', ') || (
                    <span className="mt-muted">-</span>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
