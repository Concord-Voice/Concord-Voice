import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook, act, waitFor } from '@testing-library/react';
import { resetAllStores } from '../../helpers/store-helpers';

// Mock stores
vi.mock('@/renderer/stores/audio/audioSettingsStore', async (importOriginal) => {
  const actual =
    await importOriginal<typeof import('@/renderer/stores/audio/audioSettingsStore')>();
  const initialState = {
    musicMode: false,
    echoCancellation: true,
    noiseCancellation: true,
    autoGainControl: true,
    noiseGateMode: 'off' as string,
    noiseGateLevel: -50,
    inputVolume: 100,
  };
  type AudioState = typeof initialState;
  let state: AudioState = { ...initialState };
  const listeners = new Set<(next: AudioState, previous: AudioState) => void>();
  const setState = vi.fn((partial: Partial<AudioState>) => {
    const previous = state;
    state = { ...state, ...partial };
    listeners.forEach((listener) => listener(state, previous));
  });
  const subscribe = vi.fn((listener: (next: AudioState, previous: AudioState) => void) => {
    listeners.add(listener);
    return () => listeners.delete(listener);
  });
  const store = Object.assign(
    (selector?: (s: typeof state) => unknown) => (selector ? selector(state) : state),
    {
      getState: vi.fn(() => ({ ...state, reset: () => Object.assign(state, initialState) })),
      subscribe,
      setState,
      _listeners: listeners,
      _reset: () => (state = { ...initialState }),
    }
  );
  return { ...actual, useAudioSettingsStore: store };
});

