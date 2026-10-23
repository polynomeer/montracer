// 24시간 dry-run 미리보기 (D05 §09, ADR 0052). 후보 발화 구간과 데이터 coverage를 보인다.
// 미래 알림 횟수의 예측이라고 하지 않는다. 데이터가 없던 것을 "발화 없음"으로 보이지 않는다(계약 6).
import type { DryRun, DryRunGroup, MonitorKind } from '../../api/monitors.ts';
import { formatDateTime } from '../services/format.ts';
import { StatusBadge } from '../services/states.tsx';
import { STATE_BADGE, STATE_LABEL, formatDuration, formatValue, groupLabel, reasonText } from './monitorSpec.ts';

const REASON_NOTICE: Record<string, string> = {
  no_watermark: '아직 집계된 데이터가 없어 지난 24시간을 평가할 수 없습니다.',
  stale_watermark: '집계가 멈춰 있어, 멈춘 시각까지만 평가했습니다. 지금 저장하면 데이터가 돌아올 때까지 "데이터 없음"입니다.',
  no_data: '지난 24시간 동안 이 조건에 맞는 데이터가 없습니다. 발화하지 않는다는 뜻이 아닙니다 — metric·filter를 확인하세요.',
};

/** 판정 비율. 반올림으로 100%가 아닌데 100%로, 0이 아닌데 0%로 보이지 않게 한다(데이터 없음·오류를 감추지 않는다). */
export function formatCoverage(evaluated: number, total: number): string {
  if (total <= 0) return '-';
  const pct = (evaluated / total) * 100;
  const shown = evaluated === total ? 100 : evaluated === 0 ? 0 : Math.min(99.9, Math.max(0.1, Math.round(pct * 10) / 10));
  return `${shown.toLocaleString('ko-KR')}%`;
}

export function DryRunPreview({ dr, kind, timeZone }: { dr: DryRun; kind: MonitorKind; timeZone: string }) {
  const firing = dr.groups.filter((g) => g.firing_intervals.length > 0 || g.firing_intervals_omitted > 0);
  const intervals = dr.groups.reduce((a, g) => a + g.firing_intervals.length + g.firing_intervals_omitted, 0);
  return (
    <section className="mt-monitor-dryrun" aria-labelledby="dryrun-title">
      <h3 id="dryrun-title" className="mt-card__title">
        지난 24시간 미리보기
      </h3>
      <p className="mt-label">
        지금 정의로 지난 24시간을 다시 평가한 후보 발화 구간입니다. 앞으로의 알림 횟수를 예측하지 않습니다.
        {dr.warmup_seconds > 0 && ` 처음 ${formatDuration(dr.warmup_seconds)}은 상태를 만들기 위해 앞당겨 평가했습니다.`}
      </p>
      {dr.evaluations > 0 && (
        <p className="mt-monitor-dryrun__summary">
          <span>
            후보 발화 구간 <strong>{intervals.toLocaleString('ko-KR')}개</strong>
          </span>
          <span>
            값으로 판정한 비율(coverage) <strong>{formatCoverage(dr.coverage.evaluated, dr.evaluations)}</strong>
          </span>
        </p>
      )}
      {dr.reason !== '' && REASON_NOTICE[dr.reason] !== undefined && (
        <div className="mt-notice mt-notice--warning" role="status">
          {REASON_NOTICE[dr.reason]}
        </div>
      )}
      {dr.evaluations > 0 && (
        <p>
          <strong>{dr.evaluations.toLocaleString('ko-KR')}회</strong> 평가({formatDuration(dr.step_seconds)} 간격): 값으로 판정{' '}
          {dr.coverage.evaluated.toLocaleString('ko-KR')} · 데이터 없음 {dr.coverage.no_data.toLocaleString('ko-KR')} · 오류 {dr.coverage.error.toLocaleString('ko-KR')}
          {' — '}
          {intervals === 0 ? (
            '후보 발화 없음'
          ) : (
            <strong>
              후보 발화 {intervals.toLocaleString('ko-KR')}번 · group {firing.length.toLocaleString('ko-KR')}개
            </strong>
          )}
        </p>
      )}
      {dr.groups.length > 0 && dr.from !== null && dr.to !== null && (
        <div className="mt-scroll-x">
          <table className="mt-table">
            <caption className="mt-visually-hidden">group별 후보 발화 구간</caption>
            <thead>
              <tr>
                <th scope="col">group</th>
                <th scope="col">발화 구간</th>
                <th scope="col" className="mt-num">
                  발화 시간
                </th>
                <th scope="col" className="mt-num">
                  최대값
                </th>
                <th scope="col">데이터</th>
                <th scope="col">끝 상태</th>
              </tr>
            </thead>
            <tbody>
              {dr.groups.slice(0, 50).map((g) => (
                <GroupRow key={g.group_key} g={g} from={dr.from ?? ''} to={dr.to ?? ''} kind={kind} timeZone={timeZone} />
              ))}
            </tbody>
          </table>
          {(dr.groups_omitted > 0 || dr.groups.length > 50) && (
            <p className="mt-label">
              발화 시간이 긴 순서로 {Math.min(dr.groups.length, 50)}개 group만 보입니다(전체 {dr.groups_total.toLocaleString('ko-KR')}개).
            </p>
          )}
        </div>
      )}
    </section>
  );
}

