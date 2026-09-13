import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';

/**
 * Foreground/background contrast across every theme block.
 *
 * The sibling guard, css-var-references.test.ts, proves every `var()` RESOLVES.
 * It has no concept of two declarations that must move together, which is the
 * defect class that produced this file: a rule whose background was repointed at
 * a theme token while its foreground stayed a literal renders at 1.07:1 in the
 * high-contrast theme and passes every existing assertion.
 *
 * This test pairs the two. For each rule that sets both a foreground and a
 * background, it resolves both through all 34 theme blocks and measures the
 * worst contrast. Current failures are frozen in an allowlist that ALSO fails
 * when an entry stops being a failure, so a fixed pair cannot silently rot back.
 *
 * WHAT IT CANNOT SEE, stated plainly so the green is not read as more than it is:
 *   - A rule that sets a foreground and inherits its background from an ancestor.
 *     Resolving that needs the DOM, not the stylesheet. 287 pairs in the
 *     tree carry a colour this cannot pair with anything.
 *   - Backgrounds that are gradients, `rgba(...)`, `color-mix(...)`, or keywords.
 *     Alpha compositing depends on what is behind the element.
 *   - Font size. WCAG allows 3:1 for large text; this uses 4.5:1 everywhere, so
 *     a large-text pair between 3 and 4.5 lands in the allowlist rather than
 *     being waved through. That is deliberate: a wrong exemption is invisible,
 *     an over-strict allowlist entry is merely noisy.
 */

import {
  ALLOWLIST,
  baseSelectorOf,
  winningDeclaration,
  contrastRatio,
  inheritStateColors,
  measure,
  mergeBySelector,
  parseThemeBlocks,
  splitSelectorList,
  toRgb,
} from './contrastPairs';

/**
 * Allowlist entries, keyed `<path>\t<selector>` with the accepted worst ratio.
 *
 * The ratio is the half that makes this a ratchet rather than a mute button. Keyed
 * on the pair alone, an entry excused the pair at ANY ratio — a listed failure could
 * slide from 4.4:1 to 1:1 and the suite stayed green, because the only other check
 * looks for RECOVERY above the floor. The guard claimed a pair could not worsen
 * unnoticed and that claim was false until this existed.
 */
function readAllowlist(): Map<string, number> {
  const entries = new Map<string, number>();
  for (const raw of readFileSync(ALLOWLIST, 'utf8').split('\n')) {
    const line = raw.replace(/\r$/, '');
    if (line.length === 0 || line.startsWith('#')) continue;
    const parts = line.split('\t');
    expect(parts, `malformed allowlist line: ${JSON.stringify(line)}`).toHaveLength(3);
    const baseline = Number(parts[2]);
    expect(Number.isFinite(baseline), `non-numeric ratio in: ${JSON.stringify(line)}`).toBe(true);
    entries.set(`${parts[0]}\t${parts[1]}`, baseline);
  }
  return entries;
}

/** Ratios are stored to 2dp, so only a drop beyond rounding counts as worse. */
const RATIO_EPSILON = 0.011;

describe('theme contrast pairs', () => {
  const { checked, unresolvable, failures } = measure();
  const allowed = readAllowlist();

  it('finds a theme surface to measure against at all', () => {
    // Negative control. If the marker token is ever renamed, parseThemeBlocks
    // returns [], every pair becomes unresolvable, and the suite goes green over
    // zero measurements. These floors make that loud instead of silent.
    expect(parseThemeBlocks().length).toBeGreaterThanOrEqual(30);
    expect(checked).toBeGreaterThan(250);
  });

  it('no foreground/background pair falls below 4.5:1 outside the allowlist', () => {
    const unexpected = [...failures.values()]
      .filter((f) => !allowed.has(f.key))
      .sort((a, b) => a.worst - b.worst)
      .map(
        (f) =>
          `${f.worst.toFixed(2)}:1  ${f.key.replaceAll('\t', '  ')}  (worst in ${f.worstBlock})`
      );

    expect(
      unexpected,
      'New contrast failures. Pair the foreground with its background — a themed ' +
        'background with a literal or mismatched foreground is the defect this guards. ' +
        'If the pair is genuinely acceptable, add the tab-separated line to ' +
        'contrast-pair-allowlist.txt with a reason in the commit message.'
    ).toEqual([]);
  });

  it('no allowlisted pair has got worse than its recorded baseline', () => {
    // The other half of the ratchet. Without this an entry is a permanent excuse
    // at any ratio; with it, the listed number is the contract.
    const regressed = [...failures.values()]
      .filter((f) => {
        const baseline = allowed.get(f.key);
        return baseline !== undefined && f.worst < baseline - RATIO_EPSILON;
      })
      .sort((a, b) => a.worst - b.worst)
      .map(
        (f) =>
          `${f.key.replaceAll('\t', '  ')}: was ${allowed.get(f.key)!.toFixed(2)}:1, now ` +
          `${f.worst.toFixed(2)}:1 (worst in ${f.worstBlock})`
      );

    expect(
      regressed,
      'An already-failing pair got worse. Either restore it, or update its recorded ' +
        'ratio in contrast-pair-allowlist.txt and say in the commit message why the ' +
        'regression is acceptable.'
    ).toEqual([]);
  });

  it('the allowlist has no stale entries', () => {
    const stale = [...allowed.keys()].filter((entry) => !failures.has(entry)).sort();
    expect(
      stale,
      'Fixed — now delete these lines from contrast-pair-allowlist.txt so the ratchet keeps its grip'
    ).toEqual([]);
  });

  it('reports what it structurally cannot measure', () => {
    // Not an assertion about quality — a standing reminder that a green here
    // covers the resolvable subset only. If this number collapses toward zero,
    // the extractor has probably broken rather than the tree having improved.
    expect(unresolvable).toBeGreaterThan(0);
  });
});

