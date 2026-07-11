/**
 * Regression lock for the Cloudflare Pages 404 fallback (issue #2173, Refs #1870).
 *
 * `client/desktop/public/404.html` is LOAD-BEARING, not decorative. Cloudflare
 * Pages treats a project WITHOUT a top-level 404.html as a single-page app and
 * serves /index.html (200 text/html) for every unmatched path — including a
 * missing /assets/<hash>.js. Combined with the immutable Cache-Control on
 * /assets/* (public/_headers), that poisoned browser caches with a wrong-MIME
 * 200 during deploy windows and broke every ES-module load. The file's presence
 * disables that fallback so missing assets return a genuine 404.
 *
 * If this test breaks, the fallback guard was deleted or the 404 page grew an
 * external subresource (which could itself recurse through the fallback). Either
 * restore an equivalent guard or confirm the change is intentional.
 */
import { existsSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

const CLIENT_DESKTOP_ROOT = path.resolve(__dirname, '../../..');
const FALLBACK_PATH = path.join(CLIENT_DESKTOP_ROOT, 'public/404.html');

describe('Cloudflare Pages 404 fallback (#2173)', () => {
  it('the top-level public/404.html exists (disables the SPA-fallback MIME poison)', () => {
    expect(existsSync(FALLBACK_PATH)).toBe(true);
  });

  it('is a real HTML document that retains its rationale', () => {
    const html = readFileSync(FALLBACK_PATH, 'utf8');
    expect(html).toMatch(/<!doctype html>/i);
    // The "why" must survive edits — the mechanism is non-obvious.
    expect(html).toMatch(/Cloudflare Pages/i);
  });

  it('is fully self-contained (no external subresource can recurse through the fallback)', () => {
    const html = readFileSync(FALLBACK_PATH, 'utf8');
    // No scripts, no <link> (stylesheet/icon/preload), no src= (img/iframe/script),
    // and no CSS url(). Inline <style> is fine, and <a href="/"> is navigation, not
    // a subresource. Together these guarantee the page fetches nothing from the Pages
    // origin, so it can never re-trigger the fallback it exists to prevent. (No
    // "/assets/" path-string check: the rationale comment intentionally names that
    // path, and the four assertions below already preclude any actual subresource.)
    expect(html).not.toMatch(/<script\b/i);
    expect(html).not.toMatch(/<link\b/i);
    expect(html).not.toMatch(/\bsrc\s*=/i);
    expect(html).not.toMatch(/url\(/i);
  });
});
