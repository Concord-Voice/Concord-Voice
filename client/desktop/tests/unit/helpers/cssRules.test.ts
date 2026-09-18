/**
 * Guards for the CSS rule reader that the three stylesheet guards depend on.
 *
 * This file exists because the helper shipped without one and a real defect
 * rode that gap: `ruleBody` paired an `@media` opening brace with its FIRST
 * nested rule's closing brace, so that rule was skipped while every sibling
 * resolved normally. Both stylesheet guards on this branch query exactly the
 * selector that is the first rule inside its `@media` block, so their
 * `not.toMatch(/display:\s*none/)` assertions could not have seen a
 * `display: none` added there — the assertions were live-vacuous, not
 * theoretically so.
 *
 * A parser used only by absence assertions has to be tested by ASSERTING WHAT
 * IT FINDS, because an absence assertion passes identically whether the parser
 * is correct or blind. Every case below is therefore a positive one, and each
 * at-rule fixture first asserts the declaration really is in the fixture.
 */
import { describe, it, expect } from 'vitest';
import { ruleBody } from '../../helpers/cssRules';

describe('ruleBody', () => {
  it('finds a plain top-level rule (positive control)', () => {
    // Without this, a parser that finds NOTHING would pass every at-rule case
    // below by the same route it fails them.
    expect(ruleBody('.a { color: red; }', '.a')).toMatch(/color:\s*red/);
  });

  it('returns null for a selector no rule names', () => {
    expect(ruleBody('.a { color: red; }', '.b')).toBeNull();
  });

  it('includes declarations from the FIRST rule inside an at-rule', () => {
    // The regression. `.first` is the first rule inside the @media block; the
    // old reader skipped it entirely and returned only the base rule's body.
    const css = `
      .first { opacity: 0; }
      @media (prefers-reduced-motion: reduce) {
        .first { display: none; }
        .second { visibility: hidden; }
      }
    `;
    // Vacuity guard: the fixture must actually carry the declaration, or the
    // assertion below proves nothing about the parser.
    expect(css).toMatch(/\.first\s*\{\s*display:\s*none/);

    const body = ruleBody(css, '.first') ?? '';
    expect(body).toMatch(/opacity:\s*0/); // the unconditional rule
    expect(body).toMatch(/display:\s*none/); // the at-rule nested rule
  });

  it('still includes later rules inside an at-rule', () => {
    // These always worked. Pinned so a fix aimed at the first rule cannot
    // regress the siblings that were already correct.
    const css = `@media screen { .first { color: red; } .second { visibility: hidden; } }`;
    expect(ruleBody(css, '.second')).toMatch(/visibility:\s*hidden/);
  });

  it('matches a selector list nested inside an at-rule', () => {
    // Two failure modes compounded: selector-list matching AND the at-rule
    // skip. The real VoiceStage stylesheet has exactly this shape.
    const css = `
      @media (prefers-reduced-motion: reduce) {
        .cell .controls,
        .stage .controls {
          transition: none;
        }
      }
    `;
    expect(ruleBody(css, '.cell .controls')).toMatch(/transition:\s*none/);
    expect(ruleBody(css, '.stage .controls')).toMatch(/transition:\s*none/);
  });

  it('descends through a nested at-rule', () => {
    const css = `@supports (display: grid) { @media screen { .a { gap: 4px; } } }`;
    expect(ruleBody(css, '.a')).toMatch(/gap:\s*4px/);
  });

  it('finds the first rule after a statement at-rule', () => {
    // `@charset`/`@import`/statement-`@layer` end in `;`, not a block, so they
    // land in the NEXT rule's prelude and stopped it matching. Only the rule
    // immediately after one is affected, which is why it reads as absent.
    for (const statement of ['@charset "UTF-8";', '@import url(x.css);', '@layer base, utils;']) {
      const css = `${statement} .a { color: red; }`;
      expect(ruleBody(css, '.a')).toMatch(/color:\s*red/);
    }
  });

  it('does not truncate a rule whose comment contains braces', () => {
    const css = `.a { /* like dialog { margin: auto } */ margin: auto; }`;
    expect(ruleBody(css, '.a')).toMatch(/margin:\s*auto\s*;/);
  });

  it('unions every rule naming the selector, in and out of at-rules', () => {
    const css = `
      .a, .b { font-size: 1rem; }
      .a { color: red; }
      @media print { .a { color: black; } }
    `;
    const body = ruleBody(css, '.a') ?? '';
    expect(body).toMatch(/font-size:\s*1rem/);
    expect(body).toMatch(/color:\s*red/);
    expect(body).toMatch(/color:\s*black/);
  });

  it('leaves an at-rule with no nested block queryable by its own prelude', () => {
    // @font-face and @property carry declarations directly. They must not be
    // descended into, or their body is scanned as if it held rules.
    const css = `@font-face { font-family: X; src: url(y.woff2); }`;
    expect(ruleBody(css, '@font-face')).toMatch(/font-family:\s*X/);
  });

  it('gives up rather than looping on an unbalanced stylesheet', () => {
    expect(ruleBody('.a { color: red;', '.a')).toBeNull();
  });
});
