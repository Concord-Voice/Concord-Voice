/**
 * WCAG 2.1 AA contrast guard for `--link-color` (and `--link-color-hover`)
 * against `--bg-primary` across all 30 theme blocks in `index.css`.
 *
 * ## Why this test exists
 *
 * PR #1090 (closed #800) introduced a global `a[target="_blank"]` rule that
 * consumes the `--link-color` / `--link-color-hover` design tokens from PR
 * #1076 (issue #1033). During manual QA, the Fox Den theme rendered a
 * muddy-orange link on warm-brown background that passed WCAG AA at 5.6:1
 * but read as low contrast perceptually. The Fox Den fix overrode `--link-color`
 * to complementary sky blue. The catch was accidental — the other 11 themes
 * were not spot-checked.
 *
 * This test asserts the WCAG AA contrast floor (4.5:1) for every theme block,
 * so a future theme author cannot silently ship a sub-AA link/bg pair.
 *
 * ## Why 4.5:1 and not 7:1 (AAA)
 *
 * WCAG 2.1 Success Criterion 1.4.3 — normal text minimum is 4.5:1.
 * AAA (7:1) is aspirational and would require redesigning several themes
 * whose `--accent-primary` doesn't reach 7:1 against any neutral background.
 * Tightening to AAA is a v1.1.0+ consideration.
 *
 * ## Implementation discipline
 *
 * - Mirrors `design-tokens.test.ts` — plain `indexOf` + string-scan parsing
 *   (no dynamic `RegExp`) so Semgrep's CWE-1333 ReDoS taint-sink rule is
 *   not triggered.
 * - No new runtime dependencies (no `postcss`).
 * - `extractBlockBody` is copy-pasted from the sibling test rather than
 *   factored into a shared helper module — the helper-module pattern is
 *   not yet established in `tests/unit/`, and a one-time duplication is
 *   cheaper than introducing a new convention.
 *
 * @see https://www.w3.org/WAI/WCAG21/Understanding/contrast-minimum.html
 * @see https://www.w3.org/WAI/GL/wiki/Relative_luminance
 */

import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

const WCAG_AA_THRESHOLD = 4.5;

/**
 * Exemption list for theme blocks whose `--link-color` / `--link-color-hover`
 * fall below the WCAG-AA threshold (4.5:1) against `--bg-primary`.
 *
 * As of #1182 this list is INTENTIONALLY EMPTY. The 24 previously-failing
 * (block, token) pairs across 13 themes were fixed with explicit per-theme
 * `--link-color` / `--link-color-hover` overrides in `index.css` (decoupled
 * from `--accent-primary`, mirroring the Fox Den precedent from #800 /
 * PR #1090). The guard now enforces AA on every block with zero exemptions
 * and zero `.skip`.
 *
 * The mechanism (`isExempt` + the `it.skip` branches below) is retained on
 * purpose: a future theme that regresses below AA — or a new block added
 * without a compliant link color — fails LOUD rather than being silently
 * skipped. The #1183 guard-expansion (non-text / focus-ring contrast) stacks
 * on top of it.
 *
 * **If an entry is ever re-added, do so ONLY as a temporary, issue-tracked
 * exemption, and remove it by fixing the theme's contrast — never by lowering
 * the threshold or carving out a per-theme exception.**
 */
const KNOWN_NONCOMPLIANT_BLOCKS: ReadonlyArray<{
  block: string;
  token: '--link-color' | '--link-color-hover';
  ratio: number;
}> = [
  // Intentionally empty as of #1182 — every block now meets AA in index.css.
  // See the exemption-list contract above for why the mechanism is retained.
];

function isExempt(block: string, token: '--link-color' | '--link-color-hover'): boolean {
  return KNOWN_NONCOMPLIANT_BLOCKS.some((e) => e.block === block && e.token === token);
}

