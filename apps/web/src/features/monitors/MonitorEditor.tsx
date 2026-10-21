// S09 Monitor 편집 (D05 §09 "Monitor 편집 흐름"). 1 신호 → 2 query·group → 3 threshold·window → 4 no-data·recovery →
// 5 notification → 6 미리보기·저장. 단계를 뒤로 옮겨도 입력은 남는다(form 하나).
// - 오류율은 % 입력, API는 fraction(D05 §09). 단위가 맞지 않으면 저장하지 않는다(서버 검증).
// - 저장 전 validate의 24시간 dry-run을 보인다(ADR 0052). 저장은 생성 Idempotency-Key·수정 If-Match(412는 덮어쓰지 않음).
import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { Link, useNavigate, useOutletContext, useParams, useSearchParams } from 'react-router';
import type { ApiErrorBody } from '../../api/types.ts';
import { listMetrics } from '../../api/metrics.ts';
import { createMonitor, getMonitor, listPolicies, updateMonitor, validateMonitor, type MonitorItem, type PolicyItem, type ValidateResult } from '../../api/monitors.ts';
import { useRemote, type RemoteState } from '../../api/useRemote.ts';
import { contextOnly, type InvestigationContext } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import { ErrorNotice, Skeleton } from '../services/states.tsx';
import '../services/services.css';
import { DryRunPreview } from './DryRunPreview.tsx';
import {
  describeFilter,
  AGGREGATIONS,
  DEFAULT_ERROR_METRIC,
  EVALUATION_CHOICES,
  aggregationChoice,
  evaluationChoice,
  OPERATOR_TEXT,
  WARNING_TEXT,
  emptyForm,
  fieldForPath,
  formToSpec,
  specToForm,
  type FormErrors,
  type MonitorForm,
} from './monitorSpec.ts';
import './monitors.css';

const STEPS = ['신호', 'query·group', '조건·window', '데이터 없음·복구', '알림', '미리보기·저장'] as const;

/** 필드가 있는 단계 — 서버 오류가 오면 그 단계로 돌아간다. */
const STEP_OF: Partial<Record<keyof MonitorForm, number>> = {
  name: 0,
  kind: 0,
  metric: 0,
  aggregation: 0,
  service: 1,
  environment: 1,
  groupBy: 1,
  windowMinutes: 2,
  evaluationSeconds: 2,
  operator: 2,
  threshold: 2,
  minimumRequests: 2,
  forMinutes: 2,
  noDataAction: 3,
  noDataAfterMinutes: 3,
  policyId: 4,
};

export function MonitorEditor() {
  const { org = '', monitorId } = useParams();
  const editing = monitorId !== undefined;
  const existing = useRemote(editing ? `${org}|monitor|${monitorId}` : null, 0, (signal) => getMonitor(monitorId ?? '', signal));
  if (!editing) return <EditorForm org={org} initial={null} />;
  if (existing.data === null) {
    return (
      <section className="mt-page">
        {existing.error !== null ? <ErrorNotice error={existing.error} lastSuccessMs={null} timeZone="UTC" /> : <Skeleton height={240} label="monitor를 불러오는 중" />}
      </section>
    );
  }
  return <EditorForm key={existing.data.revision} org={org} initial={existing.data} />;
}

