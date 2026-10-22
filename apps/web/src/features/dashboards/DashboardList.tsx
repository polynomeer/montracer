// S10 dashboard 목록 (D05 §10, ADR 0058). 목록은 요약(제목·위젯 수·수정)만 받는다.
import { useState } from 'react';
import { Link, useOutletContext, useParams, useSearchParams } from 'react-router';
import { listDashboards } from '../../api/dashboards.ts';
import { useRemote } from '../../api/useRemote.ts';
import { contextOnly, type InvestigationContext } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { formatDateTime } from '../services/format.ts';
import { EmptyState, ErrorNotice, Skeleton } from '../services/states.tsx';
import '../services/services.css';
import '../monitors/monitors.css';
import './dashboards.css';

export function DashboardList() {
  const { org = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const [params] = useSearchParams();
  const search = contextOnly(params);
  const [tick, setTick] = useState(0);
  const remote = useRemote(`${org}|dashboards`, tick, (signal) => listDashboards(signal));
  const status = remote.error?.status;
  return (
    <section className="mt-page" aria-labelledby="dashboards-title">
      <header className="mt-page__header">
        <h1 id="dashboards-title" className="mt-title">
          Dashboards
        </h1>
        <div className="mt-page__action mt-dash-actions">
          <button type="button" className="mt-button" onClick={() => setTick((n) => n + 1)}>
            새로고침
          </button>
          <Link className="mt-button mt-button--primary" to={{ pathname: orgPath(org, 'dashboards/new'), search }}>
            새 dashboard
          </Link>
        </div>
      </header>
      {status === 404 ? (
        <EmptyState reason="disabled" title="이 배포에서는 dashboard를 쓸 수 없습니다">
          dashboard 기능이 꺼져 있습니다(ADR 0057 Rollback).
        </EmptyState>
      ) : status === 403 ? (
        <EmptyState reason="forbidden" title="dashboard를 볼 권한이 없습니다">
          Viewer 이상의 역할이 필요합니다.
        </EmptyState>
      ) : (
        <>
          {remote.error !== null && (
            <>
              <ErrorNotice error={remote.error} lastSuccessMs={remote.lastSuccessMs} timeZone={ctx.timeZone} onRetry={() => setTick((n) => n + 1)} />
              {remote.data === null && <p className="mt-label">dashboard 목록을 불러오지 못했습니다. dashboard가 없다는 뜻이 아닙니다.</p>}
            </>
          )}
          {remote.data === null ? (
            remote.error === null && <Skeleton height={200} label="dashboard 목록을 불러오는 중" />
          ) : remote.data.length === 0 ? (
            <EmptyState reason="new" title="아직 dashboard가 없습니다" action={<Link className="mt-button mt-button--primary" to={{ pathname: orgPath(org, 'dashboards/new'), search }}>첫 dashboard 만들기</Link>}>
              자주 보는 metric을 위젯으로 모아 두면 조직이 같은 화면을 봅니다.
            </EmptyState>
          ) : (
            <section className="mt-card mt-card--flush" aria-label="dashboard 목록">
              {remote.nextCursor !== null && (
                <p className="mt-label mt-monitor-note" role="note">
                  처음 200개만 보입니다(제목순).
                </p>
              )}
              <div className="mt-scroll-x">
                <table className="mt-table">
                  <caption className="mt-visually-hidden">dashboard 목록(제목순)</caption>
                  <thead>
                    <tr>
                      <th scope="col">제목</th>
                      <th scope="col" className="mt-num">
                        위젯
                      </th>
                      <th scope="col">수정</th>
                    </tr>
                  </thead>
                  <tbody>
                    {remote.data.map((d) => (
                      <tr key={d.id}>
                        <th scope="row" className="mt-monitor-name">
                          <Link to={{ pathname: orgPath(org, `dashboards/${d.id}`), search }}>{d.title}</Link>
                        </th>
                        <td className="mt-num">{d.widget_count}</td>
                        <td className="mt-nowrap">
                          {formatDateTime(d.updated_at, ctx.timeZone)} <span className="mt-muted">{d.updated_by}</span>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </section>
          )}
        </>
      )}
    </section>
  );
}
