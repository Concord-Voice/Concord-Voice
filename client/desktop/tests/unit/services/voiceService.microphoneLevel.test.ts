import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';
import { useAudioSettingsStore } from '@/renderer/stores/audio/audioSettingsStore';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { voiceService } from '@/renderer/services/voice/voiceService';
import { mockUser } from '../../mocks/fixtures';

type Edge = { from: AudioNodeMock; to: AudioNodeMock };
type AudioNodeMock = {
  kind: string;
  inputTracks?: MediaStreamTrack[];
  connect: (to: AudioNodeMock) => void;
  gain?: { value: number; setTargetAtTime: ReturnType<typeof vi.fn> };
  stream?: MediaStream;
};
type AudioGraph = {
  edges: Edge[];
  gains: AudioNodeMock[];
  sourceTracks: MediaStreamTrack[][];
  outputTrack: TrackMock;
  context: AudioContextMock;
};

const graphs: AudioGraph[] = [];

class TrackMock {
  kind = 'audio';
  id = `mic-${Math.random()}`;
  readyState = 'live';
  enabled = true;
  stop = vi.fn(() => {
    this.readyState = 'ended';
  });
  getSettings = vi.fn().mockReturnValue({});
}

class StreamMock {
  constructor(private tracks: TrackMock[]) {}
  getTracks() {
    return this.tracks;
  }
  getAudioTracks() {
    return this.tracks.filter((track) => track.kind === 'audio');
  }
  getVideoTracks() {
    return this.tracks.filter((track) => track.kind === 'video');
  }
  addTrack(track: TrackMock) {
    this.tracks.push(track);
  }
}

class AudioContextMock {
  static rejectNextModule = false;
  static nextModuleGate: Promise<void> | null = null;
  state: AudioContextState = 'running';
  currentTime = 0;
  sampleRate = 48000;
  graph: AudioGraph;
  constructor() {
    this.graph = {
      edges: [],
      gains: [],
      sourceTracks: [],
      outputTrack: new TrackMock(),
      context: this,
    };
    this.graph.outputTrack.id = `processed-${graphs.length}`;
    graphs.push(this.graph);
  }
  createMediaStreamSource(stream: StreamMock) {
    const node: AudioNodeMock = {
      kind: 'source',
      inputTracks: stream.getAudioTracks(),
      connect: (to) => this.graph.edges.push({ from: node, to }),
    };
    this.graph.sourceTracks.push(node.inputTracks!);
    return node;
  }
  audioWorklet = {
    addModule: vi.fn(async () => {
      if (AudioContextMock.rejectNextModule) {
        AudioContextMock.rejectNextModule = false;
        throw new Error('module load failed');
      }
      const gate = AudioContextMock.nextModuleGate;
      AudioContextMock.nextModuleGate = null;
      if (gate) await gate;
    }),
  };
  createAnalyser() {
    const node: AudioNodeMock = {
      kind: 'analyser',
      connect: (to) => this.graph.edges.push({ from: node, to }),
    };
    Object.assign(node, { fftSize: 0, frequencyBinCount: 128, getByteTimeDomainData: vi.fn() });
    return node;
  }
  createGain() {
    const node: AudioNodeMock = {
      kind: 'gain',
      gain: { value: 1, setTargetAtTime: vi.fn() },
      connect: (to) => this.graph.edges.push({ from: node, to }),
    };
    this.graph.gains.push(node);
    return node;
  }
  createMediaStreamDestination() {
    const output = new StreamMock([this.graph.outputTrack]);
    const node: AudioNodeMock = {
      kind: 'destination',
      stream: output as unknown as MediaStream,
      connect: () => {},
    };
    return node;
  }
  close = vi.fn(async () => {
    this.state = 'closed';
  });
}

class AudioWorkletNodeMock {
  static failNext = false;
  static failWhenListenerRegistered = false;
  kind = 'processor';
  static created: AudioWorkletNodeMock[] = [];
  context: AudioContextMock;
  name: string;
  options: AudioWorkletNodeOptions;
  port = {
    onmessage: null as ((event: MessageEvent<unknown>) => void) | null,
    postMessage: vi.fn(),
    close: vi.fn(),
  };
  listeners = new Map<string, Set<() => void>>();
  connect = vi.fn((to: AudioNodeMock) => {
    const context = graphs.at(-1)!.context;
    context.graph.edges.push({ from: this as unknown as AudioNodeMock, to });
  });
  disconnect = vi.fn();
  constructor(context: AudioContextMock, name: string, options: AudioWorkletNodeOptions) {
    if (AudioWorkletNodeMock.failNext) {
      AudioWorkletNodeMock.failNext = false;
      throw new Error('node construction failed');
    }
    this.context = context;
    this.name = name;
    this.options = options;
    AudioWorkletNodeMock.created.push(this);
  }
  addEventListener = vi.fn((name: string, listener: () => void) => {
    if (AudioWorkletNodeMock.failWhenListenerRegistered && name === 'processorerror') {
      AudioWorkletNodeMock.failWhenListenerRegistered = false;
      listener();
    }
    const listeners = this.listeners.get(name) ?? new Set();
    listeners.add(listener);
    this.listeners.set(name, listeners);
  });
  dispatch(name: string) {
    this.listeners.get(name)?.forEach((listener) => listener());
  }
}

vi.mock('mediasoup-client', () => ({ Device: class {}, types: {} }));
vi.mock('socket.io-client', () => ({ io: vi.fn(), Socket: class {} }));
vi.mock('@/renderer/services/system/apiClient', () => ({ apiFetch: vi.fn() }));
vi.mock('@/renderer/services/e2ee/mediaEncryption', () => ({
  MEDIA_E2EE_FRAME_CRYPTO_VERSION: 5,
  MediaEncryption: class {},
  deriveFrameKey: vi.fn(),
}));
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({ e2eeService: {} }));
vi.mock('@/renderer/stores/voice/osPermissionStore', () => ({
  useOsPermissionStore: {
    getState: () => ({ checkOne: vi.fn(), openSettings: vi.fn() }),
    subscribe: () => () => {},
  },
  ensureOsPermission: vi.fn(async () => 'granted'),
}));

