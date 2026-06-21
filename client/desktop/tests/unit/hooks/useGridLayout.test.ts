import { renderHook, act } from '@testing-library/react';
import { computeGridLayout, useGridLayout } from '@/renderer/hooks/useGridLayout';

// ── ResizeObserver mock ────────────────────────────────────────────────────────

let resizeCallback: ResizeObserverCallback | null = null;
const mockObserve = vi.fn();
const mockDisconnect = vi.fn();

beforeEach(() => {
  resizeCallback = null;
  vi.clearAllMocks();
  // setup.ts defines ResizeObserver with writable: true — assign directly.
  // Must use a regular function (not arrow) so it works with `new`.
  (window as Record<string, unknown>).ResizeObserver = vi.fn(function MockResizeObserver(
    cb: ResizeObserverCallback
  ) {
    resizeCallback = cb;
    return { observe: mockObserve, unobserve: vi.fn(), disconnect: mockDisconnect };
  });
});

function triggerResize(width: number, height: number) {
  if (!resizeCallback) throw new Error('ResizeObserver callback not captured');
  act(() => {
    resizeCallback!(
      [
        {
          contentRect: {
            width,
            height,
            top: 0,
            left: 0,
            bottom: height,
            right: width,
            x: 0,
            y: 0,
            toJSON: () => ({}),
          },
          target: document.createElement('div'),
          borderBoxSize: [],
          contentBoxSize: [],
          devicePixelContentBoxSize: [],
        },
      ] as unknown as ResizeObserverEntry[],
      {} as ResizeObserver
    );
  });
}

// ── computeGridLayout (pure function) ──────────────────────────────────────────

describe('computeGridLayout', () => {
  const AR = 16 / 9;

  it('returns zero layout for count = 0', () => {
    const result = computeGridLayout(1000, 600, 0);
    expect(result).toEqual({ tileWidth: 0, tileHeight: 0, columns: 0 });
  });

  it('returns safe defaults for zero-dimension container', () => {
    const result = computeGridLayout(0, 0, 4);
    expect(result.columns).toBe(1);
    expect(result.tileWidth).toBe(80);
    expect(result.tileHeight).toBeCloseTo(80 / AR);
  });

  it('returns safe defaults for negative dimensions', () => {
    const result = computeGridLayout(-100, -50, 2);
    expect(result.columns).toBe(1);
    expect(result.tileWidth).toBe(80);
  });

  it('places a single participant to fill most of the container', () => {
    const result = computeGridLayout(1000, 600, 1, { aspectRatio: AR, gap: 4, padding: 12 });
    expect(result.columns).toBe(1);
    expect(result.tileWidth).toBeGreaterThan(400);
    expect(result.tileHeight).toBeGreaterThan(200);
  });

  it('places 2 participants side by side in a landscape container', () => {
    const result = computeGridLayout(1000, 400, 2, { aspectRatio: AR, gap: 4, padding: 12 });
    expect(result.columns).toBe(2);
  });

  it('places 4 participants in a 2x2 grid', () => {
    const result = computeGridLayout(800, 600, 4, { aspectRatio: AR, gap: 4, padding: 12 });
    expect(result.columns).toBe(2);
    expect(result.tileWidth).toBeGreaterThan(0);
    expect(result.tileHeight).toBeGreaterThan(0);
  });

  it('handles 5 participants (odd count)', () => {
    const result = computeGridLayout(1000, 600, 5, { aspectRatio: AR, gap: 4, padding: 12 });
    expect(result.columns).toBeGreaterThanOrEqual(2);
    expect(result.columns).toBeLessThanOrEqual(5);
  });

  it('handles 25 participants — tiles stay above minTileWidth', () => {
    const result = computeGridLayout(1200, 800, 25, {
      aspectRatio: AR,
      gap: 4,
      padding: 12,
      minTileWidth: 60,
    });
    expect(result.tileWidth).toBeGreaterThanOrEqual(60);
    expect(result.columns).toBeGreaterThan(1);
  });

  it('prefers fewer columns in a tall narrow container', () => {
    const tall = computeGridLayout(300, 900, 6, { aspectRatio: AR, gap: 4, padding: 12 });
    const wide = computeGridLayout(900, 300, 6, { aspectRatio: AR, gap: 4, padding: 12 });
    expect(tall.columns).toBeLessThanOrEqual(wide.columns);
  });

  it('prefers more columns in a wide short container', () => {
    const result = computeGridLayout(1600, 200, 4, { aspectRatio: AR, gap: 4, padding: 12 });
    expect(result.columns).toBeGreaterThanOrEqual(3);
  });

  it('respects custom aspect ratio (1:1)', () => {
    const result = computeGridLayout(800, 800, 4, { aspectRatio: 1, gap: 4, padding: 12 });
    expect(Math.abs(result.tileWidth - result.tileHeight)).toBeLessThan(1);
  });

  it('subtracts gap and padding correctly', () => {
    const noGap = computeGridLayout(800, 600, 4, { aspectRatio: AR, gap: 0, padding: 0 });
    const withGap = computeGridLayout(800, 600, 4, { aspectRatio: AR, gap: 20, padding: 40 });
    expect(noGap.tileWidth).toBeGreaterThan(withGap.tileWidth);
  });

  it('reduces columns when container is too small for minTileWidth', () => {
    const result = computeGridLayout(200, 600, 4, {
      aspectRatio: AR,
      gap: 4,
      padding: 12,
      minTileWidth: 80,
    });
    expect(result.columns).toBeLessThanOrEqual(2);
    expect(result.tileWidth).toBeGreaterThanOrEqual(80);
  });

  it('all tiles fit within the container (no overflow)', () => {
    const W = 1000;
    const H = 600;
    const gap = 4;
    const padding = 12;

    for (const count of [1, 2, 3, 4, 5, 8, 12, 25]) {
      const { tileWidth, tileHeight, columns } = computeGridLayout(W, H, count, {
        aspectRatio: AR,
        gap,
        padding,
      });
      const rows = Math.ceil(count / columns);
      const totalW = columns * tileWidth + (columns - 1) * gap + 2 * padding;
      const totalH = rows * tileHeight + (rows - 1) * gap + 2 * padding;
      expect(totalW).toBeLessThanOrEqual(W + 0.01);
      expect(totalH).toBeLessThanOrEqual(H + 0.01);
    }
  });

  it('uses padding=0 by default (contentRect already excludes CSS padding)', () => {
    const result = computeGridLayout(400, 300, 1, { aspectRatio: AR, gap: 4 });
    expect(result.tileWidth).toBeGreaterThan(390);
  });

  it('falls back to smaller-than-min tiles when container is too small', () => {
    const result = computeGridLayout(60, 200, 2, {
      aspectRatio: AR,
      gap: 4,
      padding: 0,
      minTileWidth: 80,
    });
    expect(result.columns).toBe(1);
    expect(result.tileWidth).toBeLessThanOrEqual(60);
    expect(result.tileWidth).toBeGreaterThan(0);
    expect(result.tileHeight).toBeGreaterThan(0);
  });

  it('fallback tiles do not overflow the container', () => {
    const W = 50;
    const H = 100;
    const result = computeGridLayout(W, H, 3, {
      aspectRatio: AR,
      gap: 4,
      padding: 0,
      minTileWidth: 80,
    });
    const rows = Math.ceil(3 / result.columns);
    const totalW = result.columns * result.tileWidth + (result.columns - 1) * 4;
    const totalH = rows * result.tileHeight + (rows - 1) * 4;
    expect(totalW).toBeLessThanOrEqual(W + 0.01);
    expect(totalH).toBeLessThanOrEqual(H + 0.01);
  });

  it('returns the column count that maximises tile area', () => {
    const W = 1000;
    const H = 600;
    const count = 6;
    const best = computeGridLayout(W, H, count, { aspectRatio: AR, gap: 4, padding: 12 });

    for (let cols = 1; cols <= count; cols++) {
      const rows = Math.ceil(count / cols);
      const availW = W - 24 - (cols - 1) * 4;
      const availH = H - 24 - (rows - 1) * 4;
      if (availW <= 0 || availH <= 0) continue;
      let tw = availW / cols;
      let th = tw / AR;
      if (th * rows > availH) {
        th = availH / rows;
        tw = th * AR;
      }
      if (tw < 80) continue;
      expect(best.tileWidth * best.tileHeight).toBeGreaterThanOrEqual(tw * th - 0.01);
    }
  });

  it('clamps tile width to maxTileWidth', () => {
    const result = computeGridLayout(1000, 1000, 1, {
      aspectRatio: 1,
      gap: 4,
      padding: 12,
      maxTileWidth: 320,
    });
    expect(result.tileWidth).toBeLessThanOrEqual(320);
    expect(result.tileHeight).toBeLessThanOrEqual(320);
  });
});

