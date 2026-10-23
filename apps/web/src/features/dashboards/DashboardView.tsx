// S10 dashboard 보기 (D05 §10~11, ADR 0058).
// - 저장된(published) 정의를 그린다. 편집은 따로(편집 화면).
// - 변수는 URL(`var-이름`)에 남아 공유된다. 선택지는 이 사용자가 볼 수 있는 서비스·환경뿐이다(무권한 값은 옵션에서도 제거, D05 §11).
// - 30초마다 갱신하고 숨은 탭에서는 멈춘다. 사용자가 멈출 수 있다(WCAG 2.2.2). 같은 query는 합치고 동시에 6개까지만 부른다.
// - 디자인 시안: 머리에 게시 revision·작성자·다음 갱신 시각, 일시 정지·링크 복사, `$이름 =` 변수, query 묶음 요약 줄.
import { useEffect, useMemo, useRef, useState } from 'react';
import { Link, useOutletContext, useParams, useSearchParams } from 'react-router';
import { getDashboard, type Variable } from '../../api/dashboards.ts';
import { listServices } from '../../api/catalog.ts';
import { queryMetrics } from '../../api/metrics.ts';
import type { MetricResult } from '../../api/types.ts';
import { useRemote } from '../../api/useRemote.ts';
import { contextOnly, formatRange, resolveRange, type InvestigationContext } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { formatDateTime, formatTimeOfDay } from '../services/format.ts';
import { EmptyState, ErrorNotice, Skeleton } from '../services/states.tsx';
import '../services/services.css';
import '../metrics/metrics.css';
import { MAX_PARALLEL, MAX_WIDGETS, REFRESH_MS, cellStyle, poolSummary, readingOrder, variableValuesChecked } from './dashboardSpec.ts';
import { QueryPool } from './queryPool.ts';
import { WidgetView } from './WidgetView.tsx';
import './dashboards.css';

