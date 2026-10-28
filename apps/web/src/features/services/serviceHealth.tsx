// 서비스의 monitor 판정 (S01·S02·S03, ADR 0073). 서버 응답 행을 서비스 키(namespace·이름·환경)에 맞춘다.
// - 이름만 걸린 행(namespace null)과 전체 환경 행(environment null)은 같은 이름·모든 환경의 서비스에 함께 적용한다(ADR 0073 §4).
// - 걸린 monitor가 없으면 "판정 없음". 응답이 잘렸거나(partial·경고) 조회하지 못했으면 "확인할 수 없음" — 정상으로 단정하지 않는다(계약 6).
// - 멈춘 정상은 정상 색이 아니다(stateBadge). 시작 시각을 모르면 시각을 보이지 않는다(지어내지 않는다).
import type { ServiceHealthData, ServiceHealthItem, ServiceHealthMonitor } from '../../api/serviceHealth.ts';
import type { RemoteState } from '../../api/useRemote.ts';
import { STATE_BADGE, STATE_LABEL, stateBadge, stateRank, type ViewState } from '../monitors/monitorSpec.ts';
import type { BadgeIcon, BadgeVariant, Tone } from './states.tsx';
import { formatClock } from './format.ts';
import { StatusBadge } from './states.tsx';

export interface ServiceKey {
  namespace: string | null;
  name: string;
  environment: string | null;
}

export type Verdict =
  | {
      kind: 'state';
      state: string;
      stale: boolean;
      since: string | null;
      monitors: ServiceHealthMonitor[];
      monitorCount: number;
      /** 여러 행(정확·이름만·전체 환경)을 합쳤으면 겹침을 몰라 수가 최소값이다 */
      countIsMinimum: boolean;
      allEnvironments: boolean;
      /** 잘린 응답의 하한 상태(경보·평가 오류·데이터 없음). 시작 시각은 보이지 않는다 */
      partial: boolean;
    }
  | { kind: 'none' }
  | { kind: 'unknown'; reason: 'partial' | 'failed' | 'forbidden' | 'loading'; lastSuccessMs?: number | null };

const isKnown = (s: string): s is ViewState => s in STATE_BADGE;

/** 나쁜 순서. 모르는 상태는 가장 나쁘게 본다(새 상태가 정상처럼 보이지 않게). */
export function healthRank(state: string, stale: boolean): number {
  return isKnown(state) ? stateRank(state, stale) : -1;
}

/** 진행 중·정상 계열: 잘린 응답에서는 실제보다 덜 나쁠 수 있다. 경보·평가 오류·데이터 없음은 하한이라 그대로 믿는다. */
const LOWER_BOUND_SAFE = new Set(['ALERT', 'EVALUATION_ERROR', 'NO_DATA']);

function matches(item: ServiceHealthItem, key: ServiceKey): boolean {
  if (item.service_name !== key.name) return false;
  if (item.service_namespace !== null && key.namespace !== null && item.service_namespace !== key.namespace) return false;
  if (item.environment !== null && key.environment !== null && item.environment !== key.environment) return false;
  return true;
}

/** 서비스 하나의 판정. data는 성공 응답, warnings는 meta.warnings. */
export function verdictFor(key: ServiceKey, data: ServiceHealthData, warnings: readonly string[]): Verdict {
  const rows = data.services.filter((s) => matches(s, key));
  const truncated = warnings.length > 0;
  if (rows.length === 0) return truncated ? { kind: 'unknown', reason: 'partial' } : { kind: 'none' };
  let worst = rows[0] as ServiceHealthItem;
  let rank = healthRank(worst.state, worst.stale);
  for (const r of rows.slice(1)) {
    const rr = healthRank(r.state, r.stale);
    if (rr < rank) {
      worst = r;
      rank = rr;
    }
  }
  const partial = truncated || rows.some((r) => r.partial);
  if (partial && !LOWER_BOUND_SAFE.has(worst.state)) return { kind: 'unknown', reason: 'partial' };
  // 같은 상태의 가장 이른 시작 시각. 하나라도 모르거나 잘린 응답이면(빠진 monitor가 더 일찍 시작했을 수 있다) 모름
  const same = rows.filter((r) => healthRank(r.state, r.stale) === rank);
  let since: string | null = null;
  if (!partial && same.every((r) => r.state_since !== null)) {
    for (const r of same) if (since === null || Date.parse(r.state_since as string) < Date.parse(since)) since = r.state_since;
  }
  const monitors = new Map<string, ServiceHealthMonitor>();
  for (const r of rows) {
    for (const m of r.monitors) {
      const prev = monitors.get(m.id);
      if (prev === undefined || healthRank(m.state, m.stale) < healthRank(prev.state, prev.stale)) monitors.set(m.id, m);
    }
  }
  const sorted = [...monitors.values()].sort((a, b) => healthRank(a.state, a.stale) - healthRank(b.state, b.stale) || a.name.localeCompare(b.name));
  return {
    kind: 'state',
    state: worst.state,
    stale: rows.some((r) => r.stale),
    since,
    monitors: sorted.slice(0, 5),
    monitorCount: Math.max(...rows.map((r) => r.monitor_count), monitors.size),
    countIsMinimum: rows.length > 1,
    allEnvironments: rows.some((r) => r.environment === null || r.includes_all_environment_monitors),
    partial,
  };
}

