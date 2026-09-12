import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// Asserted against the stylesheet/source SOURCE, not the rendered DOM.
// Vitest runs with `css: false` (no `css:` key in vite.config.ts), so
// `import './EmojiPicker.css'` is stubbed and getComputedStyle cannot
// resolve these rules -- a toBeVisible()/computed-style assertion here would
// be vacuous. Same approach as GifPicker.test.tsx's stylesheet suite.
const css = readFileSync(
  resolve(__dirname, '../../../../src/renderer/components/EmojiPicker/EmojiPicker.css'),
  'utf-8'
).replace(/\/\*[\s\S]*?\*\//g, '');

const gridSrc = readFileSync(
  resolve(__dirname, '../../../../src/renderer/components/EmojiPicker/EmojiGrid.tsx'),
  'utf-8'
);

/** Extract one rule block by exact selector (mirrors GifPicker.test.tsx's helper). */
function ruleBody(selector: string): string {
  const m = new RegExp(`(^|\\n)${selector.replace('.', '\\.')}\\s*\\{([^}]*)\\}`).exec(css);
  if (!m) throw new Error(`selector ${selector} not found in EmojiPicker.css`);
  return m[2];
}

// ── A6 -- EmojiGrid.VIEWPORT_HEIGHT / --emoji-grid-height parity ──────────
//
// EmojiGrid.tsx's virtualization window (VIEWPORT_HEIGHT) and
// EmojiPicker.css's `--emoji-grid-height` custom property are two
// independent sources for what should be one number. `.emoji-picker-loading`
// is not a cosmetic duplicate of `.emoji-picker-grid-viewport`'s height: it
// is what EmojiGrid renders WHILE LOADING, and the picker is measured at
// mount while loading -- so if the two heights diverge, the measured height
// is not the final height and the picker is placed for a box that no longer
// exists (spec §2.3). This is the emoji-side equivalent of what #2976 pinned
// for the GIF picker's body height.
describe('EmojiGrid.VIEWPORT_HEIGHT / --emoji-grid-height parity (A6)', () => {
  it('EmojiGrid.VIEWPORT_HEIGHT is 322', () => {
    const m = /const VIEWPORT_HEIGHT\s*=\s*(\d+)\s*;/.exec(gridSrc);
    expect(m, 'EmojiGrid.tsx must declare `const VIEWPORT_HEIGHT = <n>;`').not.toBeNull();
    expect(Number(m![1])).toBe(322);
  });

  it("--emoji-grid-height's min() upper arm is also 322px", () => {
    const m = /--emoji-grid-height:\s*min\(\s*(\d+)px\s*,/.exec(css);
    expect(
      m,
      '.emoji-picker must declare --emoji-grid-height: min(<n>px, calc(100vh - <m>px))'
    ).not.toBeNull();
    expect(Number(m![1])).toBe(322);
  });

  it('.emoji-picker-grid-viewport consumes --emoji-grid-height rather than a literal', () => {
    const rule = ruleBody('.emoji-picker-grid-viewport');
    expect(
      rule,
      '.emoji-picker-grid-viewport must read height from var(--emoji-grid-height) -- a ' +
        "literal height here can silently drift from EmojiGrid's VIEWPORT_HEIGHT with " +
        'nothing to catch it'
    ).toMatch(/height:\s*var\(--emoji-grid-height\)/);
  });

  it('.emoji-picker-loading consumes --emoji-grid-height rather than a literal (placement invariant, not cosmetic)', () => {
    const rule = ruleBody('.emoji-picker-loading');
    expect(
      rule,
      '.emoji-picker-loading must read height from var(--emoji-grid-height), the SAME ' +
        'property .emoji-picker-grid-viewport reads. EmojiGrid renders this element WHILE ' +
        'LOADING, and the picker measures itself at mount while loading -- a divergent ' +
        'height here means the measured height is not the final one.'
    ).toMatch(/height:\s*var\(--emoji-grid-height\)/);
  });
});

// ── A7 -- overflow: clip regression guard (emoji picker half) ─────────────
//
// Same containing-block hazard as .gif-picker (spec §0): `.emoji-picker`'s
// arrow `::after` is `position: absolute` inside this box, which any popover
// instance renders at `position: fixed` -- its own containing block -- so
// the arrow is clipped by whatever `.emoji-picker` declares for `overflow`.
// jsdom computes no clipping and no paint, so this stylesheet assertion is
// the only automatable guard against a regression to `overflow: hidden`.
describe('EmojiPicker overflow-clip regression guard (A7)', () => {
  it('declares overflow: clip with overflow-clip-margin >= 9px -- never overflow: hidden', () => {
    const picker = ruleBody('.emoji-picker');
    expect(
      picker,
      'REGRESSION: .emoji-picker reverted to overflow: hidden. On the GIF picker this ' +
        "clipped the arrow caret away in full for the feature's entire lifetime (spec " +
        '§0), and jsdom cannot detect it (no clipping, no paint) -- this stylesheet ' +
        'assertion is the only thing that can catch the same regression here.'
    ).toMatch(/overflow:\s*clip\b/);
    expect(picker).not.toMatch(/overflow:\s*hidden\b/);

    const marginMatch = /overflow-clip-margin:\s*(\d+(?:\.\d+)?)px/.exec(picker);
    expect(
      marginMatch,
      '.emoji-picker must declare overflow-clip-margin alongside overflow: clip'
    ).not.toBeNull();
    // ARROW_HALF_CHORD (~8.4853px, utils/ui/pickerAnchor.ts) is how far the
    // shared 12x12 rotated caret protrudes past the border-box edge.
    expect(Number(marginMatch![1])).toBeGreaterThanOrEqual(9);
  });
});
