// 선택 log 속성 패널 (D05 §03 EntityDrawer, §08 "attribute drawer").
// - 화면 옆 inline 패널이라 focus를 가두지 않는다. Esc·닫기로 닫고 결과 표의 그 행으로 focus를 돌려준다.
// - 선택은 URL `entity`(event_id)에 남는다. 불러온 결과에 없으면 추측하지 않고 그렇게 알린다.
// - 본문·속성은 텍스트로만 그린다(D05 §04). 서버가 준 field만 그린다(정제 전 값은 받지 않는다, D05 §08).
import { useEffect, useRef, type KeyboardEvent } from 'react';
import { Link } from 'react-router';
import type { LogItem } from '../../api/types.ts';
import { orgPath } from '../../app/nav.ts';
import { formatDateTime } from '../services/format.ts';
import { KeyValues } from '../traces/SpanDrawer.tsx';
import { isErrorSeverity, serviceLabel, severityLabel, traceLinkSearch, type ServiceOption } from './logFilters.ts';

export function LogDrawer({
  org,
  search,
  log,
  service,
  timeZone,
  loading,
  onClose,
}: {
  org: string;
  search: string;
  log: LogItem | null;
  service: ServiceOption | null;
  timeZone: string;
  loading: boolean;
  onClose: () => void;
}) {
  const closeRef = useRef<HTMLButtonElement>(null);
  // 행에서 열었을 때 패널로 focus를 옮긴다(공유 링크로 열면 옮기지 않는다 — 결과가 아직 없을 수 있다)
  const eventId = log?.event_id ?? null;
  useEffect(() => {
    if (eventId !== null && document.activeElement?.id === `log-row-${eventId}`) closeRef.current?.focus();
  }, [eventId]);
  const onKey = (e: KeyboardEvent<HTMLElement>) => {
    if (e.key === 'Escape') {
      e.preventDefault();
      onClose();
    }
  };
  return (
    <aside className="mt-card mt-span-drawer mt-log-drawer" aria-labelledby="log-drawer-title" onKeyDown={onKey}>
      <div className="mt-span-drawer__head">
        <div className="mt-span-drawer__title">
          <h2 id="log-drawer-title" className="mt-card__title">
            log 상세
          </h2>
          <button ref={closeRef} type="button" className="mt-icon-button" aria-label="log 상세 닫기 (Esc)" onClick={onClose}>
            ✕
          </button>
        </div>
        {log !== null && (
          <div className="mt-svc-meta">
            <span className="mt-mono">{formatDateTime(log.time, timeZone)}</span>
            <span className={isErrorSeverity(log.severity_number) ? 'mt-log-sev mt-log-sev--err' : 'mt-log-sev'}>
              {severityLabel(log.severity_number)} ({log.severity_number})
            </span>
            <span className="mt-mono">{service === null ? log.service_id : serviceLabel(service)}</span>
          </div>
        )}
      </div>
      <div className="mt-span-drawer__body">
        {log === null ? (
          loading ? null : (
            <p className="mt-muted">불러온 결과에 이 log가 없습니다. 시간 범위나 조건이 바뀌었을 수 있습니다.</p>
          )
        ) : (
          <>
            <h3 className="mt-label mt-kv__title">본문</h3>
            <pre className="mt-pre mt-log-body">{log.body === '' ? '(본문 없음)' : log.body}</pre>
            <dl className="mt-kv">
              <div className="mt-kv__row">
                <dt className="mt-mono">trace_id</dt>
                <dd className="mt-mono">
                  {log.trace_id === null ? (
                    <span className="mt-muted">없음 (trace와 연결되지 않은 log)</span>
                  ) : (
                    <Link to={{ pathname: orgPath(org, `traces/${log.trace_id}`), search: traceLinkSearch(search, log, Date.now()) }}>{log.trace_id}</Link>
                  )}
                </dd>
              </div>
              <div className="mt-kv__row">
                <dt className="mt-mono">span_id</dt>
                <dd className="mt-mono">{log.span_id ?? <span className="mt-muted">없음</span>}</dd>
              </div>
              <div className="mt-kv__row">
                <dt className="mt-mono">service_id</dt>
                <dd className="mt-mono">{log.service_id}</dd>
              </div>
              <div className="mt-kv__row">
                <dt className="mt-mono">event_id</dt>
                <dd className="mt-mono">{log.event_id}</dd>
              </div>
            </dl>
            <KeyValues title="속성" values={log.attributes} />
          </>
        )}
      </div>
    </aside>
  );
}