function EditorForm({ org, initial }: { org: string; initial: MonitorItem | null }) {
  const ctx = useOutletContext<InvestigationContext>();
  const [params] = useSearchParams();
  const search = contextOnly(params);
  const navigate = useNavigate();
  const [form, setForm] = useState<MonitorForm>(() => (initial === null ? emptyForm() : specToForm(initial.spec)));
  const [step, setStep] = useState(0);
  const [errors, setErrors] = useState<FormErrors>({});
  const [validation, setValidation] = useState<{ status: 'idle' | 'busy' | 'done'; result: ValidateResult | null; error: ApiErrorBody | null; httpStatus: number }>(
    { status: 'idle', result: null, error: null, httpStatus: 0 },
  );
  const [saving, setSaving] = useState<{ busy: boolean; error: string | null; conflict: boolean; idemConflict: boolean }>({
    busy: false,
    error: null,
    conflict: false,
    idemConflict: false,
  });
  // 편집 화면 하나에 key 하나: 저장을 다시 눌러도(응답 유실·재시도) monitor가 두 개 생기지 않는다(D02 §20)
  const idempotencyKey = useRef(globalThis.crypto.randomUUID());
  const policies = useRemote(`${org}|policies`, 0, (signal) => listPolicies(signal));
  const set = <K extends keyof MonitorForm>(k: K, v: MonitorForm[K]) => {
    setForm((f) => ({ ...f, [k]: v }));
    setErrors((e) => ({ ...e, [k]: undefined }));
    setValidation((s) => (s.status === 'idle' ? s : { status: 'idle', result: null, error: null, httpStatus: 0 }));
  };

  const goTo = (n: number) => {
    // 다음 단계로 갈 때 이 단계까지의 입력을 본다(뒤로는 언제나 간다)
    if (n > step) {
      const { errors: e } = formToSpec(form);
      const first = Object.keys(e).map((k) => STEP_OF[k as keyof MonitorForm] ?? 0).filter((s) => s <= step);
      if (first.length > 0) {
        setErrors(e);
        return;
      }
    }
    setStep(n);
  };

  // 미리보기 단계에 들어오면 서버 검증 + 24시간 dry-run
  useEffect(() => {
    if (step !== STEPS.length - 1) return;
    const { spec, errors: e } = formToSpec(form);
    if (spec === null) {
      setErrors(e);
      const back = Math.min(...Object.keys(e).map((k) => STEP_OF[k as keyof MonitorForm] ?? 0));
      setStep(back);
      return;
    }
    const ctrl = new AbortController();
    setValidation({ status: 'busy', result: null, error: null, httpStatus: 0 });
    validateMonitor(spec, ctrl.signal)
      .then((r) => {
        if (ctrl.signal.aborted) return;
        if (r.ok) {
          setValidation({ status: 'done', result: r.data, error: null, httpStatus: r.status });
          return;
        }
        setValidation({ status: 'done', result: null, error: r.error, httpStatus: r.status });
        const fe = serverErrors(r.error);
        if (Object.keys(fe).length > 0) {
          setErrors(fe);
          setStep(Math.min(...Object.keys(fe).map((k) => STEP_OF[k as keyof MonitorForm] ?? 0)));
        }
      })
      .catch(() => undefined);
    return () => ctrl.abort();
    // 단계에 들어올 때 한 번만 부른다(입력이 바뀌면 set이 결과를 지우고, 다시 들어오면 새로 부른다)
  }, [step]);

  const save = async () => {
    const { spec } = formToSpec(form);
    if (spec === null) return;
    setSaving({ busy: true, error: null, conflict: false, idemConflict: false });
    const signal = new AbortController().signal;
    const r = initial === null ? await createMonitor(spec, idempotencyKey.current, signal) : await updateMonitor(initial.id, spec, initial.revision, signal);
    if (r.ok) {
      void navigate({ pathname: orgPath(org, `monitors/${r.data.id}`), search });
      return;
    }
    if (r.status === 412) {
      setSaving({ busy: false, error: '다른 사람이 이 monitor를 먼저 바꿨습니다. 덮어쓰지 않았습니다.', conflict: true, idemConflict: false });
      return;
    }
    if (r.status === 409 && initial === null) {
      // 같은 key로 다른 내용이 이미 저장됐다: 앞선 저장이 응답만 잃고 성공했을 수 있다. 몰래 새 key로 다시 보내면 둘이 생긴다
      setSaving({
        busy: false,
        error: '이 화면에서 앞서 보낸 저장이 이미 처리됐을 수 있습니다(응답을 받지 못한 경우). 목록에서 먼저 확인하세요.',
        conflict: false,
        idemConflict: true,
      });
      return;
    }
    const fe = serverErrors(r.error);
    if (Object.keys(fe).length > 0) {
      setErrors(fe);
      setStep(Math.min(...Object.keys(fe).map((k) => STEP_OF[k as keyof MonitorForm] ?? 0)));
    }
    setSaving({ busy: false, error: r.error.message, conflict: false, idemConflict: false });
  };

  const title = initial === null ? '새 monitor' : `${initial.name} 편집`;
  return (
    <section className="mt-page" aria-labelledby="editor-title">
      <nav className="mt-breadcrumb" aria-label="위치">
        <Link to={{ pathname: orgPath(org, 'monitors'), search }}>Monitors</Link>
        {initial !== null && (
          <>
            {' / '}
            <Link to={{ pathname: orgPath(org, `monitors/${initial.id}`), search }}>{initial.name}</Link>
          </>
        )}
      </nav>
      <header className="mt-page__header">
        <h1 id="editor-title" className="mt-title">
          {title}
        </h1>
      </header>
      <ol className="mt-monitor-steps" aria-label="단계">
        {STEPS.map((s, i) => (
          <li key={s}>
            <button type="button" className="mt-monitor-step" aria-current={i === step ? 'step' : undefined} onClick={() => goTo(i)}>
              <span className="mt-monitor-step__n">{i + 1}</span> {s}
            </button>
          </li>
        ))}
      </ol>
      <form
        className="mt-card mt-monitor-form"
        onSubmit={(e) => {
          e.preventDefault();
          if (step < STEPS.length - 1) goTo(step + 1);
        }}
        noValidate
      >
        {step === 0 && <SignalStep form={form} set={set} errors={errors} />}
        {step === 1 && <QueryStep form={form} set={set} errors={errors} />}
        {step === 2 && <ConditionStep form={form} set={set} errors={errors} />}
        {step === 3 && <NoDataStep form={form} set={set} errors={errors} />}
        {step === 4 && <NotifyStep form={form} set={set} errors={errors} policies={policies} />}
        {step === 5 && (
          <PreviewStep validation={validation} kind={form.kind} timeZone={ctx.timeZone} />
        )}
        <div className="mt-monitor-actions mt-monitor-form__nav">
          {step > 0 && (
            <button type="button" className="mt-button" onClick={() => setStep(step - 1)}>
              이전
            </button>
          )}
          {step < STEPS.length - 1 ? (
            <button type="submit" className="mt-button mt-button--primary">
              다음
            </button>
          ) : (
            <button
              type="button"
              className="mt-button mt-button--primary"
              disabled={saving.busy || validation.status !== 'done' || validation.result === null}
              onClick={() => void save()}
            >
              {initial === null ? '저장' : '변경 저장'}
            </button>
          )}
          <Link className="mt-button" to={{ pathname: orgPath(org, initial === null ? 'monitors' : `monitors/${initial.id}`), search }}>
            취소
          </Link>
        </div>
        {saving.error !== null && (
          <div className="mt-notice mt-notice--critical" role="alert">
            <strong>저장하지 못했습니다</strong>
            <span>{saving.error}</span>
            {saving.conflict && initial !== null && (
              <Link to={{ pathname: orgPath(org, `monitors/${initial.id}`), search }}>최신 내용 보기</Link>
            )}
            {saving.idemConflict && (
              <span className="mt-monitor-actions">
                <Link to={{ pathname: orgPath(org, 'monitors'), search }}>monitor 목록 보기</Link>
                <button
                  type="button"
                  className="mt-button"
                  onClick={() => {
                    idempotencyKey.current = globalThis.crypto.randomUUID();
                    void save();
                  }}
                >
                  확인했고, 새 monitor로 저장
                </button>
              </span>
            )}
          </div>
        )}
      </form>
    </section>
  );
}

