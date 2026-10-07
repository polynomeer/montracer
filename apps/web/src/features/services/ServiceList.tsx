// Services 목록 (S02 진입점, D05 §05 · ADR 0038). 서비스 이름을 고르면 서비스 상세로 간다.
import { useState } from 'react';
import { Link, useOutletContext, useParams, useSearchParams } from 'react-router';
import { listServices } from '../../api/catalog.ts';
import { useRemote } from '../../api/useRemote.ts';
import { contextOnly, type InvestigationContext } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { formatDateTime } from './format.ts';
import { ErrorNotice, Skeleton, StatusBadge } from './states.tsx';
import './services.css';

const PAGE = 100;

export function ServiceList() {
  const { org = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const [params] = useSearchParams();
  // cursor는 이 화면 상태다(URL에 넣지 않는다 — 15분 만료 서명 값).
  const [cursors, setCursors] = useState<string[]>([]);
  const [retry, setRetry] = useState(0);
  const cursor = cursors.at(-1) ?? null;
  const env = ctx.environment;
  const state = useRemote(`${org}|services|${env ?? ''}|${cursor ?? ''}`, retry, (signal) =>
    listServices({ environment: env, cursor, limit: PAGE }, signal),
  );
  const search = contextOnly(params);

  return (
    <section className="mt-page" aria-labelledby="services-title">
      <header className="mt-page__header">
        <h1 id="services-title" className="mt-title">
          Services
        </h1>
        <span className="mt-label">{env === null ? '전체 환경' : `환경 ${env}`} · 이름순</span>
      </header>
      {state.error !== null && (
        <ErrorNotice error={state.error} lastSuccessMs={state.lastSuccessMs} onRetry={() => setRetry((n) => n + 1)} />
      )}
      {state.status === 'loading' && <Skeleton height={240} label="서비스 목록을 불러오는 중" />}
      {state.data !== null && state.data.length === 0 && (
        <div className="mt-empty">
          <strong>{env === null ? '아직 수신한 서비스가 없습니다' : `환경 ${env}에 서비스가 없습니다`}</strong>
          <span>
            {env === null
              ? '계측을 설치하면 첫 데이터를 받은 뒤 5분 안에 목록에 나타납니다.'
              : '환경 선택을 바꾸거나 전체 환경을 보세요.'}
          </span>
          {env === null && <Link to={{ pathname: orgPath(org, 'setup'), search }}>설치 안내</Link>}
        </div>
      )}
      {state.data !== null && state.data.length > 0 && (
        <div className="mt-card mt-card--flush">
          <div className="mt-scroll-x">
            <table className="mt-table">
              <thead>
                <tr>
                  <th scope="col">서비스</th>
                  <th scope="col">환경</th>
                  <th scope="col">상태</th>
                  <th scope="col">언어</th>
                  <th scope="col">소유 팀</th>
                  <th scope="col">마지막 수신</th>
                </tr>
              </thead>
              <tbody>
                {state.data.map((s) => (
                  <tr key={s.service_id}>
                    <td>
                      <Link className="mt-mono" to={{ pathname: orgPath(org, `services/${s.service_id}`), search }}>
                        {s.namespace === '' ? s.name : `${s.namespace}/${s.name}`}
                      </Link>
                    </td>
                    <td>{s.environment}</td>
                    <td>
                      <ServiceStatus status={s.status} />
                    </td>
                    <td>{s.language ?? <span className="mt-muted">알 수 없음</span>}</td>
                    <td>{s.owner_team ?? <span className="mt-muted">미설정</span>}</td>
                    <td className="mt-num-left">{formatDateTime(s.last_seen, ctx.timeZone)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="mt-card__footer">
            {cursors.length > 0 && (
              <button type="button" className="mt-button" onClick={() => setCursors((c) => c.slice(0, -1))}>
                이전
              </button>
            )}
            {state.nextCursor !== null && (
              <button
                type="button"
                className="mt-button"
                onClick={() => {
                  const next = state.nextCursor;
                  if (next !== null) setCursors((c) => [...c, next]);
                }}
              >
                다음 {PAGE}개
              </button>
            )}
          </div>
        </div>
      )}
    </section>
  );
}

export function ServiceStatus({ status }: { status: 'active' | 'inactive' | 'archived' }) {
  switch (status) {
    case 'active':
      return <StatusBadge tone="info">수신 중</StatusBadge>;
    case 'inactive':
      return <StatusBadge tone="neutral">24시간 미수신</StatusBadge>;
    default:
      return <StatusBadge tone="neutral">보관됨 (30일 미수신)</StatusBadge>;
  }
}
