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
    const tokens: Record<string, string> = {};
    for (let t = open; t <= close; t++) {
      const decl = /^\s*(--[A-Za-z0-9_-]+)\s*:\s*([^;]+);/.exec(lines[t]);
      if (decl) tokens[decl[1]] = decl[2].trim();
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
      const bg = toRgb(resolveIn(block, pair.background));
      if (!fg || !bg) continue;
      resolvedAnywhere = true;
      const ratio = contrastRatio(fg, bg);
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

export { FLOOR, ALLOWLIST, parseThemeBlocks, measure, toRgb };
export type { Failure, Rule, ThemeBlock };
