// 여러 series 시계열 차트 (D05 §03 MetricChart, §08 legend).
// - 값이 없는 step은 선을 끊는다. 0으로 잇지 않는다(계약 6). 집계 중(pending) step은 음영이다.
// - 일부만 집계된(partial) 점은 속이 빈 표식으로 구별한다(provisional window).
// - series는 선 모양 8개(+색 4개)로 구별한다(색만으로 구별하지 않는다). MAX_DRAWN개까지만 그린다.
// - 같은 데이터를 표로도 제공한다(screen reader·키보드 대안).
import { useId } from 'react';
import { REASON_LABEL, formatClock } from '../services/format.ts';
import type { Point } from '../services/red.ts';

const W = 600;
const H = 180;
export const MAX_DRAWN = 8;
// series마다 선 모양이 다르다(8개). 색 4개와 겹쳐 색만으로 구별하는 쌍이 없다(D05 §12).
const DASHES = [undefined, '7 3', '2 3', '9 3 2 3', '1 5', '12 4', '5 2 1 2 1 2', '4 6'] as const;

export interface ChartSeries {
  name: string;
  points: Point[];
}

export function seriesStyle(i: number): { color: string; dash: string | undefined; dashLabel: string } {
  const dash = DASHES[i % DASHES.length];
  return { color: `var(--mt-series-${String((i % 4) + 1)})`, dash, dashLabel: dash === undefined ? '실선' : `선 모양 ${String((i % DASHES.length) + 1)}` };
}

export function SeriesChart({
  title,
  series,
  format,
  unit,
  fromMs,
  toMs,
  timeZone,
}: {
  title: string;
  series: ChartSeries[];
  format: (v: number) => string;
  unit: string;
  fromMs: number;
  toMs: number;
  timeZone: string;
}) {
  const tableId = useId();
  const drawn = series.slice(0, MAX_DRAWN);
  const span = Math.max(toMs - fromMs, 1);
  const values = drawn.flatMap((s) => s.points.flatMap((p) => (p.value === null ? [] : [p.value])));
  const hi = values.length === 0 ? 1 : Math.max(...values, 0);
  const lo = values.length === 0 ? 0 : Math.min(...values, 0);
  const pad = (hi - lo) * 0.1 || 1;
  const top = hi + pad;
  const bottom = lo < 0 ? lo - pad : 0;
  const x = (tMs: number) => ((tMs - fromMs) / span) * W;
  const y = (v: number) => H - ((v - bottom) / (top - bottom)) * H;
  const first = drawn[0]?.points ?? [];
  const step = first.length > 1 ? (first[1]?.tMs ?? 0) - (first[0]?.tMs ?? 0) : span;
  // 한 series라도 집계 중인 step은 음영
  const pendingSteps = [...new Set(drawn.flatMap((s) => s.points.filter((p) => p.value === null && p.reason === 'pending').map((p) => p.tMs)))];
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => fromMs + f * span);
  const summary =
    values.length === 0
      ? `${title}: 표시할 값이 없습니다`
      : `${title}: series ${String(series.length)}개${series.length > drawn.length ? `(그림은 ${String(drawn.length)}개)` : ''}, 최댓값 ${format(hi)}${unit}, 최솟값 ${format(lo)}${unit}`;

  return (
    <figure className="mt-chart mt-series-figure">
      <div className="mt-series-yaxis" aria-hidden="true">
        {[top, (top + bottom) / 2, bottom].map((v, i) => (
          <span key={i}>{format(v)}</span>
        ))}
      </div>
      <svg viewBox={`0 0 ${String(W)} ${String(H)}`} preserveAspectRatio="none" className="mt-chart__plot mt-series-chart" role="img" aria-label={summary}>
        {[0.25, 0.5, 0.75].map((f) => (
          <line key={f} x1="0" x2={W} y1={H * f} y2={H * f} className="mt-chart__grid" vectorEffect="non-scaling-stroke" />
        ))}
        {bottom < 0 && <line x1="0" x2={W} y1={y(0)} y2={y(0)} className="mt-chart__zero" vectorEffect="non-scaling-stroke" />}
        {pendingSteps.map((t) => (
          <rect key={`pending-${String(t)}`} x={x(t)} y="0" width={(step / span) * W} height={H} className="mt-chart__pending" />
        ))}
        {drawn.map((s, i) => {
          const { color, dash } = seriesStyle(i);
          const segments: Point[][] = [];
          let cur: Point[] = [];
          for (const p of s.points) {
            if (p.value === null) {
              if (cur.length > 0) segments.push(cur);
              cur = [];
            } else cur.push(p);
          }
          if (cur.length > 0) segments.push(cur);
          return (
            <g key={s.name} style={{ color }}>
              {segments.map((seg) =>
                seg.length === 1 ? (
                  <circle key={`d-${String(seg[0]?.tMs)}`} cx={x(seg[0]?.tMs ?? 0)} cy={y(seg[0]?.value ?? 0)} r="2" className="mt-series-chart__dot" />
                ) : (
                  <polyline
                    key={`l-${String(seg[0]?.tMs)}`}
                    points={seg.map((p) => `${String(x(p.tMs))},${String(y(p.value ?? 0))}`).join(' ')}
                    className="mt-series-chart__line"
                    strokeDasharray={dash}
                    vectorEffect="non-scaling-stroke"
                  />
                ),
              )}
              {s.points
                .filter((p) => p.value !== null && p.partial)
                .map((p) => (
                  <circle key={`p-${String(p.tMs)}`} cx={x(p.tMs)} cy={y(p.value ?? 0)} r="2.5" className="mt-series-chart__partial" />
                ))}
            </g>
          );
        })}
      </svg>
      <div className="mt-chart__axis mt-series-xaxis" aria-hidden="true">
        {ticks.map((t) => (
          <span key={t}>{formatClock(t, timeZone, span > 24 * 3600_000)}</span>
        ))}
      </div>
      <details className="mt-chart__table">
        <summary>표로 보기</summary>
        <div className="mt-scroll-x">
          <table id={tableId} className="mt-table mt-table--dense">
            <caption className="mt-visually-hidden">{title}</caption>
            <thead>
              <tr>
                <th scope="col">시각</th>
                {drawn.map((s) => (
                  <th key={s.name} scope="col" className="mt-num">
                    {s.name} ({unit.trim() || '-'})
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {first.map((p0, row) => (
                <tr key={p0.tMs}>
                  <td className="mt-mono">{formatClock(p0.tMs, timeZone, true)}</td>
                  {drawn.map((s) => {
                    const p = s.points[row];
                    return (
                      <td key={s.name} className="mt-num">
                        {p === undefined || p.value === null ? `— ${REASON_LABEL[p?.reason ?? 'no_data']}` : `${format(p.value)}${p.partial ? ' (일부 집계)' : ''}`}
                      </td>
                    );
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </details>
    </figure>
  );
}
