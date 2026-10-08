// S01 Overview (D05 §04 "service summary, 상태→서비스", F03, ADR 0048).
// 조직 전체(또는 선택 환경)의 요청량·오류율·p95와 서비스별 요약 표. 행을 고르면 서비스 상세(S02)로 간다.
// - 원천은 비샘플링 SDK histogram이다(계약 5). trace 표본으로 세지 않는다.
// - SLO·monitor가 아직 없어 "정상/이상" 판정을 하지 않는다. 지어낸 기준으로 상태를 칠하지 않고, 정렬로 살펴보게 한다.
// - 값이 없는 서비스는 0이 아니라 이유를 보인다(계약 6).
import { useMemo, useState, type ReactNode } from 'react';
import { Link, useOutletContext, useParams, useSearchParams } from 'react-router';
import { listServices } from '../../api/catalog.ts';
import { queryMetrics } from '../../api/metrics.ts';
import type { MetricAggregation, MetricFilter, MetricQuery, MetricResult, MetricSeries } from '../../api/types.ts';
import { useRemote, type RemoteState } from '../../api/useRemote.ts';
import { contextOnly, formatRange, resolveRange, type InvestigationContext } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { REASON_LABEL, formatDateTime, formatMilliseconds, formatPercent, formatRate } from '../services/format.ts';
import { MetricChart } from '../services/MetricChart.tsx';
import { DURATION_METRIC, STATUS_KEY, combineStatus, metricWindow, single, toMilliseconds, toPoints, type MetricWindow, type Point } from '../services/red.ts';
import { RedCard, partialReason } from '../services/ServiceDetail.tsx';
import { ServiceStatus } from '../services/ServiceList.tsx';
import { ErrorNotice, Skeleton, SourceBadge, StatusBadge } from '../services/states.tsx';
import '../services/services.css';
import { ENV_KEY, SERVICE_KEYS, displayName, isSortKey, overviewRows, sortRows, type OverviewRow, type SortKey } from './overview.ts';
import './overview.css';

const CATALOG_LIMIT = 1000;

const SORT_LABEL: Record<SortKey, string> = { error_rate: '오류율', rps: '요청량', p95: 'p95', name: '이름' };

