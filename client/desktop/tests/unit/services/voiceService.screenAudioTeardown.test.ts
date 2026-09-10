/**
 * The local screen-audio teardown choke point (#3195, ADR-0043, design section 6c).
 *
 * ONE INVARIANT, AND IT IS A LOCAL/REMOTE SPLIT. `stopScreenAudioHost()` tears down
 * THIS client's own capture. Four paths in `voiceService` are local and must reach it;
 * the paths that look most like them are REMOTE and must not.
 *
 *   `closeScreenAudioConsumerForUser` closes a CONSUMER of somebody else's producer.
 *   The `producer-closed` socket handler fires for an ARBITRARY participant.
 *
 * Routing either would kill the LOCAL capture child whenever a REMOTE peer stopped
 * sharing audio: audio disappears for the one participant who did nothing, and every
 * diagnostic names the peer who did. The design's own section 6c listed two of these
 * remote paths as sites to route, which is why the negative cases below carry the same
 * weight as the positive ones -- they are the ones a plausible reading of the spec
 * gets wrong.
 *
 * The assertions are doubled deliberately: the SPY proves the path reached the choke
 * point, and the STORE proves the choke point is the thing that owns the state. A spy
 * alone would pass against a second copy of the teardown inlined at the call site.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { voiceService } from '@/renderer/services/voice/voiceService';
import { resetAllStores } from '../../helpers/store-helpers';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';

const LOCAL_USER = 'local-user';
const REMOTE_USER = 'remote-user';

/** The state a live system-loopback share is in when a teardown path runs. */
const LIVE = { mode: 'system', overrun: 3 } as const;

const track = (id: string, kind: 'video' | 'audio', readyState = 'live') => ({
  id,
  kind,
  readyState,
  enabled: true,
  muted: false,
  stop: vi.fn(),
  contentHint: '',
});

const streamOf = (tracks: ReturnType<typeof track>[]) => ({
  getTracks: () => tracks,
  getVideoTracks: () => tracks.filter((t) => t.kind === 'video'),
  getAudioTracks: () => tracks.filter((t) => t.kind === 'audio'),
  removeTrack: vi.fn(),
});

const producerStub = (id: string) => ({
  id,
  closed: false,
  paused: false,
  close: vi.fn(),
  pause: vi.fn(),
  resume: vi.fn(),
  replaceTrack: vi.fn().mockResolvedValue(undefined),
  on: vi.fn(),
});

