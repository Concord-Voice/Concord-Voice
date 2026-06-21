/**
 * Playwright visual-regression spec for Concord Voice — spec §4.7 + §6.4.
 *
 * CI ENFORCEMENT (post-#1074)
 * ---------------------------
 * Runs MANUALLY via `npm run test:e2e` — CI enforcement was removed in #1435.
 * touching `client/desktop/**`. Visual diffs against the committed
 * baselines are non-blocking — a snapshot mismatch posts a sticky PR
 * comment with the diff artifact link but does NOT fail CI.
 *
 * Re-baselining is handled by a one-shot bot-authored follow-up PR
 * (Linux baselines + Mac-baseline deletion). The existing Mac-captured
 * baselines (`*-chromium-darwin.png`) remain until that follow-up PR
 * merges; CI runs on Linux will produce subpixel-rendering drift in
 * the meantime, surfaced as the non-blocking sticky comment.
 *
 * Local re-baselining (developer workflow):
 *   1. npx playwright install          # one-time: fetch Chromium binary
 *   2. In a separate shell:
 *        cd client/desktop && npx vite --port 3001
 *   3. cd client/desktop && npx playwright test visual-regression --update-snapshots
 *   4. The CI follow-up PR handles canonical Linux baselines; do NOT
 *      commit local Mac baselines unless instructed.
 *
 * SURFACE SCOPE — WHY ONLY login
 * --------------------------------
 * The spec (§4.7) listed /chat, /voice, /settings, /auth, /modal as target
 * surfaces, but the renderer's react-router config defines only three routes:
 *   /           → login / connection page (no auth required)
 *   /app        → main app view (requires auth + live backend)
 *   /app/dms    → DMs view (requires auth + live backend)
 *
 * The surfaces named in §4.7 are sub-views rendered within /app, not
 * independent URL-addressable routes.  Navigating to /chat without auth
 * would produce a redirect or an error page — not a meaningful baseline.
 *
 * The safe, reliably-reachable surface is '/' (login page).  All 19 design
 * tokens ARE loaded at '/' (styles/index.css is part of the React bundle),
 * so theming is exercisable there.  Once auth-fixture helpers are in place,
 * additional baselines for /app sub-views can be added here.  See #1075 for
 * the planned authenticated-surface expansion.
 *
 * References:
 *   spec §4.7 + §6.4  →  [internal]specs/2026-05-19-795-1033-design-tokens-and-lock-badge-design.md
 *   #1033 (design-token taxonomy), #795 (lock badge removal), #201 (E2EE epic)
 */

import { test, expect } from '@playwright/test';

/**
 * Surfaces to snapshot.
 *
 * Currently scoped to the login page — the only route reachable without a
 * live backend or authenticated session.  Additional surfaces can be added
 * here once an auth-fixture helper is available for e2e tests.
 *
 * When expanding, apply the same { url, label } shape so the loop below
 * picks them up automatically.
 *
 * Candidate future surfaces (once auth fixtures exist):
 *   { url: '/app',     label: 'chat'     }   // main channel view
 *   { url: '/app/dms', label: 'dms'      }   // direct messages
 */
const SURFACES = [{ url: '/', label: 'login' }] as const;

/**
 * Theme modes.  null → dark (default; no data-theme attribute).
 * 'light' → light mode.  Matches the data-theme toggle in the app.
 */
const MODES = [
  { theme: null as string | null, label: 'dark' },
  { theme: 'light', label: 'light' },
] as const;

// ---------------------------------------------------------------------------
// Snapshot tests — SURFACES × MODES
// ---------------------------------------------------------------------------

for (const surface of SURFACES) {
  for (const mode of MODES) {
    test(
      `${surface.label} ${mode.label} matches baseline`,
      { tag: '@renderer-only' },
      async ({ page }) => {
        await page.goto(surface.url);

        // Apply theme mode.  Omitting data-theme keeps the default dark mode;
        // setting 'light' switches to the light variant of whatever scheme is
        // active (concord-light by default).
        await page.evaluate((t) => {
          if (t !== null) {
            document.documentElement.setAttribute('data-theme', t);
          } else {
            document.documentElement.removeAttribute('data-theme');
          }
        }, mode.theme);

        // Wait for network idle to ensure fonts + async CSS are fully applied.
        await page.waitForLoadState('networkidle');

        await expect(page).toHaveScreenshot(`${surface.label}-${mode.label}.png`, {
          maxDiffPixels: 100,
        });
      }
    );
  }
}

// ---------------------------------------------------------------------------
// Reduce-animations regression check
// ---------------------------------------------------------------------------

/**
 * Asserts that setting data-reduce-animations='true' on the document root
 * causes the CSS cascade to zero out all transition durations via the
 * `transition-duration: 0s !important` rule in the motion tokens block.
 *
 * This is a regression guard: if the reduce-animations block is accidentally
 * removed or the selector becomes incorrect, this test catches it immediately
 * without requiring a visual diff.
 */
test(
  'reduce-animations cascade zeros motion durations',
  { tag: '@renderer-only' },
  async ({ page }) => {
    await page.goto('/');

    // Inject a probe element with a known non-zero transition. Reading
    // transition-duration on documentElement alone (the prior pattern) is a
    // false-confidence assertion: the CSS spec default for elements with no
    // transition declared is '0s', so the cascade rule could be silently
    // removed and the test would still pass. By probing an element with a
    // non-zero baseline transition, we prove the cascade rule actually wins.
    await page.evaluate(() => {
      const probe = document.createElement('div');
      probe.id = '__reduce-animations-probe__';
      probe.style.transition = 'opacity 500ms ease';
      document.body.appendChild(probe);
    });

    // Baseline: the probe's transition-duration is 500ms before the override.
    const before = await page.evaluate(() => {
      const el = document.getElementById('__reduce-animations-probe__');
      return el ? getComputedStyle(el).transitionDuration : '';
    });
    expect(before, 'probe transition-duration before cascade override').toBe('0.5s');

    // Activate the reduced-motion override.
    await page.evaluate(() => {
      document.documentElement.setAttribute('data-reduce-animations', 'true');
    });

    // After: the cascade rule (`[data-reduce-animations='true'] *` with
    // `transition-duration: 0s !important`) wins, zeroing the probe transition.
    const after = await page.evaluate(() => {
      const el = document.getElementById('__reduce-animations-probe__');
      return el ? getComputedStyle(el).transitionDuration : '';
    });
    expect(after, 'probe transition-duration after cascade override').toBe('0s');
  }
);
