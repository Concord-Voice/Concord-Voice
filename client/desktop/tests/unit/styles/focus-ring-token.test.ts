/**
 * Guard: every renderer focus indicator draws in `--state-focused`.
 *
 * #798 split `--state-selected` (the accent: active tabs, selected rows) from
 * `--state-focused` (keyboard focus) so a focused control never reads as part of an
 * adjacent selected highlight. link-contrast.test.ts holds the token to 3:1 against all
 * three surfaces and keeps it distinct from the accent; this file keeps indicators on it.
 *
 * A focus indicator is an `outline*`, `box-shadow` or `border*` declaration in a rule
 * whose selector mentions `:focus` (covering `:focus-visible`, `:focus-within` and
 * `:has(:focus-visible)`). It is an ALLOWLIST: an indicator must be a solid
 * `var(--state-focused)`, paint nothing, or match a named exception below. A denylist of
 * accent tokens let `var(--state-selected)` (the accent by definition), literal colours,
 * and a translucent tint of the right token all pass.
 *
 * Static regex literals only (no dynamic `RegExp`), matching the sibling style tests'
 * Semgrep CWE-1333 posture.
 */

import { readFileSync } from 'node:fs';
import { relative, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { stripComments, walk } from './contrastPairs';

const RENDERER = resolve(__dirname, '../../../src/renderer');

type Declaration = {
  file: string;
  line: number;
  selector: string;
  property: string;
  value: string;
};

/** Every declaration with its innermost selector. A final declaration needs no `;`. */
function parse(css: string, file: string): Declaration[] {
  const out: Declaration[] = [];
  const selectors: string[] = [];
  let buf = '';
  let line = 1;
  const flush = () => {
    const colon = buf.indexOf(':');
    if (colon > 0 && selectors.length > 0) {
      out.push({
        file,
        line,
        selector: selectors[selectors.length - 1],
        property: buf.slice(0, colon).trim(),
        value: buf
          .slice(colon + 1)
          .trim()
          .replace(/\s+/g, ' '),
      });
    }
    buf = '';
  };
  for (const ch of stripComments(css)) {
    if (ch === '{') {
      selectors.push(buf.trim().replace(/\s+/g, ' '));
      buf = '';
    } else if (ch === '}') {
      flush();
      selectors.pop();
    } else if (ch === ';') {
      flush();
    } else {
      buf += ch;
    }
    if (ch === '\n') line += 1;
  }
  return out;
}

const INDICATOR =
  /^(outline(-color)?|box-shadow|border(-(top|right|bottom|left|block|inline)(-(start|end))?)?(-color)?)$/;
const PAINTS_NOTHING = /^(none|0|transparent|inherit|initial|unset)$/;
// A --bg-* band inside an inset ring, so the ring's inner edge sits on a measured surface.
const SURFACE_BAND = /^inset 0 0 0 (\d+)px var\(--bg-(primary|secondary|tertiary)\)$/;

/** Indicators in a deliberate colour. `file: null` means anywhere. */
const EXCEPTIONS: { file: string | null; value: RegExp; reason: string }[] = [
  { file: null, value: /var\(--danger\)/, reason: 'invalid field: the error colour, not focus' },
  { file: null, value: /currentColor/, reason: 'inherits the control text colour' },
  { file: 'components/Auth/SSOButton.css', value: /#4285f4/i, reason: "Google's brand blue" },
  {
    file: 'components/Chat/ImageLightbox.css',
    value: /#ffffff/i,
    reason: 'white over the dark lightbox scrim',
  },
];

/** Focus rules that remove the outline by design and draw nothing else. */
const NO_INDICATOR_BY_DESIGN = [
  {
    file: 'components/Voice/VoiceView.css',
    selector: '.voice-view__voice-area:focus',
    reason: 'programmatic tabIndex=-1 focus target after a layout swap, not a control',
  },
];

type Verdict = 'paints-nothing' | 'solid' | 'tint' | 'band' | 'exception' | 'offender';

function verdict(d: Declaration): Verdict {
  if (PAINTS_NOTHING.test(d.value)) return 'paints-nothing';
  if (SURFACE_BAND.test(d.value)) return 'band';
  if (/var\(\s*--state-focused\s*,/.test(d.value)) return 'offender';
  if (d.value.includes('var(--state-focused)')) {
    return d.value.includes('color-mix(') ? 'tint' : 'solid';
  }
  const excepted = EXCEPTIONS.some(
    (e) => (e.file === null || e.file === d.file) && e.value.test(d.value)
  );
  return excepted ? 'exception' : 'offender';
}

const isFocusIndicator = (d: Declaration) =>
  d.selector.includes(':focus') && INDICATOR.test(d.property);

const focusDecls = walk(RENDERER)
  .flatMap((path) => parse(readFileSync(path, 'utf-8'), relative(RENDERER, path)))
  .filter(isFocusIndicator);

const where = (d: Declaration) =>
  `${d.file}:${d.line}  ${d.selector} { ${d.property}: ${d.value} }`;
const ruleKey = (d: Declaration) => `${d.file}\u0000${d.selector}`;
const visible = (v: Verdict) => v === 'solid' || v === 'exception';

describe('focus indicators use --state-focused (#798)', () => {
  it('every focus indicator is a solid --state-focused, paints nothing, or is a named exception', () => {
    const offenders = focusDecls.filter((d) => verdict(d) === 'offender').map(where);
    expect(
      offenders,
      `Draw focus with var(--state-focused); the accent is the SELECTED colour:\n${offenders.join('\n')}\n`
    ).toEqual([]);
  });

  it('a translucent --state-focused tint only accompanies a solid indicator in the same rule', () => {
    const solidRules = new Set(focusDecls.filter((d) => visible(verdict(d))).map(ruleKey));
    const orphans = focusDecls
      .filter((d) => verdict(d) === 'tint' && !solidRules.has(ruleKey(d)))
      .map(where);
    expect(orphans, `A tint cannot clear 3:1 on its own:\n${orphans.join('\n')}\n`).toEqual([]);
  });

  it('a control that removes its focus outline still shows a solid indicator', () => {
    // Match by base selector (focus pseudo-classes and pseudo-elements stripped), so a
    // `:focus { outline: none }` is satisfied by a sibling `:focus-visible` ring or a
    // `:focus-visible::-webkit-slider-thumb` shadow on the same control.
    const base = (sel: string) =>
      sel
        .replace(/:focus(-visible|-within)?/g, '')
        .replace(/::[\w-]+$/, '')
        .trim();
    const shown = new Set<string>();
    for (const d of focusDecls) {
      if (!visible(verdict(d))) continue;
      for (const part of d.selector.split(',')) shown.add(`${d.file}\u0000${base(part)}`);
    }
    const bare = focusDecls
      .filter((d) => d.property.startsWith('outline') && /^(none|0)$/.test(d.value))
      .filter(
        (d) => !NO_INDICATOR_BY_DESIGN.some((x) => x.file === d.file && x.selector === d.selector)
      )
      .filter((d) => !d.selector.split(',').every((p) => shown.has(`${d.file}\u0000${base(p)}`)))
      .map(where);
    expect(bare, `Focus removed with nothing drawn in its place:\n${bare.join('\n')}\n`).toEqual(
      []
    );
  });

  it("the presence tier's surface band reaches past the inset ring's inner edge", () => {
    // outline-offset -N with width W paints the OUTER W px of the box, so the ring's
    // inner edge sits N px in. A band no deeper than N hides under the ring and leaves
    // that edge on the accent fill (1.91:1 in default light).
    const file = 'components/Settings/PresenceSettingsSection.css';
    const decls = parse(readFileSync(resolve(RENDERER, file), 'utf-8'), file);
    const find = (selector: string, property: string) =>
      decls.find((d) => d.selector === selector && d.property === property)?.value ?? '';
    const offsetPx = Number(
      /^(-?\d+)px$/.exec(find('.presence-tier-option:focus-visible', 'outline-offset'))?.[1]
    );
    const bandPx = Number(
      SURFACE_BAND.exec(find('.presence-tier-option.active:focus-visible', 'box-shadow'))?.[1]
    );
    expect(offsetPx, 'presence ring is no longer inset').toBeLessThan(0);
    expect(bandPx, 'presence band missing or not a --bg-* surface').toBeGreaterThan(
      Math.abs(offsetPx)
    );
  });

  it('the scan actually sees the indicators (vacuity floor)', () => {
    const solid = focusDecls.filter((d) => verdict(d) === 'solid');
    expect(solid.length).toBeGreaterThan(120);
  });
});

describe('parser', () => {
  it('handles nesting, :has(), selector lists, braces in comments and a missing final ;', () => {
    const css = `
      /* a comment with { braces } and ; semicolons */
      @media (max-width: 600px) {
        .a:focus-visible, .b:focus-visible { outline: 2px solid var(--state-focused) }
      }
      .c:has(:focus-visible) { border-color: var(--accent-primary); outline: none }
    `;
    const found = parse(css, 'fixture.css').map((d) => `${d.selector} | ${d.property}: ${d.value}`);
    expect(found).toEqual([
      '.a:focus-visible, .b:focus-visible | outline: 2px solid var(--state-focused)',
      '.c:has(:focus-visible) | border-color: var(--accent-primary)',
      '.c:has(:focus-visible) | outline: none',
    ]);
  });
});