function GroupRow({ g, from, to, kind, timeZone }: { g: DryRunGroup; from: string; to: string; kind: MonitorKind; timeZone: string }) {
  const f = Date.parse(from);
  const span = Math.max(Date.parse(to) - f, 1);
  const pos = (iso: string) => Math.min(Math.max(((Date.parse(iso) - f) / span) * 1000, 0), 1000);
  const n = g.firing_intervals.length + g.firing_intervals_omitted;
  const label =
    n === 0
      ? '발화 없음'
      : `${String(n)}번 발화: ` +
        g.firing_intervals
          .slice(0, 5)
          .map((iv) => `${formatDateTime(iv.start, timeZone)}부터 ${iv.end === null ? '끝까지' : formatDateTime(iv.end, timeZone)}까지`)
          .join(', ');
  return (
    <tr>
      <th scope="row" className="mt-mono mt-monitor-group">
        {groupLabel(g.labels)}
      </th>
      <td className="mt-monitor-bar-cell">
        <svg viewBox="0 0 1000 16" preserveAspectRatio="none" role="img" aria-label={label} className="mt-monitor-bar">
          <rect x="0" y="0" width="1000" height="16" className="mt-monitor-strip__bg" />
          {g.firing_intervals.map((iv) => {
            // 아주 짧은 구간·끝까지 열린 구간도 보이게 최소 폭을 두고, 막대 안에 넣는다
            const w = Math.max(pos(iv.end ?? to) - pos(iv.start), 4);
            const x = Math.min(pos(iv.start), 1000 - w);
            return <rect key={iv.start} x={x} y="2" width={w} height="12" className={iv.reason === 'no_data' ? 'mt-monitor-strip__nodata' : 'mt-monitor-strip__alert'} />;
          })}
        </svg>
      </td>
      <td className="mt-num">{g.firing_seconds === 0 ? '—' : formatDuration(g.firing_seconds)}</td>
      <td className="mt-num">{g.max_value === null ? <span className="mt-muted">값 없음</span> : formatValue(kind, g.max_value)}</td>
      <td>
        {g.no_data > 0 ? <span className="mt-muted">데이터 없음 {g.no_data.toLocaleString('ko-KR')}회</span> : '모두 있음'}
        {g.partial > 0 && <span className="mt-muted"> · 일부 집계 {g.partial.toLocaleString('ko-KR')}회</span>}
        {g.evaluated === 0 && g.last_reason !== '' && <span className="mt-muted"> · {reasonText(g.last_reason)}</span>}
      </td>
      <td>
        <StatusBadge {...STATE_BADGE[g.final_state]}>{STATE_LABEL[g.final_state]}</StatusBadge>
      </td>
    </tr>
  );
}
