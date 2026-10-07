import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { resetAllStores } from '../../helpers/store-helpers';

vi.mock('@/renderer/stores/voice/voiceStore', () => {
  const initialState = {
    audioOutputDeviceId: null as string | null,
    connectionState: 'disconnected',
    localIsTesting: false,
  };
  type VoiceState = typeof initialState;
  let state: VoiceState = { ...initialState };
  const listeners = new Set<(next: VoiceState, previous: VoiceState) => void>();
  const setState = vi.fn((partial: Partial<VoiceState>) => {
    const previous = state;
    state = { ...state, ...partial };
    listeners.forEach((listener) => listener(state, previous));
  });
  const useVoiceStore = Object.assign(
    vi.fn((selector: (state: VoiceState) => unknown) => selector(state)),
    {
      getState: vi.fn(() => ({ ...state, reset: () => setState(initialState) })),
      setState,
      subscribe: vi.fn((listener: (next: VoiceState, previous: VoiceState) => void) => {
        listeners.add(listener);
        return () => listeners.delete(listener);
      }),
    }
  );
  return { useVoiceStore };
});

vi.mock('@/renderer/services/voice/voiceService', () => ({
  voiceService: {
    beginTestSuspension: vi.fn(),
    endTestSuspension: vi.fn(),
    setLocalTestingStatus: vi.fn(),
  },
}));

import { useOutputTest } from '@/renderer/hooks/device/useOutputTest';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { voiceService } from '@/renderer/services/voice/voiceService';

let mockSetSinkId: ReturnType<typeof vi.fn>;
let mockPlay: ReturnType<typeof vi.fn>;
let mockAudioContext: Record<string, any>;

