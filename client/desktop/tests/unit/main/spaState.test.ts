import { describe, it, expect, beforeEach, vi } from 'vitest';

import {
  getRemoteSpaBaseDir,
  getRemoteSpaBaseUrl,
  getRemoteSpaUrl,
  onSpaStateChange,
  setRemoteSpaState,
} from '@/main/spaState';

describe('spaState — atomic lockstep invariant', () => {
  beforeEach(() => {
    // Reset module-scope state between tests.
    setRemoteSpaState(null);
  });

  it('starts with both variables null', () => {
    expect(getRemoteSpaBaseUrl()).toBeNull();
    expect(getRemoteSpaUrl()).toBeNull();
  });

  it('setRemoteSpaState(url) sets both atomically', () => {
    setRemoteSpaState('https://api.concordvoice.chat/spa/abc1234/index.html');
    expect(getRemoteSpaBaseUrl()).toBe('https://api.concordvoice.chat');
    expect(getRemoteSpaUrl()).toBe('https://api.concordvoice.chat/spa/abc1234/index.html');
  });

  it('setRemoteSpaState(null) clears both atomically', () => {
    setRemoteSpaState('https://api.concordvoice.chat/spa/abc/index.html');
    setRemoteSpaState(null);
    expect(getRemoteSpaBaseUrl()).toBeNull();
    expect(getRemoteSpaUrl()).toBeNull();
  });

  it('setRemoteSpaState(malformed) clears both (fail-closed)', () => {
    // Pre-populate so the failure path is observable.
    setRemoteSpaState('https://api.concordvoice.chat/spa/abc/index.html');
    setRemoteSpaState('not a url');
    expect(getRemoteSpaBaseUrl()).toBeNull();
    expect(getRemoteSpaUrl()).toBeNull();
  });

  it('successive sets preserve lockstep (regression for #815 reconciliation)', () => {
    setRemoteSpaState('https://api.concordvoice.chat/spa/abc/index.html');
    setRemoteSpaState('https://api.concordvoice.chat/spa/def5678/index.html');
    expect(getRemoteSpaBaseUrl()).toBe('https://api.concordvoice.chat');
    expect(getRemoteSpaUrl()).toBe('https://api.concordvoice.chat/spa/def5678/index.html');
  });
});

describe('getRemoteSpaBaseDir — runtime SPA base directory (#976 self-heal)', () => {
  beforeEach(() => {
    setRemoteSpaState(null);
  });

  it('returns null when no SPA is active', () => {
    expect(getRemoteSpaBaseDir()).toBeNull();
  });

  it("derives '/spa/<sha>/' for the legacy per-SHA host", () => {
    setRemoteSpaState('https://api.concordvoice.chat/spa/abc1234/index.html');
    expect(getRemoteSpaBaseDir()).toBe('/spa/abc1234/');
  });

  it("derives '/' for the flat Cloudflare Pages host served at index.html", () => {
    setRemoteSpaState('https://spa.concordvoice.chat/index.html');
    expect(getRemoteSpaBaseDir()).toBe('/');
  });

  it("derives '/' for the flat host served at the apex", () => {
    setRemoteSpaState('https://spa.concordvoice.chat/');
    expect(getRemoteSpaBaseDir()).toBe('/');
  });

  it('clears to null on malformed input (fail-closed, lockstep with origin/url)', () => {
    setRemoteSpaState('https://spa.concordvoice.chat/index.html');
    setRemoteSpaState('not a url');
    expect(getRemoteSpaBaseDir()).toBeNull();
    expect(getRemoteSpaBaseUrl()).toBeNull();
  });
});

describe('spaState event emitter (onSpaStateChange — added by #806)', () => {
  beforeEach(() => {
    setRemoteSpaState(null);
  });

  it('fires the change listener when setRemoteSpaState is called', () => {
    const listener = vi.fn();
    const unsubscribe = onSpaStateChange(listener);

    setRemoteSpaState('https://example.com/spa/abc123/index.html');

    expect(listener).toHaveBeenCalledWith('https://example.com/spa/abc123/index.html');
    unsubscribe();
  });

  it('does not call the listener after unsubscribe', () => {
    const listener = vi.fn();
    const unsubscribe = onSpaStateChange(listener);
    unsubscribe();

    setRemoteSpaState('https://example.com/spa/xyz789/index.html');

    expect(listener).not.toHaveBeenCalled();
  });

  it('supports multiple listeners', () => {
    const a = vi.fn();
    const b = vi.fn();
    const unsubA = onSpaStateChange(a);
    const unsubB = onSpaStateChange(b);

    setRemoteSpaState('https://example.com/spa/multi/index.html');

    expect(a).toHaveBeenCalled();
    expect(b).toHaveBeenCalled();
    unsubA();
    unsubB();
  });

  it('fires with null when state is cleared', () => {
    setRemoteSpaState('https://example.com/spa/initial/index.html');
    const listener = vi.fn();
    const unsubscribe = onSpaStateChange(listener);

    setRemoteSpaState(null);

    expect(listener).toHaveBeenCalledWith(null);
    unsubscribe();
  });

  it('fires with null when input URL fails to parse (matches fail-closed behavior)', () => {
    const listener = vi.fn();
    const unsubscribe = onSpaStateChange(listener);

    setRemoteSpaState('not-a-url');

    expect(listener).toHaveBeenCalledWith(null);
    unsubscribe();
  });
});
