// 알림 policy 목록 (ADR 0056, ADR 0053). monitor가 경보를 보낼 webhook 목적지다. 주소는 host만 보인다(전체 주소는 암호문으로만 저장).
// 조회는 monitors.read(Developer 이상), 만들기·바꾸기는 Tenant Admin이다 — 버튼은 보이되 서버가 403을 주면 그 이유를 보인다.
import { useState } from 'react';
import { Link, useOutletContext, useParams, useSearchParams } from 'react-router';
import { listPolicies } from '../../api/notificationPolicies.ts';
import { useRemote } from '../../api/useRemote.ts';
import { contextOnly, type InvestigationContext } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { formatDateTime } from '../services/format.ts';
import { EmptyState, ErrorNotice, Skeleton, StatusBadge } from '../services/states.tsx';
import '../services/services.css';
import '../monitors/monitors.css';
import './notifications.css';

export function PolicyList() {
  const { org = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const [params] = useSearchParams();
  const search = contextOnly(params);
  const [refreshTick, setRefreshTick] = useState(0);
  const remote = useRemote(`${org}|policies`, refreshTick, (signal) => listPolicies(signal));
  const status = remote.error?.status;

  return (
    <section className="mt-page" aria-labelledby="policies-title">
      <header className="mt-page__header">
        <h1 id="policies-title" className="mt-title">
          Notifications
        </h1>
        <span className="mt-label">경보를 받을 webhook</span>
        <div className="mt-page__action mt-monitor-actions">
          <button type="button" className="mt-button" onClick={() => setRefreshTick((n) => n + 1)}>
            새로고침
          </button>
          <Link className="mt-button mt-button--primary" to={{ pathname: orgPath(org, 'notifications/new'), search }}>
            새 policy
          </Link>
        </div>
      </header>
      {status === 404 ? (
        <EmptyState reason="disabled" title="이 배포에서는 알림을 쓸 수 없습니다">
          알림 암호화 key가 설정되지 않았습니다. 운영팀에 문의하세요(ADR 0053 Rollback).
        </EmptyState>
      ) : status === 403 ? (
        <EmptyState reason="forbidden" title="알림 policy를 볼 권한이 없습니다">
          monitor 조회 권한(Developer 이상)이 필요합니다.
        </EmptyState>
      ) : (
        <>
          {remote.error !== null && (
            <>
              <ErrorNotice error={remote.error} lastSuccessMs={remote.lastSuccessMs} timeZone={ctx.timeZone} onRetry={() => setRefreshTick((n) => n + 1)} />
              {remote.data === null && <p className="mt-label">policy 목록을 불러오지 못했습니다. policy가 없다는 뜻이 아닙니다.</p>}
            </>
          )}
          {remote.data === null ? (
            remote.error === null && <Skeleton height={200} label="policy 목록을 불러오는 중" />
          ) : remote.data.length === 0 ? (
            <EmptyState reason="new" title="아직 알림 policy가 없습니다" action={<Link className="mt-button mt-button--primary" to={{ pathname: orgPath(org, 'notifications/new'), search }}>첫 policy 만들기</Link>}>
              webhook 주소를 등록하면 monitor가 경보를 열고 닫을 때 서명된 요청을 보냅니다. 만들기는 Tenant Admin만 할 수 있습니다.
            </EmptyState>
          ) : (
            <section className="mt-card mt-card--flush" aria-label="알림 policy 목록">
              {remote.nextCursor !== null && (
                <p className="mt-label mt-monitor-note" role="note">
                  처음 200개만 보입니다(이름순).
                </p>
              )}
              <div className="mt-scroll-x">
                <table className="mt-table">
                  <caption className="mt-visually-hidden">알림 policy 목록(이름순)</caption>
                  <thead>
                    <tr>
                      <th scope="col">이름</th>
                      <th scope="col">상태</th>
                      <th scope="col">목적지</th>
                      <th scope="col">서명 key</th>
                      <th scope="col">수정</th>
                    </tr>
                  </thead>
                  <tbody>
                    {remote.data.map((p) => (
                      <tr key={p.id}>
                        <th scope="row" className="mt-monitor-name">
                          <Link to={{ pathname: orgPath(org, `notifications/${p.id}`), search }}>{p.name}</Link>
                        </th>
                        <td>
                          <StatusBadge tone={p.enabled ? 'success' : 'neutral'}>{p.enabled ? '켜짐' : '꺼짐'}</StatusBadge>
                        </td>
                        <td className="mt-mono">{p.webhook.url}</td>
                        <td className="mt-mono mt-nowrap">
                          {p.webhook.signing_key_id}
                          {p.webhook.previous_signing_key_id != null && <span className="mt-muted"> · 교체 중</span>}
                        </td>
                        <td className="mt-nowrap">{p.updated_at === undefined ? '—' : formatDateTime(p.updated_at, ctx.timeZone)}</td>
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