const ALL_32_BLOCKS = [
  ':root',
  "[data-theme='light']",
  "[data-scheme='concord']",
  "[data-scheme='concord'][data-theme='light']",
  "[data-scheme='morky']",
  "[data-scheme='morky'][data-theme='light']",
  "[data-scheme='bardic']",
  "[data-scheme='bardic'][data-theme='light']",
  "[data-scheme='foxden']",
  "[data-scheme='foxden'][data-theme='light']",
  "[data-scheme='hacker']",
  "[data-scheme='hacker'][data-theme='light']",
  "[data-scheme='spooky']",
  "[data-scheme='spooky'][data-theme='light']",
  "[data-scheme='leviathan']",
  "[data-scheme='leviathan'][data-theme='light']",
  "[data-scheme='grassynill']",
  "[data-scheme='grassynill'][data-theme='light']",
  "[data-scheme='cottoncandy']",
  "[data-scheme='cottoncandy'][data-theme='light']",
  "[data-scheme='driftwood']",
  "[data-scheme='driftwood'][data-theme='light']",
  "[data-scheme='eclipse']",
  "[data-scheme='eclipse'][data-theme='light']",
  "[data-scheme='midnightsky']",
  "[data-scheme='midnightsky'][data-theme='light']",
  "[data-scheme='agency']",
  "[data-scheme='agency'][data-theme='light']",
  "[data-scheme='defacto']",
  "[data-scheme='defacto'][data-theme='light']",
  "[data-scheme='pride']",
  "[data-scheme='pride'][data-theme='light']",
] as const;

/**
 * Extract the body of a CSS block identified by `selector`.
 *
 * Uses plain string search (indexOf) rather than a dynamic RegExp so that
 * Semgrep's ReDoS taint-sink rule (CWE-1333) is not triggered. The selector
 * strings in ALL_32_BLOCKS are compile-time constants, but the static analyser
 * cannot prove that without data-flow analysis across array indexing.
 *
 * Algorithm:
 *   1. Find the first occurrence of `\n<selector> {` or `<selector> {` at
 *      position 0 (for `:root`).
 *   2. From the opening `{`, count braces until depth returns to 0.
 *   3. Return the substring between `{` and the matching `}` (exclusive).
 *
 * Returns null if the selector is not found or braces are unbalanced.
 */
function extractBlockBody(css: string, selector: string): string | null {
  const needle = `${selector} {`;
  const needleNl = `\n${selector} {`;

  let openBracePos: number;

  if (css.startsWith(needle)) {
    openBracePos = needle.length;
  } else {
    const idx = css.indexOf(needleNl);
    if (idx === -1) {
      const needleNoSp = `${selector}{`;
      const needleNlNoSp = `\n${selector}{`;
      const idx2 = css.startsWith(needleNoSp) ? 0 : css.indexOf(needleNlNoSp);
      if (idx2 === -1) {
        return null;
      }
      const rawNeedle = css.startsWith(needleNoSp) ? needleNoSp : needleNlNoSp;
      openBracePos = idx2 + rawNeedle.length;
    } else {
      openBracePos = idx + needleNl.length;
    }
  }

  let depth = 1;
  let pos = openBracePos;

  while (pos < css.length && depth > 0) {
    const ch = css[pos];
    if (ch === '{') {
      depth++;
    } else if (ch === '}') {
      depth--;
    }
    pos++;
  }

  if (depth !== 0) {
    return null;
  }

  return css.slice(openBracePos, pos - 1);
}

/**
 * Extract the right-hand side of a CSS custom-property declaration.
 *
 * Looks for `<token>:` or `<token> :` (the two forms also recognised by
 * design-tokens.test.ts), then reads characters until the next `;`.
 * Returns the value trimmed of surrounding whitespace, or null if the
 * token is not declared in the block.
 */
function extractDeclaration(blockBody: string, tokenName: string): string | null {
  const needles = [`${tokenName}:`, `${tokenName} :`];
  let startPos = -1;
  let needleLen = 0;
  for (const n of needles) {
    const idx = blockBody.indexOf(n);
    if (idx !== -1) {
      startPos = idx;
      needleLen = n.length;
      break;
    }
  }
  if (startPos === -1) {
    return null;
  }
  const semiPos = blockBody.indexOf(';', startPos + needleLen);
  if (semiPos === -1) {
    return null;
  }
  return blockBody.slice(startPos + needleLen, semiPos).trim();
}