function createMockNode(extras: Record<string, unknown> = {}) {
  const node: Record<string, unknown> = { ...extras };
  node.connect = vi.fn(() => node);
  return node;
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.useFakeTimers();
  resetAllStores();
  (useVoiceStore as any).setState({
    audioOutputDeviceId: 'speaker-1',
    connectionState: 'disconnected',
    localIsTesting: false,
  });

  mockAudioContext = {
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
    return mockAudioContext;
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

  it('suspends call audio while output test runs in-call', async () => {
    (useVoiceStore as any).setState({
      audioOutputDeviceId: 'speaker-1',
      connectionState: 'connected',
      localIsTesting: false,
    });
    const { result } = renderHook(() => useOutputTest());

    await act(async () => {
      await result.current.playTestTone();
    });

    expect(voiceService.beginTestSuspension).toHaveBeenCalled();
    expect(voiceService.setLocalTestingStatus).toHaveBeenCalledWith(true);

    await act(async () => {
      vi.advanceTimersByTime(700);
    });

    expect(voiceService.endTestSuspension).toHaveBeenCalled();
    expect(voiceService.setLocalTestingStatus).toHaveBeenCalledWith(false);
  });

  it('routes audio to the selected output device via setSinkId', async () => {
    const { result } = renderHook(() => useOutputTest());
    await act(async () => {
      await result.current.playTestTone();
    });
    expect(mockSetSinkId).toHaveBeenCalledWith('speaker-1');
  });

  it('#3635 completes normally when deferred output selection resolves', async () => {
    let resolveSink!: () => void;
    const pendingSink = new Promise<void>((resolve) => {
      resolveSink = resolve;
    });
    mockSetSinkId.mockReturnValueOnce(pendingSink);
    const { result } = renderHook(() => useOutputTest());
    let start!: Promise<void>;
    act(() => {
      start = result.current.playTestTone();
    });
    await act(async () => {
      resolveSink();
      await start;
    });
    expect(mockPlay).toHaveBeenCalledOnce();
    expect(result.current.isTesting).toBe(true);
  });

  it('#3635 stops context setup when resume resolves after unmount', async () => {
    let resolveResume!: () => void;
    const pendingResume = new Promise<void>((resolve) => {
      resolveResume = resolve;
    });
    mockAudioContext.state = 'suspended';
    mockAudioContext.resume = vi.fn().mockReturnValue(pendingResume);
    const { result, unmount } = renderHook(() => useOutputTest());
    let start!: Promise<void>;
    act(() => {
      start = result.current.playTestTone();
    });
    unmount();
    await act(async () => {
      resolveResume();
      await start;
    });
    expect(
      mockPlay,
      'unmounted context must not start playback after resume'
    ).not.toHaveBeenCalled();
  });

  it('#3635 running-context helper return cannot create a graph after Stop', async () => {
    (useVoiceStore as any).setState({ audioOutputDeviceId: null, connectionState: 'disconnected' });
    const { result } = renderHook(() => useOutputTest());
    let start!: Promise<void>;
    let oscillatorsAtStop = 0;
    let playsAtStop = 0;
    act(() => {
      start = result.current.playTestTone();
      result.current.stopTest();
      oscillatorsAtStop = mockAudioContext.createOscillator.mock.calls.length;
      playsAtStop = mockPlay.mock.calls.length;
    });
    await act(async () => {
      await start;
    });
    expect(
      mockAudioContext.createOscillator,
      'a running context must not create tone nodes after Stop'
    ).toHaveBeenCalledTimes(oscillatorsAtStop);
    expect(
      mockPlay,
      'a stopped output test must not begin playback after Stop'
    ).toHaveBeenCalledTimes(playsAtStop);
    expect(result.current.isTesting).toBe(false);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('#3635 resumed-context helper return cannot create a graph after Stop', async () => {
    (useVoiceStore as any).setState({ audioOutputDeviceId: null, connectionState: 'disconnected' });
    let resolveResume!: () => void;
    const pendingResume = new Promise<void>((resolve) => {
      resolveResume = resolve;
    });
    mockAudioContext.state = 'suspended';
    mockAudioContext.resume = vi.fn().mockReturnValue(pendingResume);
    const { result } = renderHook(() => useOutputTest());
    let start!: Promise<void>;
    act(() => {
      start = result.current.playTestTone();
    });
    expect(mockAudioContext.resume).toHaveBeenCalledOnce();
    let oscillatorsAtStop = 0;
    let playsAtStop = 0;
    await act(async () => {
      resolveResume();
      await Promise.resolve();
      result.current.stopTest();
      oscillatorsAtStop = mockAudioContext.createOscillator.mock.calls.length;
      playsAtStop = mockPlay.mock.calls.length;
      await start;
    });
    expect(
      mockAudioContext.createOscillator,
      'a context resumed before Stop must not create nodes in the caller continuation'
    ).toHaveBeenCalledTimes(oscillatorsAtStop);
    expect(mockPlay, 'resumed output must not start playback after Stop').toHaveBeenCalledTimes(
      playsAtStop
    );
    expect(result.current.isTesting).toBe(false);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('#3635 does not start output playback after unmount during sink selection', async () => {
    let resolveSink!: () => void;
    const pendingSink = new Promise<void>((resolve) => {
      resolveSink = resolve;
    });
    mockSetSinkId.mockReturnValueOnce(pendingSink);
    (useVoiceStore as any).setState({
      audioOutputDeviceId: 'speaker-1',
      connectionState: 'connected',
      localIsTesting: false,
    });
    const { result, unmount } = renderHook(() => useOutputTest());
    let start!: Promise<void>;
    act(() => {
      start = result.current.playTestTone();
    });
    unmount();
    await act(async () => {
      resolveSink();
      await start;
    });
    expect(mockPlay, 'cancelled output must not play after sink selection').not.toHaveBeenCalled();
    expect(voiceService.endTestSuspension).toHaveBeenCalledOnce();
  });

  it('#3635 public Stop cancels a pending sink selection', async () => {
    let resolveSink!: () => void;
    const pendingSink = new Promise<void>((resolve) => {
      resolveSink = resolve;
    });
    mockSetSinkId.mockReturnValueOnce(pendingSink);
    (useVoiceStore as any).setState({ connectionState: 'connected' });
    const { result } = renderHook(() => useOutputTest());
    let start!: Promise<void>;
    act(() => {
      start = result.current.playTestTone();
    });
    act(() => result.current.stopTest());
    await act(async () => {
      resolveSink();
      await start;
    });
    expect(mockPlay, 'Stop must prevent late playback').not.toHaveBeenCalled();
    expect(voiceService.endTestSuspension).toHaveBeenCalledOnce();
  });

  it('#3635 sink helper return cannot begin playback after Stop', async () => {
    let resolveSink!: () => void;
    const pendingSink = new Promise<void>((resolve) => {
      resolveSink = resolve;
    });
    mockSetSinkId.mockReturnValueOnce(pendingSink);
    (useVoiceStore as any).setState({ connectionState: 'disconnected' });
    const { result } = renderHook(() => useOutputTest());
    let start!: Promise<void>;
    act(() => {
      start = result.current.playTestTone();
    });
    await act(async () => {
      await Promise.resolve();
    });
    expect(mockSetSinkId).toHaveBeenCalledOnce();
    let playsAtStop = 0;
    await act(async () => {
      resolveSink();
      await Promise.resolve();
      playsAtStop = mockPlay.mock.calls.length;
      result.current.stopTest();
      await start;
    });
    expect(
      mockPlay,
      'sink resolution must not start playback in a post-Stop caller continuation'
    ).toHaveBeenCalledTimes(playsAtStop);
    expect(result.current.isTesting).toBe(false);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('#3635 stops output when the selected device changes during sink selection', async () => {
    let resolveSink!: () => void;
    const pendingSink = new Promise<void>((resolve) => {
      resolveSink = resolve;
    });
    mockSetSinkId.mockReturnValueOnce(pendingSink);
    (useVoiceStore as any).setState({ connectionState: 'connected' });
    const { result } = renderHook(() => useOutputTest());
    let start!: Promise<void>;
    act(() => {
      start = result.current.playTestTone();
    });
    act(() => (useVoiceStore as any).setState({ audioOutputDeviceId: 'speaker-2' }));
    await act(async () => {
      resolveSink();
      await start;
    });
    expect(mockPlay, 'old-device completion must not start playback').not.toHaveBeenCalled();
    expect(voiceService.endTestSuspension).toHaveBeenCalledOnce();
  });

  it('#3635 predecessor completion cannot disturb its successor output test', async () => {
    let resolvePredecessorSink!: () => void;
    const predecessorSink = new Promise<void>((resolve) => {
      resolvePredecessorSink = resolve;
    });
    mockSetSinkId.mockReturnValueOnce(predecessorSink).mockResolvedValueOnce(undefined);
    (useVoiceStore as any).setState({ connectionState: 'connected' });
    const { result } = renderHook(() => useOutputTest());
    let predecessor!: Promise<void>;
    act(() => {
      predecessor = result.current.playTestTone();
    });
    let successor!: Promise<void>;
    act(() => {
      successor = result.current.playTestTone();
    });
    await act(async () => {
      await successor;
    });
    expect(result.current.isTesting).toBe(true);
    const suspensionReleases = vi.mocked(voiceService.endTestSuspension).mock.calls.length;
    await act(async () => {
      resolvePredecessorSink();
      await predecessor;
    });
    expect(mockPlay).toHaveBeenCalledOnce();
    expect(result.current.isTesting).toBe(true);
    expect(voiceService.endTestSuspension).toHaveBeenCalledTimes(suspensionReleases);
  });

  it('#3635 does not revive output test when playback resolves after unmount', async () => {
    let resolvePlay!: () => void;
    const pendingPlay = new Promise<void>((resolve) => {
      resolvePlay = resolve;
    });
    mockPlay.mockReturnValueOnce(pendingPlay);
    (useVoiceStore as any).setState({
      audioOutputDeviceId: null,
      connectionState: 'connected',
      localIsTesting: false,
    });
    const { result, unmount } = renderHook(() => useOutputTest());
    let start!: Promise<void>;
    act(() => {
      start = result.current.playTestTone();
    });
    await act(async () => {
      await Promise.resolve();
    });
    unmount();
    await act(async () => {
      resolvePlay();
      await start;
    });
    expect(result.current.isTesting).toBe(false);
    expect(vi.getTimerCount(), 'cancelled playback must not schedule cleanup work').toBe(0);
    expect(voiceService.endTestSuspension).toHaveBeenCalledOnce();
  });

  it('#3635 play helper return cannot re-arm output state or timer after Stop', async () => {
    let resolvePlay!: () => void;
    const pendingPlay = new Promise<void>((resolve) => {
      resolvePlay = resolve;
    });
    mockPlay.mockReturnValueOnce(pendingPlay);
    (useVoiceStore as any).setState({ audioOutputDeviceId: null, connectionState: 'disconnected' });
    const { result } = renderHook(() => useOutputTest());
    let start!: Promise<void>;
    act(() => {
      start = result.current.playTestTone();
    });
    await act(async () => {
      await Promise.resolve();
    });
    expect(mockPlay).toHaveBeenCalledOnce();
    await act(async () => {
      resolvePlay();
      await Promise.resolve();
      result.current.stopTest();
      await start;
    });
    expect(
      result.current.isTesting,
      'play completion after Stop must not reactivate output test state'
    ).toBe(false);
    expect(vi.getTimerCount(), 'play completion after Stop must not schedule a test timeout').toBe(
      0
    );
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

  it('#3635 reports a fixed output failure when AudioContext construction throws', async () => {
    (globalThis as any).AudioContext = function Throwing() {
      throw new Error('No audio context');
    };
    const { result } = renderHook(() => useOutputTest());
    await act(async () => {
      await result.current.playTestTone();
    });
    expect(result.current.isTesting).toBe(false);
    expect(result.current.error).toBe('Failed to play test tone');
    expect(result.current.error).not.toContain('No audio context');
  });

  it('skips setSinkId when no output device selected', async () => {
    (useVoiceStore as any).setState({
      audioOutputDeviceId: null,
      connectionState: 'disconnected',
      localIsTesting: false,
    });
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
