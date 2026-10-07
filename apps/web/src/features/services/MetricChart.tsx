// 시계열 차트 (D05 §03 MetricChart 최소 구현).
// - 값이 없는 step은 선을 끊는다. 0으로 잇지 않는다(계약 6). 집계 중(pending) 구간은 음영으로 표시한다.
// - 일부만 집계된(partial) 점은 속이 빈 표식으로 구별한다.
// - 같은 데이터를 표로도 제공한다(screen reader·키보드 대안).
import { useId } from 'react';
import { REASON_LABEL, formatClock } from './format.ts';
import type { Point } from './red.ts';

const W = 600;
const H = 120;

export interface MetricChartProps {
  title: string;
  points: Point[];
  format: (v: number) => string;
  unit: string;
  fromMs: number;
  toMs: number;
  timeZone: string;
}

export function MetricChart({ title, points, format, unit, fromMs, toMs, timeZone }: MetricChartProps) {
  const tableId = useId();
  const span = Math.max(toMs - fromMs, 1);
  const values = points.flatMap((p) => (p.value === null ? [] : [p.value]));
  const max = values.length === 0 ? 1 : Math.max(...values) * 1.15 || 1;
  const x = (tMs: number) => ((tMs - fromMs) / span) * W;
  const y = (v: number) => H - (v / max) * H;
  const step = points.length > 1 ? (points[1]?.tMs ?? 0) - (points[0]?.tMs ?? 0) : span;

  // 연속된 값 있는 점을 한 선분으로 (null에서 끊는다)
  const segments: Point[][] = [];
  let cur: Point[] = [];
  for (const p of points) {
    if (p.value === null) {
      if (cur.length > 0) segments.push(cur);
      cur = [];
    } else {
      cur.push(p);
    }
  }
  if (cur.length > 0) segments.push(cur);

  const nullCount = points.filter((p) => p.value === null).length;
  const last = [...points].reverse().find((p) => p.value !== null);
  const summary =
    values.length === 0
      ? `${title}: 표시할 값이 없습니다`
      : `${title}: 최근 ${format(last?.value ?? 0)}${unit}, 최대 ${format(Math.max(...values))}${unit}` +
        (nullCount > 0 ? `, 값 없는 step ${String(nullCount)}개` : '');
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => fromMs + f * span);

  return (
    <figure className="mt-chart">
      <svg viewBox={`0 0 ${String(W)} ${String(H)}`} preserveAspectRatio="none" className="mt-chart__plot" role="img" aria-label={summary}>
        {[0.25, 0.5, 0.75].map((f) => (
          <line key={f} x1="0" x2={W} y1={H * f} y2={H * f} className="mt-chart__grid" vectorEffect="non-scaling-stroke" />
        ))}
        {points
          .filter((p) => p.value === null && p.reason === 'pending')
          .map((p) => (
            <rect key={`pending-${String(p.tMs)}`} x={x(p.tMs)} y="0" width={(step / span) * W} height={H} className="mt-chart__pending" />
          ))}
        {segments.map((seg) =>
          seg.length === 1 ? (
            <circle key={`s-${String(seg[0]?.tMs)}`} cx={x(seg[0]?.tMs ?? 0)} cy={y(seg[0]?.value ?? 0)} r="2" className="mt-chart__dot" />
          ) : (
            <polyline
              key={`s-${String(seg[0]?.tMs)}`}
              points={seg.map((p) => `${String(x(p.tMs))},${String(y(p.value ?? 0))}`).join(' ')}
              className="mt-chart__line"
              vectorEffect="non-scaling-stroke"
            />
          ),
        )}
        {points
          .filter((p) => p.value !== null && p.partial)
          .map((p) => (
            <circle key={`p-${String(p.tMs)}`} cx={x(p.tMs)} cy={y(p.value ?? 0)} r="2.5" className="mt-chart__partial" />
          ))}
      </svg>
      <div className="mt-chart__axis" aria-hidden="true">
        {ticks.map((t) => (
          <span key={t}>{formatClock(t, timeZone, span > 24 * 3600_000)}</span>
        ))}
      </div>
      <details className="mt-chart__table">
        <summary>표로 보기</summary>
        <table id={tableId} className="mt-table mt-table--dense">
          <caption className="mt-visually-hidden">{title}</caption>
          <thead>
            <tr>
              <th scope="col">시각</th>
              <th scope="col" className="mt-num">
                값 ({unit.trim() || '-'})
              </th>
              <th scope="col">비고</th>
            </tr>
          </thead>
          <tbody>
            {points.map((p) => (
              <tr key={p.tMs}>
                <td className="mt-mono">{formatClock(p.tMs, timeZone, true)}</td>
                <td className="mt-num">{p.value === null ? '—' : format(p.value)}</td>
                <td>{p.value === null ? REASON_LABEL[p.reason ?? 'no_data'] : p.partial ? '일부 집계' : ''}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </details>
    </figure>
  );
}
