// @vitest-environment-options { "url": "https://api.example.com/" }
//
// Initializes jsdom with the SPA's production origin so `globalThis.location.origin`
// matches the chunk URLs the tests dispatch. The cross-origin guard in
// `installSelfHealHandlers` requires same-origin to allow IPC; without this
// the default jsdom origin (`http://localhost:3000`) would make every chunk
// URL test cross-origin and the guard would always short-circuit. The
// cross-origin reject test (Copilot review on PR #773) uses a different
// host explicitly to exercise the rejection path. Note: redefining
// `window.location.origin` at runtime via Object.defineProperty fails in
// jsdom because the descriptor is non-configurable; the pragma is the
// portable hook.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

declare global {
  interface Window {
    electron?: {
      spa?: {
        requestSelfHeal: (payload: {
          reason: 'chunk-load' | 'chunk-import-rejected';
          url?: string;
        }) => Promise<void>;
      };
    };
  }
}

import { installSelfHealHandlers } from '@/renderer/spaSelfHealClient';

describe('spaSelfHealClient — renderer detection', () => {
  let mockRequestSelfHeal: ReturnType<typeof vi.fn>;
  let originalElectron: Window['electron'];

  beforeEach(() => {
    originalElectron = window.electron;
    mockRequestSelfHeal = vi.fn().mockResolvedValue(undefined);
    // tests/setup.ts defines window.electron with {writable: true, configurable: false}
    // (configurable defaults to false). Direct assignment works because writable:true;
    // Object.defineProperty would fail because the descriptor is non-configurable.
    window.electron = { spa: { requestSelfHeal: mockRequestSelfHeal } };
    installSelfHealHandlers();
  });

  afterEach(() => {
    window.electron = originalElectron;
  });

  it('IPCs on script error matching SPA chunk URL pattern', () => {
    const script = document.createElement('script');
    script.src = 'https://api.example.com/spa/abc1234/assets/Settings-Xyz.js';
    document.body.appendChild(script);

    const event = new Event('error', { bubbles: false });
    Object.defineProperty(event, 'target', { value: script, writable: false });
    window.dispatchEvent(event);

    expect(mockRequestSelfHeal).toHaveBeenCalledWith({
      reason: 'chunk-load',
      url: 'https://api.example.com/spa/abc1234/assets/Settings-Xyz.js',
    });
  });

  it('IPCs on link error matching SPA chunk URL pattern', () => {
    const link = document.createElement('link');
    link.href = 'https://api.example.com/spa/abc1234/assets/main-Xyz.css';
    document.body.appendChild(link);

    const event = new Event('error', { bubbles: false });
    Object.defineProperty(event, 'target', { value: link, writable: false });
    window.dispatchEvent(event);

    expect(mockRequestSelfHeal).toHaveBeenCalledWith({
      reason: 'chunk-load',
      url: 'https://api.example.com/spa/abc1234/assets/main-Xyz.css',
    });
  });

  it('does NOT IPC on errors with non-SPA URLs', () => {
    const script = document.createElement('script');
    script.src = 'https://api.example.com/api/v1/messages/feed.js';
    document.body.appendChild(script);

    const event = new Event('error', { bubbles: false });
    Object.defineProperty(event, 'target', { value: script, writable: false });
    window.dispatchEvent(event);

    expect(mockRequestSelfHeal).not.toHaveBeenCalled();
  });

  it('does NOT IPC on cross-origin SPA-shaped chunk URL (Copilot review on PR #773)', () => {
    // SPA_CHUNK_URL_PATTERN is host-agnostic, so a third-party script with a
    // path that happens to match `/spa/<sha>/assets/...` would otherwise
    // trigger self-heal. The cross-origin guard in installSelfHealHandlers
    // requires the failing URL's origin to match `globalThis.location.origin`.
    // Here, the page's origin is stubbed to `https://api.example.com`
    // (see beforeEach), but the script comes from `evil.example.com` — the
    // guard must reject it.
    const script = document.createElement('script');
    script.src = 'https://evil.example.com/spa/abc1234/assets/Settings-Xyz.js';
    document.body.appendChild(script);

    const event = new Event('error', { bubbles: false });
    Object.defineProperty(event, 'target', { value: script, writable: false });
    window.dispatchEvent(event);

    expect(mockRequestSelfHeal).not.toHaveBeenCalled();
  });

  it('IPCs on unhandledrejection matching dynamic-import failure', () => {
    const event = Object.assign(new Event('unhandledrejection'), {
      reason: new Error(
        'Failed to fetch dynamically imported module: /spa/abc1234/assets/Settings-Xyz.js'
      ),
    });
    window.dispatchEvent(event);

    expect(mockRequestSelfHeal).toHaveBeenCalledWith({
      reason: 'chunk-import-rejected',
    });
  });

  it('IPCs on unhandledrejection matching loading-chunk failure', () => {
    const event = Object.assign(new Event('unhandledrejection'), {
      reason: new Error('Loading chunk Settings-Xyz failed.'),
    });
    window.dispatchEvent(event);

    expect(mockRequestSelfHeal).toHaveBeenCalledWith({
      reason: 'chunk-import-rejected',
    });
  });

  it('does NOT IPC on unhandledrejection with non-chunk message', () => {
    const event = Object.assign(new Event('unhandledrejection'), {
      reason: new Error('user-typed-bad-input'),
    });
    window.dispatchEvent(event);

    expect(mockRequestSelfHeal).not.toHaveBeenCalled();
  });

  it('idempotent install — calling installSelfHealHandlers twice does not double-fire', async () => {
    // The module-scope `installed` flag persists across tests, so simply
    // calling installSelfHealHandlers() a second time inside this test would
    // be a no-op due to leftover state from earlier beforeEach calls — the
    // assertion would pass tautologically. To genuinely exercise the
    // idempotency guard, reset the module and re-import freshly so `installed`
    // starts at false; THEN call install twice and verify only one listener
    // fires per event. (#753 reconciliation finding #11.)
    vi.resetModules();
    const { installSelfHealHandlers: freshInstall } = await import('@/renderer/spaSelfHealClient');

    // First call should register the listeners; second must short-circuit.
    freshInstall();
    freshInstall();

    const script = document.createElement('script');
    script.src = 'https://api.example.com/spa/abc1234/assets/Settings-Xyz.js';
    document.body.appendChild(script);

    const event = new Event('error', { bubbles: false });
    Object.defineProperty(event, 'target', { value: script, writable: false });
    window.dispatchEvent(event);

    // The outer beforeEach also installed listeners on the original module
    // copy — those fire too because they're attached to `window` directly.
    // The `installed` guard inside the FRESH module instance is what we're
    // testing: that two `freshInstall()` calls add only one new listener
    // pair (not two), so the fresh module contributes exactly one IPC fire.
    // Combined with the already-installed copy from beforeEach, total = 2.
    // If idempotency were broken, total would be 3.
    expect(mockRequestSelfHeal).toHaveBeenCalledTimes(2);
  });

  it('survives missing window.electron (no crash)', () => {
    window.electron = undefined;

    const script = document.createElement('script');
    script.src = 'https://api.example.com/spa/abc1234/assets/Settings-Xyz.js';
    document.body.appendChild(script);

    const event = new Event('error', { bubbles: false });
    Object.defineProperty(event, 'target', { value: script, writable: false });

    // Must not throw.
    expect(() => window.dispatchEvent(event)).not.toThrow();
  });
});
