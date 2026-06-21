/**
 * 4-step display type-scale guard (#1035).
 *
 * A display typography scale (--text-display-xl/lg/md/sm) lives in :root + all 30
 * theme blocks, each sized off --font-scale so the Font Size control
 * (small/large) composes. Hero surfaces (modal titles, the voice pre-join hero,
 * auth screen titles, settings section headers) migrate onto the tokens with
 * size-appropriate tracking (tight for xl/lg, the looser default for md).
 *
 * These are SOURCE-level assertions, not render/computed-style checks: jsdom's
 * getComputedStyle does not resolve CSS custom properties declared in
 * stylesheets, so a render-based font-size assertion would be vacuous (same
 * rationale as display-font-token.test.ts / link-contrast.test.ts). The real
 * cascade is a Chromium concern covered by Playwright; this locks the structural
 * invariants in milliseconds.
 *
 * Per-block PRESENCE of the four tokens is drift-guarded separately by
 * design-tokens.test.ts (ALL_23_TOKENS). This file locks the token VALUES and
 * the per-surface migrations. No dynamic RegExp (Semgrep CWE-1333 discipline,
 * matching the sibling style tests).
 */

import { readFileSync } from 'node:fs';
import { resolve, join } from 'node:path';
import { describe, expect, it } from 'vitest';

const RENDERER = resolve(__dirname, '../../../src/renderer');
const INDEX_CSS = readFileSync(join(RENDERER, 'styles/index.css'), 'utf-8');

/** Body of the CSS rule beginning at `${selector} {` (plain string scan). */
function ruleBlock(css: string, selector: string): string {
  const start = css.indexOf(`${selector} {`);
  if (start === -1) return '';
  return css.slice(start, css.indexOf('}', start));
}

describe('display type-scale tokens (#1035)', () => {
  describe('token values — px × var(--font-scale) so Font Size composes', () => {
    const cases: ReadonlyArray<[token: string, value: string]> = [
      ['--text-display-xl', 'calc(32px * var(--font-scale, 1))'],
      ['--text-display-lg', 'calc(24px * var(--font-scale, 1))'],
      ['--text-display-md', 'calc(20px * var(--font-scale, 1))'],
      ['--text-display-sm', 'calc(16px * var(--font-scale, 1))'],
    ];

    it.each(cases)('%s = %s', (token, value) => {
      // Values are uniform across all 30 blocks, so a single containment check
      // confirms the declared value (presence-per-block is design-tokens.test's job).
      expect(INDEX_CSS).toContain(`${token}: ${value};`);
    });
  });

  it('font-scale large (×1.175) composes with xl → 37.6px', () => {
    // The token is `32px * var(--font-scale)`; data-fontsize='large' sets
    // --font-scale to 1.175, so xl resolves to 37.6px. jsdom can't resolve the
    // cascade, so assert the multiplicand + arithmetic the resolution depends on.
    expect(INDEX_CSS).toContain('--text-display-xl: calc(32px * var(--font-scale, 1));');
    expect(32 * 1.175).toBeCloseTo(37.6, 5);
  });

  describe('migrated hero surfaces use the tokens + size-appropriate tracking', () => {
    const cases: ReadonlyArray<{
      file: string;
      selector: string;
      size: string;
      tracking: string;
    }> = [
      { file: 'components/ui/Modal.css', selector: '.modal-title', size: '--text-display-lg', tracking: '--font-display-tracking-tight' }, // prettier-ignore
      { file: 'components/Voice/VoiceView.css', selector: '.voice-view__join-title', size: '--text-display-lg', tracking: '--font-display-tracking-tight' }, // prettier-ignore
      { file: 'components/Auth/Login.css', selector: '.login-title', size: '--text-display-xl', tracking: '--font-display-tracking-tight' }, // prettier-ignore
      { file: 'components/Auth/Register.css', selector: '.register-title', size: '--text-display-xl', tracking: '--font-display-tracking-tight' }, // prettier-ignore
      { file: 'components/Auth/EmailVerification.css', selector: '.email-verification-header h2', size: '--text-display-xl', tracking: '--font-display-tracking-tight' }, // prettier-ignore
      { file: 'components/Auth/ChangeEmail.css', selector: '.change-email-form h2', size: '--text-display-xl', tracking: '--font-display-tracking-tight' }, // prettier-ignore
      { file: 'components/Settings/SettingsPage.css', selector: '.settings-section-title', size: '--text-display-md', tracking: '--font-display-tracking' }, // prettier-ignore
    ];

    it.each(cases)(
      '$selector → font-size var($size) + tracking var($tracking)',
      ({ file, selector, size, tracking }) => {
        const block = ruleBlock(readFileSync(join(RENDERER, file), 'utf-8'), selector);
        expect(block, `selector ${selector} not found in ${file}`).not.toBe('');
        expect(block).toContain(`font-size: var(${size})`);
        expect(block).toContain(`letter-spacing: var(${tracking})`);
      }
    );
  });

  it('ex-hardcoded auth headings no longer ship a raw px font-size', () => {
    // EmailVerification + ChangeEmail headings were hardcoded (24px / 22px,
    // NOT font-scale-aware) before #1035; the migration also fixed that.
    for (const [file, selector] of [
      ['components/Auth/EmailVerification.css', '.email-verification-header h2'],
      ['components/Auth/ChangeEmail.css', '.change-email-form h2'],
    ] as const) {
      const block = ruleBlock(readFileSync(join(RENDERER, file), 'utf-8'), selector);
      expect(block, `${selector} still has a px font-size`).not.toMatch(/font-size:\s*\d+px/);
    }
  });
});
