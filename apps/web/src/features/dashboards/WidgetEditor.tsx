// 위젯 하나 편집 (D05 §10 "위젯 추가→query→display→preview→배치"). 적용 전까지 dashboard 정의를 바꾸지 않는다.
// filter 값이 "$이름"이면 dashboard 변수다(그 변수 종류의 field에만 쓸 수 있다 — 서버와 같은 규칙).
import { useEffect, useRef, useState, type ReactNode } from 'react';
import type { DashFilter, TimeOverrideRange, Variable, Widget, WidgetKind } from '../../api/dashboards.ts';
import type { MetricAggregation } from '../../api/types.ts';
import { AGG_LABEL } from '../metrics/metricQuery.ts';
import { KIND_LABEL, MAX_TEXT, TIME_OVERRIDES, VARIABLE_FIELD, changeKind, variableRef } from './dashboardSpec.ts';

interface FilterRow {
  field: string;
  value: string;
}

function filterRows(f: DashFilter | null): FilterRow[] {
  const out: FilterRow[] = [];
  const walk = (n: DashFilter) => {
    if (n.op === 'and') (n.args ?? []).forEach(walk);
    else out.push({ field: n.field ?? '', value: n.value ?? '' });
  };
  if (f !== null) walk(f);
  return out;
}

function rowsToFilter(rows: FilterRow[]): DashFilter | null {
  const leaves = rows.filter((r) => r.field.trim() !== '' || r.value.trim() !== '').map((r) => ({ op: 'eq' as const, field: r.field.trim(), value: r.value.trim() }));
  if (leaves.length === 0) return null;
  return leaves.length === 1 ? (leaves[0] ?? null) : { op: 'and', args: leaves };
}

/** 적용 전 검사(서버가 최종 검증한다). 문제가 없으면 빈 객체. */
export function widgetProblems(w: Widget, rows: FilterRow[], groupByText: string, variables: Variable[]): Record<string, string> {
  const out: Record<string, string> = {};
  if (w.kind === 'text') {
    if ((w.text ?? '').trim() === '') out.text = '글을 입력하세요';
    else if ([...(w.text ?? '')].length > MAX_TEXT) out.text = `${String(MAX_TEXT)}자까지입니다`;
    return out;
  }
  if ((w.query?.metric ?? '').trim() === '') out.metric = 'metric 이름을 입력하세요';
  const keys = groupByText.split(',').map((k) => k.trim()).filter((k) => k !== '');
  if (keys.length > 5) out.groupBy = 'group은 5개까지입니다';
  if (w.kind === 'stat' && keys.length > 0) out.groupBy = '숫자 하나 위젯은 group으로 나누지 않습니다';
  if (w.kind === 'top_list' && keys.length === 0) out.groupBy = '상위 목록은 group 하나 이상이 필요합니다';
  rows.forEach((r, i) => {
    if (r.field.trim() === '' || r.value.trim() === '') {
      out[`filter${String(i)}`] = 'label과 값을 모두 입력하세요';
      return;
    }
    const ref = variableRef(r.value.trim());
    if (ref === null) return;
    const v = variables.find((x) => x.name === ref);
    if (v === undefined) out[`filter${String(i)}`] = `변수 $${ref}가 없습니다`;
    else if (VARIABLE_FIELD[v.kind] !== r.field.trim()) out[`filter${String(i)}`] = `$${ref}는 ${VARIABLE_FIELD[v.kind]}에만 쓸 수 있습니다`;
  });
  return out;
}

const KINDS: WidgetKind[] = ['time_series', 'stat', 'table', 'top_list', 'text'];
const AGGS = Object.keys(AGG_LABEL) as MetricAggregation[];

