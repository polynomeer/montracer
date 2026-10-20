import { EventEmitter } from 'node:events';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { CONTROL_PATHS, devApiProxy, devAuthMode, isLocalSameSite, resolveDevApiTarget } from './dev-proxy.ts';

function seedFile(content: unknown): string {
  const dir = mkdtempSync(join(tmpdir(), 'mt-seed-'));
  const f = join(dir, 'demo.json');
  writeFileSync(f, JSON.stringify(content));
  return f;
}

const seed = { tenants: [{ name: 'acme', api_token: 'tok-acme' }, { name: 'globex', api_token: 'tok-globex' }] };

describe('resolveDevApiTarget', () => {
  it('seed 파일의 첫 tenant key, MONTRACER_DEV_TENANT로 고른다', () => {
    const f = seedFile(seed);
    expect(resolveDevApiTarget({ MONTRACER_DEV_SEED_FILE: f }, '/')).toMatchObject({ token: 'tok-acme', tenantName: 'acme', url: 'http://127.0.0.1:18080', controlUrl: 'http://127.0.0.1:18081' });
    expect(resolveDevApiTarget({ MONTRACER_DEV_SEED_FILE: f, MONTRACER_DEV_TENANT: 'globex' }, '/')).toMatchObject({ token: 'tok-globex', tenantName: 'globex' });
  });

  it('명시한 token이 우선, 없는 tenant·파일은 key 없음', () => {
    expect(resolveDevApiTarget({ MONTRACER_DEV_API_TOKEN: 'explicit', MONTRACER_DEV_QUERY_URL: 'http://127.0.0.1:9' }, '/')).toMatchObject({
      token: 'explicit',
      url: 'http://127.0.0.1:9',
    });
    expect(resolveDevApiTarget({ MONTRACER_DEV_SEED_FILE: seedFile(seed), MONTRACER_DEV_TENANT: 'nope' }, '/').token).toBeNull();
    expect(resolveDevApiTarget({ MONTRACER_DEV_SEED_FILE: '/no/such/file.json' }, '/').token).toBeNull();
  });
});

describe('isLocalSameSite', () => {
  const req = (remoteAddress: string, site?: string) => ({ socket: { remoteAddress }, headers: site === undefined ? {} : { 'sec-fetch-site': site } });
  it('loopback의 같은 사이트 요청만 key를 받는다', () => {
    expect(isLocalSameSite(req('127.0.0.1', 'same-origin'))).toBe(true);
    expect(isLocalSameSite(req('::1'))).toBe(true);
    expect(isLocalSameSite(req('::ffff:127.0.0.1', 'none'))).toBe(true);
  });
  it('다른 기기(--host)·다른 사이트의 요청은 거절', () => {
    expect(isLocalSameSite(req('192.168.0.7', 'same-origin'))).toBe(false);
    expect(isLocalSameSite(req('127.0.0.1', 'cross-site'))).toBe(false);
  });
});

describe('devApiProxy', () => {
  function run(token: string | null) {
    const emitter = new EventEmitter();
    const opts = devApiProxy({ url: 'http://127.0.0.1:18080', controlUrl: 'http://127.0.0.1:18081', token, tenantName: null, source: 'test' })['/api'];
    opts?.configure?.(emitter as never, opts);
    const headers = new Map<string, string>([
      ['authorization', 'Bearer from-browser'],
      ['cookie', 'session=1'],
    ]);
    const req = {
      removeHeader: (k: string) => headers.delete(k),
      setHeader: (k: string, v: string) => headers.set(k, v),
    };
    emitter.emit('proxyReq', req);
    return headers;
  }

  it('브라우저의 Authorization·Cookie를 지우고 proxy key만 붙인다', () => {
    const h = run('tok');
    expect(h.get('authorization')).toBe('Bearer tok');
    expect(h.has('cookie')).toBe(false);
  });

  it('key가 없으면 Authorization 없이 보낸다(서버가 401)', () => {
    expect(run(null).has('authorization')).toBe(false);
  });

  it('monitor·policy 경로는 control-api로, 나머지는 query-api로(먼저 맞는 항목)', () => {
    const p = devApiProxy({ url: 'http://q', controlUrl: 'http://c', token: 't', tenantName: null, source: 'test' });
    expect(Object.keys(p)[0]).toBe(CONTROL_PATHS);
    expect(p[CONTROL_PATHS]?.target).toBe('http://c');
    expect(p['/api']?.target).toBe('http://q');
    const re = new RegExp(CONTROL_PATHS);
    for (const path of ['/api/v1/monitors', '/api/v1/monitors/x/timeline', '/api/v1/monitors?limit=1', '/api/v1/monitors/validate', '/api/v1/notification-policies/abc', '/api/v1/dashboards', '/api/v1/dashboards/x']) {
      expect(re.test(path)).toBe(true);
    }
    for (const path of ['/api/v1/metrics', '/api/v1/monitorsx', '/api/v1/dashboardsx', '/api/v1/query/metrics', '/api/v1/services']) {
      expect(re.test(path)).toBe(false);
    }
  });
});

