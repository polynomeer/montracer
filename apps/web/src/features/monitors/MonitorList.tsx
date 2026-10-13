// S09 Monitor 목록 (D05 §09, F06, ADR 0054).
// 상태는 가장 나쁜 group 상태다. 평가 전·꺼짐·데이터 없음·평가 오류를 정상과 구분하고, 평가가 멈췄으면 "갱신 멈춤"을 붙이고
// 정상 색을 쓰지 않는다(계약 6). 상태 권한이 없으면(environment 제한 key 등) "상태를 볼 수 없음"이다 — 평가 전이 아니다.
import { useMemo, useState } from 'react';
import { Link, useOutletContext, useParams, useSearchParams } from 'react-router';
import { listMonitors, type MonitorItem } from '../../api/monitors.ts';
import { useRemote } from '../../api/useRemote.ts';
import { contextOnly, type InvestigationContext } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { formatDateTime } from '../services/format.ts';
import { ErrorNotice, Skeleton, StatusBadge } from '../services/states.tsx';
import '../services/services.css';
import { STATE_LABEL, describeCondition, reasonText, stateRank, stateTone, viewState, type ViewState } from './monitorSpec.ts';
import './monitors.css';

export function StateBadge({ state, stale = false }: { state: ViewState; stale?: boolean }) {
  return (
    <span className="mt-monitor-state">
      <StatusBadge tone={stateTone(state, stale)}>{STATE_LABEL[state]}</StatusBadge>
      {stale && (
        <span className="mt-monitor-stale" title="마지막 평가가 오래됐습니다. 평가가 멈췄을 수 있습니다.">
          갱신 멈춤
        </span>
      )}
    </span>
  );
}

type Filter = ViewState | 'STALE' | 'all';
const FILTERS: Filter[] = ['all', 'ALERT', 'EVALUATION_ERROR', 'NO_DATA', 'STALE', 'RECOVERING', 'PENDING', 'NOT_EVALUATED', 'UNAVAILABLE', 'OK', 'DISABLED'];
const FILTER_LABEL = (f: Filter) => (f === 'all' ? '전체' : f === 'STALE' ? '갱신 멈춤' : STATE_LABEL[f]);

const isStale = (m: MonitorItem) => m.status?.stale ?? false;
// "정상"은 지금 정상인 것만: 갱신이 멈춘 정상은 "갱신 멈춤"으로만 걸린다
const matches = (m: MonitorItem, f: Filter) =>
  f === 'all' || (f === 'STALE' ? isStale(m) : viewState(m) === f && !(f === 'OK' && isStale(m)));

