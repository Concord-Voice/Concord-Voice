import { readdirSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

/**
 * Locks the --accent-fill button/highlight indirection shipped with the Pride scheme.
 *
 * 23 component CSS files swapped `background: var(--accent-primary)` for
 * `background: var(--accent-fill)` so Pride can recolor solid fills to the rainbow
 * gradient without per-component overrides. That swap is only safe — visually a
 * no-op for the other 14 schemes — BECAUSE --accent-fill falls back to
 * --accent-primary in :root and is overridden ONLY by Pride. These assertions are
 * the safety net for that invariant: a regression here would silently recolor every
 * theme's buttons (drop the :root default → buttons go transparent everywhere; add a
 * stray per-scheme override → that scheme's buttons diverge from accent-primary).
 */

/**
 * Verified text-free --accent-fill surfaces: slider thumbs, toggle tracks, progress
 * fills, the drag-insertion ghost, active pills/ticks, the custom radio dot. They
 * carry NO foreground text, so the rainbow needs no legibility halo. The coverage
 * test below requires EVERY --accent-fill rule to be either haloed (text-bearing) or
 * listed here (text-free) — so a new surface can't drift past the scanner. When you
 * add a text-free fill, list it here; when you add a TEXT-bearing fill, add it to a
 * pride-flourishes.css halo group instead.
 */
const KNOWN_TEXT_FREE_FILLS = new Set([
  '.about-update-progress-fill',
  '.add-friend-toggle input:checked + .add-friend-toggle-track',
  '.channel-drag-ghost',
  '.force-update-progress-fill',
  // Friend-category DnD state indicators (#324): accent used as the drag-handle icon
  // color and the grabbed-section outline — decorative/text-free, no text on an accent fill.
  ".friend-category-drag-handle[aria-pressed='true']",
  '.friend-category-grabbed',
  '.image-crop-zoom-slider::-webkit-slider-thumb',
  '.media-controls-range::-webkit-slider-thumb',
  '.participant-volume-row-slider::-webkit-slider-thumb',
  '.permission-toggle.active',
  '.radio-inner',
  '.server-active-pill',
  '.settings-slider::-webkit-slider-thumb',
  '.settings-tier-slider::-webkit-slider-thumb',
  '.settings-tier-tick.active',
  '.settings-toggle input:checked + .settings-toggle-track',
  '.settings-volume-slider::-webkit-slider-thumb',
  '.sync-toggle.active',
  '.update-banner__progress',
]);

// Recursively collect every .css file under `dir`.
function walkCssFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = resolve(dir, entry.name);
    if (entry.isDirectory()) out.push(...walkCssFiles(full));
    else if (entry.name.endsWith('.css')) out.push(full);
  }
  return out;
}

