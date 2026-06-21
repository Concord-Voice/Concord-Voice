/**
 * High Contrast Mode cascade guard (#489).
 *
 * ## Why this test exists
 *
 * During #489 development the original HCM block was placed BEFORE the
 * per-scheme `[data-scheme='X']` blocks in `index.css`. At equal specificity
 * (0,1,0) the cascade is decided by source order — so every per-scheme block
 * silently won over the HCM override and the toggle had ~no visible effect.
 * The fix was twofold: move the HCM block to the END of the file, AND expand
 * the override from a 3-token sliver to a full palette replacement so that
 * downstream components (avatars, popups, gradients) actually re-tint.
 *
 * Both invariants are structural — a future refactor that reorders the file
 * or trims the HCM block would silently regress the toggle. This test locks
 * them as source-level assertions, mirroring the parse-based approach in
 * `link-contrast.test.ts` (no JSDOM cascade — JSDOM's `getComputedStyle`
 * support for custom properties is unreliable).
 *
 * ## What it asserts
 *
 *   1. Both HCM blocks exist (`[data-high-contrast='true']` and the compound
 *      `[data-high-contrast='true'][data-theme='light']`).
 *   2. Each block declares the load-bearing palette tokens that downstream
 *      components actually consume (`--bg-primary`, `--text-primary`,
 *      `--accent-primary`, `--on-accent`, `--border-color`).
 *   3. The terminal values are the expected stark hex literals — black/white
 *      backgrounds, saturated accents — NOT a sibling scheme's palette.
 *   4. Both HCM blocks appear AFTER every `[data-scheme='*']` declaration in
 *      source order, so the cascade resolves them last at equal specificity.
 *   5. The dark/light accent-on-bg pair meets WCAG AAA (≥ 7:1) — HCM is the
 *      accessibility-mode floor; anything weaker defeats the purpose.
 *
 * ## Why a parse-based source test, not a JSDOM render test
 *
 * - JSDOM's `getComputedStyle` returns custom-property values from inline
 *   styles only, not from stylesheet rules — verified during #489. A JSDOM
 *   test that toggles `data-high-contrast` and queries `--bg-primary` would
 *   return an empty string regardless of cascade correctness.
 * - The real cascade is a Chromium concern, exercised by Playwright E2E. A
 *   source-level structural test runs in milliseconds and catches the exact
 *   regression we already shipped once, which is the load-bearing failure mode.
 */

import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

const CSS_PATH = resolve(__dirname, '../../../src/renderer/styles/index.css');
const css = readFileSync(CSS_PATH, 'utf-8');

const HCM_DARK_SELECTOR = "[data-high-contrast='true']";
const HCM_LIGHT_SELECTOR = "[data-high-contrast='true'][data-theme='light']";

/**
 * Tokens whose presence in BOTH HCM blocks is structurally required because
 * downstream components consume them directly. If a future refactor trims any
 * of these, the toggle silently regresses for the corresponding surface
 * (e.g., dropping `--on-accent` re-introduces the white-text-on-yellow Apply
 * button bug fixed during #489 review).
 */
const REQUIRED_TOKENS = [
  '--bg-primary',
  '--bg-secondary',
  '--bg-tertiary',
  '--text-primary',
  '--text-secondary',
  '--text-muted',
  '--accent-primary',
  '--accent-secondary',
  '--on-accent',
  '--border-color',
  '--gradient-brand',
  '--success',
  '--danger',
  '--link-color',
] as const;

/**
 * Extract the body of a CSS block by exact selector. Plain-string scan to
 * match the Semgrep CWE-1333 discipline already followed by sibling tests in
 * this directory (no dynamic RegExp).
 */