function serverErrors(e: ApiErrorBody): FormErrors {
  const out: FormErrors = {};
  const violations = (e.details as { field_violations?: { field: string; reason: string }[] } | undefined)?.field_violations ?? [];
  for (const v of violations) {
    const f = fieldForPath(v.field);
    if (f !== null && out[f] === undefined) out[f] = v.reason;
  }
  return out;
}

type StepProps = { form: MonitorForm; set: <K extends keyof MonitorForm>(k: K, v: MonitorForm[K]) => void; errors: FormErrors };

function Field({ id, label, error, hint, children }: { id: string; label: string; error?: string | undefined; hint?: string; children: ReactNode }) {
  return (
    <div className="mt-monitor-field">
      <label htmlFor={id}>{label}</label>
      {children}
      {hint !== undefined && (
        <span className="mt-label" id={`${id}-hint`}>
          {hint}
        </span>
      )}
      {error !== undefined && (
        <span className="mt-monitor-error" id={`${id}-error`} role="alert">
          {error}
        </span>
      )}
    </div>
  );
}

const describedBy = (id: string, error: string | undefined, hint: boolean) =>
  [hint ? `${id}-hint` : '', error !== undefined ? `${id}-error` : ''].filter((s) => s !== '').join(' ') || undefined;

function SignalStep({ form, set, errors }: StepProps) {
  const now = useMemo(() => Date.now(), []);
  const [q, setQ] = useState(form.metric);
  const metrics = useRemote(form.kind === 'metric' ? `metrics-dict|${q}` : null, 0, (signal) =>
    listMetrics({ from: new Date(now - 24 * 3600_000).toISOString(), to: new Date(now).toISOString(), q, limit: 50, cursor: null }, signal),
  );
  return (
    <fieldset className="mt-monitor-fieldset">
      <legend>1. 신호</legend>
      <Field id="m-name" label="이름" error={errors.name}>
        <input id="m-name" className="mt-input" value={form.name} onChange={(e) => set('name', e.target.value)} aria-invalid={errors.name !== undefined} aria-describedby={describedBy('m-name', errors.name, false)} maxLength={200} />
      </Field>
      <div className="mt-monitor-field" role="radiogroup" aria-labelledby="m-kind-label">
        <span id="m-kind-label">무엇을 볼까요</span>
        <label>
          <input type="radio" name="kind" checked={form.kind === 'error_ratio'} onChange={() => set('kind', 'error_ratio')} /> HTTP 오류율(5xx) — {DEFAULT_ERROR_METRIC}
        </label>
        <label>
          <input type="radio" name="kind" checked={form.kind === 'metric'} onChange={() => set('kind', 'metric')} /> metric 값(지연·사용량 등)
        </label>
      </div>
      {form.kind === 'metric' && (
        <>
          <Field id="m-metric" label="metric" error={errors.metric} hint="지난 24시간에 받은 metric에서 고릅니다">
            <input
              id="m-metric"
              className="mt-input mt-mono"
              list="m-metric-list"
              value={form.metric}
              onChange={(e) => {
                set('metric', e.target.value);
                setQ(e.target.value);
              }}
              aria-invalid={errors.metric !== undefined}
              aria-describedby={describedBy('m-metric', errors.metric, true)}
            />
            <datalist id="m-metric-list">
              {(metrics.data ?? []).map((d) => (
                <option key={d.name} value={d.name} />
              ))}
            </datalist>
          </Field>
          <Field id="m-agg" label="연산" error={errors.aggregation} hint="분위수는 histogram bucket을 합친 뒤 계산합니다(평균의 평균 아님)">
            <select id="m-agg" className="mt-select" value={form.aggregation} onChange={(e) => {
                const a = aggregationChoice(e.target.value);
                if (a !== null) set('aggregation', a);
              }} aria-describedby="m-agg-hint">
              {AGGREGATIONS.map((a) => (
                <option key={a} value={a}>
                  {a}
                </option>
              ))}
            </select>
          </Field>
        </>
      )}
    </fieldset>
  );
}

