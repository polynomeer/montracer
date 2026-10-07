import { Navigate, type RouteObject } from 'react-router';
import { OrgShell } from './AppShell.tsx';
import { screens } from './nav.ts';
import { NotFound, ScreenPlaceholder } from './screens.tsx';

// 인증 연동 전 개발용 기본 조직. 로그인 후에는 principal의 기본 조직으로 이동한다 (D05 §01).
export const DEV_DEFAULT_ORG = 'demo';

export const routes: RouteObject[] = [
  { path: '/', element: <Navigate to={`/o/${DEV_DEFAULT_ORG}/overview`} replace /> },
  {
    path: '/o/:org',
    element: <OrgShell />,
    children: [
      { index: true, element: <Navigate to="overview" replace /> },
      ...Object.values(screens).map((screen) => ({
        path: screen.path,
        element: <ScreenPlaceholder screen={screen} />,
      })),
      { path: '*', element: <NotFound /> },
    ],
  },
  { path: '*', element: <NotFound /> },
];
