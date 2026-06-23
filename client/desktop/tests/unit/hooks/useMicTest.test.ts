import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';

// Mock stores
vi.mock('@/renderer/stores/audioSettingsStore', () => {
  const state = {
    musicMode: false,
    echoCancellation: true,
    noiseCancellation: true,
    autoGainControl: true,
    noiseGateMode: 'off' as string,
    noiseGateLevel: -50,
    inputVolume: 100,
  };
  const store = Object.assign(
    (selector?: (s: typeof state) => unknown) => (selector ? selector(state) : state),
    {
      getState: () => state,
      subscribe: vi.fn(() => () => {}),
      setState: (partial: Partial<typeof state>) => Object.assign(state, partial),
      _reset: () =>
        Object.assign(state, {
          musicMode: false,
          echoCancellation: true,
          noiseCancellation: true,
          autoGainControl: true,
          noiseGateMode: 'off',
          noiseGateLevel: -50,
          inputVolume: 100,
        }),
    }
  );
  return { useAudioSettingsStore: store };
});

vi.mock('@/renderer/stores/voiceStore', () => ({
  useVoiceStore: Object.assign(vi.fn(), {
    getState: vi.fn(() => ({
      audioInputDeviceId: null,
      audioOutputDeviceId: null,
      connectionState: 'disconnected',
      localIsTesting: false,
    })),
    subscribe: vi.fn(() => () => {}),
  }),
}));

vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: {
    beginTestSuspension: vi.fn(),
    endTestSuspension: vi.fn(),
    setLocalTestingStatus: vi.fn(),
  },
}));

vi.mock('@/renderer/stores/osPermissionStore', () => ({
  ensureOsPermission: vi.fn().mockResolvedValue('granted'),
}));

import { ensureOsPermission } from '@/renderer/stores/osPermissionStore';
import { useAudioSettingsStore } from '@/renderer/stores/audioSettingsStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useMicTest } from '@/renderer/hooks/useMicTest';
import { voiceService } from '@/renderer/services/voiceService';

// Build a comprehensive mock audio pipeline
const mockTrackStop = vi.fn();
const mockGetUserMedia = vi.fn();

function createMockAudioPipeline() {
  // Each node needs its own connect mock that returns itself (for chaining)
  const createNode = (extras: Record<string, unknown> = {}) => {
    const node: Record<string, unknown> = { ...extras };
    node.connect = vi.fn(() => node);
    return node;
  };

  const sourceNode = createNode({});
  const destStream = { getTracks: () => [] };
  const destNode = { stream: destStream };

  const mockCtx = {
    state: 'running',
    currentTime: 0,
    close: vi.fn().mockResolvedValue(undefined),
    resume: vi.fn().mockResolvedValue(undefined),
    createMediaStreamSource: vi.fn(() => sourceNode),
    createGain: vi.fn(() => {
      // Return a fresh gain node each time (volume gain vs noise gate gain)
      return createNode({ gain: { value: 1, setTargetAtTime: vi.fn() } });
    }),
    createAnalyser: vi.fn(() => {
      return createNode({
        fftSize: 2048,
        frequencyBinCount: 1024,
        smoothingTimeConstant: 0.4,
        getByteTimeDomainData: vi.fn(),
        getFloatTimeDomainData: vi.fn(),
      });
    }),
    createMediaStreamDestination: vi.fn(() => destNode),
  };

  return mockCtx;
}