describe('session 모드 (ADR 0065)', () => {
  function headersFor(auth: 'key' | 'session') {
    const emitter = new EventEmitter();
    const opts = devApiProxy({ url: 'http://q', controlUrl: 'http://c', token: 'tok', tenantName: null, source: 'test', auth })['/api'];
    opts?.configure?.(emitter as never, opts);
    const headers = new Map<string, string>([
      ['authorization', 'Bearer from-browser'],
      ['cookie', '__Host-mt_session=s'],
    ]);
    emitter.emit('proxyReq', { removeHeader: (k: string) => headers.delete(k), setHeader: (k: string, v: string) => headers.set(k, v) });
    return headers;
  }
  const local = (url: string) => ({ url, headers: {}, socket: { remoteAddress: '127.0.0.1' } });

  it('session이면 cookie를 넘기고 key를 붙이지 않는다', () => {
    const h = headersFor('session');
    expect(h.get('cookie')).toBe('__Host-mt_session=s');
    expect(h.has('authorization')).toBe(false);
  });

  it('key 모드는 로그인 경로를 404로 숨기고, session 모드는 control-api로 넘긴다', () => {
    const key = devApiProxy({ url: 'http://q', controlUrl: 'http://c', token: 't', tenantName: null, source: 'test' });
    const ses = devApiProxy({ url: 'http://q', controlUrl: 'http://c', token: null, tenantName: null, source: 'test', auth: 'session' });
    expect(new RegExp(CONTROL_PATHS).test('/api/v1/auth/session')).toBe(true);
    expect(key[CONTROL_PATHS]?.bypass?.(local('/api/v1/auth/session') as never, undefined as never, {} as never)).toBe(false);
    expect(key[CONTROL_PATHS]?.bypass?.(local('/api/v1/dashboards') as never, undefined as never, {} as never)).toBeUndefined();
    expect(ses[CONTROL_PATHS]?.bypass?.(local('/api/v1/auth/session') as never, undefined as never, {} as never)).toBeUndefined();
  });

  it('MONTRACER_DEV_AUTH', () => {
    expect(devAuthMode({ MONTRACER_DEV_AUTH: 'session' })).toBe('session');
    expect(devAuthMode({})).toBe('key');
    expect(devAuthMode({ MONTRACER_DEV_AUTH: 'other' })).toBe('key');
  });

  it('session 모드: IdP에서 돌아오는 cross-site callback 이동만 받는다', () => {
    const ses = devApiProxy({ url: 'http://q', controlUrl: 'http://c', token: null, tenantName: null, source: 'test', auth: 'session' });
    const req = (method: string, url: string, headers: Record<string, string>, ip = '127.0.0.1') => ({ method, url, headers, socket: { remoteAddress: ip } });
    const nav = { 'sec-fetch-site': 'cross-site', 'sec-fetch-mode': 'navigate' };
    const bypass = (r: unknown) => ses[CONTROL_PATHS]?.bypass?.(r as never, undefined as never, {} as never);
    expect(bypass(req('GET', '/api/v1/auth/callback?code=c&state=s', nav))).toBeUndefined();
    expect(bypass(req('GET', '/api/v1/auth/session', nav))).toBe(false);
    expect(bypass(req('POST', '/api/v1/auth/callback', nav))).toBe(false);
    expect(bypass(req('GET', '/api/v1/auth/callback', { 'sec-fetch-site': 'cross-site', 'sec-fetch-mode': 'cors' }))).toBe(false);
    expect(bypass(req('GET', '/api/v1/auth/callback', nav, '10.0.0.5'))).toBe(false);
  });
});