export function WidgetEditor({
  initial,
  variables,
  onApply,
  onCancel,
  renderPreview,
}: {
  initial: Widget;
  variables: Variable[];
  onApply: (w: Widget) => void;
  onCancel: () => void;
  /** 지금 입력으로 위젯을 그린다(D05 §10 "query→display→preview") */
  renderPreview?: (w: Widget) => ReactNode;
}) {
  const [w, setW] = useState<Widget>(initial);
  const [preview, setPreview] = useState<Widget | null>(null);
  const first = useRef<HTMLSelectElement>(null);
  useEffect(() => first.current?.focus(), []);
  const [rows, setRows] = useState<FilterRow[]>(() => filterRows(initial.query?.filter ?? null));
  const [groupBy, setGroupBy] = useState((initial.query?.group_by ?? []).join(', '));
  const [errors, setErrors] = useState<Record<string, string>>({});
  const id = (s: string) => `we-${initial.id}-${s}`;
  const setQuery = (patch: Partial<NonNullable<Widget['query']>>) => setW((x) => (x.query === null ? x : { ...x, query: { ...x.query, ...patch } }));
  const build = (): Widget | null => {
    const found = widgetProblems(w, rows, groupBy, variables);
    setErrors(found);
    if (Object.keys(found).length > 0) return null;
    const keys = groupBy.split(',').map((k) => k.trim()).filter((k) => k !== '');
    return w.query === null ? w : { ...w, title: w.title.trim(), query: { ...w.query, metric: w.query.metric.trim(), filter: rowsToFilter(rows), group_by: [...new Set(keys)] } };
  };
  const apply = () => {
    const built = build();
    if (built !== null) onApply(built);
  };
  const err = (k: string) =>
    errors[k] === undefined ? null : (
      <span id={id(`${k}-error`)} className="mt-dash-error" role="alert">
        {errors[k]}
      </span>
    );
  return (
    <fieldset
      className="mt-dash-fieldset mt-card"
      aria-label={`위젯 ${initial.id} 편집`}
      onKeyDown={(e) => {
        if (e.key === 'Escape') onCancel();
      }}
    >
      <legend>위젯 편집</legend>
      <div className="mt-dash-inline">
        <div className="mt-dash-field">
          <label htmlFor={id('kind')}>종류</label>
          <select ref={first} id={id('kind')} className="mt-select" value={w.kind} onChange={(e) => setW(changeKind(w, e.target.value as WidgetKind))}>
            {KINDS.map((k) => (
              <option key={k} value={k}>
                {KIND_LABEL[k]}
              </option>
            ))}
          </select>
        </div>
        <div className="mt-dash-field">
          <label htmlFor={id('title')}>제목</label>
          <input id={id('title')} className="mt-input" value={w.title} maxLength={200} onChange={(e) => setW({ ...w, title: e.target.value })} />
        </div>
        {w.kind !== 'text' && (
          <div className="mt-dash-field">
            <label htmlFor={id('unit')}>단위</label>
            <input id={id('unit')} className="mt-input" value={w.unit ?? ''} maxLength={32} placeholder="예: ms, req/s" onChange={(e) => setW({ ...w, unit: e.target.value.trim() === '' ? null : e.target.value })} />
          </div>
        )}
      </div>
      {w.kind === 'text' ? (
        <div className="mt-dash-field">
          <label htmlFor={id('text')}>글</label>
          <textarea
            id={id('text')}
            className="mt-input"
            rows={5}
            value={w.text ?? ''}
            onChange={(e) => setW({ ...w, text: e.target.value })}
            aria-describedby={[id('text-hint'), errors.text === undefined ? '' : id('text-error')].filter((s) => s !== '').join(' ')}
          />
          <span id={id('text-hint')} className="mt-label">
            글자 그대로 보입니다(HTML·markdown은 해석하지 않습니다).
          </span>
          {err('text')}
        </div>
      ) : (
        <>
          <div className="mt-dash-inline">
            <div className="mt-dash-field">
              <label htmlFor={id('metric')}>metric</label>
              <input
                id={id('metric')}
                className="mt-input mt-mono"
                value={w.query?.metric ?? ''}
                onChange={(e) => setQuery({ metric: e.target.value })}
                aria-describedby={errors.metric === undefined ? undefined : id('metric-error')}
              />
              {err('metric')}
            </div>
            <div className="mt-dash-field">
              <label htmlFor={id('agg')}>연산</label>
              <select id={id('agg')} className="mt-select" value={w.query?.aggregation} onChange={(e) => setQuery({ aggregation: e.target.value as MetricAggregation })}>
                {AGGS.map((a) => (
                  <option key={a} value={a}>
                    {AGG_LABEL[a]}
                  </option>
                ))}
              </select>
            </div>
          </div>
          <div className="mt-dash-field">
            <label htmlFor={id('group')}>group으로 나누기</label>
            <input
              id={id('group')}
              className="mt-input mt-mono"
              value={groupBy}
              placeholder="label key를 쉼표로(예: service.name)"
              onChange={(e) => setGroupBy(e.target.value)}
              aria-describedby={errors.groupBy === undefined ? undefined : id('groupBy-error')}
            />
            {err('groupBy')}
          </div>
          <fieldset className="mt-dash-fieldset">
            <legend>조건(모두 만족)</legend>
            <span className="mt-label">
              값에 <code>$이름</code>을 쓰면 dashboard 변수를 따릅니다
              {variables.length > 0 && `(${variables.map((v) => `$${v.name} → ${VARIABLE_FIELD[v.kind]}`).join(', ')})`}.
            </span>
            {rows.map((r, i) => (
              <div key={i} className="mt-dash-inline">
                <div className="mt-dash-field">
                  <label htmlFor={id(`ff${String(i)}`)}>label {i + 1}</label>
                  <input
                    id={id(`ff${String(i)}`)}
                    className="mt-input mt-mono"
                    value={r.field}
                    onChange={(e) => setRows(rows.map((x, j) => (j === i ? { ...x, field: e.target.value } : x)))}
                  />
                </div>
                <div className="mt-dash-field">
                  <label htmlFor={id(`fv${String(i)}`)}>값 {i + 1}</label>
                  <input
                    id={id(`fv${String(i)}`)}
                    className="mt-input mt-mono"
                    value={r.value}
                    onChange={(e) => setRows(rows.map((x, j) => (j === i ? { ...x, value: e.target.value } : x)))}
                    aria-describedby={errors[`filter${String(i)}`] === undefined ? undefined : id(`filter${String(i)}-error`)}
                  />
                  {err(`filter${String(i)}`)}
                </div>
                <button type="button" className="mt-button" onClick={() => setRows(rows.filter((_, j) => j !== i))} aria-label={`조건 ${String(i + 1)} 지우기`}>
                  지우기
                </button>
              </div>
            ))}
            <div>
              <button type="button" className="mt-button" onClick={() => setRows([...rows, { field: '', value: '' }])} disabled={rows.length >= 20}>
                조건 추가
              </button>
            </div>
          </fieldset>
          <div className="mt-dash-inline">
            {w.kind !== 'time_series' && (
              <div className="mt-dash-field">
                <label htmlFor={id('dec')}>소수 자릿수</label>
                <input
                  id={id('dec')}
                  className="mt-input"
                  type="number"
                  min={0}
                  max={6}
                  value={w.display.decimals ?? 2}
                  onChange={(e) => setW({ ...w, display: { ...w.display, decimals: Math.min(6, Math.max(0, Math.trunc(Number(e.target.value) || 0))) } })}
                />
              </div>
            )}
            {(w.kind === 'table' || w.kind === 'top_list') && (
              <>
                <div className="mt-dash-field">
                  <label htmlFor={id('limit')}>보일 행</label>
                  <input
                    id={id('limit')}
                    className="mt-input"
                    type="number"
                    min={1}
                    max={100}
                    value={w.display.limit ?? 10}
                    onChange={(e) => setW({ ...w, display: { ...w.display, limit: Math.min(100, Math.max(1, Math.trunc(Number(e.target.value) || 1))) } })}
                  />
                </div>
                <div className="mt-dash-field">
                  <label htmlFor={id('sort')}>정렬</label>
                  <select id={id('sort')} className="mt-select" value={w.display.sort ?? 'desc'} onChange={(e) => setW({ ...w, display: { ...w.display, sort: e.target.value as 'desc' | 'asc' } })}>
                    <option value="desc">큰 값 먼저</option>
                    <option value="asc">작은 값 먼저</option>
                  </select>
                </div>
              </>
            )}
            <div className="mt-dash-field">
              <label htmlFor={id('time')}>시간 범위</label>
              <select
                id={id('time')}
                className="mt-select"
                value={w.time_override?.range ?? ''}
                onChange={(e) => setW({ ...w, time_override: e.target.value === '' ? null : { range: e.target.value as TimeOverrideRange } })}
              >
                <option value="">dashboard 범위를 따름</option>
                {TIME_OVERRIDES.map((r) => (
                  <option key={r} value={r}>
                    최근 {r}
                  </option>
                ))}
              </select>
            </div>
          </div>
        </>
      )}
      {renderPreview !== undefined && preview !== null && (
        <div className="mt-dash-preview-widget" aria-label="위젯 미리보기">
          {renderPreview(preview)}
        </div>
      )}
      <div className="mt-dash-actions">
        <button type="button" className="mt-button mt-button--primary" onClick={apply}>
          위젯 적용
        </button>
        {renderPreview !== undefined && (
          <button
            type="button"
            className="mt-button"
            onClick={() => {
              const built = build();
              if (built !== null) setPreview(built);
            }}
          >
            미리보기
          </button>
        )}
        <button type="button" className="mt-button" onClick={onCancel}>
          취소
        </button>
      </div>
    </fieldset>
  );
}
