/**
 * @vitest-environment jsdom
 */
import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';

describe('SPA_VERSION detection', () => {
  beforeEach(() => {
    vi.resetModules();
  });

  afterEach(() => {
    // Cleanup env stubs so VITE_SPA_VERSION doesn't leak into other tests
    // in the same worker (Copilot review on #1204 caught this — without
    // unstubAllEnvs, vi.stubEnv state persists across tests).
    vi.unstubAllEnvs();
  });

  it('prefers VITE_SPA_VERSION when set', async () => {
    vi.stubEnv('VITE_SPA_VERSION', 'abc1234');
    const { SPA_VERSION } = await import('@/renderer/config');
    expect(SPA_VERSION).toBe('abc1234');
  });

  it('falls back to URL-extracted SHA when env var is missing', async () => {
    vi.stubEnv('VITE_SPA_VERSION', '');
    // Production SPA URL contract per ADR-0001: `/spa/<sha>/...` (NOT
    // `/<sha>/...` — Copilot review on #1204 caught the earlier mistake).
    Object.defineProperty(window, 'location', {
      value: new URL('https://spa.concordvoice.chat/spa/7683eb1/index.html'),
      writable: true,
      configurable: true,
    });
    const { SPA_VERSION } = await import('@/renderer/config');
    expect(SPA_VERSION).toBe('7683eb1');
  });

  it('returns "remote" for a flat Pages SPA with no /spa/<sha>/ and no env (post-#976)', async () => {
    // Post-#976 the SPA is served flat at spa.concordvoice.chat with the version
    // stamped into VITE_SPA_VERSION at build time (main-cd.yml). If that env var
    // is somehow unset, the version isn't recoverable from the flat URL — but a
    // REMOTE SPA is still loaded, so it must NOT be mislabelled 'bundled'.
    vi.stubEnv('VITE_SPA_VERSION', '');
    Object.defineProperty(window, 'location', {
      value: new URL('https://spa.concordvoice.chat/index.html'),
      writable: true,
      configurable: true,
    });
    const { SPA_VERSION } = await import('@/renderer/config');
    expect(SPA_VERSION).toBe('remote');
  });

  it('returns "bundled" when URL has no /spa/<sha>/ segment', async () => {
    vi.stubEnv('VITE_SPA_VERSION', '');
    Object.defineProperty(window, 'location', {
      value: new URL('app://concord/index.html'),
      writable: true,
      configurable: true,
    });
    const { SPA_VERSION } = await import('@/renderer/config');
    expect(SPA_VERSION).toBe('bundled');
  });

  it('returns "bundled" on localhost dev server', async () => {
    vi.stubEnv('VITE_SPA_VERSION', '');
    Object.defineProperty(window, 'location', {
      value: new URL('http://localhost:3001/'),
      writable: true,
      configurable: true,
    });
    const { SPA_VERSION } = await import('@/renderer/config');
    expect(SPA_VERSION).toBe('bundled');
  });

  it('does NOT extract a bogus SHA that appears without the /spa/ prefix (defense)', async () => {
    // Regression guard: an attacker-controlled or misconfigured URL that happens
    // to have hex chars right after the host MUST NOT be treated as the SPA hash.
    // The /spa/ literal in the regex prevents this. Post-#976 the origin-aware
    // fallback reports this remote origin as 'remote' — NOT the bogus hex, and
    // not 'bundled' (it IS a remote host, just unversioned).
    vi.stubEnv('VITE_SPA_VERSION', '');
    Object.defineProperty(window, 'location', {
      value: new URL('https://spa.concordvoice.chat/7683eb1/index.html'),
      writable: true,
      configurable: true,
    });
    const { SPA_VERSION } = await import('@/renderer/config');
    expect(SPA_VERSION).not.toBe('7683eb1'); // security property: no bogus SHA
    expect(SPA_VERSION).toBe('remote');
  });
});
