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
 * The fix, applied during #489, changed the rule to `color: var(--bg-primary)`,
 * which is at the opposite end of the brightness spectrum in every theme
 * (black on yellow/cyan in HCM dark; white on blue/purple in HCM light;
 * theme-appropriate near-black/near-white in every non-HCM scheme). This
 * pattern is the same one used by `.avatar-circle` in `Message.css`.
 *
 * ## What this test asserts
 *
 *   1. The `.participant-tile__avatar-fallback` rule declares
 *      `color: var(--bg-primary)` — NOT `#fff`, `white`, or any other
 *      hardcoded literal that would defeat HCM and other dark schemes.
 *   2. The rule's `background` is `var(--gradient-brand)` (the load-bearing
 *      pairing — if the background changes, the color choice may need to
 *      change with it).
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

  it('color is var(--bg-primary) — not a hardcoded literal', () => {
    // The bug: `color: #fff` (or `white`) was unreadable against the HCM
    // bright-yellow gradient start-stop. The fix uses `var(--bg-primary)` so
    // the letter color flips with the theme (black in HCM dark, white in HCM
    // light), staying at the opposite end of the brightness spectrum from
    // `--gradient-brand` in every theme.
    const color = extractDeclaration(body ?? '', 'color');
    expect(color, `${SELECTOR} must declare a color`).not.toBeNull();
    expect(
      color,
      `${SELECTOR} color must be var(--bg-primary) — hardcoded white collapses to ~1:1 against the HCM yellow accent. Got: '${color}'`
    ).toBe('var(--bg-primary)');
  });

  it('background is var(--gradient-brand) — the load-bearing pairing', () => {
    // If the background changes away from --gradient-brand, the `color:
    // var(--bg-primary)` choice may need to change with it. This guard makes
    // such a change visible in review rather than silent.
    const background = extractDeclaration(body ?? '', 'background');
    expect(background, `${SELECTOR} must declare a background`).not.toBeNull();
    expect(background).toBe('var(--gradient-brand)');
  });
});
