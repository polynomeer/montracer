// 알림 policy 만들기 (ADR 0056). 저장하면 서명 secret을 한 번만 보이고, 확인한 뒤 상세로 간다.
// Idempotency-Key는 이 화면에 하나다: 응답을 잃고 다시 눌러도 policy가 둘 생기지 않는다(재생 응답에는 secret이 없다).
import { useRef, useState } from 'react';
import { Link, useNavigate, useParams, useSearchParams } from 'react-router';
import { createPolicy, type PolicyWithSecret } from '../../api/notificationPolicies.ts';
import { contextOnly } from '../../app/context.ts';
import { orgPath } from '../../app/nav.ts';
import '../services/services.css';
import '../monitors/monitors.css';
import { PolicyForm, policyFieldErrors, type PolicyFormValue } from './PolicyForm.tsx';
import { SecretReveal } from './SecretReveal.tsx';
import './notifications.css';

export function PolicyCreate() {
  const { org = '' } = useParams();
  const [params] = useSearchParams();
  const search = contextOnly(params);
  const navigate = useNavigate();
  const idempotencyKey = useRef(globalThis.crypto.randomUUID());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [fieldErrors, setFieldErrors] = useState<{ name?: string; url?: string }>({});
  const [created, setCreated] = useState<PolicyWithSecret | null>(null);

  const submit = async (v: PolicyFormValue) => {
    setBusy(true);
    setError(null);
    setFieldErrors({});
    const r = await createPolicy({ name: v.name, url: v.url, enabled: v.enabled }, idempotencyKey.current, new AbortController().signal);
    setBusy(false);
    if (r.ok) {
      setCreated(r.data);
      return;
    }
    setFieldErrors(policyFieldErrors(r.error.details));
    setError(
      r.status === 403
        ? 'Tenant Admin만 알림 policy를 만들 수 있습니다.'
        : r.status === 409
          ? '이 화면에서 앞서 보낸 저장과 내용이 다릅니다. 목록에서 먼저 확인하세요.'
          : r.error.message,
    );
  };
  const detail = (id: string) => void navigate({ pathname: orgPath(org, `notifications/${id}`), search });

  return (
    <section className="mt-page" aria-labelledby="policy-new-title">
      <nav className="mt-breadcrumb" aria-label="위치">
        <Link to={{ pathname: orgPath(org, 'notifications'), search }}>Notifications</Link>
      </nav>
      <header className="mt-page__header">
        <h1 id="policy-new-title" className="mt-title">
          새 알림 policy
        </h1>
      </header>
      {created === null ? (
        <>
          <PolicyForm initial={{ name: '', url: '', enabled: true }} currentHost={null} busy={busy} serverErrors={fieldErrors} submitLabel="만들기" onSubmit={(v) => void submit(v)} />
          {error !== null && (
            <div className="mt-notice mt-notice--critical" role="alert">
              <strong>만들지 못했습니다</strong>
              <span>{error}</span>
            </div>
          )}
        </>
      ) : created.signingSecret !== null ? (
        <SecretReveal secret={created.signingSecret} keyId={created.data.webhook.signing_key_id} doneLabel="저장했습니다 — policy 보기" onDone={() => detail(created.data.id)} />
      ) : (
        // 같은 요청의 재생 응답: secret은 다시 보이지 않는다(서버가 평문을 저장하지 않는다)
        <div className="mt-notice mt-notice--warning" role="alert">
          <strong>이미 만들어진 policy입니다</strong>
          <span>앞선 요청의 응답을 받지 못했던 것 같습니다. 서명 secret은 다시 볼 수 없으니, policy 화면에서 secret을 교체하세요.</span>
          <div>
            <button type="button" className="mt-button" onClick={() => detail(created.data.id)}>
              policy 보기
            </button>
          </div>
        </div>
      )}
    </section>
  );
}
