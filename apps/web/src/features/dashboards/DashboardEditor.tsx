// S10 dashboard 편집 (D05 §10 "보기와 편집 분리, autosave는 로컬 draft, 명시적 publish", ADR 0058).
// - 저장(publish) 전까지 공유 화면은 바뀌지 않는다. 편집 중인 정의는 이 브라우저에만 초안으로 남는다(새로고침해도 복원).
// - 배치는 drag 대신 키보드로 위치·크기를 고친다(D05 §10). 겹침·24열 초과는 저장 전에 막는다.
// - 다른 사람이 먼저 저장했으면(412) 덮어쓰지 않고 차이를 보인 뒤 고르게 한다: 내 편집을 최신 위에 다시 적용 / 복제로 저장 / 버리기.
import { useEffect, useMemo, useRef, useState } from 'react';
import { Link, useNavigate, useOutletContext, useParams, useSearchParams } from 'react-router';
import { createDashboard, deleteDashboard, getDashboard, updateDashboard, type DashboardItem, type DashboardSpec, type Placement, type Variable, type Widget, type WidgetKind } from '../../api/dashboards.ts';
import { queryMetrics } from '../../api/metrics.ts';
import type { ApiErrorBody, MetricResult } from '../../api/types.ts';
import { useRemote } from '../../api/useRemote.ts';
import { contextOnly, resolveRange, type InvestigationContext } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { formatTimeOfDay } from '../services/format.ts';
import { ConfirmDialog, ErrorNotice, Skeleton } from '../services/states.tsx';
import '../services/services.css';
import '../monitors/monitors.css';
import {
  KIND_LABEL,
  MAX_PARALLEL,
  MAX_VARIABLES,
  MAX_WIDGETS,
  canPlace,
  cellStyle,
  clearDraft,
  emptySpec,
  loadDraft,
  newWidget,
  newWidgetId,
  nextPlacement,
  placementProblem,
  readingOrder,
  saveDraft,
  specDiff,
  specProblems,
  type Draft,
} from './dashboardSpec.ts';
import { QueryPool } from './queryPool.ts';
import { WidgetEditor } from './WidgetEditor.tsx';
import { WidgetView } from './WidgetView.tsx';
import './dashboards.css';

const NEW = 'new';

export function DashboardEditor() {
  const { org = '', dashboardId } = useParams();
  const editing = dashboardId !== undefined;
  const existing = useRemote(editing ? `${org}|dashboard|${dashboardId}` : null, 0, (signal) => getDashboard(dashboardId ?? '', signal));
  if (!editing) return <EditorBody org={org} base={null} />;
  if (existing.error?.status === 404) {
    return (
      <section className="mt-placeholder">
        <h1 className="mt-title">dashboard를 찾을 수 없습니다</h1>
        <p>삭제됐거나, 볼 수 있는 권한이 없거나, 이 배포에서 dashboard 기능이 꺼져 있습니다.</p>
      </section>
    );
  }
  if (existing.data === null) {
    return (
      <section className="mt-page">
        {existing.error !== null ? (
          <ErrorNotice error={existing.error} lastSuccessMs={null} timeZone="UTC" />
        ) : (
          <Skeleton height={240} label="dashboard를 불러오는 중" />
        )}
      </section>
    );
  }
  return <EditorBody key={existing.data.id} org={org} base={existing.data} />;
}

type Conflict = { latest: DashboardItem; theirs: string[]; mine: string[]; copyKey: string };

