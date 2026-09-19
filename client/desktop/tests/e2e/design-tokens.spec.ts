/**
 * Playwright runtime resolution test for the 23 CSS design tokens (#1033, extended
 * by the 4-step display type scale in #1035).
 *
 * Why Playwright (not Vitest+jsdom): jsdom returns empty string for
 * getComputedStyle().getPropertyValue('--token-name') regardless of declarations —
 * a known limitation. Real Chromium via Playwright is the only place this assertion
 * is meaningful. The complementary Vitest file-parse test (design-tokens.test.ts)
 * catches typos in source CSS; this test catches runtime cascade failures.
 *
 * Stratified sample: 5 visually-distinct schemes (concord = brand baseline,
 * hacker = high-contrast monospace, spooky = warm orange, midnightsky = cool
 * deep blue, defacto = neutral graphite) × 2 modes = 10 combinations per spec
 * §6.3. Schema symmetry guarantees the other 9 schemes resolve identically; the
 * file-parse test covers them.
 *
 * The test navigates to '/' which renders the login/connection page — no auth
 * required, but styles/index.css is loaded as part of the React bundle.
 *
 * References: spec §4.6 + §6.3 at
 *   [internal]specs/2026-05-19-795-1033-design-tokens-and-lock-badge-design.md
 * Issues: #1033 (design-token taxonomy), #795 (lock badge removal), #201 (E2EE epic)
 */

import { test, expect } from '@playwright/test';

const STRATIFIED_SAMPLE = [
  { scheme: 'concord', theme: null as string | null, label: 'concord-dark' },
  { scheme: 'concord', theme: 'light', label: 'concord-light' },
  { scheme: 'hacker', theme: null, label: 'hacker-dark' },
  { scheme: 'hacker', theme: 'light', label: 'hacker-light' },
  { scheme: 'spooky', theme: null, label: 'spooky-dark' },
  { scheme: 'spooky', theme: 'light', label: 'spooky-light' },
  { scheme: 'midnightsky', theme: null, label: 'midnightsky-dark' },
  { scheme: 'midnightsky', theme: 'light', label: 'midnightsky-light' },
  { scheme: 'defacto', theme: null, label: 'defacto-dark' },
  { scheme: 'defacto', theme: 'light', label: 'defacto-light' },
] as const;

const ALL_23_TOKENS = [
  // State (3)
  '--state-selected',
  '--state-focused',
  '--state-hover',
  // Typography (4)
  '--font-display-stack',
  '--font-body-stack',
  '--font-display-tracking',
  '--font-display-tracking-tight',
  // Display (4) — 4-step display type scale (#1035)
  '--text-display-xl',
  '--text-display-lg',
  '--text-display-md',
  '--text-display-sm',
  // Scale (3)
  '--radius-base',
  '--radius-elevated',
  '--radius-modal',
  // Motion (5)
  '--motion-duration-fast',
  '--motion-duration-base',
  '--motion-duration-slow',
  '--motion-curve-base',
  '--motion-curve-decel',
  // Link (3)
  '--link-color',
  '--link-color-hover',
  '--link-color-visited',
  // Encryption (1)
  '--state-encryption-pending',
] as const;

for (const combo of STRATIFIED_SAMPLE) {
  test(
    `all 23 design tokens resolve in ${combo.label}`,
    { tag: '@renderer-only' },
    async ({ page }) => {
      // Navigate to the login page — no auth required; styles/index.css is loaded
      // as part of the React entry bundle.
      await page.goto('/');

      // Apply the stratified scheme/theme combination to the document root.
      // data-scheme selects the color scheme; data-theme selects dark/light mode.
      // Omitting data-theme keeps the scheme in its default (dark) mode.
      await page.evaluate(
        ({ scheme, theme }) => {
          document.documentElement.setAttribute('data-scheme', scheme);
          if (theme !== null) {
            document.documentElement.setAttribute('data-theme', theme);
          } else {
            document.documentElement.removeAttribute('data-theme');
          }
        },
        { scheme: combo.scheme, theme: combo.theme }
      );

      // Resolve every token and assert non-empty. An empty string indicates the
      // token is undeclared in the active cascade block — a schema-symmetry failure.
      for (const token of ALL_23_TOKENS) {
        const value = await page.evaluate(
          (tok) => getComputedStyle(document.documentElement).getPropertyValue(tok).trim(),
          token
        );
        expect(value, `Token ${token} must resolve in theme ${combo.label}`).not.toBe('');
      }
    }
  );
}

// ---------------------------------------------------------------------------
// Per-scheme cascade override verification
// ---------------------------------------------------------------------------

