/**
 * ParticipantTile avatar-fallback contrast guard (#489).
 *
 * ## Why this test exists
 *
 * The `.participant-tile__avatar-fallback` element renders the user's initials
 * on top of `var(--gradient-brand)` — a saturated brand gradient that, in HCM,
 * becomes bright yellow → cyan (dark mode) or blue → purple (light mode). The
 * fallback's `color` was originally hardcoded to `#fff`, which collapses
 * against the HCM yellow start-stop to ~1.07:1 (effectively unreadable).
 *
 * #489 changed the rule to `color: var(--bg-primary)`, on the claim that it
 * sits at the opposite end of the brightness range in every theme. It did not:
 * once the contrast ratchet learned to measure gradients, the same pair read
 * 1.20:1 in Concord light and below 4.5:1 in 18 of the 32 scheme and theme
 * combinations. Brand-filled controls now take `--brand-fill` and `--on-brand`,
 * a pair each theme block chooses to hold 4.5:1 across the whole fill, and the
 * ratchet measures it in every block.
 *
 * ## What this test asserts
 *
 *   1. The `.participant-tile__avatar-fallback` rule declares
 *      `color: var(--on-brand)` — NOT `#fff`, `white`, any other hardcoded
 *      literal, or a background token that only happens to contrast.
 *   2. The rule's `background` is `var(--brand-fill)`, the fill that
 *      `--on-brand` is chosen against.
 *
 * A source-inspection test is the right shape here: JSDOM does not resolve
 * custom-property values from stylesheet rules (only inline styles), so a
 * render test would silently pass regardless of the actual declaration.
 */

import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

const CSS_PATH = resolve(__dirname, '../../../src/renderer/components/Voice/ParticipantTile.css');
const css = readFileSync(CSS_PATH, 'utf-8');

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

describe('ParticipantTile avatar-fallback HCM contrast (#489)', () => {
  const SELECTOR = '.participant-tile__avatar-fallback';
  const body = extractBlockBody(css, SELECTOR);

  it(`${SELECTOR} rule exists`, () => {
    expect(body, `Missing rule ${SELECTOR} in ParticipantTile.css`).not.toBeNull();
  });

  it('color is var(--on-brand) — not a hardcoded literal', () => {
    // The first bug: `color: #fff` read ~1.07:1 on the HCM yellow stop. The
    // second: `var(--bg-primary)` read 1.20:1 in Concord light. --on-brand is
    // chosen per theme block against the whole fill.
    const color = extractDeclaration(body ?? '', 'color');
    expect(color, `${SELECTOR} must declare a color`).not.toBeNull();
    expect(
      color,
      `${SELECTOR} color must be var(--on-brand) — it is the colour chosen for the brand fill. Got: '${color}'`
    ).toBe('var(--on-brand)');
  });

  it('background is var(--brand-fill) — the load-bearing pairing', () => {
    // --on-brand is chosen against --brand-fill. A different background would
    // need its own foreground, so this guard makes such a change visible.
    const background = extractDeclaration(body ?? '', 'background');
    expect(background, `${SELECTOR} must declare a background`).not.toBeNull();
    expect(background).toBe('var(--brand-fill)');
  });
});
