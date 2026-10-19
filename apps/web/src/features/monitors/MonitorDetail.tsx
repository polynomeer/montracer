// S09 Monitor 상세 (D05 §09 "상태와 timeline", F06, ADR 0054).
// - group별 지금 상태와 값(없으면 이유), 평가·상태 전이·알림 시도를 서로 다른 모양으로 보인다.
// - 조사 context 범위(최대 7일, 평가 기록 보존)의 timeline을 같은 절대시간으로 연다(경보 deep link 공유).
import { useEffect, useMemo, useRef, useState } from 'react';
import { Link, useNavigate, useOutletContext, useParams, useSearchParams } from 'react-router';
import {
  deleteMonitor,
  getMonitor,
  listPolicies,
  monitorGroups,
  monitorTimeline,
  type EvaluationItem,
  type GroupItem,
  type MonitorItem,
  type NotificationItem,
  type Timeline,
  type TransitionItem,
} from '../../api/monitors.ts';
import { useRemote, type RemoteState } from '../../api/useRemote.ts';
import { contextOnly, formatRange, resolveRange, type InvestigationContext } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { formatDateTime } from '../services/format.ts';
import { ErrorNotice, Skeleton, StatusBadge } from '../services/states.tsx';
import '../services/services.css';
import { StateBadge } from './MonitorList.tsx';
import { STATE_LABEL, STATE_TONE, describeCondition, describeFilter, describeNoData, formatDuration, formatValue, groupLabel, isGroupState, reasonText, viewState } from './monitorSpec.ts';
import './monitors.css';
import { metaTruncated } from '../../api/types.ts';

const MAX_TIMELINE_MS = 7 * 24 * 3600 * 1000;
const MAX_EVENTS = 200;

// 상태 권한이 없으면(environment 제한 key, telemetry.read 없음) groups·timeline은 403이다. 오류가 아니라 안내로 보인다.
function StateForbidden() {
  return (
    <p className="mt-notice mt-notice--info" role="note">
      이 권한으로는 상태를 볼 수 없습니다. 상태는 조직 전체 telemetry를 평가한 결과입니다.
    </p>
  );
}