describe('baseSelectorOf', () => {
  // Every case here fails under a plain `/:(hover|focus|...)/g` replace, which is
  // what this file shipped with first. The `:not()` cases silently DROPPED pairs
  // from measurement rather than mis-measuring them, so the suite stayed green
  // over exactly the failures it exists to catch.
  it.each([
    // Nested states must survive — stripping them yields `.btn:not()`, which
    // matches no base rule, so the pair is never compared to anything.
    ['.btn-danger:hover:not(:disabled)', '.btn-danger:not(:disabled)'],
    ['.x:not(:hover)', '.x:not(:hover)'],
    ['.x:hover:not(.y:disabled)', '.x:not(.y:disabled)'],
    // Longest-first alternation — `focus` before `focus-visible` leaves `-visible`.
    ['.x:focus-visible', '.x'],
    ['.x:focus-within', '.x'],
    ['.x:focus', '.x'],
    // Ordinary cases.
    ['.x:hover', '.x'],
    ['.x:active', '.x'],
    ['.x', '.x'],
    ['.a .b:hover', '.a .b'],
  ])('%s → %s', (input, expected) => {
    expect(baseSelectorOf(input)).toBe(expected);
  });

  it('inherits a foreground through a compound state selector', () => {
    const rules = [
      {
        file: 'f.css',
        selector: '.btn',
        color: '#ffffff',
        background: null,
        inheritedColor: false,
      },
      {
        file: 'f.css',
        selector: '.btn:hover:not(:disabled)',
        color: null,
        background: 'var(--danger)',
        inheritedColor: false,
      },
    ];
    inheritStateColors(rules);
    expect(rules[1].color).toBe('#ffffff');
    expect(rules[1].inheritedColor).toBe(true);
  });
});

describe('splitSelectorList', () => {
  // The original extractor kept `selectorList.split('\n').pop()`, so every
  // selector but the last was discarded and never measured. A dropped pair and a
  // passing pair are indistinguishable in the output, which is what made this
  // worth a test rather than a comment.
  it.each([
    ['.a', ['.a']],
    ['.a, .b', ['.a', '.b']],
    ['.role-reorder-notice,\n.role-reorder-alert', ['.role-reorder-notice', '.role-reorder-alert']],
    // Commas inside a functional pseudo are arguments, not separators.
    ['.a:not(.b, .c)', ['.a:not(.b, .c)']],
    ['.a:is(.b, .c), .d', ['.a:is(.b, .c)', '.d']],
    // Descendant whitespace collapses so the key matches however it was written.
    ['.a   .b,\n  .c', ['.a .b', '.c']],
  ])('%j → %j', (input, expected) => {
    expect(splitSelectorList(input)).toEqual(expected);
  });
});