function QueryStep({ form, set, errors }: StepProps) {
  const [groupText, setGroupText] = useState(form.groupBy.join(', '));
  return (
    <fieldset className="mt-monitor-fieldset">
      <legend>2. 범위와 group</legend>
      <Field id="m-service" label="서비스(service.name)" error={errors.service} hint="비우면 모든 서비스">
        <input id="m-service" className="mt-input mt-mono" value={form.service} onChange={(e) => set('service', e.target.value)} aria-describedby={describedBy('m-service', errors.service, true)} />
      </Field>
      <Field id="m-env" label="환경(deployment.environment.name)" hint="비우면 모든 환경">
        <input id="m-env" className="mt-input mt-mono" value={form.environment} onChange={(e) => set('environment', e.target.value)} aria-describedby="m-env-hint" />
      </Field>
      {form.extraFilters.length > 0 && (
        <div className="mt-notice mt-notice--info" role="note">
          <strong>이 화면에서 바꿀 수 없는 범위 조건 {form.extraFilters.length}개 — 그대로 유지합니다</strong>
          <ul className="mt-mono">
            {form.extraFilters.map((f, i) => (
              <li key={i}>{describeFilter(f)}</li>
            ))}
          </ul>
        </div>
      )}
      <Field id="m-group" label="group으로 나누기" error={errors.groupBy} hint="label key를 쉼표로(예: service.name, http.route). group마다 따로 평가하고 알립니다. 최대 5개, group 1,000개까지">
        <input
          id="m-group"
          className="mt-input mt-mono"
          value={groupText}
          onChange={(e) => {
            setGroupText(e.target.value);
            set(
              'groupBy',
              e.target.value
                .split(',')
                .map((s) => s.trim())
                .filter((s) => s !== ''),
            );
          }}
          aria-invalid={errors.groupBy !== undefined}
          aria-describedby={describedBy('m-group', errors.groupBy, true)}
        />
      </Field>
    </fieldset>
  );
}

