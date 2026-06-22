/**
 * CSP regression guard (dev-source side).
 *
 * The renderer's Content-Security-Policy is declared via a meta tag in
 * `client/desktop/index.html`. Any directive removal can introduce a
 * latent break — most notably for cross-origin resource flows like the
 * KLIPY media proxy (`<img src="${API_BASE}/api/v1/klipy/media?...">`),
 * which target the API origin and must therefore appear in `img-src`,
 * `media-src`, and `connect-src`.
 *
 * This file asserts the SOURCE policy in `index.html` — the dev-mode CSP
 * including loopback (`http://localhost:*`, `http://127.0.0.1:*`) entries.
 * The production-built CSP is stripped of loopback entries by the
 * `csp-prod-strip` Vite plugin (see `vite.config.ts`); that transformation
 * is exercised in `tests/unit/csp-prod-strip.test.ts`.
 *
 * These assertions are intentionally substring matches rather than full
 * CSP parsing — the goal is a fast deterministic guard against accidental
 * directive removal, not exhaustive policy validation.
 */
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

/**
 * Extract the CSP content attribute from `index.html`, tolerant to attribute
 * order. The two-step parse (find the `<meta>` tag containing the CSP
 * `http-equiv`, then extract its `content` attribute) doesn't care whether
 * `http-equiv=` or `content=` appears first — important because Prettier
 * and similar formatters may reflow HTML attributes over time.
 *
 * The content-attribute regex captures the opening quote via `(["'])` and
 * back-references it as `\1` for the closing quote. A symmetric `["']` on
 * both ends would stop at the first single quote inside the CSP value (the
 * value contains `'self'`, `'wasm-unsafe-eval'`, etc.), truncating the
 * extracted policy to the first few directives.
 */
function extractCsp(): string {
  const html = readFileSync(resolve(__dirname, '../../index.html'), 'utf-8');
  const metaMatch = html.match(
    /<meta[^>]+http-equiv\s*=\s*["']Content-Security-Policy["'][^>]*\/?>/i
  );
  if (!metaMatch) {
    throw new Error('CSP meta tag not found in index.html');
  }
  const contentMatch = metaMatch[0].match(/content\s*=\s*(["'])([\s\S]*?)\1/i);
  if (!contentMatch) {
    throw new Error('CSP meta tag found but content attribute missing');
  }
  return contentMatch[2];
}

describe('renderer CSP (dev-source from index.html)', () => {
  const csp = extractCsp();

  it('declares default-src self', () => {
    expect(csp).toMatch(/default-src\s+'self'/);
  });

  // The KLIPY GIF proxy and other API resources are fetched from the API
  // origin (not the renderer origin, which is `app://concord/` in bundled
  // mode after PR #830). `<img>` and `<video>` resolve relative URLs against
  // the renderer origin, so the API origin MUST be allowlisted for
  // img-src/media-src or cross-origin GIFs are blocked by CSP before they
  // ever reach the webRequest auth-injection interceptor.
  describe('cross-origin media-proxy allowances', () => {
    it('img-src allows the API origin in dev (localhost)', () => {
      const imgSrc = csp.match(/img-src\s+([^;]+);/)?.[1] ?? '';
      expect(imgSrc).toContain('http://localhost:*');
      expect(imgSrc).toContain('http://127.0.0.1:*');
    });

    it('img-src allows the API origin in production', () => {
      const imgSrc = csp.match(/img-src\s+([^;]+);/)?.[1] ?? '';
      expect(imgSrc).toContain('https://concordvoice.chat');
      expect(imgSrc).toContain('https://*.concordvoice.chat');
    });

    it('media-src allows the API origin in dev (localhost)', () => {
      const mediaSrc = csp.match(/media-src\s+([^;]+);/)?.[1] ?? '';
      expect(mediaSrc).toContain('http://localhost:*');
      expect(mediaSrc).toContain('http://127.0.0.1:*');
    });

    it('media-src allows the API origin in production', () => {
      const mediaSrc = csp.match(/media-src\s+([^;]+);/)?.[1] ?? '';
      expect(mediaSrc).toContain('https://concordvoice.chat');
      expect(mediaSrc).toContain('https://*.concordvoice.chat');
    });
  });

  // connect-src governs fetch/XHR/WebSocket. These have been allowlisted since
  // before the KLIPY work; this assertion is a coherence check ensuring the
  // three media-bearing directives (connect/img/media) stay in sync. Both the
  // apex and wildcard subdomain are asserted so a future edit that drops just
  // the apex (which would break apex-host fetches) is caught.
  it('connect-src allows the same API origins as img-src/media-src', () => {
    const connectSrc = csp.match(/connect-src\s+([^;]+);/)?.[1] ?? '';
    expect(connectSrc).toContain('http://localhost:*');
    expect(connectSrc).toContain('https://concordvoice.chat');
    expect(connectSrc).toContain('https://*.concordvoice.chat');
  });
});