describe('mergeBySelector', () => {
  it('pairs a foreground and background declared in separate rules', () => {
    // The real shape: a shared comma rule sets the surface, a later rule of its
    // own sets the text. Neither rule alone carries a pair, so without merging
    // the selector is never measured at all.
    const merged = mergeBySelector([
      {
        file: 'f.css',
        selector: '.notice',
        color: null,
        background: 'var(--bg-tertiary)',
        inheritedColor: false,
      },
      {
        file: 'f.css',
        selector: '.notice',
        color: 'var(--text-muted)',
        background: null,
        inheritedColor: false,
      },
    ]);
    expect(merged).toHaveLength(1);
    expect(merged[0].color).toBe('var(--text-muted)');
    expect(merged[0].background).toBe('var(--bg-tertiary)');
  });

  it('lets a later declaration win, as the cascade does', () => {
    const merged = mergeBySelector([
      { file: 'f.css', selector: '.x', color: '#111111', background: null, inheritedColor: false },
      { file: 'f.css', selector: '.x', color: '#222222', background: null, inheritedColor: false },
    ]);
    expect(merged[0].color).toBe('#222222');
  });

  it('keeps selectors from different files apart', () => {
    const merged = mergeBySelector([
      { file: 'a.css', selector: '.x', color: '#111111', background: null, inheritedColor: false },
      { file: 'b.css', selector: '.x', color: null, background: '#ffffff', inheritedColor: false },
    ]);
    expect(merged).toHaveLength(2);
    expect(merged.every((r) => r.color === null || r.background === null)).toBe(true);
  });
});

describe('toRgb', () => {
  // Returning null for a keyword classified the pair UNRESOLVABLE and skipped it,
  // which is the same silent-skip shape as the two selector bugs: `color: white`
  // on an accent fill measures 1.07:1 in dark high contrast and was invisible.
  it.each([
    ['white', [255, 255, 255]],
    ['black', [0, 0, 0]],
    ['WHITE', [255, 255, 255]],
    ['#fff', [255, 255, 255]],
    ['#ffffff', [255, 255, 255]],
  ])('%s parses', (input, expected) => {
    expect(toRgb(input)).toEqual(expected);
  });

  it.each([['transparent'], ['none'], ['inherit'], ['currentColor'], ['var(--x)']])(
    '%s stays unresolvable',
    (input) => {
      // Not a gap. Each of these depends on what is painted behind or above the
      // element, which a stylesheet scan cannot know; they are counted as
      // unresolvable rather than guessed at.
      expect(toRgb(input)).toBeNull();
    }
  );
});

describe('winningDeclaration', () => {
  const COLOR = () => /(?:^|[;\s])color\s*:\s*([^;]+);/g;
  // Taking the FIRST match scored `color: #000; color: #fff` as black — a rule
  // that paints white on white was measured as passing.
  it.each([
    ['color: #000;', '#000'],
    ['color: #000; color: #fff;', '#fff'],
    ['color: #fff !important; color: #000;', '#fff'],
    ['color: #111; color: #222 !important; color: #333;', '#222'],
    ['color: #abc !IMPORTANT;', '#abc'],
    ['background: #fff;', null],
  ])('%s → %s', (body, expected) => {
    expect(winningDeclaration(body, COLOR())).toBe(expected);
  });

  it('strips !important so the value still parses as a colour', () => {
    const won = winningDeclaration('color: #ffffff !important;', COLOR());
    expect(toRgb(won)).toEqual([255, 255, 255]);
  });
});

describe('theme block token inheritance', () => {
  it('lays :root under every other block', () => {
    // These selectors all match the same element, so a token a scheme does not
    // redeclare still resolves through the cascade. Reading only a block's own
    // map made such a token look undeclared and skipped the whole block.
    const blocks = parseThemeBlocks();
    const root = blocks.find((b) => b.selector === ':root');
    expect(root).toBeDefined();
    const rootKeys = Object.keys(root!.tokens);
    expect(rootKeys.length).toBeGreaterThan(20);
    for (const block of blocks) {
      for (const key of rootKeys) {
        expect(block.tokens[key], `${block.selector} cannot resolve ${key}`).toBeDefined();
      }
    }
  });

  it('lets a block override what it does redeclare', () => {
    const blocks = parseThemeBlocks();
    const hc = blocks.find((b) => b.selector === "[data-high-contrast='true']");
    expect(hc).toBeDefined();
    // Inheritance must not clobber the block's own value.
    expect(hc!.tokens['--accent-color']).toBe('#ffff00');
    expect(hc!.tokens['--on-accent']).toBe('#000000');
  });
});

describe('contrastRatio', () => {
  it.each([
    ['#000000', '#ffffff', 21],
    ['#ffffff', '#ffffff', 1],
    ['#ffffff', '#ffff00', 1.07],
    ['#000000', '#ffff00', 19.56],
  ])('%s on %s ≈ %s', (fg, bg, expected) => {
    const ratio = contrastRatio(toRgb(fg)!, toRgb(bg)!);
    expect(ratio).toBeCloseTo(expected as number, 1);
  });
});
