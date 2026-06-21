import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

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
  // Build the search needle: look for the selector followed by optional
  // whitespace and `{`.  We check both `\n<selector> {` and `<selector> {`
  // at position 0 to handle the first block in the file (`:root`).
  const needle = `${selector} {`;
  const needleNl = `\n${selector} {`;

  let openBracePos: number;

  if (css.startsWith(needle)) {
    openBracePos = needle.length; // points to char after `{`
  } else {
    const idx = css.indexOf(needleNl);
    if (idx === -1) {
      // Also try with no space before brace (e.g. `selector{`)
      const needleNoSp = `${selector}{`;
      const needleNlNoSp = `\n${selector}{`;
      const idx2 = css.startsWith(needleNoSp) ? 0 : css.indexOf(needleNlNoSp);
      if (idx2 === -1) {
        return null;
      }
      const rawNeedle = css.startsWith(needleNoSp) ? needleNoSp : needleNlNoSp;
      openBracePos = idx2 + rawNeedle.length;
    } else {
      openBracePos = idx + needleNl.length; // points to char after `{`
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

describe('design-token schema symmetry', () => {
  // From client/desktop/tests/unit/styles/ the relative path to
  // client/desktop/src/renderer/styles/index.css is ../../../src/renderer/styles/index.css
  const cssPath = resolve(__dirname, '../../../src/renderer/styles/index.css');
  const css = readFileSync(cssPath, 'utf-8');

  it('CSS file selector count matches ALL_32_BLOCKS array length (drift guard)', () => {
    // Counts top-level theme-block selectors. Without this check, adding a
    // 33rd block to index.css (e.g., a new color scheme) without updating
    // ALL_32_BLOCKS would let the per-block tests pass (existing 32 still
    // declared) while leaving the new block unchecked. This assertion makes
    // that drift loud.
    //
    // Literal regex (not constructed from variables) — Semgrep CWE-1333
    // ReDoS taint applies only to dynamic RegExp construction.
    const matches = css.match(/^(?::root|\[data-(?:scheme|theme)=)/gm);
    expect(matches?.length).toBe(ALL_32_BLOCKS.length);
  });

  describe.each(ALL_32_BLOCKS)('block %s', (block) => {
    const blockBody = extractBlockBody(css, block);

    it('block exists in styles/index.css', () => {
      expect(blockBody).not.toBeNull();
    });

    it.each(ALL_23_TOKENS)('declares %s', (token) => {
      // Use plain string search to avoid dynamic RegExp construction (Semgrep
      // CWE-1333).  CSS custom property declarations use `token:` or `token :`
      // — check both forms.  `token` comes from ALL_23_TOKENS (as const), so
      // it is a compile-time constant, but the static analyser treats any
      // function-parameter RegExp as a taint sink.
      const body = blockBody ?? '';
      const declared = body.includes(`${token}:`) || body.includes(`${token} :`);
      expect(declared).toBe(true);
    });
  });
});
