// S04·S08 건수·facet·히스토그램 (ADR 0074). 집계 응답을 화면 상태로 바꾸고 그린다.
// - 집계가 꺼졌으면(404) 건수 자리를 보이지 않는다. 불러오는 중·실패는 "건수를 볼 수 없음"이다 — 0으로 보이지 않는다(계약 6).
// - facet 수는 그 facet의 조건을 빼고 센 것이다(excludes_own_filter). 그래서 고른 뒤에도 다른 값의 수가 보이고, 합이 총 수와 다르다.
// - 칸의 count가 null(future)이면 막대를 그리지 않고 "아직 없음"으로, incomplete면 빗금으로 "수집 중일 수 있음"을 보인다.
// - 빈 지난 칸(0)은 "저장된 행 없음"이다. 수집 공백·보존 만료를 구별하지 못한다(ADR 0074 알려진 한계).
// - trace 수는 표본의 수다. 오류 trace 비율(%)은 보이지 않는다(tail sampling은 오류를 먼저 남긴다, 계약 5).
import type { AggregateBucket, AggregateData, AggregateFacet } from '../../api/aggregate.ts';
import type { RemoteState } from '../../api/useRemote.ts';
import { formatClock } from '../services/format.ts';
import { MAX_URL_IDS } from '../logs/logFilters.ts';

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
      return `${noun} 수를 볼 수 없음${unavailableHint(a.status)}`;
    case 'ok':
      return `${noun} ${a.data.total.toLocaleString('ko-KR')}개`;
  }
}

/** 집계 실패의 다음 행동(ADR 0074 §5): 예산·시간 초과는 범위를 줄이고, 동시 실행 상한은 잠시 뒤 다시. */
export function unavailableHint(status: number): string {
  if (status === 422 || status === 504) return ' · 범위를 줄이세요';
  if (status === 429) return ' · 잠시 뒤 새로고침하세요';
  return '';
}

export interface Total {
  /** 수가 있다(그 수 "중" 불러온 행) — 아니면 상태 글이다 */
  ok: boolean;
  text: string;
}

/** 결과 머리의 총 수. 꺼졌으면 null. */
export function totalOf(a: Availability, noun: string): Total | null {
  const text = totalText(a, noun);
  return text === null ? null : { ok: a.kind === 'ok', text };
}

export function facetOf(a: Availability, field: string): AggregateFacet | null {
  return a.kind === 'ok' ? (a.data.facets.find((f) => f.field === field) ?? null) : null;
}

export interface FacetRow {
  value: string;
  count: number | null;
  /** 수 칸의 글: 수, "상위 밖"(top_n 밖 — 0이 아니다), 또는 ""(셀 수 없음·아직 모름) */
  countText: string;
  selected: boolean;
}

/**
 * checkbox 목록: 응답의 top 값 + 고른 값. 고른 값이 응답에 없으면
 * - 셀 수 없는 facet(values null)·집계 없음: 수 칸을 비운다(이유는 facet 안내에)
 * - 그 밖(other_count) > 0: "상위 밖"
 * - 그 밖이 0: 저장된 행이 없다(0)
 */
export function facetRows(facet: AggregateFacet | null, selected: readonly string[]): FacetRow[] {
  const rows: FacetRow[] = (facet?.values ?? []).map((v) => ({
    value: v.value,
    count: v.count,
    countText: v.count.toLocaleString('ko-KR'),
    selected: selected.includes(v.value),
  }));
  for (const s of selected) {
    if (rows.some((r) => r.value === s)) continue;
    if (facet === null || facet.values === null) rows.push({ value: s, count: null, countText: '', selected: true });
    else if ((facet.other_count ?? 0) > 0) rows.push({ value: s, count: null, countText: '상위 밖', selected: true });
    else rows.push({ value: s, count: 0, countText: '0', selected: true });
  }
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
  hint,
  a,
  field,
  selected,
  label,
  onToggle,
}: {
  id: string;
  title: string;
  /** facet 뜻 설명(legend 아래) */
  hint?: string;
  a: Availability;
  field: string;
  selected: readonly string[];
  label: (value: string) => string;
  onToggle: (value: string) => void;
}) {
  if (a.kind === 'off') return null;
  const facet = facetOf(a, field);
  const rows = facetRows(facet, selected);
  const note = a.kind === 'unavailable' ? `건수를 볼 수 없음${unavailableHint(a.status)}` : a.kind === 'loading' ? '세는 중' : facetNote(facet);
  const full = selected.length >= MAX_URL_IDS;
  const described = [hint === undefined ? null : `${id}-hint`, note === null ? null : `${id}-note`, full ? `${id}-full` : null].filter((x) => x !== null);
  return (
    <fieldset className="mt-facet" aria-describedby={described.length === 0 ? undefined : described.join(' ')}>
      <legend className="mt-facet__title">{title}</legend>
      {hint !== undefined && (
        <span id={`${id}-hint`} className="mt-label">
          {hint}
        </span>
      )}
      {note !== null && (
        <span id={`${id}-note`} className="mt-label">
          {note}
        </span>
      )}
      {rows.map((r, i) => (
        <div key={r.value} className="mt-field mt-field--check mt-facet__row">
          {/* 상한(20개)에 닿으면 고르지 않은 값은 막고 이유를 보인다 */}
          <input id={`${id}-${String(i)}`} type="checkbox" checked={r.selected} disabled={full && !r.selected} onChange={() => onToggle(r.value)} />
          <label htmlFor={`${id}-${String(i)}`}>{label(r.value)}</label>
          <span className="mt-facet__count mt-num">{r.countText}</span>
        </div>
      ))}
      {full && (
        <span id={`${id}-full`} className="mt-label">
          최대 {MAX_URL_IDS}개까지 고를 수 있습니다.
        </span>
      )}
      {a.kind === 'ok' && rows.length === 0 && facet !== null && facet.values !== null && <span className="mt-label">이 범위에 값 없음</span>}
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
