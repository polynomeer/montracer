// S01 monitor 상태 요약 (D05 §04 "상태→서비스", 디자인 시안 D2-7). monitor 목록 API(ADR 0049·0054)만 쓴다.
// - 상태 칸은 monitor 수다. 경보·평가 오류·데이터 없음·멈춤을 정상과 따로 센다 — 0이나 정상에 섞지 않는다(계약 6).
//   "갱신 멈춤" 칸은 멈춘 정상만 센다(stateBadge·stateRank와 같은 규칙). 멈춘 경보는 경보 칸에 세고 "n개 멈춤"을 붙인다.
// - 주의가 필요한 monitor는 나쁜 순서(STATE_ORDER, 멈춘 정상은 데이터 없음 다음) 상위 5개다.
// - 첫 page(200개)만 센다. 더 있으면 수에 "+"를 붙이고 "없음"을 단정하지 않는다.
// - API가 꺼진 배포(404)·권한 없음(403)·실패는 "monitor 없음"이 아니라 그 이유를 말한다. 갱신이 실패하면 칸을 중립색으로 바꾼다.
// - 시간 범위·환경과 무관한 현재 상태다(monitor 목록 API는 환경 조건을 받지 않는다).
import { Link } from 'react-router';
import { listMonitors, type MonitorItem } from '../../api/monitors.ts';
import { useRemote } from '../../api/useRemote.ts';
import { orgPath } from '../../app/nav.ts';
import { StateBadge } from '../monitors/MonitorList.tsx';
import { describeCondition, stateBadge, stateRank, viewState, type ViewState } from '../monitors/monitorSpec.ts';
import { EmptyState, ErrorNotice, Skeleton } from '../services/states.tsx';
import '../monitors/monitors.css';

/** 칸 순서: 나쁜 것부터. 멈춘 monitor는 상태와 따로 센다(정상처럼 보이지 않게). */
type Tile = 'ALERT' | 'EVALUATION_ERROR' | 'NO_DATA' | 'STALE' | 'PENDING' | 'RECOVERING' | 'OK' | 'OTHER';

const TILE_LABEL: Record<Tile, string> = {
  ALERT: '경보',
  EVALUATION_ERROR: '평가 오류',
  NO_DATA: '데이터 없음',
  STALE: '갱신 멈춤',
  PENDING: '대기',
  RECOVERING: '복구 중',
  OK: '정상',
  OTHER: '판정 전·꺼짐·볼 수 없음',
};

const TILE_HINT: Partial<Record<Tile, string>> = {
  NO_DATA: '정상으로 세지 않음',
  STALE: '마지막 평가가 오래됨',
  OTHER: '정상으로 세지 않음',
};

export function tileOf(m: MonitorItem): Tile {
  const state: ViewState = viewState(m);
  if (state === 'OK' && m.status?.stale === true) return 'STALE';
  switch (state) {
    case 'ALERT':
    case 'EVALUATION_ERROR':
    case 'NO_DATA':
    case 'PENDING':
    case 'RECOVERING':
    case 'OK':
      return state;
    default:
      return 'OTHER';
  }
}

/** 주의가 필요한 monitor: 정상·꺼짐이 아닌 것(멈춘 정상은 포함), 나쁜 순서. */
export function attention(monitors: readonly MonitorItem[]): MonitorItem[] {
  return monitors
    .filter((m) => {
      const s = viewState(m);
      const stale = m.status?.stale ?? false;
      return s !== 'DISABLED' && (s !== 'OK' || stale);
    })
    .sort((a, b) => stateRank(viewState(a), a.status?.stale ?? false) - stateRank(viewState(b), b.status?.stale ?? false) || a.name.localeCompare(b.name));
}

const TILES: Tile[] = ['ALERT', 'EVALUATION_ERROR', 'NO_DATA', 'STALE', 'PENDING', 'RECOVERING', 'OK', 'OTHER'];