export function MonitorList() {
  const { org = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const [params, setParams] = useSearchParams();
  const [refreshTick, setRefreshTick] = useState(0);
  const remote = useRemote(`${org}|monitors`, refreshTick, (signal) => listMonitors(null, signal));
  const raw = params.get('state');
  const filter: Filter = raw !== null && (FILTERS as string[]).includes(raw) ? (raw as Filter) : 'all';
  const search = contextOnly(params);
  const rows = useMemo(() => {
    const all = [...(remote.data ?? [])];
    all.sort((a, b) => stateRank(viewState(a), isStale(a)) - stateRank(viewState(b), isStale(b)) || a.name.localeCompare(b.name, 'ko'));
    return all.filter((m) => matches(m, filter));
  }, [remote.data, filter]);
  const counts = useMemo(() => {
    const c: Partial<Record<Filter, number>> = {};
    for (const f of FILTERS) c[f] = (remote.data ?? []).filter((m) => matches(m, f)).length;
    return c;
  }, [remote.data]);
  const forbidden = remote.meta?.warnings.includes('status_forbidden') ?? false;

  const setFilter = (v: string) => {
    const next = new URLSearchParams(params);
    if (v === 'all') next.delete('state');
    else next.set('state', v);
    setParams(next, { replace: true });
  };

  return (
    <section className="mt-page" aria-labelledby="monitors-title">
      <header className="mt-page__header">
        <h1 id="monitors-title" className="mt-title">
          Monitors
        </h1>
        <span className="mt-label">
          {(remote.data ?? []).length.toLocaleString('ko-KR')}개
          {(counts.ALERT ?? 0) > 0 && ` · 경보 ${String(counts.ALERT)}`}
        </span>
        <div className="mt-page__action mt-monitor-actions">
          <button type="button" className="mt-button" onClick={() => setRefreshTick((n) => n + 1)}>
            새로고침
          </button>
          <Link className="mt-button mt-button--primary" to={{ pathname: orgPath(org, 'monitors/new'), search }}>
            새 monitor
          </Link>
        </div>
      </header>
      <div className="mt-monitor-filter">
        <label className="mt-label" htmlFor="monitor-state-filter">
          상태
        </label>
        <select id="monitor-state-filter" className="mt-select" value={filter} onChange={(e) => setFilter(e.target.value)}>
          {FILTERS.map((f) => (
            <option key={f} value={f}>
              {f === 'all' ? FILTER_LABEL(f) : `${FILTER_LABEL(f)} (${String(counts[f] ?? 0)})`}
            </option>
          ))}
        </select>
      </div>
      {forbidden && (
        <p className="mt-notice mt-notice--info" role="note">
          이 권한으로는 monitor 상태를 볼 수 없습니다. 상태는 조직 전체 telemetry를 평가한 결과라 environment가 제한된 key나 telemetry 조회 권한이 없으면 정의만 보입니다.
        </p>
      )}
      {remote.error !== null && (
        <>
          <ErrorNotice error={remote.error} lastSuccessMs={remote.lastSuccessMs} timeZone={ctx.timeZone} onRetry={() => setRefreshTick((n) => n + 1)} />
          {remote.data === null && <p className="mt-label">monitor 목록을 불러오지 못했습니다. monitor가 없다는 뜻이 아닙니다.</p>}
        </>
      )}
      {remote.data === null ? (
        remote.error === null && <Skeleton height={240} label="monitor 목록을 불러오는 중" />
      ) : remote.data.length === 0 ? (
        <div className="mt-empty">
          <strong>아직 monitor가 없습니다</strong>
          <span>오류율이나 지연 같은 조건을 정하면 1분마다 평가하고, 조건을 넘으면 알림을 보냅니다.</span>
          <Link to={{ pathname: orgPath(org, 'monitors/new'), search }}>첫 monitor 만들기</Link>
        </div>
      ) : (
        <section className="mt-card mt-card--flush" aria-label="monitor 목록">
          {remote.nextCursor !== null && (
            <p className="mt-label mt-monitor-note" role="note">
              처음 200개만 보입니다.
            </p>
          )}
          {rows.length === 0 ? (
            <div className="mt-empty mt-empty--inline">
              <strong>이 상태의 monitor가 없습니다</strong>
            </div>
          ) : (
            <div className="mt-scroll-x">
              <table className="mt-table">
                <caption className="mt-visually-hidden">monitor 목록(나쁜 상태 먼저)</caption>
                <thead>
                  <tr>
                    <th scope="col">이름</th>
                    <th scope="col">상태</th>
                    <th scope="col">group</th>
                    <th scope="col">조건</th>
                    <th scope="col">마지막 평가</th>
                    <th scope="col">알림</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((m) => (
                    <tr key={m.id}>
                      <th scope="row" className="mt-monitor-name">
                        <Link to={{ pathname: orgPath(org, `monitors/${m.id}`), search }}>{m.name}</Link>
                      </th>
                      <td>
                        <StateBadge state={viewState(m)} stale={isStale(m)} />
                      </td>
                      <td>
                        <GroupCounts m={m} />
                      </td>
                      <td className="mt-monitor-condition">{describeCondition(m.spec)}</td>
                      <td className="mt-nowrap">
                        {m.status?.last_evaluation == null ? (
                          <span className="mt-muted">없음</span>
                        ) : (
                          <>
                            {formatDateTime(m.status.last_evaluation.slot, ctx.timeZone)}
                            {m.status.last_evaluation.status !== 'evaluated' && (
                              <span className="mt-muted"> · {reasonText(m.status.last_evaluation.reason) || m.status.last_evaluation.status}</span>
                            )}
                          </>
                        )}
                      </td>
                      <td className="mt-nowrap">{m.spec.notification_policy_id === null ? <span className="mt-muted">없음</span> : '연결됨'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      )}
    </section>
  );
}

function GroupCounts({ m }: { m: MonitorItem }) {
  const by = m.status?.groups_by_state ?? {};
  const total = Object.values(by).reduce((a, n) => a + (n ?? 0), 0);
  if (total === 0) return <span className="mt-muted">—</span>;
  const notable = (['ALERT', 'EVALUATION_ERROR', 'NO_DATA', 'PENDING', 'RECOVERING'] as const).filter((s) => (by[s] ?? 0) > 0);
  return (
    <span>
      {notable.map((s) => `${STATE_LABEL[s]} ${String(by[s] ?? 0)}`).join(' · ')}
      {notable.length > 0 && ' / '}
      {total.toLocaleString('ko-KR')}개
    </span>
  );
}
