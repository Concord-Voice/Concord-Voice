/**
 * Minimal CSS rule reader for tests that need to assert a declaration exists.
 *
 * These guards are narrow on purpose. jsdom performs no layout and no cascade,
 * so a rule that is missing, dead, or overridden is invisible to every other
 * automated check in the repo — a component mounts fine and every assertion
 * passes while the thing renders wrong. A handful of load-bearing declarations
 * are worth pinning at the text level; most are not, and this is not an
 * invitation to assert styling generally. Prefer looking at the app.
 */
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

/**
 * Resolve a path inside the desktop package.
 *
 * Deliberately cwd-relative rather than `import.meta.url`: Vitest transforms
 * test files, so `import.meta.url` is not a `file:` URL and `fileURLToPath`
 * throws. That throw surfaces as "no tests" rather than a failure, which makes
 * a clean run and a mutated one look identical — a falsification against it
 * proves nothing.
 */
export function fromDesktopPkg(relativePath: string): string {
  return resolve(process.cwd(), relativePath);
}

/**
 * Read any source file from a package-relative path.
 *
 * A CSS guard sometimes has to check the COMPONENT rather than the stylesheet:
 * whether the element a rule keys `:focus-visible` on is one that can take
 * focus is a fact about the TSX, and a stylesheet-only guard cannot see it.
 */
export function readSource(relativePath: string): string {
  return readFileSync(fromDesktopPkg(relativePath), 'utf8');
}

/** Read a stylesheet from a package-relative path. */
export function readCss(relativePath: string): string {
  return readSource(relativePath);
}

/** Collapse whitespace so a selector written across lines compares equal. */
const norm = (v: string) => v.trim().replace(/\s+/g, ' ');

/**
 * Index of the `}` that closes the `{` at `open`, or -1 when unbalanced.
 *
 * Counting depth is the whole fix for the at-rule defect described on
 * `ruleBody`. `indexOf('}', open)` answers "where is the next closing brace",
 * which is a different question and happens to agree only for a rule that
 * contains no nested block.
 */
function matchingBrace(css: string, open: number): number {
  let depth = 0;
  for (let i = open; i < css.length; i += 1) {
    if (css[i] === '{') depth += 1;
    else if (css[i] === '}') {
      depth -= 1;
      if (depth === 0) return i;
    }
  }
  return -1;
}

/** Walk one nesting level, descending into conditional group rules. */
function collectBodies(scope: string, wanted: string, bodies: string[]): void {
  let cursor = 0;
  for (;;) {
    const open = scope.indexOf('{', cursor);
    if (open === -1) break;
    const close = matchingBrace(scope, open);
    if (close === -1) break;

    // Everything since the previous block ends up in the prelude, which
    // includes any STATEMENT at-rule that sits between them. For
    // `@charset "UTF-8"; .a { … }` the raw prelude is `@charset "UTF-8"; .a`,
    // which matches no selector, so the first rule after a `@charset`,
    // `@import` or statement-form `@layer` was dropped. Keep only what follows
    // the last `;`.
    //
    // A `;` inside a quoted string would cut in the wrong place. No stylesheet
    // here has one, and handling it properly needs a real tokenizer — which is
    // more than these guards are worth. Stated rather than silently assumed.
    const rawPrelude = scope.slice(cursor, open);
    const prelude = rawPrelude.slice(rawPrelude.lastIndexOf(';') + 1);
    const body = scope.slice(open + 1, close);

    if (norm(prelude).startsWith('@') && body.includes('{')) {
      // A conditional group rule (@media, @supports, @layer, @container): its
      // body holds RULES, not declarations, so descend rather than match it.
      // An at-rule with no nested block (@font-face, @property) falls through
      // to the ordinary arm, where its own prelude can still be queried.
      collectBodies(body, wanted, bodies);
    } else if (prelude.split(',').map(norm).includes(wanted)) {
      bodies.push(body);
    }

    cursor = close + 1;
  }
}

/**
 * Every declaration a selector receives, from all rules that name it, joined —
 * including rules nested inside `@media` and friends. Null when the selector
 * appears in no rule at all.
 *
 * The UNION rather than the first block, because that is the question these
 * guards actually ask — "is this declaration present for this selector?" — and
 * a selector legitimately appears in several rules. `.user-panel-activity-policy`
 * does: a shared typography rule it sits in beside a sibling, plus its own. A
 * first-match reader returned the shared one and reported the declarations in
 * the specific rule missing.
 *
 * It deliberately does NOT model the cascade: later declarations are appended,
 * not resolved over earlier ones, and a rule's at-rule CONDITION is discarded
 * along the way — a declaration that applies only under
 * `prefers-reduced-motion` reads the same as an unconditional one. That is fine
 * for presence and `not.toMatch` absence checks, and wrong for anything asking
 * which value actually wins, or under what condition. Do not use it for that.
 *
 * Three parsing details, each of which produced a wrong answer on this branch:
 *
 * 1. COMMENTS ARE STRIPPED FIRST. A `}` inside a comment ends a block early —
 *    a rule whose comment contained `dialog { margin: auto }` had its body
 *    truncated at that brace while the declaration sat two lines below.
 * 2. SELECTOR LISTS. `a, b { ... }` never contains the literal `a {`, so a
 *    shared rule was reported absent entirely.
 * 3. BRACES ARE BALANCED, AND AT-RULES ARE DESCENDED INTO. Pairing an
 *    `@media` opening brace with `indexOf('}')` pairs it with its FIRST nested
 *    rule's closing brace, so that rule was skipped entirely while its
 *    siblings resolved normally — which is why the defect looked absent.
 *    It was live: both CSS guards on this branch query exactly the selector
 *    that is the first rule inside its `@media` block, so an absence
 *    assertion could not see a `display: none` added there. An earlier version
 *    of this docblock claimed absence checks were safe because "an override
 *    anywhere still shows up"; an override inside an at-rule did not.
 */
export function ruleBody(css: string, selector: string): string | null {
  const bare = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const bodies: string[] = [];
  collectBodies(bare, norm(selector), bodies);
  return bodies.length === 0 ? null : bodies.join('\n');
}
