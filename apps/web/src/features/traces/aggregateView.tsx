// S04·S08 건수·facet·히스토그램 (ADR 0074). 집계 응답을 화면 상태로 바꾸고 그린다.
// - 집계가 꺼졌으면(404) 건수 자리를 보이지 않는다. 불러오는 중·실패는 "건수를 볼 수 없음"이다 — 0으로 보이지 않는다(계약 6).
// - facet 수는 그 facet의 조건을 빼고 센 것이다(excludes_own_filter). 그래서 고른 뒤에도 다른 값의 수가 보이고, 합이 총 수와 다르다.
// - 칸의 count가 null(future)이면 막대를 그리지 않고 "아직 없음"으로, incomplete면 빗금으로 "수집 중일 수 있음"을 보인다.
// - 빈 지난 칸(0)은 "저장된 행 없음"이다. 수집 공백·보존 만료를 구별하지 못한다(ADR 0074 알려진 한계).
// - trace 수는 표본의 수다. 오류 trace 비율(%)은 보이지 않는다(tail sampling은 오류를 먼저 남긴다, 계약 5).
import type { AggregateBucket, AggregateData, AggregateFacet } from '../../api/aggregate.ts';
import type { RemoteState } from '../../api/useRemote.ts';
import { formatClock } from '../services/format.ts';

export type Availability =
  | { kind: 'off' }
  | { kind: 'loading' }
  | { kind: 'unavailable'; status: number }
  | { kind: 'ok'; data: AggregateData; refreshing: boolean };

/** 집계 응답 상태. 404는 기능 꺼짐, 그 밖의 실패는 지난 값을 지금 값처럼 보이지 않는다. */
export function availability(remote: RemoteState<AggregateData>): Availability {
  if (remote.error?.status === 404) return { kind: 'off' };
  if (remote.error !== null) return { kind: 'unavailable', status: remote.error.status };
  if (remote.data === null) return { kind: 'loading' };
  return { kind: 'ok', data: remote.data, refreshing: remote.refreshing };
}

/** 총 수 글. noun은 "저장된 trace"·"log". 없으면 null(기능 꺼짐). */
export function totalText(a: Availability, noun: string): string | null {
  switch (a.kind) {
    case 'off':
      return null;
    case 'loading':
      return `${noun} 수 세는 중`;
    case 'unavailable':
      return a.status === 422 ? `${noun} 수를 볼 수 없음 · 범위가 너무 큼` : `${noun} 수를 볼 수 없음`;
    case 'ok':
      return `${noun} ${a.data.total.toLocaleString('ko-KR')}개`;
  }
}

export function facetOf(a: Availability, field: string): AggregateFacet | null {
  return a.kind === 'ok' ? (a.data.facets.find((f) => f.field === field) ?? null) : null;
}

export interface FacetRow {
  value: string;
  /** null이면 top_n 밖이라 이 응답에 수가 없다(0이 아니다) */
  count: number | null;
  selected: boolean;
}

/** checkbox 목록: 응답의 top 값 + 고른 값(응답에 없으면 수 없음). 수 내림차순 그대로, 고른 값 중 빠진 것은 뒤에. */
export function facetRows(facet: AggregateFacet | null, selected: readonly string[]): FacetRow[] {
  const rows: FacetRow[] = (facet?.values ?? []).map((v) => ({ value: v.value, count: v.count, selected: selected.includes(v.value) }));
  for (const s of selected) if (!rows.some((r) => r.value === s)) rows.push({ value: s, count: null, selected: true });
  return rows;
}

const FACET_REASON: Record<string, string> = {
  too_many_values: '값이 너무 많아 셀 수 없음',
  budget_exceeded: '조회 예산을 넘어 셀 수 없음',
  timeout: '시간 안에 셀 수 없음',
};

/** facet 머리 아래 안내(셀 수 없는 이유, 조건을 빼고 셌다는 것, top 밖·값 없음 수). 없으면 null. */
export function facetNote(facet: AggregateFacet | null): string | null {
  if (facet === null) return null;
  if (facet.values === null) return FACET_REASON[facet.reason ?? ''] ?? '셀 수 없음';
  const parts: string[] = [];
  if (facet.excludes_own_filter) parts.push('이 항목의 조건을 빼고 센 수');
  if ((facet.other_count ?? 0) > 0) parts.push(`그 밖 ${(facet.other_count ?? 0).toLocaleString('ko-KR')}개`);
  if ((facet.missing_count ?? 0) > 0) parts.push(`값 없음 ${(facet.missing_count ?? 0).toLocaleString('ko-KR')}개`);
  return parts.length === 0 ? null : parts.join(' · ');
}

