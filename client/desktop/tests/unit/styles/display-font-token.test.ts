/**
 * Display-font tokenization guard (#1034).
 *
 * After #1034, every display-font *usage* in the renderer resolves via the
 * `--font-display-stack` token (defined per-theme in index.css) instead of a
 * hardcoded `'Droidiga'` literal — so a future theme can swap the display
 * typeface without touching component CSS.
 *
 * These are source-level assertions, not render/computed-style checks:
 * jsdom's `getComputedStyle` does not resolve CSS custom properties declared
 * in stylesheets, so a render-based `font-family` assertion would be vacuous
 * (same rationale as `high-contrast-cascade.test.ts`). The real cascade is a
 * Chromium concern covered by Playwright; this locks the structural
 * invariants in milliseconds.
 *
 * Invariants:
 *   1. `--font-display-stack` is defined in index.css and currently maps to
 *      the Droidiga face.
 *   2. NO component CSS hardcodes a display-font *usage* (`'Droidiga',` — i.e.
 *      the literal followed by a fallback). The ONLY permitted `'Droidiga'`
 *      literals are the two `@font-face` *definitions* (`font-family:
 *      'Droidiga';`, no comma) in index.css and Titlebar.css. A reintroduced
 *      hardcoded usage silently bypasses the token and breaks theme override.
 *   3. The hierarchical surfaces migrated by #1034 reference the token.
 */

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { resolve, join } from 'node:path';
import { describe, expect, it } from 'vitest';

const RENDERER = resolve(__dirname, '../../../src/renderer');
const INDEX_CSS = join(RENDERER, 'styles/index.css');

/** Recursively collect every .css file under a directory. */
function cssFilesUnder(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      out.push(...cssFilesUnder(full));
    } else if (entry.endsWith('.css')) {
      out.push(full);
    }
  }
  return out;
}

describe('display-font tokenization (#1034)', () => {
  it('defines --font-display-stack mapped to the Droidiga face in index.css', () => {
    const css = readFileSync(INDEX_CSS, 'utf-8');
    expect(css).toMatch(/--font-display-stack:\s*'Droidiga'[^;]*;/);
  });

  it('has no hardcoded display-font usages — every usage goes through the token', () => {
    // A *usage* is `'Droidiga'` followed by a comma (it has a fallback stack).
    // A *definition* is `font-family: 'Droidiga';` (no comma) inside @font-face.
    const offenders: string[] = [];
    for (const file of cssFilesUnder(join(RENDERER, 'components'))) {
      const css = readFileSync(file, 'utf-8');
      css.split('\n').forEach((line, i) => {
        if (line.includes("'Droidiga',")) {
          offenders.push(`${file.replace(RENDERER, 'src/renderer')}:${i + 1}: ${line.trim()}`);
        }
      });
    }
    expect(
      offenders,
      `Hardcoded display-font usage(s) found — replace with var(--font-display-stack):\n${offenders.join('\n')}`
    ).toEqual([]);
  });

  it('permits the @font-face definitions only (Titlebar.css re-declares the face)', () => {
    // Sanity: the Titlebar @font-face is the one allowed non-index 'Droidiga'
    // literal. It must be a definition (no comma), not a usage.
    const titlebar = readFileSync(join(RENDERER, 'components/Titlebar/Titlebar.css'), 'utf-8');
    expect(titlebar).toContain("font-family: 'Droidiga';");
    expect(titlebar).not.toContain("'Droidiga',");
  });

  describe('migrated hierarchical surfaces reference the token', () => {
    const cases: ReadonlyArray<{ file: string; selector: string }> = [
      { file: 'components/Chat/Message.css', selector: '.message-username' },
      { file: 'components/Voice/VoiceView.css', selector: '.voice-view__channel-name' },
      { file: 'components/Channels/ChannelList.css', selector: '.channel-group-header span' },
      { file: 'components/Members/MemberList.css', selector: '.member-group-header span' },
      { file: 'components/Layout/ServerBar.css', selector: '.server-bar-tooltip-name' },
      { file: 'components/Auth/EmailVerification.css', selector: '.email-verification-header h2' },
      { file: 'components/Auth/ChangeEmail.css', selector: '.change-email-form h2' },
    ];

    it.each(cases)('$selector ($file) uses var(--font-display-stack)', ({ file, selector }) => {
      const css = readFileSync(join(RENDERER, file), 'utf-8');
      // Extract the selector's block and assert it references the token.
      const start = css.indexOf(`${selector} {`);
      expect(start, `selector ${selector} not found in ${file}`).toBeGreaterThanOrEqual(0);
      const block = css.slice(start, css.indexOf('}', start));
      expect(block).toContain('var(--font-display-stack)');
    });
  });
});