// Strip /* ... */ comments so braces inside them can't confuse the rule scanner.
// Literal regex (not built from a variable) — Semgrep CWE-1333 ReDoS taint applies
// only to dynamic RegExp; `[\s\S]*?` is lazy + linear (no catastrophic backtracking).
function stripCssComments(css: string): string {
  return css.replace(/\/\*[\s\S]*?\*\//g, '');
}

// Yield { selector, body } per rule, recursing into at-rules (e.g. @media) so nested
// rules are reported with their own selector. Selector slices drop any leading
// at-statement (`@import ...;`) by taking the text after the last `;`.
function* cssRules(css: string): Generator<{ selector: string; body: string }> {
  let i = 0;
  while (i < css.length) {
    const open = css.indexOf('{', i);
    if (open === -1) break;
    const raw = css.slice(i, open);
    const selector = raw.slice(raw.lastIndexOf(';') + 1).trim();
    let depth = 1;
    let j = open + 1;
    while (j < css.length && depth > 0) {
      if (css[j] === '{') depth++;
      else if (css[j] === '}') depth--;
      j++;
    }
    const body = css.slice(open + 1, j - 1);
    if (selector.startsWith('@')) yield* cssRules(body);
    else yield { selector, body };
    i = j;
  }
}

// Canonicalize a combinator-free compound selector so authored token order is
// irrelevant (`.a.b` === `.b.a`, `:hover:focus` === `:focus:hover`). Tokenizes into
// simple selectors (`.class`, `#id`, `:pseudo`, `::pseudo`, `:pseudo(...)`, `[attr]`)
// and sorts them. Literal regex — no dynamic RegExp / ReDoS.
function canonicalizeSelector(selector: string): string {
  const tokens = selector.match(/::?[\w-]+(?:\([^)]*\))?|\.[\w-]+|#[\w-]+|\[[^\]]*\]/g);
  return tokens ? [...tokens].sort().join('') : selector;
}

describe('--accent-fill button/highlight indirection', () => {
  const stylesDir = resolve(__dirname, '../../../src/renderer/styles');
  const indexCss = readFileSync(resolve(stylesDir, 'index.css'), 'utf-8');
  const prideCss = readFileSync(resolve(stylesDir, 'pride-flourishes.css'), 'utf-8');

  it(':root defaults --accent-fill to --accent-primary (neutral for non-Pride schemes)', () => {
    expect(indexCss).toContain('--accent-fill: var(--accent-primary)');
  });

  it('--accent-fill is declared exactly once in index.css — only :root, no scheme block diverges', () => {
    // Literal regex (not constructed from variables) — Semgrep CWE-1333 ReDoS taint
    // applies only to dynamic RegExp construction. Matches the declaration form
    // (`--accent-fill:`), not the var(--accent-fill) usage sites.
    const declarations = indexCss.match(/--accent-fill\s*:/g) ?? [];
    expect(declarations.length).toBe(1);
  });

  it('Pride overrides --accent-fill to the rainbow brand gradient', () => {
    expect(prideCss).toContain('--accent-fill: var(--gradient-brand)');
  });

  it('the Pride override stylesheet is imported so the override actually loads', () => {
    const mainTsx = readFileSync(resolve(stylesDir, '../main.tsx'), 'utf-8');
    expect(mainTsx).toContain('pride-flourishes.css');
  });

  it('every --accent-fill surface is accounted for: text-bearing → matching halo, text-free → allowlisted', () => {
    // Derived inventory guard (no hardcoded expectation list): scan the component CSS
    // for EVERY rule whose background is the rainbow var(--accent-fill), and require
    // each to be classified —
    //   • white text (--on-accent)  → present in the dark-halo group
    //   • dark text  (--bg-primary) → present in the light-halo group
    //   • no co-located text color  → in KNOWN_TEXT_FREE_FILLS (verified text-free) OR
    //                                  a halo group (a split-rule text surface)
    // Membership is exact + order-canonicalized (not a substring match), so neither a
    // superstring (`.foo` vs `.foo-bar`) nor authored token order (`.a.b` vs `.b.a`)
    // can yield a false pass/fail. Any unclassified or un-haloed surface fails HERE —
    // closing both the separate-rule-color blind spot and the substring-match gap.
    const rendererDir = resolve(stylesDir, '..');
    const componentCss = walkCssFiles(rendererDir).filter(
      (p) => !p.endsWith('index.css') && !p.endsWith('pride-flourishes.css')
    );

    // Build the two halo membership sets (canonicalized) from pride-flourishes.css.
    const haloSet = (shadowColor: string): Set<string> => {
      const rule = [...cssRules(stripCssComments(prideCss))].find(
        (r) => r.body.includes('text-shadow') && r.body.includes(shadowColor)
      );
      return new Set(
        (rule?.selector ?? '')
          .split(',')
          .map((s) => s.trim().replace(/^\[data-scheme='pride'\]\s+/, ''))
          .filter(Boolean)
          .map(canonicalizeSelector)
      );
    };
    const darkHalo = haloSet('rgba(0, 0, 0');
    const lightHalo = haloSet('rgba(255, 255, 255');

    let accentFillSurfaces = 0;
    const missingHalo: string[] = [];
    const unclassified: string[] = [];

    for (const file of componentCss) {
      for (const { selector, body } of cssRules(stripCssComments(readFileSync(file, 'utf-8')))) {
        if (!body.includes('var(--accent-fill)')) continue;
        const white = body.includes('color: var(--on-accent)');
        const dark = body.includes('color: var(--bg-primary)');
        for (const sel of selector
          .split(',')
          .map((s) => s.trim())
          .filter(Boolean)) {
          accentFillSurfaces++;
          const canon = canonicalizeSelector(sel);
          if (white) {
            if (!darkHalo.has(canon)) missingHalo.push(`${sel} (white text → needs a dark halo)`);
          } else if (dark) {
            if (!lightHalo.has(canon)) missingHalo.push(`${sel} (dark text → needs a light halo)`);
          } else if (
            !KNOWN_TEXT_FREE_FILLS.has(sel) &&
            !darkHalo.has(canon) &&
            !lightHalo.has(canon)
          ) {
            unclassified.push(sel);
          }
        }
      }
    }

    // Guard against a vacuous pass if the scanner ever silently finds nothing.
    expect(accentFillSurfaces).toBeGreaterThanOrEqual(45);
    expect(
      missingHalo,
      `text-bearing --accent-fill surface(s) without a matching Pride legibility halo:\n  ${missingHalo.join('\n  ')}`
    ).toEqual([]);
    expect(
      unclassified,
      'un-haloed --accent-fill surface(s) with no co-located text color — add each to a ' +
        `pride-flourishes halo group (if it paints text) or KNOWN_TEXT_FREE_FILLS (if text-free):\n  ${unclassified.join('\n  ')}`
    ).toEqual([]);
  });
});