/** 30초 자동 갱신. 숨은 탭에서는 멈추고, 다시 보이면 놓친 갱신을 한 번 한다. enabled가 false면(일시 정지) 부르지 않는다. */
function useAutoRefresh(enabled: boolean): [number, () => void, number] {
  const [tick, setTick] = useState(0);
  const [at, setAt] = useState(() => Date.now());
  const missed = useRef(false);
  useEffect(() => {
    if (!enabled) return undefined;
    const bump = () => {
      setTick((n) => n + 1);
      setAt(Date.now());
    };
    const id = setInterval(() => {
      if (document.hidden) missed.current = true;
      else bump();
    }, REFRESH_MS);
    const onVisible = () => {
      if (!document.hidden && missed.current) {
        missed.current = false;
        bump();
      }
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      clearInterval(id);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [enabled]);
  return [
    tick,
    () => {
      setTick((n) => n + 1);
      setAt(Date.now());
    },
    at,
  ];
}

export function DashboardView() {
  const { org = '', dashboardId = '' } = useParams();
  const ctx = useOutletContext<InvestigationContext>();
  const [params, setParams] = useSearchParams();
  const search = contextOnly(params);
  const [paused, setPaused] = useState(false);
  const [copied, setCopied] = useState<'idle' | 'done' | 'failed'>('idle');
  const [tick, refresh, refreshedAt] = useAutoRefresh(!paused);
  // 정의는 새로고침 버튼으로 다시 읽는다(다른 사람이 publish한 정의를 받는다). 자동 갱신은 위젯 값만
  const [defTick, setDefTick] = useState(0);
  const remote = useRemote(`${org}|dashboard|${dashboardId}`, defTick, (signal) => getDashboard(dashboardId, signal));
  // 화면마다 조회 묶음 하나(같은 query 합치기·병렬 6)
  const pool = useMemo(() => new QueryPool<MetricResult>(MAX_PARALLEL, (key, signal) => queryMetrics(JSON.parse(key) as never, signal)), []);
  const rangeKey = JSON.stringify(ctx.range);
  const nowMs = useMemo(() => Date.now(), [rangeKey, tick]);
  const { fromMs, toMs } = resolveRange(ctx.range, nowMs);

  if (remote.error?.status === 404) {
    return (
      <section className="mt-placeholder">
        <h1 className="mt-title">dashboard를 찾을 수 없습니다</h1>
        <p>삭제됐거나, 볼 수 있는 권한이 없거나, 이 배포에서 dashboard 기능이 꺼져 있습니다.</p>
        <Link to={{ pathname: orgPath(org, 'dashboards'), search }}>dashboard 목록</Link>
      </section>
    );
  }
  const d = remote.data;
  const spec = d?.spec;
  const { values, invalid } = spec === undefined ? { values: {}, invalid: [] } : variableValuesChecked(spec.variables, params);
  const setVariable = (name: string, value: string | null) => {
    const next = new URLSearchParams(params);
    next.set(`var-${name}`, value ?? '');
    setParams(next, { replace: true });
  };
  const byId = new Map((spec?.widgets ?? []).map((w) => [w.id, w]));

  return (
    <section className="mt-page" aria-labelledby="dash-title">
      <nav className="mt-breadcrumb" aria-label="위치">
        <Link to={{ pathname: orgPath(org, 'dashboards'), search }}>Dashboards</Link>
      </nav>
      <header className="mt-page__header">
        <h1 id="dash-title" className="mt-title">
          {d?.title ?? '…'}
        </h1>
        <span className="mt-label mt-dash-meta">
          {d != null && (
            <span>
              게시본 r{String(d.revision)} · {formatDateTime(d.updated_at, ctx.timeZone)} {d.updated_by}
            </span>
          )}
          <span>
            {formatRange(ctx.range, nowMs, ctx.timeZone)}
            {ctx.environment !== null && ` · 환경 ${ctx.environment}`}
          </span>
          <span>
            {paused
              ? `자동 갱신 멈춤 · 마지막 ${formatTimeOfDay(refreshedAt, ctx.timeZone)}`
              : `자동 갱신 30초 · 마지막 ${formatTimeOfDay(refreshedAt, ctx.timeZone)} · 다음 ${formatTimeOfDay(refreshedAt + REFRESH_MS, ctx.timeZone)}`}
          </span>
        </span>
        <div className="mt-page__action mt-dash-actions">
          <button type="button" className="mt-button" aria-pressed={paused} onClick={() => setPaused((p) => !p)}>
            {paused ? '자동 갱신 켜기' : '일시 정지'}
          </button>
          <button
            type="button"
            className="mt-button"
            onClick={() => {
              // 지금 주소(변수·시간 범위 포함)를 복사한다. 실패하면 그렇다고 말한다(조용히 넘어가지 않음)
              const done = (ok: boolean) => setCopied(ok ? 'done' : 'failed');
              navigator.clipboard.writeText(window.location.href).then(() => done(true), () => done(false));
            }}
          >
            링크 복사
          </button>
          <span className="mt-label" role="status">
            {copied === 'done' ? '복사함' : copied === 'failed' ? '복사하지 못했습니다' : ''}
          </span>
          <button
            type="button"
            className="mt-button"
            onClick={() => {
              refresh();
              setDefTick((n) => n + 1);
            }}
          >
            새로고침
          </button>
          {d != null && (
            <Link className="mt-button" to={{ pathname: orgPath(org, `dashboards/${d.id}/edit`), search }}>
              편집
            </Link>
          )}
        </div>
      </header>
      {spec !== undefined && spec.description !== '' && <p className="mt-dash-description">{spec.description}</p>}
      {remote.error !== null && <ErrorNotice error={remote.error} lastSuccessMs={remote.lastSuccessMs} timeZone={ctx.timeZone} />}
      {spec === undefined ? (
        remote.error === null && <Skeleton height={240} label="dashboard를 불러오는 중" />
      ) : (
        <>
          {invalid.length > 0 && (
            <p className="mt-notice mt-notice--warning" role="note">
              주소의 변수 값이 올바르지 않아 기본값을 씁니다: {invalid.map((n) => `$${n}`).join(', ')}
            </p>
          )}
          {ctx.environment !== null && (
            <p className="mt-label" role="note">
              상단 환경({ctx.environment})으로 거릅니다. 환경을 직접 거르는 위젯은 그 조건을 따릅니다.
            </p>
          )}
          {spec.variables.length > 0 && <VariableBar org={org} variables={spec.variables} values={values} environment={ctx.environment} onChange={setVariable} />}
          {spec.widgets.length > 0 && <PoolLine summary={poolSummary(spec.widgets, values, fromMs, toMs, nowMs, ctx.environment)} />}
          {spec.widgets.length === 0 ? (
            <EmptyState title="위젯이 없습니다" action={<Link className="mt-button" to={{ pathname: orgPath(org, `dashboards/${d?.id ?? ''}/edit`), search }}>위젯 추가하기</Link>} />
          ) : (
            <div className="mt-dash-grid">
              {readingOrder(spec.layout).map((p) => {
                const w = byId.get(p.widget_id);
                if (w === undefined) return null;
                return (
                  <div key={w.id} className="mt-dash-cell" style={cellStyle(p)}>
                    <WidgetView
                      widget={w}
                      variables={spec.variables}
                      values={values}
                      rangeKey={rangeKey}
                      globalEnv={ctx.environment}
                      fromMs={fromMs}
                      toMs={toMs}
                      nowMs={nowMs}
                      refreshTick={tick}
                      pool={pool}
                      timeZone={ctx.timeZone}
                    />
                  </div>
                );
              })}
            </div>
          )}
        </>
      )}
    </section>
  );
}

/** query 묶음 요약 줄(D05 §10: 같은 query 합치기, view당 병렬 6). 위젯 수는 한도(50)와 함께. */
function PoolLine({ summary }: { summary: ReturnType<typeof poolSummary> }) {
  return (
    <p className="mt-label mt-dash-pool">
      위젯 {summary.widgets}/{MAX_WIDGETS} · query {summary.queries}개
      {summary.merged > 0 && `(같은 query ${String(summary.merged)}개 합침, 요청 ${String(summary.requests)}개)`} · 동시 {MAX_PARALLEL}개까지
    </p>
  );
}

/** 변수 선택. 선택지는 이 사용자가 볼 수 있는 서비스 목록에서 온다(권한 밖 값은 보이지 않는다). */
function VariableBar({
  org,
  variables,
  values,
  environment,
  onChange,
}: {
  org: string;
  variables: Variable[];
  values: Record<string, string | null>;
  environment: string | null;
  onChange: (name: string, value: string | null) => void;
}) {
  const services = useRemote(`${org}|dash-var-services|${environment ?? ''}`, 0, (signal) => listServices({ environment, cursor: null, limit: 200 }, signal));
  // 선택지는 볼 수 있는 값뿐이다. 지금 값(기본값·URL)이 목록에 없으면 따로 "(목록에 없음)"으로 보인다 — 선택지로 섞지 않는다
  const options = (v: Variable): string[] => {
    const list = services.data ?? [];
    const all = v.kind === 'service' ? list.map((s) => s.name) : list.map((s) => s.environment);
    return [...new Set(all)].sort((a, b) => a.localeCompare(b));
  };
  return (
    <div className="mt-dash-vars" role="group" aria-label="dashboard 변수">
      {variables.map((v) => (
        <label key={v.name} className="mt-dash-var" title={v.kind === 'service' ? '서비스' : '환경'}>
          <span className="mt-dash-var__name">${v.name} =</span>
          <select className="mt-select" value={values[v.name] ?? ''} onChange={(e) => onChange(v.name, e.target.value === '' ? null : e.target.value)}>
            <option value="">전체</option>
            {values[v.name] != null && services.data !== null && !options(v).includes(values[v.name] ?? '') && (
              <option value={values[v.name] ?? ''} disabled>
                {values[v.name]} (목록에 없음)
              </option>
            )}
            {options(v).map((o) => (
              <option key={o} value={o}>
                {o}
              </option>
            ))}
          </select>
        </label>
      ))}
      <span className="mt-label">볼 수 있는 값만 고를 수 있습니다 · 시간은 상단 범위를 씁니다</span>
      {services.error !== null && <span className="mt-label">선택지를 불러오지 못했습니다. 지금 값은 그대로 씁니다.</span>}
      {services.nextCursor !== null && <span className="mt-label">서비스가 많아 처음 200개만 고를 수 있습니다.</span>}
    </div>
  );
}
