/**
 * Guard: every load-bearing `var(--token)` in renderer CSS must resolve.
 *
 * Companion to design-tokens.test.ts, which checks that the 23 DECLARED tokens exist
 * in all 32 theme blocks. This checks the other direction — that every token a
 * stylesheet REFERENCES is declared somewhere. `--border-primary` and
 * `--border-secondary` fell through exactly that gap: referenced in 13 places between
 * them, declared nowhere.
 *
 * Why only references with no usable fallback fail this test
 * ---------------------------------------------------------
 * `var(--undefined)` with no fallback is invalid at computed-value time: the whole
 * declaration becomes `unset`, so `border-top: 1px solid var(--nope)` renders NO
 * BORDER rather than a default-coloured one. The element silently loses the property.
 * That is a functional defect and is gated here.
 *
 * `var(--undefined, #444)` still paints — just always the literal, ignoring the theme.
 * A real bug too (a dark grey on 15 light schemes), but cosmetic rather than
 * structural, and there is a standing backlog of them, so gating it here would fail on
 * files this test's author must not touch. Left ungated deliberately; see PR #3270.
 *
 * What this proves, and what it does not
 * --------------------------------------
 * It proves a referenced token is DECLARED SOMEWHERE IN THE TREE — lexical presence,
 * not resolvability in the DOM state where the reference is actually used. A token
 * declared in one file under `.some-widget` and referenced from another under a
 * different selector passes. Modelling that properly needs real cascade resolution,
 * which jsdom cannot do (see high-contrast-cascade.test.ts). It is not live today —
 * every token in index.css is declared on a root-level selector — but do not read a
 * green run as "every reference resolves in every DOM state".
 *
 * Known limits, all deliberate and all in the false-NEGATIVE direction (this guard
 * under-reports rather than blocking a correct build):
 *   - Scope is `client/desktop/src` only. `client/admin/**` ships its own CSS with the
 *     same defect class and is NOT covered.
 *   - `stripComments` is not string-aware, so a `/*` inside a CSS string or `url()`
 *     would open a comment and hide the `var()`s after it. No instance exists today.
 *   - Runtime tokens are collected from TS/TSX source without stripping comments, so a
 *     commented-out declaration still registers the token as known.
 */

import { readFileSync, readdirSync } from 'node:fs';
import { resolve, join, relative } from 'node:path';
import { describe, expect, it } from 'vitest';

const SRC = resolve(__dirname, '../../../src');
const SRC_PARENT = resolve(SRC, '..');

/** Collect files by extension. Symlinks are skipped — they can escape the tree or cycle. */
function walk(dir: string, ext: string[]): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    if (entry.isSymbolicLink()) return [];
    const full = join(dir, entry.name);
    if (entry.isDirectory()) return walk(full, ext);
    return ext.some((e) => full.endsWith(e)) ? [full] : [];
  });
}

