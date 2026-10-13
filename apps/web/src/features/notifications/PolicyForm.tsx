// 이름·webhook 주소·켜기 입력. 주소는 저장 뒤 host만 보이므로, 수정에서는 비워 두면 저장된 주소를 그대로 둔다.
import { useState, type FormEvent } from 'react';
import { validatePolicyInput } from './policyText.ts';

export interface PolicyFormValue {
  name: string;
  url: string;
  enabled: boolean;
}

export function PolicyForm({
  initial,
  currentHost,
  busy,
  serverErrors,
  submitLabel,
  onSubmit,
}: {
  initial: PolicyFormValue;
  /** 수정이면 지금 목적지(host). 생성이면 null */
  currentHost: string | null;
  busy: boolean;
  serverErrors: { name?: string; url?: string };
  submitLabel: string;
  onSubmit: (v: PolicyFormValue) => void;
}) {
  const [v, setV] = useState(initial);
  const [errors, setErrors] = useState<{ name?: string; url?: string }>({});
  const editing = currentHost !== null;
  const shown = { name: errors.name ?? serverErrors.name, url: errors.url ?? serverErrors.url };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const found = validatePolicyInput(v.name, v.url, !editing);
    setErrors(found);
    if (Object.keys(found).length === 0) onSubmit({ ...v, name: v.name.trim(), url: v.url.trim() });
  };
  return (
    <form className="mt-monitor-form" onSubmit={submit} noValidate>
      <fieldset className="mt-monitor-fieldset" disabled={busy}>
        <div className="mt-monitor-field">
          <label htmlFor="policy-name">이름</label>
          <input
            id="policy-name"
            className="mt-input"
            value={v.name}
            onChange={(e) => setV({ ...v, name: e.target.value })}
            aria-invalid={shown.name !== undefined}
            aria-describedby={shown.name !== undefined ? 'policy-name-error' : undefined}
          />
          {shown.name !== undefined && (
            <span id="policy-name-error" className="mt-monitor-error" role="alert">
              {shown.name}
            </span>
          )}
        </div>
        <div className="mt-monitor-field">
          <label htmlFor="policy-url">{editing ? '새 webhook 주소(바꿀 때만)' : 'webhook 주소'}</label>
          <input
            id="policy-url"
            className="mt-input mt-mono"
            type="url"
            inputMode="url"
            autoComplete="off"
            spellCheck={false}
            placeholder="https://hooks.example.com/…"
            value={v.url}
            onChange={(e) => setV({ ...v, url: e.target.value })}
            aria-invalid={shown.url !== undefined}
            aria-describedby={['policy-url-hint', shown.url !== undefined ? 'policy-url-error' : ''].filter((s) => s !== '').join(' ')}
          />
          <span id="policy-url-hint" className="mt-label">
            {editing
              ? `지금 목적지: ${currentHost ?? ''} — 전체 주소는 다시 보이지 않습니다. 비워 두면 그대로 둡니다.`
              : 'https만 받습니다. 주소의 token은 암호화해 저장하고, 저장 뒤에는 host만 보입니다. 사설·내부 주소로는 보내지 않습니다.'}
          </span>
          {shown.url !== undefined && (
            <span id="policy-url-error" className="mt-monitor-error" role="alert">
              {shown.url}
            </span>
          )}
        </div>
        <label className="mt-monitor-check">
          <input type="checkbox" checked={v.enabled} onChange={(e) => setV({ ...v, enabled: e.target.checked })} />
          켜기 (끄면 경보 알림을 보내지 않습니다. 시험 전송은 됩니다)
        </label>
      </fieldset>
      <div className="mt-monitor-form__nav mt-monitor-actions">
        <button type="submit" className="mt-button mt-button--primary" disabled={busy}>
          {busy ? '저장 중…' : submitLabel}
        </button>
      </div>
    </form>
  );
}

/** 서버 field violation(name, webhook.url) → form 필드 */
export function policyFieldErrors(details: unknown): { name?: string; url?: string } {
  const vs = (details as { field_violations?: { field: string; reason: string }[] } | undefined)?.field_violations ?? [];
  const out: { name?: string; url?: string } = {};
  for (const v of vs) {
    if (v.field === 'name') out.name ??= v.reason;
    if (v.field === 'webhook.url') out.url ??= v.reason;
  }
  return out;
}