function ConditionStep({ form, set, errors }: StepProps) {
  const ratio = form.kind === 'error_ratio';
  return (
    <fieldset className="mt-monitor-fieldset">
      <legend>3. 조건과 window</legend>
      <div className="mt-monitor-inline">
        <Field id="m-op" label="조건">
          <select id="m-op" className="mt-select" value={form.operator} onChange={(e) => set('operator', e.target.value as MonitorForm['operator'])}>
            {(['gt', 'gte', 'lt', 'lte'] as const).map((o) => (
              <option key={o} value={o}>
                {OPERATOR_TEXT[o]}
              </option>
            ))}
          </select>
        </Field>
        <Field id="m-threshold" label={ratio ? '임계값(%)' : '임계값'} error={errors.threshold} hint={ratio ? '예: 2 → 오류율 2% 초과' : 'metric 단위 그대로(지연 histogram은 보통 초)'}>
          <input id="m-threshold" className="mt-input" inputMode="decimal" value={form.threshold} onChange={(e) => set('threshold', e.target.value)} aria-invalid={errors.threshold !== undefined} aria-describedby={describedBy('m-threshold', errors.threshold, true)} />
        </Field>
      </div>
      <div className="mt-monitor-inline">
        <Field id="m-window" label="window(분)" error={errors.windowMinutes} hint="이 길이의 값을 한 번에 봅니다. 평가 주기의 배수">
          <input id="m-window" className="mt-input" inputMode="numeric" value={form.windowMinutes} onChange={(e) => set('windowMinutes', e.target.value)} aria-invalid={errors.windowMinutes !== undefined} aria-describedby={describedBy('m-window', errors.windowMinutes, true)} />
        </Field>
        <Field id="m-eval" label="평가 주기" error={errors.evaluationSeconds}>
          <select id="m-eval" className="mt-select" value={form.evaluationSeconds} onChange={(e) => {
            const s = evaluationChoice(e.target.value);
            if (s !== null) set('evaluationSeconds', s);
          }}>
            {EVALUATION_CHOICES.map((s) => (
              <option key={s} value={s}>
                {s < 60 ? `${String(s)}초` : `${String(s / 60)}분`}
              </option>
            ))}
          </select>
        </Field>
        <Field id="m-for" label="유지(분)" error={errors.forMinutes} hint="이만큼 위반이 이어져야 경보. 0이면 한 번 위반으로 경보">
          <input id="m-for" className="mt-input" inputMode="decimal" value={form.forMinutes} onChange={(e) => set('forMinutes', e.target.value)} aria-invalid={errors.forMinutes !== undefined} aria-describedby={describedBy('m-for', errors.forMinutes, true)} />
        </Field>
      </div>
      {ratio && (
        <Field id="m-min" label="최소 요청 수" error={errors.minimumRequests} hint="window 안 요청이 이보다 적으면 판정하지 않고 '데이터 없음'(저트래픽 노이즈 방지)">
          <input id="m-min" className="mt-input" inputMode="numeric" value={form.minimumRequests} onChange={(e) => set('minimumRequests', e.target.value)} aria-invalid={errors.minimumRequests !== undefined} aria-describedby={describedBy('m-min', errors.minimumRequests, true)} />
        </Field>
      )}
    </fieldset>
  );
}

function NoDataStep({ form, set, errors }: StepProps) {
  return (
    <fieldset className="mt-monitor-fieldset">
      <legend>4. 데이터 없음과 복구</legend>
      <div className="mt-monitor-field" role="radiogroup" aria-labelledby="m-nodata-label">
        <span id="m-nodata-label">데이터가 없을 때</span>
        <label>
          <input type="radio" name="nodata" checked={form.noDataAction === 'no_data'} onChange={() => set('noDataAction', 'no_data')} /> "데이터 없음" 상태로 둔다(알림 없음)
        </label>
        <label>
          <input type="radio" name="nodata" checked={form.noDataAction === 'alert'} onChange={() => set('noDataAction', 'alert')} /> 오래 없으면 경보
        </label>
        <span className="mt-label">데이터가 없는 것을 정상으로 보지는 않습니다.</span>
      </div>
      {form.noDataAction === 'alert' && (
        <Field id="m-after" label="경보까지(분)" error={errors.noDataAfterMinutes} hint="window보다 길어야 합니다">
          <input id="m-after" className="mt-input" inputMode="decimal" value={form.noDataAfterMinutes} onChange={(e) => set('noDataAfterMinutes', e.target.value)} aria-invalid={errors.noDataAfterMinutes !== undefined} aria-describedby={describedBy('m-after', errors.noDataAfterMinutes, true)} />
        </Field>
      )}
      <p className="mt-label">복구: 조건이 2회 연속 정상이면 경보가 닫힙니다(고정).</p>
      <label className="mt-monitor-check">
        <input type="checkbox" checked={form.enabled} onChange={(e) => set('enabled', e.target.checked)} /> 저장하면 바로 평가 시작
      </label>
    </fieldset>
  );
}

