/**
 * Regression lock for the /assets/* status-aware caching Pages Function (#2173).
 *
 * public/_headers marks /assets/* immutable (URL-matched, status-blind), so a missing
 * hashed chunk during a deploy window would be served as an immutable fallback and
 * poison the browser cache for hours. The Function forces `no-store` on the fallback
 * (404, or any text/html at an /assets/* path) and applies the canonical immutable
 * policy to real assets itself (Cloudflare does not reliably apply `_headers` to Pages
 * Function responses, so the Function is the source of truth — #2174 P2 review).
 */
import { describe, it, expect } from 'vitest';
// eslint-disable-next-line -- tests are un-linted; import the literal [[path]].js Function.
import { onRequest } from '../../../functions/assets/[[path]].js';

const ctxReturning = (response: Response) => ({ next: async () => response });

describe('assets/[[path]] Pages Function (#2173)', () => {
  it('keeps a real asset (200 application/javascript) 200 + immutable + nosniff', async () => {
    const res = new Response('export const x = 1;', {
      status: 200,
      headers: {
        'content-type': 'application/javascript',
        'cache-control': 'public, max-age=31536000, immutable',
      },
    });
    const out = await onRequest(ctxReturning(res));
    expect(out.status).toBe(200);
    expect(out.headers.get('content-type')).toBe('application/javascript');
    expect(out.headers.get('cache-control')).toBe('public, max-age=31536000, immutable');
    expect(out.headers.get('x-content-type-options')).toBe('nosniff');
  });

  it('ADDS immutable + nosniff to a real asset even when _headers did not (Function is source of truth)', async () => {
    // Simulates the environment the P2 review flagged: _headers is NOT applied to the
    // Pages Function response, so the origin asset arrives with no cache policy. The
    // Function must supply the canonical /assets/* immutable policy itself.
    const res = new Response('export const x = 1;', {
      status: 200,
      headers: { 'content-type': 'application/javascript' },
    });
    const out = await onRequest(ctxReturning(res));
    expect(out.status).toBe(200);
    expect(out.headers.get('cache-control')).toBe('public, max-age=31536000, immutable');
    expect(out.headers.get('x-content-type-options')).toBe('nosniff');
  });

  it('forces no-store on a 404 fallback (prevents the immutable-404 poisoning)', async () => {
    const res = new Response('<!doctype html>404', {
      status: 404,
      headers: {
        'content-type': 'text/html; charset=utf-8',
        'cache-control': 'public, max-age=31536000, immutable',
      },
    });
    const out = await onRequest(ctxReturning(res));
    expect(out.status).toBe(404);
    expect(out.headers.get('cache-control')).toBe('no-store');
  });

  it('forces no-store on a 200 text/html fallback (SPA shell served at an /assets path)', async () => {
    const res = new Response('<!doctype html>shell', {
      status: 200,
      headers: {
        'content-type': 'text/html',
        'cache-control': 'public, max-age=31536000, immutable',
      },
    });
    const out = await onRequest(ctxReturning(res));
    expect(out.headers.get('cache-control')).toBe('no-store');
  });

  it('forces no-store on a transient non-HTML 5xx (never immutable-cache an error asset)', async () => {
    // The regression the #2174 P2 review caught: a 503 with a JSON/empty body during a
    // deploy window is not a 404 and not text/html, so it must NOT fall through to the
    // immutable policy — it has to be no-store so the client re-fetches the real asset.
    for (const status of [500, 502, 503]) {
      const res = new Response('{"error":"unavailable"}', {
        status,
        headers: { 'content-type': 'application/json' },
      });
      const out = await onRequest(ctxReturning(res));
      expect(out.status).toBe(status);
      expect(out.headers.get('cache-control')).toBe('no-store');
    }
  });

  it('forces no-store on a bodyless error with no content-type at all', async () => {
    const res = new Response(null, { status: 503 });
    const out = await onRequest(ctxReturning(res));
    expect(out.headers.get('cache-control')).toBe('no-store');
  });

  it('forces no-store on a 3xx redirect at an /assets path', async () => {
    const res = new Response(null, {
      status: 302,
      headers: { location: '/assets/other.js' },
    });
    const out = await onRequest(ctxReturning(res));
    expect(out.headers.get('cache-control')).toBe('no-store');
  });
});