vi.mock('@/renderer/stores/voice/voiceStore', () => {
  const initialState = {
    audioInputDeviceId: null as string | null,
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
    vi.fn((selector?: (state: VoiceState) => unknown) => (selector ? selector(state) : state)),
    {
      getState: vi.fn(() => ({ ...state, reset: () => setState(initialState) })),
      _getCanonicalState: () => ({ ...state, reset: () => setState(initialState) }),
      subscribe: vi.fn((listener: (next: VoiceState, previous: VoiceState) => void) => {
        listeners.add(listener);
        return () => listeners.delete(listener);
      }),
      setState,
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

vi.mock('@/renderer/stores/voice/osPermissionStore', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/stores/voice/osPermissionStore')>();
  return {
    ...actual,
    ensureOsPermission: vi.fn().mockResolvedValue('granted'),
  };
});

import { ensureOsPermission } from '@/renderer/stores/voice/osPermissionStore';
import { useAudioSettingsStore } from '@/renderer/stores/audio/audioSettingsStore';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { useMicTest } from '@/renderer/hooks/device/useMicTest';
import { voiceService } from '@/renderer/services/voice/voiceService';

// Build a comprehensive mock audio pipeline
const mockTrackStop = vi.fn();
const mockGetUserMedia = vi.fn();
let mockAudioContext: ReturnType<typeof createMockAudioPipeline>;
let nextModuleGate: Promise<void> | null = null;
let nextNodeConstructionFailure = false;
let nextAudioContextState: AudioContextState = 'running';
let nextAudioContextResume: (() => Promise<void>) | null = null;
let mockDestinationTrackStop: ReturnType<typeof vi.fn>;
let mockLoopbackSetSinkId: ReturnType<typeof vi.fn>;
let mockLoopbackAudioElement: {
  srcObject: MediaStream | null;
  setSinkId: ReturnType<typeof vi.fn>;
  play: ReturnType<typeof vi.fn>;
  pause: ReturnType<typeof vi.fn>;
} | null;
const mockProcessorNodes: Array<{
  context: Record<string, unknown>;
  name: string;
  options: unknown;
  instance: MockAudioWorkletNode;
}> = [];
class MockAudioWorkletNode {
  port = {
    onmessage: null as ((event: MessageEvent<unknown>) => void) | null,
    postMessage: vi.fn(),
    close: vi.fn(),
  };
  disconnect = vi.fn();
  addEventListener = vi.fn();
  connect: ReturnType<typeof vi.fn>;
  constructor(
    readonly context: Record<string, unknown>,
    readonly name: string,
    readonly options: unknown
  ) {
    if (nextNodeConstructionFailure) {
      nextNodeConstructionFailure = false;
      throw new Error('node construction failed');
    }
    mockProcessorNodes.push({ context, name, options, instance: this });
    this.connect = vi.fn((to: Record<string, unknown>) => {
      (
        context._connections as Array<{
          from: Record<string, unknown>;
          to: Record<string, unknown>;
        }>
      ).push({
        from: this as unknown as Record<string, unknown>,
        to,
      });
    });
  }
}

function createMockAudioPipeline() {
  const contextState = nextAudioContextState;
  const resumeAudioContext = nextAudioContextResume;
  const connections: Array<{ from: Record<string, unknown>; to: Record<string, unknown> }> = [];
  const gainNodes: Record<string, unknown>[] = [];
  // Each node needs its own connect mock that returns itself (for chaining)
  const createNode = (extras: Record<string, unknown> = {}) => {
    const node: Record<string, unknown> = { ...extras };
    node.connect = vi.fn((to: Record<string, unknown>) => {
      connections.push({ from: node, to });
      return to;
    });
    return node;
  };

  const sourceNode = createNode({});
  const destStream = { getTracks: () => [{ stop: mockDestinationTrackStop }] };
  const destNode = { stream: destStream };

  const mockCtx = {
    state: contextState,
    currentTime: 0,
    sampleRate: 48000,
    audioWorklet: {
      addModule: vi.fn(async () => {
        const gate = nextModuleGate;
        nextModuleGate = null;
        if (gate) await gate;
      }),
    },
    close: vi.fn().mockResolvedValue(undefined),
    resume: vi.fn(() => resumeAudioContext?.() ?? Promise.resolve()),
    createMediaStreamSource: vi.fn(() => sourceNode),
    createGain: vi.fn(() => {
      // Return a fresh gain node each time (volume gain vs noise gate gain)
      const gainNode = createNode({ gain: { value: 1, setTargetAtTime: vi.fn() } });
      gainNodes.push(gainNode);
      return gainNode;
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
    _connections: connections,
    _gainNodes: gainNodes,
    _sourceNode: sourceNode,
  };

  return mockCtx;
}

beforeEach(() => {
  vi.clearAllMocks();
  nextModuleGate = null;
  nextNodeConstructionFailure = false;
  nextAudioContextState = 'running';
  nextAudioContextResume = null;
  vi.mocked(useVoiceStore.getState).mockImplementation(() =>
    (useVoiceStore as any)._getCanonicalState()
  );
  ((useAudioSettingsStore as any)._listeners as Set<unknown>).clear();
  (useAudioSettingsStore as any)._reset();
  resetAllStores();
  (useVoiceStore as any).setState({
    audioInputDeviceId: null,
    audioOutputDeviceId: null,
    connectionState: 'disconnected',
    localIsTesting: false,
  } as any);
  vi.mocked(ensureOsPermission).mockResolvedValue('granted');

  // Mock AudioContext constructor — must use a class/function form for `new`
  mockDestinationTrackStop = vi.fn();
  mockAudioContext = createMockAudioPipeline();
  mockProcessorNodes.length = 0;
  (globalThis as any).AudioContext = function MockAudioContext() {
    mockAudioContext = createMockAudioPipeline();
    nextAudioContextState = 'running';
    nextAudioContextResume = null;
    return mockAudioContext;
  };
  (globalThis as any).AudioWorkletNode = MockAudioWorkletNode;

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
  mockLoopbackAudioElement = null;
  mockLoopbackSetSinkId = vi.fn().mockResolvedValue(undefined);
  (globalThis as any).Audio = function MockAudio() {
    this.srcObject = null;
    this.setSinkId = mockLoopbackSetSinkId;
    this.play = vi.fn().mockResolvedValue(undefined);
    this.pause = vi.fn();
    mockLoopbackAudioElement = this;
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
  const startMicPipeline = async () => {
    const { result } = renderHook(() => useMicTest());
    await act(async () => {
      await result.current.startTest();
    });
    expect(result.current.isTesting).toBe(true);
    return result;
  };

  const findManualLevelGain = () => {
    const graph = mockAudioContext as typeof mockAudioContext & {
      _connections: Array<{ from: Record<string, unknown>; to: Record<string, unknown> }>;
      _gainNodes: Record<string, unknown>[];
      _sourceNode: Record<string, unknown>;
    };
    const levelGains = graph._gainNodes;
    expect(
      levelGains,
      'the graph must contain exactly one manual microphone-level GainNode'
    ).toHaveLength(1);
    if (levelGains.length !== 1) return null;
    return { graph, levelGain: levelGains[0] };
  };

  const expectManualGainBeforeGate = () => {
    const graphBits = findManualLevelGain();
    if (!graphBits) return null;
    const { graph, levelGain } = graphBits;
    const sourceGains = graph._connections
      .filter(({ from, to }) => from === graph._sourceNode && graph._gainNodes.includes(to))
      .map(({ to }) => to);
    expect(graph._gainNodes, 'the only GainNode must be before the protector').toHaveLength(1);
    expect(
      sourceGains,
      'the raw microphone source must enter the manual level GainNode before the protector'
    ).toEqual([levelGain]);
    const processor = mockProcessorNodes.find(({ context }) => context === mockAudioContext);
    expect(processor?.name).toBe('concord-mic-processor');
    const processorNode = graph._connections.find(({ from, to }) => from === levelGain)?.to;
    expect(processorNode).toBeDefined();
    expect(graph._connections.some(({ from }) => from === processorNode)).toBe(true);
    expect(
      graph._connections.some(
        ({ from, to }) => from === processorNode && graph._gainNodes.includes(to)
      ),
      'the test graph must not boost audio after the peak protector'
    ).toBe(false);
    expect(
      (processor?.options as { processorOptions?: unknown } | undefined)?.processorOptions
    ).toMatchObject({
      protectAgcPeaks:
        (useAudioSettingsStore as any).getState().autoGainControl &&
        !(useAudioSettingsStore as any).getState().musicMode,
      gate:
        (useAudioSettingsStore as any).getState().noiseGateMode === 'manual'
          ? { kind: 'fixed' }
          : { kind: 'off' },
    });
    return levelGain;
  };

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
    it('#3638 keeps one microphone-level GainNode before the protector and uses unity when AGC is effective', async () => {
      (useAudioSettingsStore as any).setState({
        autoGainControl: true,
        musicMode: false,
        noiseGateMode: 'manual',
        inputVolume: 200,
      });
      await startMicPipeline();

      const levelGain = findManualLevelGain()?.levelGain;
      expect((levelGain?.gain as { value: number } | undefined)?.value).toBe(1);
      expectManualGainBeforeGate();
    });

    it('#3638 reports pre-gate dBFS from validated processor windows', async () => {
      const result = await startMicPipeline();
      const processor = mockProcessorNodes.at(-1)?.instance;
      expect(processor?.port.onmessage).toBeTypeOf('function');

      act(() => {
        processor?.port.onmessage?.({
          data: { peak: 0.25, frames: 960, valid: true, overloaded: false },
        } as MessageEvent<unknown>);
      });

      expect(result.current.dbfsLevel).toBeCloseTo(20 * Math.log10(0.25));
      expect(mockAudioContext.createAnalyser).not.toHaveBeenCalled();
    });

    it('#3638 keeps the pre-limiter overload warning visible until Test stops', async () => {
      const result = await startMicPipeline();
      const processor = mockProcessorNodes.at(-1)?.instance;
      const report = (overloaded: boolean) =>
        processor?.port.onmessage?.({
          data: { peak: 0.25, frames: 960, valid: true, overloaded },
        } as MessageEvent<unknown>);

      act(() => report(true));
      expect(result.current.inputOverloaded).toBe(true);

      act(() => report(false));
      expect(result.current.inputOverloaded).toBe(true);

      act(() => result.current.stopTest());
      expect(result.current.inputOverloaded).toBe(false);
    });

    it('#3638 closes the processor and capture when Test stops', async () => {
      const result = await startMicPipeline();
      const processor = mockProcessorNodes.at(-1)?.instance;

      act(() => result.current.stopTest());

      expect(processor?.port.postMessage).toHaveBeenCalledWith({ type: 'close' });
      expect(processor?.port.close).toHaveBeenCalledOnce();
      expect(processor?.disconnect).toHaveBeenCalledOnce();
      expect(mockTrackStop).toHaveBeenCalledOnce();
      expect(result.current.isTesting).toBe(false);
    });

    it('#3638 disposes a delayed processor candidate when Test is stopped during module loading', async () => {
      let resolveModule!: () => void;
      const moduleReady = new Promise<void>((resolve) => {
        resolveModule = resolve;
      });
      nextModuleGate = moduleReady;
      const { result } = renderHook(() => useMicTest());
      let starting!: Promise<void>;
      act(() => {
        starting = result.current.startTest();
      });
      await waitFor(() => expect(mockAudioContext.audioWorklet.addModule).toHaveBeenCalled());
      const candidateContext = mockAudioContext;

      act(() => result.current.stopTest());
      resolveModule();
      await act(async () => starting);

      expect(result.current.isTesting).toBe(false);
      expect(candidateContext.close).toHaveBeenCalledOnce();
      expect(mockTrackStop).toHaveBeenCalledOnce();
      expect(mockProcessorNodes.at(-1)?.instance.disconnect).toHaveBeenCalledOnce();
      expect(mockLoopbackAudioElement).toBeNull();
    });

    it.each([{ inputVolume: 0 }, { inputVolume: 100 }, { inputVolume: 200 }])(
      '#3638 applies saved manual microphone level $inputVolume% before the processor when AGC is off',
      async ({ inputVolume }) => {
        (useAudioSettingsStore as any).setState({
          autoGainControl: false,
          musicMode: false,
          noiseGateMode: 'manual',
          inputVolume,
        });
        await startMicPipeline();

        const levelGain = findManualLevelGain()?.levelGain;
        expect((levelGain?.gain as { value: number } | undefined)?.value).toBe(inputVolume / 100);
        expectManualGainBeforeGate();
      }
    );

    it('#3635 uses retained manual level in Music Mode without clearing the saved AGC preference', async () => {
      (useAudioSettingsStore as any).setState({
        autoGainControl: true,
        musicMode: true,
        noiseGateMode: 'manual',
        inputVolume: 200,
      });
      await startMicPipeline();

      expect((useAudioSettingsStore as any).getState().autoGainControl).toBe(true);
      expect(mockGetUserMedia).toHaveBeenCalledWith({
        audio: expect.objectContaining({ autoGainControl: false }),
      });
      const levelGain = findManualLevelGain()?.levelGain;
      expect((levelGain?.gain as { value: number } | undefined)?.value).toBe(2);
      expectManualGainBeforeGate();
    });

    it.each([
      ['AGC enabled', { autoGainControl: false, musicMode: false }, { autoGainControl: true }, 100],
      [
        'AGC disabled',
        { autoGainControl: true, musicMode: false },
        { autoGainControl: false },
        200,
      ],
      ['Music Mode enabled', { autoGainControl: true, musicMode: false }, { musicMode: true }, 200],
      [
        'Music Mode disabled',
        { autoGainControl: true, musicMode: true },
        { musicMode: false },
        100,
      ],
      [
        'saved manual level changed',
        { autoGainControl: false, musicMode: false },
        { inputVolume: 0 },
        0,
      ],
      [
        'effective microphone level changed to zero while AGC remains effective',
        { autoGainControl: true, musicMode: false },
        { inputVolume: 0 },
        0,
      ],
      [
        'positive saved level changed while AGC remains effective',
        { autoGainControl: true, musicMode: false },
        { inputVolume: 150 },
        null,
      ],
    ])(
      '#3638 committed %s updates the active Test graph to the effective level',
      async (_label, initialSettings, update, effectivePercent) => {
        (useAudioSettingsStore as any).setState({
          ...initialSettings,
          noiseGateMode: 'manual',
          inputVolume: 200,
        });
        await startMicPipeline();
        const levelGain = findManualLevelGain()?.levelGain;
        const gainParam = levelGain?.gain as
          { value: number; setTargetAtTime: ReturnType<typeof vi.fn> } | undefined;
        const previousProcessor = mockProcessorNodes.at(-1);
        const oldProcessorCount = mockProcessorNodes.length;
        act(() => (useAudioSettingsStore as any).setState(update));
        const changesEffectiveAgc =
          Object.hasOwn(update as object, 'autoGainControl') ||
          Object.hasOwn(update as object, 'musicMode');
        if (changesEffectiveAgc) {
          expect(
            previousProcessor?.instance.port.close,
            'an AGC or Music Mode transition must synchronously retire the prior Test graph'
          ).toHaveBeenCalledOnce();
          await waitFor(() => expect(mockProcessorNodes.length).toBeGreaterThan(oldProcessorCount));
          const settings = (useAudioSettingsStore as any).getState();
          expect(
            (mockProcessorNodes.at(-1)?.options as { processorOptions?: unknown })?.processorOptions
          ).toMatchObject({
            protectAgcPeaks: settings.autoGainControl && !settings.musicMode,
            gate: { kind: 'fixed', thresholdDbfs: -50 },
          });
          expect((findManualLevelGain()?.levelGain.gain as { value: number }).value).toBe(
            (effectivePercent as number) / 100
          );
        } else if (effectivePercent == null) {
          expect(
            gainParam?.value,
            'positive saved manual levels must retain unity Test gain while AGC is effective'
          ).toBe(1);
          expect(
            gainParam?.setTargetAtTime,
            'changing a positive saved level while effective AGC remains on must not update the Test gain'
          ).not.toHaveBeenCalled();
        } else {
          expect(
            gainParam?.setTargetAtTime,
            'committed manual-level changes must update the active Test graph'
          ).toHaveBeenCalledWith(effectivePercent / 100, 0, 0.01);
        }
      }
    );

    it('#3635 starts normally when deferred microphone permission and capture resolve', async () => {
      let resolvePermission!: (permission: 'granted') => void;
      const permission = new Promise<'granted'>((resolve) => {
        resolvePermission = resolve;
      });
      let resolveCapture!: (stream: MediaStream) => void;
      const capture = new Promise<MediaStream>((resolve) => {
        resolveCapture = resolve;
      });
      vi.mocked(ensureOsPermission).mockReturnValueOnce(permission);
      mockGetUserMedia.mockReturnValueOnce(capture);
      const { result } = renderHook(() => useMicTest());
      let start!: Promise<void>;
      act(() => {
        start = result.current.startTest();
      });
      await act(async () => {
        resolvePermission('granted');
        await Promise.resolve();
      });
      expect(mockGetUserMedia).toHaveBeenCalledOnce();
      await act(async () => {
        resolveCapture({ getTracks: () => [{ stop: mockTrackStop }] } as unknown as MediaStream);
        await start;
      });
      expect(result.current.isTesting).toBe(true);
    });

    it('#3635 applies committed processing changes after pending capture and lets Stop cancel its successor', async () => {
      (useAudioSettingsStore as any).setState({
        autoGainControl: true,
        musicMode: false,
        noiseCancellation: true,
        echoCancellation: true,
      });
      (useVoiceStore as any).setState({ audioInputDeviceId: 'mic-processing-restart' });

      let resolveInitialCapture!: (stream: MediaStream) => void;
      const initialCapture = new Promise<MediaStream>((resolve) => {
        resolveInitialCapture = resolve;
      });
      let resolveSuccessorCapture!: (stream: MediaStream) => void;
      const successorCapture = new Promise<MediaStream>((resolve) => {
        resolveSuccessorCapture = resolve;
      });
      const initialTrackStop = vi.fn();
      const successorTrackStop = vi.fn();
      const captureConstraints: MediaTrackConstraints[] = [];
      mockGetUserMedia.mockImplementation((constraints: MediaStreamConstraints) => {
        captureConstraints.push(constraints.audio as MediaTrackConstraints);
        return captureConstraints.length === 1 ? initialCapture : successorCapture;
      });

      const { result } = renderHook(() => useMicTest());
      let initialStart!: Promise<void>;
      act(() => {
        initialStart = result.current.startTest();
      });
      await waitFor(() => expect(mockGetUserMedia).toHaveBeenCalledOnce());
      expect(captureConstraints[0]).toMatchObject({
        deviceId: { exact: 'mic-processing-restart' },
        autoGainControl: true,
      });

      act(() => (useAudioSettingsStore as any).setState({ autoGainControl: false }));
      await act(async () => {
        resolveInitialCapture({
          getTracks: () => [{ stop: initialTrackStop }],
        } as unknown as MediaStream);
        await initialStart;
      });
      await waitFor(() => expect(mockGetUserMedia).toHaveBeenCalledTimes(2), { timeout: 250 });

      expect(captureConstraints[1]).toMatchObject({
        deviceId: { exact: 'mic-processing-restart' },
        autoGainControl: false,
      });
      expect(
        initialTrackStop,
        'the superseded pending capture must be released'
      ).toHaveBeenCalledOnce();

      act(() => result.current.stopTest());
      await act(async () => {
        resolveSuccessorCapture({
          getTracks: () => [{ stop: successorTrackStop }],
        } as unknown as MediaStream);
        await Promise.resolve();
      });
      await waitFor(() => expect(successorTrackStop).toHaveBeenCalledOnce());
      expect(
        mockGetUserMedia,
        'explicit Stop must cancel any later processing restart'
      ).toHaveBeenCalledTimes(2);
      expect(result.current.isTesting).toBe(false);
    });

    it('#3635 ignores permission granted after microphone test cancellation', async () => {
      let resolvePermission!: (permission: 'granted') => void;
      const permission = new Promise<'granted'>((resolve) => {
        resolvePermission = resolve;
      });
      vi.mocked(ensureOsPermission).mockReturnValueOnce(permission);
      vi.mocked(useVoiceStore.getState).mockReturnValue({
        audioInputDeviceId: 'mic-1',
        audioOutputDeviceId: null,
        connectionState: 'connected',
        localIsTesting: false,
        reset: vi.fn(),
      } as any);
      const { result } = renderHook(() => useMicTest());
      let start!: Promise<void>;
      act(() => {
        start = result.current.startTest();
      });
      act(() => result.current.stopTest());
      await act(async () => {
        resolvePermission('granted');
        await start;
      });
      expect(
        mockGetUserMedia,
        'cancelled permission must not start capture'
      ).not.toHaveBeenCalled();
      expect(voiceService.beginTestSuspension).not.toHaveBeenCalled();
      expect(result.current.isTesting).toBe(false);
    });

    it('#3635 releases microphone capture resolved after cancellation', async () => {
      let resolveCapture!: (stream: MediaStream) => void;
      const capture = new Promise<MediaStream>((resolve) => {
        resolveCapture = resolve;
      });
      const stop = vi.fn();
      mockGetUserMedia.mockReturnValueOnce(capture);
      vi.mocked(useVoiceStore.getState).mockReturnValue({
        audioInputDeviceId: 'mic-1',
        audioOutputDeviceId: null,
        connectionState: 'connected',
        localIsTesting: false,
        reset: vi.fn(),
      } as any);
      const { result } = renderHook(() => useMicTest());
      let start!: Promise<void>;
      act(() => {
        start = result.current.startTest();
      });
      await act(async () => {
        await Promise.resolve();
      });
      act(() => result.current.stopTest());
      await act(async () => {
        resolveCapture({ getTracks: () => [{ stop }] } as unknown as MediaStream);
        await start;
      });
      expect(
        stop,
        'cancelled capture must release its late microphone track'
      ).toHaveBeenCalledOnce();
      expect(result.current.isTesting).toBe(false);
      expect(voiceService.endTestSuspension).toHaveBeenCalledOnce();
    });

    it('#3635 restarts the pending Test with the latest selected microphone', async () => {
      vi.useFakeTimers();
      let resolveFirstRestartCapture!: (stream: MediaStream) => void;
      const firstRestartCapture = new Promise<MediaStream>((resolve) => {
        resolveFirstRestartCapture = resolve;
      });
      const requestedInputs: Array<MediaTrackConstraints['deviceId']> = [];
      mockGetUserMedia.mockImplementation((constraints: MediaStreamConstraints) => {
        const audio = constraints.audio as MediaTrackConstraints;
        requestedInputs.push(audio.deviceId);
        if (requestedInputs.length === 2) return firstRestartCapture;
        return Promise.resolve({ getTracks: () => [{ stop: vi.fn() }] } as unknown as MediaStream);
      });

      const { result } = renderHook(() => useMicTest());
      await act(async () => {
        await result.current.startTest();
      });
      expect(result.current.isTesting).toBe(true);

      act(() => (useVoiceStore as any).setState({ audioInputDeviceId: 'mic-first-change' }));
      await act(async () => {
        vi.advanceTimersByTime(0);
        await Promise.resolve();
        await Promise.resolve();
      });
      expect(mockGetUserMedia).toHaveBeenCalledTimes(2);
      expect(requestedInputs[1]).toEqual({ exact: 'mic-first-change' });

      act(() => (useVoiceStore as any).setState({ audioInputDeviceId: 'mic-latest' }));
      const lateTrackStop = vi.fn();
      await act(async () => {
        resolveFirstRestartCapture({
          getTracks: () => [{ stop: lateTrackStop }],
        } as unknown as MediaStream);
        await Promise.resolve();
        await Promise.resolve();
        vi.advanceTimersByTime(0);
        await Promise.resolve();
        await Promise.resolve();
      });

      expect(
        requestedInputs.some(
          (deviceId) => JSON.stringify(deviceId) === JSON.stringify({ exact: 'mic-latest' })
        ),
        'a second device change during restart must eventually capture the latest selected microphone'
      ).toBe(true);
      expect(result.current.isTesting).toBe(true);
      expect(
        lateTrackStop,
        'the superseded microphone capture must be released'
      ).toHaveBeenCalledOnce();
    });

    it('#3635 closes a microphone AudioContext while resume is still pending on Stop', async () => {
      let resolveResume!: () => void;
      const pendingResume = new Promise<void>((resolve) => {
        resolveResume = resolve;
      });
      nextAudioContextState = 'suspended';
      nextAudioContextResume = () => pendingResume;
      const { result } = renderHook(() => useMicTest());
      let start!: Promise<void>;
      act(() => {
        start = result.current.startTest();
      });
      await act(async () => {
        await Promise.resolve();
        await Promise.resolve();
      });
      expect(mockAudioContext.resume).toHaveBeenCalledOnce();
      act(() => result.current.stopTest());
      expect(
        mockAudioContext.close,
        'Stop must close the locally-owned context before resume resolves'
      ).toHaveBeenCalledOnce();
      await act(async () => {
        resolveResume();
        await start;
      });
    });

    it('#3635 starts microphone testing when AudioContext resume completes normally', async () => {
      let resolveResume!: () => void;
      const pendingResume = new Promise<void>((resolve) => {
        resolveResume = resolve;
      });
      nextAudioContextState = 'suspended';
      nextAudioContextResume = () => pendingResume;
      const { result } = renderHook(() => useMicTest());
      let start!: Promise<void>;
      act(() => {
        start = result.current.startTest();
      });
      await act(async () => {
        await Promise.resolve();
        await Promise.resolve();
      });
      expect(mockAudioContext.resume).toHaveBeenCalledOnce();
      await act(async () => {
        resolveResume();
        await start;
      });
      expect(result.current.isTesting).toBe(true);
    });

    it('#3635 does not build a microphone graph after Stop wins the context-helper return boundary', async () => {
      const { result } = renderHook(() => useMicTest());
      let sourceCallsAtStop = 0;
      let gainCallsAtStop = 0;
      let loopbackCallsAtStop = 0;
      let playsAtStop = 0;
      (globalThis as any).AudioContext = function MockAudioContext() {
        queueMicrotask(() => {
          sourceCallsAtStop = mockAudioContext.createMediaStreamSource.mock.calls.length;
          gainCallsAtStop = mockAudioContext.createGain.mock.calls.length;
          loopbackCallsAtStop = mockAudioContext.createMediaStreamDestination.mock.calls.length;
          playsAtStop = mockLoopbackAudioElement?.play.mock.calls.length ?? 0;
          result.current.stopTest();
        });
        return mockAudioContext;
      };
      let start!: Promise<void>;
      await act(async () => {
        start = result.current.startTest();
        await start;
      });
      expect(
        mockAudioContext.close,
        'Stop must close the context returned by the helper'
      ).toHaveBeenCalledOnce();
      expect(
        mockTrackStop,
        'Stop must release the microphone stream acquired before context creation'
      ).toHaveBeenCalledOnce();
      expect(
        mockAudioContext.createMediaStreamSource,
        'Stop must prevent graph construction after the returned context has been cancelled'
      ).toHaveBeenCalledTimes(sourceCallsAtStop);
      expect(
        mockAudioContext.createGain,
        'Stop must prevent late microphone gain nodes'
      ).toHaveBeenCalledTimes(gainCallsAtStop);
      expect(
        mockAudioContext.createMediaStreamDestination,
        'Stop must prevent a late loopback destination'
      ).toHaveBeenCalledTimes(loopbackCallsAtStop);
      expect(
        mockLoopbackAudioElement?.play.mock.calls.length ?? 0,
        'Stop must prevent late loopback playback'
      ).toBe(playsAtStop);
      expect(result.current.isTesting).toBe(false);
      expect(vi.mocked(requestAnimationFrame)).not.toHaveBeenCalled();
    });

    it('#3635 does not start meter polling after Stop wins the loopback-helper return boundary', async () => {
      let resolvePlay!: () => void;
      const pendingPlay = new Promise<void>((resolve) => {
        resolvePlay = resolve;
      });
      const { result } = renderHook(() => useMicTest());
      (globalThis as any).Audio = function MockPendingAudio() {
        this.srcObject = null;
        this.setSinkId = mockLoopbackSetSinkId;
        this.play = vi.fn().mockReturnValue(pendingPlay);
        this.pause = vi.fn();
        mockLoopbackAudioElement = this;
      };
      let start!: Promise<void>;
      act(() => {
        start = result.current.startTest();
      });
      await act(async () => {
        for (
          let index = 0;
          index < 8 && !mockLoopbackAudioElement?.play.mock.calls.length;
          index++
        ) {
          await Promise.resolve();
        }
      });
      expect(mockLoopbackAudioElement?.play).toHaveBeenCalledOnce();
      let rafCallsAtStop = 0;
      await act(async () => {
        resolvePlay();
        await Promise.resolve();
        rafCallsAtStop = vi.mocked(requestAnimationFrame).mock.calls.length;
        result.current.stopTest();
        await start;
      });
      expect(
        vi.mocked(requestAnimationFrame),
        'cancelled loopback completion must not start meter polling after Stop'
      ).toHaveBeenCalledTimes(rafCallsAtStop);
      expect(result.current.isTesting).toBe(false);
    });

    it('#3635 releases loopback element and destination track while sink selection is pending', async () => {
      let resolveSink!: () => void;
      const pendingSink = new Promise<void>((resolve) => {
        resolveSink = resolve;
      });
      mockLoopbackSetSinkId.mockReturnValueOnce(pendingSink);
      (useVoiceStore as any).setState({ audioOutputDeviceId: 'speaker-1' });
      const { result } = renderHook(() => useMicTest());
      let start!: Promise<void>;
      act(() => {
        start = result.current.startTest();
      });
      await waitFor(() => expect(mockLoopbackSetSinkId).toHaveBeenCalledOnce());
      const audioElement = mockLoopbackAudioElement;
      expect(audioElement).not.toBeNull();
      const destinationStream = audioElement!.srcObject;
      expect(destinationStream).not.toBeNull();
      act(() => result.current.stopTest());
      expect(
        audioElement!.pause,
        'Stop must pause the pending loopback element'
      ).toHaveBeenCalledOnce();
      expect(audioElement!.srcObject, 'Stop must detach its loopback stream').toBeNull();
      expect(
        mockDestinationTrackStop,
        'Stop must release the pending destination track'
      ).toHaveBeenCalledOnce();
      await act(async () => {
        resolveSink();
        await start;
      });
      expect(audioElement!.srcObject).toBeNull();
      expect(result.current.isTesting).toBe(false);
    });

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

    it.each(['module load', 'node construction'] as const)(
      'reports %s failure as microphone processing failure after successful capture',
      async (failure) => {
        if (failure === 'module load') {
          nextModuleGate = Promise.resolve().then(() => {
            throw new Error('module load failed');
          });
        } else {
          nextNodeConstructionFailure = true;
        }

        const { result } = renderHook(() => useMicTest());
        await act(async () => {
          await result.current.startTest();
        });

        expect(mockGetUserMedia).toHaveBeenCalledOnce();
        expect(result.current.isTesting).toBe(false);
        expect(result.current.error).toBe('Microphone processing failed. Retry Test.');
        expect(mockTrackStop).toHaveBeenCalledOnce();
      }
    );
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
      expect(mockProcessorNodes.at(-1)?.instance.port.close).toHaveBeenCalledOnce();
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