/**
 * Catches the case where a future edit removes a `--link-color: var(--accent-primary)`
 * declaration from a scheme block — the cascade would silently fall back to :root
 * and the per-token non-emptiness test (above) would still pass. By asserting that
 * two schemes resolve --link-color to *different* values (via their per-scheme
 * --accent-primary), we prove each scheme block's per-token override is actually
 * winning the cascade.
 */
test(
  'per-scheme overrides win the cascade for var()-bound tokens',
  { tag: '@renderer-only' },
  async ({ page }) => {
    await page.goto('/');

    async function resolveToken(scheme: string, token: string): Promise<string> {
      await page.evaluate((s) => {
        document.documentElement.setAttribute('data-scheme', s);
        document.documentElement.removeAttribute('data-theme');
      }, scheme);
      return page.evaluate(
        (t) => getComputedStyle(document.documentElement).getPropertyValue(t).trim(),
        token
      );
    }

    // --link-color binds to var(--accent-primary), which is per-scheme distinct.
    // concord's --accent-primary is pink/brand; hacker's is green/monospace. They
    // MUST differ — if not, one scheme's override was lost from the cascade.
    const concordLink = await resolveToken('concord', '--link-color');
    const hackerLink = await resolveToken('hacker', '--link-color');
    expect(concordLink, 'concord --link-color must resolve').not.toBe('');
    expect(hackerLink, 'hacker --link-color must resolve').not.toBe('');
    expect(concordLink).not.toBe(hackerLink);
  }
);

// ---------------------------------------------------------------------------
// --state-encryption-pending direct value assertion
// ---------------------------------------------------------------------------

/**
 * Catches the case where the sole consumer of the new --state-encryption-pending
 * token (Message.css:175 — the .decrypt-failed.pending-keys color migration)
 * silently breaks via a token value change. The token is set to #faa61a literal
 * across all 26 blocks (per spec §5 + verified by file-parse test). Without this
 * runtime value assertion, a typo or scope-creep edit in the token's value would
 * not be caught by the per-token non-emptiness test.
 */
test(
  '--state-encryption-pending resolves to #faa61a (the sole-consumer color)',
  { tag: '@renderer-only' },
  async ({ page }) => {
    await page.goto('/');
    await page.evaluate(() => {
      document.documentElement.setAttribute('data-scheme', 'concord');
      document.documentElement.removeAttribute('data-theme');
    });

    // CSS custom property values returned by getPropertyValue are the raw
    // declaration text — '#faa61a' verbatim, not the normalized rgb() form.
    // (Normalization only happens when the value is consumed as a property.)
    const value = await page.evaluate(() =>
      getComputedStyle(document.documentElement)
        .getPropertyValue('--state-encryption-pending')
        .trim()
    );

    expect(value).toBe('#faa61a');
  }
);

// ---------------------------------------------------------------------------
// Display type-scale × Font Size composition (#1035)
// ---------------------------------------------------------------------------

/**
 * The 4-step display scale is sized `Npx * var(--font-scale)`, so the Font Size
 * control must compose with it. jsdom can't resolve the cascade (the Vitest
 * file-parse test only locks the source formula); only real Chromium computes the
 * px. A probe element reads the resolved font-size: at data-fontsize='large'
 * (--font-scale-discrete 1.175, --ui-scale pinned to 1) xl → 32×1.175 = 37.6px,
 * lg → 24×1.175 = 28.2px.
 */
test(
  'display scale composes with Font Size — large × xl = 37.6px',
  { tag: '@renderer-only' },
  async ({ page }) => {
    await page.goto('/');

    const sizes = await page.evaluate(() => {
      document.documentElement.setAttribute('data-fontsize', 'large');
      // Pin --ui-scale so the assertion isolates Font Size from the UI-scale slider.
      document.documentElement.style.setProperty('--ui-scale', '1');
      const probe = document.createElement('div');
      document.body.appendChild(probe);
      const read = (token: string): string => {
        probe.style.fontSize = `var(${token})`;
        return getComputedStyle(probe).fontSize;
      };
      const out = { xl: read('--text-display-xl'), lg: read('--text-display-lg') };
      probe.remove();
      return out;
    });

    expect(parseFloat(sizes.xl), `xl resolved to ${sizes.xl}`).toBeCloseTo(37.6, 1); // 32 × 1.175
    expect(parseFloat(sizes.lg), `lg resolved to ${sizes.lg}`).toBeCloseTo(28.2, 1); // 24 × 1.175
  }
);

// ---------------------------------------------------------------------------
// Dyslexic Support reaches the display stack — cascade verification (#2366)
// ---------------------------------------------------------------------------