function extractBlockBody(source: string, selector: string): string | null {
  const needle = `${selector} {`;
  const needleNl = `\n${selector} {`;
  let openBracePos: number;
  if (source.startsWith(needle)) {
    openBracePos = needle.length;
  } else {
    const idx = source.indexOf(needleNl);
    if (idx === -1) return null;
    openBracePos = idx + needleNl.length;
  }
  let depth = 1;
  let pos = openBracePos;
  while (pos < source.length && depth > 0) {
    const ch = source[pos];
    if (ch === '{') depth++;
    else if (ch === '}') depth--;
    pos++;
  }
  if (depth !== 0) return null;
  return source.slice(openBracePos, pos - 1);
}

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
  if (startPos === -1) return null;
  const semiPos = blockBody.indexOf(';', startPos + needleLen);
  if (semiPos === -1) return null;
  return blockBody.slice(startPos + needleLen, semiPos).trim();
}

// ── WCAG contrast helpers (copy-pasted from link-contrast.test.ts; the
// shared-helper-module pattern is not yet established in tests/unit/, and a
// second one-time duplication is cheaper than introducing a new convention.
// See the discussion at the top of link-contrast.test.ts.)

function parseHex(hex: string): { r: number; g: number; b: number } {
  let h = hex.startsWith('#') ? hex.slice(1) : hex;
  if (h.length === 3) {
    h = h
      .split('')
      .map((c) => c + c)
      .join('');
  }
  if (h.length !== 6) throw new Error(`Invalid hex color: ${hex}`);
  const r = parseInt(h.slice(0, 2), 16);
  const g = parseInt(h.slice(2, 4), 16);
  const b = parseInt(h.slice(4, 6), 16);
  if (Number.isNaN(r) || Number.isNaN(g) || Number.isNaN(b)) {
    throw new Error(`Invalid hex color: ${hex}`);
  }
  return { r, g, b };
}

function channelToLinear(c8: number): number {
  const c = c8 / 255;
  return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
}

function relativeLuminance(hex: string): number {
  const { r, g, b } = parseHex(hex);
  return 0.2126 * channelToLinear(r) + 0.7152 * channelToLinear(g) + 0.0722 * channelToLinear(b);
}

function wcagContrast(hexA: string, hexB: string): number {
  const lA = relativeLuminance(hexA);
  const lB = relativeLuminance(hexB);
  return (Math.max(lA, lB) + 0.05) / (Math.min(lA, lB) + 0.05);
}

