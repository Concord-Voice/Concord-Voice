/**
 * An `audiocap:interrupted` push, or the user's own "Share sound" OFF, that lands while
 * `produceScreenAudioFromStream` awaits the SFU round trip for the per-process bridge
 * track must not be undone when that round trip settles (#3394 PR 2, found by the
 * Phase-4 red-team pass). Before the fix a resolved produce registered the torn-down
 * track, flipped `isScreenAudioOn` back to true, and rewrote `interrupted` as
 * `{ mode: 'system' }` -- the whole-machine label, on a WINDOW share -- because the mode
 * was re-read from a bridge the interrupt had already released. A rejected produce
 * overwrote `interrupted` with `degraded` the same way.
 *
 * The ordering driven here is the production one: `capturePerProcessScreenAudio` (claim)
 * -> `publishScreenCapture` (video produce, commit `localScreenStream`, then await the
 * audio produce). `produceScreen` is not serialized on the `screen` reproduce tail, so
 * the interrupt handler the claim enqueues runs on an idle tail DURING that await.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { voiceService } from '@/renderer/services/voice/voiceService';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import {
  deliverInterrupt,
  resetScreenAudioInterruptsForTest,
} from '@/renderer/services/voice/screenAudioInterrupts';
import { resetAllStores } from '../../helpers/store-helpers';
import { deferred } from '../../helpers/deferred';

// jsdom has no MediaStreamTrackGenerator. The fake mirrors the one property that
// matters: `stop()` ends the generator track, as `generator.stop()` does.
vi.mock('@/renderer/services/voice/screenAudioBridge', () => ({
  createScreenAudioBridge: vi.fn((generation: number) => {
    const t = {
      id: `bridge-track-${generation}`,
      kind: 'audio',
      readyState: 'live',
      muted: false,
      enabled: true,
      contentHint: '',
      onended: null as null | (() => void),
      stop() {
        this.readyState = 'ended';
      },
    };
    return {
      track: t,
      generation,
      stop: vi.fn(() => {
        t.readyState = 'ended';
      }),
      stats: vi.fn(),
    };
  }),
}));

/* eslint-disable @typescript-eslint/no-explicit-any -- private members under attack */
const fakeTrack = (id: string, kind: 'audio' | 'video') => ({
  id,
  kind,
  readyState: 'live',
  muted: false,
  enabled: true,
  contentHint: '',
  onended: null as null | (() => void),
  stop() {
    this.readyState = 'ended';
  },
});

class FakeStream {
  private tracks: any[];
  constructor(tracks: any[]) {
    this.tracks = [...tracks];
  }
  getTracks() {
    return [...this.tracks];
  }
  getVideoTracks() {
    return this.tracks.filter((t) => t.kind === 'video');
  }
  getAudioTracks() {
    return this.tracks.filter((t) => t.kind === 'audio');
  }
  addTrack(t: any) {
    if (!this.tracks.includes(t)) this.tracks.push(t);
  }
  removeTrack(t: any) {
    this.tracks = this.tracks.filter((x) => x !== t);
  }
}

const producerStub = (id: string) => ({
  id,
  closed: false,
  paused: false,
  close: vi.fn(),
  on: vi.fn(),
  pause: vi.fn(),
  resume: vi.fn(),
});

const GENERATION = 7;
const WINDOW_SOURCE = 'window:42:0';