export function MonitorDetail() {
  const { org = '', monitorId = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const [refreshTick, setRefreshTick] = useState(0);
  const rangeKey = JSON.stringify(ctx.range);
  const nowMs = useMemo(() => Date.now(), [rangeKey, refreshTick]);
  const { fromMs, toMs } = resolveRange(ctx.range, nowMs);
  const tooLong = toMs - fromMs > MAX_TIMELINE_MS;
  const from = new Date(tooLong ? toMs - MAX_TIMELINE_MS : fromMs).toISOString();
  const to = new Date(toMs).toISOString();
  const search = contextOnly(params);

  const monitor = useRemote(`${org}|monitor|${monitorId}`, refreshTick, (signal) => getMonitor(monitorId, signal));
  // 정의 응답이 상태를 볼 수 없다고 하면(status null) groups·timeline을 부르지 않는다
  const stateHidden = monitor.data !== null && monitor.data.status === null;
  const groups = useRemote(stateHidden ? null : `${org}|monitor-groups|${monitorId}`, refreshTick, (signal) => monitorGroups(monitorId, signal));
  const timeline = useRemote(stateHidden ? null : `${org}|monitor-timeline|${monitorId}|${from}|${to}`, refreshTick, (signal) =>
    monitorTimeline(monitorId, from, to, signal),
  );
  const policies = useRemote(`${org}|policies`, 0, (signal) => listPolicies(signal));
  const [deleting, setDeleting] = useState<'idle' | 'confirm' | 'busy'>('idle');
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const deleteTrigger = useRef<HTMLButtonElement>(null);
  const deleteConfirm = useRef<HTMLButtonElement>(null);
  const dialogOpen = deleting !== 'idle';
  // 확인 대화가 열리면 확인 버튼으로, 닫히면 삭제 버튼으로 focus를 옮긴다(키보드 사용자가 위치를 잃지 않게)
  const wasOpen = useRef(false);
  useEffect(() => {
    if (dialogOpen) deleteConfirm.current?.focus();
    else if (wasOpen.current) deleteTrigger.current?.focus();
    wasOpen.current = dialogOpen;
  }, [dialogOpen]);

  if (monitor.error?.status === 404) {
    return (
      <section className="mt-placeholder">
        <h1 className="mt-title">monitor를 찾을 수 없습니다</h1>
        <p>삭제됐거나, 볼 수 있는 권한이 없거나, 이 환경에서 monitor 기능이 꺼져 있습니다.</p>
        <Link to={{ pathname: orgPath(org, 'monitors'), search }}>monitor 목록</Link>
      </section>
    );
  }
  const m = monitor.data;

  const onDelete = async () => {
    if (m === null) return;
    setDeleting('busy');
    setDeleteError(null);
    const r = await deleteMonitor(m.id, m.revision, new AbortController().signal);
    if (r.ok) {
      void navigate({ pathname: orgPath(org, 'monitors'), search });
      return;
    }
    setDeleting('confirm');
    setDeleteError(r.status === 412 ? '다른 변경이 먼저 저장되었습니다. 새로고침한 뒤 다시 시도하세요.' : r.error.message);
  };

  const policyName =
    m?.spec.notification_policy_id == null
      ? null
      : (policies.data?.find((p) => p.id === m.spec.notification_policy_id)?.name ?? `policy ${m.spec.notification_policy_id.slice(0, 8)}`);

  return (
    <section className="mt-page" aria-labelledby="monitor-title">
      <nav className="mt-breadcrumb" aria-label="위치">
        <Link to={{ pathname: orgPath(org, 'monitors'), search }}>Monitors</Link>
      </nav>
      <header className="mt-page__header">
        <h1 id="monitor-title" className="mt-title">
          {m?.name ?? '…'}
        </h1>
        {m !== null && <StateBadge state={viewState(m)} stale={m.status?.stale ?? false} />}
        <div className="mt-page__action mt-monitor-actions">
          <button type="button" className="mt-button" onClick={() => setRefreshTick((n) => n + 1)}>
            새로고침
          </button>
          {m !== null && (
            <>
              <Link className="mt-button" to={{ pathname: orgPath(org, `monitors/${m.id}/edit`), search }}>
                편집
              </Link>
              <button ref={deleteTrigger} type="button" className="mt-button" onClick={() => setDeleting('confirm')} disabled={deleting === 'busy'}>
                삭제
              </button>
            </>
          )}
        </div>
      </header>
      {deleting !== 'idle' && m !== null && (
        <div
          className="mt-notice mt-notice--warning"
          role="alertdialog"
          aria-labelledby="delete-title"
          onKeyDown={(e) => {
            if (e.key === 'Escape' && deleting !== 'busy') setDeleting('idle');
          }}
        >
          <strong id="delete-title">“{m.name}”를 삭제할까요?</strong>
          <span>평가가 멈추고, 열린 경보는 닫힘 알림을 보낸 뒤 끝납니다. 평가 기록은 보존 기간 동안 남습니다.</span>
          {deleteError !== null && <span role="alert">{deleteError}</span>}
          <div className="mt-monitor-actions">
            <button ref={deleteConfirm} type="button" className="mt-button mt-button--danger" onClick={() => void onDelete()} disabled={deleting === 'busy'}>
              삭제
            </button>
            <button type="button" className="mt-button" onClick={() => setDeleting('idle')}>
              취소
            </button>
          </div>
        </div>
      )}
      {monitor.error !== null && <ErrorNotice error={monitor.error} lastSuccessMs={monitor.lastSuccessMs} timeZone={ctx.timeZone} />}
      {m === null ? (
        monitor.error === null && <Skeleton height={160} label="monitor를 불러오는 중" />
      ) : (
        <>
          <Summary m={m} policyName={policyName} timeZone={ctx.timeZone} />
          {stateHidden ? null : <Groups m={m} remote={groups} timeZone={ctx.timeZone} />}
        </>
      )}
      <section className="mt-card" aria-labelledby="timeline-title">
        <div className="mt-card__head">
          <h2 id="timeline-title" className="mt-card__title">
            Timeline
          </h2>
          <span className="mt-label">{formatRange(ctx.range, nowMs, ctx.timeZone)}</span>
        </div>
        {tooLong && (
          <p className="mt-label" role="note">
            평가 기록은 7일 동안 보존되어 마지막 7일만 보입니다.
          </p>
        )}
        {(stateHidden || timeline.error?.status === 403) && <StateForbidden />}
        {timeline.error !== null && timeline.error.status !== 404 && timeline.error.status !== 403 && (
          <ErrorNotice error={timeline.error} lastSuccessMs={timeline.lastSuccessMs} timeZone={ctx.timeZone} />
        )}
        {stateHidden ? null : timeline.data === null ? (
          timeline.error === null && <Skeleton height={120} label="timeline을 불러오는 중" />
        ) : (
          <TimelineView tl={timeline.data} kind={m?.spec.query.kind ?? 'metric'} timeZone={ctx.timeZone} />
        )}
      </section>
    </section>
  );
}

function Summary({ m, policyName, timeZone }: { m: MonitorItem; policyName: string | null; timeZone: string }) {
  const s = m.spec;
  const last = m.status?.last_evaluation ?? null;
  return (
    <section className="mt-card" aria-label="정의">
      <dl className="mt-monitor-def">
        <dt>조건</dt>
        <dd>{describeCondition(s)}</dd>
        <dt>데이터 없음</dt>
        <dd>{describeNoData(s)}</dd>
        <dt>평가</dt>
        <dd>
          {formatDuration(s.evaluation_seconds)}마다 · 복구는 {s.recovery_evaluations}회 연속 정상
          {!m.enabled && ' · 꺼짐'}
        </dd>
        <dt>범위</dt>
        <dd className="mt-mono">{s.query.filter === null ? <span className="mt-muted">전체</span> : describeFilter(s.query.filter)}</dd>
        <dt>group</dt>
        <dd>{s.query.group_by.length === 0 ? '없음(전체 하나)' : s.query.group_by.join(', ')}</dd>
        <dt>알림</dt>
        <dd>{policyName ?? <span className="mt-muted">없음</span>}</dd>
        <dt>마지막 평가</dt>
        <dd>
          {last === null ? (
            <span className="mt-muted">아직 평가 전</span>
          ) : (
            <>
              {formatDateTime(last.slot, timeZone)} · {last.status === 'evaluated' ? `group ${String(last.groups)}개, 경보 ${String(last.alerting)}` : reasonText(last.reason) || last.status}
              {m.status?.stale === true && <span className="mt-monitor-stale"> · 평가가 멈춘 것 같습니다</span>}
            </>
          )}
        </dd>
        <dt>revision</dt>
        <dd>
          {m.revision} · {formatDateTime(m.updated_at, timeZone)} {m.updated_by}
        </dd>
      </dl>
    </section>
  );
}

function Groups({ m, remote, timeZone }: { m: MonitorItem; remote: RemoteState<GroupItem[]>; timeZone: string }) {
  const truncated = metaTruncated(remote.meta);
  return (
    <section className="mt-card mt-card--flush" aria-labelledby="groups-title">
      <div className="mt-card__head">
        <h2 id="groups-title" className="mt-card__title">
          Group 상태
        </h2>
        <span className="mt-label">나쁜 상태 먼저{truncated && ` · 처음 ${String(remote.data?.length ?? 0)}개만`}</span>
      </div>
      {remote.error?.status === 403 && <StateForbidden />}
      {remote.error !== null && remote.error.status !== 404 && remote.error.status !== 403 && <ErrorNotice error={remote.error} lastSuccessMs={remote.lastSuccessMs} timeZone={timeZone} />}
      {remote.data === null ? (
        remote.error === null && <Skeleton height={120} label="group 상태를 불러오는 중" />
      ) : remote.data.length === 0 ? (
        <div className="mt-empty mt-empty--inline">
          <strong>{m.status?.last_evaluation == null ? '아직 평가 전입니다' : '평가된 group이 없습니다'}</strong>
          <span>{m.status?.last_evaluation == null ? '저장 뒤 첫 평가 주기가 지나면 나타납니다.' : '조건에 맞는 series가 없습니다. 정상이라는 뜻이 아닙니다.'}</span>
        </div>
      ) : (
        <div className="mt-scroll-x">
          <table className="mt-table">
            <caption className="mt-visually-hidden">group별 지금 상태</caption>
            <thead>
              <tr>
                <th scope="col">group</th>
                <th scope="col">상태</th>
                <th scope="col" className="mt-num">
                  마지막 값
                </th>
                <th scope="col">시작</th>
                <th scope="col">마지막 window</th>
              </tr>
            </thead>
            <tbody>
              {remote.data.map((g) => (
                <tr key={g.group_hash}>
                  <th scope="row" className="mt-mono mt-monitor-group">
                    {groupLabel(g.labels)}
                  </th>
                  <td>
                    <StatusBadge tone={STATE_TONE[g.state]}>{STATE_LABEL[g.state]}</StatusBadge>
                  </td>
                  <td className="mt-num">
                    {g.last_value === null ? <span className="mt-muted">{reasonText(g.last_reason) || '값 없음'}</span> : formatValue(m.spec.query.kind, g.last_value)}
                  </td>
                  <td>
                    {g.violation_since !== null
                      ? `위반 ${formatDateTime(g.violation_since, timeZone)}`
                      : g.nodata_since !== null
                        ? `데이터 없음 ${formatDateTime(g.nodata_since, timeZone)}`
                        : <span className="mt-muted">—</span>}
                  </td>
                  <td>{g.last_window_end === null ? <span className="mt-muted">—</span> : formatDateTime(g.last_window_end, timeZone)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

type EventRow =
  | { kind: 'transition'; at: string; t: TransitionItem }
  | { kind: 'notification'; at: string; n: NotificationItem };

const NOTIFY_STATUS: Record<NotificationItem['status'], string> = { pending: '보내는 중', delivered: '전달됨', failed: '전달 실패(포기)', canceled: '취소됨' };

function TimelineView({ tl, kind, timeZone }: { tl: Timeline; kind: 'error_ratio' | 'metric'; timeZone: string }) {
  const events: EventRow[] = [
    ...tl.transitions.map((t): EventRow => ({ kind: 'transition', at: t.at, t })),
    ...tl.notifications.map((n): EventRow => ({ kind: 'notification', at: n.created_at, n })),
  ].sort((a, b) => b.at.localeCompare(a.at));
  return (
    <div className="mt-monitor-timeline">
      <EvaluationStrip evaluations={tl.evaluations} from={tl.from} to={tl.to} truncated={tl.evaluations_truncated} />
      {(tl.evaluations_truncated || tl.transitions_truncated || tl.notifications_truncated) && (
        <p className="mt-label" role="note">
          기간이 길어 가장 최근 2,000개만 보입니다. 범위를 줄이면 모두 볼 수 있습니다.
        </p>
      )}
      {events.length === 0 ? (
        <p className="mt-muted">이 기간에 상태 전이·알림이 없습니다.</p>
      ) : (
        <ol className="mt-monitor-events" aria-label="상태 전이와 알림">
          {events.slice(0, MAX_EVENTS).map((e) =>
            e.kind === 'transition' ? (
              <li key={`t-${e.at}-${e.t.group_hash}-${e.t.from}`} className="mt-monitor-event">
                <span className="mt-monitor-event__icon" aria-hidden="true">
                  {e.t.opened ? '▲' : e.t.closed ? '▼' : '◆'}
                </span>
                <time dateTime={e.at}>{formatDateTime(e.at, timeZone)}</time>
                <span>
                  {e.t.opened ? '경보 열림' : e.t.closed ? '경보 닫힘' : '상태 변화'}: {stateText(e.t.from)} → {e.t.to === null ? '끝남' : stateText(e.t.to)}
                  {e.t.reason !== null && ` (${reasonText(e.t.reason)})`}
                  {e.t.value !== null && ` · 값 ${formatValue(kind, e.t.value)}`}
                </span>
                <span className="mt-mono mt-muted">{groupLabel(e.t.labels)}</span>
              </li>
            ) : (
              <li key={`n-${e.n.id}`} className="mt-monitor-event">
                <span className="mt-monitor-event__icon" aria-hidden="true">
                  ✉
                </span>
                <time dateTime={e.at}>{formatDateTime(e.at, timeZone)}</time>
                <span>
                  알림 {notifyKind(e.n)}: {NOTIFY_STATUS[e.n.status]} · 시도 {e.n.attempts}회
                  {e.n.last_status_code !== null && ` · 응답 ${String(e.n.last_status_code)}`}
                  {e.n.last_error !== null && e.n.status !== 'delivered' && ` · ${e.n.last_error}`}
                </span>
              </li>
            ),
          )}
        </ol>
      )}
      {events.length > MAX_EVENTS && (
        <p className="mt-label" role="note">
          최근 {MAX_EVENTS}개만 보입니다(전체 {events.length.toLocaleString('ko-KR')}개). 범위를 줄이면 앞의 기록을 볼 수 있습니다.
        </p>
      )}
    </div>
  );
}

// 모르는 종류는 원문 그대로 보인다(닫힘으로 짐작하지 않는다 — 열린 사건이 닫힌 것처럼 보이지 않게)
function notifyKind(n: NotificationItem): string {
  switch (n.event_type) {
    case 'alert.opened':
      return '(열림)';
    case 'alert.resolved':
      return '(닫힘)';
    case 'alert.renotified':
      return `(재알림 ${String(n.renotify_sequence ?? '?')}/3)`;
    default:
      return `(${String(n.event_type)})`;
  }
}

function stateText(s: string): string {
  return isGroupState(s) ? STATE_LABEL[s] : s;
}

// 평가마다 한 칸. 색과 함께 높이·위치로 나눈다(색만으로 구분하지 않는다, D05 §08):
// 경보 있음 = 위까지 찬 막대, 오류 = 위까지 찬 막대(경고색), 경보 없음 = 낮은 막대(중립색 — 정상이라는 초록을 쓰지 않는다),
// 데이터 없음 = 바닥의 짧은 눈금. group이 하나도 없던 평가는 값으로 판정한 것이 아니므로 데이터 없음이다(계약 6).
// 평가가 없던 구간은 비워 둔다(멈춤이 보이게). 표시 한도로 앞부분이 잘렸으면 그 구간을 흐린 바탕으로 보인다.
type Cell = 'alert' | 'quiet' | 'nodata' | 'error';
const CELL_SHAPE: Record<Cell, { y: number; h: number }> = { alert: { y: 2, h: 20 }, error: { y: 2, h: 20 }, quiet: { y: 12, h: 10 }, nodata: { y: 19, h: 3 } };

export function cellOf(e: EvaluationItem): Cell {
  if (e.status === 'error') return 'error';
  if (e.status === 'no_data' || e.groups === 0) return 'nodata';
  return e.alerting > 0 ? 'alert' : 'quiet';
}

export function EvaluationStrip({ evaluations, from, to, truncated = false }: { evaluations: EvaluationItem[]; from: string; to: string; truncated?: boolean }) {
  const f = Date.parse(from);
  const span = Math.max(Date.parse(to) - f, 1);
  const xOf = (iso: string) => Math.min(Math.max(((Date.parse(iso) - f) / span) * 1000, 0), 998);
  const counts: Record<Cell, number> = { alert: 0, quiet: 0, nodata: 0, error: 0 };
  const cells = evaluations.map((e) => {
    const cls = cellOf(e);
    counts[cls] += 1;
    return { x: xOf(e.slot), cls, key: e.slot };
  });
  const cut = truncated && evaluations.length > 0 ? xOf(evaluations[0]?.slot ?? from) : 0;
  const label =
    `평가 ${String(evaluations.length)}회: 경보 있음 ${String(counts.alert)}, 경보 없음 ${String(counts.quiet)}, 데이터 없음 ${String(counts.nodata)}, 오류 ${String(counts.error)}` +
    (cut > 0 ? ' — 앞부분은 표시 한도로 잘렸습니다' : '');
  return (
    <figure className="mt-monitor-strip">
      <svg viewBox="0 0 1000 24" preserveAspectRatio="none" role="img" aria-label={label}>
        <rect x="0" y="0" width="1000" height="24" className="mt-monitor-strip__bg" />
        {cut > 0 && <rect x="0" y="0" width={cut} height="24" className="mt-monitor-strip__cut" />}
        {cells.map((c) => (
          <rect key={c.key} x={c.x} y={CELL_SHAPE[c.cls].y} width="2" height={CELL_SHAPE[c.cls].h} className={`mt-monitor-strip__${c.cls}`} />
        ))}
      </svg>
      <figcaption className="mt-label">{label}</figcaption>
    </figure>
  );
}
