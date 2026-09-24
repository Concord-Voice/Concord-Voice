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
import { useSubscriptionStore } from '@/renderer/stores/auth/subscriptionStore';
import { createScreenAudioBridge } from '@/renderer/services/voice/screenAudioBridge';
import { deferred } from '../../helpers/deferred';

// The bridge builds a `MediaStreamTrackGenerator`, which jsdom does not implement.
// The T5a cases below are about the interrupt wiring, not the bridge itself --
// `screenAudioBridge.test.ts` owns that. Mirrors
// `voiceService.switchScreenSource.test.ts`'s mock shape.
vi.mock('@/renderer/services/voice/screenAudioBridge', () => ({
  createScreenAudioBridge: vi.fn(() => ({
    track: { id: 'bridge-track', kind: 'audio', readyState: 'live', muted: false, stop: vi.fn() },
    stop: vi.fn(),
  })),
}));

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

  it('LOCAL: the server closing a screen-audio producer this client still holds routes (teardown rail 2)', async () => {
    const heldProducer = producerStub('p-local');
    svc.producers.set('screen-audio', heldProducer);
    useVoiceStore.getState().setScreenAudioOn(true);
    const setScreenAudioEnabledSpy = vi.spyOn(svc, 'setScreenAudioEnabled');
    const producerClosed = socketHandler('producer-closed');

    producerClosed({ producerId: 'p-local', userId: LOCAL_USER, source: 'screen-audio' });
    await setScreenAudioEnabledSpy.mock.results[0]?.value;

    expect(stop).toHaveBeenCalledTimes(1);
    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'off', overrun: 0 });
    // The held client-side producer twin is really retired -- RTP stops and the
    // map entry is gone, not merely the store flag flipped.
    expect(heldProducer.close).toHaveBeenCalledTimes(1);
    expect(svc.producers.has('screen-audio')).toBe(false);
    expect(useVoiceStore.getState().isScreenAudioOn).toBe(false);
  });

  // F1: the system-loopback rung, the actual bug this fix closes. A server close of a
  // HELD screen-audio producer routes through the Share-sound OFF path
  // (setScreenAudioEnabled(false)), never closeProducer('screen-audio') -- the latter
  // runs cleanupScreenAudioState, which STOPS and removes the captured audio track, so
  // turning Share sound back on later found a dead track and fell through to a full
  // re-capture that replaced the VIDEO track and glitched every viewer for a change
  // that was only ever supposed to touch audio (see frontend.md "Turning audio OFF
  // must not stop the captured track"). Proven two ways: the track itself survives,
  // and re-enabling reuses it via produceScreenAudioFromStream rather than re-capturing.
  it('LOCAL: a held loopback screen-audio producer keeps its captured track and can be turned back on (F1)', async () => {
    const heldProducer = producerStub('p-local');
    svc.producers.set('screen-audio', heldProducer);
    useVoiceStore.getState().setScreenAudioOn(true);
    const audioTrack = svc.localScreenStream.getAudioTracks()[0];
    const setScreenAudioEnabledSpy = vi.spyOn(svc, 'setScreenAudioEnabled');
    const producerClosed = socketHandler('producer-closed');

    producerClosed({ producerId: 'p-local', userId: LOCAL_USER, source: 'screen-audio' });
    await setScreenAudioEnabledSpy.mock.results[0]?.value;

    // (i) the RTP twin really stopped and is gone from the map.
    expect(heldProducer.close).toHaveBeenCalledTimes(1);
    expect(svc.producers.has('screen-audio')).toBe(false);

    // (ii) the loopback capture track itself is NOT stopped or removed -- it is
    // still live in localScreenStream, ready to be reused.
    expect(audioTrack.stop).not.toHaveBeenCalled();
    expect(audioTrack.readyState).toBe('live');
    expect(svc.localScreenStream.getAudioTracks()).toContain(audioTrack);

    // (iii) turning Share sound back on reuses the still-live track through the
    // audio-only produce path, rather than falling through to a full re-capture
    // that would replace the video track and glitch every viewer.
    await svc.setScreenAudioEnabled(true);

    expect(svc.produceScreenAudioFromStream).toHaveBeenCalledTimes(1);
  });

  // F1b: the sibling fix for the `screen` source. Unlike screen-audio, a server close
  // of a held `screen` producer still routes through closeProducer('screen') -- ending
  // the WHOLE share is correct here, because the server closed the producer that
  // carries the share itself, not just its audio.
  it('LOCAL: the server closing a screen producer this client still holds tears down the local share (F1b)', async () => {
    const closeProducerSpy = vi.spyOn(svc, 'closeProducer');
    const producerClosed = socketHandler('producer-closed');

    producerClosed({ producerId: screenProducer.id, userId: LOCAL_USER, source: 'screen' });

    expect(closeProducerSpy).toHaveBeenCalledTimes(1);
    await closeProducerSpy.mock.results[0]?.value;

    expect(screenProducer.close).toHaveBeenCalledTimes(1);
    expect(svc.producers.has('screen')).toBe(false);
    expect(svc.localScreenStream).toBeNull();
  });

  // Control for F1b: an echo naming a `screen` producer id this client does not
  // (any longer) hold -- e.g. a reproduce already replaced it -- must not touch the
  // live share. Mirrors the existing "echo for a re-produced (old) producer id"
  // control below, for the `screen` source instead of `screen-audio`.
  it('control: an echo for an old (already-replaced) screen producer id leaves the live share running', async () => {
    const closeProducerSpy = vi.spyOn(svc, 'closeProducer');
    const producerClosed = socketHandler('producer-closed');

    producerClosed({ producerId: 'p-old-screen', userId: LOCAL_USER, source: 'screen' });
    // Flush any fire-and-forget microtasks a (mis-)routed close would have scheduled.
    await Promise.resolve();
    await Promise.resolve();

    expect(closeProducerSpy).not.toHaveBeenCalled();
    expect(screenProducer.close).not.toHaveBeenCalled();
    expect(svc.producers.get('screen')).toBe(screenProducer);
    expect(svc.localScreenStream).not.toBeNull();
  });

  // #3394 PR 3 regression: rail 2 fired on ANY local-user producer-closed echo,
  // including the echo of a close THIS client initiated itself (the interrupt
  // handler's own `close-producer` emit, or a reproduce/switch closing the OLD
  // producer id while a NEW one is live) -- overwriting live state with 'off'
  // 7ms after the real write. The planned fix gates rail 2 on ownership: only
  // tear down when `this.producers.get('screen-audio')` still holds the id the
  // echo names, mirroring the mic/camera branch above in the same handler.
  it('LOCAL: the echo of a close this client made itself does not overwrite interrupted', () => {
    useVoiceStore.getState().setScreenAudioState({
      mode: 'interrupted',
      reason: 'child-crash',
      generation: 2,
      overrun: 0,
    });
    // No screen-audio producer held: it was already retired before this echo arrives.
    const producerClosed = socketHandler('producer-closed');

    producerClosed({ producerId: 'p-retired', userId: LOCAL_USER, source: 'screen-audio' });

    expect(stop).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().screenAudio).toEqual({
      mode: 'interrupted',
      reason: 'child-crash',
      generation: 2,
      overrun: 0,
    });
  });

  it('LOCAL: the echo for a re-produced (old) producer id leaves the live capture running', () => {
    svc.producers.set('screen-audio', producerStub('p-new'));
    const producerClosed = socketHandler('producer-closed');

    producerClosed({ producerId: 'p-old', userId: LOCAL_USER, source: 'screen-audio' });

    expect(stop).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().screenAudio).toEqual(LIVE);
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

  it('#2153 T0: screen audio is produced at the highest allowed tier rate, never uncapped', async () => {
    // Measured in T0: with no cap, stereo system audio ran at 537 kbps and the policer paused
    // it. A studio-entitled user proves the cap follows the entitlement, not a constant.
    useSubscriptionStore.setState({
      entitlement: {
        ...useSubscriptionStore.getState().entitlement,
        allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard', 'high', 'hifi', 'studio'],
      },
    });
    delete svc.produceScreenAudioFromStream;
    svc.produceEncrypted = vi.fn().mockResolvedValue(producerStub('screen-audio-1'));

    await svc.produceScreenAudioFromStream(svc.localScreenStream);

    expect(svc.produceEncrypted).toHaveBeenCalledTimes(1); // positive control: it produced
    const opts = svc.produceEncrypted.mock.calls[0][1];
    expect(opts.encodings?.[0]?.maxBitrate).toBe(510_000);
    expect(opts.codecOptions?.opusMaxAverageBitrate).toBe(510_000);
  });

  // Mirrors the media plane's resolveAllowedOpusBitrateCeiling: an unknown tier contributes
  // nothing, and a list with no known tier floors at `standard`, never at 0.
  it.each([
    [['bogus'], 96_000],
    [['bogus', 'high'], 192_000],
  ])('#2153 T0: allowed tiers %j cap screen audio at %i', async (tiers, expected) => {
    useSubscriptionStore.setState({
      entitlement: { ...useSubscriptionStore.getState().entitlement, allowedAudioTiers: tiers },
    });
    delete svc.produceScreenAudioFromStream;
    svc.produceEncrypted = vi.fn().mockResolvedValue(producerStub('screen-audio-1'));

    await svc.produceScreenAudioFromStream(svc.localScreenStream);

    expect(svc.produceEncrypted).toHaveBeenCalledTimes(1); // positive control: it produced
    expect(svc.produceEncrypted.mock.calls[0][1].encodings?.[0]?.maxBitrate).toBe(expected);
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

  // #3198 Task 13b: the degrade reason must reach the user, not just the store.
  // Asserted at the OUTERMOST OBSERVABLE SEAM -- the rendered slot error text --
  // not at screenAudioDegradeMessage's return value, per tests.md "Test the
  // consumer, not the handshake": a mapping function can be correct while nothing
  // ever calls it with the live reason.
  it('surfaces the degrade reason as a user-visible slot error', async () => {
    delete svc.produceScreenAudioFromStream;
    svc.produceEncrypted = vi
      .fn()
      .mockRejectedValue(
        new Error('Only one active screen-audio producer allowed per participant')
      );

    await svc.produceScreenAudioFromStream(svc.localScreenStream);

    expect(useVoiceStore.getState().videoSlotError).toBe(
      'This call can’t carry another audio track, so your screen is being shared without sound.'
    );
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

/**
 * A stream stub whose `addTrack` actually mutates what `getAudioTracks()` returns.
 * `capturePerProcessScreenAudio` attaches the bridge's audio track by calling
 * `stream.addTrack(bridge.track)` on the video-only stream `videoOnly()` returns, and
 * `produceScreenAudioFromStream` reads `stream.getAudioTracks()` to find it -- the
 * fixed-array `streamOf` above cannot see a track added after construction.
 */
function mutableStreamOf(initial: ReturnType<typeof track>[]) {
  let tracks = [...initial];
  return {
    getTracks: () => tracks,
    getVideoTracks: () => tracks.filter((t) => t.kind === 'video'),
    getAudioTracks: () => tracks.filter((t) => t.kind === 'audio'),
    addTrack: (t: ReturnType<typeof track>) => {
      tracks = [...tracks, t];
    },
    removeTrack: (t: ReturnType<typeof track>) => {
      tracks = tracks.filter((x) => x !== t);
    },
  };
}

/**
 * A live per-process share moves to an interrupted state (#3394 PR 2 T5a).
 *
 * `screenAudioInterrupts.ts` does not exist yet, and neither does the
 * `capturePerProcessScreenAudio` -> `claimInterrupts` wiring the plan's Step 3
 * describes. Every case that needs the interrupt to actually be DELIVERED reaches the
 * module through a non-literal dynamic `import()`, which fails for a "module missing"
 * reason until both land -- see this task's instructions and
 * `screenAudioInterrupts.test.ts` for the module's own unit coverage.
 *
 * The CONTROL case first, and it needs no import: it proves the harness reaches a
 * live per-process share (a `screen-audio` producer, `screenAudio.mode ===
 * 'per-process'`, and a bridge with a generation) using ONLY code that exists today.
 * Without it, a failure in any interrupt case is indistinguishable from a fixture that
 * never reached the per-process rung in the first place (tests.md § "Test the
 * consumer, not the handshake").
 */
describe('screen-audio interrupt handling (#3394 PR 2 T5a)', () => {
  /* eslint-disable @typescript-eslint/no-explicit-any -- the capture seam, the bridge
     field and the interrupt claim are all private; driving the real singleton is the
     only way the wiring between capturePerProcessScreenAudio and the interrupts
     module is under test at all. */
  const GENERATION = 42;
  const WINDOW_ID = 'window:9:0';
  const MOD_PATH = '../../../src/renderer/services/voice/screenAudioInterrupts';

  let svc: any;
  let screenProducer: ReturnType<typeof producerStub>;
  let audioProducer: ReturnType<typeof producerStub>;
  let audiocapStop: ReturnType<typeof vi.fn>;
  let bridgeStop: ReturnType<typeof vi.fn>;
  let bridgeTrack: ReturnType<typeof track>;
  let videoTrack: ReturnType<typeof track>;
  let stream: ReturnType<typeof mutableStreamOf>;

  /**
   * Delivers an interrupt through the not-yet-existing module (never through a
   * private method -- per this task's instructions) and flushes the per-source
   * reproduce tail the plan's handler is queued onto, so the queued
   * `handleScreenAudioInterrupt` continuation has actually settled before the
   * assertions run.
   */
  async function deliverLiveInterrupt(reason: string, generation = GENERATION): Promise<void> {
    const mod = (await import(/* @vite-ignore */ MOD_PATH)) as {
      deliverInterrupt: (payload: unknown) => void;
    };
    mod.deliverInterrupt({ generation, reason });
    await svc.videoReproduceQueues.screen;
  }

  beforeEach(async () => {
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
    svc.videoReproduceSessionActive = true;
    svc.videoReproduceQueues.screen = Promise.resolve();
    svc.videoReproduceQueues.camera = Promise.resolve();

    audiocapStop = vi.fn().mockResolvedValue(undefined);
    globalThis.electron = {
      ...globalThis.electron,
      audiocap: {
        start: vi
          .fn()
          .mockResolvedValue({ ok: true, generation: GENERATION, perProcessAudio: true }),
        stop: audiocapStop,
      },
    } as unknown as typeof globalThis.electron;

    bridgeStop = vi.fn();
    bridgeTrack = track('bridge-audio', 'audio');
    vi.mocked(createScreenAudioBridge).mockReturnValue({
      track: bridgeTrack,
      stop: bridgeStop,
      generation: GENERATION,
    } as any);

    videoTrack = track('video', 'video');
    const videoOnly = () => Promise.resolve(mutableStreamOf([videoTrack]));
    // Reaches the live per-process rung using production code that already exists --
    // the claim registration this describe is testing is the only piece that does not.
    const result = await svc.capturePerProcessScreenAudio(WINDOW_ID, videoOnly);
    stream = result.stream;
    svc.localScreenStream = stream;

    screenProducer = producerStub('screen-producer');
    svc.producers.set('screen', screenProducer);

    audioProducer = producerStub('screen-audio-producer');
    svc.produceEncrypted = vi.fn().mockResolvedValue(audioProducer);
    await svc.produceScreenAudioFromStream(stream);

    // Setup itself routes through `stopScreenAudioHost()` once (inside
    // `capturePerProcessScreenAudio`, before the bridge is built) and that call is not
    // part of what any test below is measuring -- only the invokes an INTERRUPT
    // causes are.
    audiocapStop.mockClear();
  });

  it('CONTROL: reaches a live per-process share with a screen-audio producer', () => {
    expect(svc.producers.get('screen-audio')).toBe(audioProducer);
    expect(useVoiceStore.getState().screenAudio.mode).toBe('per-process');
    expect((svc.screenAudioBridge as any)?.generation).toBe(GENERATION);
  });

  it('an interrupt for the live generation retires audio only, closes zero audiocap hosts, and marks the share interrupted', async () => {
    await deliverLiveInterrupt('child-crash');

    // The audio half was really retired -- so the "keeps the video live" and
    // "zero audiocap:stop" assertions below are about SCOPE, not about the handler
    // having quietly done nothing.
    expect(audioProducer.close).toHaveBeenCalledTimes(1);
    expect(bridgeTrack.stop).toHaveBeenCalledTimes(1);

    expect(screenProducer.close).not.toHaveBeenCalled();
    expect(videoTrack.stop).not.toHaveBeenCalled();
    expect(stream.getVideoTracks()).toHaveLength(1);
    // Zero audiocap:stop invokes: an interrupt releases the bridge, it does not run
    // the full `stopScreenAudioHost()` teardown -- that distinction is the whole
    // reason `releaseScreenAudioBridge()` exists as its own method in the plan.
    expect(audiocapStop).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().isScreenAudioOn).toBe(false);
    expect(useVoiceStore.getState().screenAudio).toEqual({
      mode: 'interrupted',
      reason: 'child-crash',
      generation: GENERATION,
      overrun: 0,
    });
  });

  it('an interrupt for an older generation changes nothing', async () => {
    await deliverLiveInterrupt('child-crash', GENERATION - 1);

    expect(audioProducer.close).not.toHaveBeenCalled();
    expect(audiocapStop).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().screenAudio.mode).toBe('per-process');
  });

  it('a later share end moves interrupted to off', async () => {
    await deliverLiveInterrupt('protocol-fault');
    // Precondition: the share really is interrupted before the share-end call below,
    // so the transition assertion is about the TRANSITION and not about a mode that
    // was already 'off'.
    expect(useVoiceStore.getState().screenAudio.mode).toBe('interrupted');

    svc.stopScreenAudioHost();

    expect(audiocapStop).toHaveBeenCalledTimes(1);
    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'off', overrun: 0 });
  });

  /**
   * Mutation `Ma`: delete `handleScreenAudioInterrupt`'s FIRST currentness check --
   * `if (this.screenAudioBridge?.generation !== generation) return;`, the one BEFORE
   * the `retireScreenAudioProducer` await. Only reachable when the interrupt is
   * QUEUED behind other screen work and the share is superseded before its turn
   * comes up -- the two cases below hold the screen reproduce tail busy with a prior
   * job so the interrupt sits queued, supersede (or don't) while it waits, then
   * release the tail. The `deliverLiveInterrupt` helper above cannot exercise this:
   * it awaits the tail itself, so nothing can run between delivery and settlement.
   */
  async function loadInterruptsModule(): Promise<{ deliverInterrupt: (payload: unknown) => void }> {
    return (await import(/* @vite-ignore */ MOD_PATH)) as {
      deliverInterrupt: (payload: unknown) => void;
    };
  }

  it('Ma CONTROL: an interrupt still ends interrupted when nothing supersedes it while queued', async () => {
    const hold = deferred<void>();
    void svc.enqueueVideoReproduce('screen', () => hold.promise);
    const mod = await loadInterruptsModule();

    mod.deliverInterrupt({ generation: GENERATION, reason: 'child-crash' });
    hold.resolve();
    await svc.videoReproduceQueues.screen;

    expect(useVoiceStore.getState().screenAudio).toEqual({
      mode: 'interrupted',
      reason: 'child-crash',
      generation: GENERATION,
      overrun: 0,
    });
  });

  it('Ma: an interrupt superseded before its queued turn runs never touches the new state', async () => {
    const hold = deferred<void>();
    void svc.enqueueVideoReproduce('screen', () => hold.promise);
    const mod = await loadInterruptsModule();

    // Queues behind the held prior job -- nothing runs yet.
    mod.deliverInterrupt({ generation: GENERATION, reason: 'child-crash' });

    // Supersede BEFORE the queued interrupt's turn: end the old share for real (bridge
    // -> null) and install the NEW share's own screen-audio producer, exactly what a
    // second share running concurrently with a stale queued interrupt would leave
    // behind.
    svc.stopScreenAudioHost();
    const newAudioProducer = producerStub('new-screen-audio-producer');
    svc.producers.set('screen-audio', newAudioProducer);
    useVoiceStore.getState().setScreenAudioOn(true);
    useVoiceStore.getState().setScreenAudioState({ mode: 'system', overrun: 0 });

    hold.resolve();
    await svc.videoReproduceQueues.screen;

    // The stale interrupt must not touch the NEW state at all.
    expect(newAudioProducer.close).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().isScreenAudioOn).toBe(true);
    expect(useVoiceStore.getState().screenAudio.mode).not.toBe('interrupted');
  });

  /**
   * Mutation `Mb`: delete `handleScreenAudioInterrupt`'s SECOND currentness check --
   * `if (this.screenAudioBridge?.generation !== generation) return;`, the one AFTER
   * the `retireScreenAudioProducer` await. Reachable only when a share end lands
   * WHILE that await is still in flight: `retireScreenAudioProducer`'s own
   * `drainSendTransportQueue` round trip is held on a deferred so the share end can
   * run mid-retire, then released.
   */
  it('Mb: a share end that lands mid-retire wins, not the stale interrupt', async () => {
    const holdDrain = deferred<void>();
    svc.drainSendTransportQueue = vi.fn().mockReturnValue(holdDrain.promise);
    const mod = await loadInterruptsModule();

    mod.deliverInterrupt({ generation: GENERATION, reason: 'child-crash' });

    // Let the retire's synchronous portion (closing the audio producer) run, so we
    // know we are inside the pending `drainSendTransportQueue` await before ending
    // the share for real.
    await vi.waitFor(() => expect(audioProducer.close).toHaveBeenCalled());
    svc.stopScreenAudioHost();

    holdDrain.resolve();
    await svc.videoReproduceQueues.screen;

    // The share end already owns this stream; the stale interrupt must not touch it
    // or overwrite the 'off' state the real share end wrote.
    expect(bridgeTrack.stop).not.toHaveBeenCalled();
    expect(audiocapStop).toHaveBeenCalledTimes(1); // the real share end reaped the host
    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'off', overrun: 0 });
  });
  /* eslint-enable @typescript-eslint/no-explicit-any */
});
