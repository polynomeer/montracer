import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';
import { devApiProxy, resolveDevApiTarget } from './dev-proxy.ts';

const repoRoot = fileURLToPath(new URL('../..', import.meta.url));

export default defineConfig(({ command, mode, isPreview }) => {
  // dev server에서만. preview(빌드 결과 확인)와 시험(vitest)에는 붙이지 않는다.
  const api = command === 'serve' && mode !== 'test' && isPreview !== true ? resolveDevApiTarget(process.env, repoRoot) : null;
  if (api !== null) {
    // token 값은 출력하지 않는다
    console.info(`[montracer] /api → ${api.url} (key: ${api.token === null ? '없음' : 'seed'}, ${api.source})`);
  }
  return {
    plugins: [react()],
    // 개발용 기본 조직 slug: seed tenant 이름 (ADR 0041 결정 10, ADR 0042)
    define: { 'import.meta.env.VITE_DEV_ORG': JSON.stringify(api?.tenantName ?? 'demo') },
    server: { port: 15173, strictPort: true, ...(api === null ? {} : { proxy: devApiProxy(api) }) },
    test: {
      environment: 'jsdom',
      globals: true,
      setupFiles: ['./src/test/setup.ts'],
      css: false,
    },
  };
});
