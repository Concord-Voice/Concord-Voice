import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * Measurement for contrast-pairs.test.ts, extracted so the allowlist generator
 * and the test share ONE implementation. They diverged once before on the
 * sibling undefined-token allowlist — a generator regex counted a nested
 * reference the test correctly skipped, and the allowlist was off by one
 * against a test that was right. A shared module makes that impossible rather
 * than unlikely.
 */
const HERE = dirname(fileURLToPath(import.meta.url));
const DESKTOP = resolve(HERE, '../../..');
const RENDERER = join(DESKTOP, 'src/renderer');
const THEME_FILE = join(RENDERER, 'styles/index.css');
const ALLOWLIST = join(HERE, 'contrast-pair-allowlist.txt');

/** WCAG 2.1 AA for normal text. */
const FLOOR = 4.5;

/**
 * Pseudo-classes that make a rule a STATE of a base selector rather than its own
 * element. Longest-first: a plain alternation with `focus` before `focus-visible`
 * matches the prefix and leaves `-visible` glued to the selector.
 */
const STATE_PSEUDO = /^:(focus-visible|focus-within|hover|focus|active|disabled)\b/;

/**
 * Reduce a state rule to the selector whose foreground it inherits.
 *
 * Depth-aware on purpose. A regex that strips these anywhere reaches inside
 * `:not(...)` and turns `.btn:hover:not(:disabled)` into `.btn:not()`, so the
 * base lookup misses, no foreground is inherited, and the pair is dropped from
 * measurement without ever being compared. That is a false negative in a guard —
 * strictly worse than the defect it exists to find, because the green still
 * reads as coverage. Only top-level states are stripped; anything inside
 * parentheses is left exactly as written.
 */
export function baseSelectorOf(selector: string): string {
  let out = '';
  let depth = 0;
  for (let i = 0; i < selector.length; i++) {
    const ch = selector[i];
    if (ch === '(') depth++;
    else if (ch === ')') depth--;
    else if (ch === ':' && depth === 0) {
      const m = STATE_PSEUDO.exec(selector.slice(i));
      if (m) {
        i += m[0].length - 1;
        continue;
      }
    }
    out += ch;
  }
  return out.trim();
}

/**
 * Candidate base selectors, most specific first. `.btn:hover:not(:disabled)` may
 * be declared as `.btn:not(:disabled)` or plainly as `.btn`; both are ordinary,
 * so both are tried rather than guessing which convention a file follows.
 */
function baseCandidates(selector: string): string[] {
  const stripped = baseSelectorOf(selector);
  const withoutFunctional = stripped.replace(/:[a-z-]+\([^)]*\)/g, '').trim();
  return withoutFunctional && withoutFunctional !== stripped
    ? [stripped, withoutFunctional]
    : [stripped];
}

function stripComments(css: string): string {
  // Preserve line count so a reported line number still means something.
  return css.replace(/\/\*[\s\S]*?\*\//g, (m) => '\n'.repeat((m.match(/\n/g) ?? []).length));
}

function walk(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    if (entry.isSymbolicLink()) return [];
    const full = join(dir, entry.name);
    if (entry.isDirectory()) return walk(full);
    return full.endsWith('.css') ? [full] : [];
  });
}

interface ThemeBlock {
  selector: string;
  tokens: Record<string, string>;
}

/**
 * The theme blocks are the ones that declare `--on-accent`. That token is
 * present in every block and nowhere else, which makes it a reliable marker
 * without hardcoding a block count that drifts the moment a scheme is added.
 */