/** 값별 checkbox. 집계가 없으면(꺼짐·실패) 그리지 않는다 — 조건 선택은 위 select가 계속 한다. */
export function FacetChecklist({
  id,
  title,
  a,
  field,
  selected,
  label,
  onToggle,
}: {
  id: string;
  title: string;
  a: Availability;
  field: string;
  selected: readonly string[];
  label: (value: string) => string;
  onToggle: (value: string) => void;
}) {
  if (a.kind === 'off') return null;
  const facet = facetOf(a, field);
  const rows = facetRows(facet, selected);
  const note = a.kind === 'unavailable' ? '건수를 볼 수 없음' : a.kind === 'loading' ? '세는 중' : facetNote(facet);
  return (
    <fieldset className="mt-facet" aria-describedby={note === null ? undefined : `${id}-note`}>
      <legend className="mt-facet__title">{title}</legend>
      {note !== null && (
        <span id={`${id}-note`} className="mt-label">
          {note}
        </span>
      )}
      {rows.map((r, i) => (
        <div key={r.value} className="mt-field mt-field--check mt-facet__row">
          <input id={`${id}-${String(i)}`} type="checkbox" checked={r.selected} onChange={() => onToggle(r.value)} />
          <label htmlFor={`${id}-${String(i)}`}>{label(r.value)}</label>
          <span className="mt-facet__count mt-num">{r.count === null ? (a.kind === 'ok' ? '상위 밖' : '') : r.count.toLocaleString('ko-KR')}</span>
        </div>
      ))}
      {a.kind === 'ok' && rows.length === 0 && facet?.values !== null && <span className="mt-label">이 범위에 값 없음</span>}
    </fieldset>
  );
}

export interface Segment {
  key: string;
  label: string;
  className: string;
}

/** 칸의 막대 조각 값(segments 순서). null 칸(future)은 호출하지 않는다. */
export type SegmentValues = (b: AggregateBucket) => number[];

/** 범위 전체 시간 히스토그램. 막대 높이는 칸의 count, 조각은 segments(아래에서 위로). */
export function CountHistogram({
  title,
  buckets,
  segments,
  values,
  timeZone,
  noun,
}: {
  title: string;
  buckets: AggregateBucket[];
  segments: Segment[];
  values: SegmentValues;
  timeZone: string;
  noun: string;
}) {
  const max = Math.max(1, ...buckets.map((b) => b.count ?? 0));
  const first = buckets[0];
  const last = buckets[buckets.length - 1];
  const fromMs = first === undefined ? 0 : Date.parse(first.start);
  const toMs = last === undefined ? 0 : Date.parse(last.end);
  const withDate = toMs - fromMs > 24 * 3600_000;
  const future = buckets.filter((b) => b.count === null).length;
  const incomplete = buckets.some((b) => b.incomplete);
  return (
    <figure className="mt-hist">
      <div className="mt-hist__legend">
        {segments.map((s) => (
          <span key={s.key} className="mt-legend">
            <span className={`mt-hist__swatch ${s.className}`} aria-hidden="true" />
            {s.label}
          </span>
        ))}
        {incomplete && (
          <span className="mt-legend">
            <span className="mt-hist__swatch mt-hist__swatch--incomplete" aria-hidden="true" />
            수집 중일 수 있음
          </span>
        )}
        {future > 0 && <span className="mt-legend">점선 칸 · 아직 오지 않은 시각</span>}
      </div>
      <div className="mt-hist__plot" role="img" aria-label={`${title}. 칸 ${String(buckets.length)}개, 칸당 최대 ${max.toLocaleString('ko-KR')}개. 칸별 수는 아래 표에 있습니다.`}>
        {buckets.map((b) => {
          if (b.count === null) return <span key={b.start} className="mt-hist__bar mt-hist__bar--future" />;
          const vals = values(b);
          return (
            <span key={b.start} className={b.incomplete ? 'mt-hist__bar mt-hist__bar--incomplete' : 'mt-hist__bar'} style={{ height: `${String((b.count / max) * 100)}%` }}>
              {segments.map((s, i) => {
                const v = vals[i] ?? 0;
                return v > 0 ? <span key={s.key} className={s.className} style={{ flexGrow: v }} /> : null;
              })}
            </span>
          );
        })}
      </div>
      <div className="mt-chart__axis" aria-hidden="true">
        {[0, 0.5, 1].map((f) => (
          <span key={f}>{formatClock(fromMs + f * (toMs - fromMs), timeZone, withDate)}</span>
        ))}
      </div>
      <table className="mt-visually-hidden">
        <caption>{title}</caption>
        <thead>
          <tr>
            <th scope="col">시작</th>
            <th scope="col">{noun} 수</th>
            {segments.map((s) => (
              <th key={s.key} scope="col">
                {s.label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {buckets.map((b) => (
            <tr key={b.start}>
              <td>{formatClock(Date.parse(b.start), timeZone, withDate)}</td>
              <td>{b.count === null ? '아직 없음' : `${b.count.toLocaleString('ko-KR')}${b.incomplete ? ' (수집 중일 수 있음)' : ''}`}</td>
              {segments.map((s, i) => (
                <td key={s.key}>{b.count === null ? '' : (values(b)[i] ?? 0).toLocaleString('ko-KR')}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </figure>
  );
}
