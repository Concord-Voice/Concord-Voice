/**
 * @vitest-environment node
 *
 * Loopback HTTP server for SSO OAuth callback capture.
 *
 * Runs in node environment because it binds an actual TCP listener on
 * 127.0.0.1 and uses fetch() against it — jsdom's fetch shim does not
 * proxy to the real network stack.
 */
import { describe, expect, it } from 'vitest';
import { startLoopback } from '../../../src/main/ssoLoopback';

describe('startLoopback', () => {
  it('resolves with code+state on /oauth/callback', async () => {
    const handle = await startLoopback();
    expect(handle.port).toBeGreaterThan(0);
    expect(handle.redirectURI).toMatch(/^http:\/\/127\.0\.0\.1:\d+\/oauth\/callback$/);

    // Simulate provider redirect
    const callbackURL = `${handle.redirectURI}?code=test-code&state=test-state`;
    const fetchPromise = fetch(callbackURL);

    const result = await handle.promise;
    expect(result.code).toBe('test-code');
    expect(result.state).toBe('test-state');

    const resp = await fetchPromise;
    expect(resp.status).toBe(200);
    expect(await resp.text()).toContain('Signed in');
  });

  it('rejects with oauth_provider_error when ?error= present', async () => {
    const handle = await startLoopback();
    const errURL = `${handle.redirectURI}?error=access_denied`;
    void fetch(errURL).catch(() => {});

    await expect(handle.promise).rejects.toThrow(/access_denied/);
  });

  it('rejects with oauth_timeout after TIMEOUT_MS', async () => {
    const handle = await startLoopback({ timeoutMs: 200 });
    await expect(handle.promise).rejects.toThrow(/oauth_timeout/);
  }, 10_000);

  it('returns 404 for non-callback paths', async () => {
    const handle = await startLoopback();
    try {
      const resp = await fetch(`http://127.0.0.1:${handle.port}/some/other/path`);
      expect(resp.status).toBe(404);
    } finally {
      handle.close();
    }
  });

  it('binds 127.0.0.1 only — not externally reachable', async () => {
    const handle = await startLoopback();
    try {
      // Loopback binding means 0.0.0.0 reachability is implementation-defined,
      // but on most stacks attempts to connect via the host's external IP fail.
      // We assert that the listen address is exactly 127.0.0.1.
      expect(handle.bindAddress).toBe('127.0.0.1');
    } finally {
      handle.close();
    }
  });

  it('close() rejects pending promise with oauth_cancelled', async () => {
    // Without this, a renderer that calls cancelLoopback (e.g. user backed
    // out of SSO) would leave the handle.promise pending forever — the
    // awaiting IPC handler would also hang. close() must settle the promise.
    const handle = await startLoopback({ timeoutMs: 60_000 });
    handle.close();
    await expect(handle.promise).rejects.toThrow(/oauth_cancelled/);
  });

  it('close() is idempotent after a successful resolve', async () => {
    // The settled flag prevents close()-after-resolve from rejecting an
    // already-fulfilled promise (which would surface as an unhandled
    // rejection in tests + a stuck modal in production).
    const handle = await startLoopback();
    void fetch(`${handle.redirectURI}?code=ok&state=ok`).catch(() => {});
    const result = await handle.promise;
    expect(result.code).toBe('ok');
    // Idempotent close — must not throw or change the resolved value.
    expect(() => handle.close()).not.toThrow();
  });

  // ── Apple POST callback (#271) ──────────────────────────────────────────
  // Apple's response_mode=form_post sends the OAuth callback as POST
  // application/x-www-form-urlencoded body. The loopback server must accept
  // it without breaking the existing Google GET path. The 'user' field is
  // populated only on the first authentication for a given Services ID and
  // carries the user's first/last name JSON — this becomes appleUserData
  // on the resolved LoopbackResult. Subsequent auths legitimately omit it.

  it('handles POST application/x-www-form-urlencoded with code+state+user — resolves with appleUserData', async () => {
    const handle = await startLoopback();
    const userJson = '{"name":{"firstName":"Jane","lastName":"Doe"},"email":"jane@example.com"}';
    const body = new URLSearchParams({
      code: 'apple-code',
      state: 'apple-state',
      user: userJson,
    }).toString();

    const fetchPromise = fetch(handle.redirectURI, {
      method: 'POST',
      headers: { 'content-type': 'application/x-www-form-urlencoded' },
      body,
    });

    const result = await handle.promise;
    expect(result.code).toBe('apple-code');
    expect(result.state).toBe('apple-state');
    expect(result.appleUserData).toBe(userJson);

    const resp = await fetchPromise;
    expect(resp.status).toBe(200);
  });

  it('handles POST without user field — resolves without appleUserData', async () => {
    // Subsequent Apple authentications (Scenario C in the spec) have no
    // user field; result.appleUserData must be undefined, not "".
    const handle = await startLoopback();
    const body = new URLSearchParams({
      code: 'apple-code-2',
      state: 'apple-state-2',
    }).toString();

    void fetch(handle.redirectURI, {
      method: 'POST',
      headers: { 'content-type': 'application/x-www-form-urlencoded' },
      body,
    }).catch(() => {});

    const result = await handle.promise;
    expect(result.code).toBe('apple-code-2');
    expect(result.state).toBe('apple-state-2');
    expect(result.appleUserData).toBeUndefined();
  });

  it('rejects POST > 64 KiB with 413 — defends against oversize bodies', async () => {
    // Defense in depth: legitimate Apple POSTs are < 2 KiB. A hostile
    // localhost peer streaming a multi-megabyte body would otherwise
    // accumulate in memory until the OOM killer fires. 413 + req.destroy()
    // tears down the socket so the attacker cannot keep streaming.
    const handle = await startLoopback({ timeoutMs: 2000 });
    // 65 KiB of padding > 64 KiB cap. Real Apple bodies are ~1 KiB.
    const padding = 'a'.repeat(65 * 1024);
    const body = new URLSearchParams({
      code: 'c',
      state: 's',
      pad: padding,
    }).toString();

    const fetchPromise = fetch(handle.redirectURI, {
      method: 'POST',
      headers: { 'content-type': 'application/x-www-form-urlencoded' },
      body,
    }).catch((err: unknown) => err);

    // Promise should reject via timeout (request destroyed before completing).
    await expect(handle.promise).rejects.toThrow(/oauth_timeout/);
    // The fetch may receive 413 OR the connection may be destroyed mid-flight
    // (legitimate either way — the goal is to NOT resolve with the oversize body).
    await fetchPromise;
  }, 10_000);

  it('rejects POST with content-type !== application/x-www-form-urlencoded with 405', async () => {
    // POST-body smuggling defense (spec §9). A hostile peer sending JSON
    // (or anything else) is rejected with 405 Method Not Allowed; the
    // loopback never tries to URL-decode a non-form-encoded body.
    const handle = await startLoopback();
    try {
      const resp = await fetch(handle.redirectURI, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ code: 'c', state: 's' }),
      });
      expect(resp.status).toBe(405);
    } finally {
      handle.close();
    }
  });

  it('still serves Google GET path with code+state — no appleUserData (regression)', async () => {
    // Backwards-compat regression test: the existing Google flow uses
    // GET /oauth/callback?code=...&state=...; adding POST support must
    // NOT break it. result.appleUserData must be undefined on this branch
    // (it is Apple-only).
    const handle = await startLoopback();
    void fetch(`${handle.redirectURI}?code=google-code&state=google-state`).catch(() => {});

    const result = await handle.promise;
    expect(result.code).toBe('google-code');
    expect(result.state).toBe('google-state');
    expect(result.appleUserData).toBeUndefined();
  });

  it('rejects POST with error param — resolves to oauth_provider_error', async () => {
    // Apple can POST an error response (e.g., user_cancelled_authorize) via
    // form_post just like Google can pass ?error= in the GET. Surface it
    // through the same shared param-handler path.
    const handle = await startLoopback();
    const body = new URLSearchParams({ error: 'user_cancelled_authorize' }).toString();

    void fetch(handle.redirectURI, {
      method: 'POST',
      headers: { 'content-type': 'application/x-www-form-urlencoded' },
      body,
    }).catch(() => {});

    await expect(handle.promise).rejects.toThrow(/user_cancelled_authorize/);
  });

  it('does not resolve on POST with malformed body (no code or state)', async () => {
    // Probe / malformed POST: no code AND no state → 400 returned to the
    // peer, but the loopback promise stays pending so a follow-up legitimate
    // hit can still resolve. This mirrors the GET-path malformed-callback
    // behavior. Verified by asserting that the promise times out rather
    // than rejecting with oauth_provider_error.
    const handle = await startLoopback({ timeoutMs: 300 });
    void fetch(handle.redirectURI, {
      method: 'POST',
      headers: { 'content-type': 'application/x-www-form-urlencoded' },
      body: 'random=junk',
    }).catch(() => {});

    await expect(handle.promise).rejects.toThrow(/oauth_timeout/);
  }, 5_000);
});
