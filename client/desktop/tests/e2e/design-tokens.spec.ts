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
// Application font layers — cascade verification (#2366)
// ---------------------------------------------------------------------------

/**
 * The assertions Vitest structurally cannot make (#2366): jsdom returns '' for every
 * custom property, so only a browser can prove which declaration WINS. Every font rule
 * is declared on body or a region root; these read computed values on those elements.
 * `agency-light` is the worst case: a two-attribute theme block, and the one scheme whose
 * display face (Atkinson) differs from the other 30 (Droidiga).
 */
const FONT_CASCADE_CASES = [
  { scheme: 'agency', theme: 'light', label: 'agency-light' },
  { scheme: 'agency', theme: null as string | null, label: 'agency-dark' },
  { scheme: 'concord', theme: 'light', label: 'concord-light' },
  { scheme: 'concord', theme: null as string | null, label: 'concord-dark' },
] as const;

type FontAttrs = Partial<
  Record<'appfont' | 'fontHeadings' | 'fontNav' | 'fontMessages' | 'fontBrand', string>
>;

async function applyFontContext(
  page: import('@playwright/test').Page,
  scheme: string,
  theme: string | null,
  attrs: FontAttrs
): Promise<void> {
  await page.evaluate(
    ({ s, t, a }) => {
      const root = document.documentElement;
      root.setAttribute('data-scheme', s);
      if (t === null) root.removeAttribute('data-theme');
      else root.setAttribute('data-theme', t);
      for (const key of ['appfont', 'fontHeadings', 'fontNav', 'fontMessages', 'fontBrand']) {
        const v = (a as Record<string, string | undefined>)[key];
        if (v === undefined) delete root.dataset[key];
        else root.dataset[key] = v;
      }
      // Probe elements for the regions; the login page renders none of them.
      // message-list-empty is the empty/loading state, a sibling of message-list.
      for (const cls of ['layout-channel-panel', 'message-list', 'message-list-empty']) {
        if (document.querySelector(`[data-probe='${cls}']`)) continue;
        const region = document.createElement('div');
        region.className = cls;
        region.dataset.probe = cls;
        const text = document.createElement('span');
        text.className = 'probe-text';
        text.textContent = 'x';
        const display = document.createElement('span');
        display.className = 'probe-display';
        display.style.fontFamily = 'var(--font-display-stack)';
        display.textContent = 'x';
        region.append(text, display);
        document.body.appendChild(region);
      }
      // A bare form control, which takes the system font unless something says inherit.
      if (!document.querySelector("[data-probe='control']")) {
        const control = document.createElement('button');
        control.dataset.probe = 'control';
        control.textContent = 'x';
        document.body.appendChild(control);
      }
      // A themed scope root, as useUserThemeScope renders another user's profile:
      // its data-scheme re-matches a theme block that re-declares the display stack.
      const scopes: [string, Element][] = [
        ['scope-body', document.body],
        ['scope-nav', document.querySelector("[data-probe='layout-channel-panel']")!],
        ['scope-msg', document.querySelector("[data-probe='message-list']")!],
      ];
      for (const [name, parent] of scopes) {
        if (document.querySelector(`[data-probe='${name}']`)) continue;
        const scope = document.createElement('div');
        scope.dataset.probe = name;
        scope.dataset.scheme = 'concord';
        scope.dataset.theme = 'light';
        const display = document.createElement('span');
        display.className = 'probe-display';
        display.style.fontFamily = 'var(--font-display-stack)';
        display.textContent = 'x';
        scope.append(display);
        parent.appendChild(scope);
      }
    },
    { s: scheme, t: theme, a: attrs }
  );
}

const probe = (page: import('@playwright/test').Page) =>
  page.evaluate(() => {
    const family = (sel: string) => {
      const el = document.querySelector(sel);
      return el ? getComputedStyle(el).fontFamily : '';
    };
    const bodyStyle = getComputedStyle(document.body);
    return {
      body: bodyStyle.fontFamily,
      display: bodyStyle.getPropertyValue('--font-display-stack').trim(),
      brand: bodyStyle.getPropertyValue('--font-brand-stack').trim(),
      navText: family("[data-probe='layout-channel-panel'] .probe-text"),
      navDisplay: family("[data-probe='layout-channel-panel'] .probe-display"),
      msgText: family("[data-probe='message-list'] .probe-text"),
      msgDisplay: family("[data-probe='message-list'] .probe-display"),
      emptyText: family("[data-probe='message-list-empty'] .probe-text"),
      emptyDisplay: family("[data-probe='message-list-empty'] .probe-display"),
      control: family("[data-probe='control']"),
      scopeBody: family("[data-probe='scope-body'] .probe-display"),
      scopeNav: family("[data-probe='scope-nav'] .probe-display"),
      scopeMsg: family("[data-probe='scope-msg'] .probe-display"),
    };
  });