function EditorBody({ org, base }: { org: string; base: DashboardItem | null }) {
  const ctx = useOutletContext<InvestigationContext>();
  const [params] = useSearchParams();
  const search = contextOnly(params);
  const navigate = useNavigate();
  const draftId = base?.id ?? NEW;
  const [spec, setSpec] = useState<DashboardSpec>(() => base?.spec ?? emptySpec());
  const [revision, setRevision] = useState<number | null>(base?.revision ?? null);
  // 충돌 비교의 기준: 서버에서 읽은 정의. 이전 revision 기준 초안을 복원하면 그 초안의 기준 정의로 바뀐다
  // (그래야 "그 사이 바뀐 것"이 복원 이후가 아니라 초안 이후의 남의 변경을 보인다)
  const [diffBase, setDiffBase] = useState<DashboardSpec>(() => base?.spec ?? emptySpec());
  const [restorable, setRestorable] = useState<Draft | null>(() => {
    const d = loadDraft(org, draftId);
    return d !== null && JSON.stringify(d.spec) !== JSON.stringify(base?.spec ?? emptySpec()) ? d : null;
  });
  const [editingWidget, setEditingWidget] = useState<string | null>(null);
  // 방금 추가한 위젯: 편집을 취소하면 지운다(빈 위젯이 남지 않게)
  const [freshWidget, setFreshWidget] = useState<string | null>(null);
  // 위젯 편집·위치 대화를 닫으면 그 버튼으로 focus를 돌려준다
  const [focusAfter, setFocusAfter] = useState<string | null>(null);
  const [problems, setProblems] = useState<string[]>([]);
  const pool = useMemo(() => new QueryPool<MetricResult>(MAX_PARALLEL, (key, signal) => queryMetrics(JSON.parse(key) as never, signal)), []);
  const previewNow = useMemo(() => Date.now(), []);
  const previewRange = resolveRange(ctx.range, previewNow);
  const [placing, setPlacing] = useState<string | null>(null);
  const [addKind, setAddKind] = useState<WidgetKind>('time_series');
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<{ body: ApiErrorBody; status: number } | null>(null);
  const [conflict, setConflict] = useState<Conflict | null>(null);
  const [titleError, setTitleError] = useState<string | null>(null);
  const idempotencyKey = useRef(globalThis.crypto.randomUUID());
  const dirty = JSON.stringify(spec) !== JSON.stringify(base?.spec ?? emptySpec());

  // 로컬 초안 autosave(1초 뒤). 복원 여부를 묻는 동안은 덮어쓰지 않는다. 원래대로 되돌리면 초안을 지운다
  useEffect(() => {
    if (restorable !== null) return;
    if (!dirty) {
      clearDraft(org, draftId);
      return;
    }
    const t = setTimeout(() => saveDraft(org, draftId, { baseRevision: revision, baseSpec: diffBase, spec, savedAt: Date.now() }), 1000);
    return () => clearTimeout(t);
  }, [spec, restorable, dirty, org, draftId, revision, diffBase]);
  useEffect(() => {
    if (focusAfter === null) return;
    document.getElementById(focusAfter)?.focus();
    setFocusAfter(null);
  }, [focusAfter]);

  const update = (patch: Partial<DashboardSpec>) => setSpec((s) => ({ ...s, ...patch }));
  const widgetById = new Map(spec.widgets.map((w) => [w.id, w]));

  const addH = addKind === 'stat' ? 3 : 6;
  const addWidget = () => {
    const id = newWidgetId(spec.widgets);
    const w = newWidget(id, addKind);
    update({ widgets: [...spec.widgets, w], layout: [...spec.layout, nextPlacement(spec.layout, id, addKind === 'stat' ? 6 : 12, addH)] });
    setEditingWidget(id);
    setFreshWidget(id);
  };
  const applyWidget = (w: Widget) => {
    update({ widgets: spec.widgets.map((x) => (x.id === w.id ? w : x)) });
    setEditingWidget(null);
    setFreshWidget(null);
    setFocusAfter(`dash-edit-${w.id}`);
  };
  const cancelWidget = (id: string) => {
    if (freshWidget === id) {
      removeWidget(id);
      setFocusAfter('dash-add-kind');
    } else {
      setFocusAfter(`dash-edit-${id}`);
    }
    setEditingWidget(null);
    setFreshWidget(null);
  };
  const removeWidget = (id: string) => update({ widgets: spec.widgets.filter((w) => w.id !== id), layout: spec.layout.filter((p) => p.widget_id !== id) });

  const save = async (opts: { asCopy?: boolean; overRevision?: number; copyKey?: string } = {}) => {
    if (spec.title.trim() === '') {
      setTitleError('제목을 입력하세요');
      return;
    }
    setTitleError(null);
    // 저장 전 전체 검사(취소로 남은 빈 위젯, 끊긴 변수 참조, 배치) — 서버 422로만 알게 되지 않게
    const found = specProblems(spec);
    setProblems(found);
    if (found.length > 0) return;
    setSaving(true);
    setSaveError(null);
    const signal = new AbortController().signal;
    const body = opts.asCopy === true ? { ...spec, title: `${spec.title.trim()} (복사본)`.slice(0, 200) } : spec;
    const r =
      base === null || opts.asCopy === true
        ? await createDashboard(body, opts.asCopy === true ? (opts.copyKey ?? globalThis.crypto.randomUUID()) : idempotencyKey.current, signal)
        : await updateDashboard(base.id, body, opts.overRevision ?? revision ?? base.revision, signal);
    setSaving(false);
    if (r.ok) {
      clearDraft(org, draftId);
      void navigate({ pathname: orgPath(org, `dashboards/${r.data.id}`), search });
      return;
    }
    if (r.status === 412 && base !== null) {
      // 다른 변경이 먼저 저장됐다: 최신본을 읽어 양쪽 차이를 보인다(덮어쓰지 않는다)
      const latest = await getDashboard(base.id, new AbortController().signal);
      if (latest.ok) {
        setConflict({ latest: latest.data, theirs: specDiff(diffBase, latest.data.spec), mine: specDiff(diffBase, spec), copyKey: globalThis.crypto.randomUUID() });
        return;
      }
    }
    setSaveError({ body: r.error, status: r.status });
  };

  const remove = async () => {
    if (base === null) return;
    // 지우기는 서버에서 읽은 revision 기준이다(복원한 초안의 revision이 아니다)
    const r = await deleteDashboard(base.id, base.revision, new AbortController().signal);
    if (r.ok) {
      clearDraft(org, draftId);
      void navigate({ pathname: orgPath(org, 'dashboards'), search });
      return;
    }
    setSaveError({
      body: r.status === 412 ? { ...r.error, message: '다른 변경이 먼저 저장되어 지우지 않았습니다. 최신 내용을 확인한 뒤 다시 지우세요.' } : r.error,
      status: r.status,
    });
  };

  const title = base === null ? '새 dashboard' : `${base.title} 편집`;
  return (
    <section className="mt-page mt-dash-editor" aria-labelledby="dash-edit-title">
      <nav className="mt-breadcrumb" aria-label="위치">
        <Link to={{ pathname: orgPath(org, 'dashboards'), search }}>Dashboards</Link>
        {base !== null && (
          <>
            {' / '}
            <Link to={{ pathname: orgPath(org, `dashboards/${base.id}`), search }}>{base.title}</Link>
          </>
        )}
      </nav>
      <header className="mt-page__header">
        <h1 id="dash-edit-title" className="mt-title">
          {title}
        </h1>
        <span className="mt-label">{dirty ? '저장하지 않은 편집이 있습니다(이 브라우저에 초안으로 남습니다)' : '바뀐 것 없음'}</span>
      </header>

      {restorable !== null && (
        <div className="mt-notice mt-notice--info" role="status">
          <strong>저장하지 않은 초안이 있습니다({formatTimeOfDay(restorable.savedAt, ctx.timeZone)})</strong>
          {restorable.baseRevision !== null && base !== null && restorable.baseRevision !== base.revision && (
            <span>초안은 revision {restorable.baseRevision} 기준입니다. 그 뒤 다른 변경이 저장됐습니다 — 복원한 뒤 저장하면 비교 화면이 나옵니다.</span>
          )}
          <div className="mt-dash-actions">
            <button
              type="button"
              className="mt-button"
              onClick={() => {
                setSpec(restorable.spec);
                if (restorable.baseRevision !== null) setRevision(restorable.baseRevision);
                if (restorable.baseSpec !== null) setDiffBase(restorable.baseSpec);
                setRestorable(null);
              }}
            >
              초안 복원
            </button>
            <button
              type="button"
              className="mt-button"
              onClick={() => {
                clearDraft(org, draftId);
                setRestorable(null);
              }}
            >
              초안 버리기
            </button>
          </div>
        </div>
      )}

      <fieldset className="mt-dash-fieldset">
        <legend>정보</legend>
        <div className="mt-dash-field">
          <label htmlFor="dash-title-input">제목</label>
          <input
            id="dash-title-input"
            className="mt-input"
            value={spec.title}
            maxLength={200}
            onChange={(e) => update({ title: e.target.value })}
            aria-invalid={titleError !== null}
            aria-describedby={titleError === null ? undefined : 'dash-title-error'}
          />
          {titleError !== null && (
            <span id="dash-title-error" className="mt-dash-error" role="alert">
              {titleError}
            </span>
          )}
        </div>
        <div className="mt-dash-field">
          <label htmlFor="dash-desc">설명</label>
          <textarea id="dash-desc" className="mt-input" rows={2} maxLength={2000} value={spec.description} onChange={(e) => update({ description: e.target.value })} />
        </div>
      </fieldset>

      <VariablesEditor variables={spec.variables} onChange={(variables) => update({ variables })} />

      <fieldset className="mt-dash-fieldset">
        <legend>
          위젯 ({spec.widgets.length}/{MAX_WIDGETS})
        </legend>
        {readingOrder(spec.layout).map((p) => {
          const w = widgetById.get(p.widget_id);
          if (w === undefined) return null;
          return (
            <div key={w.id}>
              <div className="mt-dash-list-item">
                <span>
                  <strong>{w.title === '' ? (w.query?.metric || '(제목 없음)') : w.title}</strong> <span className="mt-muted">· {KIND_LABEL[w.kind]} · 위치 {p.x},{p.y} 크기 {p.w}×{p.h}</span>
                </span>
                <span className="mt-dash-cell__edit">
                  <button id={`dash-edit-${w.id}`} type="button" className="mt-button" onClick={() => setEditingWidget(w.id)} aria-label={`${w.title || w.id} 편집`}>
                    편집
                  </button>
                  <button id={`dash-place-${w.id}`} type="button" className="mt-button" onClick={() => setPlacing(w.id)} aria-label={`${w.title || w.id} 위치·크기`}>
                    위치·크기
                  </button>
                  <button type="button" className="mt-button mt-button--danger" onClick={() => removeWidget(w.id)} aria-label={`${w.title || w.id} 삭제`}>
                    삭제
                  </button>
                </span>
              </div>
              {editingWidget === w.id && (
                <WidgetEditor
                  initial={w}
                  variables={spec.variables}
                  onApply={applyWidget}
                  onCancel={() => cancelWidget(w.id)}
                  renderPreview={(draft) => (
                    <WidgetView
                      widget={draft}
                      values={Object.fromEntries(spec.variables.map((v) => [v.name, v.default]))}
                      rangeKey={JSON.stringify(ctx.range)}
                      globalEnv={ctx.environment}
                      fromMs={previewRange.fromMs}
                      toMs={previewRange.toMs}
                      nowMs={previewNow}
                      refreshTick={0}
                      pool={pool}
                      timeZone={ctx.timeZone}
                    />
                  )}
                />
              )}
              {placing === w.id && (
                <PlacementEditor
                  placement={p}
                  layout={spec.layout}
                  onApply={(np) => {
                    update({ layout: spec.layout.map((x) => (x.widget_id === np.widget_id ? np : x)) });
                    setPlacing(null);
                    setFocusAfter(`dash-place-${w.id}`);
                  }}
                  onCancel={() => {
                    setPlacing(null);
                    setFocusAfter(`dash-place-${w.id}`);
                  }}
                />
              )}
            </div>
          );
        })}
        <div className="mt-dash-actions">
          <label className="mt-label" htmlFor="dash-add-kind">
            새 위젯
          </label>
          <select id="dash-add-kind" className="mt-select" value={addKind} onChange={(e) => setAddKind(e.target.value as WidgetKind)}>
            {(['time_series', 'stat', 'table', 'top_list', 'text'] as const).map((k) => (
              <option key={k} value={k}>
                {KIND_LABEL[k]}
              </option>
            ))}
          </select>
          <button type="button" className="mt-button" onClick={addWidget} disabled={spec.widgets.length >= MAX_WIDGETS || !canPlace(spec.layout, addH) || editingWidget !== null}>
            위젯 추가
          </button>
          <span className="mt-label">histogram 분포 위젯은 아직 없습니다(분포 조회 API 이후).</span>
        </div>
      </fieldset>

      {spec.widgets.length > 0 && (
        <section aria-labelledby="dash-preview-title">
          <h2 id="dash-preview-title" className="mt-card__title">
            배치 미리보기(24열)
          </h2>
          <div className="mt-dash-grid mt-dash-preview" aria-hidden="true">
            {spec.layout.map((p) => (
              <div key={p.widget_id} className="mt-dash-cell" style={cellStyle(p)}>
                <div className="mt-card mt-dash-widget">
                  <span className="mt-label">{widgetById.get(p.widget_id)?.title || p.widget_id}</span>
                </div>
              </div>
            ))}
          </div>
        </section>
      )}

      {conflict !== null && (
        <div className="mt-notice mt-notice--warning" role="alert">
          <strong>
            다른 변경이 먼저 저장되었습니다(revision {conflict.latest.revision}, {conflict.latest.updated_by}). 덮어쓰지 않았습니다.
          </strong>
          <span>그 사이 바뀐 것: {conflict.theirs.length === 0 ? '없음' : conflict.theirs.join(' · ')}</span>
          <span>내 편집: {conflict.mine.length === 0 ? '없음' : conflict.mine.join(' · ')}</span>
          <div className="mt-dash-actions">
            <button
              type="button"
              className="mt-button"
              onClick={() => {
                setRevision(conflict.latest.revision);
                setConflict(null);
                void save({ overRevision: conflict.latest.revision });
              }}
            >
              내 편집을 최신 위에 다시 적용
            </button>
            <button
              type="button"
              className="mt-button"
              onClick={() => {
                const key = conflict.copyKey;
                setConflict(null);
                void save({ asCopy: true, copyKey: key });
              }}
            >
              새 dashboard로 복제해 저장
            </button>
            <button
              type="button"
              className="mt-button"
              onClick={() => {
                clearDraft(org, draftId);
                void navigate({ pathname: orgPath(org, `dashboards/${conflict.latest.id}`), search });
              }}
            >
              내 편집 버리고 최신 보기
            </button>
          </div>
        </div>
      )}
      {problems.length > 0 && (
        <div className="mt-notice mt-notice--critical" role="alert">
          <strong>저장하기 전에 고칠 것이 있습니다</strong>
          {problems.map((p) => (
            <span key={p}>{p}</span>
          ))}
        </div>
      )}
      {saveError !== null && (
        <div className="mt-notice mt-notice--critical" role="alert">
          <strong>저장하지 못했습니다</strong>
          <span>{saveError.status === 403 ? 'dashboard를 바꿀 권한이 없습니다(Developer 이상).' : saveError.body.message}</span>
          {((saveError.body.details as { field_violations?: { field: string; reason: string }[] } | undefined)?.field_violations ?? []).map((v) => (
            <span key={v.field} className="mt-mono">
              {v.field}: {v.reason}
            </span>
          ))}
        </div>
      )}
      <div className="mt-dash-actions mt-monitor-form__nav">
        <button type="button" className="mt-button mt-button--primary" onClick={() => void save()} disabled={saving}>
          {saving ? '저장 중…' : base === null ? '만들기' : '저장(공유 화면에 반영)'}
        </button>
        {base !== null && (
          <Link className="mt-button" to={{ pathname: orgPath(org, `dashboards/${base.id}`), search }}>
            편집 그만두기
          </Link>
        )}
        {base !== null && <DeleteButton title={base.title} onConfirm={remove} />}
      </div>
    </section>
  );
}