describe('High Contrast Mode cascade (#489)', () => {
  describe('HCM blocks exist in index.css', () => {
    it('dark HCM block is declared', () => {
      expect(
        extractBlockBody(css, HCM_DARK_SELECTOR),
        `Missing block ${HCM_DARK_SELECTOR} in index.css`
      ).not.toBeNull();
    });

    it('light HCM compound block is declared', () => {
      expect(
        extractBlockBody(css, HCM_LIGHT_SELECTOR),
        `Missing compound block ${HCM_LIGHT_SELECTOR} in index.css`
      ).not.toBeNull();
    });
  });

  describe('HCM blocks declare every required palette token', () => {
    const darkBody = extractBlockBody(css, HCM_DARK_SELECTOR) ?? '';
    const lightBody = extractBlockBody(css, HCM_LIGHT_SELECTOR) ?? '';

    it.each(REQUIRED_TOKENS)('dark HCM block declares %s', (token) => {
      expect(
        extractDeclaration(darkBody, token),
        `${HCM_DARK_SELECTOR} must declare ${token} — downstream components consume it directly`
      ).not.toBeNull();
    });

    it.each(REQUIRED_TOKENS)('light HCM block declares %s', (token) => {
      expect(
        extractDeclaration(lightBody, token),
        `${HCM_LIGHT_SELECTOR} must declare ${token} — downstream components consume it directly`
      ).not.toBeNull();
    });
  });

  describe('HCM blocks resolve to stark, expected hex values', () => {
    const darkBody = extractBlockBody(css, HCM_DARK_SELECTOR) ?? '';
    const lightBody = extractBlockBody(css, HCM_LIGHT_SELECTOR) ?? '';

    it('dark HCM: --bg-primary is pure black', () => {
      expect(extractDeclaration(darkBody, '--bg-primary')).toBe('#000000');
    });

    it('dark HCM: --text-primary is pure white', () => {
      expect(extractDeclaration(darkBody, '--text-primary')).toBe('#ffffff');
    });

    it('dark HCM: --on-accent is black (legible on saturated yellow accent)', () => {
      // Regression guard for the white-text-on-yellow Apply button fixed
      // during #489 review. If --on-accent drifts back to #fff (or to a
      // theme-style off-white), the contrast collapses below WCAG AA on the
      // yellow accent.
      expect(extractDeclaration(darkBody, '--on-accent')).toBe('#000000');
    });

    it('light HCM: --bg-primary is pure white', () => {
      expect(extractDeclaration(lightBody, '--bg-primary')).toBe('#ffffff');
    });

    it('light HCM: --text-primary is pure black', () => {
      expect(extractDeclaration(lightBody, '--text-primary')).toBe('#000000');
    });

    it('light HCM: --on-accent is white (legible on saturated blue/purple accent)', () => {
      expect(extractDeclaration(lightBody, '--on-accent')).toBe('#ffffff');
    });
  });

  describe('cascade discipline: HCM blocks live after every per-scheme block', () => {
    // The bug this guards against: if HCM is declared BEFORE any
    // [data-scheme='X'] block, the per-scheme rule wins at equal specificity
    // by source order, silently defeating the toggle. This was the exact
    // failure mode during #489 development.
    it('dark HCM block source position is after the last [data-scheme=...] declaration', () => {
      const hcmDarkPos = css.indexOf(`${HCM_DARK_SELECTOR} {`);
      const lastSchemePos = css.lastIndexOf("[data-scheme='");
      expect(hcmDarkPos).toBeGreaterThan(-1);
      expect(lastSchemePos).toBeGreaterThan(-1);
      expect(
        hcmDarkPos,
        `HCM dark block (idx ${hcmDarkPos}) must appear AFTER the last [data-scheme='*'] block (idx ${lastSchemePos}) — otherwise the per-scheme rule wins the cascade at equal specificity.`
      ).toBeGreaterThan(lastSchemePos);
    });

    it('light HCM compound block source position is after the last [data-scheme=...] declaration', () => {
      const hcmLightPos = css.indexOf(`${HCM_LIGHT_SELECTOR} {`);
      const lastSchemePos = css.lastIndexOf("[data-scheme='");
      expect(hcmLightPos).toBeGreaterThan(-1);
      expect(lastSchemePos).toBeGreaterThan(-1);
      expect(
        hcmLightPos,
        `HCM light compound block (idx ${hcmLightPos}) must appear AFTER the last [data-scheme='*'] block (idx ${lastSchemePos}).`
      ).toBeGreaterThan(lastSchemePos);
    });

    it('dark HCM block precedes the light HCM compound block (so light correctly overrides dark)', () => {
      // Compound `[data-high-contrast='true'][data-theme='light']` has higher
      // specificity (0,2,0) than the base `[data-high-contrast='true']`
      // (0,1,0), but both blocks share token assignments. Source-order
      // discipline still matters for ergonomic editing — dark goes first.
      const darkPos = css.indexOf(`${HCM_DARK_SELECTOR} {`);
      const lightPos = css.indexOf(`${HCM_LIGHT_SELECTOR} {`);
      expect(darkPos).toBeGreaterThan(-1);
      expect(lightPos).toBeGreaterThan(-1);
      expect(lightPos).toBeGreaterThan(darkPos);
    });
  });

  describe('WCAG AAA accent contrast (HCM is the accessibility floor)', () => {
    const darkBody = extractBlockBody(css, HCM_DARK_SELECTOR) ?? '';
    const lightBody = extractBlockBody(css, HCM_LIGHT_SELECTOR) ?? '';

    it('dark HCM: --accent-primary on --bg-primary meets WCAG AAA (≥7:1)', () => {
      const accent = extractDeclaration(darkBody, '--accent-primary') ?? '';
      const bg = extractDeclaration(darkBody, '--bg-primary') ?? '';
      const ratio = wcagContrast(accent, bg);
      expect(
        ratio,
        `dark HCM accent '${accent}' on bg '${bg}' = ${ratio.toFixed(2)}:1, below WCAG AAA 7:1`
      ).toBeGreaterThanOrEqual(7);
    });

    it('dark HCM: --text-primary on --bg-primary meets WCAG AAA (≥7:1)', () => {
      const text = extractDeclaration(darkBody, '--text-primary') ?? '';
      const bg = extractDeclaration(darkBody, '--bg-primary') ?? '';
      const ratio = wcagContrast(text, bg);
      expect(
        ratio,
        `dark HCM text '${text}' on bg '${bg}' = ${ratio.toFixed(2)}:1, below WCAG AAA 7:1`
      ).toBeGreaterThanOrEqual(7);
    });

    it('light HCM: --accent-primary on --bg-primary meets WCAG AAA (≥7:1)', () => {
      const accent = extractDeclaration(lightBody, '--accent-primary') ?? '';
      const bg = extractDeclaration(lightBody, '--bg-primary') ?? '';
      const ratio = wcagContrast(accent, bg);
      expect(
        ratio,
        `light HCM accent '${accent}' on bg '${bg}' = ${ratio.toFixed(2)}:1, below WCAG AAA 7:1`
      ).toBeGreaterThanOrEqual(7);
    });

    it('light HCM: --text-primary on --bg-primary meets WCAG AAA (≥7:1)', () => {
      const text = extractDeclaration(lightBody, '--text-primary') ?? '';
      const bg = extractDeclaration(lightBody, '--bg-primary') ?? '';
      const ratio = wcagContrast(text, bg);
      expect(
        ratio,
        `light HCM text '${text}' on bg '${bg}' = ${ratio.toFixed(2)}:1, below WCAG AAA 7:1`
      ).toBeGreaterThanOrEqual(7);
    });

    it('dark HCM: --on-accent on --accent-primary meets WCAG AAA (legible Apply-button text)', () => {
      const onAccent = extractDeclaration(darkBody, '--on-accent') ?? '';
      const accent = extractDeclaration(darkBody, '--accent-primary') ?? '';
      const ratio = wcagContrast(onAccent, accent);
      expect(
        ratio,
        `dark HCM on-accent '${onAccent}' on accent '${accent}' = ${ratio.toFixed(2)}:1, below WCAG AAA 7:1 — Apply-button text would be illegible`
      ).toBeGreaterThanOrEqual(7);
    });

    it('light HCM: --on-accent on --accent-primary meets WCAG AAA (legible Apply-button text)', () => {
      const onAccent = extractDeclaration(lightBody, '--on-accent') ?? '';
      const accent = extractDeclaration(lightBody, '--accent-primary') ?? '';
      const ratio = wcagContrast(onAccent, accent);
      expect(
        ratio,
        `light HCM on-accent '${onAccent}' on accent '${accent}' = ${ratio.toFixed(2)}:1, below WCAG AAA 7:1`
      ).toBeGreaterThanOrEqual(7);
    });
  });

  describe('focus-ring boost is declared (3px outline for HCM users)', () => {
    it("an `*:focus-visible` rule is scoped under [data-high-contrast='true']", () => {
      // Looser shape-check — the rule lives outside a `{ ... }` token block
      // (it's a top-level selector, not a custom-property dictionary), so
      // extractBlockBody isn't a clean fit. A substring scan against the
      // canonical rule head is enough.
      const expected = "[data-high-contrast='true'] *:focus-visible";
      expect(
        css.includes(expected),
        `Missing focus-ring boost rule "${expected} { ... }" — HCM users rely on the thicker outline for keyboard navigation visibility.`
      ).toBe(true);
    });
  });
});