/** 응답 상태로 서비스마다 판정한다. 404(기능 꺼짐)는 null — 화면은 판정 열 자체를 보이지 않는다. */
export function healthVerdicts(remote: RemoteState<ServiceHealthData>): ((key: ServiceKey) => Verdict) | null {
  if (remote.error?.status === 404) return null;
  const data = remote.data;
  if (data === null) {
    const reason = remote.error === null ? 'loading' : remote.error.status === 403 ? 'forbidden' : 'failed';
    return () => ({ kind: 'unknown', reason });
  }
  // 갱신이 실패하면 지난 값을 지금 상태처럼 보이지 않는다(마지막 성공 시각은 알린다)
  if (remote.error !== null) {
    const reason = remote.error.status === 403 ? 'forbidden' : 'failed';
    return () => ({ kind: 'unknown', reason, lastSuccessMs: remote.lastSuccessMs });
  }
  const warnings = remote.meta?.warnings ?? [];
  return (key) => verdictFor(key, data, warnings);
}

const UNKNOWN_TEXT: Record<Extract<Verdict, { kind: 'unknown' }>['reason'], string> = {
  partial: '확인할 수 없음 · 일부 monitor만 반영',
  failed: '확인할 수 없음 · 상태 조회 실패',
  forbidden: '확인할 수 없음 · 상태를 볼 권한 없음',
  loading: '불러오는 중',
};

/** 판정의 글(배지·접근 이름). compact면 시각만 남기고 설명을 줄인다. */
export function verdictText(verdict: Verdict, timeZone: string, compact = false): string {
  if (verdict.kind === 'none') return compact ? '판정 없음' : '판정 없음 · 걸린 monitor 없음';
  if (verdict.kind === 'unknown') {
    if (verdict.reason === 'loading') return '상태 불러오는 중';
    if (compact) return '확인할 수 없음';
    const last = verdict.lastSuccessMs == null ? '' : ` (마지막 성공 ${formatClock(verdict.lastSuccessMs, timeZone)})`;
    return UNKNOWN_TEXT[verdict.reason] + last;
  }
  const parts = [isKnown(verdict.state) ? STATE_LABEL[verdict.state] : verdict.state];
  // 멈춘 monitor가 있으면 어떤 상태든 지금 값이 아닐 수 있다
  if (verdict.stale) parts.push('갱신 멈춤');
  if (verdict.partial) parts.push('일부 monitor만 반영');
  if (verdict.since !== null) parts.push(`${formatClock(Date.parse(verdict.since), timeZone)}부터`);
  if (!compact && verdict.allEnvironments) parts.push('전체 환경 monitor 포함');
  return parts.join(' · ');
}

/** 판정의 배지 모양. 판정 없음·확인할 수 없음은 중립이고, 멈춘 정상은 정상 색이 아니다. */
export function verdictBadge(verdict: Verdict): { tone: Tone; icon: BadgeIcon; variant: BadgeVariant } {
  if (verdict.kind !== 'state' || !isKnown(verdict.state)) return { tone: 'neutral', icon: 'minus', variant: 'default' };
  return stateBadge(verdict.state, verdict.stale);
}

/** 판정 배지. compact면 시각·monitor 수를 줄인다(표의 칸). */
export function VerdictBadge({ verdict, timeZone, compact = false }: { verdict: Verdict; timeZone: string; compact?: boolean }) {
  if (verdict.kind === 'unknown' && verdict.reason === 'loading') {
    return (
      <span className="mt-muted">
        <span aria-hidden="true">…</span>
        <span className="mt-visually-hidden">상태 불러오는 중</span>
      </span>
    );
  }
  // 칸(compact)에는 줄인 글을, 이유·전체 환경 표시는 title로 남긴다
  const full = verdictText(verdict, timeZone);
  const b = verdictBadge(verdict);
  return (
    <span title={compact ? full : undefined}>
      <StatusBadge tone={b.tone} icon={b.icon} variant={b.variant}>
        {verdictText(verdict, timeZone, compact)}
      </StatusBadge>
    </span>
  );
}