/** Remove comments, preserving newlines so reported line numbers stay true. */
function stripComments(css: string): string {
  return css.replace(/\/\*[\s\S]*?\*\//g, (m) => '\n'.repeat((m.match(/\n/g) ?? []).length));
}

interface ParsedVar {
  token: string;
  fallback: string | null;
  end: number;
  malformed: boolean;
}

/**
 * Read the argument list of the `var(` whose `(` sits at `open`. Returns the token
 * name, the raw fallback text (null when absent), the index just past the closing
 * paren, and whether the parens ever balanced.
 */
function parseVar(src: string, open: number): ParsedVar {
  let depth = 0;
  let comma = -1;
  let i = open;
  let closed = false;
  for (; i < src.length; i++) {
    const c = src[i];
    if (c === '(') depth++;
    else if (c === ')') {
      depth--;
      if (depth === 0) {
        closed = true;
        break;
      }
    } else if (c === ',' && depth === 1 && comma === -1) comma = i;
  }
  const inner = src.slice(open + 1, i);
  const token = (comma === -1 ? inner : src.slice(open + 1, comma)).trim();
  const fallback = comma === -1 ? null : src.slice(comma + 1, i).trim();
  return { token, fallback, end: i + 1, malformed: !closed };
}

/** Indices of the `(` of each `var(` at paren-depth 0 within `value`. */
function topLevelVarOpens(value: string): number[] {
  const out: number[] = [];
  let depth = 0;
  for (let i = 0; i < value.length; i++) {
    const c = value[i];
    if (c === '(') {
      if (depth === 0 && /var$/i.test(value.slice(Math.max(0, i - 3), i))) out.push(i);
      depth++;
    } else if (c === ')') {
      depth--;
    }
  }
  return out;
}

/**
 * Mirrors the CSS substitution rule: a value resolves when every `var()` it contains
 * at the top level resolves. A fallback is itself a value, so `var(--a, var(--b))` is
 * fine when either resolves, and `var(--a, 1px solid var(--b))` is NOT fine when both
 * `--a` and `--b` are undefined — the embedded `var()` is load-bearing even though the
 * fallback does not begin with it.
 */
function resolves(value: string, known: Set<string>): boolean {
  const trimmed = value.trim();
  if (trimmed.length === 0) return false;
  for (const open of topLevelVarOpens(trimmed)) {
    const { token, fallback, malformed } = parseVar(trimmed, open);
    if (malformed) return false;
    if (known.has(token)) continue;
    if (fallback === null || !resolves(fallback, known)) return false;
  }
  return true;
}

function collectKnownTokens(): { known: Set<string>; cssFiles: number } {
  const known = new Set<string>();
  const cssFiles = walk(SRC, ['.css']);
  for (const file of cssFiles) {
    const css = stripComments(readFileSync(file, 'utf-8'));
    for (const m of css.matchAll(/(--[A-Za-z0-9_-]+)\s*:/g)) known.add(m[1]);
  }
  // Tokens written from script at runtime never appear in a stylesheet. Two shapes
  // exist in this renderer and BOTH are load-bearing: a direct setProperty call, and a
  // quoted key in a React inline-style object (ParticipantGrid, FolderBar). Collecting
  // only the first would make the guard report a live token as "declared nowhere" the
  // moment someone removed a fallback that is currently masking it.
  for (const file of walk(SRC, ['.ts', '.tsx'])) {
    const ts = readFileSync(file, 'utf-8');
    for (const m of ts.matchAll(/setProperty\(\s*[`'"](--[A-Za-z0-9_-]+)/g)) known.add(m[1]);
    for (const m of ts.matchAll(/[`'"](--[A-Za-z0-9_-]+)[`'"]\s*:/g)) known.add(m[1]);
  }
  return { known, cssFiles: cssFiles.length };
}

describe('CSS custom-property references (client/desktop only)', () => {
  it('every var() without a usable fallback names a token that is declared somewhere', () => {
    const { known, cssFiles } = collectKnownTokens();

    // A silently-empty scan would make the assertion below pass while checking nothing.
    // Deliberately floors with headroom, not exact counts — an exact count churns on
    // every new component stylesheet. Live at time of writing: 140 files, 100 tokens.
    expect(cssFiles, 'no CSS files were scanned — the walker found nothing').toBeGreaterThan(100);
    expect(
      known.size,
      'no tokens were collected — the declaration regex matched nothing'
    ).toBeGreaterThan(50);
    // Negative control, and NOT interchangeable with the floors above: over-collection
    // makes `known` GROW, so no lower bound can see it. If the declaration regex ever
    // loses its `:` anchor, every *reference* becomes a "declaration", every undefined
    // token looks known, and the guard passes while checking nothing. `--border-primary`
    // is the sentinel — declared nowhere, still referenced in the tree as a
    // fallback-bearing reference, and the token this guard was written for.
    expect(
      known.has('--border-primary'),
      'the declaration regex is over-collecting — it is matching references, not declarations'
    ).toBe(false);
    // These are set ONLY from React inline-style objects and are declared in no
    // stylesheet. Nothing in the tree scan exercises the arm that collects them: every
    // CSS reference to them currently carries a fallback, so deleting that arm changes
    // no result today. It would go wrong later and silently — the first time someone
    // dropped one of those fallbacks, the guard would report a live, correctly-set
    // token as "declared nowhere" and send the reader to the CSS instead of to here.
    for (const runtime of ['--folder-scale', '--tile-w', '--tile-slot-w', '--pill-space']) {
      expect(
        known.has(runtime),
        `${runtime} is set from a React inline-style object — the collector must see it`
      ).toBe(true);
    }

    const broken: string[] = [];
    for (const file of walk(SRC, ['.css'])) {
      const css = stripComments(readFileSync(file, 'utf-8'));
      const lines = css.split('\n');
      const rel = relative(SRC_PARENT, file);
      // Case-insensitive: CSS function names are ASCII case-insensitive, so `VAR(--x)`
      // is a real reference.
      const scan = /var\(/gi;
      let m: RegExpExecArray | null;
      while ((m = scan.exec(css)) !== null) {
        const open = m.index + 3;
        const { token, fallback, end, malformed } = parseVar(css, open);
        const line = css.slice(0, m.index).split('\n').length;
        const source = lines[line - 1]?.trim() ?? '';
        if (malformed) {
          broken.push(`${rel}:${line}  ${source}  -> unterminated var( — cannot be checked`);
          break; // the rest of this file cannot be scanned reliably
        }
        if (!known.has(token) && !(fallback !== null && resolves(fallback, known))) {
          broken.push(`${rel}:${line}  ${source}  -> ${token} is declared nowhere`);
        }
        scan.lastIndex = end;
      }
    }

    expect(
      broken,
      `These declarations are dropped entirely at computed-value time, so the property does not render:\n${broken.join('\n')}\n`
    ).toEqual([]);
  });

  // The ratchet for the cosmetic class. The case above gates references that render
  // NOTHING; this one freezes the ones that render the WRONG THING, so the set can
  // only shrink. Asserting equality rather than a subset is deliberate and the whole
  // point: without the stale half, a future no-fallback bug could be "fixed" by adding
  // a fallback and the build would stay green over a live defect.
  it('the undefined-with-fallback inventory matches the frozen allowlist exactly', () => {
    const { known } = collectKnownTokens();
    const live = new Set<string>();
    for (const file of walk(SRC, ['.css'])) {
      const css = stripComments(readFileSync(file, 'utf-8'));
      const rel = relative(SRC_PARENT, file);
      const scan = /var\(/gi;
      let m: RegExpExecArray | null;
      while ((m = scan.exec(css)) !== null) {
        const { token, fallback, end, malformed } = parseVar(css, m.index + 3);
        if (malformed) break;
        if (fallback !== null && !known.has(token)) live.add(`${rel} ${token}`);
        scan.lastIndex = end;
      }
    }

    const allow = new Set(
      readFileSync(join(__dirname, 'undefined-token-allowlist.txt'), 'utf-8')
        .split('\n')
        .map((l) => l.trim())
        .filter((l) => l.length > 0 && !l.startsWith('#'))
    );

    const added = [...live].filter((x) => !allow.has(x)).sort();
    const stale = [...allow].filter((x) => !live.has(x)).sort();

    expect(
      added,
      `New undefined-token references. These paint the literal in their fallback and ignore the theme — fix the token rather than adding a line to the allowlist:\n${added.join('\n')}\n`
    ).toEqual([]);
    expect(
      stale,
      `Fixed — now delete these lines from undefined-token-allowlist.txt so the ratchet keeps its grip:\n${stale.join('\n')}\n`
    ).toEqual([]);
  });

  // The self-test. The case above pins the REPO's current state and goes red when
  // someone writes a bad reference; this one pins the PARSER and goes red when the
  // substitution model itself breaks. They fail for different reasons and want
  // different messages, which is why they are two cases rather than one.
  //
  // This exists because a mutation run once at authoring time only proves the guard
  // worked that afternoon ([internal]rules/tests.md: "Prove it by mutation, not by
  // inspection"). Carrying the discriminating cases makes that proof permanent —
  // the same reason .github/scripts/tests/ pairs each guard with a self-test.
  describe('resolves() — the CSS substitution model', () => {
    const known = new Set(['--known', '--other']);

    it.each([
      ['var(--nope, #444)', true, 'a literal fallback resolves'],
      ['var(--nope, var(--known))', true, 'a nested var() fallback resolves'],
      ['var(--known, var(--nope))', true, 'an UNUSED fallback does not invalidate'],
      ['var(--known)', true, 'a declared token resolves'],
      ['var( --known )', true, 'whitespace inside var() is tolerated'],
      ['VAR(--known)', true, 'the function name is ASCII case-insensitive'],
      ['var(--nope)', false, 'a bare undefined token is dropped'],
      ['var(--nope, var(--alsonope))', false, 'an all-undefined chain is dropped'],
      // The shape that slipped past the first version of this guard: the fallback does
      // not START with var(), so a leading-only check treated any non-empty text as
      // resolving — while Chromium drops the declaration.
      [
        'var(--nope, 1px solid var(--alsonope))',
        false,
        'an EMBEDDED undefined var() is load-bearing',
      ],
      ['var(--nope, 1px solid var(--known))', true, 'an embedded declared var() resolves'],
      ['var(--nope, )', false, 'an empty fallback resolves to nothing'],
      ['var(--nope', false, 'an unterminated var() is refused, never assumed good'],
    ])('%s -> %s (%s)', (value, expected) => {
      expect(resolves(value, known)).toBe(expected);
    });
  });
});
