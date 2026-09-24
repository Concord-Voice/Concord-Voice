/**
 * Canonical fixed screen-share resolution presets (#2163). Excludes 'source'
 * (native — display-dependent) and custom `WxH`. Single source of truth for the
 * string→dims mapping previously duplicated across voiceService + VideoConfigSection.
 */
export const SCREEN_RES_DIMS: Record<string, { w: number; h: number }> = {
  '720p': { w: 1280, h: 720 },
  '1080p': { w: 1920, h: 1080 },
  '1440p': { w: 2560, h: 1440 },
  '4K': { w: 3840, h: 2160 },
};

/**
 * Resolve a screen-share resolution selection to pixel dimensions. Handles the
 * fixed presets, a literal `WxH` custom string, and 'source' (via the caller's
 * detected/actual source dims). Unknown → the provided source dims.
 */
export function resolveScreenDims(
  res: string,
  sourceDims: { w: number; h: number }
): { w: number; h: number } {
  if (res === 'source') return sourceDims;
  const custom = /^(\d+)x(\d+)$/.exec(res);
  if (custom) return { w: Number(custom[1]), h: Number(custom[2]) };
  return SCREEN_RES_DIMS[res] ?? sourceDims;
}

/**
 * The largest-area valid display in a getDisplayInfo report, or null when there is
 * none: an empty report, or one whose every display is malformed (a width or height
 * that is not > 0, which also rejects NaN). The picker, Settings and the capture path
 * (voiceService.resolveCaptureDims) all read 'source' through this and fall back to
 * 4K on null, so they cannot disagree about what counts as a real display.
 */
export function largestDisplayDims(
  displays: readonly { width: number; height: number }[]
): { w: number; h: number } | null {
  const valid = displays.filter((d) => d.width > 0 && d.height > 0);
  if (valid.length === 0) return null;
  const best = valid.reduce((b, d) => (d.width * d.height > b.width * b.height ? d : b), valid[0]);
  return { w: best.width, h: best.height };
}

/**
 * Highest fixed screen-share resolution whose height fits the ceiling (#2163).
 * Native/Infinity → '4K' (largest). Below 720p → '720p' (floor fallback).
 */
export function highestFreeScreenResolution(heightCeiling: number): string {
  let best = '720p';
  let bestH = -1;
  for (const [key, dims] of Object.entries(SCREEN_RES_DIMS)) {
    if (dims.h <= heightCeiling && dims.h > bestH) {
      bestH = dims.h;
      best = key;
    }
  }
  return best;
}