function NotifyStep({ form, set, errors, policies }: StepProps & { policies: RemoteState<PolicyItem[]> }) {
  const unavailable = policies.error !== null && (policies.error.status === 404 || policies.error.status === 403);
  const loadFailed = policies.error !== null && !unavailable;
  // 지금 연결된 policy가 목록에 없으면(못 불러옴·권한·삭제 중) 따로 보여 선택이 몰래 "알림 없음"으로 바뀌지 않게 한다
  const unknownCurrent = form.policyId !== '' && !(policies.data ?? []).some((p) => p.id === form.policyId);
  return (
    <fieldset className="mt-monitor-fieldset">
      <legend>5. 알림</legend>
      {loadFailed && (
        <p className="mt-notice mt-notice--warning" role="status">
          알림 policy 목록을 불러오지 못했습니다({policies.error?.body.message}). 지금 연결된 policy는 그대로 유지됩니다.
        </p>
      )}
      {unavailable && !unknownCurrent ? (
        <p className="mt-label">알림 policy를 쓸 수 없습니다(권한이 없거나 이 배포에서 꺼져 있음). 알림 없이 저장할 수 있습니다.</p>
      ) : (
        <Field id="m-policy" label="알림 policy" error={errors.policyId} hint="경보가 열리고 닫힐 때 webhook으로 알립니다. policy는 조직 관리자가 만듭니다">
          <select id="m-policy" className="mt-select" value={form.policyId} onChange={(e) => set('policyId', e.target.value)} aria-describedby={describedBy('m-policy', errors.policyId, true)}>
            <option value="">알림 없음</option>
            {unknownCurrent && <option value={form.policyId}>지금 연결된 policy ({form.policyId.slice(0, 8)}…)</option>}
            {(policies.data ?? []).map((p) => (
              <option key={p.id} value={p.id}>
                {p.name} — {p.webhook.url}
                {p.enabled ? '' : ' (꺼짐)'}
              </option>
            ))}
          </select>
        </Field>
      )}
      {policies.nextCursor !== null && <p className="mt-label">policy가 많아 이름순 처음 200개만 고를 수 있습니다.</p>}
    </fieldset>
  );
}

function PreviewStep({
  validation,
  kind,
  timeZone,
}: {
  validation: { status: 'idle' | 'busy' | 'done'; result: ValidateResult | null; error: ApiErrorBody | null; httpStatus: number };
  kind: MonitorForm['kind'];
  timeZone: string;
}) {
  if (validation.status !== 'done') return <Skeleton height={200} label="정의를 검증하고 지난 24시간을 다시 평가하는 중" />;
  if (validation.error !== null) {
    return (
      <div className="mt-notice mt-notice--critical" role="alert">
        <strong>{validation.httpStatus === 422 ? '아직 지원하지 않는 설정입니다' : '정의를 확인하세요'}</strong>
        <span>{validation.error.message}</span>
      </div>
    );
  }
  const r = validation.result;
  if (r === null) return null;
  return (
    <div className="mt-monitor-preview">
      {r.warnings.length > 0 && (
        <ul className="mt-notice mt-notice--info" aria-label="경고">
          {r.warnings.map((w) => (
            <li key={w}>{WARNING_TEXT[w] ?? w}</li>
          ))}
        </ul>
      )}
      {r.dry_run === null ? <p className="mt-muted">미리보기 없이도 저장할 수 있습니다.</p> : <DryRunPreview dr={r.dry_run} kind={kind} timeZone={timeZone} />}
    </div>
  );
}
