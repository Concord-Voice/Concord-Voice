import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';
import { useAudioSettingsStore } from '@/renderer/stores/audio/audioSettingsStore';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { voiceService } from '@/renderer/services/voice/voiceService';

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
  stop = vi.fn();
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
  const originalAudioContext = globalThis.AudioContext;
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
    expect(graphSet).toHaveLength(2);
    const levelGraph = graphSet.find((graph) =>
      graph.edges.some(({ from, to }) => from.kind === 'source' && to.kind === 'gain')
    )!;
    const gateGraph = graphSet.find((graph) =>
      graph.edges.some(({ from, to }) => from.kind === 'source' && to.kind === 'analyser')
    )!;
    expect(
      levelGraph.sourceTracks[0]?.[0],
      'raw microphone capture must enter the manual level stage'
    ).toBe(raw);
    expect(
      levelGraph.gains[0]?.gain?.value,
      'manual microphone level must use the effective AGC-aware percentage'
    ).toBe(expectedLevel / 100);
    expect(levelGraph.edges.map(({ from, to }) => [from.kind, to.kind])).toEqual([
      ['source', 'gain'],
      ['gain', 'destination'],
    ]);
    expect(
      gateGraph.sourceTracks[0]?.[0],
      'the gate must receive the processed microphone-level output'
    ).toBe(levelGraph.outputTrack);
    expect(gateGraph.edges.map(({ from, to }) => [from.kind, to.kind])).toEqual([
      ['source', 'analyser'],
      ['analyser', 'gain'],
      ['gain', 'destination'],
    ]);
    expect(finalTrack, 'the outbound producer must use the gated microphone track').toBe(
      gateGraph.outputTrack
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

  it('reaches the outbound producer after selected-device capture and both real processing stages', async () => {
    useAudioSettingsStore.getState().setAutoGainControl(false);
    useAudioSettingsStore.getState().setInputVolume(150);
    await service.produceAudio();
    expect(navigator.mediaDevices.getUserMedia).toHaveBeenCalledWith(
      expect.objectContaining({
        audio: expect.objectContaining({ deviceId: { exact: 'selected-mic' } }),
      })
    );
    expect(captured).toHaveLength(1);
    expect(graphs).toHaveLength(2);
    expect(service.sendTransport.produce).toHaveBeenCalledWith(
      expect.objectContaining({
        track: producedTracks[0],
        appData: { source: 'mic' },
      })
    );
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
    assertGraph(captured[1], producedTracks[1], 175, graphs.slice(-2));
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
});
