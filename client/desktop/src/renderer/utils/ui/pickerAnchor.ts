/**
 * Pure geometry for anchor-relative popover/dialog placement and arrow
 * linkage (#2370). This module is the shared primitive the project lead
 * approved with a hard boundary (see spec §1 R11 /
 * `[internal]rules/frontend.md` § Picker geometry and arrow linkage): plain
 * numbers in, plain numbers out — no React import, no DOM node, no ref, no
 * `useEffect`. If a future change needs any of those, it has become the
 * primitive the lead declined and belongs in a new decision, not here.
 */

/** Gap between the anchor and the picker's edge, matching
 *  `AttributedPopover`'s `ANCHOR_GAP` (`AttributedPopover.tsx:16`). */
export const PICKER_ANCHOR_GAP = 12;

/** Minimum distance kept between the picker and the viewport edge. */
export const VIEWPORT_GUTTER = 8;

/** Half-diagonal of the 12x12 arrow caret rotated 45deg — `6 * sqrt(2)`,
 *  ~8.4853. This is how far the caret's tip protrudes past the picker's
 *  border-box edge; see the `overflow-clip-margin` rule this derives at
 *  `[internal]rules/frontend.md` § Picker geometry and arrow linkage. */
export const ARROW_HALF_CHORD = 6 * Math.SQRT2;

/**
 * Minimum distance the arrow's centre may sit from the picker's left/right
 * edge, so the caret's rotated corners never spill past the picker's own
 * rounded corner. Derived, not pinned:
 * `ceil(cornerRadius + ARROW_HALF_CHORD - borderWidth)`.
 * `arrowInset(10, 1) === 18` (GIF); `arrowInset(8, 1) === 16` (emoji).
 */
export function arrowInset(cornerRadius: number, borderWidth: number): number {
  return Math.ceil(cornerRadius + ARROW_HALF_CHORD - borderWidth);
}

export interface ResolveAnchoredPlacementInput {
  /** Anchor's `getBoundingClientRect().top`. */
  anchorTop: number;
  /** Anchor's `getBoundingClientRect().right`. */
  anchorRight: number;
  /** Anchor's horizontal centre — `rect.left + rect.width / 2`. */
  anchorCenterX: number;
  /** The picker's own measured `offsetWidth`. */
  measuredWidth: number;
  /** The picker's own measured `offsetHeight`. */
  measuredHeight: number;
  viewportWidth: number;
  /** Accepted for a self-describing, symmetric input shape. The Y rule below
   *  only ever clamps toward the `VIEWPORT_GUTTER` floor — the picker always
   *  renders above its anchor, so the viewport's far (bottom) edge is not a
   *  bound this function needs; short-viewport height fitting is handled by
   *  the stylesheet's `min()` body-height formulas instead (spec §2.3). */
  viewportHeight: number;
  /** The picker's own CSS `border-width` in px — offsets in border-box
   *  coordinates are 1 border-width off from the padding box CSS `left`
   *  resolves against. */
  borderWidth: number;
  /** The picker's own CSS `border-radius` in px. */
  cornerRadius: number;
}

export interface ResolvedAnchoredPlacement {
  /** CSS `left`, in border-box viewport coordinates. */
  left: number;
  /** CSS `top`, in border-box viewport coordinates. */
  top: number;
  /** The arrow's horizontal offset, in PADDING-BOX coordinates (see below). */
  arrowX: number;
  /** Whether the arrow should render at all. */
  showArrow: boolean;
}

/**
 * Places a picker above and right-aligned to its anchor, clamped to the
 * viewport, and resolves the arrow's offset plus whether it should render.
 * Total for every input, including degenerate (zero, negative, or
 * larger-than-viewport) measured dimensions — see the defensive `Math.max`
 * wrapping below, the same shape `AttributedPopover.tsx:54-57` uses.
 */
export function resolveAnchoredPlacement(
  input: ResolveAnchoredPlacementInput
): ResolvedAnchoredPlacement {
  const {
    anchorTop,
    anchorRight,
    anchorCenterX,
    measuredWidth,
    measuredHeight,
    viewportWidth,
    borderWidth,
    cornerRadius,
  } = input;

  // Right-aligned to the anchor, clamped into the viewport.
  const preferredLeft = anchorRight - measuredWidth;
  const left = Math.max(
    VIEWPORT_GUTTER,
    Math.min(preferredLeft, viewportWidth - measuredWidth - VIEWPORT_GUTTER)
  );

  // Above the anchor, clamped to the gutter floor. Reaching that floor means
  // the picker's bottom edge is no longer adjacent to the anchor, which is
  // what disqualifies the arrow below (R7/R9) — the Y case is the real
  // liar, not the X clamp.
  const naturalTop = anchorTop - measuredHeight - PICKER_ANCHOR_GAP;
  const top = Math.max(VIEWPORT_GUTTER, naturalTop);
  const topWasClamped = naturalTop < VIEWPORT_GUTTER;

  // arrowX is PADDING-BOX coordinates — anchorCenterX - left - borderWidth —
  // because CSS `left` on the arrow's ::after resolves against the padding
  // box, not the border box. Omitting the borderWidth term (the pre-#2370
  // GIF implementation) lands the caret 1px off from the anchor's centre.
  const inset = arrowInset(cornerRadius, borderWidth);
  const paddingBoxWidth = measuredWidth - 2 * borderWidth;
  const rawArrowX = anchorCenterX - left - borderWidth;
  const arrowX = Math.max(inset, Math.min(rawArrowX, Math.max(inset, paddingBoxWidth - inset)));

  const anchorOutsideSpan = anchorCenterX < left || anchorCenterX > left + measuredWidth;
  const showArrow = !topWasClamped && !anchorOutsideSpan;

  return { left, top, arrowX, showArrow };
}
