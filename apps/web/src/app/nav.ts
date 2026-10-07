// 정보 구조와 화면 목록 (D05 §01 정보 구조, §04 화면 목록과 라우트).
// 경로는 조직 기준 상대 경로다. 실제 URL은 /o/{org}/{path}.

// experience: G2·G3 계약 기능 (D05 §01). admin: 별도 관리 권한 (D05 §01 Admin).
// 표시 정책일 뿐 보안 경계가 아니다. 서버가 요청마다 다시 인가한다 (D05 §04).
export type Entitlement = 'experience' | 'admin';

export interface Screen {
  /** D05 §04 화면 ID. 화면이 없는 메뉴는 null. */
  id: string | null;
  title: string;
  /** 조직 기준 상대 경로. */
  path: string;
  /** 관련 명세 절 (placeholder 화면에 표시). */
  spec: string;
}

export interface NavItem {
  label: string;
  screen: Screen;
}

export interface NavGroup {
  name: string;
  items: readonly NavItem[];
  /** 이 권한이 없으면 그룹 전체를 숨긴다 (D05 §01: 미구매 기능은 별도 탐색 영역). */
  requires?: Entitlement;
}

const s = (id: string | null, title: string, path: string, spec: string): Screen => ({ id, title, path, spec });

export const screens = {
  overview: s('S01', 'Overview', 'overview', 'D05 §04'),
  services: s('S02', 'Services', 'services', 'D05 §05'),
  serviceDetail: s('S02', '서비스 상세', 'services/:serviceId', 'D05 §05'),
  map: s('S03', 'Service Map', 'map', 'D05 §07'),
  infrastructure: s(null, 'Infrastructure', 'infrastructure', 'D05 §01'),
  traces: s('S04', 'Trace Explorer', 'traces', 'D05 §06'),
  traceDetail: s('S05', 'Trace 상세', 'traces/:traceId', 'D05 §06'),
  logs: s('S08', 'Logs', 'logs', 'D05 §08'),
  metrics: s('S08', 'Metrics', 'metrics', 'D05 §08'),
  errors: s('S07', 'Errors', 'errors', 'D05 §08'),
  errorDetail: s('S07', '오류 상세', 'errors/:errorId', 'D05 §08'),
  profiles: s('S11', 'Profiles', 'profiles', 'D05 §08'),
  databases: s('S11', 'Databases', 'databases', 'D05 §08'),
  rum: s('S12', 'RUM', 'rum', 'D05 §08'),
  replays: s('S12', 'Replays', 'replays', 'D05 §08'),
  synthetics: s('S12', 'Synthetics', 'synthetics', 'D05 §08'),
  monitors: s('S09', 'Monitors', 'monitors', 'D05 §09'),
  monitorDetail: s('S09', 'Monitor', 'monitors/:monitorId', 'D05 §09'),
  slos: s('S09', 'SLOs', 'slos', 'D05 §09'),
  incidents: s('S09', 'Incidents', 'incidents', 'D05 §09'),
  dashboards: s('S10', 'Dashboards', 'dashboards', 'D05 §10'),
  dashboardDetail: s('S10', 'Dashboard', 'dashboards/:dashboardId', 'D05 §10'),
  setup: s('S13', '설치', 'setup', 'D05 §10'),
  agents: s('S13', 'Agents', 'agents', 'D05 §10'),
  agentDetail: s('S06', 'JVM Inspector', 'agents/:agentId', 'D05 §07'),
  team: s(null, 'Team', 'settings/team', 'D05 §01'),
  access: s(null, 'Access', 'settings/access', 'D04 §01'),
  dataControls: s('S14', 'Data Controls', 'settings/data', 'D05 §10'),
  usage: s('S14', 'Usage', 'settings/usage', 'D05 §10'),
  audit: s(null, 'Audit', 'settings/audit', 'D04 §12'),
} as const satisfies Record<string, Screen>;

const item = (label: string, screen: Screen): NavItem => ({ label, screen });

export const navGroups: readonly NavGroup[] = [
  {
    name: 'Observe',
    items: [
      item('Overview', screens.overview),
      item('Services', screens.services),
      item('Service Map', screens.map),
      item('Infrastructure', screens.infrastructure),
    ],
  },
  {
    name: 'Investigate',
    items: [
      item('Traces', screens.traces),
      item('Logs', screens.logs),
      item('Metrics', screens.metrics),
      item('Errors', screens.errors),
      item('Profiles', screens.profiles),
      item('Databases', screens.databases),
    ],
  },
  {
    name: 'Experience',
    requires: 'experience',
    items: [item('RUM', screens.rum), item('Replays', screens.replays), item('Synthetics', screens.synthetics)],
  },
  {
    name: 'Respond',
    items: [item('Monitors', screens.monitors), item('SLOs', screens.slos), item('Incidents', screens.incidents)],
  },
  {
    name: 'Organize',
    items: [item('Dashboards', screens.dashboards), item('Integrations', screens.setup), item('Agents', screens.agents)],
  },
  {
    name: 'Admin',
    requires: 'admin',
    items: [
      item('Team', screens.team),
      item('Access', screens.access),
      item('Data Controls', screens.dataControls),
      item('Usage', screens.usage),
      item('Audit', screens.audit),
    ],
  },
];

export function visibleGroups(entitlements: ReadonlySet<Entitlement>): NavGroup[] {
  return navGroups.filter((g) => g.requires === undefined || entitlements.has(g.requires));
}

export function orgPath(org: string, path: string): string {
  return `/o/${encodeURIComponent(org)}/${path}`;
}
