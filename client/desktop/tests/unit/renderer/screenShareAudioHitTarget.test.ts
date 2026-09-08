import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// Comments are stripped BEFORE parsing. The rule regex captures a selector as
// "everything since the last brace", and a comment contains no braces — so a
// documented rule's captured selector is its comment plus the selector, and an
// exact-match lookup finds nothing at all.
const css = readFileSync(
  resolve(__dirname, '../../../src/renderer/components/Voice/ScreenShareAudioControls.css'),
  'utf-8'
).replace(/\/\*[\s\S]*?\*\//g, '');

/**
 * Asserted against the stylesheet: Vitest runs with `css: false` and jsdom does
 * no layout, so a getComputedStyle check on a hit target would pass regardless.
 */
function ruleBody(selector: string): string | null {
  const ruleRegex = /([^{}]+)\{([^{}]*)\}/g;
  let body: string | null = null;
  let match: RegExpExecArray | null;
  while ((match = ruleRegex.exec(css)) !== null) {
    // EXACT selector match, not a token match. `.screenshare-audio-controls__mute`
    // is a prefix of `...__mute::before`, `...__mute:disabled` and
    // `...__mute[aria-pressed='true']`, and a token boundary accepts `:` and `[`
    // — so a token match silently returned the aria-pressed rule's body and the
    // assertion read a colour declaration looking for `position`.
    const selectors = match[1].split(',').map((part) => part.trim());
    if (selectors.includes(selector)) body = match[2];
  }
  return body;
}

describe('screen-share audio control hit target', () => {
  it('expands the mute button to a 44x44 pointer target', () => {
    const body = ruleBody('.screenshare-audio-controls__mute::before');
    expect(body).not.toBeNull();
    expect(body as string).toMatch(/width:\s*44px/);
    expect(body as string).toMatch(/height:\s*44px/);
  });

  it('anchors that target on a positioned button, or it would escape to the page', () => {
    // An absolutely-positioned ::before resolves against the nearest positioned
    // ancestor. Without position:relative here it would centre on the stage, not
    // on the button — a 44x44 dead zone in the middle of the video.
    const body = ruleBody('.screenshare-audio-controls__mute');
    expect(body as string).toMatch(/position:\s*relative/);
  });

  it('keeps enough gap that the target cannot reach the slider', () => {
    // The target overhangs the 24px button by 10px per side; an 8px gap would put
    // it 2px inside the slider and steal the start of a drag.
    const body = ruleBody('.screenshare-audio-controls');
    const gap = /gap:\s*(\d+)px/.exec(body ?? '')?.[1];
    expect(Number(gap)).toBeGreaterThanOrEqual(10);
  });
});
