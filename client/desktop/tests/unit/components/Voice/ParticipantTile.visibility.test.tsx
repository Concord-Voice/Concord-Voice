import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, waitFor, cleanup } from '@testing-library/react';
import type { VoiceParticipant } from '../../../../src/renderer/stores/voiceStore';

const setRemoteVideoVisibility = vi.fn();
const removeRemoteVideoTile = vi.fn();
vi.mock('../../../../src/renderer/services/voiceService', () => ({
  voiceService: { setRemoteVideoVisibility, removeRemoteVideoTile },
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
    setRemoteVideoVisibility.mockClear();
    removeRemoteVideoTile.mockClear();
    ioCallback = null;
    (window as unknown as { IntersectionObserver: unknown }).IntersectionObserver = CaptureIO;
    // jsdom's HTMLMediaElement.play() returns undefined; the tile calls .play().catch(...).
    vi.spyOn(HTMLVideoElement.prototype, 'play').mockResolvedValue(undefined);
  });

  it('reports per-tile visibility (with a stable tileKey) on intersect changes', async () => {
    render(<ParticipantTile participant={makeParticipant()} />);
    await waitFor(() => expect(ioCallback).not.toBeNull());

    ioCallback!([{ isIntersecting: false }]);
    await waitFor(() =>
      expect(setRemoteVideoVisibility).toHaveBeenCalledWith('user-A', false, expect.any(String))
    );

    ioCallback!([{ isIntersecting: true }]);
    await waitFor(() =>
      expect(setRemoteVideoVisibility).toHaveBeenCalledWith('user-A', true, expect.any(String))
    );

    // The same tileKey is used across reports from one instance.
    const keys = setRemoteVideoVisibility.mock.calls.map((c) => c[2]);
    expect(new Set(keys).size).toBe(1);
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
    await waitFor(() => expect(setRemoteVideoVisibility).toHaveBeenCalled());
    const tileKey = setRemoteVideoVisibility.mock.calls[0][2];
    setRemoteVideoVisibility.mockClear();

    unmount();
    await waitFor(() => expect(removeRemoteVideoTile).toHaveBeenCalledWith('user-A', tileKey));
    // unmount must NOT report the tile as hidden (that would freeze other surfaces)
    expect(setRemoteVideoVisibility).not.toHaveBeenCalledWith('user-A', false, expect.any(String));
    cleanup();
  });
});
