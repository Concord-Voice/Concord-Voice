/**
 * Baron Neue must-not-reappear guard (#1643).
 *
 * Baron Neue (by Frank Hemmekam) is free for PERSONAL use only — a commercial-
 * licensing violation if bundled. It was removed in favor of its freely-commercial
 * sibling Droidiga. This guard fails if Baron Neue reappears as an @font-face /
 * font-family reference in index.css OR as a bundled asset filename.
 *
 * Source-level scan (mirrors display-font-token.test.ts #1034). Scoped to index.css
 * + the font asset dir ONLY — never docs/tests — so it does not self-trip on the
 * deliberate prose mentions (LICENSES.md removal note; App.test.tsx history comments).
 */
import { readFileSync, readdirSync } from 'node:fs';
import { resolve, join } from 'node:path';
import { describe, expect, it } from 'vitest';

const RENDERER = resolve(__dirname, '../../../src/renderer');
const INDEX_CSS = join(RENDERER, 'styles/index.css');
const FONTS_DIR = resolve(__dirname, '../../../public/branding/Concord-Voice/fonts');

const BARON = /baron[\s_-]?neue/i;

/** Line numbers + text of any Baron Neue reference in `css` (for the failure message). */
function baronOffenders(css: string): string[] {
  const out: string[] = [];
  css.split('\n').forEach((line, i) => {
    if (BARON.test(line)) out.push(`${i + 1}: ${line.trim()}`);
  });
  return out;
}

describe('Baron Neue must-not-reappear guard (#1643)', () => {
  it('index.css has no Baron Neue @font-face / font-family reference', () => {
    const offenders = baronOffenders(readFileSync(INDEX_CSS, 'utf-8'));
    expect(offenders, `Baron Neue reference(s) in index.css:\n${offenders.join('\n')}`).toEqual([]);
  });

  it('no bundled font asset filename contains "Baron"', () => {
    const offenders = readdirSync(FONTS_DIR).filter((name) => BARON.test(name));
    expect(offenders, `Baron Neue asset file(s):\n${offenders.join('\n')}`).toEqual([]);
  });

  it('the scanner is not vacuous (positive control)', () => {
    expect(baronOffenders("@font-face { font-family: 'Baron Neue'; }")).toHaveLength(1);
    expect(baronOffenders("body { font-family: 'BaronNeue', sans-serif; }")).toHaveLength(1);
    expect(baronOffenders("body { font-family: 'Droidiga', sans-serif; }")).toHaveLength(0);
  });
});