// ── useGridLayout (hook) ───────────────────────────────────────────────────────

function makeRef(el: HTMLElement | null = null) {
  return { current: el } as React.RefObject<HTMLElement | null>;
}

describe('useGridLayout', () => {
  it('returns fallback layout before ResizeObserver fires', () => {
    const div = document.createElement('div');
    const ref = makeRef(div);
    const { result } = renderHook(() => useGridLayout(ref, 4));
    expect(result.current.columns).toBe(1);
    expect(result.current.tileWidth).toBe(80);
  });

  it('updates layout when ResizeObserver fires', () => {
    const div = document.createElement('div');
    const ref = makeRef(div);
    const { result } = renderHook(() => useGridLayout(ref, 4));

    triggerResize(1000, 600);

    expect(result.current.tileWidth).toBeGreaterThan(80);
    expect(result.current.tileHeight).toBeGreaterThan(0);
    expect(result.current.columns).toBeGreaterThanOrEqual(1);
  });

  it('re-computes when count changes', () => {
    const div = document.createElement('div');
    const ref = makeRef(div);
    const { result, rerender } = renderHook(({ count }) => useGridLayout(ref, count), {
      initialProps: { count: 2 },
    });

    triggerResize(1000, 600);
    const tileW2 = result.current.tileWidth;

    rerender({ count: 8 });
    expect(result.current.tileWidth).toBeLessThan(tileW2);
  });

  it('observes the container element', () => {
    const div = document.createElement('div');
    const ref = makeRef(div);
    renderHook(() => useGridLayout(ref, 1));
    expect(mockObserve).toHaveBeenCalledWith(div);
  });

  it('disconnects ResizeObserver on unmount', () => {
    const div = document.createElement('div');
    const ref = makeRef(div);
    const { unmount } = renderHook(() => useGridLayout(ref, 1));
    unmount();
    expect(mockDisconnect).toHaveBeenCalled();
  });

  it('handles null ref gracefully', () => {
    const ref = makeRef(null);
    const { result } = renderHook(() => useGridLayout(ref, 4));
    expect(result.current.columns).toBe(1);
    expect(mockObserve).not.toHaveBeenCalled();
  });

  it('accepts custom options', () => {
    const div = document.createElement('div');
    const ref = makeRef(div);
    const { result } = renderHook(() =>
      useGridLayout(ref, 4, { aspectRatio: 1, gap: 8, padding: 16, minTileWidth: 100 })
    );

    triggerResize(800, 800);

    expect(Math.abs(result.current.tileWidth - result.current.tileHeight)).toBeLessThan(1);
    expect(result.current.tileWidth).toBeGreaterThanOrEqual(100);
  });
});