/**
 * Resolve a CSS value to a terminal hex literal, following `var()` chains.
 *
 * Handles three cases:
 *   1. Direct hex literal (`#RRGGBB` or `#RGB`) — returned as-is.
 *   2. `var(--token)` — looks up `--token` in `blockBody`, then in
 *      `rootBody` as fallback (mirrors CSS cascade semantics), then
 *      recurses with `depth - 1`.
 *   3. Anything else (e.g., `rgb(...)`, `hsl(...)`, named color) —
 *      returns null. The current `index.css` only uses hex + var() for
 *      the tokens under test, so this is a structural restriction, not
 *      a feature limitation.
 *
 * Depth cap of 2 is sufficient for `--link-color` → `--accent-primary` → `#hex`
 * (the deepest chain in current `index.css`).
 */
function resolveToHex(
  value: string,
  blockBody: string,
  rootBody: string,
  depth = 2
): string | null {
  const trimmed = value.trim();

  // Case 1: direct hex — validate the shape (#RGB or #RRGGBB) so a malformed
  // `#`-value (e.g. `#xyz`, `#1234`) resolves to null and surfaces via the
  // caller's contextual "did not resolve to hex for block ..." guard, instead
  // of slipping through to an unscoped `Invalid hex color` throw from parseHex
  // downstream. Makes resolveToHex the true upstream gate parseHex documents
  // it to be (#1272 — enhanced-pr-review silent-failure finding).
  if (trimmed.startsWith('#')) {
    return isValidHexLiteral(trimmed) ? trimmed : null;
  }

  // Case 2: var() indirection
  if (trimmed.startsWith('var(')) {
    if (depth <= 0) {
      return null;
    }
    const closeIdx = trimmed.indexOf(')');
    if (closeIdx === -1) {
      return null;
    }
    const inner = trimmed.slice(4, closeIdx).trim();
    // var(--token) or var(--token, fallback) — extract both
    const commaIdx = inner.indexOf(',');
    const tokenName = (commaIdx === -1 ? inner : inner.slice(0, commaIdx)).trim();
    const fallbackValue = commaIdx === -1 ? null : inner.slice(commaIdx + 1).trim();

    const refBlock = extractDeclaration(blockBody, tokenName);
    if (refBlock !== null) {
      return resolveToHex(refBlock, blockBody, rootBody, depth - 1);
    }
    const refRoot = extractDeclaration(rootBody, tokenName);
    if (refRoot !== null) {
      return resolveToHex(refRoot, blockBody, rootBody, depth - 1);
    }
    // CSS var() fallback semantics: if token resolves to neither block nor
    // root, attempt the inline fallback. Per Gitar review on PR #1178 —
    // protects against latent test failures if a future theme declares
    // `--token: var(--custom, #abcdef)` where --custom is intentionally
    // undefined.
    if (fallbackValue !== null) {
      return resolveToHex(fallbackValue, blockBody, rootBody, depth - 1);
    }
    return null;
  }

  // Case 3: anything else
  return null;
}

/**
 * Compute the WCAG 2.1 contrast ratio between two hex colors.
 *
 * @param hexA - Color in `#RRGGBB` or `#RGB` form
 * @param hexB - Color in `#RRGGBB` or `#RGB` form
 * @returns Contrast ratio in [1.0, 21.0]
 *
 * Throws if either hex is malformed (defensive; should not happen since
 * `resolveToHex` is the upstream gate).
 */
function wcagContrast(hexA: string, hexB: string): number {
  const lumA = relativeLuminance(hexA);
  const lumB = relativeLuminance(hexB);
  const lighter = Math.max(lumA, lumB);
  const darker = Math.min(lumA, lumB);
  return (lighter + 0.05) / (darker + 0.05);
}

