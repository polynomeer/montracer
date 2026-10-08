import { Navigate, useParams, type RouteObject } from 'react-router';
import { OrgShell } from './AppShell.tsx';
import { screens } from './nav.ts';
import { NotFound, ScreenPlaceholder } from './screens.tsx';
import { LogExplorer } from '../features/logs/LogExplorer.tsx';
import { ServiceDetail } from '../features/services/ServiceDetail.tsx';
import { ServiceList } from '../features/services/ServiceList.tsx';
import { TraceDetail } from '../features/traces/TraceDetail.tsx';
import { TraceExplorer } from '../features/traces/TraceExplorer.tsx';

// 인증 연동 전 개발용 기본 조직. 로그인 후에는 principal의 기본 조직으로 이동한다 (D05 §01).
// dev server는 seed tenant 이름을 넣는다(vite.config.ts). 없으면 'demo'.
export const DEV_DEFAULT_ORG: string = (import.meta.env.VITE_DEV_ORG as string | undefined) ?? 'demo';

export const routes: RouteObject[] = [
  { path: '/', element: <Navigate to={`/o/${DEV_DEFAULT_ORG}/overview`} replace /> },
  {
    path: '/o/:org',
    element: <OrgShell />,
    children: [
      { index: true, element: <Navigate to="overview" replace /> },
      { path: screens.services.path, element: <ServiceList /> },
      // 서비스가 바뀌면 화면 상태(조회 결과)를 새로 시작한다
      { path: screens.serviceDetail.path, element: <ServiceDetailRoute /> },
      { path: screens.traces.path, element: <TraceExplorer /> },
      { path: screens.traceDetail.path, element: <TraceDetailRoute /> },
      { path: screens.logs.path, element: <LogExplorer /> },
      ...Object.values(screens)
        .filter((screen) => screen !== screens.services && screen !== screens.serviceDetail && screen !== screens.traces && screen !== screens.traceDetail && screen !== screens.logs)
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
