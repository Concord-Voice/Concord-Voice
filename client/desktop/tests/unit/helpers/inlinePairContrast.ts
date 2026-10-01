import { contrastRatio, sampleFill } from '../styles/contrastPairs';

type Rgb = [number, number, number];

/** A computed `rgb(r, g, b)` as jsdom reports it, or null. */
function parseRgb(value: string): Rgb | null {
  const m = /^rgb\((\d+), (\d+), (\d+)\)$/.exec(value.trim());
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null;
}

/**
 * The worst contrast of an element's inline label colour over its inline fill,
 * across every point of that fill. Returns 0 when either is missing, because a
 * label that takes its colour from the stylesheet while its fill is set inline is
 * not paired with that fill at all.
 */
export function inlinePairWorst(el: HTMLElement): number {
  const text = parseRgb(el.style.color);
  const background = el.style.background || el.style.backgroundColor;
  const solid = parseRgb(background);
  const fill = solid ? [solid] : sampleFill(background || null);
  if (!text || !fill) return 0;
  return Math.min(...fill.map((bg) => contrastRatio(text, bg)));
}