function relativeLuminance(hex: string): number {
  const { r, g, b } = parseHex(hex);
  const linR = channelToLinear(r);
  const linG = channelToLinear(g);
  const linB = channelToLinear(b);
  return 0.2126 * linR + 0.7152 * linG + 0.0722 * linB;
}

function channelToLinear(channel8bit: number): number {
  const c = channel8bit / 255;
  return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
}

function parseHex(hex: string): { r: number; g: number; b: number } {
  let h = hex.startsWith('#') ? hex.slice(1) : hex;
  // Expand #RGB to #RRGGBB
  if (h.length === 3) {
    h = h
      .split('')
      .map((c) => c + c)
      .join('');
  }
  if (h.length !== 6) {
    throw new Error(`Invalid hex color: ${hex}`);
  }
  const r = parseInt(h.slice(0, 2), 16);
  const g = parseInt(h.slice(2, 4), 16);
  const b = parseInt(h.slice(4, 6), 16);
  if (Number.isNaN(r) || Number.isNaN(g) || Number.isNaN(b)) {
    throw new Error(`Invalid hex color: ${hex}`);
  }
  return { r, g, b };
}

/**
 * True if `value` is a well-formed `#RGB` or `#RRGGBB` hex literal — the two
 * shapes `parseHex` accepts. Plain-string scan (no RegExp) to stay within this
 * file's Semgrep CWE-1333 discipline. Used by `resolveToHex` Case 1 to reject a
 * malformed `#`-value so the caller's block-context guard reports it, rather
 * than a downstream `parseHex` throw with no block name (#1272).
 */