function VariablesEditor({ variables, onChange }: { variables: Variable[]; onChange: (v: Variable[]) => void }) {
  const set = (i: number, patch: Partial<Variable>) => onChange(variables.map((v, j) => (j === i ? { ...v, ...patch } : v)));
  const names = variables.map((v) => v.name);
  return (
    <fieldset className="mt-dash-fieldset">
      <legend>변수</legend>
      <span className="mt-label">위젯 조건의 값에 $이름을 쓰면, 보는 사람이 고른 값으로 거릅니다. 기본값을 비우면 "전체"입니다.</span>
      {variables.map((v, i) => {
        const bad = !/^[a-z][a-z0-9_]{0,31}$/.test(v.name) ? '영문 소문자로 시작, 소문자·숫자·_ 32자까지' : names.indexOf(v.name) !== i ? '이름이 겹칩니다' : null;
        return (
          <div key={i} className="mt-dash-inline">
            <div className="mt-dash-field">
              <label htmlFor={`var-name-${String(i)}`}>이름</label>
              <input id={`var-name-${String(i)}`} className="mt-input mt-mono" value={v.name} onChange={(e) => set(i, { name: e.target.value })} aria-describedby={bad === null ? undefined : `var-name-${String(i)}-error`} />
              {bad !== null && (
                <span id={`var-name-${String(i)}-error`} className="mt-dash-error" role="alert">
                  {bad}
                </span>
              )}
            </div>
            <div className="mt-dash-field">
              <label htmlFor={`var-kind-${String(i)}`}>종류</label>
              <select id={`var-kind-${String(i)}`} className="mt-select" value={v.kind} onChange={(e) => set(i, { kind: e.target.value as Variable['kind'] })}>
                <option value="service">서비스(service.name)</option>
                <option value="environment">환경(deployment.environment.name)</option>
              </select>
            </div>
            <div className="mt-dash-field">
              <label htmlFor={`var-default-${String(i)}`}>기본값</label>
              <input
                id={`var-default-${String(i)}`}
                className="mt-input mt-mono"
                value={v.default ?? ''}
                placeholder="전체"
                onChange={(e) => set(i, { default: e.target.value.trim() === '' ? null : e.target.value })}
              />
            </div>
            <button type="button" className="mt-button" onClick={() => onChange(variables.filter((_, j) => j !== i))} aria-label={`변수 ${v.name} 지우기`}>
              지우기
            </button>
          </div>
        );
      })}
      <div>
        <button
          type="button"
          className="mt-button"
          disabled={variables.length >= MAX_VARIABLES}
          onClick={() => onChange([...variables, { name: variables.some((v) => v.name === 'service') ? `var${String(variables.length + 1)}` : 'service', kind: 'service', default: null }])}
        >
          변수 추가
        </button>
        <span className="mt-label"> 팀 변수는 서비스 소유 팀 정보가 생긴 뒤 쓸 수 있습니다.</span>
      </div>
    </fieldset>
  );
}