describe('voiceService screen-audio teardown choke point (#3195 section 6c)', () => {
  /* eslint-disable @typescript-eslint/no-explicit-any -- the choke point and every path
     that reaches it are private; these tests exercise the real singleton rather than a
     re-implementation of it, which is the only way the local/remote split is under test. */
  let svc: any;
  let stop: ReturnType<typeof vi.spyOn>;
  let screenProducer: ReturnType<typeof producerStub>;

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useUserStore.setState({ user: { id: LOCAL_USER } as any });

    svc = voiceService as any;
    svc.producers.clear();
    svc.consumers = new Map();
    svc.consumerMeta = new Map();
    svc.pendingScreenAudioProducers = new Map();
    svc.socket = { emit: vi.fn(), on: vi.fn(), io: { on: vi.fn() } };
    svc.sendTransport = { id: 'transport-1', closed: false };
    svc.drainSendTransportQueue = vi.fn().mockResolvedValue(undefined);
    svc.produceScreenAudioFromStream = vi.fn().mockResolvedValue(undefined);
    // The singleton is shared across the tests in this file; drop any own-property
    // stub a previous test left so the REAL queued body runs.
    delete svc.switchScreenSourceQueued;
    svc.publishScreenAudioCapability = vi.fn().mockResolvedValue(undefined);
    svc.closeConsumerAndNotify = vi.fn();
    svc.onProducerClosed = undefined;
    // Every screen mutation is serialized on the per-source reproduce tail, and
    // enqueueVideoReproduce discards the operation when no session is live. Without
    // this the local cases would pass for the wrong reason: the body never runs.
    svc.videoReproduceSessionActive = true;

    screenProducer = producerStub('screen-producer-1');
    svc.producers.set('screen', screenProducer);
    svc.localScreenStream = streamOf([track('v', 'video'), track('a', 'audio')]);

    stop = vi.spyOn(svc, 'stopScreenAudioHost');
    useVoiceStore.getState().setScreenAudioState({ ...LIVE });
  });

  afterEach(() => {
    stop.mockRestore();
  });

  // -------------------------------------------------------------------------
  // LOCAL -- must route
  // -------------------------------------------------------------------------

  it('LOCAL: turning screen audio off routes through the choke point', async () => {
    svc.producers.set('screen-audio', producerStub('screen-audio-1'));

    await svc.setScreenAudioEnabled(false);

    expect(stop).toHaveBeenCalledTimes(1);
    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'off', overrun: 0 });
  });

  it('LOCAL: the switchScreenSource continuation routes through the choke point', async () => {
    const captured = streamOf([track('new-video', 'video')]);
    svc.acquireScreenCapture = vi.fn().mockResolvedValue({
      stream: captured,
      sourceId: 'screen:1',
    });
    svc.producers.set('screen-audio', producerStub('screen-audio-1'));

    await svc.switchScreenSource('screen:1');

    expect(stop).toHaveBeenCalledTimes(1);
    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'off', overrun: 0 });
  });

  it('LOCAL: closing the standalone screen-audio producer routes through the choke point', async () => {
    svc.producers.set('screen-audio', producerStub('screen-audio-1'));

    await svc.closeProducer('screen-audio');

    expect(stop).toHaveBeenCalledTimes(1);
    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'off', overrun: 0 });
  });

  it('LOCAL: closing the whole screen share routes through the choke point', async () => {
    svc.producers.set('screen-audio', producerStub('screen-audio-1'));

    await svc.closeProducer('screen');

    expect(stop).toHaveBeenCalledTimes(1);
    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'off', overrun: 0 });
  });

  // -------------------------------------------------------------------------
  // REMOTE -- must NOT route
  // -------------------------------------------------------------------------

  it('REMOTE: closing a peer’s screen-audio CONSUMER leaves the local capture alone', () => {
    svc.consumerMeta.set('consumer-1', { source: 'screen-audio', producerUserId: REMOTE_USER });

    svc.closeScreenAudioConsumerForUser(REMOTE_USER, useVoiceStore.getState());

    // The consumer really was closed -- so the negative below is about ROUTING, not
    // about the path having quietly done nothing.
    expect(svc.closeConsumerAndNotify).toHaveBeenCalledWith('consumer-1');
    expect(stop).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().screenAudio).toEqual(LIVE);
  });

  it('REMOTE: a peer’s producer-closed leaves the local capture alone', () => {
    // Seeded so the branch has observable work to do: an assertion that a participant
    // who was never added has no screenAudioStream is true before the handler runs, and
    // would let a handler that took the wrong branch pass.
    svc.pendingScreenAudioProducers.set(REMOTE_USER, 'p-remote');
    const producerClosed = socketHandler('producer-closed');

    producerClosed({ producerId: 'p-remote', userId: REMOTE_USER, source: 'screen-audio' });

    expect(svc.pendingScreenAudioProducers.has(REMOTE_USER)).toBe(false);
    expect(stop).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().screenAudio).toEqual(LIVE);
  });

  it('LOCAL: the same producer-closed for THIS user does route (teardown rail 2)', () => {
    const producerClosed = socketHandler('producer-closed');

    producerClosed({ producerId: 'p-local', userId: LOCAL_USER, source: 'screen-audio' });

    expect(stop).toHaveBeenCalledTimes(1);
    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'off', overrun: 0 });
  });

  // A signed-out or not-yet-hydrated user store must not make every peer look local.
  it('REMOTE: producer-closed with no known local user never routes', () => {
    useUserStore.setState({ user: undefined } as any);
    svc.pendingScreenAudioProducers.set(REMOTE_USER, 'p-remote');
    const producerClosed = socketHandler('producer-closed');

    producerClosed({ producerId: 'p-remote', userId: REMOTE_USER, source: 'screen-audio' });

    expect(svc.pendingScreenAudioProducers.has(REMOTE_USER)).toBe(false);
    expect(stop).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().screenAudio).toEqual(LIVE);
  });

  // -------------------------------------------------------------------------
  // LOCAL -- the four failure paths that retire an audio producer WITHOUT any
  // deliberate teardown call. Each already cleared `isScreenAudioOn`; none
  // cleared `screenAudio.mode`, which has no auto-clear twin (its whole reason
  // for existing is that `setVideoSlotError` self-dismisses after 5000 ms and a
  // share that lost its sound stays lost). So the stale value survived until
  // some later deliberate teardown happened to run.
  // -------------------------------------------------------------------------

  it('LOCAL: the OS ending the capture track routes through the choke point', () => {
    const audioProducer = producerStub('screen-audio-1');
    svc.producers.set('screen-audio', audioProducer);
    const audioTrack: any = track('a', 'audio');

    svc.bindScreenAudioTrackEnded(audioTrack, audioProducer);
    // Precondition, not incidental: firing an `onended` the service never bound
    // would leave every assertion below true of a no-op (tests.md § Vacuity).
    expect(typeof audioTrack.onended).toBe('function');
    audioTrack.onended();

    // The path really did retire the producer, so the routing assertion is about
    // ROUTING rather than about the handler having quietly done nothing.
    expect(audioProducer.close).toHaveBeenCalled();
    expect(stop).toHaveBeenCalledTimes(1);
    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'off', overrun: 0 });
  });

  it('LOCAL: the audio producer’s transportclose routes through the choke point', async () => {
    const audioProducer = producerStub('screen-audio-1');
    // The beforeEach stubs the whole method; drop the own-property stub so the
    // real body runs, and stub only the SFU round trip inside it.
    delete svc.produceScreenAudioFromStream;
    svc.produceEncrypted = vi.fn().mockResolvedValue(audioProducer);

    await svc.produceScreenAudioFromStream(svc.localScreenStream);

    expect(svc.producers.get('screen-audio')).toBe(audioProducer);
    const close = audioProducer.on.mock.calls.find(
      (c: unknown[]) => c[0] === 'transportclose'
    )?.[1] as () => void;
    expect(typeof close).toBe('function');
    close();

    expect(stop).toHaveBeenCalledTimes(1);
    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'off', overrun: 0 });
  });

  // -------------------------------------------------------------------------
  // A REFUSED PRODUCE IS A USER-VISIBLE DEGRADE, not a console line
  // -------------------------------------------------------------------------

  it('surfaces an SFU produce refusal as a degraded verdict with a reason', async () => {
    delete svc.produceScreenAudioFromStream;
    svc.produceEncrypted = vi
      .fn()
      .mockRejectedValue(
        new Error('Only one active screen-audio producer allowed per participant')
      );

    await svc.produceScreenAudioFromStream(svc.localScreenStream);

    // C9: video-only WITH a reason. The union refuses a reason-less `degraded`,
    // which is what stops this from degrading into an unexplained state.
    expect(useVoiceStore.getState().screenAudio).toEqual({
      mode: 'degraded',
      reason: 'produce-rejected',
      overrun: 0,
    });
    expect(svc.producers.has('screen-audio')).toBe(false);
  });

  it('does NOT write a degraded verdict for a share that already ended', async () => {
    const captured = svc.localScreenStream;
    delete svc.produceScreenAudioFromStream;
    svc.produceEncrypted = vi.fn().mockImplementation(() => {
      // The stop lands INSIDE the produce round trip -- the window the success
      // path's five-way currentness check exists for. `ScreenAudioState` has no
      // auto-clear, so a degraded write here is a permanent badge on nothing.
      svc.localScreenStream = null;
      return Promise.reject(new Error('transport closed'));
    });

    await svc.produceScreenAudioFromStream(captured);

    expect(useVoiceStore.getState().screenAudio).toEqual(LIVE);
  });

  /**
   * Register the real socket handlers on the stub socket and return the one bound to
   * `event`. `setupSocketListeners` only registers callbacks; it never invokes them.
   */
  function socketHandler(event: string): (payload: unknown) => unknown {
    svc.setupSocketListeners();
    const call = svc.socket.on.mock.calls.find((c: unknown[]) => c[0] === event);
    if (!call) throw new Error(`no handler registered for ${event}`);
    return call[1] as (payload: unknown) => unknown;
  }
  /* eslint-enable @typescript-eslint/no-explicit-any */
});