describe('interrupt / audio-off racing the per-process screen-audio produce (#3394 PR 2)', () => {
  let svc: any;
  let electron: any;
  let savedAudiocap: unknown;
  let transport: { id: string; closed: boolean };

  beforeEach(() => {
    resetAllStores();
    resetScreenAudioInterruptsForTest();
    vi.clearAllMocks();
    useUserStore.setState({ user: { id: 'local-user' } as any });

    electron = (globalThis as any).electron ??= {};
    savedAudiocap = electron.audiocap;
    electron.audiocap = {
      start: vi.fn().mockResolvedValue({ ok: true, generation: GENERATION, perProcessAudio: true }),
      stop: vi.fn().mockResolvedValue(undefined),
    };

    svc = voiceService as any;
    svc.producers.clear();
    svc.screenAudioBridge = null;
    svc.localScreenStream = null;
    transport = { id: 'transport-1', closed: false };
    svc.sendTransport = transport;
    svc.socket = { emit: vi.fn(), on: vi.fn(), io: { on: vi.fn() } };
    svc.drainSendTransportQueue = vi.fn().mockResolvedValue(undefined);
    svc.videoReproduceSessionActive = true;
    // publishScreenCapture collaborators that are irrelevant to the race.
    svc.pickScreenCodec = () => ({
      codec: { mimeType: 'video/VP8' },
      encodings: [{}],
      effectiveBitrate: 1_000_000,
    });
    svc.requireSelectedVideoCodec = (c: unknown) => c;
    svc.computeStartBitrate = () => 1000;
    svc.applyDegradationPreference = vi.fn();
    svc.startPacketLossMonitor = vi.fn();
    svc.getProducerCodecMimeType = () => 'video/VP8';
    svc.publishScreenAudioCapability = vi.fn().mockResolvedValue(undefined);
  });

  afterEach(() => {
    electron.audiocap = savedAudiocap;
    for (const k of [
      'pickScreenCodec',
      'requireSelectedVideoCodec',
      'computeStartBitrate',
      'applyDegradationPreference',
      'startPacketLossMonitor',
      'getProducerCodecMimeType',
      'publishScreenAudioCapability',
      'produceEncrypted',
      'drainSendTransportQueue',
    ]) {
      delete svc[k];
    }
  });

  /** Capture (claims GENERATION) and start publishing, parked on the AUDIO produce. */
  async function shareParkedOnAudioProduce() {
    const stream = new FakeStream([fakeTrack('screen-video', 'video')]);
    const captured = await svc.capturePerProcessScreenAudio(
      WINDOW_SOURCE,
      async () => stream,
      () => true
    );
    expect(captured.stream.getAudioTracks()).toHaveLength(1);
    expect(useVoiceStore.getState().screenAudio.mode).toBe('per-process');

    const audioProduce = deferred<ReturnType<typeof producerStub>>();
    svc.produceEncrypted = vi
      .fn()
      .mockResolvedValueOnce(producerStub('screen-video-producer'))
      .mockImplementationOnce(() => audioProduce.promise);

    const token = svc.captureVideoReproduceToken('screen');
    const publishing: Promise<void> = svc.publishScreenCapture(
      { captured: { ...captured, sourceId: WINDOW_SOURCE }, options: { contentType: 'detail' } },
      token,
      transport,
      svc.socket,
      () => true
    );
    // Parked inside `await this.produceEncrypted(...)` for the bridge audio track.
    await vi.waitFor(() => expect(svc.produceEncrypted).toHaveBeenCalledTimes(2));
    expect(svc.localScreenStream).toBe(stream);
    electron.audiocap.stop.mockClear();
    return { audioProduce, publishing };
  }

  it('an interrupt delivered during the audio produce is NOT undone when the produce resolves', async () => {
    const { audioProduce, publishing } = await shareParkedOnAudioProduce();

    // Main retired the child and pushed; preload forwards exactly this shape.
    deliverInterrupt({ generation: GENERATION, reason: 'child-crash' });
    await vi.waitFor(() =>
      expect(useVoiceStore.getState().screenAudio).toEqual({
        mode: 'interrupted',
        reason: 'child-crash',
        generation: GENERATION,
        overrun: 0,
      })
    );
    expect(useVoiceStore.getState().isScreenAudioOn).toBe(false);

    audioProduce.resolve(producerStub('screen-audio-producer'));
    await publishing;

    const state = useVoiceStore.getState();
    // INVARIANT: a fault only ENDS a capture. The notice must survive, the toggle must
    // stay off, and no producer may be registered for the torn-down bridge track.
    expect(state.screenAudio.mode).toBe('interrupted');
    expect(state.isScreenAudioOn).toBe(false);
    expect(svc.producers.has('screen-audio')).toBe(false);
    // And the interrupt path never reaps over `audiocap:stop` (main already did).
    expect(electron.audiocap.stop).not.toHaveBeenCalled();
  });

  it('an interrupt during the audio produce survives a REJECTED produce', async () => {
    const { audioProduce, publishing } = await shareParkedOnAudioProduce();

    deliverInterrupt({ generation: GENERATION, reason: 'protocol-fault' });
    await vi.waitFor(() => expect(useVoiceStore.getState().screenAudio.mode).toBe('interrupted'));

    audioProduce.reject(new Error('produce rejected by the SFU'));
    await publishing;

    const state = useVoiceStore.getState();
    // Not overwritten with `{ mode: 'degraded', reason: 'produce-rejected' }`.
    expect(state.screenAudio).toEqual({
      mode: 'interrupted',
      reason: 'protocol-fault',
      generation: GENERATION,
      overrun: 0,
    });
    expect(svc.producers.has('screen-audio')).toBe(false);
  });

  it('twin: the user turning Share sound OFF during the audio produce stays OFF', async () => {
    const { audioProduce, publishing } = await shareParkedOnAudioProduce();

    await svc.setScreenAudioEnabled(false);
    expect(useVoiceStore.getState().isScreenAudioOn).toBe(false);
    expect(useVoiceStore.getState().screenAudio.mode).toBe('off');

    audioProduce.resolve(producerStub('screen-audio-producer'));
    await publishing;

    const state = useVoiceStore.getState();
    expect(state.isScreenAudioOn).toBe(false);
    expect(state.screenAudio.mode).toBe('off');
    expect(svc.producers.has('screen-audio')).toBe(false);
  });
});
