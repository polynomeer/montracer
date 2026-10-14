import { Navigate, useParams, type RouteObject } from 'react-router';
import { OrgShell } from './AppShell.tsx';
import { screens, type Screen } from './nav.ts';
import { NotFound, ScreenPlaceholder } from './screens.tsx';
import { DashboardEditor } from '../features/dashboards/DashboardEditor.tsx';
import { DashboardList } from '../features/dashboards/DashboardList.tsx';
import { DashboardView } from '../features/dashboards/DashboardView.tsx';
import { LogExplorer } from '../features/logs/LogExplorer.tsx';
import { MetricsExplorer } from '../features/metrics/MetricsExplorer.tsx';
import { MonitorDetail } from '../features/monitors/MonitorDetail.tsx';
import { MonitorEditor } from '../features/monitors/MonitorEditor.tsx';
import { MonitorList } from '../features/monitors/MonitorList.tsx';
import { PolicyCreate } from '../features/notifications/PolicyCreate.tsx';
import { PolicyDetail } from '../features/notifications/PolicyDetail.tsx';
import { PolicyList } from '../features/notifications/PolicyList.tsx';
import { Overview } from '../features/overview/Overview.tsx';
import { ServiceDetail } from '../features/services/ServiceDetail.tsx';
import { ServiceList } from '../features/services/ServiceList.tsx';
import { TraceDetail } from '../features/traces/TraceDetail.tsx';
import { TraceExplorer } from '../features/traces/TraceExplorer.tsx';

// 인증 연동 전 개발용 기본 조직. 로그인 후에는 principal의 기본 조직으로 이동한다 (D05 §01).
// dev server는 seed tenant 이름을 넣는다(vite.config.ts). 없으면 'demo'.
export const DEV_DEFAULT_ORG: string = (import.meta.env.VITE_DEV_ORG as string | undefined) ?? 'demo';

// 구현된 화면(나머지는 placeholder)
const IMPLEMENTED = new Set<Screen>([
  screens.overview,
  screens.services,
  screens.serviceDetail,
  screens.traces,
  screens.traceDetail,
  screens.logs,
  screens.metrics,
  screens.monitors,
  screens.monitorNew,
  screens.monitorDetail,
  screens.monitorEdit,
  screens.notifications,
  screens.notificationNew,
  screens.notificationDetail,
  screens.dashboards,
  screens.dashboardNew,
  screens.dashboardDetail,
  screens.dashboardEdit,
]);

export const routes: RouteObject[] = [
  { path: '/', element: <Navigate to={`/o/${DEV_DEFAULT_ORG}/overview`} replace /> },
  {
    path: '/o/:org',
    element: <OrgShell />,
    children: [
      { index: true, element: <Navigate to="overview" replace /> },
      { path: screens.overview.path, element: <Overview /> },
      { path: screens.services.path, element: <ServiceList /> },
      // 서비스가 바뀌면 화면 상태(조회 결과)를 새로 시작한다
      { path: screens.serviceDetail.path, element: <ServiceDetailRoute /> },
      { path: screens.traces.path, element: <TraceExplorer /> },
      { path: screens.traceDetail.path, element: <TraceDetailRoute /> },
      { path: screens.logs.path, element: <LogExplorer /> },
      { path: screens.metrics.path, element: <MetricsExplorer /> },
      { path: screens.monitors.path, element: <MonitorList /> },
      { path: screens.monitorNew.path, element: <MonitorEditor /> },
      // monitor가 바뀌면 화면 상태를 새로 시작한다
      { path: screens.monitorDetail.path, element: <MonitorDetailRoute /> },
      { path: screens.monitorEdit.path, element: <MonitorEditRoute /> },
      { path: screens.dashboards.path, element: <DashboardList /> },
      { path: screens.dashboardNew.path, element: <DashboardEditor /> },
      { path: screens.dashboardDetail.path, element: <DashboardDetailRoute /> },
      { path: screens.dashboardEdit.path, element: <DashboardEditRoute /> },
      { path: screens.notifications.path, element: <PolicyList /> },
      { path: screens.notificationNew.path, element: <PolicyCreate /> },
      { path: screens.notificationDetail.path, element: <PolicyDetailRoute /> },
      ...Object.values(screens)
        .filter((screen) => !IMPLEMENTED.has(screen))
        .map((screen) => ({
          path: screen.path,
          element: <ScreenPlaceholder screen={screen} />,
        })),
      { path: '*', element: <NotFound /> },
    ],
  },
  { path: '*', element: <NotFound /> },
];

function ServiceDetailRoute() {
  const { serviceId = '' } = useParams();
  return <ServiceDetail key={serviceId} />;
}

function TraceDetailRoute() {
  const { traceId = '' } = useParams();
  return <TraceDetail key={traceId} />;
}

function MonitorDetailRoute() {
  const { monitorId = '' } = useParams();
  return <MonitorDetail key={monitorId} />;
}

function MonitorEditRoute() {
  const { monitorId = '' } = useParams();
  return <MonitorEditor key={monitorId} />;
}

// policy가 바뀌면 화면 상태(시험 결과·표시 중인 secret)를 새로 시작한다
function PolicyDetailRoute() {
  const { policyId = '' } = useParams();
  return <PolicyDetail key={policyId} />;
}

// dashboard가 바뀌면 화면 상태(변수 선택지·조회·편집 중인 정의)를 새로 시작한다
function DashboardDetailRoute() {
  const { dashboardId = '' } = useParams();
  return <DashboardView key={dashboardId} />;
}

function DashboardEditRoute() {
  const { dashboardId = '' } = useParams();
  return <DashboardEditor key={dashboardId} />;
}
