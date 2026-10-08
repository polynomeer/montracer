// S02 서비스 상세 (D05 §05, ADR 0042).
// - 서비스 metadata(GET /services/{id})와 RED metric query를 같은 고정 시각(nowMs)·범위로 부른다.
//   nowMs는 범위가 바뀌거나 새로고침할 때만 바뀐다 — 그래야 모든 카드·차트·표가 같은 구간을 본다.
// - RED는 비샘플링 SDK histogram이다(source badge). 값이 없으면 이유를 보이고 0으로 그리지 않는다.
// - 아직 API가 없는 탭(의존성·인스턴스·오류·프로파일·배포)은 비어 있는 척하지 않고 이유를 말한다.
import { useMemo, useState, type ReactNode } from 'react';
import { Link, useOutletContext, useParams, useSearchParams } from 'react-router';
import { getService } from '../../api/catalog.ts';
import { queryMetrics } from '../../api/metrics.ts';
import type { MetricAggregation, MetricQuery, MetricResult, MetricSeries, ServiceItem } from '../../api/types.ts';
import { useRemote, type RemoteState } from '../../api/useRemote.ts';
import { contextOnly, formatRange, resolveRange, type InvestigationContext } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { MetricChart } from './MetricChart.tsx';
import {
  DURATION_METRIC,
  METHOD_KEY,
  ROUTE_KEY,
  STATUS_KEY,
  combineStatus,
  metricWindow,
  resourceRows,
  serviceFilter,
  single,
  toMilliseconds,
  toPoints,
  type MetricWindow,
  type Point,
  type ResourceRow,
} from './red.ts';
import { REASON_LABEL, formatDateTime, formatMilliseconds, formatPercent, formatRate, formatSeconds } from './format.ts';
import { ServiceStatus } from './ServiceList.tsx';
import { ErrorNotice, Skeleton, SourceBadge, StatusBadge } from './states.tsx';
import './services.css';

const TABS = [
  { id: 'overview', label: '개요' },
  { id: 'resources', label: '리소스' },
  { id: 'dependencies', label: '의존성' },
  { id: 'instances', label: '인스턴스' },
  { id: 'errors', label: '오류' },
  { id: 'profiles', label: '프로파일' },
  { id: 'deployments', label: '배포' },
] as const;
type TabId = (typeof TABS)[number]['id'];

// 아직 API가 없는 탭과 그 이유 (ADR 0042, 작업계획서 E03·E04).
const UNAVAILABLE: Record<Exclude<TabId, 'overview' | 'resources'>, string> = {
  dependencies: '의존성은 service map API(POST /service-map/query)가 생기면 보입니다.',
  instances: '인스턴스는 수집 경로가 instance·version을 저장하기 시작하면 보입니다.',
  errors: '오류 그룹은 errors 검색 API가 생기면 보입니다.',
  profiles: '프로파일은 profile 수집(Phase G1)과 함께 제공됩니다.',
  deployments: '배포 marker와 버전 비교는 배포 이벤트 API(POST /deployments)가 생기면 보입니다.',
};

function isTab(v: string | null): v is TabId {
  return TABS.some((t) => t.id === v);
}

