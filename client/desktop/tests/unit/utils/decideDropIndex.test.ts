/**
 * Unit tests for `src/renderer/utils/policy/decideDropIndex.ts` (#2359).
 *
 * SYNTHETIC RECTS ONLY — nothing is rendered here, deliberately. jsdom reports
 * zero-height rects and Vitest runs with `css: false`, so a geometry assertion
 * taken through rendered DOM passes for the wrong reason. That is precisely how
 * the previous attempt at this feature ended up with drop logic no test
 * executed. Rendered geometry belongs to Playwright; this file owns the maths.
 */
import { describe, it, expect } from 'vitest';

import { decideDropIndex } from '@/renderer/utils/policy/decideDropIndex';

/** A synthetic, fully-formed DOMRect. Only `top`/`bottom` are read. */
function rect(top: number, height: number): DOMRect {
  const bottom = top + height;
  return {
    x: 0,
    y: top,
    width: 200,
    height,
    top,
    right: 200,
    bottom,
    left: 0,
    toJSON: () => ({}),
  } as DOMRect;
}

/** Three 40px rows with 10px gutters: [0,40] [50,90] [100,140]. Mids: 20/70/120. */
const stack = [rect(0, 40), rect(50, 40), rect(100, 40)];
const GAP = 10;

describe('decideDropIndex — empty and single-row bands', () => {
  it('inserts at 0 for an empty band, whatever the pointer is doing', () => {
    for (const y of [-1000, 0, 37, 1000]) {
      expect(decideDropIndex(y, [], GAP)).toBe(0);
    }
  });

  it('splits a single row strictly at its midpoint', () => {
    const one = [rect(100, 40)]; // mid = 120

    expect(decideDropIndex(119, one, GAP)).toBe(0);
    expect(decideDropIndex(121, one, GAP)).toBe(1);
  });

  it('breaks the exact-midpoint tie AFTER the row (at-or-below inserts after)', () => {
    const one = [rect(100, 40)];

    expect(decideDropIndex(120, one, GAP)).toBe(1);
    // …and the pixel above it is still before, so the tie-break is a boundary,
    // not a plateau.
    expect(decideDropIndex(119.999, one, GAP)).toBe(0);
  });
});

describe('decideDropIndex — 50/50 midpoint, no dead band', () => {
  it('maps each row to before/after purely by its own midpoint', () => {
    // Row 0 (mid 20)
    expect(decideDropIndex(0, stack, GAP)).toBe(0);
    expect(decideDropIndex(19, stack, GAP)).toBe(0);
    expect(decideDropIndex(20, stack, GAP)).toBe(1);
    expect(decideDropIndex(39, stack, GAP)).toBe(1);
    // Row 1 (mid 70)
    expect(decideDropIndex(69, stack, GAP)).toBe(1);
    expect(decideDropIndex(70, stack, GAP)).toBe(2);
    // Row 2 (mid 120)
    expect(decideDropIndex(119, stack, GAP)).toBe(2);
    expect(decideDropIndex(120, stack, GAP)).toBe(3);
  });

  it('has NO dead band — the middle half of a row still resolves by midpoint', () => {
    // Contrast with ChannelList.tsx's `deadZone = rect.height * 0.25` tri-state:
    // for row 2 ([100,140]) that dead zone would be [110,130] and would make
    // both of these a THIRD outcome. Here they are ordinary before/after.
    expect(decideDropIndex(111, stack, GAP)).toBe(2); // 27.5% into the row
    expect(decideDropIndex(129, stack, GAP)).toBe(3); // 72.5% into the row

    // Same check on the first row's dead-zone band [10,30].
    expect(decideDropIndex(11, stack, GAP)).toBe(0);
    expect(decideDropIndex(29, stack, GAP)).toBe(1);
  });

  it('changes its answer exactly at the row midpoints and nowhere else', () => {
    // Structural restatement of "strict 50/50, no hysteresis, no dead band":
    // the result is monotone non-decreasing in pointerY, and every transition
    // sits on a midpoint.
    const transitions: number[] = [];
    let previous = decideDropIndex(-60, stack, GAP);

    for (let y = -60; y <= 220; y += 1) {
      const current = decideDropIndex(y, stack, GAP);
      expect(current).toBeGreaterThanOrEqual(previous);
      expect(current).toBeLessThanOrEqual(stack.length);
      if (current !== previous) {
        transitions.push(y);
        previous = current;
      }
    }

    expect(transitions).toEqual([20, 70, 120]);
  });
});

describe('decideDropIndex — totality (gutters, above, below)', () => {
  it('resolves a pointer in a gutter to a definite index, never "nowhere"', () => {
    // Gutter between row 0 and row 1 is [40,50]; it belongs to index 1 because
    // row 1's midpoint (70) has not been reached.
    for (const y of [41, 45, 49]) {
      expect(decideDropIndex(y, stack, GAP)).toBe(1);
    }
    // Gutter between row 1 and row 2 is [90,100] → index 2.
    for (const y of [91, 95, 99]) {
      expect(decideDropIndex(y, stack, GAP)).toBe(2);
    }
  });

  it('resolves a pointer above the first row to 0', () => {
    for (const y of [-1, -9, -10, -11, -1000]) {
      expect(decideDropIndex(y, stack, GAP)).toBe(0);
    }
  });

  it('resolves a pointer below the last row to rects.length', () => {
    for (const y of [141, 150, 151, 1000]) {
      expect(decideDropIndex(y, stack, GAP)).toBe(stack.length);
    }
  });
});

describe('decideDropIndex — gapPx', () => {
  const sweep = (gap: number): number[] => {
    const out: number[] = [];
    for (let y = -80; y <= 240; y += 1) {
      out.push(decideDropIndex(y, stack, gap));
    }
    return out;
  };

  it('changes NO decision for well-formed sorted, non-overlapping rects', () => {
    const baseline = sweep(0);

    for (const gap of [1, 4, 8, 10, 16, 64, 999]) {
      expect(sweep(gap)).toEqual(baseline);
    }
  });

  it('treats a non-finite or non-positive gap the same as 0', () => {
    const baseline = sweep(0);

    for (const gap of [Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY, -5, -0]) {
      expect(sweep(gap)).toEqual(baseline);
    }
  });
});

describe('decideDropIndex — degenerate pointer input (documented actual behaviour)', () => {
  it('resolves a NaN pointerY to rects.length rather than throwing', () => {
    // Every `y < mid` comparison against NaN is false, so the loop falls
    // through to the tail index. Recorded as ACTUAL behaviour: the function
    // stays total, but "after the last row" is an arbitrary answer to an
    // un-answerable question. A caller must never hand it a NaN pointer.
    expect(decideDropIndex(Number.NaN, stack, GAP)).toBe(stack.length);
    expect(decideDropIndex(Number.NaN, [], GAP)).toBe(0);
  });
});
