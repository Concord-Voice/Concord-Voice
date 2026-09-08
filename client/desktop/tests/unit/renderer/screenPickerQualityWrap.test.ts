import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

const pickerCss = readFileSync(
  resolve(__dirname, '../../../src/renderer/components/Voice/ScreenSharePicker.css'),
  'utf-8'
);

/**
 * Body of the last rule whose selector list carries `selector` as a COMPLETE
 * token. The token boundary matters here more than usual: `.screen-picker__quality`
 * is a prefix of `__quality-row`, `__quality-label` and `__quality-select`, so a
 * substring match would happily read a sibling's body and report on the wrong rule.
 *
 * Asserted against the stylesheet rather than the DOM because Vitest runs with
 * the default `css: false` and jsdom performs no layout at all — a
 * `getComputedStyle` assertion here would pass whatever the rule said.
 */
function ruleBody(css: string, selector: string): string | null {
  const ruleRegex = /([^{}]+)\{([^{}]*)\}/g;
  const tokenRe = new RegExp(selector.replace('.', '\\.') + '(?![\\w-])');
  let body: string | null = null;
  let match: RegExpExecArray | null;
  while ((match = ruleRegex.exec(css)) !== null) {
    if (tokenRe.test(match[1])) body = match[2];
  }
  return body;
}

describe('screen-share picker quality row', () => {
  it('wraps the quality container, so Content cannot run off the modal edge', () => {
    // Audio, Resolution, Frame Rate and Content are four separate rows sharing
    // one flex line. Without wrapping, the last of them is clipped by the modal
    // at narrow widths — not scrolled out of reach, but unreachable.
    const body = ruleBody(pickerCss, '.screen-picker__quality');
    expect(body).not.toBeNull();
    expect(body as string).toMatch(/flex-wrap:\s*wrap/);
  });

  it('puts no min-width floor on a quality row, which would worsen narrow widths', () => {
    // A tripwire for the obvious wrong fix. Each row is a [label][select] pair,
    // so a floor applied at the ROW level forces a ~280px minimum per row —
    // wider than the space that was overflowing in the first place.
    const body = ruleBody(pickerCss, '.screen-picker__quality-row');
    expect(body).not.toBeNull();
    expect(body as string).not.toMatch(/min-width/);
  });
});