/** 위치·크기를 숫자와 화살표 버튼으로 고친다(D05 §10 "drag 대신 키보드 이동·resize dialog"). */
function PlacementEditor({ placement, layout, onApply, onCancel }: { placement: Placement; layout: Placement[]; onApply: (p: Placement) => void; onCancel: () => void }) {
  const [p, setP] = useState(placement);
  const problem = placementProblem(p, layout);
  const first = useRef<HTMLInputElement>(null);
  useEffect(() => first.current?.focus(), []);
  const field = (k: 'x' | 'y' | 'w' | 'h', label: string) => (
    <div className="mt-dash-field">
      <label htmlFor={`place-${p.widget_id}-${k}`}>{label}</label>
      <input
        ref={k === 'x' ? first : undefined}
        id={`place-${p.widget_id}-${k}`}
        className="mt-input"
        type="number"
        value={p[k]}
        onChange={(e) => setP({ ...p, [k]: Math.trunc(Number(e.target.value)) })}
      />
    </div>
  );
  const nudge = (dx: number, dy: number) => setP({ ...p, x: p.x + dx, y: Math.max(0, p.y + dy) });
  return (
    <fieldset
      className="mt-dash-fieldset mt-card"
      aria-label={`${placement.widget_id} 위치·크기`}
      onKeyDown={(e) => {
        if (e.key === 'Escape') onCancel();
      }}
    >
      <legend>위치·크기(24열 grid)</legend>
      <div className="mt-dash-inline">
        {field('x', '가로 위치(0~23)')}
        {field('y', '세로 위치')}
        {field('w', '폭(열)')}
        {field('h', '높이(행)')}
      </div>
      <div className="mt-dash-actions" role="group" aria-label="한 칸씩 옮기기">
        <button type="button" className="mt-button" onClick={() => nudge(-1, 0)}>
          ← 왼쪽
        </button>
        <button type="button" className="mt-button" onClick={() => nudge(1, 0)}>
          오른쪽 →
        </button>
        <button type="button" className="mt-button" onClick={() => nudge(0, -1)}>
          ↑ 위
        </button>
        <button type="button" className="mt-button" onClick={() => nudge(0, 1)}>
          ↓ 아래
        </button>
      </div>
      {problem !== null && (
        <span className="mt-dash-error" role="alert">
          {problem}
        </span>
      )}
      <div className="mt-dash-actions">
        <button type="button" className="mt-button mt-button--primary" disabled={problem !== null} onClick={() => onApply(p)}>
          적용
        </button>
        <button type="button" className="mt-button" onClick={onCancel}>
          취소
        </button>
      </div>
    </fieldset>
  );
}

function DeleteButton({ title, onConfirm }: { title: string; onConfirm: () => Promise<void> }) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const confirm = useRef<HTMLButtonElement>(null);
  const wasOpen = useRef(false);
  useEffect(() => {
    if (open) confirm.current?.focus();
    else if (wasOpen.current) trigger.current?.focus();
    wasOpen.current = open;
  }, [open]);
  return (
    <>
      <button ref={trigger} type="button" className="mt-button mt-button--danger" onClick={() => setOpen(true)} disabled={open}>
        dashboard 삭제
      </button>
      {open && (
        <ConfirmDialog
          id="dash-delete"
          title={`“${title}”를 삭제할까요?`}
          confirmLabel="삭제"
          busyLabel="삭제 중…"
          busy={busy}
          onConfirm={() => {
            setBusy(true);
            void onConfirm().finally(() => setBusy(false));
          }}
          onCancel={() => setOpen(false)}
          confirmRef={confirm}
        />
      )}
    </>
  );
}
