import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import {
  contrastRatio,
  expandVars,
  parseThemeBlocks,
  splitSelectorList,
  stripComments,
  toRgb,
  type ThemeBlock,
} from './contrastPairs';

/**
 * Contrast of the step-up credentials stage (`MFAFactorPicker.css`, every
 * `.step-up__*` rule) across every theme block, measured against the surface
 * each rule actually sits on.
 *
 * `contrast-pairs.test.ts` cannot see these rules: it skips a `color-mix()`
 * background, and a label, helper or status line sets only a foreground and
 * takes its surface from the modal. This file pairs them by name instead, so
 * each rule is stated with the surface it is read against:
 *
 *   modal   `.modal-container` in ui/Modal.css (read below, so a change fails
 *           here): `--bg-secondary`. Labels, helpers, the status line, links,
 *           and everything outside the field.
 *   field   `.step-up__input`: `--bg-tertiary`.
 *   refused `color-mix(in srgb, --danger 8%, --bg-tertiary)`: the fill of the
 *           error box and of a refused input. Evaluated from the CSS text.
 *
 * Checks:
 *   1. Every text colour the rules use is >= 4.5:1 on the surface it sits on.
 *   2. `--danger` as a border, against the modal surface outside the field
 *      (WCAG 1.4.11, 3:1).
 *   3. The error glyph's stroke, against the box fill it is drawn on (3:1),
 *      since the glyph has no outside of its own. It strokes `--text-primary`,
 *      not `--danger`: `--danger` on the tinted fill measures 2.63:1 in Cotton
 *      Candy light and 2.91:1 in Foxden, so the box's `--danger` border carries
 *      the error cue and the glyph takes the text colour.
 *   4. `--danger` is never a text colour, and is used only for borders and
 *      tints.
 *
 * `--border-color` is deliberately NOT asserted at 3:1. Every shipped field uses
 * it below that, and this stage matches the convention; the refused state is
 * the one that does not rely on it (border, tint, glyph and text together).
 *
 * Set STEP_UP_CONTRAST_TABLE=1 to print the measured table.
 *
 * Like the sibling guards this reads the stylesheet as text and uses no
 * dynamic RegExp.
 */

const HERE = dirname(fileURLToPath(import.meta.url));
const RENDERER = resolve(HERE, '../../../src/renderer');
const PICKER_CSS = resolve(RENDERER, 'components/Auth/MFAFactorPicker.css');
const MODAL_CSS = resolve(RENDERER, 'components/ui/Modal.css');

const TEXT_FLOOR = 4.5;
const NON_TEXT_FLOOR = 3;

type Rgb = [number, number, number];

// ── Reading the stylesheet ───────────────────────────────────────────────

interface CssRule {
  selector: string;
  decls: Array<{ prop: string; value: string }>;
}

function parseRules(path: string): CssRule[] {
  const css = stripComments(readFileSync(path, 'utf8'));
  const rules: CssRule[] = [];
  for (const m of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const prelude = m[1].trim();
    if (prelude.startsWith('@')) continue;
    const decls = m[2]
      .split(';')
      .map((raw) => raw.trim())
      .filter((raw) => raw.length > 0)
      .map((raw) => {
        const colon = raw.indexOf(':');
        return { prop: raw.slice(0, colon).trim(), value: raw.slice(colon + 1).trim() };
      });
    for (const selector of splitSelectorList(prelude)) rules.push({ selector, decls });
  }
  return rules;
}

const RULES = parseRules(PICKER_CSS);

function declarations(selector: string, prop: string): string[] {
  return RULES.filter((r) => r.selector === selector).flatMap((r) =>
    r.decls.filter((d) => d.prop === prop).map((d) => d.value)
  );
}

/** The last value declared, as the cascade resolves equal specificity. */
function declared(selector: string, prop: string): string {
  const values = declarations(selector, prop);
  if (values.length === 0) throw new Error(`${selector} declares no ${prop}`);
  return values[values.length - 1];
}

