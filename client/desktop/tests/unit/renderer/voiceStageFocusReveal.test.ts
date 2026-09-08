import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

const voiceStageCss = readFileSync(
  resolve(__dirname, '../../../src/renderer/components/Voice/VoiceStage.css'),
  'utf-8'
);

/**
 * True when `css` contains a rule revealing `controlClass` (setting
 * `opacity: 1`) via a keyboard-focus pseudo-class (`:focus`, `:focus-within`,
 * or `:focus-visible`) on the control itself or an ancestor. A rule that
 * reveals the control only via `:hover` does not count — that is exactly
 * the WCAG 2.4.7 gap under test.
 */
function revealsOnFocus(css: string, controlClass: string): boolean {
  const ruleRegex = /([^{}]+)\{([^{}]*)\}/g;
  let match: RegExpExecArray | null;
  while ((match = ruleRegex.exec(css)) !== null) {
    const [, selectorList, body] = match;
    // COMPLETE class token, not a substring. `.voice-stage__nav` must not be
    // satisfied by a rule that only targets `.voice-stage__nav--prev`, or a fix
    // applied to one modifier would green the case while the other control stays
    // invisible — vacuity mode 4, "one of N call sites".
    const tokenRe = new RegExp(controlClass.replace('.', '\\.') + '(?![\\w-])');
    if (!tokenRe.test(selectorList)) continue;
    if (!/:focus(-within|-visible)?\b/.test(selectorList)) continue;
    if (!/opacity:\s*1\b/.test(body)) continue;
    return true;
  }
  return false;
}

describe('VoiceStage overlay controls reveal on keyboard focus (WCAG 2.4.7)', () => {
  it('__layout-toggle reveals on hover only', () => {
    expect(
      revealsOnFocus(voiceStageCss, '.voice-stage__layout-toggle'),
      '.voice-stage__layout-toggle reveals on hover only — no :focus-within/:focus-visible rule sets opacity: 1'
    ).toBe(true);
  });

  it('__nav reveals on hover only', () => {
    expect(
      revealsOnFocus(voiceStageCss, '.voice-stage__nav'),
      '.voice-stage__nav reveals on hover only — no :focus-within/:focus-visible rule sets opacity: 1'
    ).toBe(true);
  });

  it('__pip-btn reveals on hover only', () => {
    expect(
      revealsOnFocus(voiceStageCss, '.voice-stage__pip-btn'),
      '.voice-stage__pip-btn reveals on hover only — no :focus-within/:focus-visible rule sets opacity: 1'
    ).toBe(true);
  });
});
