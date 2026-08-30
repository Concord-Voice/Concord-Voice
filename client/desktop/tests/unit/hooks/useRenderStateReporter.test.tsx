import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { renderHook, waitFor, cleanup } from '@testing-library/react';

const setRemoteVideoRenderState = vi.fn();
const removeRemoteVideoTile = vi.fn();
vi.mock('../../../src/renderer/services/voice/voiceService', () => ({
  voiceService: { setRemoteVideoRenderState, removeRemoteVideoTile },
}));

// Capture the IntersectionObserver callback so the test can drive intersection.
let ioCallback: ((entries: Array<{ isIntersecting: boolean }>) => void) | null = null;
class CaptureIO {
  constructor(cb: (e: Array<{ isIntersecting: boolean }>) => void) {
    ioCallback = cb;
  }
  observe = vi.fn();
  unobserve = vi.fn();
  disconnect = vi.fn();
}

// Capture the ResizeObserver callback so the test can drive size-only changes.
let roCallback: (() => void) | null = null;
const roDisconnect = vi.fn();
class CaptureRO {
  constructor(cb: () => void) {
    roCallback = cb;
  }
  observe = vi.fn();
  unobserve = vi.fn();
  disconnect = roDisconnect;
}

import { useRenderStateReporter } from '../../../src/renderer/hooks/voice/useRenderStateReporter';

function setVisibility(state: 'visible' | 'hidden') {
  Object.defineProperty(document, 'visibilityState', { value: state, configurable: true });
  document.dispatchEvent(new Event('visibilitychange'));
}

async function mountReporter(source: 'camera' | 'screen') {
  const el = document.createElement('div');
  const elementRef = { current: el as HTMLElement | null };
  renderHook(() =>
    useRenderStateReporter({
      userId: 'u',
      tileId: 't',
      source,
      elementRef,
      role: 'focus',
      enabled: true,
    })
  );
  // Drive intersection until the lazy voiceService import resolves and a report lands.
  await waitFor(() => {
    ioCallback?.([{ isIntersecting: true }]);
    expect(setRemoteVideoRenderState).toHaveBeenCalled();
  });
}

describe('useRenderStateReporter (#1924 window-hide demand)', () => {
  beforeEach(() => {
    setRemoteVideoRenderState.mockClear();
    removeRemoteVideoTile.mockClear();
    ioCallback = null;
    roCallback = null;
    roDisconnect.mockClear();
    (window as unknown as { IntersectionObserver: unknown }).IntersectionObserver = CaptureIO;
    (window as unknown as { ResizeObserver: unknown }).ResizeObserver = CaptureRO;
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({
      x: 0,
      y: 0,
      width: 640,
      height: 360,
      top: 0,
      left: 0,
      right: 640,
      bottom: 360,
      toJSON: () => ({}),
    } as DOMRect);
    Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true });
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    setVisibility('visible');
  });

  it('screen tile reports visible:false when the window is hidden (no stale gate demand)', async () => {
    await mountReporter('screen');
    expect(setRemoteVideoRenderState).toHaveBeenLastCalledWith(
      'u',
      't',
      expect.objectContaining({ visible: true, focusedWindow: true }),
      'screen'
    );

    setRemoteVideoRenderState.mockClear();
    setVisibility('hidden');

    expect(setRemoteVideoRenderState).toHaveBeenLastCalledWith(
      'u',
      't',
      expect.objectContaining({ visible: false, focusedWindow: false }),
      'screen'
    );
  });

  it('camera tile stays visible:true when intersecting even if the window hides (unchanged)', async () => {
    await mountReporter('camera');
    setRemoteVideoRenderState.mockClear();
    setVisibility('hidden');

    expect(setRemoteVideoRenderState).toHaveBeenLastCalledWith(
      'u',
      't',
      expect.objectContaining({ visible: true, focusedWindow: false }),
      'camera'
    );
  });

  it('screen tile restores visible:true when the window is shown again', async () => {
    await mountReporter('screen');
    setVisibility('hidden');
    setRemoteVideoRenderState.mockClear();

    setVisibility('visible');

    expect(setRemoteVideoRenderState).toHaveBeenLastCalledWith(
      'u',
      't',
      expect.objectContaining({ visible: true, focusedWindow: true }),
      'screen'
    );
  });

  const mockRect = (width: number, height: number) =>
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({
      x: 0,
      y: 0,
      width,
      height,
      top: 0,
      left: 0,
      right: width,
      bottom: height,
      toJSON: () => ({}),
    } as DOMRect);

  it('re-reports demand on a ResizeObserver size-only change (splitter drag / window resize) (#1924 review)', async () => {
    await mountReporter('screen');
    expect(roCallback).toBeTypeOf('function');
    setRemoteVideoRenderState.mockClear();

    // Same visibility/intersection, new rendered box — only the ResizeObserver fires.
    mockRect(320, 180);
    roCallback?.();

    expect(setRemoteVideoRenderState).toHaveBeenLastCalledWith(
      'u',
      't',
      expect.objectContaining({ cssWidth: 320, cssHeight: 180 }),
      'screen'
    );
  });

  it('skips a ResizeObserver fire for a not-yet-laid-out (<1px) box', async () => {
    await mountReporter('screen');
    setRemoteVideoRenderState.mockClear();

    mockRect(0, 0);
    roCallback?.();

    expect(setRemoteVideoRenderState).not.toHaveBeenCalled();
  });

  it('disconnects the ResizeObserver on unmount', async () => {
    const el = document.createElement('div');
    const elementRef = { current: el as HTMLElement | null };
    const { unmount } = renderHook(() =>
      useRenderStateReporter({
        userId: 'u',
        tileId: 't',
        source: 'screen',
        elementRef,
        role: 'focus',
        enabled: true,
      })
    );
    await waitFor(() => {
      ioCallback?.([{ isIntersecting: true }]);
      expect(setRemoteVideoRenderState).toHaveBeenCalled();
    });
    roDisconnect.mockClear();

    unmount();

    expect(roDisconnect).toHaveBeenCalled();
  });
});
