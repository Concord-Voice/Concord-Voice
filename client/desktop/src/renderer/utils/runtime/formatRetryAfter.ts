/**
 * Format a retry-after value (in seconds) to a human-readable string.
 * - >= 1h: "Xh Ym" or "Xh" (if minutes are 0)
 * - < 1h: "Xm"
 * - < 1m: "Xs"
 */
export function formatRetryAfter(seconds: number): string {
  if (seconds < 60) {
    return `${seconds}s`;
  }
  if (seconds < 3600) {
    return `${Math.floor(seconds / 60)}m`;
  }
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  return m === 0 ? `${h}h` : `${h}h ${m}m`;
}