function isValidHexLiteral(value: string): boolean {
  if (!value.startsWith('#')) {
    return false;
  }
  const body = value.slice(1);
  if (body.length !== 3 && body.length !== 6) {
    return false;
  }
  for (const ch of body) {
    const isHexDigit =
      (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F');
    if (!isHexDigit) {
      return false;
    }
  }
  return true;
}

describe('wcagContrast helper', () => {
  it('returns 21.0 for white-on-black (WCAG max)', () => {
    expect(wcagContrast('#ffffff', '#000000')).toBeCloseTo(21.0, 1);
  });

  it('returns 1.0 for identical colors (WCAG min)', () => {
    expect(wcagContrast('#7f7f7f', '#7f7f7f')).toBeCloseTo(1.0, 5);
  });

  it('returns ~9.5 for Fox Den dark link (#80d8ff on #3d1e00, known good)', () => {
    const ratio = wcagContrast('#80d8ff', '#3d1e00');
    expect(ratio).toBeGreaterThan(9.0);
    expect(ratio).toBeLessThan(10.0);
  });

  it('parses 3-char hex shorthand', () => {
    expect(wcagContrast('#fff', '#000')).toBeCloseTo(21.0, 1);
  });

  it('throws on malformed hex', () => {
    expect(() => wcagContrast('#zzz', '#000')).toThrow();
  });
});

describe('resolveToHex helper', () => {
  const rootBody = `
    --accent-primary: #fa709a;
    --link-color: var(--accent-primary);
  `;
  const themeBody = `
    --accent-primary: #ff6d00;
    --link-color: #80d8ff;
  `;

  it('returns direct hex literal as-is', () => {
    expect(resolveToHex('#ff6d00', themeBody, rootBody)).toBe('#ff6d00');
  });

  it('resolves one-level var() indirection', () => {
    expect(resolveToHex('var(--accent-primary)', themeBody, rootBody)).toBe('#ff6d00');
  });

  it('resolves var() chain through root fallback', () => {
    // theme has no --accent-primary, but root does
    const themeBodyNoAccent = `
      --link-color: var(--accent-primary);
    `;
    expect(resolveToHex('var(--accent-primary)', themeBodyNoAccent, rootBody)).toBe('#fa709a');
  });

  it('returns null when chain exceeds depth cap', () => {
    const deepRoot = `
      --a: var(--b);
      --b: var(--c);
      --c: var(--d);
      --d: #ffffff;
    `;
    expect(resolveToHex('var(--a)', '', deepRoot, 2)).toBeNull();
  });

  it('returns null for unresolved token', () => {
    expect(resolveToHex('var(--nonexistent)', themeBody, rootBody)).toBeNull();
  });

  it('resolves the var() fallback when the token is undefined (per Gitar #1178 review)', () => {
    // var(--unresolved, #abcdef) — neither blockBody nor rootBody declares
    // `--unresolved`, so the CSS fallback value `#abcdef` should be used.
    expect(resolveToHex('var(--unresolved, #abcdef)', themeBody, rootBody)).toBe('#abcdef');
  });

  it('returns null when fallback is unresolvable (e.g., a named color)', () => {
    // Named colors (e.g., `red`, `tomato`) are not supported by the parser —
    // it only handles hex literals and `var()` chains. A fallback that
    // resolves to a named color returns null, which is the safe failure mode.
    expect(resolveToHex('var(--unresolved, tomato)', themeBody, rootBody)).toBeNull();
  });

  it('returns null for a malformed hex literal so the caller reports block context (#1272)', () => {
    // Pre-#1272, Case 1 returned any `#`-prefixed string as-is, so a malformed
    // value bypassed the null guard and threw an unscoped `Invalid hex color`
    // from parseHex (no block name). Now it resolves to null and the caller's
    // contextual "did not resolve to hex for block ..." guard fires instead.
    expect(resolveToHex('#xyz', themeBody, rootBody)).toBeNull();
    expect(resolveToHex('#1234', themeBody, rootBody)).toBeNull();
    expect(resolveToHex('#11111111', themeBody, rootBody)).toBeNull();
  });

  it('still returns well-formed 3- and 6-digit hex literals as-is', () => {
    expect(resolveToHex('#fff', themeBody, rootBody)).toBe('#fff');
    expect(resolveToHex('#ff6d00', themeBody, rootBody)).toBe('#ff6d00');
  });
});

describe('WCAG-AA link contrast across all theme blocks', () => {
  const cssPath = resolve(__dirname, '../../../src/renderer/styles/index.css');
  const css = readFileSync(cssPath, 'utf-8');
  const rootBody = extractBlockBody(css, ':root') ?? '';

  describe.each(ALL_32_BLOCKS)('block %s', (block) => {
    const blockBody = extractBlockBody(css, block);

    it('block exists', () => {
      expect(blockBody).not.toBeNull();
    });

    it('--link-color resolves to a hex literal', () => {
      const linkValue = extractDeclaration(blockBody ?? '', '--link-color');
      expect(linkValue, `--link-color not declared in ${block}`).not.toBeNull();
      const linkHex = resolveToHex(linkValue ?? '', blockBody ?? '', rootBody);
      expect(
        linkHex,
        `--link-color in ${block} did not resolve to hex (value: ${linkValue})`
      ).not.toBeNull();
    });

    it('--bg-primary resolves to a hex literal', () => {
      const bgValue = extractDeclaration(blockBody ?? '', '--bg-primary');
      expect(bgValue, `--bg-primary not declared in ${block}`).not.toBeNull();
      const bgHex = resolveToHex(bgValue ?? '', blockBody ?? '', rootBody);
      expect(
        bgHex,
        `--bg-primary in ${block} did not resolve to hex (value: ${bgValue})`
      ).not.toBeNull();
    });

    const linkTest = isExempt(block, '--link-color') ? it.skip : it;
    linkTest(
      `--link-color has WCAG AA contrast ≥ ${WCAG_AA_THRESHOLD}:1 against --bg-primary`,
      () => {
        const linkValue = extractDeclaration(blockBody ?? '', '--link-color');
        const bgValue = extractDeclaration(blockBody ?? '', '--bg-primary');
        const linkHex = resolveToHex(linkValue ?? '', blockBody ?? '', rootBody);
        const bgHex = resolveToHex(bgValue ?? '', blockBody ?? '', rootBody);

        if (linkHex === null || bgHex === null) {
          throw new Error(
            `Could not resolve --link-color or --bg-primary to hex for block ${block}`
          );
        }

        const ratio = wcagContrast(linkHex, bgHex);
        expect(
          ratio,
          `Theme '${block}' link '${linkHex}' on bg '${bgHex}' = ${ratio.toFixed(2)}:1, below WCAG AA ${WCAG_AA_THRESHOLD}:1`
        ).toBeGreaterThanOrEqual(WCAG_AA_THRESHOLD);
      }
    );

    // The hover variant is optional: a block may inherit --link-color-hover from
    // :root (enforced by :root's own assertion). Gate at collection time so an
    // undeclared token reports as a visible SKIP, not a silent green pass (#1272
    // — honest-skip); an in-body early `return` would show a misleading pass.
    // Also skips KNOWN_NONCOMPLIANT exemptions.
    const hoverValue = extractDeclaration(blockBody ?? '', '--link-color-hover');
    const hoverTest = hoverValue === null || isExempt(block, '--link-color-hover') ? it.skip : it;
    hoverTest(
      `--link-color-hover has WCAG AA contrast ≥ ${WCAG_AA_THRESHOLD}:1 against --bg-primary`,
      () => {
        const bgValue = extractDeclaration(blockBody ?? '', '--bg-primary');
        const hoverHex = resolveToHex(hoverValue ?? '', blockBody ?? '', rootBody);
        const bgHex = resolveToHex(bgValue ?? '', blockBody ?? '', rootBody);

        if (hoverHex === null || bgHex === null) {
          throw new Error(
            `Could not resolve --link-color-hover or --bg-primary to hex for block ${block}`
          );
        }

        const ratio = wcagContrast(hoverHex, bgHex);
        expect(
          ratio,
          `Theme '${block}' link-hover '${hoverHex}' on bg '${bgHex}' = ${ratio.toFixed(2)}:1, below WCAG AA ${WCAG_AA_THRESHOLD}:1`
        ).toBeGreaterThanOrEqual(WCAG_AA_THRESHOLD);
      }
    );

    // #1183: body-text contrast guard. --text-primary / --text-secondary must meet
    // WCAG 2.1 AA (≥ 4.5:1 normal text) against --bg-primary. All 30 blocks already
    // pass (verified at authoring), so no exemption inventory is needed — these
    // assertions lock in compliance and catch future regressions. Only blocks that
    // *declare* the token are enforced (inherited values are covered by :root —
    // today all 30 blocks declare both, so every pair is checked directly).
    //
    // Deliberately scoped to text (the 4.5:1 threshold). The non-text surface —
    // accent/status/focus-ring colours at the 3:1 WCAG 1.4.11 threshold — is a
    // separate follow-up under #1183 (different threshold + per-pair design calls).
    for (const textToken of ['--text-primary', '--text-secondary'] as const) {
      // Gate at collection time so a token inherited from :root (undeclared in
      // this block) reports as a visible SKIP rather than a silent green pass
      // (#1272 — honest-skip); an in-body early `return` would show a misleading
      // pass.
      const textValue = extractDeclaration(blockBody ?? '', textToken);
      const textTest = textValue === null ? it.skip : it;
      textTest(
        `${textToken} has WCAG AA contrast ≥ ${WCAG_AA_THRESHOLD}:1 against --bg-primary`,
        () => {
          const bgValue = extractDeclaration(blockBody ?? '', '--bg-primary');
          const textHex = resolveToHex(textValue ?? '', blockBody ?? '', rootBody);
          const bgHex = resolveToHex(bgValue ?? '', blockBody ?? '', rootBody);

          if (textHex === null || bgHex === null) {
            throw new Error(
              `Could not resolve ${textToken} or --bg-primary to hex for block ${block}`
            );
          }

          const ratio = wcagContrast(textHex, bgHex);
          expect(
            ratio,
            `Theme '${block}' ${textToken} '${textHex}' on bg '${bgHex}' = ${ratio.toFixed(2)}:1, below WCAG AA ${WCAG_AA_THRESHOLD}:1`
          ).toBeGreaterThanOrEqual(WCAG_AA_THRESHOLD);
        }
      );
    }
  });
});

describe('failure-path: detects deliberately-bad link/bg pair', () => {
  // Synthetic fixture so we don't have to mutate index.css to verify
  // that the assertion fires with a useful message. Pair chosen to be
  // unambiguously below 4.5:1 — `#cc0000` on `#000000` computes to 3.57:1
  // (the same ratio as the production [data-scheme='eclipse'] failure
  // captured in KNOWN_NONCOMPLIANT_BLOCKS).
  const badFixture = `
:root {
  --bg-primary: #000000;
  --link-color: #cc0000;
}
  `;

  it('synthetic low-contrast pair produces sub-AA ratio', () => {
    const block = extractBlockBody(badFixture, ':root');
    expect(block).not.toBeNull();
    const linkHex = resolveToHex(
      extractDeclaration(block ?? '', '--link-color') ?? '',
      block ?? '',
      block ?? ''
    );
    const bgHex = resolveToHex(
      extractDeclaration(block ?? '', '--bg-primary') ?? '',
      block ?? '',
      block ?? ''
    );
    expect(linkHex).toBe('#cc0000');
    expect(bgHex).toBe('#000000');
    const ratio = wcagContrast(linkHex ?? '#000000', bgHex ?? '#000000');
    expect(ratio).toBeLessThan(WCAG_AA_THRESHOLD);
    // Spot-check the actual ratio matches the known-bad Eclipse value
    expect(ratio).toBeCloseTo(3.57, 1);
  });

  it('failure message format names theme, both hex, and actual ratio', () => {
    // Reconstruct the same shape the production assertion would produce
    // and verify a future maintainer / test author would get an actionable
    // string.
    const block = "[data-scheme='example']";
    const linkHex = '#cc0000';
    const bgHex = '#000000';
    const ratio = wcagContrast(linkHex, bgHex);
    const msg = `Theme '${block}' link '${linkHex}' on bg '${bgHex}' = ${ratio.toFixed(2)}:1, below WCAG AA ${WCAG_AA_THRESHOLD}:1`;
    expect(msg).toContain(block);
    expect(msg).toContain(linkHex);
    expect(msg).toContain(bgHex);
    expect(msg).toContain(':1');
    expect(msg).toContain(String(WCAG_AA_THRESHOLD));
  });
});

describe('exemption discipline', () => {
  // KNOWN_NONCOMPLIANT_BLOCKS is empty as of #1182, so these three invariant
  // checks currently pass vacuously. They are retained to harden the exemption
  // mechanism for any future (issue-tracked) re-population — e.g. the #1183
  // guard-expansion. See the exemption-list contract above the array.
  it('KNOWN_NONCOMPLIANT_BLOCKS contains no duplicates', () => {
    const seen = new Set<string>();
    for (const e of KNOWN_NONCOMPLIANT_BLOCKS) {
      const key = `${e.block}|${e.token}`;
      expect(seen.has(key), `Duplicate exemption: ${key}`).toBe(false);
      seen.add(key);
    }
  });

  it('every exempted block exists in ALL_32_BLOCKS', () => {
    const validBlocks = new Set<string>(ALL_32_BLOCKS);
    for (const e of KNOWN_NONCOMPLIANT_BLOCKS) {
      expect(
        validBlocks.has(e.block),
        `Exempted block '${e.block}' is not in ALL_32_BLOCKS — typo or stale entry`
      ).toBe(true);
    }
  });

  it('every exempted ratio is below the AA threshold (sanity-check exemption rationale)', () => {
    for (const e of KNOWN_NONCOMPLIANT_BLOCKS) {
      expect(
        e.ratio < WCAG_AA_THRESHOLD,
        `Exempted entry ${e.block}/${e.token} has ratio ${e.ratio} ≥ ${WCAG_AA_THRESHOLD} — should not be exempt`
      ).toBe(true);
    }
  });
});
