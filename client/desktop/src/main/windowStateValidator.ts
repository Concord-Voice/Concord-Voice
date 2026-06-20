export interface SavedState {
  x: number;
  y: number;
  width: number;
  height: number;
  isMaximized: boolean;
}

export interface DisplayInfo {
  workArea: { x: number; y: number; width: number; height: number };
}

const MIN_WIDTH = 800;
const MIN_HEIGHT = 600;
const VISIBILITY_THRESHOLD = 50;

function isFiniteNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value);
}

function passesNanCheck(saved: unknown): saved is SavedState {
  if (saved === null || typeof saved !== 'object') return false;
  const s = saved as Record<string, unknown>;
  return (
    isFiniteNumber(s.x) &&
    isFiniteNumber(s.y) &&
    isFiniteNumber(s.width) &&
    isFiniteNumber(s.height) &&
    typeof s.isMaximized === 'boolean'
  );
}

function intersectsAnyDisplay(saved: SavedState, displays: DisplayInfo[]): boolean {
  return displays.some((d) => {
    const overlapW = Math.max(
      0,
      Math.min(saved.x + saved.width, d.workArea.x + d.workArea.width) -
        Math.max(saved.x, d.workArea.x)
    );
    const overlapH = Math.max(
      0,
      Math.min(saved.y + saved.height, d.workArea.y + d.workArea.height) -
        Math.max(saved.y, d.workArea.y)
    );
    return overlapW >= VISIBILITY_THRESHOLD && overlapH >= VISIBILITY_THRESHOLD;
  });
}

function passesSizeSanityCheck(saved: SavedState, displays: DisplayInfo[]): boolean {
  if (saved.width < MIN_WIDTH || saved.height < MIN_HEIGHT) return false;
  const totalW = displays.reduce((sum, d) => sum + d.workArea.width, 0);
  const totalH = displays.reduce((sum, d) => sum + d.workArea.height, 0);
  return saved.width <= totalW && saved.height <= totalH;
}

function passesNegativeCoordsCheck(saved: SavedState): boolean {
  return (
    saved.x >= -(saved.width - VISIBILITY_THRESHOLD) &&
    saved.y >= -(saved.height - VISIBILITY_THRESHOLD)
  );
}

export function validateBounds(saved: unknown, displays: DisplayInfo[]): SavedState | null {
  if (!passesNanCheck(saved)) return null;
  if (!intersectsAnyDisplay(saved, displays)) return null;
  if (!passesSizeSanityCheck(saved, displays)) return null;
  if (!passesNegativeCoordsCheck(saved)) return null;
  return saved;
}
