import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';

vi.mock('@/renderer/stores/voiceStore', () => ({
  useVoiceStore: Object.assign(vi.fn(), {
    getState: vi.fn(() => ({ audioOutputDeviceId: 'speaker-1' })),
  }),
}));

import { useOutputTest } from '@/renderer/hooks/useOutputTest';
import { useVoiceStore } from '@/renderer/stores/voiceStore';

let mockSetSinkId: ReturnType<typeof vi.fn>;
let mockPlay: ReturnType<typeof vi.fn>;

function createMockNode(extras: Record<string, unknown> = {}) {
  const node: Record<string, unknown> = { ...extras };
  node.connect = vi.fn(() => node);
  return node;
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.useFakeTimers();

  const mockCtx = {
    state: 'running',
    currentTime: 0,
    close: vi.fn().mockResolvedValue(undefined),
    resume: vi.fn().mockResolvedValue(undefined),
    createMediaStreamDestination: vi.fn(() => ({
      stream: { getTracks: () => [] },
    })),
    createOscillator: vi.fn(() =>
      createMockNode({
        type: 'sine',
        frequency: { value: 0 },
        start: vi.fn(),
        stop: vi.fn(),
      })
    ),
    createGain: vi.fn(() =>
      createMockNode({
        gain: {
          setValueAtTime: vi.fn(),
          linearRampToValueAtTime: vi.fn(),
        },
      })
    ),
  };
  (globalThis as any).AudioContext = function MockAudioContext() {
    return mockCtx;
  };

  mockSetSinkId = vi.fn().mockResolvedValue(undefined);
  mockPlay = vi.fn().mockResolvedValue(undefined);
  (globalThis as any).Audio = function MockAudio() {
    this.srcObject = null;
    this.setSinkId = mockSetSinkId;
    this.play = mockPlay;
    this.pause = vi.fn();
  };
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('useOutputTest', () => {
  it('returns correct initial state', () => {
    const { result } = renderHook(() => useOutputTest());
    expect(result.current.isTesting).toBe(false);
    expect(result.current.error).toBeNull();
    expect(typeof result.current.playTestTone).toBe('function');
  });

  it('sets isTesting true while tone is playing', async () => {
    const { result } = renderHook(() => useOutputTest());
    await act(async () => {
      await result.current.playTestTone();
    });
    expect(result.current.isTesting).toBe(true);
  });

  it('routes audio to the selected output device via setSinkId', async () => {
    const { result } = renderHook(() => useOutputTest());
    await act(async () => {
      await result.current.playTestTone();
    });
    expect(mockSetSinkId).toHaveBeenCalledWith('speaker-1');
  });

  it('plays audio', async () => {
    const { result } = renderHook(() => useOutputTest());
    await act(async () => {
      await result.current.playTestTone();
    });
    expect(mockPlay).toHaveBeenCalled();
  });

  it('falls back gracefully when setSinkId rejects', async () => {
    mockSetSinkId.mockRejectedValueOnce(new Error('unavailable'));
    const { result } = renderHook(() => useOutputTest());
    await act(async () => {
      await result.current.playTestTone();
    });
    expect(result.current.error).toBeNull();
    expect(result.current.isTesting).toBe(true);
  });

  it('stops playing after timer elapses', async () => {
    const { result } = renderHook(() => useOutputTest());
    await act(async () => {
      await result.current.playTestTone();
    });
    await act(async () => {
      vi.advanceTimersByTime(700);
    });
    expect(result.current.isTesting).toBe(false);
  });

  it('sets error when AudioContext construction throws', async () => {
    (globalThis as any).AudioContext = function Throwing() {
      throw new Error('No audio context');
    };
    const { result } = renderHook(() => useOutputTest());
    await act(async () => {
      await result.current.playTestTone();
    });
    expect(result.current.isTesting).toBe(false);
    expect(result.current.error).toBe('No audio context');
  });

  it('skips setSinkId when no output device selected', async () => {
    (useVoiceStore as any).getState.mockReturnValueOnce({ audioOutputDeviceId: null });
    const { result } = renderHook(() => useOutputTest());
    await act(async () => {
      await result.current.playTestTone();
    });
    expect(mockSetSinkId).not.toHaveBeenCalled();
  });

  it('cleans up on unmount', async () => {
    const { result, unmount } = renderHook(() => useOutputTest());
    await act(async () => {
      await result.current.playTestTone();
    });
    expect(result.current.isTesting).toBe(true);
    expect(() => unmount()).not.toThrow();
  });
});