export function MonitorSummary({ org, search, refreshTick, timeZone }: { org: string; search: string; refreshTick: number; timeZone: string }) {
  const remote = useRemote(`${org}|overview|monitors`, refreshTick, (signal) => listMonitors(null, signal));
  const status = remote.error?.status;
  const monitors = remote.data ?? [];
  const counts = new Map<Tile, number>();
  const staleCounts = new Map<Tile, number>();
  for (const m of monitors) {
    const t = tileOf(m);
    counts.set(t, (counts.get(t) ?? 0) + 1);
    if (t !== 'STALE' && m.status?.stale === true) staleCounts.set(t, (staleCounts.get(t) ?? 0) + 1);
  }
  const truncated = remote.nextCursor !== null;
  const failedRefresh = remote.error !== null;
  const top = attention(monitors).slice(0, 5);
  return (
    <section className="mt-card mt-overview-monitors" aria-labelledby="overview-monitors-title">
      <div className="mt-card__head">
        <h2 id="overview-monitors-title" className="mt-card__title">
          Monitor 상태
        </h2>
        <span className="mt-label">전체 환경 · 현재 상태</span>
        <Link to={{ pathname: orgPath(org, 'monitors'), search }}>모든 monitor</Link>
      </div>
      {status === 404 ? (
        <EmptyState inline reason="disabled" title="이 배포에서는 monitor를 쓸 수 없습니다">
          monitor 기능이 꺼져 있습니다. 상태가 정상이라는 뜻이 아닙니다.
        </EmptyState>
      ) : status === 403 ? (
        <EmptyState inline reason="forbidden" title="monitor를 볼 권한이 없습니다" />
      ) : remote.data === null ? (
        remote.error !== null ? (
          <div className="mt-overview-error">
            <ErrorNotice error={remote.error} lastSuccessMs={null} timeZone={timeZone} />
            <p className="mt-label">monitor 상태를 불러오지 못했습니다. 경보가 없다는 뜻이 아닙니다.</p>
          </div>
        ) : (
          <Skeleton height={96} label="monitor 상태를 불러오는 중" />
        )
      ) : monitors.length === 0 ? (
        <EmptyState
          inline
          reason="new"
          title="아직 monitor가 없습니다"
          action={
            <Link className="mt-button" to={{ pathname: orgPath(org, 'monitors/new'), search }}>
              monitor 만들기
            </Link>
          }
        >
          monitor가 없으면 서비스의 정상·이상을 판정하지 않습니다.
        </EmptyState>
      ) : (
        <>
          {remote.error !== null && <ErrorNotice error={remote.error} lastSuccessMs={remote.lastSuccessMs} timeZone={timeZone} />}
          {failedRefresh && <p className="mt-label">아래는 마지막 성공 기준입니다. 지금 상태와 다를 수 있습니다.</p>}
          <ul className="mt-overview-tiles" aria-label="상태별 monitor 수">
            {TILES.map((t) => {
              const n = counts.get(t) ?? 0;
              const badge = t === 'STALE' ? stateBadge('OK', true) : t === 'OTHER' ? stateBadge('NOT_EVALUATED', false) : stateBadge(t, false);
              const tone = failedRefresh ? 'neutral' : badge.tone;
              const stale = staleCounts.get(t) ?? 0;
              return (
                <li key={t} className={`mt-overview-tile mt-overview-tile--${tone}${n === 0 && !truncated ? ' mt-overview-tile--zero' : ''}`}>
                  <span className="mt-overview-tile__count">
                    {n.toLocaleString('ko-KR')}
                    {truncated ? '+' : ''}
                  </span>
                  <span className="mt-overview-tile__label">{TILE_LABEL[t]}</span>
                  {TILE_HINT[t] !== undefined && <span className="mt-label">{TILE_HINT[t]}</span>}
                  {stale > 0 && <span className="mt-label">그중 {stale.toLocaleString('ko-KR')}개 갱신 멈춤</span>}
                </li>
              );
            })}
          </ul>
          {truncated && (
            <p className="mt-label" role="note">
              처음 200개 monitor만 셌습니다(수는 최소값). 모두 보려면 Monitors 화면을 쓰세요.
            </p>
          )}
          <h3 className="mt-label mt-kv__title">주의가 필요한 monitor</h3>
          {top.length === 0 ? (
            <p className="mt-muted">
              {truncated ? '처음 200개 중에는 정상·꺼짐이 아닌 monitor가 없습니다. 나머지는 확인하지 않았습니다.' : '정상·꺼짐이 아닌 monitor가 없습니다.'}
            </p>
          ) : (
            <ul className="mt-overview-attention">
              {top.map((m) => (
                <li key={m.id}>
                  <StateBadge state={viewState(m)} stale={m.status?.stale ?? false} />
                  <Link to={{ pathname: orgPath(org, `monitors/${m.id}`), search }}>{m.name}</Link>
                  <span className="mt-label">{describeCondition(m.spec)}</span>
                </li>
              ))}
            </ul>
          )}
        </>
      )}
    </section>
  );
}
