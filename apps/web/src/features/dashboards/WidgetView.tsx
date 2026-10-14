// 위젯 하나 (D05 §10~11, ADR 0058). 상태 계약(D05 §03):
// - 값이 없으면(결측·집계 중·조회 실패) 0이나 빈칸이 아니라 이유를 보인다(계약 6).
// - 일부만 집계된 값(partial)은 "일부"로 표시한다. 마지막 성공 뒤 실패하면 그 값을 유지하고 "갱신 실패"를 붙인다.
// - text 위젯은 글자 그대로(HTML·markdown 해석 없음).
import { useState } from 'react';
import type { Widget } from '../../api/dashboards.ts';
import type { MetricResult, MetricSeries } from '../../api/types.ts';
import { useRemote } from '../../api/useRemote.ts';
import { SeriesChart } from '../metrics/SeriesChart.tsx';
import { formatValue, seriesName } from '../metrics/metricQuery.ts';
import { REASON_LABEL, formatTimeOfDay } from '../services/format.ts';
import { toPoints } from '../services/red.ts';
import { StatusBadge, describeError } from '../services/states.tsx';
import type { QueryPool } from './queryPool.ts';
import { KIND_LABEL, queryKey, resolveQuery, widgetRange, widgetStateKey, widgetVariables, type VariableValues } from './dashboardSpec.ts';

export interface WidgetViewProps {
  widget: Widget;
  values: VariableValues;
  /** 조사 context 범위의 식별값(상대 범위면 "1h" 같은 값) — 화면 상태 key에 쓴다 */
  rangeKey: string;
  /** 상단 조사 context의 환경(없으면 null) */
  globalEnv: string | null;
  fromMs: number;
  toMs: number;
  nowMs: number;
  refreshTick: number;
  pool: QueryPool<MetricResult>;
  timeZone: string;
}

const fixed = (decimals: number) => new Intl.NumberFormat('ko-KR', { maximumFractionDigits: decimals, minimumFractionDigits: 0 });

/** 한 점 요약 series의 값. 값이 없으면 null과 이유. */
export function summaryValue(s: MetricSeries): { value: number | null; reason: string | null; partial: boolean } {
  const p = s.points[0];
  if (p === undefined) return { value: null, reason: REASON_LABEL.no_series, partial: false };
  if (p.v === null) return { value: null, reason: REASON_LABEL[p.reason ?? 'no_data'], partial: p.partial };
  return { value: p.v, reason: null, partial: p.partial };
}

/** table·top_list 행: 값이 있는 행을 정렬하고, 값이 없는 행은 뒤에 이유와 함께 둔다(0으로 정렬하지 않는다). */
export function rankedRows(series: MetricSeries[], groupBy: string[], sort: 'desc' | 'asc', limit: number) {
  const rows = series.map((s) => ({ name: seriesName(s.labels, groupBy), ...summaryValue(s) }));
  const valued = rows.filter((r) => r.value !== null).sort((a, b) => (sort === 'desc' ? (b.value ?? 0) - (a.value ?? 0) : (a.value ?? 0) - (b.value ?? 0)));
  const missing = rows.filter((r) => r.value === null);
  const all = [...valued, ...missing];
  return { rows: all.slice(0, limit), hidden: Math.max(all.length - limit, 0) };
}

