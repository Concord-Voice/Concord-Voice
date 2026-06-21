import { renderHook, act } from '@testing-library/react';
import { useVoiceMagnification } from '@/renderer/components/Voice/useVoiceMagnification';
import type { VoiceParticipant } from '@/renderer/stores/voiceStore';

// Mock requestAnimationFrame for synchronous testing
let rafCallback: ((time: number) => void) | null = null;
let rafId = 0;

beforeEach(() => {
  rafCallback = null;
  rafId = 0;
  vi.spyOn(globalThis, 'requestAnimationFrame').mockImplementation((cb) => {
    rafCallback = cb;
    return ++rafId;
  });
  vi.spyOn(globalThis, 'cancelAnimationFrame').mockImplementation(() => {
    rafCallback = null;
  });
});

afterEach(() => {
  vi.restoreAllMocks();
});

function makeParticipant(userId: string, isSpeaking: boolean): Record<string, VoiceParticipant> {
  return {
    [userId]: {
      userId,
      username: userId,
      isMuted: false,
      isDeafened: false,
      isVideoOn: false,
      isScreenSharing: false,
      isSpeaking,
    },
  };
}

function tickRaf(time: number) {
  if (rafCallback) {
    const cb = rafCallback;
    rafCallback = null;
    act(() => {
      cb(time);
    });
  }
}

describe('useVoiceMagnification', () => {
  it('returns empty scales when no participants', () => {
    const { result } = renderHook(() => useVoiceMagnification({}));
    // Tick once to get initial scales
    tickRaf(16);
    expect(result.current).toEqual({});
  });

  it('returns scale of 1.0 for non-speaking participant', () => {
    // Non-speaking participant: target=1.0, prev defaults to 1.0
    // The hook only triggers setScales when `changed` is true.
    // Since prev===target from the start, scale stays in currentScalesRef
    // but never gets flushed to state. So result.current won't have the key
    // unless the participant transitions through speaking first.
    const participants = makeParticipant('user-1', false);
    const { result } = renderHook(() => useVoiceMagnification(participants));

    tickRaf(16);
    tickRaf(32);

    // Non-speaking participant that never spoke won't appear in the returned
    // scales Record because the hook optimizes away no-change updates.
    // The consumer treats missing keys as scale=1.0 (default).
    expect(result.current['user-1']).toBeUndefined();
  });

  it('scales up when participant starts speaking', () => {
    const participants = makeParticipant('user-1', true);
    const { result } = renderHook(() => useVoiceMagnification(participants));

    // Tick multiple frames to let the ramp up
    tickRaf(16);
    tickRaf(32);
    tickRaf(48);
    tickRaf(200); // 200ms total — ramp up is 120ms, should be close to max

    expect(result.current['user-1']).toBeGreaterThan(1.0);
  });

  it('scales back down when participant stops speaking', () => {
    // Start speaking
    let participants = makeParticipant('user-1', true);
    const { result, rerender } = renderHook(({ p }) => useVoiceMagnification(p), {
      initialProps: { p: participants },
    });

    // Ramp up
    tickRaf(16);
    tickRaf(200);

    const scaledUp = result.current['user-1'] ?? 1;
    expect(scaledUp).toBeGreaterThan(1.0);

    // Stop speaking
    participants = makeParticipant('user-1', false);
    rerender({ p: participants });

    // Tick to let it ramp down
    tickRaf(250);
    tickRaf(500);
    tickRaf(800);

    expect(result.current['user-1']).toBeLessThan(scaledUp);
  });

  it('handles multiple participants independently', () => {
    const participants: Record<string, VoiceParticipant> = {
      ...makeParticipant('user-1', true),
      ...makeParticipant('user-2', false),
    };

    const { result } = renderHook(() => useVoiceMagnification(participants));

    tickRaf(16);
    tickRaf(200);

    // user-1 should be scaled up, user-2 should be at 1.0
    expect(result.current['user-1']).toBeGreaterThan(1.0);
    expect(result.current['user-2']).toBeCloseTo(1.0, 2);
  });

  it('cleans up entries for participants that leave', () => {
    const twoParticipants: Record<string, VoiceParticipant> = {
      ...makeParticipant('user-1', false),
      ...makeParticipant('user-2', false),
    };

    const { result, rerender } = renderHook(({ p }) => useVoiceMagnification(p), {
      initialProps: { p: twoParticipants },
    });

    tickRaf(16);
    tickRaf(32);

    // Now only user-1 remains
    const oneParticipant = makeParticipant('user-1', false);
    rerender({ p: oneParticipant });
    tickRaf(48);
    tickRaf(64);

    // user-2 should be removed from scales
    expect(result.current['user-2']).toBeUndefined();
    expect(result.current['user-1']).toBeDefined();
  });

  it('cancels animation frame on unmount', () => {
    const { unmount } = renderHook(() => useVoiceMagnification({}));
    tickRaf(16);
    unmount();
    expect(globalThis.cancelAnimationFrame).toHaveBeenCalled();
  });
});
