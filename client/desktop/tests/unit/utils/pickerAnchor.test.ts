import { describe, expect, it } from 'vitest';
import {
  ARROW_HALF_CHORD,
  arrowInset,
  resolveAnchoredPlacement,
  type ResolveAnchoredPlacementInput,
} from '@/renderer/utils/ui/pickerAnchor';

// #2370 A5 -- pickerAnchor.ts is pure geometry (no React, no DOM, no ref, no
// useEffect -- see the module's own header comment and [internal]rules/frontend.md
// § Picker geometry and arrow linkage), so it is unit-tested directly with
// plain numbers in, plain numbers out.

describe('arrowInset (A5)', () => {
  it('arrowInset(10, 1) === 18 -- GIF picker geometry (cornerRadius 10, border 1)', () => {
    expect(arrowInset(10, 1)).toBe(18);
  });

  it('arrowInset(8, 1) === 16 -- emoji picker geometry (cornerRadius 8, border 1)', () => {
    expect(arrowInset(8, 1)).toBe(16);
  });

  it('is the derived formula ceil(cornerRadius + ARROW_HALF_CHORD - borderWidth), not two pinned literals', () => {
    // A test that only hardcodes 18/16 cannot distinguish "the formula
    // happens to produce these two values" from "these two values are
    // pinned independent of the formula" -- recompute from the exported
    // constant so a change to ARROW_HALF_CHORD (or to the formula's shape)
    // moves this assertion in lockstep with production, rather than silently
    // diverging from it.
    expect(arrowInset(10, 1)).toBe(Math.ceil(10 + ARROW_HALF_CHORD - 1));
    expect(arrowInset(8, 1)).toBe(Math.ceil(8 + ARROW_HALF_CHORD - 1));
  });

  it('a different cornerRadius/borderWidth pair derives a different inset (not a shared constant)', () => {
    // Two pickers with different cornerRadius must NOT collapse to the same
    // inset -- pinning one shared literal for both either grazes the
    // tighter-cornered picker's corner or wastes pixels on the other
    // (spec §1 R2).
    expect(arrowInset(10, 1)).not.toBe(arrowInset(8, 1));
  });
});

describe('resolveAnchoredPlacement totality (A5)', () => {
  const baseInput: ResolveAnchoredPlacementInput = {
    anchorTop: 500,
    anchorRight: 300,
    anchorCenterX: 260,
    measuredWidth: 400,
    measuredHeight: 300,
    viewportWidth: 1200,
    viewportHeight: 800,
    borderWidth: 1,
    cornerRadius: 10,
  };

  /** The documented clamp band from spec §2.2: arrowX is always within
   *  [inset, max(inset, paddingBoxWidth - inset)]. Computed independently of
   *  resolveAnchoredPlacement's own internals -- from arrowInset (already
   *  pinned above) and the documented paddingBoxWidth formula -- so this is
   *  a check against the PUBLISHED contract, not a mirror of the
   *  implementation. */
  function expectedArrowBand(input: ResolveAnchoredPlacementInput) {
    const inset = arrowInset(input.cornerRadius, input.borderWidth);
    const paddingBoxWidth = input.measuredWidth - 2 * input.borderWidth;
    return { inset, upper: Math.max(inset, paddingBoxWidth - inset) };
  }

  function assertTotal(input: ResolveAnchoredPlacementInput) {
    const result = resolveAnchoredPlacement(input);
    expect(Number.isFinite(result.left)).toBe(true);
    expect(Number.isFinite(result.top)).toBe(true);
    expect(Number.isFinite(result.arrowX)).toBe(true);
    expect(typeof result.showArrow).toBe('boolean');

    const { inset, upper } = expectedArrowBand(input);
    expect(result.arrowX).toBeGreaterThanOrEqual(inset);
    expect(result.arrowX).toBeLessThanOrEqual(upper);
    return result;
  }

  it('a picker wider than the viewport still resolves finite geometry within the clamp band', () => {
    assertTotal({ ...baseInput, measuredWidth: 2000, viewportWidth: 800 });
  });

  it('zero measured width/height still resolves finite geometry within the clamp band', () => {
    assertTotal({ ...baseInput, measuredWidth: 0, measuredHeight: 0 });
  });

  it('an anchor entirely off-screen (negative coordinates) still resolves finite geometry', () => {
    assertTotal({
      ...baseInput,
      anchorTop: -1000,
      anchorRight: -500,
      anchorCenterX: -500,
    });
  });

  it('an anchor far beyond the viewport (huge coordinates) still resolves finite geometry', () => {
    assertTotal({
      ...baseInput,
      anchorTop: 100_000,
      anchorRight: 100_000,
      anchorCenterX: 100_000,
    });
  });

  it('zero viewport dimensions still resolve finite geometry', () => {
    assertTotal({ ...baseInput, viewportWidth: 0, viewportHeight: 0 });
  });

  it('a negative measured width (a corrupt measurement) still resolves finite geometry', () => {
    assertTotal({ ...baseInput, measuredWidth: -50, measuredHeight: -20 });
  });

  // ── Non-degenerate behavioral pins, so the totality checks above cannot ──
  // ── be satisfied by a function that is merely total and otherwise wrong ──

  it('clamps left into the viewport when the anchor would place the picker off the right edge', () => {
    const result = resolveAnchoredPlacement({
      ...baseInput,
      anchorRight: 1500,
      measuredWidth: 400,
      viewportWidth: 1200,
    });
    // preferredLeft = 1500 - 400 = 1100; viewport ceiling = 1200-400-8 = 792,
    // and 1100 > 792 -- this DOES exercise the clamp (unlike a preferredLeft
    // that already fits, which would pass this assertion whether or not the
    // clamp code path ran at all).
    expect(result.left).toBe(792);
  });

  it('suppresses the arrow when the anchor centre falls outside the resolved picker span', () => {
    const result = resolveAnchoredPlacement({
      ...baseInput,
      anchorRight: 2000,
      anchorCenterX: 2000,
      measuredWidth: 300,
      viewportWidth: 1200,
    });
    expect(result.showArrow).toBe(false);
  });

  it('shows the arrow in the ordinary case (positive control for the suppression checks above)', () => {
    const result = resolveAnchoredPlacement({
      ...baseInput,
      anchorTop: 800,
      anchorRight: 500,
      anchorCenterX: 460,
      measuredWidth: 300,
      measuredHeight: 200,
      viewportWidth: 1200,
      viewportHeight: 900,
    });
    expect(result.showArrow).toBe(true);
  });
});