export function WidgetView({ widget: w, values, rangeKey, globalEnv, fromMs, toMs, nowMs, refreshTick, pool, timeZone }: WidgetViewProps) {
  const [retry, setRetry] = useState(0);
  const range = widgetRange(w, fromMs, toMs, nowMs);
  const q = resolveQuery(w, values, range.fromMs, range.toMs, globalEnv);
  // 화면 상태 key는 조회 조건만(분마다 움직이는 절대 시각은 빼고) — 갱신 중 이전 값 유지, 실패 시 마지막 값 + "갱신 실패"
  const stateKey = q === null ? null : widgetStateKey(w, values, rangeKey, globalEnv);
  const remote = useRemote(stateKey, refreshTick + retry, (signal) => (q === null ? Promise.reject(new Error('no query')) : pool.run(queryKey(q), signal)));
  const partialStore = remote.meta?.partial === true;
  const vars = widgetVariables(w);
  const unit = w.unit ?? '';
  const title = w.title === '' ? (w.query?.metric ?? KIND_LABEL[w.kind]) : w.title;

  if (w.kind === 'text') {
    return (
      <article className="mt-card mt-dash-widget" aria-label={title}>
        {w.title !== '' && <h3 className="mt-card__title">{w.title}</h3>}
        <p className="mt-dash-text">{w.text}</p>
      </article>
    );
  }

  let body;
  if (q === null) {
    body = <p className="mt-muted">조회 범위가 7일을 넘어 그릴 수 없습니다. 범위를 줄이세요.</p>;
  } else if (remote.data === null) {
    body =
      remote.error !== null ? (
        // 위젯마다 live region을 두지 않는다(50개가 갱신마다 읽히지 않게, D05 §12). 오류는 정적 글과 재시도
        <div className="mt-dash-error-box">
          <p className="mt-muted">
            {describeError(remote.error).title} — {remote.error.body.message}
          </p>
          {remote.error.body.request_id !== '' && <span className="mt-label mt-mono">request_id {remote.error.body.request_id}</span>}
          <button type="button" className="mt-button" onClick={() => setRetry((n) => n + 1)}>
            다시 시도
          </button>
        </div>
      ) : (
        <div className="mt-skeleton mt-dash-skeleton" aria-label={`${title} 불러오는 중`} />
      );
  } else {
    const series = remote.data.series;
    const groupBy = w.query?.group_by ?? [];
    if (series.length === 0) {
      body = <p className="mt-muted">받은 metric이 없습니다(값이 0이라는 뜻이 아닙니다).</p>;
    } else if (w.kind === 'time_series') {
      body = (
        <SeriesChart
          title={title}
          series={series.map((s) => ({ name: seriesName(s.labels, groupBy), points: toPoints(s) }))}
          format={formatValue}
          unit={unit}
          fromMs={Date.parse(q.range.from)}
          toMs={Date.parse(q.range.to)}
          timeZone={timeZone}
        />
      );
    } else if (w.kind === 'stat') {
      const v = summaryValue(series[0] as MetricSeries);
      body = (
        <p className="mt-dash-stat">
          {v.value === null ? (
            <span className="mt-muted">{v.reason}</span>
          ) : (
            <>
              <strong>{fixed(w.display.decimals ?? 2).format(v.value)}</strong>
              {unit !== '' && <span className="mt-dash-unit">{unit}</span>}
              {(v.partial || partialStore) && <span className="mt-muted"> (일부)</span>}
            </>
          )}
        </p>
      );
    } else {
      const { rows, hidden } = rankedRows(series, groupBy, w.display.sort ?? 'desc', w.display.limit ?? 10);
      const max = Math.max(...rows.map((r) => r.value ?? 0), 0);
      body = (
        <div className="mt-scroll-x">
          <table className="mt-table mt-dash-table">
            <caption className="mt-visually-hidden">{title}</caption>
            <thead>
              <tr>
                <th scope="col">{groupBy.length === 0 ? '대상' : groupBy.join(', ')}</th>
                <th scope="col" className="mt-num">
                  값{unit !== '' && ` (${unit})`}
                </th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.name}>
                  <th scope="row" className="mt-mono mt-dash-row">
                    {r.name}
                  </th>
                  <td className="mt-num">
                    {r.value === null ? (
                      <span className="mt-muted">{r.reason}</span>
                    ) : (
                      <>
                        {w.kind === 'top_list' && max > 0 && <span className="mt-dash-bar" style={{ width: `${String((r.value / max) * 100)}%` }} aria-hidden="true" />}
                        <span className="mt-dash-value">
                          {fixed(w.display.decimals ?? 2).format(r.value)}
                          {(r.partial || partialStore) && <span className="mt-muted"> (일부)</span>}
                        </span>
                      </>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {hidden > 0 && <p className="mt-label">{w.display.sort === 'asc' ? '작은' : '큰'} 순서로 {rows.length}개만 보입니다(나머지 {hidden}개).</p>}
        </div>
      );
    }
  }

  return (
    <article className="mt-card mt-dash-widget" aria-label={title} aria-busy={remote.refreshing || (remote.data === null && remote.error === null && q !== null)}>
      <header className="mt-dash-widget__head">
        <h3 className="mt-card__title">{title}</h3>
        <span className="mt-dash-badges">
          {vars.map((v) => (
            <span key={v} className="mt-source-badge" title="dashboard 변수를 따릅니다">
              ${v}
            </span>
          ))}
          {w.time_override !== null && (
            <span className="mt-source-badge" title="이 위젯만 다른 시간 범위입니다">
              최근 {w.time_override.range}
            </span>
          )}
          {partialStore && remote.error === null && (
            <StatusBadge tone="warning">일부 저장소 응답 없음</StatusBadge>
          )}
          {remote.error !== null && remote.data !== null && (
            <>
              <StatusBadge tone="warning">
                갱신 실패{remote.lastSuccessMs !== null && ` · ${formatTimeOfDay(remote.lastSuccessMs, timeZone)} 값`}
              </StatusBadge>
              <button type="button" className="mt-button mt-dash-retry" onClick={() => setRetry((n) => n + 1)} aria-label={`${title} 다시 시도`}>
                다시 시도
              </button>
            </>
          )}
        </span>
      </header>
      {body}
    </article>
  );
}
