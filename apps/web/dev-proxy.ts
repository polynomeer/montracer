// 로컬 개발 전용 API proxy (ADR 0042). `pnpm dev`일 때만 쓴다.
// - 브라우저의 /api 요청을 query-api(기본 127.0.0.1:18080)로 넘기고, 서버 쪽에서 seed API key를 Authorization에 붙인다.
//   key는 브라우저로 가지 않는다(번들·localStorage·응답 어디에도 없음). CORS도 필요 없다(같은 origin).
// - 브라우저가 보낸 Authorization·Cookie는 지우고 proxy가 정한 key만 쓴다.
// - 이 컴퓨터(loopback)에서 온 같은 사이트 요청만 넘긴다. `vite --host`로 열어도 다른 기기·다른 사이트가 seed key로 조회하지 못한다(404).
// - `vite preview`에는 붙이지 않는다(vite.config.ts).
// - 운영 인증(session cookie + CSRF, D02 §12)을 대신하지 않는다. build 결과물에는 이 코드가 들어가지 않는다.
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { IncomingMessage } from 'node:http';
import type { ProxyOptions } from 'vite';

export interface DevApiTarget {
  url: string;
  token: string | null;
  tenantName: string | null;
  source: string;
}

interface SeedState {
  tenants?: { name?: unknown; api_token?: unknown }[];
}

/**
 * 우선순위: MONTRACER_DEV_API_TOKEN → seed 상태 파일(make seed)의 tenant(MONTRACER_DEV_TENANT, 기본 첫 tenant).
 * seed 파일은 MONTRACER_DEV_SEED_FILE 또는 레포 루트 `.seed/demo.json`이다.
 */
export function resolveDevApiTarget(env: NodeJS.ProcessEnv, repoRoot: string): DevApiTarget {
  const url = env.MONTRACER_DEV_QUERY_URL ?? 'http://127.0.0.1:18080';
  const explicit = env.MONTRACER_DEV_API_TOKEN;
  if (explicit !== undefined && explicit !== '') {
    return { url, token: explicit, tenantName: env.MONTRACER_DEV_TENANT ?? null, source: 'MONTRACER_DEV_API_TOKEN' };
  }
  const file = env.MONTRACER_DEV_SEED_FILE ?? resolve(repoRoot, '.seed/demo.json');
  let state: SeedState;
  try {
    state = JSON.parse(readFileSync(file, 'utf8')) as SeedState;
  } catch {
    return { url, token: null, tenantName: null, source: `${file} 없음 (make seed)` };
  }
  const tenants = Array.isArray(state.tenants) ? state.tenants : [];
  const want = env.MONTRACER_DEV_TENANT;
  const t = tenants.find((x) => want === undefined || x.name === want);
  if (t === undefined || typeof t.api_token !== 'string' || typeof t.name !== 'string') {
    return { url, token: null, tenantName: null, source: `${file}에 tenant ${want ?? ''} 없음` };
  }
  return { url, token: t.api_token, tenantName: t.name, source: file };
}

const LOOPBACK = new Set(['127.0.0.1', '::1', '::ffff:127.0.0.1']);

/** 이 컴퓨터에서 온, 다른 사이트가 시키지 않은 요청인가. */
export function isLocalSameSite(req: Pick<IncomingMessage, 'headers'> & { socket: { remoteAddress?: string | undefined } }): boolean {
  if (!LOOPBACK.has(req.socket.remoteAddress ?? '')) return false;
  return req.headers['sec-fetch-site'] !== 'cross-site';
}

export function devApiProxy(target: DevApiTarget): Record<string, ProxyOptions> {
  return {
    '/api': {
      target: target.url,
      changeOrigin: true,
      // false면 vite가 proxy하지 않고 404를 돌려준다
      bypass: (req) => (isLocalSameSite(req) ? undefined : false),
      configure(proxy) {
        proxy.on('proxyReq', (req) => {
          req.removeHeader('cookie');
          req.removeHeader('authorization');
          if (target.token !== null) req.setHeader('authorization', `Bearer ${target.token}`);
        });
      },
    },
  };
}