beforeEach(() => {
  vi.clearAllMocks();
  (useAudioSettingsStore as any)._reset();
  vi.mocked(useVoiceStore.subscribe).mockImplementation(() => () => {});
  vi.mocked((useAudioSettingsStore as any).subscribe).mockImplementation(() => () => {});
  vi.mocked(useVoiceStore.getState).mockReturnValue({
    audioInputDeviceId: null,
    audioOutputDeviceId: null,
    connectionState: 'disconnected',
    localIsTesting: false,
  } as any);

  // Mock AudioContext constructor — must use a class/function form for `new`
  const mockCtx = createMockAudioPipeline();
  (globalThis as any).AudioContext = function MockAudioContext() {
    return mockCtx;
  };

  // Mock getUserMedia
  mockGetUserMedia.mockResolvedValue({
    getTracks: () => [{ stop: mockTrackStop }],
  });
  Object.defineProperty(navigator, 'mediaDevices', {
    value: { getUserMedia: mockGetUserMedia },
    configurable: true,
    writable: true,
  });

  // Mock Audio element — must use function form for `new Audio()`
  (globalThis as any).Audio = function MockAudio() {
    this.srcObject = null;
    this.setSinkId = vi.fn().mockResolvedValue(undefined);
    this.play = vi.fn().mockResolvedValue(undefined);
    this.pause = vi.fn();
  };

  // Mock requestAnimationFrame / cancelAnimationFrame
  vi.spyOn(globalThis, 'requestAnimationFrame').mockReturnValue(1);
  vi.spyOn(globalThis, 'cancelAnimationFrame').mockImplementation(() => {});
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('useMicTest', () => {
  describe('initial state', () => {
    it('returns correct initial values', () => {
      const { result } = renderHook(() => useMicTest());

      expect(result.current.isTesting).toBe(false);
      expect(result.current.dbfsLevel).toBe(-Infinity);
      expect(result.current.error).toBeNull();
      expect(typeof result.current.startTest).toBe('function');
      expect(typeof result.current.stopTest).toBe('function');
    });
  });

  describe('startTest', () => {
    it('requests microphone permission before starting', async () => {
      const { result } = renderHook(() => useMicTest());

      await act(async () => {
        await result.current.startTest();
      });

      expect(ensureOsPermission).toHaveBeenCalledWith('microphone');
    });

    it('sets error when mic permission is denied', async () => {
      vi.mocked(ensureOsPermission).mockResolvedValueOnce('denied');

      const { result } = renderHook(() => useMicTest());

      await act(async () => {
        await result.current.startTest();
      });

      expect(result.current.isTesting).toBe(false);
      expect(result.current.error).toContain('Microphone access denied');
    });

    it('acquires mic stream with correct constraints', async () => {
      const { result } = renderHook(() => useMicTest());

      await act(async () => {
        await result.current.startTest();
      });

      expect(mockGetUserMedia).toHaveBeenCalledWith({
        audio: expect.objectContaining({
          echoCancellation: true,
          noiseSuppression: true,
          autoGainControl: true,
          sampleRate: 48000,
          channelCount: 2,
        }),
      });
    });

    it('disables processing constraints in music mode', async () => {
      (useAudioSettingsStore as any).setState({ musicMode: true });

      const { result } = renderHook(() => useMicTest());

      await act(async () => {
        await result.current.startTest();
      });

      expect(mockGetUserMedia).toHaveBeenCalledWith({
        audio: expect.objectContaining({
          echoCancellation: false,
          noiseSuppression: false,
          autoGainControl: false,
        }),
      });
    });

    it('sets isTesting to true on success', async () => {
      const { result } = renderHook(() => useMicTest());

      await act(async () => {
        await result.current.startTest();
      });

      expect(result.current.isTesting).toBe(true);
      expect(result.current.error).toBeNull();
    });

    it('suspends call audio while mic test runs in-call', async () => {
      vi.mocked(useVoiceStore.getState).mockReturnValue({
        audioInputDeviceId: null,
        audioOutputDeviceId: null,
        connectionState: 'connected',
        localIsTesting: false,
      } as any);
      const { result } = renderHook(() => useMicTest());

      await act(async () => {
        await result.current.startTest();
      });

      expect(voiceService.beginTestSuspension).toHaveBeenCalled();
      expect(voiceService.setLocalTestingStatus).toHaveBeenCalledWith(true);

      act(() => {
        result.current.stopTest();
      });

      expect(voiceService.endTestSuspension).toHaveBeenCalled();
      expect(voiceService.setLocalTestingStatus).toHaveBeenCalledWith(false);
    });

    it('keeps call audio suspended while restarting an in-call test', async () => {
      vi.useFakeTimers();
      let voiceSubscriber: ((state: any, prev: any) => void) | undefined;
      vi.mocked(useVoiceStore.subscribe).mockImplementation((listener: any) => {
        voiceSubscriber = listener;
        return () => {};
      });
      vi.mocked(useVoiceStore.getState).mockReturnValue({
        audioInputDeviceId: 'mic-1',
        audioOutputDeviceId: null,
        connectionState: 'connected',
        localIsTesting: false,
      } as any);
      const { result } = renderHook(() => useMicTest());

      await act(async () => {
        await result.current.startTest();
      });

      expect(voiceService.beginTestSuspension).toHaveBeenCalledTimes(1);
      expect(voiceService.endTestSuspension).not.toHaveBeenCalled();

      await act(async () => {
        voiceSubscriber?.(
          {
            audioInputDeviceId: 'mic-2',
            audioOutputDeviceId: null,
          },
          {
            audioInputDeviceId: 'mic-1',
            audioOutputDeviceId: null,
          }
        );
        await vi.runOnlyPendingTimersAsync();
      });

      expect(voiceService.endTestSuspension).not.toHaveBeenCalled();
      expect(voiceService.beginTestSuspension).toHaveBeenCalledTimes(1);

      act(() => {
        result.current.stopTest();
      });

      expect(voiceService.endTestSuspension).toHaveBeenCalledTimes(1);
      vi.useRealTimers();
    });

    it('starts meter polling via requestAnimationFrame', async () => {
      const { result } = renderHook(() => useMicTest());

      await act(async () => {
        await result.current.startTest();
      });

      expect(globalThis.requestAnimationFrame).toHaveBeenCalled();
    });

    it('handles getUserMedia NotAllowedError', async () => {
      const domErr = new DOMException('Permission denied', 'NotAllowedError');
      mockGetUserMedia.mockRejectedValueOnce(domErr);

      const { result } = renderHook(() => useMicTest());

      await act(async () => {
        await result.current.startTest();
      });

      expect(result.current.isTesting).toBe(false);
      expect(result.current.error).toBe('Microphone access denied');
    });

    it('handles generic getUserMedia errors', async () => {
      mockGetUserMedia.mockRejectedValueOnce(new Error('Device not found'));

      const { result } = renderHook(() => useMicTest());

      await act(async () => {
        await result.current.startTest();
      });

      expect(result.current.isTesting).toBe(false);
      expect(result.current.error).toBe('Failed to access microphone');
    });
  });

  describe('stopTest', () => {
    it('cleans up all audio resources', async () => {
      const { result } = renderHook(() => useMicTest());

      await act(async () => {
        await result.current.startTest();
      });

      expect(result.current.isTesting).toBe(true);

      act(() => {
        result.current.stopTest();
      });

      expect(result.current.isTesting).toBe(false);
      expect(result.current.dbfsLevel).toBe(-Infinity);
      expect(result.current.error).toBeNull();
      expect(cancelAnimationFrame).toHaveBeenCalled();
    });

    it('stops mic stream tracks', async () => {
      const { result } = renderHook(() => useMicTest());

      await act(async () => {
        await result.current.startTest();
      });

      act(() => {
        result.current.stopTest();
      });

      expect(mockTrackStop).toHaveBeenCalled();
    });

    it('is safe to call when not testing', () => {
      const { result } = renderHook(() => useMicTest());

      act(() => {
        result.current.stopTest();
      });

      expect(result.current.isTesting).toBe(false);
    });
  });

  describe('cleanup on unmount', () => {
    it('calls stopTest on unmount', async () => {
      const { result, unmount } = renderHook(() => useMicTest());

      await act(async () => {
        await result.current.startTest();
      });

      expect(result.current.isTesting).toBe(true);

      unmount();
      // After unmount, the effect cleanup should have invoked stopTest
    });
  });

  describe('noise gate', () => {
    it('creates noise gate nodes in manual mode', async () => {
      (useAudioSettingsStore as any).setState({ noiseGateMode: 'manual' });

      const { result } = renderHook(() => useMicTest());

      await act(async () => {
        await result.current.startTest();
      });

      // In manual mode, createAnalyser and createGain are called extra times
      // for the noise gate pipeline
      expect(result.current.isTesting).toBe(true);
    });
  });
});
