import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, waitFor, cleanup } from '@testing-library/react';
import type { VoiceParticipant } from '../../../../src/renderer/stores/voiceStore';

const setRemoteVideoRenderState = vi.fn();
const removeRemoteVideoTile = vi.fn();
vi.mock('../../../../src/renderer/services/voiceService', () => ({
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

import ParticipantTile from '../../../../src/renderer/components/Voice/ParticipantTile';

function makeParticipant(over: Partial<VoiceParticipant> = {}): VoiceParticipant {
  return {
    userId: 'user-A',
    username: 'alice',
    isVideoOn: true,
    videoStream: { id: 'stream-A' } as unknown as MediaStream,
    ...over,
  } as VoiceParticipant;
}

describe('ParticipantTile visibility-pause (#1541)', () => {
  beforeEach(() => {
    setRemoteVideoRenderState.mockClear();
    removeRemoteVideoTile.mockClear();
    ioCallback = null;
    (window as unknown as { IntersectionObserver: unknown }).IntersectionObserver = CaptureIO;
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({
      x: 0,
      y: 0,
      width: 640,
      height: 360,
      top: 0,
      right: 640,
      bottom: 360,
      left: 0,
      toJSON: () => ({}),
    });
    // jsdom's HTMLMediaElement.play() returns undefined; the tile calls .play().catch(...).
    vi.spyOn(HTMLVideoElement.prototype, 'play').mockResolvedValue(undefined);
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it('reports per-tile visibility (with a stable tileKey) on intersect changes', async () => {
    render(<ParticipantTile participant={makeParticipant()} />);
    await waitFor(() => expect(ioCallback).not.toBeNull());

    ioCallback!([{ isIntersecting: false }]);
    await waitFor(() =>
      expect(setRemoteVideoRenderState).toHaveBeenCalledWith(
        'user-A',
        expect.any(String),
        expect.objectContaining({ visible: false })
      )
    );

    ioCallback!([{ isIntersecting: true }]);
    await waitFor(() =>
      expect(setRemoteVideoRenderState).toHaveBeenCalledWith(
        'user-A',
        expect.any(String),
        expect.objectContaining({
          visible: true,
          cssWidth: 640,
          cssHeight: 360,
          role: 'grid',
          focusedWindow: true,
        })
      )
    );

    // The same tileKey is used across reports from one instance.
    const keys = setRemoteVideoRenderState.mock.calls.map((c) => c[1]);
    expect(new Set(keys).size).toBe(1);
  });

  it('reports compact remote camera tiles as thumbnails', async () => {
    render(<ParticipantTile participant={makeParticipant()} compact />);
    await waitFor(() => expect(ioCallback).not.toBeNull());

    ioCallback!([{ isIntersecting: true }]);
    await waitFor(() =>
      expect(setRemoteVideoRenderState).toHaveBeenCalledWith(
        'user-A',
        expect.any(String),
        expect.objectContaining({
          visible: true,
          role: 'thumbnail',
        })
      )
    );
  });

  it('does not observe the local participant', () => {
    render(<ParticipantTile participant={makeParticipant()} isLocal />);
    expect(ioCallback).toBeNull();
  });

  it('deregisters its tile on unmount (does NOT report hidden)', async () => {
    const { unmount } = render(<ParticipantTile participant={makeParticipant()} />);
    await waitFor(() => expect(ioCallback).not.toBeNull());
    // capture the tileKey this instance uses
    ioCallback!([{ isIntersecting: true }]);
    await waitFor(() => expect(setRemoteVideoRenderState).toHaveBeenCalled());
    const tileKey = setRemoteVideoRenderState.mock.calls[0][1];
    setRemoteVideoRenderState.mockClear();

    unmount();
    await waitFor(() => expect(removeRemoteVideoTile).toHaveBeenCalledWith('user-A', tileKey));
    ioCallback!([{ isIntersecting: false }]);
    // unmount must NOT report again, including for queued observer callbacks.
    expect(setRemoteVideoRenderState).not.toHaveBeenCalled();
  });
});