export function Overview() {
  const { org = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const [params, setParams] = useSearchParams();
  const [refreshTick, setRefreshTick] = useState(0);
  const rangeKey = JSON.stringify(ctx.range);
  const nowMs = useMemo(() => Date.now(), [rangeKey, refreshTick]);
  const { fromMs, toMs } = resolveRange(ctx.range, nowMs);
  const win = metricWindow(fromMs, toMs);
  const rawSort = params.get('sort');
  const sort: SortKey = isSortKey(rawSort) ? rawSort : 'error_rate';
  const setSort = (k: SortKey) => {
    const next = new URLSearchParams(params);
    if (k === 'error_rate') next.delete('sort');
    else next.set('sort', k);
    setParams(next, { replace: true });
  };

  return (
    <section className="mt-page" aria-labelledby="overview-title">
      <header className="mt-page__header">
        <h1 id="overview-title" className="mt-title">
          Overview
        </h1>
        <span className="mt-label">
          {ctx.environment === null ? '전체 환경' : `환경 ${ctx.environment}`} · {formatRange(ctx.range, nowMs, ctx.timeZone)}
        </span>
        <button type="button" className="mt-button mt-page__action" onClick={() => setRefreshTick((n) => n + 1)}>
          새로고침
        </button>
      </header>
      {win === null ? (
        <div className="mt-notice mt-notice--warning" role="alert">
          metric은 7일까지 조회할 수 있습니다. 시간 범위를 줄이세요.
        </div>
      ) : (
        <Body org={org} ctx={ctx} win={win} refreshTick={refreshTick} sort={sort} onSort={setSort} search={contextOnly(params)} />
      )}
    </section>
  );
}

function useOverviewQueries(org: string, environment: string | null, win: MetricWindow, refreshTick: number) {
  const filter: MetricFilter | undefined = environment === null ? undefined : { op: 'eq', field: ENV_KEY, value: environment };
  const make = (aggregation: MetricAggregation, stepSeconds: number, groupBy: string[]): MetricQuery => ({
    range: { from: win.fromIso, to: win.toIso },
    step_seconds: stepSeconds,
    expression: { metric: DURATION_METRIC, aggregation, ...(filter === undefined ? {} : { filter }), ...(groupBy.length === 0 ? {} : { group_by: groupBy }) },
  });
  const use = (name: string, q: MetricQuery) =>
    useRemote<MetricResult>(`${org}|overview|${name}|${JSON.stringify(q)}`, refreshTick, (signal) => queryMetrics(q, signal));
  const whole = win.rangeSeconds;
  return {
    chartCounts: use('chart-count', make('count', win.chartStepSeconds, [STATUS_KEY])),
    chartP95: use('chart-p95', make('p95', win.chartStepSeconds, [])),
    totalCounts: use('total-count', make('count', whole, [STATUS_KEY])),
    totalP95: use('total-p95', make('p95', whole, [])),
    serviceCounts: use('svc-count', make('count', whole, [...SERVICE_KEYS, STATUS_KEY])),
    serviceP95: use('svc-p95', make('p95', whole, [...SERVICE_KEYS])),
  };
}

const seriesOf = (s: RemoteState<MetricResult>): MetricSeries[] => s.data?.series ?? [];

function toMs(points: Point[], unit: string | null): Point[] {
  return points.map((p) => {
    if (p.value === null) return p;
    const ms = toMilliseconds(p.value, unit);
    return ms === null ? { ...p, value: null, reason: 'unit_conflict' } : { ...p, value: ms };
  });
}

function Body({
  org,
  ctx,
  win,
  refreshTick,
  sort,
  onSort,
  search,
}: {
  org: string;
  ctx: InvestigationContext;
  win: MetricWindow;
  refreshTick: number;
  sort: SortKey;
  onSort: (k: SortKey) => void;
  search: string;
}) {
  const q = useOverviewQueries(org, ctx.environment, win, refreshTick);
  const catalog = useRemote(`${org}|services|${ctx.environment ?? ''}|overview`, refreshTick, (signal) =>
    listServices({ environment: ctx.environment, cursor: null, limit: CATALOG_LIMIT }, signal),
  );
  const all = Object.values(q);
  // 위쪽 알림은 카드 조회의 실패다. 서비스 표·catalog 실패는 표 자리에서 알린다(같은 실패를 두 번 보이지 않게).
  const failed = [q.chartCounts, q.chartP95, q.totalCounts, q.totalP95].find((s) => s.error !== null);
  const loading = all.some((s) => s.status === 'loading');
  const fromMs = Date.parse(win.fromIso);
  const toMsEnd = Date.parse(win.toIso);

  const chart = combineStatus(seriesOf(q.chartCounts), win.chartStepSeconds);
  const total = combineStatus(seriesOf(q.totalCounts), win.rangeSeconds);
  // 조회가 실패한 값은 "metric 없음"이 아니라 "확인할 수 없음"이다(D05 §03 실패 > empty)
  const failedPoint: Point = { tMs: Number.NaN, value: null, reason: 'query_failed', partial: false };
  const totalOr = (s: RemoteState<MetricResult>, p: Point): Point => (s.error !== null && s.data === null ? failedPoint : p);
  const chartOr = (s: RemoteState<MetricResult>, node: ReactNode): ReactNode =>
    s.error !== null && s.data === null ? <p className="mt-muted">차트를 확인할 수 없음(조회 실패)</p> : node;
  const p95Unit = seriesOf(q.chartP95)[0]?.unit ?? seriesOf(q.totalP95)[0]?.unit ?? null;
  const p95Chart = toMs(toPoints(seriesOf(q.chartP95)[0]), p95Unit);
  const p95Total = single(toMs(toPoints(seriesOf(q.totalP95)[0]), seriesOf(q.totalP95)[0]?.unit ?? null));
  const marks = all.map((s) => s.meta?.watermark ?? null);
  const watermark = marks.some((m) => m === null) ? null : marks.reduce<string | null>((a, m) => (a === null || (m !== null && m < a) ? m : a), null);
  const partialNote = partialReason(watermark, toMsEnd);
  const noMetric = !loading && failed === undefined && seriesOf(q.totalCounts).length === 0;

  // 서비스 표: metric 조회가 실패하면 표 대신 실패를 보인다. catalog만 실패하면 metric 행을 보이되 등록 여부는 "확인 안 됨"이다.
  const tableError = q.serviceCounts.error ?? q.serviceP95.error;
  const catalogSettled = catalog.data !== null || catalog.error !== null;
  const rowsReady = q.serviceCounts.data !== null && q.serviceP95.data !== null && catalogSettled;
  const catalogComplete = catalog.data !== null && catalog.nextCursor === null;
  const rows = rowsReady
    ? sortRows(overviewRows(catalog.data ?? [], seriesOf(q.serviceCounts), seriesOf(q.serviceP95), win.rangeSeconds, catalogComplete), sort)
    : [];

  return (
    <div className="mt-red">
      {failed?.error != null && <ErrorNotice error={failed.error} lastSuccessMs={failed.lastSuccessMs} timeZone={ctx.timeZone} />}
      <div className="mt-red__source">
        <SourceBadge>SDK metric · 비샘플링</SourceBadge>
        <span className="mt-label">
          {DURATION_METRIC}
          {watermark !== null && ` · ${formatDateTime(watermark, ctx.timeZone)}까지 집계 확정`}
        </span>
      </div>
      {noMetric ? (
        <div className="mt-empty">
          <strong>이 범위에 HTTP 서버 metric이 없습니다</strong>
          <span>OpenTelemetry HTTP 서버 계측이 metric을 보내는지 확인하거나 시간 범위를 넓혀 보세요. 요청이 0건이라는 뜻이 아닙니다.</span>
          <Link to={{ pathname: orgPath(org, 'setup'), search }}>설치 안내</Link>
        </div>
      ) : (
        <div className="mt-red__cards">
          <RedCard
            title="요청량"
            unit="req/s"
            total={totalOr(q.totalCounts, single(total.requestsPerSecond))}
            partialNote={partialNote}
            format={formatRate}
            loading={q.totalCounts.status === 'loading'}
            chart={chartOr(q.chartCounts, <MetricChart title="요청량" points={chart.requestsPerSecond} format={formatRate} unit=" req/s" fromMs={fromMs} toMs={toMsEnd} timeZone={ctx.timeZone} />)}
            chartLoading={q.chartCounts.status === 'loading'}
          />
          <RedCard
            title="오류율 (5xx)"
            unit="%"
            total={totalOr(q.totalCounts, single(total.errorRate))}
            partialNote={partialNote}
            format={formatPercent}
            loading={q.totalCounts.status === 'loading'}
            chart={chartOr(q.chartCounts, <MetricChart title="오류율" points={chart.errorRate} format={formatPercent} unit="%" fromMs={fromMs} toMs={toMsEnd} timeZone={ctx.timeZone} />)}
            chartLoading={q.chartCounts.status === 'loading'}
          />
          <RedCard
            title="p95 지연"
            unit=""
            total={totalOr(q.totalP95, p95Total)}
            partialNote={partialNote}
            format={formatMilliseconds}
            loading={q.totalP95.status === 'loading'}
            note="모든 서비스 bucket 병합"
            chart={chartOr(q.chartP95, <MetricChart title="p95 지연" points={p95Chart} format={formatMilliseconds} unit="" fromMs={fromMs} toMs={toMsEnd} timeZone={ctx.timeZone} />)}
            chartLoading={q.chartP95.status === 'loading'}
          />
        </div>
      )}
      <ServiceTable
        org={org}
        rows={rows}
        loading={!rowsReady && tableError === null}
        error={tableError}
        catalogError={catalog.error !== null}
        sort={sort}
        onSort={onSort}
        search={search}
        catalogTruncated={catalog.data !== null && catalog.nextCursor !== null}
        partialNote={partialNote}
        timeZone={ctx.timeZone}
      />
    </div>
  );
}

function ServiceTable({
  org,
  rows,
  loading,
  error,
  catalogError,
  sort,
  onSort,
  search,
  catalogTruncated,
  partialNote,
  timeZone,
}: {
  org: string;
  rows: OverviewRow[];
  loading: boolean;
  error: RemoteState<MetricResult>['error'];
  catalogError: boolean;
  sort: SortKey;
  onSort: (k: SortKey) => void;
  search: string;
  catalogTruncated: boolean;
  partialNote: string;
  timeZone: string;
}) {
  const header = (k: SortKey, numeric: boolean) => (
    <th scope="col" className={numeric ? 'mt-num' : undefined} aria-sort={sort === k ? (k === 'name' ? 'ascending' : 'descending') : 'none'}>
      <button type="button" className="mt-sort-button" onClick={() => onSort(k)}>
        {SORT_LABEL[k]}
        <span aria-hidden="true">{sort === k ? (k === 'name' ? ' ▲' : ' ▼') : ''}</span>
      </button>
    </th>
  );
  return (
    <section className="mt-card mt-card--flush" aria-labelledby="overview-services-title">
      <div className="mt-card__head">
        <h2 id="overview-services-title" className="mt-card__title">
          서비스
        </h2>
        <span className="mt-label">
          {rows.length.toLocaleString('ko-KR')}개 · {SORT_LABEL[sort]} {sort === 'name' ? '가나다순' : '큰 순'}(값 없는 서비스는 뒤)
        </span>
      </div>
      <p className="mt-label mt-overview-note" role="note">
        SLO·monitor가 아직 없어 정상·이상 판정은 하지 않습니다. 오류율·p95가 큰 서비스부터 살펴보세요.
      </p>
      {catalogTruncated && (
        <p className="mt-label mt-overview-note" role="note">
          서비스 catalog는 {CATALOG_LIMIT.toLocaleString('ko-KR')}개까지 불러왔습니다. metric이 있는 서비스는 모두 표에 있고, 목록 밖 서비스의 등록 여부는 확인하지 않았습니다.
        </p>
      )}
      {catalogError && (
        <p className="mt-label mt-overview-note" role="note">
          서비스 catalog를 불러오지 못해 등록 여부·수신 상태를 확인할 수 없습니다. metric이 있는 서비스만 보입니다.
        </p>
      )}
      {error !== null ? (
        <div className="mt-overview-error">
          <ErrorNotice error={error} lastSuccessMs={null} timeZone={timeZone} />
          <p className="mt-label">서비스 요약을 불러오지 못했습니다. 서비스가 없다는 뜻이 아닙니다.</p>
        </div>
      ) : loading ? (
        <Skeleton height={200} label="서비스 요약을 불러오는 중" />
      ) : rows.length === 0 ? (
        <div className="mt-empty mt-empty--inline">
          <strong>표시할 서비스가 없습니다</strong>
          <span>계측을 설치하면 첫 데이터를 받은 뒤 목록에 나타납니다.</span>
        </div>
      ) : (
        <div className="mt-scroll-x">
          <table className="mt-table">
            <caption className="mt-visually-hidden">
              서비스 요약
              {rows.some((r) => r.partial || r.p95.partial) ? ` (* 일부 집계: ${partialNote})` : ''}
            </caption>
            <thead>
              <tr>
                {header('name', false)}
                <th scope="col">환경</th>
                <th scope="col">수신</th>
                {header('rps', true)}
                {header('error_rate', true)}
                {header('p95', true)}
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.key}>
                  <th scope="row" className="mt-overview-svc">
                    {r.serviceId === null ? (
                      <>
                        <span className="mt-mono">{displayName(r) || '(이름 없음)'}</span>{' '}
                        <StatusBadge tone="neutral">{r.catalog === 'unregistered' ? 'catalog 미등록' : 'catalog 확인 안 됨'}</StatusBadge>
                      </>
                    ) : (
                      <Link className="mt-mono" to={{ pathname: orgPath(org, `services/${r.serviceId}`), search }}>
                        {displayName(r)}
                      </Link>
                    )}
                  </th>
                  <td>{r.environment === '' ? <span className="mt-muted">없음</span> : r.environment}</td>
                  <td>{r.catalogStatus === null ? <span className="mt-muted">-</span> : <ServiceStatus status={r.catalogStatus} />}</td>
                  {r.countReason !== null ? (
                    <td className="mt-num" colSpan={2}>
                      <span className="mt-muted">{r.countReason === 'no_series' ? '요청 metric 없음' : REASON_LABEL[r.countReason]}</span>
                    </td>
                  ) : (
                    <>
                      <td className="mt-num">
                        {r.requestsPerSecond === null ? '—' : `${formatRate(r.requestsPerSecond)} req/s`}
                        {r.partial && <PartialMark note={partialNote} />}
                      </td>
                      <td className="mt-num">
                        {r.errorRate === null ? <span className="mt-muted">요청 없음</span> : `${formatPercent(r.errorRate)}%`}
                        {r.partial && <PartialMark note={partialNote} />}
                      </td>
                    </>
                  )}
                  <td className="mt-num">
                    {r.p95.value === null ? (
                      <span className="mt-muted">{r.p95.reason === 'no_series' ? '—' : REASON_LABEL[r.p95.reason ?? 'no_data']}</span>
                    ) : (
                      <>
                        {formatMilliseconds(r.p95.value)}
                        {r.p95.partial && <PartialMark note={partialNote} />}
                      </>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {rows.some((r) => r.partial || r.p95.partial) && (
            <p className="mt-label mt-overview-note">
              <span aria-hidden="true">* </span>
              {partialNote}
            </p>
          )}
        </div>
      )}
    </section>
  );
}

// 칸마다는 짧게, 이유는 표 아래에 한 번만(screen reader가 같은 긴 문장을 칸마다 읽지 않게)
function PartialMark({ note }: { note: string }) {
  return (
    <span className="mt-partial-mark" title={note}>
      <span aria-hidden="true">*</span>
      <span className="mt-visually-hidden">일부 집계</span>
    </span>
  );
}