// ── Painting a value in one theme block ──────────────────────────────────

/**
 * A fully opaque colour: a hex value, or `color-mix(in srgb, A p%, B)` of two.
 * Throws on anything else rather than skipping, so an unreadable token fails
 * the suite instead of quietly measuring nothing.
 */
function paint(block: ThemeBlock, value: string): Rgb {
  const expanded = expandVars(block.tokens, value);
  if (expanded === null) throw new Error(`${block.selector}: cannot resolve ${value}`);
  const mix =
    /^color-mix\(\s*in srgb\s*,\s*(#[0-9a-fA-F]{3,6})\s+([\d.]+)%\s*,\s*(#[0-9a-fA-F]{3,6})\s*\)$/.exec(
      expanded
    );
  if (mix) {
    const [a, b] = [toRgb(mix[1]), toRgb(mix[3])];
    if (!a || !b) throw new Error(`${block.selector}: cannot read ${expanded}`);
    const weight = Number(mix[2]) / 100;
    return a.map((v, i) => v * weight + b[i] * (1 - weight)) as Rgb;
  }
  const rgb = toRgb(expanded);
  if (!rgb) throw new Error(`${block.selector}: ${value} resolved to ${expanded}, not a colour`);
  return rgb;
}

// ── Surfaces ─────────────────────────────────────────────────────────────

/** The host modal's surface, read from Modal.css so a change there changes what is measured. */
function modalSurface(): string {
  const rules = parseRules(MODAL_CSS).filter((r) => r.selector === '.modal-container');
  const values = rules.flatMap((r) => r.decls.filter((d) => d.prop === 'background'));
  if (values.length === 0) throw new Error('.modal-container declares no background');
  return values[values.length - 1].value;
}

const MODAL_BG = modalSurface();

const REFUSED_FILL = declared(".step-up__input[aria-invalid='true']", 'background');
const FIELD_BG = declared('.step-up__input', 'background');
const ERROR_FILL = declared('.step-up__error', 'background');

/**
 * Each rule that sets a text colour, with the CSS values of the surfaces it is
 * read on. A rule that sets a colour and is missing here fails the
 * classification test below: a new rule must say where it sits.
 */
const TEXT_ON: Record<string, Array<{ surface: string; bg: string }>> = {
  '.step-up__label': [{ surface: 'modal', bg: MODAL_BG }],
  '.step-up__helper': [{ surface: 'modal', bg: MODAL_BG }],
  '.step-up__status': [{ surface: 'modal', bg: MODAL_BG }],
  '.step-up__link': [{ surface: 'modal', bg: MODAL_BG }],
  // The same input rule is read on both its fills: idle, and refused (the tint).
  '.step-up__input': [
    { surface: 'field', bg: FIELD_BG },
    { surface: 'refused fill', bg: REFUSED_FILL },
  ],
  '.step-up__error': [{ surface: 'error box', bg: ERROR_FILL }],
};

/** Each use of --danger that is a border, and what is adjacent to it. */
const DANGER_EDGES = [
  {
    use: 'refused input border',
    selector: ".step-up__input[aria-invalid='true']",
    prop: 'border-color',
    // Outside the field: the modal the input sits on.
    adjacent: 'modal surface',
    adjacentBg: MODAL_BG,
  },
  {
    use: 'error box border',
    selector: '.step-up__error',
    prop: 'border',
    adjacent: 'modal surface',
    adjacentBg: MODAL_BG,
  },
] as const;

/**
 * The error glyph: a graphic with no outside of its own, drawn on the error
 * box's fill. Its stroke is a text-colour token, never `--danger`.
 */
const GLYPH = {
  selector: '.step-up__error-glyph',
  prop: 'stroke',
  stroke: declared('.step-up__error-glyph', 'stroke'),
  adjacent: 'error box fill',
  adjacentBg: ERROR_FILL,
} as const;

/** Every `--danger` use the stylesheet may carry: the two borders and the two tints. */
const DANGER_ALLOWED = new Set([
  ...DANGER_EDGES.map((e) => `${e.selector}|${e.prop}`),
  ".step-up__input[aria-invalid='true']|background",
  '.step-up__error|background',
]);

const TEXT_PROPS = new Set([
  'color',
  '-webkit-text-fill-color',
  'text-decoration-color',
  'caret-color',
  'text-shadow',
]);

// ── Measurement ──────────────────────────────────────────────────────────

const BLOCKS = parseThemeBlocks();

interface Measurement {
  check: string;
  block: string;
  pair: string;
  ratio: number;
  floor: number;
}

function measureText(block: ThemeBlock): Measurement[] {
  return Object.entries(TEXT_ON).flatMap(([selector, surfaces]) => {
    const fg = paint(block, declared(selector, 'color'));
    return surfaces.map(({ surface, bg }) => ({
      check: 'text',
      block: block.selector,
      pair: `${selector} ${declared(selector, 'color')} on ${surface}`,
      ratio: contrastRatio(fg, paint(block, bg)),
      floor: TEXT_FLOOR,
    }));
  });
}

function measureDanger(block: ThemeBlock): Measurement[] {
  const danger = paint(block, 'var(--danger)');
  return DANGER_EDGES.map((edge) => ({
    check: 'danger',
    block: block.selector,
    pair: `--danger ${edge.use} on ${edge.adjacent}`,
    ratio: contrastRatio(danger, paint(block, edge.adjacentBg)),
    floor: NON_TEXT_FLOOR,
  }));
}

function measureGlyph(block: ThemeBlock): Measurement[] {
  return [
    {
      check: 'glyph',
      block: block.selector,
      pair: `${GLYPH.selector} stroke ${GLYPH.stroke} on ${GLYPH.adjacent}`,
      ratio: contrastRatio(paint(block, GLYPH.stroke), paint(block, GLYPH.adjacentBg)),
      floor: NON_TEXT_FLOOR,
    },
  ];
}

function failures(measured: Measurement[]): string[] {
  return measured
    .filter((m) => m.ratio < m.floor)
    .map((m) => `${m.block}: ${m.pair} = ${m.ratio.toFixed(2)}:1 (needs ${m.floor}:1)`);
}

describe('step-up picker contrast', () => {
  it('reads every scheme x theme combination', () => {
    // 34 blocks: :root and the generic light block (2), 15 schemes x dark/light
    // (30), and the high-contrast pair (2). Pinned as a set, so a scheme added
    // or dropped fails here rather than silently changing what is measured.
    const schemes = [
      'concord',
      'morky',
      'bardic',
      'foxden',
      'hacker',
      'spooky',
      'leviathan',
      'grassynill',
      'cottoncandy',
      'driftwood',
      'eclipse',
      'midnightsky',
      'agency',
      'defacto',
      'pride',
    ];
    const expected = [
      ':root',
      "[data-theme='light']",
      ...schemes.flatMap((scheme) => [
        `[data-scheme='${scheme}']`,
        `[data-scheme='${scheme}'][data-theme='light']`,
      ]),
      "[data-high-contrast='true']",
      "[data-high-contrast='true'][data-theme='light']",
    ];
    expect(BLOCKS.map((b) => b.selector)).toEqual(expected);
    expect(BLOCKS).toHaveLength(34);
  });

  it('reads the host modal surface from Modal.css', () => {
    expect(MODAL_BG).toBe('var(--bg-secondary)');
  });

  describe('text is at least 4.5:1 on the surface it sits on', () => {
    it.each(BLOCKS.map((b) => [b.selector, b] as const))('%s', (_name, block) => {
      expect(failures(measureText(block))).toEqual([]);
    });
  });

  describe('--danger borders are at least 3:1 against what is adjacent', () => {
    it.each(BLOCKS.map((b) => [b.selector, b] as const))('%s', (_name, block) => {
      expect(failures(measureDanger(block))).toEqual([]);
    });
  });

  // The glyph left --danger because --danger on the tinted fill fell below 3:1
  // in two blocks. Mutant: stroking it --danger again turns this red in those.
  describe('the error glyph stroke is at least 3:1 against the error box fill', () => {
    it.each(BLOCKS.map((b) => [b.selector, b] as const))('%s', (_name, block) => {
      expect(failures(measureGlyph(block))).toEqual([]);
    });

    it('strokes a text-colour token, not --danger', () => {
      expect(GLYPH.stroke).toBe('var(--text-primary)');
    });

    // A control proving the measurement can fail: the colour it replaced does.
    it('would fail with --danger in at least one block, so the check can see the defect', () => {
      const danger = BLOCKS.flatMap((block) =>
        measureGlyph(block).map((m) => ({
          ...m,
          ratio: contrastRatio(paint(block, 'var(--danger)'), paint(block, GLYPH.adjacentBg)),
        }))
      );
      expect(failures(danger).length).toBeGreaterThan(0);
    });
  });

  describe('--danger is never a text colour', () => {
    it('no text-colour property of any rule reads --danger', () => {
      const offenders = RULES.flatMap((r) =>
        r.decls
          .filter((d) => TEXT_PROPS.has(d.prop) && d.value.includes('--danger'))
          .map((d) => `${r.selector} { ${d.prop}: ${d.value} }`)
      );
      expect(offenders).toEqual([]);
    });

    it('every --danger use is a classified border or tint', () => {
      const uses = RULES.flatMap((r) =>
        r.decls.filter((d) => d.value.includes('--danger')).map((d) => `${r.selector}|${d.prop}`)
      );
      expect(uses.length).toBeGreaterThan(0);
      expect(uses.filter((use) => !DANGER_ALLOWED.has(use))).toEqual([]);
      // And the allowed list names no use the stylesheet has dropped.
      expect([...DANGER_ALLOWED].filter((use) => !uses.includes(use))).toEqual([]);
    });

    it('the tint is a background mix, never the text', () => {
      for (const fill of [REFUSED_FILL, ERROR_FILL]) {
        expect(fill).toMatch(/^color-mix\(in srgb, var\(--danger\) \d+%, var\(--bg-tertiary\)\)$/);
      }
    });
  });

  describe('classification', () => {
    it('every rule that sets a text colour says what it sits on', () => {
      const coloured = [
        ...new Set(
          RULES.filter((r) => r.decls.some((d) => d.prop === 'color')).map((r) => r.selector)
        ),
      ];
      expect(coloured.filter((selector) => !(selector in TEXT_ON))).toEqual([]);
      expect(Object.keys(TEXT_ON).filter((selector) => !coloured.includes(selector))).toEqual([]);
    });

    it('every text colour is a theme token, so it tracks all theme blocks', () => {
      for (const selector of Object.keys(TEXT_ON)) {
        expect(declared(selector, 'color')).toMatch(/^var\(--[a-z-]+\)$/);
      }
    });
  });

  it.runIf(process.env.STEP_UP_CONTRAST_TABLE === '1')('prints the measured table', () => {
    const rows = BLOCKS.flatMap((b) => [
      ...measureText(b),
      ...measureDanger(b),
      ...measureGlyph(b),
    ]);
    const worst = new Map<string, Measurement>();
    for (const m of rows) {
      const key = `${m.check}|${m.pair}`;
      const prior = worst.get(key);
      if (!prior || m.ratio < prior.ratio) worst.set(key, m);
    }
    const lines = [...worst.values()].map(
      (m) => `${m.ratio.toFixed(2).padStart(6)}:1  ${m.pair}  [${m.block}]`
    );
    process.stdout.write(
      `\nworst ratio per pair across ${BLOCKS.length} blocks\n${lines.join('\n')}\n`
    );
    expect(lines.length).toBeGreaterThan(0);
  });
});
