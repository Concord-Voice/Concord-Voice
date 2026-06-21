import { useState, useEffect, useMemo } from 'react';

export interface GridLayout {
  tileWidth: number;
  tileHeight: number;
  columns: number;
}

interface GridLayoutOptions {
  /** Tile width-to-height ratio (default: 16/9). */
  aspectRatio?: number;
  /** Gap between tiles in px (default: 4). */
  gap?: number;
  /** Extra inset in px beyond CSS padding (default: 0 — contentRect already excludes CSS padding). */
  padding?: number;
  /** Minimum tile width in px (default: 80). */
  minTileWidth?: number;
  /** Maximum tile width in px (default: none). Clamps oversized tiles (e.g. single-user). */
  maxTileWidth?: number;
}

const DEFAULT_ASPECT_RATIO = 16 / 9;
const DEFAULT_GAP = 4;
const DEFAULT_PADDING = 0;
const DEFAULT_MIN_TILE_WIDTH = 80;

/** Clamp tile width to maxTileWidth and recompute height from aspect ratio. */
function clampTile(tileW: number, tileH: number, aspectRatio: number, maxTileWidth?: number) {
  if (maxTileWidth && tileW > maxTileWidth) {
    return { tileW: maxTileWidth, tileH: maxTileWidth / aspectRatio };
  }
  return { tileW, tileH };
}

/** Fallback layout when container is too small for minTileWidth. */
function computeFallbackLayout(
  containerWidth: number,
  containerHeight: number,
  count: number,
  aspectRatio: number,
  gap: number,
  padding: number,
  maxTileWidth?: number
): GridLayout {
  const rows = count; // single column
  const availW = Math.max(1, containerWidth - 2 * padding);
  const availH = Math.max(1, containerHeight - 2 * padding - (rows - 1) * gap);
  let tileW = availW;
  let tileH = tileW / aspectRatio;
  if (tileH * rows > availH) {
    tileH = availH / rows;
    tileW = tileH * aspectRatio;
  }
  const clamped = clampTile(tileW, tileH, aspectRatio, maxTileWidth);
  return {
    tileWidth: Math.max(0, clamped.tileW),
    tileHeight: Math.max(0, clamped.tileH),
    columns: 1,
  };
}

/**
 * Compute the optimal grid layout that maximises tile area while fitting all
 * `count` tiles (of a fixed aspect ratio) inside a container of known size.
 *
 * Iterates every possible column count (1 … count) and picks the one that
 * yields the largest tile area without overflow.
 */
export function computeGridLayout(
  containerWidth: number,
  containerHeight: number,
  count: number,
  options: GridLayoutOptions = {}
): GridLayout {
  const {
    aspectRatio = DEFAULT_ASPECT_RATIO,
    gap = DEFAULT_GAP,
    padding = DEFAULT_PADDING,
    minTileWidth = DEFAULT_MIN_TILE_WIDTH,
    maxTileWidth,
  } = options;

  if (count <= 0) return { tileWidth: 0, tileHeight: 0, columns: 0 };

  if (containerWidth <= 0 || containerHeight <= 0) {
    const clamped = clampTile(minTileWidth, minTileWidth / aspectRatio, aspectRatio, maxTileWidth);
    return { tileWidth: clamped.tileW, tileHeight: clamped.tileH, columns: 1 };
  }

  let bestLayout: GridLayout = {
    tileWidth: minTileWidth,
    tileHeight: minTileWidth / aspectRatio,
    columns: 1,
  };
  let bestArea = 0;

  for (let cols = 1; cols <= count; cols++) {
    const rows = Math.ceil(count / cols);

    const availW = containerWidth - 2 * padding - (cols - 1) * gap;
    const availH = containerHeight - 2 * padding - (rows - 1) * gap;

    if (availW <= 0 || availH <= 0) continue;

    let tileW = availW / cols;
    let tileH = tileW / aspectRatio;

    // If tiles overflow vertically, constrain by height instead
    if (tileH * rows > availH) {
      tileH = availH / rows;
      tileW = tileH * aspectRatio;
    }

    // Clamp to maxTileWidth
    ({ tileW, tileH } = clampTile(tileW, tileH, aspectRatio, maxTileWidth));

    if (tileW < minTileWidth) continue;

    const area = tileW * tileH;
    if (area > bestArea) {
      bestArea = area;
      bestLayout = { tileWidth: tileW, tileHeight: tileH, columns: cols };
    }
  }

  // Fallback: container too small for minTileWidth — allow smaller tiles to avoid overflow
  if (bestArea === 0) {
    return computeFallbackLayout(
      containerWidth,
      containerHeight,
      count,
      aspectRatio,
      gap,
      padding,
      maxTileWidth
    );
  }

  return bestLayout;
}

/**
 * React hook that observes a container element's dimensions and computes the
 * optimal grid tile size for `count` participants.
 */
export function useGridLayout(
  containerRef: React.RefObject<HTMLElement | null>,
  count: number,
  options: GridLayoutOptions = {}
): GridLayout {
  const {
    aspectRatio = DEFAULT_ASPECT_RATIO,
    gap = DEFAULT_GAP,
    padding = DEFAULT_PADDING,
    minTileWidth = DEFAULT_MIN_TILE_WIDTH,
    maxTileWidth,
  } = options;

  const [dimensions, setDimensions] = useState<{ width: number; height: number }>({
    width: 0,
    height: 0,
  });

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;

    const observer = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) {
        setDimensions({
          width: entry.contentRect.width,
          height: entry.contentRect.height,
        });
      }
    });

    observer.observe(el);
    return () => observer.disconnect();
  }, [containerRef]);

  return useMemo(
    () =>
      computeGridLayout(dimensions.width, dimensions.height, count, {
        aspectRatio,
        gap,
        padding,
        minTileWidth,
        maxTileWidth,
      }),
    [
      dimensions.width,
      dimensions.height,
      count,
      aspectRatio,
      gap,
      padding,
      minTileWidth,
      maxTileWidth,
    ]
  );
}
