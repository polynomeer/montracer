import { EventEmitter } from 'node:events';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { devApiProxy, isLocalSameSite, resolveDevApiTarget } from './dev-proxy.ts';

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
    expect(resolveDevApiTarget({ MONTRACER_DEV_SEED_FILE: f }, '/')).toMatchObject({ token: 'tok-acme', tenantName: 'acme', url: 'http://127.0.0.1:18080' });
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
    const opts = devApiProxy({ url: 'http://127.0.0.1:18080', token, tenantName: null, source: 'test' })['/api'];
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
});