function parseThemeBlocks(): ThemeBlock[] {
  const lines = stripComments(readFileSync(THEME_FILE, 'utf8')).split('\n');
  const blocks: ThemeBlock[] = [];
  for (let i = 0; i < lines.length; i++) {
    if (!/^\s*--on-accent\s*:/.test(lines[i])) continue;
    let open = i;
    while (open > 0 && !/\{\s*$/.test(lines[open])) open--;
    let close = i;
    while (close < lines.length && !/^\s*\}/.test(lines[close])) close++;
    // Read the block as one text rather than line by line. Pride's
    // --gradient-brand spans eight lines, and a per-line match never saw it, so
    // that scheme silently measured Concord's gradient inherited from :root.
    const tokens: Record<string, string> = {};
    const body = lines.slice(open + 1, close).join('\n');
    for (const decl of body.matchAll(/(--[A-Za-z0-9_-]+)\s*:\s*([^;{}]+);/g)) {
      tokens[decl[1]] = decl[2].replace(/\s+/g, ' ').trim();
    }
    blocks.push({ selector: lines[open].replace(/\{\s*$/, '').trim(), tokens });
  }

  // Every block here matches the SAME element, so a token a scheme does not
  // redeclare still resolves — it comes from `:root` through the cascade. Reading
  // only a block's own map made any such token look undeclared, which returned
  // null and skipped the ENTIRE block: `.channel-unread-badge` is 2.46:1 in
  // Concord Light and only its passing 7.31:1 root-dark pairing was ever measured.
  //
  // `:root` is laid down as the base for every block. This is an approximation of
  // the real cascade, which would also layer `[data-theme='light']` beneath a
  // scheme's light block by specificity and source order; that refinement is not
  // modelled, so a token declared ONLY in the generic light block still resolves
  // per-block rather than per-cascade. It is strictly more faithful than reading
  // one map, and the direction of the remaining gap is toward measuring fewer
  // pairs, never toward passing one that should fail.
  const root = blocks.find((b) => b.selector === ':root');
  if (root) {
    for (const block of blocks) {
      if (block === root) continue;
      block.tokens = { ...root.tokens, ...block.tokens };
    }
  }
  return blocks;
}

/** Resolve a value through one theme block, following `var()` chains and fallbacks. */
function resolveIn(block: ThemeBlock, value: string | null, depth = 0): string | null {
  if (value === null || depth > 8) return value;
  const m = /^var\(\s*(--[A-Za-z0-9_-]+)\s*(?:,\s*([\s\S]+))?\)$/.exec(value.trim());
  if (!m) return value.trim();
  const declared = block.tokens[m[1]];
  if (declared !== undefined) return resolveIn(block, declared, depth + 1);
  return m[2] !== undefined ? resolveIn(block, m[2], depth + 1) : null;
}

/**
 * The opaque CSS colour keywords this tree actually uses as a foreground or a
 * background. Returning null for these classified the pair as UNRESOLVABLE and
 * skipped it — `CreateChannelModal`'s `.btn-primary` pairs `color: white` with
 * `--accent-color` and measures 1.07:1 in dark high contrast, invisible to the
 * guard until this existed.
 *
 * Deliberately not the full 148-name table. `transparent`, `none`, `inherit` and
 * `currentColor` are genuinely unresolvable here — each depends on what is painted
 * behind or above the element — so they stay in the unresolvable count, which is
 * honest rather than a gap. A keyword outside this table lands there too, counted
 * rather than silently passing.
 */
const NAMED_COLORS: Record<string, string> = {
  white: '#ffffff',
  black: '#000000',
  red: '#ff0000',
  green: '#008000',
  blue: '#0000ff',
  yellow: '#ffff00',
  orange: '#ffa500',
  gray: '#808080',
  grey: '#808080',
};

function toRgb(value: string | null): [number, number, number] | null {
  if (value === null) return null;
  const raw = value.trim().toLowerCase();
  const c = NAMED_COLORS[raw] ?? raw;
  if (/^#[0-9a-f]{6}$/.test(c)) {
    return [1, 3, 5].map((i) => parseInt(c.slice(i, i + 2), 16)) as [number, number, number];
  }
  if (/^#[0-9a-f]{3}$/.test(c)) {
    return [1, 2, 3].map((i) => parseInt(c[i] + c[i], 16)) as [number, number, number];
  }
  return null;
}

function luminance(rgb: [number, number, number]): number {
  const [r, g, b] = rgb.map((v) => {
    const s = v / 255;
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

export function contrastRatio(fg: [number, number, number], bg: [number, number, number]): number {
  const a = luminance(fg);
  const b = luminance(bg);
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
}

/** Split at commas that are not inside parentheses. */
function splitTopLevel(value: string): string[] {
  const parts: string[] = [];
  let depth = 0;
  let current = '';
  for (const ch of value) {
    if (ch === '(') depth++;
    else if (ch === ')') depth--;
    if (ch === ',' && depth === 0) {
      parts.push(current.trim());
      current = '';
      continue;
    }
    current += ch;
  }
  parts.push(current.trim());
  return parts;
}

/**
 * Expand every `var()` in a value through one theme block, fallbacks included.
 *
 * `resolveIn` only follows a value that IS a single `var()`. A fill that embeds
 * one — Pride's `--brand-fill` lays a scrim over `var(--gradient-brand)` — needs
 * each reference replaced where it stands. Returns null when a reference has no
 * declaration and no fallback, which keeps the pair unresolvable rather than
 * measured against a guess.
 */
export function expandVars(
  tokens: Record<string, string>,
  value: string,
  depth = 0
): string | null {
  if (depth > 8) return null;
  let out = '';
  let i = 0;
  while (i < value.length) {
    const start = value.indexOf('var(', i);
    if (start < 0) {
      out += value.slice(i);
      break;
    }
    out += value.slice(i, start);
    let level = 0;
    let end = start + 3;
    for (; end < value.length; end++) {
      if (value[end] === '(') level++;
      else if (value[end] === ')' && --level === 0) break;
    }
    if (level !== 0) return null;
    const [name, ...rest] = splitTopLevel(value.slice(start + 4, end));
    const replacement = tokens[name] ?? (rest.length > 0 ? rest.join(', ') : undefined);
    if (replacement === undefined) return null;
    const expanded = expandVars(tokens, replacement, depth + 1);
    if (expanded === null) return null;
    out += expanded;
    i = end + 1;
  }
  return out.trim();
}

type Rgba = [number, number, number, number];

/** A colour with its alpha: hex, the named colours above, `rgb()` or `rgba()`. */
function toRgba(value: string): Rgba | null {
  const opaque = toRgb(value);
  if (opaque) return [...opaque, 1];
  const m =
    /^rgba?\(\s*([\d.]+)[\s,]+([\d.]+)[\s,]+([\d.]+)\s*(?:[,/]\s*([\d.]+)(%?))?\s*\)$/i.exec(
      value.trim()
    );
  if (!m) return null;
  // CSS clamps out-of-range components: rgb(999 999 999) paints white.
  const clamp = (v: number, max: number) => Math.min(max, Math.max(0, v));
  const alpha = m[4] === undefined ? 1 : Number(m[4]) / (m[5] === '%' ? 100 : 1);
  return [
    clamp(Number(m[1]), 255),
    clamp(Number(m[2]), 255),
    clamp(Number(m[3]), 255),
    clamp(alpha, 1),
  ];
}

const GRADIENT_SAMPLES = 100;

/** Samples inside each transition, so one narrower than 1% is still measured. */
const SEGMENT_SAMPLES = 32;

type Stop = { color: Rgba; at: number };

/** The only first argument that is not a colour stop: a direction or an angle. */
const DIRECTION =
  /^(?:to\s+(?:left|right|top|bottom)(?:\s+(?:left|right|top|bottom))?|-?[\d.]+(?:deg|rad|grad|turn))$/i;

/**
 * The stops of one `linear-gradient` layer, placed where CSS places them.
 *
 * Interpolation is in sRGB, which is what Chromium does for a gradient with no
 * interpolation clause. The first argument is skipped only when it is a direction
 * or an angle; anything else there is read as a stop. An argument this parser
 * cannot read, an interpolation clause (`in oklab`) included, returns null, so an
 * unsupported gradient stays unresolvable instead of being measured without it.
 *
 * Positions follow CSS: a missing first or last position is 0% or 100%, a
 * position before an earlier one is raised to it, and a run of stops with no
 * position is spread evenly between the stops on either side of it.
 */
function gradientStops(layer: string): Stop[] | null {
  const m = /^linear-gradient\(([\s\S]*)\)$/.exec(layer.trim());
  if (!m) return null;
  const args = splitTopLevel(m[1]);
  if (args.length > 0 && DIRECTION.test(args[0])) args.shift();
  const parsed = args.map((arg) => {
    const pos = /\s+(-?[\d.]+)%$/.exec(arg);
    const colour = pos ? arg.slice(0, pos.index) : arg;
    return { color: toRgba(colour), at: pos ? Number(pos[1]) / 100 : null };
  });
  if (parsed.length < 2 || parsed.some((stop) => stop.color === null)) return null;
  const at = parsed.map((stop) => stop.at);
  at[0] ??= 0;
  at[at.length - 1] ??= 1;
  let highest = -Infinity;
  for (let i = 0; i < at.length; i++) {
    const position = at[i];
    if (position === null) continue;
    highest = Math.max(highest, position);
    at[i] = highest;
  }
  for (let i = 1; i < at.length; i++) {
    if (at[i] !== null) continue;
    let next = i;
    while (at[next] === null) next++;
    const from = at[i - 1] as number;
    const to = at[next] as number;
    for (let k = i; k < next; k++) at[k] = from + ((to - from) * (k - i + 1)) / (next - i + 1);
    i = next;
  }
  return parsed.map((stop, i) => ({ color: stop.color as Rgba, at: at[i] as number }));
}

/** The colour a layer paints at `t`, interpolated with premultiplied alpha as CSS does. */
function colorAt(stops: Stop[], t: number): Rgba {
  let k = 0;
  while (k < stops.length - 2 && t > stops[k + 1].at) k++;
  const from = stops[k];
  const to = stops[k + 1];
  const span = to.at - from.at;
  let u = 0;
  if (span > 0) u = Math.min(1, Math.max(0, (t - from.at) / span));
  else if (t >= to.at) u = 1;
  const alpha = from.color[3] + (to.color[3] - from.color[3]) * u;
  if (alpha === 0) return [0, 0, 0, 0];
  const premultiplied = (j: number) =>
    from.color[j] * from.color[3] + (to.color[j] * to.color[3] - from.color[j] * from.color[3]) * u;
  return [premultiplied(0) / alpha, premultiplied(1) / alpha, premultiplied(2) / alpha, alpha];
}

/**
 * The colours one varying layer paints between 0% and 100%.
 *
 * The first GRADIENT_SAMPLES + 1 entries are evenly spaced, one per 1%, so a
 * caller can index them by percentage. Then come SEGMENT_SAMPLES points inside
 * every transition between two stops, clipped to the element, because a
 * transition narrower than the spacing can dip between two samples. Last come
 * the stops' own colours, but only for stops the element paints: a stop past
 * either end still shapes the colours inside, which colorAt accounts for, and
 * is never painted itself.
 */
function paintedSamples(stops: Stop[]): Rgba[] {
  const out = Array.from({ length: GRADIENT_SAMPLES + 1 }, (_, i) =>
    colorAt(stops, i / GRADIENT_SAMPLES)
  );
  for (let k = 0; k + 1 < stops.length; k++) {
    const from = Math.max(0, stops[k].at);
    const to = Math.min(1, stops[k + 1].at);
    if (to <= from) continue;
    for (let j = 1; j < SEGMENT_SAMPLES; j++) {
      out.push(colorAt(stops, from + ((to - from) * j) / SEGMENT_SAMPLES));
    }
  }
  for (const stop of stops) if (stop.at >= 0 && stop.at <= 1) out.push(stop.color);
  return out;
}

function isUniform(stops: Stop[]): boolean {
  return stops.every((stop) => stop.color.every((v, j) => v === stops[0].color[j]));
}

/**
 * Every colour a background can paint behind its text, or null when that is not
 * knowable from the stylesheet.
 *
 * A solid colour is one sample. A `linear-gradient` is sampled along its length,
 * inside every transition and at each stop it paints (see paintedSamples), so a
 * narrow stripe or transition between two samples is still measured, and a
 * contrast check takes the worst sample: a label can sit over any part of the
 * fill. Layers above the last are composited over it, so a
 * translucent scrim over a gradient is measured as the colours it produces.
 *
 * At most one layer may vary. Each layer runs in its own direction, and matching
 * two varying layers point by point would pair colours that never meet on
 * screen, so two varying layers stay unresolvable. A layer that paints one colour
 * throughout, like Pride's scrim, composites the same way in any direction. The
 * bottom layer must be opaque; what shows through a translucent one depends on
 * the ancestor, so that stays unresolvable too.
 */
export function sampleFill(value: string | null): Array<[number, number, number]> | null {
  if (value === null) return null;
  const solid = toRgb(value);
  if (solid) return [solid];
  const layers = splitTopLevel(value).map(gradientStops);
  if (layers.some((layer) => layer === null)) return null;
  const stacked = layers as Stop[][];
  if (stacked[stacked.length - 1].some((stop) => stop.color[3] !== 1)) return null;
  const varying = stacked.filter((layer) => !isUniform(layer));
  if (varying.length > 1) return null;
  const moving = varying[0];
  const painted = moving ? paintedSamples(moving) : [stacked[0][0].color];
  return painted.map((colour) => {
    let out: [number, number, number] = [0, 0, 0];
    for (let k = stacked.length - 1; k >= 0; k--) {
      const over = stacked[k] === moving ? colour : stacked[k][0].color;
      const a = over[3];
      out = [0, 1, 2].map((j) => over[j] * a + out[j] * (1 - a)) as [number, number, number];
    }
    return out;
  });
}

interface Rule {
  file: string;
  selector: string;
  color: string | null;
  background: string | null;
  inheritedColor: boolean;
}

/**
 * Split a selector list into its individual selectors.
 *
 * Keeping only the last line — which this file did first — silently drops every
 * selector but one from a multi-line comma list, and a dropped selector is never
 * measured against anything. `.role-reorder-notice, .role-reorder-alert` took its
 * background from the shared rule and its foreground from a later rule of its own,
 * so discarding the first name hid a 2.00:1 pair from the ratchet entirely.
 *
 * Commas inside `:not(...)`/`:is(...)` are arguments, not separators, so the split
 * is depth-aware for the same reason `baseSelectorOf` is.
 */
export function splitSelectorList(selectorList: string): string[] {
  const out: string[] = [];
  let current = '';
  let depth = 0;
  for (const ch of selectorList) {
    if (ch === '(') depth++;
    else if (ch === ')') depth--;
    if (ch === ',' && depth === 0) {
      out.push(current.trim());
      current = '';
      continue;
    }
    current += ch;
  }
  out.push(current.trim());
  return out.map((s) => s.replace(/\s+/g, ' ').trim()).filter((s) => s.length > 0);
}

/**
 * The declaration the browser actually paints when a rule sets a property more
 * than once: `!important` beats normal, and among equals the LAST one wins.
 *
 * Taking the first match measured `color: #000; color: #fff` as black, so a rule
 * that renders white-on-white was scored as passing. The `!important` suffix is
 * stripped here too — left attached it reaches `toRgb`, fails to parse, and turns
 * a declared colour into an unresolvable one.
 */
export function winningDeclaration(body: string, pattern: RegExp): string | null {
  let normal: string | null = null;
  let important: string | null = null;
  for (const m of body.matchAll(pattern)) {
    const raw = m[1].trim();
    const bang = /!\s*important\s*$/i.test(raw);
    const value = raw.replace(/!\s*important\s*$/i, '').trim();
    if (bang) important = value;
    else normal = value;
  }
  return important ?? normal;
}

function collectRules(): Rule[] {
  // The theme file is scanned too. Excluding it wholesale also excluded its
  // ordinary painted rules — `body` sets both a background and a foreground — so
  // a low-contrast change there would never have been measured. Its token blocks
  // declare only custom properties, and `--accent-color:` does not match a
  // `color:` property pattern (the preceding character is `-`, not start-of-line,
  // `;` or space), so they contribute no rules and need no special-casing.
  const files = walk(RENDERER);
  const rules: Rule[] = [];
  for (const file of files) {
    const css = stripComments(readFileSync(file, 'utf8'));
    for (const m of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
      const selectorList = m[1].trim();
      // An at-rule prelude (`@media …`) is not a selector and declares nothing.
      if (selectorList.startsWith('@')) continue;
      const body = m[2];
      const color = winningDeclaration(body, /(?:^|[;\s])color\s*:\s*([^;]+);/g);
      const background = winningDeclaration(
        body,
        /(?:^|[;\s])background(?:-color)?\s*:\s*([^;]+);/g
      );
      if (!color && !background) continue;
      for (const selector of splitSelectorList(selectorList)) {
        rules.push({
          file: relative(DESKTOP, file),
          selector,
          color,
          background,
          inheritedColor: false,
        });
      }
    }
  }
  return rules;
}

/**
 * Collapse repeated declarations of the same selector, later winning — the
 * cascade's behaviour for equal specificity.
 *
 * Needed because a foreground and its background are routinely declared apart:
 * a shared comma rule sets the surface and a per-selector rule sets the text.
 * Without this, each half is a rule with one property and the pair is never
 * formed, which is the shape that hid `.role-reorder-notice`.
 *
 * The approximation is deliberate and worth naming: declarations inside a media
 * query are folded in with the rest, so a selector whose background is only set
 * under a breakpoint is treated as though it always carries it. That errs toward
 * measuring a pair that may not co-occur, which surfaces as a reviewable
 * allowlist line rather than as silence.
 */
export function mergeBySelector(rules: Rule[]): Rule[] {
  const merged = new Map<string, Rule>();
  for (const r of rules) {
    const key = `${r.file}|${r.selector}`;
    const existing = merged.get(key);
    if (!existing) {
      merged.set(key, { ...r });
      continue;
    }
    if (r.color) existing.color = r.color;
    if (r.background) existing.background = r.background;
  }
  return [...merged.values()];
}

/**
 * A `:hover` rule that changes only the background keeps the base rule's
 * foreground. Skipping that inheritance would miss an entire class — the two
 * hover findings on PR #3285 were exactly this shape, and the worst pair in the
 * whole tree (1.06:1) is a hover state.
 */
export function inheritStateColors(rules: Rule[]): Rule[] {
  const base = new Map<string, string>();
  for (const r of rules) {
    // A rule is a base rule when stripping top-level states changes nothing.
    if (r.color && baseSelectorOf(r.selector) === r.selector) {
      base.set(`${r.file}|${r.selector}`, r.color);
    }
  }
  for (const r of rules) {
    if (r.color || !r.background) continue;
    for (const candidate of baseCandidates(r.selector)) {
      const inherited = base.get(`${r.file}|${candidate}`);
      if (inherited) {
        r.color = inherited;
        r.inheritedColor = true;
        break;
      }
    }
  }
  return rules;
}

interface Failure {
  key: string;
  worst: number;
  worstBlock: string;
}

function measure(): { checked: number; unresolvable: number; failures: Map<string, Failure> } {
  const blocks = parseThemeBlocks();
  const pairs = inheritStateColors(mergeBySelector(collectRules())).filter(
    (r) => r.color && r.background
  );
  const failures = new Map<string, Failure>();
  let checked = 0;
  let unresolvable = 0;

  for (const pair of pairs) {
    let worst = Infinity;
    let worstBlock = '';
    let resolvedAnywhere = false;
    for (const block of blocks) {
      const fg = toRgb(resolveIn(block, pair.color));
      const bgs = sampleFill(
        pair.background === null ? null : expandVars(block.tokens, pair.background)
      );
      if (!fg || !bgs) continue;
      resolvedAnywhere = true;
      const ratio = Math.min(...bgs.map((bg) => contrastRatio(fg, bg)));
      if (ratio < worst) {
        worst = ratio;
        worstBlock = block.selector;
      }
    }
    if (!resolvedAnywhere) {
      unresolvable++;
      continue;
    }
    checked++;
    if (worst < FLOOR) {
      const key = `${pair.file}\t${pair.selector}`;
      const prior = failures.get(key);
      if (!prior || worst < prior.worst) failures.set(key, { key, worst, worstBlock });
    }
  }
  return { checked, unresolvable, failures };
}

export { FLOOR, ALLOWLIST, parseThemeBlocks, measure, toRgb, stripComments, walk };
export type { Failure, Rule, ThemeBlock };