export function ServiceDetail() {
  const { org = '', serviceId = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const [params] = useSearchParams();
  const rawTab = params.get('tab');
  const tab: TabId = isTab(rawTab) ? rawTab : 'overview';
  const [refreshTick, setRefreshTick] = useState(0);
  const rangeKey = JSON.stringify(ctx.range);
  // 범위·새로고침이 바뀔 때만 시각을 다시 잡는다(렌더마다 바뀌면 조회가 끝없이 다시 나간다).
  const nowMs = useMemo(() => Date.now(), [rangeKey, refreshTick]);
  const { fromMs, toMs } = resolveRange(ctx.range, nowMs);
  const win = metricWindow(fromMs, toMs);

  const service = useRemote(`${org}|service|${serviceId}`, refreshTick, (signal) => getService(serviceId, signal));

  if (service.error?.status === 404 || service.error?.status === 400) {
    return (
      <section className="mt-page" aria-labelledby="svc-missing">
        <h1 id="svc-missing" className="mt-title">
          서비스를 찾을 수 없습니다
        </h1>
        <p>주소가 바뀌었거나 볼 수 있는 권한이 없습니다.</p>
        <Link to={{ pathname: orgPath(org, 'services'), search: contextOnly(params) }}>서비스 목록으로</Link>
      </section>
    );
  }

  const svc = service.data;
  return (
    <section className="mt-page" aria-labelledby="svc-title">
      <nav aria-label="경로" className="mt-breadcrumb">
        <Link to={{ pathname: orgPath(org, 'services'), search: contextOnly(params) }}>Services</Link>
        <span aria-hidden="true">/</span>
        <span>{svc?.name ?? serviceId}</span>
      </nav>
      {service.error !== null && (
        <ErrorNotice error={service.error} lastSuccessMs={service.lastSuccessMs} onRetry={() => setRefreshTick((n) => n + 1)} />
      )}
      {svc === null ? (
        service.status === 'loading' ? <Skeleton height={64} label="서비스 정보를 불러오는 중" /> : null
      ) : (
        <ServiceHeader svc={svc} ctx={ctx} refreshing={service.refreshing} onRefresh={() => setRefreshTick((n) => n + 1)} />
      )}
      <nav aria-label="서비스 탭" className="mt-tabs">
        {TABS.map((t) => {
          const q = new URLSearchParams(params);
          if (t.id === 'overview') q.delete('tab');
          else q.set('tab', t.id);
          const s = q.toString();
          return (
            <Link key={t.id} to={{ search: s === '' ? '' : `?${s}` }} aria-current={t.id === tab ? 'page' : undefined} className="mt-tab">
              {t.label}
            </Link>
          );
        })}
      </nav>
      {svc !== null && (tab === 'overview' || tab === 'resources') && (
        <>
          {svc.environment !== ctx.environment && ctx.environment !== null && (
            <div className="mt-notice mt-notice--info" role="note">
              이 서비스는 환경 <strong>{svc.environment}</strong>에 있습니다. 선택한 환경({ctx.environment})과 달라도 이 서비스의 값을 보여줍니다.
            </div>
          )}
          {win === null ? (
            <div className="mt-notice mt-notice--warning" role="alert">
              metric은 최대 7일까지 조회할 수 있습니다. 시간 범위를 줄이세요.
            </div>
          ) : (
            <Red org={org} svc={svc} win={win} tab={tab} ctx={ctx} refreshTick={refreshTick} rangeText={formatRange(ctx.range, nowMs, ctx.timeZone)} />
          )}
        </>
      )}
      {tab !== 'overview' && tab !== 'resources' && (
        <div className="mt-empty">
          <strong>아직 제공되지 않습니다</strong>
          <span>{UNAVAILABLE[tab]}</span>
        </div>
      )}
    </section>
  );
}

function ServiceHeader({ svc, ctx, refreshing, onRefresh }: { svc: ServiceItem; ctx: InvestigationContext; refreshing: boolean; onRefresh: () => void }) {
  return (
    <header className="mt-svc-header">
      <div className="mt-svc-header__title">
        <h1 id="svc-title" className="mt-title" title={svc.name}>
          {svc.name}
        </h1>
        <ServiceStatus status={svc.status} />
        <StatusBadge tone="neutral">판정 없음 · monitor 없음</StatusBadge>
        <div className="mt-svc-header__actions">
          {refreshing && (
            <span className="mt-label" role="status">
              새로 불러오는 중…
            </span>
          )}
          <button type="button" className="mt-button" onClick={onRefresh}>
            새로고침
          </button>
        </div>
      </div>
      <dl className="mt-svc-meta">
        <div>
          <dt>namespace</dt>
          <dd className="mt-mono">{svc.namespace === '' ? '—' : svc.namespace}</dd>
        </div>
        <div>
          <dt>환경</dt>
          <dd className="mt-mono">{svc.environment}</dd>
        </div>
        <div>
          <dt>소유 팀</dt>
          <dd>{svc.owner_team ?? <span className="mt-muted">미설정</span>}</dd>
        </div>
        <div>
          <dt>Tier</dt>
          <dd>{svc.tier ?? <span className="mt-muted">미설정</span>}</dd>
        </div>
        <div>
          <dt>언어</dt>
          <dd>{svc.language ?? <span className="mt-muted">알 수 없음</span>}</dd>
        </div>
        <div>
          <dt>계측 상태</dt>
          <dd>
            <span className="mt-muted">확인할 수 없음</span>
          </dd>
        </div>
        <div>
          <dt>마지막 수신</dt>
          <dd>{formatDateTime(svc.last_seen, ctx.timeZone)}</dd>
        </div>
      </dl>
    </header>
  );
}

// RED query 7개. 모두 같은 범위(win)와 서비스 조건을 쓴다.
function useRedQueries(org: string, svc: ServiceItem, win: MetricWindow, refreshTick: number) {
  const filter = serviceFilter(svc);
  const make = (aggregation: MetricAggregation, stepSeconds: number, groupBy: string[]): MetricQuery => ({
    range: { from: win.fromIso, to: win.toIso },
    step_seconds: stepSeconds,
    expression: { metric: DURATION_METRIC, aggregation, filter, group_by: groupBy },
  });
  // 고정된 순서로 7번 호출한다(hook 규칙). key는 조직·서비스·query 전체다(tenant와 조건이 다르면 다른 결과).
  const use = (name: string, q: MetricQuery) =>
    useRemote<MetricResult>(`${org}|${svc.service_id}|${name}|${JSON.stringify(q)}`, refreshTick, (signal) => queryMetrics(q, signal));
  const whole = win.rangeSeconds; // 범위 전체를 한 점으로 (ADR 0042)
  return {
    chartCounts: use('chart-count', make('count', win.chartStepSeconds, [STATUS_KEY])),
    chartP95: use('chart-p95', make('p95', win.chartStepSeconds, [])),
    totalCounts: use('total-count', make('count', whole, [STATUS_KEY])),
    totalP95: use('total-p95', make('p95', whole, [])),
    routeCounts: use('route-count', make('count', whole, [ROUTE_KEY, METHOD_KEY, STATUS_KEY])),
    routeP95: use('route-p95', make('p95', whole, [ROUTE_KEY, METHOD_KEY])),
    routeSum: use('route-sum', make('hist_sum', whole, [ROUTE_KEY, METHOD_KEY])),
  };
}

const seriesOf = (s: RemoteState<MetricResult>): MetricSeries[] => s.data?.series ?? [];

/**
 * partial의 이유를 추측하지 않는다(D05 §03). 범위 끝이 집계 확정 경계(watermark)보다 늦으면 최근 구간이 아직 집계 중이고,
 * 아니면 일부 1분 window가 비어 값이 실제보다 작을 수 있다(서버 partial의 다른 원인, ADR 0027 §3).
 */
export function partialReason(watermarkIso: string | null, toMs: number): string {
  if (watermarkIso === null) return '일부 집계(이유 확인할 수 없음)';
  return Date.parse(watermarkIso) < toMs ? '일부 집계(최근 구간 집계 중)' : '일부 집계(빠진 window가 있어 값이 작을 수 있음)';
}

function Red({
  org,
  svc,
  win,
  tab,
  ctx,
  refreshTick,
  rangeText,
}: {
  org: string;
  svc: ServiceItem;
  win: MetricWindow;
  tab: TabId;
  ctx: InvestigationContext;
  refreshTick: number;
  rangeText: string;
}) {
  const q = useRedQueries(org, svc, win, refreshTick);
  const all = Object.values(q);
  // 상태 우선순위(D05 §03): 실패 > partial > empty > 정상.
  // 위쪽 알림은 카드 조회(개요 탭)의 첫 실패다. 리소스 표 조회의 실패는 표 자리에서 알린다(같은 실패를 두 번 보이지 않게).
  const cardQueries = [q.chartCounts, q.chartP95, q.totalCounts, q.totalP95];
  const cardFailed = cardQueries.find((s) => s.error !== null);
  // 실패했지만 이전 결과를 계속 보이는 카드가 있으면 그중 가장 이른 마지막 성공 시각을 알린다(D05 §03 last successful at).
  // 대표 실패(cardFailed)가 처음부터 실패한 query여도 다른 카드의 이전 값이 현재 값처럼 보이지 않게 한다.
  const staleSince = cardQueries
    .filter((s) => s.error !== null && s.data !== null && s.lastSuccessMs !== null)
    .reduce<number | null>((a, s) => (a === null || (s.lastSuccessMs ?? a) < a ? s.lastSuccessMs : a), null);
  const tableFailed = [q.routeCounts, q.routeP95, q.routeSum].find((s) => s.error !== null);
  const loading = all.some((s) => s.status === 'loading');
  const fromMs = Date.parse(win.fromIso);
  const toMs = Date.parse(win.toIso);

  const chart = combineStatus(seriesOf(q.chartCounts), win.chartStepSeconds);
  const total = combineStatus(seriesOf(q.totalCounts), win.rangeSeconds);
  const p95Series = seriesOf(q.chartP95)[0];
  const p95Unit = p95Series?.unit ?? seriesOf(q.totalP95)[0]?.unit ?? null;
  const toMsPoints = (pts: Point[]): Point[] =>
    pts.map((p) => {
      if (p.value === null) return p;
      const ms = toMilliseconds(p.value, p95Unit);
      return ms === null ? { ...p, value: null, reason: 'unit_conflict' } : { ...p, value: ms };
    });
  const p95Chart = toMsPoints(toPoints(p95Series));
  const p95Total = single(toMsPoints(toPoints(seriesOf(q.totalP95)[0])));
  // 조회가 실패한 값은 "받은 metric 없음"이 아니라 "확인할 수 없음"이다(D05 §03 실패 > empty, ADR 0042 변경 이력).
  // 같은 조건의 새로고침만 실패해 이전 결과(data)가 남아 있으면 그 값을 보이고 위쪽 알림이 "마지막 성공 기준"임을 말한다.
  const failedPoint: Point = { tMs: Number.NaN, value: null, reason: 'query_failed', partial: false };
  const totalOr = (s: RemoteState<MetricResult>, p: Point): Point => (s.error !== null && s.data === null ? failedPoint : p);
  const chartOr = (s: RemoteState<MetricResult>, node: ReactNode): ReactNode =>
    s.error !== null && s.data === null ? <p className="mt-muted">차트를 확인할 수 없음(조회 실패)</p> : node;
  // 어느 query든 실패했으면 "metric을 받지 못했다"고 단정하지 않는다
  const noSeries = !loading && cardFailed === undefined && tableFailed === undefined && seriesOf(q.totalCounts).length === 0;
  const rows = resourceRows(seriesOf(q.routeCounts), seriesOf(q.routeP95), seriesOf(q.routeSum), win.rangeSeconds);
  const sumUnit = seriesOf(q.routeSum)[0]?.unit ?? null;
  // 집계 확정 경계: 7개 query 중 가장 이른 watermark (모든 값이 확정된 시각). 하나라도 모르면 표시하지 않는다.
  const marks = all.map((s) => s.meta?.watermark ?? null);
  const watermark = marks.some((m) => m === null) ? null : marks.reduce<string | null>((a, m) => (a === null || (m !== null && m < a) ? m : a), null);
  const partialNote = partialReason(watermark, toMs);

  return (
    <div className="mt-red">
      {tab === 'overview' && cardFailed?.error != null && <ErrorNotice error={cardFailed.error} lastSuccessMs={staleSince} />}
      <div className="mt-red__source">
        <SourceBadge>SDK metric · 비샘플링</SourceBadge>
        <span className="mt-label">
          {DURATION_METRIC} · {rangeText}
          {watermark !== null && ` · ${formatDateTime(watermark, ctx.timeZone)}까지 집계 확정`}
        </span>
      </div>
      {noSeries && (
        <div className="mt-empty">
          <strong>이 서비스에서 HTTP 서버 metric을 받지 못했습니다</strong>
          <span>
            {DURATION_METRIC} histogram이 이 범위에 없습니다. OpenTelemetry HTTP 서버 계측이 켜져 있는지 확인하거나 시간 범위를 넓혀 보세요.
          </span>
          <Link to={{ pathname: orgPath(org, 'setup') }}>설치 안내</Link>
        </div>
      )}
      {tab === 'overview' && !noSeries && (
        <div className="mt-red__cards">
          <RedCard
            title="요청량"
            unit="req/s"
            total={totalOr(q.totalCounts, single(total.requestsPerSecond))}
            partialNote={partialNote}
            format={formatRate}
            loading={q.totalCounts.status === 'loading'}
            chart={chartOr(
              q.chartCounts,
              <MetricChart title="요청량" points={chart.requestsPerSecond} format={formatRate} unit=" req/s" fromMs={fromMs} toMs={toMs} timeZone={ctx.timeZone} />,
            )}
            chartLoading={q.chartCounts.status === 'loading'}
          />
          <RedCard
            title="오류율 (5xx)"
            unit="%"
            total={totalOr(q.totalCounts, single(total.errorRate))}
            partialNote={partialNote}
            format={formatPercent}
            loading={q.totalCounts.status === 'loading'}
            chart={chartOr(
              q.chartCounts,
              <MetricChart title="오류율" points={chart.errorRate} format={formatPercent} unit="%" fromMs={fromMs} toMs={toMs} timeZone={ctx.timeZone} />,
            )}
            chartLoading={q.chartCounts.status === 'loading'}
          />
          <RedCard
            title="p95 지연"
            unit=""
            total={totalOr(q.totalP95, p95Total)}
            partialNote={partialNote}
            format={formatMilliseconds}
            loading={q.totalP95.status === 'loading'}
            note="histogram bucket 병합"
            chart={chartOr(
              q.chartP95,
              <MetricChart title="p95 지연" points={p95Chart} format={formatMilliseconds} unit="" fromMs={fromMs} toMs={toMs} timeZone={ctx.timeZone} />,
            )}
            chartLoading={q.chartP95.status === 'loading'}
          />
        </div>
      )}
      {!noSeries && (
        <ResourceTable
          org={org}
          serviceId={svc.service_id}
          rows={tab === 'overview' ? rows.slice(0, 5) : rows}
          totalRows={rows.length}
          sumUnit={sumUnit}
          p95Unit={seriesOf(q.routeP95)[0]?.unit ?? null}
          // 세 query가 모두 와야 행의 열이 채워진다. 하나라도 처음 불러오는 중이면 "받은 metric 없음" 열 대신 skeleton
          loading={[q.routeCounts, q.routeP95, q.routeSum].some((s) => s.status === 'loading' && s.data === null)}
          failed={tableFailed ?? null}
          more={tab === 'overview' && rows.length > 5}
        />
      )}
    </div>
  );
}

function ValueOrReason({ point, format }: { point: Point; format: (v: number) => string }) {
  if (point.value === null) {
    return <span className="mt-muted">{REASON_LABEL[point.reason ?? 'no_data']}</span>;
  }
  return <>{format(point.value)}</>;
}

function RedCard({
  title,
  unit,
  total,
  partialNote,
  format,
  loading,
  note,
  chart,
  chartLoading,
}: {
  title: string;
  unit: string;
  total: Point;
  partialNote: string;
  format: (v: number) => string;
  loading: boolean;
  note?: string;
  chart: ReactNode;
  chartLoading: boolean;
}) {
  return (
    <section className="mt-card mt-red-card" aria-label={title}>
      <h2 className="mt-card__title">{title}</h2>
      <p className="mt-metric">
        {loading && total.value === null ? (
          <span className="mt-muted">불러오는 중</span>
        ) : (
          <>
            <ValueOrReason point={total} format={format} />
            {total.value !== null && unit !== '' && <span className="mt-metric__unit"> {unit}</span>}
          </>
        )}
      </p>
      <p className="mt-label">
        구간 전체{note === undefined ? '' : ` · ${note}`}
        {total.partial && ` · ${partialNote}`}
      </p>
      {chartLoading ? <Skeleton height={120} label={`${title} 차트를 불러오는 중`} /> : chart}
    </section>
  );
}

function ResourceTable({
  org,
  serviceId,
  rows,
  totalRows,
  sumUnit,
  p95Unit,
  loading,
  failed,
  more,
}: {
  org: string;
  serviceId: string;
  rows: ResourceRow[];
  totalRows: number;
  sumUnit: string | null;
  p95Unit: string | null;
  loading: boolean;
  failed: RemoteState<MetricResult> | null;
  more: boolean;
}) {
  const [params] = useSearchParams();
  const resourcesTab = new URLSearchParams(params);
  resourcesTab.set('tab', 'resources');
  // 이 서비스의 trace 검색(S04, ADR 0043): 조사 context + 서비스 조건
  const traceSearch = (errorsOnly: boolean) => {
    const q = new URLSearchParams(contextOnly(params));
    q.set('service', serviceId);
    if (errorsOnly) q.set('errors', '1');
    return `?${q.toString()}`;
  };
  // 이 서비스의 ERROR 이상 log(S08, ADR 0045)
  const logQuery = new URLSearchParams(contextOnly(params));
  logQuery.set('service', serviceId);
  logQuery.set('sev', 'error');
  const logSearch = `?${logQuery.toString()}`;
  const fmtP95 = (v: number) => {
    const ms = toMilliseconds(v, p95Unit);
    return ms === null ? '단위 불명' : formatMilliseconds(ms);
  };
  const fmtSum = (v: number) => (sumUnit === 's' ? formatSeconds(v) : sumUnit === 'ms' ? formatSeconds(v / 1000) : '단위 불명');
  return (
    <section className="mt-card mt-card--flush" aria-labelledby="resources-title">
      <div className="mt-card__head">
        <h2 id="resources-title" className="mt-card__title">
          리소스
        </h2>
        {failed?.error == null && <span className="mt-label">endpoint {totalRows}개 · 총 소요시간 내림차순</span>}
      </div>
      {failed?.error != null ? (
        // 세 query 중 하나라도 실패하면 행의 일부 열이 "받은 metric 없음"처럼 보이므로 표 대신 실패를 보인다.
        <div className="mt-table-error">
          <ErrorNotice error={failed.error} lastSuccessMs={null} />
          <p className="mt-label">endpoint별 요청을 불러오지 못했습니다. endpoint가 없다는 뜻이 아닙니다.</p>
        </div>
      ) : loading ? (
        <Skeleton height={120} label="리소스를 불러오는 중" />
      ) : rows.length === 0 ? (
        <div className="mt-empty mt-empty--inline">
          <span>이 범위에 endpoint(http.route)별 요청이 없습니다.</span>
        </div>
      ) : (
        <div className="mt-scroll-x">
          <table className="mt-table">
            <thead>
              <tr>
                <th scope="col">Endpoint</th>
                <th scope="col" className="mt-num">
                  요청/s
                </th>
                <th scope="col" className="mt-num">
                  오류율
                </th>
                <th scope="col" className="mt-num">
                  p95
                </th>
                <th scope="col" className="mt-num" aria-sort="descending">
                  총 소요시간
                </th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={`${r.method} ${r.route}`}>
                  <td className="mt-mono">
                    {r.method === '' ? '' : `${r.method} `}
                    {r.route === '' ? <span className="mt-muted">(route 없음)</span> : r.route}
                    {r.partial && <span className="mt-label"> · 일부 집계</span>}
                  </td>
                  <td className="mt-num">
                    {r.requestsPerSecond === null ? <span className="mt-muted">{REASON_LABEL[r.countReason ?? 'no_data']}</span> : formatRate(r.requestsPerSecond)}
                  </td>
                  <td className="mt-num">
                    {r.errorRate === null ? (
                      <span className="mt-muted">{REASON_LABEL[r.requestsPerSecond === null ? (r.countReason ?? 'no_data') : 'no_requests']}</span>
                    ) : (
                      `${formatPercent(r.errorRate)}%`
                    )}
                  </td>
                  <td className="mt-num">
                    <ValueOrReason point={r.p95} format={fmtP95} />
                  </td>
                  <td className="mt-num">
                    <ValueOrReason point={r.totalSeconds} format={fmtSum} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <div className="mt-card__footer">
        {more && <Link to={{ search: `?${resourcesTab.toString()}` }}>모든 리소스 보기</Link>}
        <Link to={{ pathname: orgPath(org, 'traces'), search: traceSearch(true) }}>오류 trace 보기</Link>
        <Link to={{ pathname: orgPath(org, 'traces'), search: traceSearch(false) }}>이 서비스의 trace</Link>
        <Link to={{ pathname: orgPath(org, 'logs'), search: logSearch }}>이 서비스의 오류 log</Link>
      </div>
    </section>
  );
}