const ALL_DYSLEXIC: FontAttrs = {
  appfont: 'opendyslexic',
  fontHeadings: 'opendyslexic',
  fontNav: 'opendyslexic',
  fontMessages: 'opendyslexic',
  fontBrand: 'opendyslexic',
};

for (const combo of FONT_CASCADE_CASES) {
  test(
    `Dyslexic Support reaches every layer and the wordmark in ${combo.label} (#2366)`,
    { tag: '@renderer-only' },
    async ({ page }) => {
      await page.goto('/');
      await applyFontContext(page, combo.scheme, combo.theme, ALL_DYSLEXIC);
      const p = await probe(page);
      for (const [layer, value] of Object.entries(p)) {
        expect(value, `${layer} in ${combo.label}`).toContain('OpenDyslexic');
      }
    }
  );
}

test(
  'One Font: a pick moves body, headings and both regions; the wordmark stays brand (#2366)',
  { tag: '@renderer-only' },
  async ({ page }) => {
    await page.goto('/');
    await applyFontContext(page, 'concord', 'light', {
      appfont: 'inter',
      fontHeadings: 'inter',
      fontNav: 'default',
      fontMessages: 'default',
      fontBrand: 'default',
    });
    const p = await probe(page);
    expect(p.body).toContain('Inter');
    expect(p.control).toContain('Inter');
    expect(p.display).toContain('Inter');
    expect(p.navText).toContain('Inter');
    expect(p.navDisplay).toContain('Inter');
    expect(p.msgText).toContain('Inter');
    // Another user's themed profile surface still shows the viewer's headings font.
    expect(p.scopeBody).toContain('Inter');
    expect(p.scopeNav).toContain('Inter');
    expect(p.scopeMsg).toContain('Inter');
    expect(p.brand).toContain('Droidiga');
    expect(p.brand).not.toContain('Inter');
  }
);

test(
  'Font by Area: each region keeps its own font for text AND headers (#2366)',
  { tag: '@renderer-only' },
  async ({ page }) => {
    await page.goto('/');
    await applyFontContext(page, 'agency', 'light', {
      appfont: 'atkinson',
      fontHeadings: 'lexend',
      fontNav: 'inter',
      fontMessages: 'lato',
      fontBrand: 'default',
    });
    const p = await probe(page);
    expect(p.display).toContain('Lexend');
    expect(p.navText).toContain('Inter');
    expect(p.navDisplay).toContain('Inter');
    expect(p.msgText).toContain('Lato');
    expect(p.msgDisplay).toContain('Lato');
    // An empty or loading channel shows the Messages font too.
    expect(p.emptyText).toContain('Lato');
    expect(p.emptyDisplay).toContain('Lato');
    // A themed scope root follows Headings outside the regions and the area inside them.
    expect(p.scopeBody).toContain('Lexend');
    expect(p.scopeNav).toContain('Inter');
    expect(p.scopeMsg).toContain('Lato');
    // Agency's own display face, unaffected by the Headings pick declared on body.
    expect(p.brand).toContain('Atkinson');
  }
);

test(
  'Concord Voice Default pins the base pair on a bundling theme: Source Sans + Droidiga',
  { tag: '@renderer-only' },
  async ({ page }) => {
    await page.goto('/');
    // What the resolver emits for appFont 'sourcesans' under Agency (headings 'concord').
    await applyFontContext(page, 'agency', 'light', {
      appfont: 'sourcesans',
      fontHeadings: 'concord',
      fontNav: 'default',
      fontMessages: 'default',
      fontBrand: 'default',
    });
    const p = await probe(page);
    expect(p.body).toContain('SourceSans');
    // Agency's own display face is Atkinson; the pin must beat it on body and inside regions.
    expect(p.display).toContain('Droidiga');
    expect(p.display).not.toContain('Atkinson');
    expect(p.navDisplay).toContain('Droidiga');
    expect(p.msgDisplay).toContain('Droidiga');
    // The wordmark keeps the theme's face; only Dyslexic Support moves it.
    expect(p.brand).toContain('Atkinson');
  }
);

test(
  "'default' everywhere leaves the theme's faces standing (#2366)",
  { tag: '@renderer-only' },
  async ({ page }) => {
    await page.goto('/');
    await applyFontContext(page, 'agency', 'light', {});
    const p = await probe(page);
    expect(p.display).toContain('Atkinson');
    expect(p.brand).toContain('Atkinson');
    expect(p.navDisplay).toContain('Atkinson');
    // With no pick, the viewed user's theme keeps its own display face.
    expect(p.scopeBody).toContain('Droidiga');
  }
);
