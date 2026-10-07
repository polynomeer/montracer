// duration 분포 산점도 (D05 §06 Explorer 중앙). 지금까지 불러온 결과 trace를 시작 시각 × duration으로 찍는다.
// - 오류 trace는 색만이 아니라 모양(다이아몬드)으로도 구별한다(D05 §02).
// - 끌어서 시간 구간을 고르면 조사 범위가 그 절대 구간으로 바뀐다(brushing). 되돌리기는 브라우저 뒤로 가기·"범위 되돌리기".
// - 같은 데이터는 아래 결과 표가 대안이다(키보드·screen reader).
import { useRef, useState, type PointerEvent } from 'react';
import type { TraceSummary } from '../../api/types.ts';
import { formatClock, formatMilliseconds } from '../services/format.ts';

export interface TraceScatterProps {
  rows: TraceSummary[];
  fromMs: number;
  toMs: number;
  timeZone: string;
  onBrush: (fromMs: number, toMs: number) => void;
}

export function TraceScatter({ rows, fromMs, toMs, timeZone, onBrush }: TraceScatterProps) {
  const boxRef = useRef<HTMLDivElement>(null);
  const [drag, setDrag] = useState<{ x0: number; x1: number } | null>(null);
  const span = Math.max(toMs - fromMs, 1);
  const maxMs = Math.max(1, ...rows.map((r) => r.duration_ms)) * 1.1;
  const errors = rows.filter((r) => r.has_error).length;

  const toX = (e: PointerEvent<HTMLDivElement>) => {
    const rect = boxRef.current?.getBoundingClientRect();
    if (rect === undefined || rect.width === 0) return 0;
    return Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width));
  };
  const finish = () => {
    if (drag !== null && Math.abs(drag.x1 - drag.x0) > 0.008) {
      const a = fromMs + Math.min(drag.x0, drag.x1) * span;
      const b = fromMs + Math.max(drag.x0, drag.x1) * span;
      onBrush(Math.floor(a), Math.ceil(b));
    }
    setDrag(null);
  };
  const pct = (v: number) => `${String(Math.min(100, Math.max(0, v * 100)))}%`;

  return (
    <figure className="mt-scatter">
      <div className="mt-scatter__plot">
        <div className="mt-scatter__yaxis mt-num" aria-hidden="true">
          <span>{formatMilliseconds(maxMs)}</span>
          <span>{formatMilliseconds(maxMs / 2)}</span>
          <span>0</span>
        </div>
        {/* 점은 HTML로 찍는다(SVG 비율 늘림에 표식 모양이 찌그러지지 않게) */}
        <div
          ref={boxRef}
          className="mt-scatter__box"
          role="img"
          aria-label={`trace ${String(rows.length)}개의 duration 분포, 오류 ${String(errors)}개. 끌어서 시간 구간을 고를 수 있습니다. 키보드로는 상단 시간 선택을 쓰세요. 같은 데이터는 아래 결과 표에 있습니다.`}
          onPointerDown={(e) => {
            const v = toX(e);
            setDrag({ x0: v, x1: v });
            e.currentTarget.setPointerCapture?.(e.pointerId);
          }}
          onPointerMove={(e) => {
            if (drag !== null) setDrag({ ...drag, x1: toX(e) });
          }}
          onPointerUp={finish}
          onPointerCancel={() => setDrag(null)}
        >
          {rows.map((r) => (
            <span
              key={r.trace_id}
              className={r.has_error ? 'mt-scatter__err' : 'mt-scatter__ok'}
              style={{ left: pct((Date.parse(r.start_time) - fromMs) / span), top: pct(1 - r.duration_ms / maxMs) }}
            />
          ))}
          {drag !== null && (
            <span className="mt-scatter__brush" style={{ left: pct(Math.min(drag.x0, drag.x1)), width: pct(Math.abs(drag.x1 - drag.x0)) }} />
          )}
        </div>
      </div>
      <div className="mt-chart__axis mt-scatter__xaxis" aria-hidden="true">
        {[0, 0.25, 0.5, 0.75, 1].map((f) => (
          <span key={f}>{formatClock(fromMs + f * span, timeZone, span > 24 * 3600_000)}</span>
        ))}
      </div>
    </figure>
  );
}