describe('voiceService microphone processing graph (#3635)', () => {
  const resumePaths = [
    'toggleMute',
    'resumeLocalProducer',
    'Microphone Test restoration',
    'solo exit',
  ] as const;
  const graphPolicies = [
    ['AGC disabled', false, false],
    ['Music Mode enabled', true, true],
    ['AGC peak protection enabled', false, true],
  ] as const;
  const resumableControls = graphPolicies.flatMap(([mode, musicMode, autoGainControl]) =>
    resumePaths.map((resumePath) => [mode, musicMode, autoGainControl, resumePath] as const)
  );
  const originalAudioContext = globalThis.AudioContext;
  const originalAudioWorkletNode = globalThis.AudioWorkletNode;
  const originalMediaStream = globalThis.MediaStream;
  const originalGetUserMedia = navigator.mediaDevices?.getUserMedia;
  let captured: TrackMock[];
  let producedTracks: MediaStreamTrack[];
  let producer: {
    id: string;
    pause: ReturnType<typeof vi.fn>;
    resume: ReturnType<typeof vi.fn>;
    replaceTrack: ReturnType<typeof vi.fn>;
    close: ReturnType<typeof vi.fn>;
    on: ReturnType<typeof vi.fn>;
  };
  let service: any;

  beforeEach(() => {
    resetAllStores();
    graphs.length = 0;
    AudioWorkletNodeMock.created = [];
    AudioWorkletNodeMock.failNext = false;
    AudioWorkletNodeMock.failWhenListenerRegistered = false;
    AudioContextMock.rejectNextModule = false;
    AudioContextMock.nextModuleGate = null;
    captured = [];
    producedTracks = [];
    producer = {
      id: 'mic-producer',
      pause: vi.fn(),
      resume: vi.fn(),
      replaceTrack: vi.fn(async ({ track }) => {
        producedTracks.push(track);
      }),
      close: vi.fn(),
      on: vi.fn(),
    };
    service = voiceService as any;
    service.emergencyCleanup();
    service.producers.clear();
    service.device = {};
    service.sendTransport = {
      closed: false,
      _awaitQueue: { push: async (callback: () => Promise<void>) => callback() },
      produce: vi.fn(async ({ track }) => {
        producedTracks.push(track);
        return producer;
      }),
      close: vi.fn(),
    };
    service.ensureOsPermission = vi.fn(async () => undefined);
    service.startLocalVAD = vi.fn();
    service.startPacketLossMonitor = vi.fn();
    service.clearMediaPolicyPausedByProducer = vi.fn();
    useVoiceStore.setState({ audioInputDeviceId: 'selected-mic' });
    useAudioSettingsStore.getState().setNoiseGateMode('manual');
    useAudioSettingsStore.getState().setNoiseGateLevel(-50);
    Object.defineProperty(globalThis, 'AudioContext', {
      configurable: true,
      writable: true,
      value: AudioContextMock,
    });
    Object.defineProperty(globalThis, 'AudioWorkletNode', {
      configurable: true,
      writable: true,
      value: AudioWorkletNodeMock,
    });
    Object.defineProperty(globalThis, 'MediaStream', {
      configurable: true,
      writable: true,
      value: StreamMock,
    });
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: {
        getUserMedia: vi.fn(async () => {
          const track = new TrackMock();
          captured.push(track);
          return new StreamMock([track]);
        }),
      },
    });
  });

  afterEach(() => {
    service.emergencyCleanup();
    service.sendTransport = null;
    service.device = null;
    delete service.ensureOsPermission;
    delete service.startLocalVAD;
    delete service.startPacketLossMonitor;
    delete service.clearMediaPolicyPausedByProducer;
    Object.defineProperty(globalThis, 'AudioContext', {
      configurable: true,
      writable: true,
      value: originalAudioContext,
    });
    Object.defineProperty(globalThis, 'AudioWorkletNode', {
      configurable: true,
      writable: true,
      value: originalAudioWorkletNode,
    });
    Object.defineProperty(globalThis, 'MediaStream', {
      configurable: true,
      writable: true,
      value: originalMediaStream,
    });
    if (originalGetUserMedia)
      Object.defineProperty(navigator, 'mediaDevices', {
        configurable: true,
        value: { getUserMedia: originalGetUserMedia },
      });
    else Object.defineProperty(navigator, 'mediaDevices', { configurable: true, value: undefined });
  });

  function assertGraph(
    raw: TrackMock,
    finalTrack: MediaStreamTrack,
    expectedLevel: number,
    graphSet = graphs
  ) {
    expect(graphSet, 'all microphone processing must share one AudioContext').toHaveLength(1);
    const graph = graphSet[0];
    expect(
      graph.sourceTracks[0]?.[0],
      'raw microphone capture must enter the single protected processing graph'
    ).toBe(raw);
    expect(
      graph.gains[0]?.gain?.value,
      'the only gain stage must use the effective AGC-aware percentage before protection'
    ).toBe(expectedLevel / 100);
    expect(graph.edges.map(({ from, to }) => [from.kind, to.kind])).toEqual([
      ['source', 'gain'],
      ['gain', 'processor'],
      ['processor', 'destination'],
    ]);
    expect(graph.gains, 'no gain stage may run after the peak protector').toHaveLength(1);
    const processor = AudioWorkletNodeMock.created.find((node) => node.context === graph.context);
    expect(processor?.name).toBe('concord-mic-processor');
    const settings = useAudioSettingsStore.getState();
    expect(processor?.options.processorOptions).toMatchObject({
      protectAgcPeaks: settings.autoGainControl && !settings.musicMode,
      gate: settings.noiseGateMode === 'manual' ? { kind: 'fixed' } : { kind: 'off' },
    });
    expect(finalTrack, 'the outbound producer must use the protected graph track').toBe(
      graph.outputTrack
    );
  }

  it.each([
    {
      name: 'AGC on preserves explicit stored zero',
      agc: true,
      music: false,
      stored: 0,
      effective: 0,
    },
    { name: 'AGC on ignores stored 200', agc: true, music: false, stored: 200, effective: 100 },
    { name: 'AGC off applies zero', agc: false, music: false, stored: 0, effective: 0 },
    { name: 'AGC off applies unity', agc: false, music: false, stored: 100, effective: 100 },
    { name: 'AGC off applies 200 percent', agc: false, music: false, stored: 200, effective: 200 },
    {
      name: 'Music Mode uses manual level despite retained AGC flag',
      agc: true,
      music: true,
      stored: 200,
      effective: 200,
    },
  ])('builds the expected real graph: $name', async ({ agc, music, stored, effective }) => {
    useAudioSettingsStore.getState().setAutoGainControl(agc);
    useAudioSettingsStore.getState().setMusicMode(music);
    useAudioSettingsStore.getState().setInputVolume(stored);
    await service.produceAudio();
    expect(navigator.mediaDevices.getUserMedia).toHaveBeenCalledWith(
      expect.objectContaining({
        audio: expect.objectContaining({
          deviceId: { exact: 'selected-mic' },
          autoGainControl: music ? false : agc,
        }),
      })
    );
    assertGraph(captured[0], producedTracks[0], effective);
  });

  it('keeps an upgraded persisted zero silent in the actual producer gain stage under AGC', async () => {
    localStorage.setItem(
      'concord:audio-advanced',
      JSON.stringify({
        state: {
          inputVolume: 0,
          autoGainControl: true,
          musicMode: false,
          noiseGateMode: 'manual',
          noiseGateLevel: -50,
        },
        version: 2,
      })
    );

    await useAudioSettingsStore.persist.rehydrate();
    expect(useAudioSettingsStore.getState().inputVolume).toBe(0);
    await service.produceAudio();

    expect(navigator.mediaDevices.getUserMedia).toHaveBeenCalledWith(
      expect.objectContaining({
        audio: expect.objectContaining({
          deviceId: { exact: 'selected-mic' },
          autoGainControl: true,
        }),
      })
    );
    assertGraph(captured[0], producedTracks[0], 0);
    expect(service.sendTransport.produce).toHaveBeenCalledWith(
      expect.objectContaining({ track: producedTracks[0] })
    );
  });

  it('reaches the outbound producer after selected-device capture and one protected graph', async () => {
    useAudioSettingsStore.getState().setAutoGainControl(false);
    useAudioSettingsStore.getState().setInputVolume(150);
    await service.produceAudio();
    expect(navigator.mediaDevices.getUserMedia).toHaveBeenCalledWith(
      expect.objectContaining({
        audio: expect.objectContaining({ deviceId: { exact: 'selected-mic' } }),
      })
    );
    expect(captured).toHaveLength(1);
    expect(graphs).toHaveLength(1);
    expect(service.sendTransport.produce).toHaveBeenCalledWith(
      expect.objectContaining({
        track: producedTracks[0],
        appData: { source: 'mic' },
      })
    );
  });

  it.each(['module load', 'node construction'] as const)(
    'fails closed on microphone processor %s failure during initial publication',
    async (failure) => {
      useAudioSettingsStore.getState().setAutoGainControl(true);
      if (failure === 'module load') AudioContextMock.rejectNextModule = true;
      else AudioWorkletNodeMock.failNext = true;

      await expect(service.produceAudio()).rejects.toThrow();

      expect(service.sendTransport.produce).not.toHaveBeenCalled();
      expect(captured).toHaveLength(1);
      expect(captured[0].readyState).toBe('ended');
      expect(service.producers.has('mic')).toBe(false);
    }
  );

  it('resumes the protected old microphone after a settings replacement fails', async () => {
    useAudioSettingsStore.getState().setAutoGainControl(true);
    await service.produceAudio();
    const oldGraph = service.micGraph;
    const resumeCount = producer.resume.mock.calls.length;
    AudioContextMock.rejectNextModule = true;
    useAudioSettingsStore.getState().setNoiseCancellation(false);

    await service.liveReplaceAudioTrack();

    expect(service.micGraph).toBe(oldGraph);
    expect(oldGraph.track.readyState).toBe('live');
    expect(producer.resume).toHaveBeenCalledTimes(resumeCount + 1);
    expect(useVoiceStore.getState().joinError).toMatch(/Microphone processing failed/);
  });

  it('does not resume an old unprotected graph when a protected replacement fails', async () => {
    useAudioSettingsStore.getState().setAutoGainControl(false);
    await service.produceAudio();
    const resumeCount = producer.resume.mock.calls.length;
    AudioContextMock.rejectNextModule = true;
    useAudioSettingsStore.getState().setAutoGainControl(true);

    await service.liveReplaceAudioTrack();

    expect(service.micGraph).not.toBeNull();
    expect(producer.resume).toHaveBeenCalledTimes(resumeCount);
  });

  it('adopts a pending replacement after the old processor fails', async () => {
    await service.produceAudio();
    service.socket = { emit: vi.fn() };
    let finishModule!: () => void;
    AudioContextMock.nextModuleGate = new Promise<void>((resolve) => {
      finishModule = resolve;
    });

    const replacement = service.liveReplaceAudioTrack();
    await vi.waitFor(() => expect(graphs).toHaveLength(2));
    AudioWorkletNodeMock.created[0].dispatch('processorerror');
    expect(service.micGraph).toBeNull();
    finishModule();
    await replacement;

    expect(service.micGraph).not.toBeNull();
    expect(service.micGraph.track.readyState).toBe('live');
    expect(producer.replaceTrack).toHaveBeenCalledWith({ track: service.micGraph.track });
    expect(service.socket.emit).toHaveBeenCalledWith('resume-producer', {
      producerId: producer.id,
    });
  });

  it('shows microphone capture guidance when live replacement capture fails', async () => {
    await service.produceAudio();
    const busy = new Error('busy');
    busy.name = 'NotReadableError';
    vi.mocked(navigator.mediaDevices.getUserMedia).mockRejectedValueOnce(busy);

    await service.liveReplaceAudioTrack();

    expect(useVoiceStore.getState().joinError).toMatch(
      /selected microphone.*in use by another app/
    );
  });

  it('does not adopt or retry initial capture when processorerror fires before publication', async () => {
    AudioWorkletNodeMock.failWhenListenerRegistered = true;

    await expect(service.produceAudio()).rejects.toThrow(/Microphone processing failed/);

    expect(navigator.mediaDevices.getUserMedia).toHaveBeenCalledOnce();
    expect(service.sendTransport.produce).not.toHaveBeenCalled();
    expect(service.producers.has('mic')).toBe(false);
    expect(captured).toHaveLength(1);
    expect(captured[0].readyState).toBe('ended');
    expect(graphs[0].context.close).toHaveBeenCalledOnce();
    expect(useVoiceStore.getState().joinError).toMatch(/Microphone processing failed/);
  });

  it('retires the server microphone slot when its processor fails during publication', async () => {
    const emit = vi.fn((event: string, _payload: unknown, ack?: (result: unknown) => void) => {
      if (event === 'close-producer') ack?.({ success: true });
    });
    service.socket = { emit };
    const drain = vi.fn(async (callback: () => Promise<void>) => callback());
    service.sendTransport._awaitQueue.push = drain;
    service.sendTransport.produce.mockImplementationOnce(async ({ track }) => {
      producedTracks.push(track);
      AudioWorkletNodeMock.created.at(-1)!.dispatch('processorerror');
      return producer;
    });

    await expect(service.produceAudio()).rejects.toThrow(/Microphone processing failed/);

    expect(producer.close).toHaveBeenCalledOnce();
    expect(emit).toHaveBeenCalledWith(
      'close-producer',
      { producerId: producer.id },
      expect.any(Function)
    );
    expect(drain).toHaveBeenCalledOnce();
    expect(service.producers.has('mic')).toBe(false);
  });

  it('preserves microphone-processing guidance when the server close acknowledgement fails', async () => {
    service.socket = {
      emit: vi.fn((event: string, _payload: unknown, ack?: (result: unknown) => void) => {
        if (event === 'close-producer') ack?.({ error: 'close failed' });
      }),
    };
    const drain = vi.fn(async (callback: () => Promise<void>) => callback());
    service.sendTransport._awaitQueue.push = drain;
    service.sendTransport.produce.mockImplementationOnce(async () => {
      AudioWorkletNodeMock.created.at(-1)!.dispatch('processorerror');
      return producer;
    });

    await expect(service.produceAudio()).rejects.toMatchObject({
      code: 'microphone_processing_failed',
    });
    expect(drain).toHaveBeenCalledOnce();
    expect(producer.close).toHaveBeenCalledOnce();
  });

  it('discards a delayed replacement built for stale device and AGC settings', async () => {
    await service.produceAudio();
    const activeGraph = service.micGraph;
    let resolveModule!: () => void;
    AudioContextMock.nextModuleGate = new Promise<void>((resolve) => {
      resolveModule = resolve;
    });
    const firstReplacement = service.liveReplaceAudioTrack();
    await vi.waitFor(() => expect(graphs).toHaveLength(2));
    const staleCapture = captured[1];
    expect(service.micGraph).toBe(activeGraph);

    useVoiceStore.setState({ audioInputDeviceId: 'latest-mic' });
    useAudioSettingsStore.getState().setAutoGainControl(false);
    const latestReplacement = service.liveReplaceAudioTrack();
    resolveModule();
    await Promise.all([firstReplacement, latestReplacement]);

    expect(staleCapture.readyState).toBe('ended');
    expect(navigator.mediaDevices.getUserMedia).toHaveBeenLastCalledWith(
      expect.objectContaining({
        audio: expect.objectContaining({ deviceId: { exact: 'latest-mic' } }),
      })
    );
    expect(service.micGraph).not.toBe(activeGraph);
    expect(service.micGraph?.processor.node).toBeDefined();
    expect(AudioWorkletNodeMock.created.at(-1)?.options.processorOptions).toMatchObject({
      protectAgcPeaks: false,
    });
    assertGraph(captured.at(-1)!, service.micGraph.track, 100, graphs.slice(-1));
  });

  it('does not resume the microphone when mute changes during processor module loading', async () => {
    await service.produceAudio();
    const resumeCount = producer.resume.mock.calls.length;
    let resolveModule!: () => void;
    AudioContextMock.nextModuleGate = new Promise<void>((resolve) => {
      resolveModule = resolve;
    });
    const replacement = service.liveReplaceAudioTrack();
    await vi.waitFor(() => expect(graphs).toHaveLength(2));
    useVoiceStore.setState({ isMuted: true });
    resolveModule();
    await replacement;

    expect(producer.pause).toHaveBeenCalled();
    expect(producer.resume).toHaveBeenCalledTimes(resumeCount);
  });

  it.each(['module load', 'node construction'] as const)(
    'keeps the active protected producer when replacement %s fails',
    async (failure) => {
      await service.produceAudio();
      const activeGraph = service.micGraph;
      const activeStream = service.localMicStream;
      if (failure === 'module load') AudioContextMock.rejectNextModule = true;
      else AudioWorkletNodeMock.failNext = true;

      await service.liveReplaceAudioTrack();

      expect(service.micGraph).toBe(activeGraph);
      expect(service.localMicStream).toBe(activeStream);
      expect(producer.replaceTrack).not.toHaveBeenCalled();
      expect(producer.resume).toHaveBeenCalled();
      expect(captured[0].readyState).toBe('live');
      expect(captured[1].readyState).toBe('ended');
    }
  );

  it('pauses and retires the live microphone when its processor reports a runtime error', async () => {
    await service.produceAudio();
    const processor = AudioWorkletNodeMock.created[0];
    expect(service.producers.get('mic')).toBe(producer);

    processor.dispatch('processorerror');

    expect(producer.pause).toHaveBeenCalledOnce();
    expect(service.producers.get('mic')).toBe(producer);
    expect(service.micGraph).toBeNull();
    expect(useVoiceStore.getState().joinError).toMatch(/Microphone processing stopped/);
    expect(captured[0].readyState).toBe('ended');
    expect(service.sendTransport.produce).toHaveBeenCalledOnce();
  });

  it('keeps a failed worklet producer paused through mute and local resume controls', async () => {
    await service.produceAudio();
    service.socket = { emit: vi.fn() };
    AudioWorkletNodeMock.created[0].dispatch('processorerror');
    const resumeCount = producer.resume.mock.calls.length;

    useVoiceStore.setState({ isMuted: true });
    await service.toggleMute();
    Object.assign(producer, { paused: true });
    service.resumeLocalProducer('mic');

    expect(producer.resume).toHaveBeenCalledTimes(resumeCount);
    expect(service.socket.emit).not.toHaveBeenCalledWith('resume-producer', {
      producerId: producer.id,
    });
  });

  it('does not restore a failed worklet producer when Microphone Test ends', async () => {
    await service.produceAudio();
    Object.assign(producer, { kind: 'audio', paused: false });
    service.socket = { emit: vi.fn() };
    service.beginTestSuspension();
    Object.assign(producer, { paused: true });
    AudioWorkletNodeMock.created[0].dispatch('processorerror');
    const resumeCount = producer.resume.mock.calls.length;

    service.endTestSuspension();

    expect(producer.resume).toHaveBeenCalledTimes(resumeCount);
    expect(service.socket.emit).not.toHaveBeenCalledWith('resume-producer', {
      producerId: producer.id,
    });
  });

  // Regression introduced by #3653: failed AGC activation must keep the retained graph paused.
  it.each([
    'toggleMute',
    'resumeLocalProducer',
    'Microphone Test restoration',
    'solo exit',
  ] as const)(
    'does not resume a retained AGC-off graph through %s after enabling AGC fails',
    async (resumePath) => {
      useAudioSettingsStore.getState().setAutoGainControl(false);
      useUserStore.setState({ user: { ...mockUser, id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa' } });
      await service.produceAudio();
      service.setupLiveSubscriptions();
      producer.pause.mockImplementation(() => Object.assign(producer, { paused: true }));
      producer.resume.mockImplementation(() => Object.assign(producer, { paused: false }));
      const oldGraph = service.micGraph;
      expect(oldGraph.settings.autoGainControl).toBe(false);
      Object.assign(producer, { kind: 'audio', paused: false, closed: false });
      if (resumePath === 'Microphone Test restoration') {
        service.beginTestSuspension();
        Object.assign(producer, { paused: true });
      }

      service.socket = { emit: vi.fn() };
      AudioContextMock.rejectNextModule = true;
      useAudioSettingsStore.getState().setAutoGainControl(true);
      await service.micReplaceTrackQueue;

      expect(service.micGraph).toBe(oldGraph);
      expect(useVoiceStore.getState().joinError).toMatch(/Microphone processing failed/);
      const resumeCount = producer.resume.mock.calls.length;
      Object.assign(producer, { paused: true });
      if (resumePath === 'toggleMute') {
        await service.toggleMute();
        await service.toggleMute();
      } else if (resumePath === 'resumeLocalProducer') {
        service.resumeLocalProducer('mic');
      } else if (resumePath === 'Microphone Test restoration') {
        service.endTestSuspension();
      } else {
        useVoiceStore.setState({ isSoloBandwidthSaving: true });
        service.exitSoloBandwidthSaving();
      }

      expect(
        producer.resume,
        'AGC-on policy must never resume the retained AGC-off graph'
      ).toHaveBeenCalledTimes(resumeCount);
      expect(
        service.socket.emit,
        'AGC-on policy must never resume the retained AGC-off graph'
      ).not.toHaveBeenCalledWith('resume-producer', { producerId: producer.id });
    }
  );

  it.each(resumableControls)(
    '%s graph (musicMode=%s, autoGainControl=%s) can resume through %s',
    async (_mode, musicMode, autoGainControl, resumePath) => {
      useAudioSettingsStore.getState().setMusicMode(musicMode);
      useAudioSettingsStore.getState().setAutoGainControl(autoGainControl);
      await service.produceAudio();
      service.setupLiveSubscriptions();
      producer.pause.mockImplementation(() => Object.assign(producer, { paused: true }));
      producer.resume.mockImplementation(() => Object.assign(producer, { paused: false }));
      Object.assign(producer, { kind: 'audio', paused: false, closed: false });
      service.socket = { emit: vi.fn() };
      const resumeCount = producer.resume.mock.calls.length;

      if (resumePath === 'toggleMute') {
        Object.assign(producer, { paused: true });
        useVoiceStore.setState({ isMuted: true });
        await service.toggleMute();
      } else if (resumePath === 'resumeLocalProducer') {
        Object.assign(producer, { paused: true });
        service.resumeLocalProducer('mic');
      } else if (resumePath === 'Microphone Test restoration') {
        service.beginTestSuspension();
        service.endTestSuspension();
      } else {
        Object.assign(producer, { paused: true });
        useVoiceStore.setState({ isSoloBandwidthSaving: true });
        service.exitSoloBandwidthSaving();
      }

      expect(producer.resume).toHaveBeenCalledTimes(resumeCount + 1);
      expect(service.socket.emit).toHaveBeenCalledWith('resume-producer', {
        producerId: producer.id,
      });
    }
  );

  it('recovers a processorerror-paused producer with a protected graph and ignores stale callbacks', async () => {
    useAudioSettingsStore.getState().setAutoGainControl(false);
    await service.produceAudio();
    service.setupLiveSubscriptions();
    service.socket = { emit: vi.fn() };
    const failedProcessor = AudioWorkletNodeMock.created[0];
    const failedTrack = captured[0];
    failedProcessor.dispatch('processorerror');

    expect(service.producers.get('mic')).toBe(producer);
    expect(producer.pause).toHaveBeenCalledOnce();
    expect(service.socket.emit).toHaveBeenCalledWith('pause-producer', {
      producerId: producer.id,
    });
    expect(failedTrack.readyState).toBe('ended');

    useAudioSettingsStore.getState().setAutoGainControl(true);
    await vi.waitFor(() => expect(service.micGraph).not.toBeNull());

    const recoveredGraph = service.micGraph;
    expect(recoveredGraph).not.toBeNull();
    expect(service.producers.get('mic')).toBe(producer);
    expect(producer.replaceTrack).toHaveBeenCalledWith({ track: recoveredGraph.track });
    expect(recoveredGraph.track).toBe(graphs.at(-1)?.outputTrack);
    expect(AudioWorkletNodeMock.created.at(-1)?.options.processorOptions).toMatchObject({
      protectAgcPeaks: true,
    });
    expect(useVoiceStore.getState().joinError).toBeNull();
    expect(service.socket.emit).toHaveBeenCalledWith('resume-producer', {
      producerId: producer.id,
    });

    const pauseCountAfterRecovery = producer.pause.mock.calls.length;
    failedProcessor.dispatch('processorerror');
    expect(service.micGraph).toBe(recoveredGraph);
    expect(producer.pause).toHaveBeenCalledTimes(pauseCountAfterRecovery);
    expect(useVoiceStore.getState().joinError).toBeNull();
  });

  it('preserves an unrelated join error after a processor recovery succeeds', async () => {
    useAudioSettingsStore.getState().setAutoGainControl(false);
    await service.produceAudio();
    service.setupLiveSubscriptions();
    AudioWorkletNodeMock.created[0].dispatch('processorerror');
    useVoiceStore.getState().setJoinError('A separate voice connection error');

    useAudioSettingsStore.getState().setAutoGainControl(true);
    await vi.waitFor(() => expect(service.micGraph).not.toBeNull());

    expect(useVoiceStore.getState().joinError).toBe('A separate voice connection error');
    expect(service.producers.get('mic')).toBe(producer);
    expect(AudioWorkletNodeMock.created.at(-1)?.options.processorOptions).toMatchObject({
      protectAgcPeaks: true,
    });
  });

  it('does not resume a policy-paused mic after replacing its track', async () => {
    await service.produceAudio();
    const resumeCount = producer.resume.mock.calls.length;
    service.socket = { emit: vi.fn() };
    useVoiceStore.getState().setMediaPolicyPaused('mic', producer.id);

    await service.liveReplaceAudioTrack();

    expect(producer.replaceTrack).toHaveBeenCalledOnce();
    expect(producer.resume).toHaveBeenCalledTimes(resumeCount);
    expect(service.socket.emit).not.toHaveBeenCalledWith('resume-producer', {
      producerId: producer.id,
    });
  });

  it('keeps a replacement microphone producer paused when mute changes during replaceTrack', async () => {
    const oldTrack = new TrackMock();
    service.localMicStream = new StreamMock([oldTrack]);
    service.producers.set('mic', producer);
    let resolveReplacement!: () => void;
    producer.replaceTrack.mockImplementationOnce(async ({ track }) => {
      producedTracks.push(track);
      await new Promise<void>((resolve) => {
        resolveReplacement = resolve;
      });
    });
    useVoiceStore.setState({ audioInputDeviceId: 'replacement-mic', isMuted: false });
    const replacement = service.liveReplaceAudioTrack();
    await vi.waitFor(() => expect(navigator.mediaDevices.getUserMedia).toHaveBeenCalled());
    useVoiceStore.setState({ isMuted: true });
    resolveReplacement();
    await replacement;
    expect(navigator.mediaDevices.getUserMedia).toHaveBeenCalledWith(
      expect.objectContaining({
        audio: expect.objectContaining({ deviceId: { exact: 'replacement-mic' } }),
      })
    );
    expect(producer.pause).toHaveBeenCalledOnce();
    expect(
      producer.resume,
      'a user mute that arrives during track replacement must keep the producer suspended'
    ).not.toHaveBeenCalled();
  });

  it('keeps a replacement microphone producer paused when server mute arrives during replaceTrack', async () => {
    const oldTrack = new TrackMock();
    service.localMicStream = new StreamMock([oldTrack]);
    service.producers.set('mic', producer);
    let resolveReplacement!: () => void;
    producer.replaceTrack.mockImplementationOnce(async ({ track }) => {
      producedTracks.push(track);
      await new Promise<void>((resolve) => {
        resolveReplacement = resolve;
      });
    });
    useUserStore.setState({ user: { id: 'local-user' } as any });
    useVoiceStore.setState({
      audioInputDeviceId: 'replacement-mic',
      isMuted: false,
      participants: {
        'local-user': { userId: 'local-user', serverMuted: false, serverDeafened: false } as any,
      },
    });
    const replacement = service.liveReplaceAudioTrack();
    await vi.waitFor(() => expect(navigator.mediaDevices.getUserMedia).toHaveBeenCalled());
    useVoiceStore.setState({
      participants: {
        'local-user': { userId: 'local-user', serverMuted: true, serverDeafened: false } as any,
      },
    });
    resolveReplacement();
    await replacement;
    expect(
      producer.resume,
      'server mute arriving during track replacement must keep the producer suspended'
    ).not.toHaveBeenCalled();
  });

  it('does not amplify saved manual level changes while AGC remains on', async () => {
    useAudioSettingsStore.getState().setAutoGainControl(true);
    useAudioSettingsStore.getState().setInputVolume(100);
    await service.produceAudio();
    const inputGraph = graphs.find((graph) =>
      graph.edges.some(({ from, to }) => from.kind === 'source' && to.kind === 'gain')
    )!;
    const gain = inputGraph.gains[0].gain!;

    useAudioSettingsStore.getState().setInputVolume(180);
    const agcOnTargets = gain.setTargetAtTime.mock.calls.map(([value]) => value);
    expect(
      agcOnTargets.every((value) => value === 1),
      'stored manual level changes must leave the live gain at unity while AGC is effective'
    ).toBe(true);
  });

  it('applies the saved manual level when committed AGC changes from on to off', async () => {
    useAudioSettingsStore.getState().setAutoGainControl(true);
    useAudioSettingsStore.getState().setInputVolume(180);
    await service.produceAudio();
    const gain = graphs.find((graph) =>
      graph.edges.some(({ from, to }) => from.kind === 'source' && to.kind === 'gain')
    )!.gains[0].gain!;
    useAudioSettingsStore.getState().setAutoGainControl(false);
    expect(gain.setTargetAtTime).toHaveBeenLastCalledWith(
      1.8,
      expect.any(Number),
      expect.any(Number)
    );
  });

  it('pauses the unprotected producer synchronously when AGC is enabled', async () => {
    useAudioSettingsStore.getState().setAutoGainControl(false);
    await service.produceAudio();
    service.setupLiveSubscriptions();
    const previousProcessor = AudioWorkletNodeMock.created.at(-1)!;
    const previousPauseCalls = producer.pause.mock.calls.length;

    useAudioSettingsStore.getState().setAutoGainControl(true);

    expect(
      producer.pause.mock.calls.length,
      'the old unprotected microphone path must pause before the queued replacement or module load'
    ).toBeGreaterThan(previousPauseCalls);
    await vi.waitFor(() => expect(AudioWorkletNodeMock.created.length).toBeGreaterThan(1));
    await vi.waitFor(() => expect(service.micGraph?.processor).not.toBe(previousProcessor));
    expect(previousProcessor.port.close).toHaveBeenCalledOnce();
    expect(AudioWorkletNodeMock.created.at(-1)?.options.processorOptions).toMatchObject({
      protectAgcPeaks: true,
    });
  });

  it('uses a committed manual level change made while microphone capture is pending', async () => {
    useAudioSettingsStore.getState().setAutoGainControl(false);
    useAudioSettingsStore.getState().setInputVolume(100);
    let resolveCapture!: (stream: StreamMock) => void;
    const pendingCapture = new Promise<StreamMock>((resolve) => {
      resolveCapture = resolve;
    });
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn(() => pendingCapture) },
    });

    const starting = service.produceAudio();
    await vi.waitFor(() => expect(navigator.mediaDevices.getUserMedia).toHaveBeenCalled());
    useAudioSettingsStore.getState().setInputVolume(175);
    const capturedTrack = new TrackMock();
    captured.push(capturedTrack);
    resolveCapture(new StreamMock([capturedTrack]));
    await starting;

    const inputLevelGraph = graphs.find((graph) =>
      graph.edges.some(({ from, to }) => from.kind === 'source' && to.kind === 'gain')
    )!;
    expect(
      inputLevelGraph.gains[0]?.gain?.value,
      'the pending capture must publish with the latest committed manual level'
    ).toBe(1.75);
  });

  it('uses manual level when Music Mode is committed with the stored AGC flag still on', async () => {
    useAudioSettingsStore.getState().setAutoGainControl(true);
    useAudioSettingsStore.getState().setInputVolume(180);
    await service.produceAudio();
    const gain = graphs.find((graph) =>
      graph.edges.some(({ from, to }) => from.kind === 'source' && to.kind === 'gain')
    )!.gains[0].gain!;
    useAudioSettingsStore.getState().setMusicMode(true);
    expect(gain.setTargetAtTime).toHaveBeenLastCalledWith(
      1.8,
      expect.any(Number),
      expect.any(Number)
    );
    useAudioSettingsStore.getState().setInputVolume(65);
    expect(gain.setTargetAtTime).toHaveBeenLastCalledWith(
      0.65,
      expect.any(Number),
      expect.any(Number)
    );
  });

  it('keeps the consumed gain finite when persisted input volume is non-finite', async () => {
    useAudioSettingsStore.getState().setAutoGainControl(false);
    useAudioSettingsStore.setState({ inputVolume: Number.NaN });
    await service.produceAudio();
    const inputGraph = graphs.find((graph) =>
      graph.edges.some(({ from, to }) => from.kind === 'source' && to.kind === 'gain')
    )!;
    expect(
      inputGraph.gains[0].gain?.value,
      'the live microphone gain must never consume a non-finite persisted value'
    ).toBe(1);
    expect(Number.isFinite(inputGraph.gains[0].gain?.value)).toBe(true);
  });

  it('rebuilds the microphone graph through the live re-produce path using selected device and effective gain', async () => {
    useAudioSettingsStore.getState().setAutoGainControl(false);
    useAudioSettingsStore.getState().setInputVolume(175);
    await service.produceAudio();
    const firstProducer = service.producers.get('mic');
    service.producers.set('mic', firstProducer);
    useVoiceStore.setState({ audioInputDeviceId: 'reproduce-mic' });
    await service.liveReproduceAudio();
    expect(captured).toHaveLength(2);
    expect(navigator.mediaDevices.getUserMedia).toHaveBeenLastCalledWith(
      expect.objectContaining({
        audio: expect.objectContaining({ deviceId: { exact: 'reproduce-mic' } }),
      })
    );
    assertGraph(captured[1], producedTracks[1], 175, graphs.slice(-1));
  });

  it('applies effective gain before the gate when replacing a selected microphone', async () => {
    const oldTrack = new TrackMock();
    service.localMicStream = new StreamMock([oldTrack]);
    service.producers.set('mic', producer);
    useAudioSettingsStore.getState().setAutoGainControl(false);
    useAudioSettingsStore.getState().setInputVolume(160);
    useVoiceStore.setState({ audioInputDeviceId: 'replacement-mic' });
    await service.liveReplaceAudioTrack();
    expect(captured).toHaveLength(1);
    assertGraph(captured[0], producedTracks.at(-1)!, 160, graphs.slice(-2));
  });

  it('keeps the old graph track live for rollback after a successful but stale replacement', async () => {
    await service.produceAudio();
    const oldGraph = service.micGraph;
    const stopTracks = service.sendTransport.produce.mock.calls[0][0].stopTracks !== false;
    let producerTrack = oldGraph.track;
    let releaseSwap!: () => void;
    const swap = new Promise<void>((resolve) => {
      releaseSwap = resolve;
    });
    producer.replaceTrack.mockImplementation(async ({ track }) => {
      if (track.readyState === 'ended') throw new Error('track ended');
      if (producer.replaceTrack.mock.calls.length === 1) await swap;
      if (stopTracks) producerTrack.stop();
      producerTrack = track;
    });

    const replacement = service.liveReplaceAudioTrack();
    await vi.waitFor(() => expect(producer.replaceTrack).toHaveBeenCalledOnce());
    useVoiceStore.setState({ audioInputDeviceId: 'newer-device' });
    releaseSwap();
    await replacement;

    expect(producer.replaceTrack).toHaveBeenCalledTimes(2);
    expect(producerTrack).toBe(oldGraph.track);
    expect(oldGraph.track.readyState).toBe('live');
    expect(graphs.at(-1)?.outputTrack.readyState).toBe('ended');

    await service.closeProducer('mic');
    expect(oldGraph.track.readyState).toBe('ended');
    expect(captured[0].readyState).toBe('ended');
  });

  it.each([true, false])(
    'restores the old track after a failed swap only when rollback succeeds (%s)',
    async (restoreSucceeds) => {
      await service.produceAudio();
      const oldGraph = service.micGraph;
      const resumeCount = producer.resume.mock.calls.length;
      producer.replaceTrack.mockRejectedValueOnce(new Error('swap failed'));
      if (!restoreSucceeds)
        producer.replaceTrack.mockRejectedValueOnce(new Error('restore failed'));

      await service.liveReplaceAudioTrack();

      expect(producer.replaceTrack).toHaveBeenCalledTimes(2);
      expect(producer.replaceTrack).toHaveBeenLastCalledWith({ track: oldGraph.track });
      expect(service.micGraph).toBe(restoreSucceeds ? oldGraph : null);
      expect(oldGraph.track.readyState).toBe(restoreSucceeds ? 'live' : 'ended');
      expect(producer.resume).toHaveBeenCalledTimes(resumeCount + Number(restoreSucceeds));
      expect(useVoiceStore.getState().joinError).toMatch(/Microphone processing failed/);
    }
  );

  it('retires the old graph when neither stale-swap rollback can restore its track', async () => {
    await service.produceAudio();
    const oldGraph = service.micGraph;
    const resumeCount = producer.resume.mock.calls.length;
    let finishSwap!: () => void;
    producer.replaceTrack
      .mockImplementationOnce(async () => {
        await new Promise<void>((resolve) => {
          finishSwap = resolve;
        });
      })
      .mockRejectedValueOnce(new Error('rollback failed'))
      .mockRejectedValueOnce(new Error('rollback retry failed'));

    const replacement = service.liveReplaceAudioTrack();
    await vi.waitFor(() => expect(producer.replaceTrack).toHaveBeenCalledOnce());
    useVoiceStore.setState({ audioInputDeviceId: 'newer-device' });
    finishSwap();
    await replacement;

    expect(producer.replaceTrack).toHaveBeenCalledTimes(3);
    expect(service.micGraph).toBeNull();
    expect(oldGraph.track.readyState).toBe('ended');
    expect(graphs.at(-1)?.outputTrack.readyState).toBe('ended');
    expect(producer.resume).toHaveBeenCalledTimes(resumeCount);
    expect(service.hasLiveMicGraph(producer)).toBe(false);
  });

  it('does not let an older delayed producer replacement overwrite the newer microphone', async () => {
    service.localMicStream = new StreamMock([new TrackMock()]);
    service.producers.set('mic', producer);
    useVoiceStore.setState({ audioInputDeviceId: 'first-replacement' });

    let finishFirst!: () => void;
    let activeProducerTrack: MediaStreamTrack | undefined;
    let replacementCalls = 0;
    producer.replaceTrack.mockImplementation(async ({ track }) => {
      producedTracks.push(track);
      replacementCalls++;
      if (replacementCalls === 1) {
        await new Promise<void>((resolve) => {
          finishFirst = resolve;
        });
      }
      activeProducerTrack = track;
    });
    vi.mocked(navigator.mediaDevices.getUserMedia as ReturnType<typeof vi.fn>)
      .mockImplementationOnce(async () => {
        const track = new TrackMock();
        track.id = 'first-replacement-track';
        captured.push(track);
        return new StreamMock([track]);
      })
      .mockImplementationOnce(async () => {
        const track = new TrackMock();
        track.id = 'second-replacement-track';
        captured.push(track);
        return new StreamMock([track]);
      });

    const first = service.liveReplaceAudioTrack();
    await vi.waitFor(() => expect(producer.replaceTrack).toHaveBeenCalledOnce());
    useVoiceStore.setState({ audioInputDeviceId: 'second-replacement' });
    const second = service.liveReplaceAudioTrack();
    const secondCaptureStartedBeforeFirstFinished =
      navigator.mediaDevices.getUserMedia.mock.calls.length > 1;
    finishFirst();
    await Promise.all([first, second]);

    expect(
      secondCaptureStartedBeforeFirstFinished,
      'the next replacement must wait for the active replaceTrack transaction'
    ).toBe(false);
    expect(
      activeProducerTrack,
      'a stale in-flight replaceTrack completion must never publish over its newer successor'
    ).toBe(graphs.at(-1)?.outputTrack);
  });
});
