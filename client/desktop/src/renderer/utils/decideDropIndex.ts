/**
 * Pure insertion-index decision for the role reorder band (#2359). No DOM, no
 * store, no React — the caller measures, this decides.
 *
 * Extracted from the component on purpose: jsdom returns zero-height rects and
 * Vitest runs with `css: false`, so any in-jsdom geometry assertion is vacuous.
 * That is why #2839's drop logic was executed by no test at all. Feed this
 * synthetic rects instead, and leave the rendered result to Playwright.
 */

/**
 * Decide where a pointer at `pointerY` would insert into a flat, vertically
 * stacked band, returning an insertion index in `0..rects.length` (the index
 * BEFORE which the dragged row lands; `rects.length` means "after the last
 * row").
 *
 * Strict 50/50 midpoint per rect: above a rect's vertical centre inserts before
 * it, at or below inserts after it. **No dead band and no hysteresis** — this
 * DELIBERATELY DIVERGES from `ChannelList.tsx`'s
 * `deadZone = rect.height * 0.25` tri-state, and the two must NOT be
 * harmonized. That dead zone exists because the channel list has an
 * into-category drop target competing with the between-row targets, so the
 * middle of a row is a third, distinct outcome. The role band is flat — every
 * position is "between" — so the tri-state buys nothing, and its
 * keep-last-position early-return (`if (lastInsertRef.current?.targetId ===
 * targetId) return;`) is the single most likely way to strand the insertion
 * indicator on a stale row.
 *
 * `gapPx` is the inter-row gutter. It bounds the clamp that makes the decision
 * TOTAL: a pointer in a gutter, or above the first row, or below the last one,
 * still resolves to a definite index rather than to "no target". There is no
 * "nowhere" outcome to leave an indicator behind.
 *
 * `rects` must be in visual top-to-bottom order — the band's own render order.
 * An empty band inserts at 0.
 */
export function decideDropIndex(
  pointerY: number,
  rects: readonly DOMRect[],
  gapPx: number
): number {
  if (rects.length === 0) {
    return 0;
  }

  const gap = Number.isFinite(gapPx) && gapPx > 0 ? gapPx : 0;
  const first = rects[0];
  const last = rects[rects.length - 1];
  const y = Math.min(Math.max(pointerY, first.top - gap), last.bottom + gap);

  for (let i = 0; i < rects.length; i += 1) {
    const rect = rects[i];
    if (y < (rect.top + rect.bottom) / 2) {
      return i;
    }
  }

  return rects.length;
}
