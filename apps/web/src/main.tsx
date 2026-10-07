import '@montracer/design-tokens/tokens.css';
import 'pretendard/dist/web/variable/pretendardvariable-dynamic-subset.css';
import './app/shell.css';

import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { RouterProvider, createBrowserRouter } from 'react-router';
import { routes } from './app/routes.tsx';
import { applyThemePreference, loadThemePreference } from './app/theme.ts';

// 첫 그림 전에 테마를 적용해 깜빡임을 줄인다.
applyThemePreference(loadThemePreference());

const root = document.getElementById('root');
if (root === null) throw new Error('#root 요소가 없습니다');

createRoot(root).render(
  <StrictMode>
    <RouterProvider router={createBrowserRouter(routes)} />
  </StrictMode>,
);