/**
 * The assertion Vitest structurally cannot make.
 *
 * `tests/unit/styles/design-tokens.test.ts` proves the sink's rules are PRESENT in
 * source; jsdom returns '' for every custom property regardless of what is declared, so
 * it can never prove one WINS. That distinction is the whole risk in this one rule.
 *
 * #2366 leaves the five other font ids on their original `[data-appfont='<id>'] body`
 * rules, which are attribute+type selectors that outrank the bare `body` rule outright
 * — no tie, nothing to verify at runtime. OpenDyslexic additionally redefines
 * `--font-display-stack`, and that is the one rule that MUST live at :root, because the
 * display font is a token each surface opts into by name rather than something
 * inherited. At :root it is (0,2,0), which merely TIES with
 * `[data-scheme='…'][data-theme='light']` and wins on source order alone.
 *
 * `agency-light` is the deliberate worst case on both axes: a two-attribute block, and
 * the ONLY scheme whose `--font-display-stack` diverges from the other 30 (Atkinson
 * Hyperlegible Next rather than Droidiga), so a sink that failed to win resolves to a
 * real, plausible, wrong font rather than to an empty string.
 */
const FONT_CASCADE_CASES = [
  { scheme: 'agency', theme: 'light', label: 'agency-light' },
  { scheme: 'agency', theme: null as string | null, label: 'agency-dark' },
  { scheme: 'concord', theme: 'light', label: 'concord-light' },
  { scheme: 'concord', theme: null as string | null, label: 'concord-dark' },
] as const;

async function applyFontContext(
  page: import('@playwright/test').Page,
  scheme: string,
  theme: string | null,
  appfont: string | null
): Promise<void> {
  await page.evaluate(
    ({ s, t, f }) => {
      const root = document.documentElement;
      root.setAttribute('data-scheme', s);
      if (t === null) root.removeAttribute('data-theme');
      else root.setAttribute('data-theme', t);
      if (f === null) delete root.dataset.appfont;
      else root.dataset.appfont = f;
    },
    { s: scheme, t: theme, f: appfont }
  );
}

const readDisplayStack = (page: import('@playwright/test').Page) =>
  page.evaluate(() =>
    getComputedStyle(document.documentElement).getPropertyValue('--font-display-stack').trim()
  );

/** The rendered body font — the inherited half, asserted as an OUTCOME not a token. */
const readBodyFont = (page: import('@playwright/test').Page) =>
  page.evaluate(() => getComputedStyle(document.body).fontFamily);

for (const combo of FONT_CASCADE_CASES) {
  test(
    `Dyslexic Support reaches the display stack in ${combo.label} (#2366)`,
    { tag: '@renderer-only' },
    async ({ page }) => {
      await page.goto('/');
      await applyFontContext(page, combo.scheme, combo.theme, 'opendyslexic');

      // The half that was already working: `[data-appfont] body` sets the body font,
      // and everything that inherits follows it.
      expect(await readBodyFont(page), `body font in ${combo.label}`).toContain('OpenDyslexic');

      // The half #2366 adds: headings, the server/channel nav and the titlebar read the
      // DISPLAY stack, so an accommodation that stopped at body left them in the brand
      // face. This is the assertion that fails if the :root rule loses its cascade tie.
      expect(await readDisplayStack(page), `display stack in ${combo.label}`).toContain(
        'OpenDyslexic'
      );
    }
  );
}

test(
  'an ordinary font pick moves BODY only, leaving the display face alone (#2366)',
  { tag: '@renderer-only' },
  async ({ page }) => {
    // The distinguishing control. Without it the tests above pass just as well on a
    // sink that overrode the display stack for EVERY font id — a different feature
    // (#2366's layered-assignment half). This is what makes "Dyslexic Support is the
    // one id that goes global" tellable apart from "all fonts go global".
    await page.goto('/');
    await applyFontContext(page, 'concord', 'light', 'inter');

    expect(await readBodyFont(page)).toContain('Inter');

    const display = await readDisplayStack(page);
    expect(display).toContain('Droidiga');
    expect(display).not.toContain('Inter');
  }
);

test(
  "'default' leaves the theme's own display face standing (#2366)",
  { tag: '@renderer-only' },
  async ({ page }) => {
    // 'default' is the "no explicit pick" sentinel and deliberately has no rule, so
    // Agency keeps its bundled display face rather than falling back to Droidiga.
    await page.goto('/');
    await applyFontContext(page, 'agency', 'light', null);

    expect(await readDisplayStack(page)).toContain('Atkinson');
  }
);
